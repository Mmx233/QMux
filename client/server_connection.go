package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
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

	conn          atomic.Pointer[quic.Conn]
	controlStream atomic.Pointer[quic.Stream]
	capabilities  []string

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

	controlOnce     sync.Once
	controlWG       sync.WaitGroup
	controlStarted  atomic.Bool
	controlDone     chan struct{}
	controlMu       sync.Mutex
	controlErr      error
	controlClosing  bool
	controlPending  atomic.Int64
	controlProgress chan struct{}
	drainCommands   chan drainCommand
	drainComplete   chan int64
	drainEpoch      atomic.Uint32
	writeDrain      func(*quic.Stream) error
	handlerMu       sync.RWMutex
	nonHeartbeat    NonHeartbeatHandler

	closeOnce sync.Once
	closeMu   sync.Mutex
	closed    bool
	closeErr  error
	closeDone chan struct{}
	onClosed  func()

	// Protected by the owning ConnectionManager.publishMu.
	capacityEndpoint int
	capacityPhase    clientGenerationPhase
}

type drainCommand struct {
	result chan error
}

type controlReadResult struct {
	msgType    byte
	payload    []byte
	drain      protocol.DrainCompleteMsg
	drainEpoch uint32
	err        error
}

const (
	drainNotRequested uint32 = iota
	drainRequestWriting
	drainRequestCommitted
	drainCompleteReceived
)

// NewServerConnection creates a new ServerConnection instance.
// The sessionCache should be obtained from SessionCacheManager to ensure
// proper isolation between different servers.
func NewServerConnection(serverAddr, serverName string, sessionCache tls.ClientSessionCache, logger zerolog.Logger) *ServerConnection {
	ctx, cancel := context.WithCancel(context.Background())

	sc := &ServerConnection{
		serverAddr:       serverAddr,
		serverName:       serverName,
		sessionCache:     sessionCache,
		capacityEndpoint: -1,
		logger: logger.With().
			Str("server_addr", serverAddr).
			Logger(),
		ctx:             ctx,
		cancel:          cancel,
		controlDone:     make(chan struct{}),
		controlProgress: make(chan struct{}, 1),
		drainCommands:   make(chan drainCommand, 1),
		drainComplete:   make(chan int64, 1),
		closeDone:       make(chan struct{}),
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
	dialAddr, originalHost, err := resolveServerAddress(ctx, net.DefaultResolver, sc.serverAddr)
	if err != nil {
		sc.state.Store(int32(StateDisconnected))
		return fmt.Errorf("resolve server %s: %w", sc.serverAddr, err)
	}
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = originalHost
	}

	conn, err := quic.DialAddr(ctx, dialAddr, tlsConfig, quicConfig)
	if err != nil {
		sc.state.Store(int32(StateDisconnected))
		return fmt.Errorf("dial server %s: %w", sc.serverAddr, err)
	}

	sc.conn.Store(conn)
	sc.state.Store(int32(StateConnected))
	sc.logger.Info().Msg("connected to server")

	return nil
}

func resolveServerAddress(ctx context.Context, resolver *net.Resolver, address string) (string, string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", "", err
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return address, host, nil
	}

	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", "", err
	}
	if len(addresses) == 0 {
		return "", "", fmt.Errorf("host %q resolved without an address", host)
	}
	selectedAddress := preferredServerIP(addresses)
	return net.JoinHostPort(selectedAddress.String(), port), host, nil
}

func preferredServerIP(addresses []net.IPAddr) net.IPAddr {
	for _, address := range addresses {
		if address.IP.To4() != nil {
			return address
		}
	}
	return addresses[0]
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
	return sc.conn.Load()
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
	if sc.ctx.Err() != nil {
		return
	}
	wasHealthy := sc.healthy.Swap(true)
	sc.lastHeartbeat.Store(time.Now().UnixNano())

	if !wasHealthy {
		sc.state.Store(int32(StateConnected))
		sc.logger.Info().Msg("connection marked healthy")
	}
	if sc.ctx.Err() != nil {
		sc.healthy.Store(false)
		sc.state.Store(int32(StateDisconnected))
	}
}

