package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/auth"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/Mmx233/QMux/server/tls/stek"
	"github.com/Mmx233/QMux/server/traffic"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	registrationTimeout         = 10 * time.Second
	maxPendingRegistrations     = 128
	registrationErrorCode       = quic.ApplicationErrorCode(1)
	registrationStreamErrorCode = quic.StreamErrorCode(1)
	registrationFailureReason   = "registration failed"
)

// Server represents the QMux server
type Server struct {
	config               *config.Server
	pools                map[string]*pool.ConnectionPool // quicAddr -> pool
	trafficManager       *traffic.Manager
	authenticator        auth.Auth
	registrationTimeout  time.Duration
	writeRegistrationAck registrationAckWriter
	logger               zerolog.Logger
}

// Snapshot is a point-in-time, value-only view of server readiness.
type Snapshot struct {
	Routes []RouteSnapshot
	Ready  bool
}

// RouteSnapshot describes one configured traffic route.
type RouteSnapshot struct {
	QuicAddr           string
	TrafficAddr        string
	Protocol           string
	Listening          bool
	TCPEligibleClients int
	UDPEligibleClients int
	Ready              bool
}

type registrationAckWriter func(
	io.Writer,
	bool,
	string,
	string,
	[]string,
	string,
) error

type trafficLifecycle interface {
	Start(context.Context) error
	Close()
	Wait()
}

type listenerStartFunc func(context.Context, config.QuicListener) error

type listenerErrorState struct {
	mu           sync.Mutex
	first        error
	shuttingDown bool
	notify       chan struct{}
}

func newListenerErrorState() *listenerErrorState {
	return &listenerErrorState{notify: make(chan struct{})}
}

func (s *listenerErrorState) report(err error) {
	if err == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown || s.first != nil {
		return
	}
	s.first = err
	close(s.notify)
}

func (s *listenerErrorState) beginShutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shuttingDown = true
	return s.first
}

// New creates a new server
func New(conf *config.Server) (*Server, error) {
	if conf == nil {
		return nil, errors.New("server config is nil")
	}

	// Apply defaults to ensure all required fields have values
	conf.ApplyDefaults()
	if err := validateListeners(conf.Listeners); err != nil {
		return nil, fmt.Errorf("invalid listeners: %w", err)
	}

	ownedConfig := *conf
	ownedConfig.Listeners = cloneListeners(conf.Listeners)

	logger := log.With().Str("com", "server").Logger()

	// Load TLS certificates
	if err := ownedConfig.TLS.LoadCertificates(); err != nil {
		return nil, fmt.Errorf("load certificates: %w", err)
	}

	// Validate auth config
	if err := ownedConfig.Auth.Validate(); err != nil {
		return nil, fmt.Errorf("invalid auth config: %w", err)
	}

	// Create authenticator using factory
	authenticator, err := ownedConfig.Auth.CreateAuthenticator()
	if err != nil {
		return nil, fmt.Errorf("create authenticator: %w", err)
	}

	// Log the auth method being used
	method := ownedConfig.Auth.Method
	if method == "" {
		method = "mtls"
	}
	logger.Info().Str("method", method).Msg("authentication enabled")

	// Create connection pools for each listener
	pools := make(map[string]*pool.ConnectionPool) // quicAddr -> pool
	for _, listener := range ownedConfig.Listeners {
		var balancer pool.LoadBalancer
		switch ownedConfig.LoadBalancer {
		case "round-robin":
			balancer = pool.NewRoundRobinBalancer()
		default:
			balancer = pool.NewLeastConnectionsBalancer()
		}
		p := pool.New(listener.QuicAddr, balancer, logger)

		pools[listener.QuicAddr] = p
		logger.Info().
			Str("quic_addr", listener.QuicAddr).
			Str("balancer", balancer.Name()).
			Msg("created connection pool")
	}

	srv := &Server{
		config:               &ownedConfig,
		pools:                pools,
		authenticator:        authenticator,
		registrationTimeout:  registrationTimeout,
		writeRegistrationAck: protocol.WriteRegisterAckWithAuth,
		logger:               logger,
	}
	srv.trafficManager = traffic.NewManager(srv.config, srv.pools, srv.logger)
	return srv, nil
}

