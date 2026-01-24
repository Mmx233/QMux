package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

// ConnectionState represents the state of a server connection
type ConnectionState int

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateConnected
	StateUnhealthy
)

// String returns a string representation of the connection state
func (s ConnectionState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// ReconnectionCallback is a function type for signaling that reconnection is needed.
// It receives the server address that needs reconnection.
type ReconnectionCallback func(serverAddr string)

// ServerConnection represents a connection to a single server instance.
// Each ServerConnection maintains its own TLS session cache to ensure
// session tickets are isolated between different servers.
type ServerConnection struct {
	serverAddr   string
	serverName   string
	sessionCache tls.ClientSessionCache

	conn          *quic.Conn
	controlStream *quic.Stream

	// Health tracking
	healthy       atomic.Bool
	lastHeartbeat atomic.Int64

	// Bidirectional heartbeat tracking - tracks when heartbeats are received from server
	lastReceivedFromServer atomic.Int64

	// Health check configuration
	healthTimeout time.Duration

	// Reconnection callback - called when health check detects timeout
	reconnectCallback ReconnectionCallback

	// Connection state
	state atomic.Int32

	logger zerolog.Logger

	ctx    context.Context
	cancel context.CancelFunc
}

// NewServerConnection creates a new ServerConnection instance.
// The sessionCache should be obtained from SessionCacheManager to ensure
// proper isolation between different servers.
func NewServerConnection(serverAddr, serverName string, sessionCache tls.ClientSessionCache, logger zerolog.Logger) *ServerConnection {
	ctx, cancel := context.WithCancel(context.Background())

	sc := &ServerConnection{
		serverAddr:   serverAddr,
		serverName:   serverName,
		sessionCache: sessionCache,
		logger: logger.With().
			Str("server_addr", serverAddr).
			Logger(),
		ctx:    ctx,
		cancel: cancel,
	}

	// Initialize as disconnected and unhealthy
	sc.state.Store(int32(StateDisconnected))
	sc.healthy.Store(false)

	return sc
}

// Connect establishes the QUIC connection using the isolated session cache.
// The baseTLSConfig should contain certificates and CA pool, but the session
// cache will be overridden with this connection's isolated cache.
func (sc *ServerConnection) Connect(ctx context.Context, baseTLSConfig *tls.Config, quicConfig *quic.Config) error {
	sc.state.Store(int32(StateConnecting))
	sc.logger.Info().Msg("connecting to server")

	// Create TLS config with isolated session cache
	tlsConfig := baseTLSConfig.Clone()
	tlsConfig.ServerName = sc.serverName
	tlsConfig.ClientSessionCache = sc.sessionCache

	conn, err := quic.DialAddr(ctx, sc.serverAddr, tlsConfig, quicConfig)
	if err != nil {
		sc.state.Store(int32(StateDisconnected))
		return fmt.Errorf("dial server %s: %w", sc.serverAddr, err)
	}

	sc.conn = conn
	sc.state.Store(int32(StateConnected))
	sc.logger.Info().Msg("connected to server")

	return nil
}

// ServerAddr returns the server address this connection is for.
func (sc *ServerConnection) ServerAddr() string {
	return sc.serverAddr
}

// ServerName returns the TLS server name for this connection.
func (sc *ServerConnection) ServerName() string {
	return sc.serverName
}

// State returns the current connection state.
func (sc *ServerConnection) State() ConnectionState {
	return ConnectionState(sc.state.Load())
}

// Connection returns the underlying QUIC connection.
// Returns nil if not connected.
func (sc *ServerConnection) Connection() *quic.Conn {
	return sc.conn
}

// --- Health Tracking Methods ---

// IsHealthy returns the current health status of the connection.
// A connection is healthy if it has successfully sent heartbeats
// within the configured timeout.
func (sc *ServerConnection) IsHealthy() bool {
	return sc.healthy.Load()
}

// MarkHealthy marks the connection as healthy and updates the last heartbeat timestamp.
func (sc *ServerConnection) MarkHealthy() {
	wasHealthy := sc.healthy.Swap(true)
	sc.lastHeartbeat.Store(time.Now().UnixNano())

	if !wasHealthy {
		sc.state.Store(int32(StateConnected))
		sc.logger.Info().Msg("connection marked healthy")
	}
}

// MarkUnhealthy marks the connection as unhealthy.
func (sc *ServerConnection) MarkUnhealthy() {
	wasHealthy := sc.healthy.Swap(false)

	if wasHealthy {
		sc.state.Store(int32(StateUnhealthy))
		sc.logger.Warn().Msg("connection marked unhealthy")
	}
}

// LastHeartbeat returns the timestamp of the last successful heartbeat.
// Returns zero time if no heartbeat has been sent.
func (sc *ServerConnection) LastHeartbeat() time.Time {
	ns := sc.lastHeartbeat.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// CheckHealth checks if the connection is healthy based on the heartbeat timeout.
// If the time since the last heartbeat exceeds the timeout, the connection is marked unhealthy.
func (sc *ServerConnection) CheckHealth(timeout time.Duration) bool {
	lastHB := sc.LastHeartbeat()
	if lastHB.IsZero() {
		// No heartbeat yet - consider unhealthy if we've been connected for a while
		return false
	}

	if time.Since(lastHB) > timeout {
		sc.MarkUnhealthy()
		return false
	}

	return sc.IsHealthy()
}

// --- Bidirectional Heartbeat Methods ---

// LastReceivedFromServer returns the timestamp of when the last heartbeat was received from the server.
// Returns zero time if no heartbeat has been received yet.
func (sc *ServerConnection) LastReceivedFromServer() time.Time {
	ns := sc.lastReceivedFromServer.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// UpdateLastReceivedFromServer updates the timestamp of when a heartbeat was received from the server.
// This method is thread-safe using atomic operations.
func (sc *ServerConnection) UpdateLastReceivedFromServer() {
	sc.lastReceivedFromServer.Store(time.Now().UnixNano())
	sc.logger.Debug().Msg("received heartbeat from server")
}

// SetHealthConfig configures the health check parameters for the connection.
// healthTimeout is the maximum duration allowed between received heartbeats before marking unhealthy.
func (sc *ServerConnection) SetHealthConfig(healthTimeout time.Duration) {
	sc.healthTimeout = healthTimeout
}

// SetReconnectCallback sets the callback function to be called when reconnection is needed.
// This is called when the health check detects a timeout.
func (sc *ServerConnection) SetReconnectCallback(callback ReconnectionCallback) {
	sc.reconnectCallback = callback
}

// CheckReceivedHealth checks if the connection is healthy based on received heartbeats.
// Returns true if a heartbeat has been received within the configured healthTimeout.
// Returns false if no heartbeat has been received or if the timeout has been exceeded.
func (sc *ServerConnection) CheckReceivedHealth() bool {
	lastReceived := sc.LastReceivedFromServer()
	if lastReceived.IsZero() {
		return false
	}
	return time.Since(lastReceived) <= sc.healthTimeout
}

// heartbeatLoop handles bidirectional heartbeat messages in a single goroutine.
// It sends heartbeats to the server at the configured interval,
// receives heartbeats from the server updating lastReceivedFromServer timestamp,
// and checks for heartbeat timeout to detect unhealthy connection.
func (sc *ServerConnection) heartbeatLoop(sendInterval time.Duration) {
	sc.logger.Debug().
		Dur("send_interval", sendInterval).
		Dur("health_timeout", sc.healthTimeout).
		Msg("starting heartbeat loop")

	// Create a ticker for sending heartbeats
	sendTicker := time.NewTicker(sendInterval)
	defer sendTicker.Stop()

	// Channel to receive messages from the read goroutine
	type readResult struct {
		msgType byte
		payload []byte
		err     error
	}
	readCh := make(chan readResult, 1)

	// Start a goroutine to read messages
	go func(ctx context.Context, stream *quic.Stream, readCh chan readResult) {
		for {
			if stream == nil {
				return
			}
			msgType, payload, err := protocol.ReadMessage(stream)
			select {
			case readCh <- readResult{msgType: msgType, payload: payload, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}(sc.ctx, sc.controlStream, readCh)

	heartbeatDeadline := time.After(sc.healthTimeout)
	for {
		select {
		case <-sc.ctx.Done():
			sc.logger.Debug().Msg("heartbeat loop stopped: context cancelled")
			return

		case <-sendTicker.C:
			// Send heartbeat to server
			if err := sc.SendHeartbeat(); err != nil {
				sc.logger.Debug().Err(err).Msg("heartbeat send failed, exiting loop")
				return
			}

		case result := <-readCh:
			if result.err != nil {
				sc.logger.Debug().Err(result.err).Msg("read from control stream failed")
				// Mark unhealthy and trigger reconnection on read error
				sc.MarkUnhealthy()
				if sc.reconnectCallback != nil {
					sc.reconnectCallback(sc.serverAddr)
				}
				return
			}

			if result.msgType == protocol.MsgTypeHeartbeat {
				// Update last received timestamp and reset deadline
				sc.UpdateLastReceivedFromServer()
				sc.logger.Debug().Msg("heartbeat received from server")
				heartbeatDeadline = time.After(sc.healthTimeout)
			} else {
				// Route non-heartbeat messages to handler
				if nonHeartbeatHandler != nil {
					if err := nonHeartbeatHandler(result.msgType, result.payload); err != nil {
						sc.logger.Warn().
							Uint8("msg_type", result.msgType).
							Err(err).
							Msg("error handling non-heartbeat message")
					}
				} else {
					sc.logger.Debug().
						Uint8("msg_type", result.msgType).
						Msg("received non-heartbeat message (no handler set)")
				}
			}

		case <-heartbeatDeadline:
			lastReceived := sc.LastReceivedFromServer()
			timeSinceLastReceived := time.Since(lastReceived)
			sc.logger.Warn().
				Dur("time_since_last_received", timeSinceLastReceived).
				Dur("timeout", sc.healthTimeout).
				Msg("server heartbeat timeout")

			// Mark connection as unhealthy
			sc.MarkUnhealthy()

			// Trigger reconnection if callback is set
			if sc.reconnectCallback != nil {
				sc.logger.Info().Msg("triggering reconnection due to health timeout")
				sc.reconnectCallback(sc.serverAddr)
			}
			return
		}
	}
}

// NonHeartbeatHandler is a function type for handling non-heartbeat messages received on the control stream.
// It receives the message type and payload, and returns an error if handling fails.
type NonHeartbeatHandler func(msgType byte, payload []byte) error

// nonHeartbeatHandler is the handler for non-heartbeat messages.
// If nil, non-heartbeat messages are logged and ignored.
var nonHeartbeatHandler NonHeartbeatHandler

// SetNonHeartbeatHandler sets the handler for non-heartbeat messages received on the control stream.
// This allows routing of non-heartbeat messages to appropriate handlers without blocking heartbeat processing.
func (sc *ServerConnection) SetNonHeartbeatHandler(handler NonHeartbeatHandler) {
	nonHeartbeatHandler = handler
}

// StartHeartbeatLoops starts the unified heartbeat loop for this connection.
// The loop handles sending heartbeats, receiving heartbeats, and health checking
// all in a single goroutine (similar to server-side implementation).
//
// All operations use the same connection context for coordinated shutdown.
func (sc *ServerConnection) StartHeartbeatLoops(heartbeatInterval time.Duration) {
	sc.logger.Debug().
		Dur("heartbeat_interval", heartbeatInterval).
		Dur("health_timeout", sc.healthTimeout).
		Msg("starting heartbeat loop")

	go sc.heartbeatLoop(heartbeatInterval)
}

// --- Connection Lifecycle Methods ---

// Register sends a registration message to the server and waits for acknowledgment.
// This should be called after Connect() succeeds.
func (sc *ServerConnection) Register(clientID string) error {
	if sc.conn == nil {
		return fmt.Errorf("not connected")
	}

	sc.logger.Info().Str("client_id", clientID).Msg("registering with server")

	// Open control stream
	stream, err := sc.conn.OpenStreamSync(sc.ctx)
	if err != nil {
		return fmt.Errorf("open control stream: %w", err)
	}
	sc.controlStream = stream

	// Send registration message
	err = protocol.WriteRegister(
		stream,
		clientID,
		protocol.ProtocolVersion,
		config.DefaultCapabilities,
	)
	if err != nil {
		return fmt.Errorf("send registration: %w", err)
	}

	// Read acknowledgment
	var ackMsg protocol.RegisterAckMsg
	if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeRegisterAck, &ackMsg); err != nil {
		return fmt.Errorf("read registration ack: %w", err)
	}

	if !ackMsg.Success {
		return fmt.Errorf("registration failed: %s", ackMsg.Message)
	}

	// Mark as healthy after successful registration
	sc.MarkHealthy()

	sc.logger.Info().Str("message", ackMsg.Message).Msg("registered with server")
	return nil
}

// SendHeartbeat sends a heartbeat message on the control stream.
// This is a non-blocking operation that does not wait for any response.
// On success, the connection is marked healthy. On failure, it's marked unhealthy
// and reconnection is triggered if a callback is set.
func (sc *ServerConnection) SendHeartbeat() error {
	if sc.controlStream == nil {
		// No control stream is a failure condition - mark unhealthy (Requirement 1.3)
		sc.MarkUnhealthy()
		sc.logger.Error().Msg("heartbeat send failed: no control stream")

		// Trigger reconnection if callback is set (Requirement 1.3)
		if sc.reconnectCallback != nil {
			sc.logger.Info().Msg("triggering reconnection due to missing control stream")
			sc.reconnectCallback(sc.serverAddr)
		}

		return fmt.Errorf("no control stream")
	}

	// Non-blocking write - does not wait for any response (Requirement 1.2)
	// The heartbeat message contains a Unix timestamp (Requirement 1.4)
	err := protocol.WriteHeartbeat(sc.controlStream, time.Now().Unix())
	if err != nil {
		// Mark connection as unhealthy on write error (Requirement 1.3)
		sc.MarkUnhealthy()
		sc.logger.Error().Err(err).Msg("heartbeat send failed")

		// Trigger reconnection if callback is set (Requirement 1.3)
		if sc.reconnectCallback != nil {
			sc.logger.Info().Msg("triggering reconnection due to heartbeat write error")
			sc.reconnectCallback(sc.serverAddr)
		}

		return fmt.Errorf("send heartbeat: %w", err)
	}

	sc.MarkHealthy()
	sc.logger.Debug().Msg("heartbeat sent")
	return nil
}

// AcceptStream accepts an incoming stream from this server.
// This blocks until a stream is available or the context is cancelled.
func (sc *ServerConnection) AcceptStream(ctx context.Context) (*quic.Stream, error) {
	if sc.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	stream, err := sc.conn.AcceptStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("accept stream: %w", err)
	}

	return stream, nil
}

// Close gracefully closes the connection.
// Active streams can continue until they complete or fail naturally.
func (sc *ServerConnection) Close() error {
	sc.cancel()

	// Close control stream first
	if sc.controlStream != nil {
		sc.controlStream.Close()
		sc.controlStream = nil
	}

	// Always mark as disconnected and unhealthy
	sc.state.Store(int32(StateDisconnected))
	sc.healthy.Store(false)

	// Close QUIC connection if exists
	if sc.conn != nil {
		err := sc.conn.CloseWithError(0, "shutdown")
		sc.conn = nil
		sc.logger.Info().Msg("connection closed")
		return err
	}

	return nil
}

// ServerConnectionInfo provides connection status information for monitoring.
type ServerConnectionInfo struct {
	Address                string
	ServerName             string
	State                  ConnectionState
	Healthy                bool
	LastHeartbeat          time.Time
	LastReceivedFromServer time.Time
	ConnectedAt            time.Time
}

// Info returns current connection status information.
func (sc *ServerConnection) Info() ServerConnectionInfo {
	return ServerConnectionInfo{
		Address:                sc.serverAddr,
		ServerName:             sc.serverName,
		State:                  sc.State(),
		Healthy:                sc.IsHealthy(),
		LastHeartbeat:          sc.LastHeartbeat(),
		LastReceivedFromServer: sc.LastReceivedFromServer(),
	}
}