// MarkUnhealthy marks the connection as unhealthy.
func (sc *ServerConnection) MarkUnhealthy() {
	if sc.ctx.Err() != nil {
		sc.healthy.Store(false)
		sc.state.Store(int32(StateDisconnected))
		return
	}
	wasHealthy := sc.healthy.Swap(false)

	if wasHealthy {
		sc.state.Store(int32(StateUnhealthy))
		sc.logger.Warn().Msg("connection marked unhealthy")
	}
	if sc.ctx.Err() != nil {
		sc.healthy.Store(false)
		sc.state.Store(int32(StateDisconnected))
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
func (sc *ServerConnection) heartbeatLoop(sendInterval time.Duration, controlStream *quic.Stream) error {
	sc.logger.Debug().
		Dur("send_interval", sendInterval).
		Dur("health_timeout", sc.healthTimeout).
		Msg("starting heartbeat loop")

	// Create a ticker for sending heartbeats
	sendTicker := time.NewTicker(sendInterval)
	defer sendTicker.Stop()

	if controlStream == nil {
		return fmt.Errorf("no control stream")
	}
	readCh := make(chan controlReadResult, 1)
	readerCtx, cancelReader := context.WithCancel(sc.ctx)
	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)
		for {
			msgType, payload, err := protocol.ReadMessage(controlStream)
			result := controlReadResult{msgType: msgType, payload: payload, err: err}
			if err == nil && msgType == protocol.MsgTypeDrainComplete {
				result.drain, result.err = protocol.DecodeDrainComplete(payload)
			}
			result.drainEpoch = sc.drainEpoch.Load()
			sc.controlPending.Add(1)
			select {
			case readCh <- result:
			case <-readerCtx.Done():
				sc.controlPending.Add(-1)
				return
			}
			if result.err != nil {
				return
			}
		}
	}()
	defer func() {
		cancelReader()
		controlStream.CancelRead(0)
		<-readerDone
	}()

	now := time.Now()
	healthExpiry := now.Add(sc.healthTimeout)
	heartbeatDeadline := time.After(time.Until(healthExpiry))
	completeFence := int64(-2)
	for {
		select {
		case <-sc.ctx.Done():
			sc.logger.Debug().Msg("heartbeat loop stopped: context cancelled")
			return nil

		case command := <-sc.drainCommands:
			sc.drainEpoch.Store(drainRequestWriting)
			now := time.Now()
			writeDeadline, fresh := drainWriteDeadline(now, sendInterval, healthExpiry)
			var err error
			if !fresh {
				err = fmt.Errorf("drain request deadline expired")
			} else if err = controlStream.SetWriteDeadline(writeDeadline); err == nil {
				if sc.writeDrain == nil {
					err = protocol.WriteDrainRequest(controlStream)
				} else {
					err = sc.writeDrain(controlStream)
				}
			}
			if err == nil {
				sc.drainEpoch.Store(drainRequestCommitted)
			}
			command.result <- err
			if err != nil {
				return fmt.Errorf("write drain request: %w", err)
			}

		case <-sendTicker.C:
			now := time.Now()
			writeDeadline := now.Add(sendInterval)
			if healthExpiry.Before(writeDeadline) {
				writeDeadline = healthExpiry
			}
			if !writeDeadline.After(now) {
				continue
			}
			if err := writeHeartbeat(controlStream, writeDeadline); err != nil {
				sc.logger.Debug().Err(err).Msg("heartbeat send failed, exiting loop")
				return fmt.Errorf("send heartbeat: %w", err)
			}
			sc.MarkHealthy()
			sc.logger.Debug().Msg("heartbeat sent")

		case result := <-readCh:
			if result.err != nil {
				sc.logger.Debug().Err(result.err).Msg("read from control stream failed")
				return fmt.Errorf("read control stream: %w", result.err)
			}

			switch result.msgType {
			case protocol.MsgTypeHeartbeat:
				// Update last received timestamp and reset deadline
				sc.UpdateLastReceivedFromServer()
				sc.logger.Debug().Msg("heartbeat received from server")
				healthExpiry = time.Now().Add(sc.healthTimeout)
				heartbeatDeadline = time.After(time.Until(healthExpiry))
			case protocol.MsgTypeDrainComplete:
				if !sc.HasCapability(protocol.CapabilityTCPDrainV1) {
					return fmt.Errorf("unnegotiated drain complete")
				}
				currentEpoch := sc.drainEpoch.Load()
				if result.drainEpoch == drainNotRequested ||
					result.drainEpoch == drainRequestWriting && currentEpoch < drainRequestCommitted ||
					currentEpoch < drainRequestCommitted {
					return fmt.Errorf("unsolicited drain complete")
				}
				if currentEpoch == drainCompleteReceived {
					if completeFence != result.drain.AcceptFence {
						return fmt.Errorf("conflicting drain complete fence %d after %d", result.drain.AcceptFence, completeFence)
					}
					sc.controlReadDone()
					continue
				}
				completeFence = result.drain.AcceptFence
				sc.drainEpoch.Store(drainCompleteReceived)
				select {
				case sc.drainComplete <- result.drain.AcceptFence:
				default:
				}
			case protocol.MsgTypeDrainRequest:
				return fmt.Errorf("wrong-direction drain request")
			default:
				// Route non-heartbeat messages to handler
				sc.handlerMu.RLock()
				handler := sc.nonHeartbeat
				sc.handlerMu.RUnlock()
				if handler != nil {
					if err := handler(result.msgType, result.payload); err != nil {
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
			sc.controlReadDone()

		case <-heartbeatDeadline:
			lastReceived := sc.LastReceivedFromServer()
			timeSinceLastReceived := time.Since(lastReceived)
			sc.logger.Warn().
				Dur("time_since_last_received", timeSinceLastReceived).
				Dur("timeout", sc.healthTimeout).
				Msg("server heartbeat timeout")

			return fmt.Errorf("server heartbeat timeout")
		}
	}
}

func drainWriteDeadline(now time.Time, interval time.Duration, healthExpiry time.Time) (time.Time, bool) {
	deadline := now.Add(interval)
	if healthExpiry.Before(deadline) {
		deadline = healthExpiry
	}
	return deadline, deadline.After(now)
}

func (sc *ServerConnection) controlReadDone() {
	sc.controlPending.Add(-1)
	select {
	case sc.controlProgress <- struct{}{}:
	default:
	}
}

// NonHeartbeatHandler is a function type for handling non-heartbeat messages received on the control stream.
// It receives the message type and payload, and returns an error if handling fails.
type NonHeartbeatHandler func(msgType byte, payload []byte) error

// SetNonHeartbeatHandler sets the handler for non-heartbeat messages received on the control stream.
// This allows routing of non-heartbeat messages to appropriate handlers without blocking heartbeat processing.
func (sc *ServerConnection) SetNonHeartbeatHandler(handler NonHeartbeatHandler) {
	sc.handlerMu.Lock()
	sc.nonHeartbeat = handler
	sc.handlerMu.Unlock()
}

// StartHeartbeatLoops starts the unified heartbeat loop for this connection.
// The loop handles sending heartbeats, receiving heartbeats, and health checking
// all in a single goroutine (similar to server-side implementation).
//
// All operations use the same connection context for coordinated shutdown.
func (sc *ServerConnection) StartHeartbeatLoops(heartbeatInterval time.Duration) {
	sc.controlMu.Lock()
	if sc.controlClosing {
		sc.controlMu.Unlock()
		return
	}
	sc.controlOnce.Do(func() {
		sc.logger.Debug().
			Dur("heartbeat_interval", heartbeatInterval).
			Dur("health_timeout", sc.healthTimeout).
			Msg("starting heartbeat loop")
		sc.controlStarted.Store(true)
		controlStream := sc.controlStream.Load()
		sc.controlWG.Go(func() {
			err := sc.heartbeatLoop(heartbeatInterval, controlStream)
			if err != nil && sc.ctx.Err() == nil {
				sc.MarkUnhealthy()
				if sc.reconnectCallback != nil {
					sc.reconnectCallback(sc.serverAddr)
				}
			}
			sc.controlMu.Lock()
			sc.controlErr = err
			sc.controlMu.Unlock()
			close(sc.controlDone)
		})
	})
	sc.controlMu.Unlock()
}

// HasCapability reports whether registration selected capability for this generation.
func (sc *ServerConnection) HasCapability(capability string) bool {
	return slices.Contains(sc.capabilities, capability)
}

func (sc *ServerConnection) requestDrain(ctx context.Context) error {
	if !sc.controlStarted.Load() {
		return fmt.Errorf("control loop not started")
	}
	result := make(chan error, 1)
	select {
	case sc.drainCommands <- drainCommand{result: result}:
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-sc.ctx.Done():
		return context.Cause(sc.ctx)
	case <-sc.controlDone:
		return sc.controlResult()
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-sc.ctx.Done():
		return context.Cause(sc.ctx)
	case <-sc.controlDone:
		return sc.controlResult()
	}
}

func (sc *ServerConnection) controlResult() error {
	sc.controlMu.Lock()
	defer sc.controlMu.Unlock()
	if sc.controlErr == nil {
		return fmt.Errorf("control loop stopped")
	}
	return sc.controlErr
}

func (sc *ServerConnection) controlAlive() bool {
	if !sc.controlStarted.Load() {
		return false
	}
	select {
	case <-sc.controlDone:
		return false
	default:
		return true
	}
}

func (sc *ServerConnection) waitControl() {
	sc.controlMu.Lock()
	sc.controlClosing = true
	sc.controlMu.Unlock()
	sc.controlWG.Wait()
}

// --- Connection Lifecycle Methods ---

// Register sends an mTLS registration message to the server and waits for acknowledgment.
// It is retained as the source-compatible wrapper for callers using the default
// authentication mode.
func (sc *ServerConnection) Register(ctx context.Context, clientID string) error {
	return sc.RegisterWithAuth(ctx, clientID, config.ClientAuth{Method: config.ClientAuthMethodMTLS})
}

// RegisterWithAuth sends a registration message to the server and waits for acknowledgment.
// The context governs opening the stream, writing the registration, and reading and
// validating the acknowledgment. A Background context has no registration deadline;
// ConnectionManager supplies its internal 30-second per-attempt deadline.
// Success installs the provisional control stream; ConnectionManager owns health,
// publication, and heartbeat startup.
// This should be called after Connect succeeds.
func (sc *ServerConnection) RegisterWithAuth(ctx context.Context, clientID string, auth config.ClientAuth) error {
	conn := sc.conn.Load()
	if conn == nil {
		return fmt.Errorf("not connected")
	}
	auth.ApplyDefaults()
	if err := auth.Validate(); err != nil {
		return fmt.Errorf("invalid client authentication: %w", err)
	}
	if err := sc.waitForHandshake(ctx, conn); err != nil {
		return err
	}

	capabilities := config.DefaultCapabilities
	expectedAuthScheme := ""
	var registerAuth *protocol.RegisterAuth
	if auth.Method == config.ClientAuthMethodToken {
		expectedAuthScheme = sharedtoken.Scheme
		proof, err := sharedtoken.Compute(
			[]byte(auth.Token),
			sharedtoken.Transcript{
				ClientID:     clientID,
				Version:      protocol.ProtocolVersion,
				Capabilities: capabilities,
			},
			conn.ConnectionState().TLS,
		)
		if err != nil {
			return fmt.Errorf("compute token authentication proof: %w", err)
		}
		registerAuth = &protocol.RegisterAuth{
			Scheme: sharedtoken.Scheme,
			Proof:  proof,
		}
	}

	sc.logger.Info().Str("client_id", clientID).Msg("registering with server")

	// Open control stream
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return registrationIOError(ctx, "open control stream", err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		stream.CancelRead(0)
		stream.CancelWrite(0)
		_ = stream.Close()
	}()

	if deadline, ok := ctx.Deadline(); ok {
		if err := stream.SetDeadline(deadline); err != nil {
			return registrationIOError(ctx, "set registration deadline", err)
		}
	}

	cancelUnblocked := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		defer close(cancelUnblocked)
		_ = stream.SetDeadline(time.Now())
	})
	stopCalled := false
	stopAndWait := func() bool {
		stopCalled = true
		if stopCancellation() {
			return true
		}
		<-cancelUnblocked
		return false
	}
	defer func() {
		if !stopCalled {
			_ = stopAndWait()
		}
	}()

	// Send registration message
	err = protocol.WriteRegisterWithAuth(
		stream,
		clientID,
		protocol.ProtocolVersion,
		capabilities,
		registerAuth,
	)
	if err != nil {
		return registrationIOError(ctx, "send registration", err)
	}

	// Read acknowledgment
	var ackMsg protocol.RegisterAckMsg
	if err := protocol.ReadTypedMessageLimited(
		stream,
		protocol.MsgTypeRegisterAck,
		&ackMsg,
		protocol.MaxRegistrationPayloadSize,
	); err != nil {
		return registrationIOError(ctx, "read registration ack", err)
	}

	if err := sc.acceptRegisterAckWithAuth(ackMsg, expectedAuthScheme); err != nil {
		return err
	}
	sc.capabilities = slices.Clone(ackMsg.SelectedCapabilities)
	if !stopAndWait() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("registration canceled: %w", err)
		}
		return fmt.Errorf("registration cancellation callback already started")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("registration canceled: %w", err)
	}
	if err := stream.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear registration deadline: %w", err)
	}

	sc.controlStream.Store(stream)
	committed = true

	sc.logger.Info().Str("message", ackMsg.Message).Msg("registered with server")
	return nil
}