// validateListeners establishes the route invariants required by readiness.
// COR-005 owns broader configuration validation.
func validateListeners(listeners []config.QuicListener) error {
	if len(listeners) == 0 {
		return errors.New("at least one listener is required")
	}

	type socketClaim struct {
		network string
		address string
	}
	claims := make(map[socketClaim]string, len(listeners)*2)
	claim := func(network, address, field string) error {
		// Exact configured strings only; equivalent OS bind aliases still fail transactionally.
		key := socketClaim{network: network, address: address}
		if existing, ok := claims[key]; ok {
			return fmt.Errorf("%s conflicts with %s on %s socket %q", field, existing, network, address)
		}
		claims[key] = field
		return nil
	}

	for i, listener := range listeners {
		if err := validateListenerAddress(listener.QuicAddr); err != nil {
			return fmt.Errorf("listeners[%d].quic_addr: %w", i, err)
		}
		if err := validateListenerAddress(listener.TrafficAddr); err != nil {
			return fmt.Errorf("listeners[%d].traffic_addr: %w", i, err)
		}
		switch listener.Protocol {
		case "tcp", "udp", "both":
		default:
			return fmt.Errorf("listeners[%d].protocol must be tcp, udp, or both", i)
		}

		quicField := fmt.Sprintf("listeners[%d].quic_addr", i)
		if err := claim("udp", listener.QuicAddr, quicField); err != nil {
			return err
		}
		trafficField := fmt.Sprintf("listeners[%d].traffic_addr", i)
		if listener.Protocol == "tcp" || listener.Protocol == "both" {
			if err := claim("tcp", listener.TrafficAddr, trafficField); err != nil {
				return err
			}
		}
		if listener.Protocol == "udp" || listener.Protocol == "both" {
			if err := claim("udp", listener.TrafficAddr, trafficField); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateListenerAddress(address string) error {
	if address == "" {
		return errors.New("address cannot be empty")
	}
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid address format %q: %w", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return fmt.Errorf("invalid port in address %q: %w", address, err)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d in address %q", port, address)
	}
	return nil
}

func cloneListeners(listeners []config.QuicListener) []config.QuicListener {
	cloned := slices.Clone(listeners)
	for i := range cloned {
		if value := listeners[i].UDP.EnableFragmentation; value != nil {
			copied := *value
			cloned[i].UDP.EnableFragmentation = &copied
		}
		if value := listeners[i].UDP.EnableBufferPooling; value != nil {
			copied := *value
			cloned[i].UDP.EnableBufferPooling = &copied
		}
	}
	return cloned
}

// Start creates and runs a server for backward compatibility.
func Start(ctx context.Context, conf *config.Server) error {
	srv, err := New(conf)
	if err != nil {
		return err
	}
	return srv.Start(ctx)
}

// Start runs the server until cancellation or a component failure.
func (s *Server) Start(ctx context.Context) error {
	defer func() {
		for _, connectionPool := range s.pools {
			connectionPool.Stop()
		}
	}()

	return superviseServer(ctx, s.trafficManager, s.config.Listeners, s.startListener)
}

// Snapshot returns the current route and aggregate readiness state. It is
// race-free but intentionally not globally linearizable across route pools.
func (s *Server) Snapshot() Snapshot {
	listening := s.trafficManager != nil && s.trafficManager.Running()
	snapshot := Snapshot{
		Routes: make([]RouteSnapshot, 0, len(s.config.Listeners)),
		Ready:  len(s.config.Listeners) > 0,
	}
	for _, listener := range s.config.Listeners {
		route := RouteSnapshot{
			QuicAddr:    listener.QuicAddr,
			TrafficAddr: listener.TrafficAddr,
			Protocol:    listener.Protocol,
			Listening:   listening,
		}
		if connectionPool := s.pools[listener.QuicAddr]; connectionPool != nil {
			switch listener.Protocol {
			case "tcp":
				route.TCPEligibleClients = connectionPool.EligibleCount("tcp")
				route.Ready = listening && route.TCPEligibleClients > 0
			case "udp":
				route.UDPEligibleClients = connectionPool.EligibleCount("udp")
				route.Ready = listening && route.UDPEligibleClients > 0
			case "both":
				route.TCPEligibleClients = connectionPool.EligibleCount("tcp")
				route.UDPEligibleClients = connectionPool.EligibleCount("udp")
				route.Ready = listening && route.TCPEligibleClients > 0 && route.UDPEligibleClients > 0
			}
		}
		snapshot.Routes = append(snapshot.Routes, route)
		snapshot.Ready = snapshot.Ready && route.Ready
	}
	return snapshot
}

func superviseServer(
	ctx context.Context,
	trafficManager trafficLifecycle,
	listenerConfs []config.QuicListener,
	startListener listenerStartFunc,
) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}

	// Traffic Start is a startup transaction. Runtime ownership begins only
	// after it has successfully bound and launched all configured listeners.
	if err := trafficManager.Start(ctx); err != nil {
		trafficManager.Close()
		trafficManager.Wait()
		if cause := context.Cause(ctx); cause != nil &&
			(errors.Is(err, cause) || errors.Is(err, ctx.Err())) {
			return cause
		}
		return fmt.Errorf("start traffic manager: %w", err)
	}

	// QUIC listener cancellation is deliberately detached from caller
	// cancellation. This lets the supervisor initiate traffic shutdown before
	// closing QUIC transports and the established tunnels they own.
	listenerCtx, cancelListeners := context.WithCancelCause(context.WithoutCancel(ctx))
	var listenerWG sync.WaitGroup
	errorState := newListenerErrorState()
	for _, listenerConf := range listenerConfs {
		listenerWG.Add(1)
		go func(lc config.QuicListener) {
			defer listenerWG.Done()
			err := startListener(listenerCtx, lc)
			if err == nil && listenerCtx.Err() == nil {
				err = errors.New("listener stopped unexpectedly")
			}
			if err != nil {
				errorState.report(fmt.Errorf("listener on %s: %w", lc.QuicAddr, err))
			}
		}(listenerConf)
	}

	select {
	case <-ctx.Done():
	case <-errorState.notify:
	}

	firstErr := errorState.beginShutdown()
	shutdownCause := firstErr
	if shutdownCause == nil {
		shutdownCause = context.Cause(ctx)
	}
	if shutdownCause == nil {
		shutdownCause = context.Canceled
	}

	// Two-phase shutdown is important: stop admitting traffic, then close all
	// QUIC transports and join their connection handlers before waiting for
	// traffic tunnels to finish unwinding.
	trafficManager.Close()
	cancelListeners(shutdownCause)
	listenerWG.Wait()
	trafficManager.Wait()

	if firstErr != nil {
		return firstErr
	}
	return context.Cause(ctx)
}

