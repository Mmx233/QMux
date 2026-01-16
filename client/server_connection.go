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
// On success, the connection is marked healthy. On failure, it's marked unhealthy.
func (sc *ServerConnection) SendHeartbeat() error {
	if sc.controlStream == nil {
		return fmt.Errorf("no control stream")
	}

	err := protocol.WriteHeartbeat(sc.controlStream, time.Now().Unix())
	if err != nil {
		sc.MarkUnhealthy()
		sc.logger.Error().Err(err).Msg("heartbeat failed")
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
	Address       string
	ServerName    string
	State         ConnectionState
	Healthy       bool
	LastHeartbeat time.Time
	ConnectedAt   time.Time
}

// Info returns current connection status information.
func (sc *ServerConnection) Info() ServerConnectionInfo {
	return ServerConnectionInfo{
		Address:       sc.serverAddr,
		ServerName:    sc.serverName,
		State:         sc.State(),
		Healthy:       sc.IsHealthy(),
		LastHeartbeat: sc.LastHeartbeat(),
	}
}