func (sc *ServerConnection) waitForHandshake(ctx context.Context, conn *quic.Conn) error {
	select {
	case <-conn.HandshakeComplete():
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for TLS handshake: %w", ctx.Err())
	case <-conn.Context().Done():
		return fmt.Errorf("wait for TLS handshake: connection closed: %w", context.Cause(conn.Context()))
	}
}

func registrationIOError(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w: %w", operation, ctxErr, err)
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return fmt.Errorf("%s: %w: %w", operation, context.DeadlineExceeded, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (sc *ServerConnection) acceptRegisterAck(ack protocol.RegisterAckMsg) error {
	return protocol.ValidateRegisterAck(ack)
}

func (sc *ServerConnection) acceptRegisterAckWithAuth(ack protocol.RegisterAckMsg, expectedAuthScheme string) error {
	return protocol.ValidateRegisterAckWithAuth(ack, expectedAuthScheme)
}

// SendHeartbeat sends a heartbeat message on the control stream with a health-timeout deadline.
// It must not run concurrently with the active heartbeat loop.
// On success, the connection is marked healthy. On failure, it's marked unhealthy
// and reconnection is triggered if a callback is set.
func (sc *ServerConnection) SendHeartbeat() error {
	timeout := sc.healthTimeout
	if timeout <= 0 {
		timeout = config.DefaultHealthTimeout
	}
	return sc.sendHeartbeat(sc.controlStream.Load(), time.Now().Add(timeout))
}

func (sc *ServerConnection) sendHeartbeat(controlStream *quic.Stream, deadline time.Time) error {
	err := writeHeartbeat(controlStream, deadline)
	if err != nil {
		// Heartbeat write failures mark the generation unhealthy (Requirement 1.3).
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

func writeHeartbeat(controlStream *quic.Stream, deadline time.Time) error {
	if controlStream == nil {
		return fmt.Errorf("no control stream")
	}
	if err := controlStream.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return protocol.WriteHeartbeat(controlStream, time.Now().Unix())
}

// AcceptStream accepts an incoming stream from this server.
// This blocks until a stream is available or the context is cancelled.
func (sc *ServerConnection) AcceptStream(ctx context.Context) (*quic.Stream, error) {
	conn := sc.conn.Load()
	if conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("accept stream: %w", err)
	}

	return stream, nil
}

// Close abruptly closes the connection and all active streams.
func (sc *ServerConnection) Close() error {
	sc.closeOnce.Do(func() {
		sc.controlMu.Lock()
		sc.controlClosing = true
		sc.controlMu.Unlock()
		sc.cancel()

		if controlStream := sc.controlStream.Swap(nil); controlStream != nil {
			_ = controlStream.Close()
		}

		sc.state.Store(int32(StateDisconnected))
		sc.healthy.Store(false)

		if conn := sc.conn.Swap(nil); conn != nil {
			sc.closeErr = conn.CloseWithError(0, "shutdown")
			sc.logger.Info().Msg("connection closed")
		}
		sc.closeMu.Lock()
		sc.closed = true
		onClosed := sc.onClosed
		sc.closeMu.Unlock()
		if onClosed != nil {
			onClosed()
		}
		close(sc.closeDone)
	})
	return sc.closeErr
}

func (sc *ServerConnection) setOnClosed(onClosed func()) bool {
	sc.closeMu.Lock()
	if !sc.closed {
		sc.onClosed = onClosed
		sc.closeMu.Unlock()
		return true
	}
	sc.closeMu.Unlock()
	return false
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