// startListener starts a QUIC listener
func (s *Server) startListener(ctx context.Context, listenerConf config.QuicListener) error {
	logger := s.logger.With().Str("quic_addr", listenerConf.QuicAddr).Logger()

	// Parse QUIC address
	udpAddr, err := net.ResolveUDPAddr("udp", listenerConf.QuicAddr)
	if err != nil {
		return fmt.Errorf("resolve QUIC address: %w", err)
	}

	// Create UDP listener
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen UDP: %w", err)
	}
	defer func() { _ = udpConn.Close() }()

	// Initialize session ticket key rotation
	var stekManager *stek.RotateManager
	if s.config.TLS.SessionTicketEncryptionKeyRotationInterval > 0 {
		overlap := s.config.TLS.SessionTicketEncryptionKeyRotationOverlap
		if overlap == 0 {
			overlap = 2
		}

		stekManager, err = stek.NewRotateManager(
			s.config.TLS.SessionTicketEncryptionKeyRotationInterval,
			overlap,
		)
		if err != nil {
			return fmt.Errorf("initialize session ticket key rotation: %w", err)
		}

		logger.Info().
			Dur("rotation_interval", s.config.TLS.SessionTicketEncryptionKeyRotationInterval).
			Uint8("key_overlap", overlap).
			Msg("session ticket key rotation enabled")
	}

	// Configure TLS based on auth method
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{s.config.TLS.ServerCert},
	}

	// For mTLS, require and verify client certificates
	// For token auth, no client cert verification is needed
	if s.config.Auth.Method == "" || s.config.Auth.Method == "mtls" {
		tlsConf.ClientAuth = tls.RequireAndVerifyClientCert
		tlsConf.ClientCAs = s.config.Auth.CACertPool
	} else {
		// Token-based auth doesn't require client certificates
		tlsConf.ClientAuth = tls.NoClientCert
	}

	// Configure session ticket keys with automatic rotation
	if stekManager != nil {
		tlsConf.SetSessionTicketKeys(*stekManager.Keys.Load())
		tlsConf.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			cfg := tlsConf.Clone()
			cfg.SetSessionTicketKeys(*stekManager.Keys.Load())
			return cfg, nil
		}
	}

	// Get QUIC config
	quicConf := listenerConf.GetConfig()

	// Create QUIC transport
	tr := quic.Transport{
		Conn: udpConn,
	}

	ln, err := tr.Listen(tlsConf, quicConf)
	if err != nil {
		_ = tr.Close()
		return fmt.Errorf("listen QUIC: %w", err)
	}
	var connectionWG sync.WaitGroup
	defer func() {
		_ = ln.Close()
		_ = tr.Close()
		_ = udpConn.Close()
		connectionWG.Wait()
		if stekManager != nil {
			stekManager.Stop()
		}
	}()

	// Start session ticket key rotation
	if stekManager != nil {
		stekManager.Start(ctx)
	}

	logger.Info().
		Str("quic_addr", listenerConf.QuicAddr).
		Str("traffic_addr", listenerConf.TrafficAddr).
		Str("protocol", listenerConf.Protocol).
		Msg("QUIC listener started")
	pendingRegistrations := make(chan struct{}, maxPendingRegistrations)

	// Accept connections
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept connection: %w", err)
		}

		permit, ok := acquirePendingRegistration(pendingRegistrations)
		if !ok {
			logger.Warn().Str("remote", conn.RemoteAddr().String()).Msg("pending registration limit reached")
			_ = conn.CloseWithError(registrationErrorCode, registrationFailureReason)
			continue
		}

		connectionWG.Go(func() {
			s.handleConnection(ctx, conn, listenerConf.QuicAddr, permit)
		})
	}
}

type registrationPermit struct {
	slots chan struct{}
	once  sync.Once
}

func acquirePendingRegistration(slots chan struct{}) (*registrationPermit, bool) {
	select {
	case slots <- struct{}{}:
		return &registrationPermit{slots: slots}, true
	default:
		return nil, false
	}
}

func (p *registrationPermit) Release() {
	if p == nil {
		return
	}
	p.once.Do(func() { <-p.slots })
}

// handleConnection handles a new QUIC connection
func (s *Server) handleConnection(
	ctx context.Context,
	conn *quic.Conn,
	quicAddr string,
	registrationPermit *registrationPermit,
) {
	defer registrationPermit.Release()
	logger := s.logger.With().
		Str("remote", conn.RemoteAddr().String()).
		Str("quic_addr", quicAddr).
		Logger()

	logger.Info().Msg("new connection")

	timeout := s.registrationTimeout
	if timeout <= 0 {
		timeout = registrationTimeout
	}
	registrationCtx, cancelRegistration := context.WithTimeout(ctx, timeout)
	defer cancelRegistration()
	registrationDeadline, _ := registrationCtx.Deadline()
	registrationSucceeded := false
	defer func() {
		if !registrationSucceeded {
			_ = conn.CloseWithError(registrationErrorCode, registrationFailureReason)
		}
	}()

	// A completed handshake is the freshness boundary for exporter-bound auth.
	select {
	case <-conn.HandshakeComplete():
	case <-registrationCtx.Done():
		logger.Error().Err(context.Cause(registrationCtx)).Msg("TLS handshake did not complete during registration")
		return
	case <-conn.Context().Done():
		logger.Error().Err(context.Cause(conn.Context())).Msg("connection closed before TLS handshake completed")
		return
	}

	// Accept control stream (first stream from client)
	controlStream, err := conn.AcceptStream(registrationCtx)
	if err != nil {
		logger.Error().Err(err).Msg("accept control stream failed")
		return
	}
	if err := controlStream.SetDeadline(registrationDeadline); err != nil {
		logger.Error().Err(err).Msg("set registration stream deadline failed")
		return
	}

	// Stream deadlines bound stalls; cancellation also interrupts the stream
	// immediately instead of waiting for the remaining timeout.
	stopCancellationWatch := make(chan struct{})
	cancellationWatchStopped := make(chan struct{})
	go func() {
		defer close(cancellationWatchStopped)
		select {
		case <-ctx.Done():
			controlStream.CancelRead(registrationStreamErrorCode)
			controlStream.CancelWrite(registrationStreamErrorCode)
		case <-stopCancellationWatch:
		}
	}()
	var stopWatchOnce sync.Once
	stopWatch := func() {
		stopWatchOnce.Do(func() {
			close(stopCancellationWatch)
			<-cancellationWatchStopped
		})
	}
	defer stopWatch()

	// Read registration message
	var regMsg protocol.RegisterMsg
	if err := protocol.ReadTypedMessageLimited(
		controlStream,
		protocol.MsgTypeRegister,
		&regMsg,
		protocol.MaxRegistrationPayloadSize,
	); err != nil {
		logger.Error().Err(err).Msg("read registration failed")
		return
	}

	// The authenticator was selected by server configuration. Registration
	// fields can never select or switch the server's authentication policy.
	authRegistration := auth.Registration{
		ClientID:     regMsg.ClientID,
		Version:      regMsg.Version,
		Capabilities: regMsg.Capabilities,
	}
	if regMsg.Auth != nil {
		authRegistration.Scheme = regMsg.Auth.Scheme
		authRegistration.Proof = regMsg.Auth.Proof
	}
	if err := s.authenticator.Verify(conn.ConnectionState().TLS, authRegistration); err != nil {
		logger.Error().Err(err).Msg("authentication failed")
		return
	}

	logger = logger.With().
		Str("client_id", regMsg.ClientID).
		Str("version", regMsg.Version).
		Logger()

	// Reject incompatible peers before constructing or publishing a pool entry.
	if err := protocol.ValidateRegistration(regMsg.Version, regMsg.Capabilities); err != nil {
		logger.Warn().Err(err).Msg("registration negotiation failed")
		_ = s.writeRegisterAck(controlStream, false, err.Error(), protocol.ProtocolVersion, nil, "")
		return
	}
	selectedCapabilities := protocol.SelectCapabilities(regMsg.Capabilities, config.DefaultCapabilities)

	logger.Info().
		Strs("capabilities", regMsg.Capabilities).
		Msg("client registered")

	// Create client connection
	clientConn := &pool.ClientConn{
		ID:            regMsg.ClientID,
		Conn:          conn,
		ControlStream: controlStream,
		RegisteredAt:  time.Now(),
		LastSeen:      time.Now(),
		Metadata: pool.ClientMetadata{
			Version:      regMsg.Version,
			Capabilities: slices.Clone(selectedCapabilities),
		},
	}

	// Reserve the client ID without publishing it to traffic selection. The
	// connection becomes visible only after the success Ack is on the wire.
	poolInst := s.pools[quicAddr]
	reservation, err := poolInst.Reserve(clientConn)
	if err != nil {
		logger.Error().Err(err).Msg("reserve pool entry failed")
		_ = s.writeRegisterAck(controlStream, false, "registration unavailable", protocol.ProtocolVersion, nil, "")
		return
	}
	defer poolInst.Abort(reservation)

	selectedAuthScheme := s.authenticator.SelectedScheme()
	if err := s.writeRegisterAck(
		controlStream,
		true,
		"registered",
		protocol.ProtocolVersion,
		selectedCapabilities,
		selectedAuthScheme,
	); err != nil {
		logger.Error().Err(err).Msg("send ack failed")
		return
	}
	if err := poolInst.Commit(reservation); err != nil {
		logger.Error().Err(err).Msg("commit pool entry failed")
		return
	}
	defer func() {
		if !poolInst.Remove(clientConn) {
			logger.Warn().
				Time("registered_at", clientConn.RegisteredAt).
				Msg("client generation was not current during deferred cleanup")
		}
	}()

	// Stop registration cancellation before clearing the transaction deadline.
	stopWatch()
	cancelRegistration()
	if err := controlStream.SetDeadline(time.Time{}); err != nil {
		logger.Error().Err(err).Msg("clear registration stream deadline failed")
		return
	}
	registrationSucceeded = true
	registrationPermit.Release()

	s.handleControlStream(ctx, poolInst, clientConn, quicAddr)
}

func (s *Server) writeRegisterAck(
	w io.Writer,
	success bool,
	message, serverVersion string,
	selectedCapabilities []string,
	selectedAuthScheme string,
) error {
	writer := s.writeRegistrationAck
	if writer == nil {
		writer = protocol.WriteRegisterAckWithAuth
	}
	return writer(w, success, message, serverVersion, selectedCapabilities, selectedAuthScheme)
}

// handleControlStream handles bidirectional heartbeat messages on the control stream.
// It sends heartbeats to the client at the configured interval,
// receives heartbeats from the client updating LastSeen timestamp,
// and checks for heartbeat timeout to detect unhealthy clients.
func (s *Server) handleControlStream(
	ctx context.Context,
	poolInst *pool.ConnectionPool,
	clientConn *pool.ClientConn,
	quicAddr string,
) {
	ctx, cancel := context.WithCancel(ctx)

	logger := s.logger.With().
		Str("client_id", clientConn.ID).
		Time("registered_at", clientConn.RegisteredAt).
		Str("quic_addr", quicAddr).
		Logger()

	// Create a ticker for sending heartbeats
	heartBeatTicker := time.NewTicker(s.config.HeartbeatInterval)
	defer heartBeatTicker.Stop()

	// Channel to receive messages from the read goroutine
	type readResult struct {
		msgType byte
		err     error
	}
	readCh := make(chan readResult, 1)
	readerDone := make(chan struct{})

	// Start a goroutine to read messages
	go func(ctx context.Context, stream *quic.Stream, readCh chan readResult, conn *quic.Conn) {
		defer close(readerDone)
		for {
			msgType, _, err := protocol.ReadMessage(stream)
			select {
			case readCh <- readResult{msgType: msgType, err: err}:
			case <-ctx.Done():
				return
			case <-conn.Context().Done():
				return
			}
			if err != nil {
				return
			}
		}
	}(ctx, clientConn.ControlStream, readCh, clientConn.Conn)
	defer func() {
		cancel()
		clientConn.ControlStream.CancelRead(registrationStreamErrorCode)
		<-readerDone
	}()

	heartbeatDeadline := time.After(s.config.HealthTimeout)
	for {
		select {
		case <-ctx.Done():
			return

		case <-clientConn.Conn.Context().Done():
			// QUIC connection closed
			return

		case <-heartBeatTicker.C:
			// Send heartbeat to client
			if err := protocol.WriteHeartbeat(clientConn.ControlStream, time.Now().Unix()); err != nil {
				logger.Debug().Err(err).Msg("failed to send heartbeat to client")
				if !poolInst.MarkUnhealthy(clientConn) {
					logger.Debug().Msg("ignored stale heartbeat write failure")
				}
				return
			}
			logger.Debug().Msg("heartbeat sent to client")

		case result := <-readCh:
			if result.err != nil {
				logger.Debug().Err(result.err).Msg("read heartbeat failed")
				return
			}

			if result.msgType == protocol.MsgTypeHeartbeat {
				if !poolInst.UpdateLastSeen(clientConn) {
					logger.Debug().Msg("ignored heartbeat from stale client generation")
					return
				}
				logger.Debug().Msg("heartbeat received from client")
				heartbeatDeadline = time.After(s.config.HealthTimeout)
			}

		case <-heartbeatDeadline:
			timeSinceLastSeen := time.Since(clientConn.LastSeen)
			logger.Warn().
				Dur("time_since_last_seen", timeSinceLastSeen).
				Dur("timeout", s.config.HealthTimeout).
				Msg("client heartbeat timeout, closing connection")

			if !poolInst.MarkUnhealthy(clientConn) {
				logger.Debug().Msg("ignored timeout for stale client generation")
			}
			_ = clientConn.Conn.CloseWithError(1, "heartbeat timeout")
			return
		}
	}
}
