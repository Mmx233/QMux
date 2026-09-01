package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/auth"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/Mmx233/QMux/server/tls/stek"
	"github.com/Mmx233/QMux/server/traffic"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	registrationTimeout         = 10 * time.Second
	registrationErrorCode       = quic.ApplicationErrorCode(1)
	registrationStreamErrorCode = quic.StreamErrorCode(1)
	registrationFailureReason   = "registration failed"
)

// Server represents the QMux server
type Server struct {
	config               *config.Server
	pools                map[string]*pool.ConnectionPool // quicAddr -> pool
	handshakes           map[string]*handshakeStats      // quicAddr -> pre-Accept handshakes
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
	TCPAdmission       traffic.TCPAdmissionSnapshot
	UDPAdmission       traffic.UDPAdmissionSnapshot
	Handshake          HandshakeSnapshot
	PoolCapacity       pool.CapacitySnapshot
	Ready              bool
}

// HandshakeSnapshot is a point-in-time, value-only view of pre-Accept QUIC
// handshakes for one listener.
type HandshakeSnapshot struct {
	Current          int64
	HighWater        int64
	AccountingFaults uint64
}

type handshakeStats struct {
	mu               sync.Mutex
	current          atomic.Int64
	highWater        atomic.Int64
	accountingFaults atomic.Uint64
}

func (s *handshakeStats) snapshot() HandshakeSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return HandshakeSnapshot{
		Current:          s.current.Load(),
		HighWater:        s.highWater.Load(),
		AccountingFaults: s.accountingFaults.Load(),
	}
}

func (s *handshakeStats) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.current.Add(1)
	for high := s.highWater.Load(); current > high && !s.highWater.CompareAndSwap(high, current); high = s.highWater.Load() {
	}
}

func (s *handshakeStats) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		current := s.current.Load()
		if current == 0 {
			s.accountingFaults.Add(1)
			return
		}
		if s.current.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func (s *handshakeStats) tracer(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
	return &handshakeTrace{stats: s}
}

type handshakeTrace struct {
	stats *handshakeStats
	start sync.Once
	end   sync.Once
}

func (t *handshakeTrace) AddProducer() qlogwriter.Recorder {
	t.start.Do(t.stats.start)
	return &handshakeRecorder{trace: t}
}

func (*handshakeTrace) SupportsSchemas(string) bool { return true }

func (t *handshakeTrace) finish() {
	t.end.Do(t.stats.finish)
}

type handshakeRecorder struct {
	trace *handshakeTrace
	once  sync.Once
}

func (r *handshakeRecorder) RecordEvent(event qlogwriter.Event) {
	switch event.(type) {
	case qlog.ALPNInformation, *qlog.ALPNInformation:
		r.trace.finish()
	}
}

func (r *handshakeRecorder) Close() error {
	r.once.Do(r.trace.finish)
	return nil
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

	conf.ApplyDefaults()
	if err := conf.Validate(); err != nil {
		return nil, fmt.Errorf("invalid server config: %w", err)
	}

	ownedConfig := *conf
	ownedConfig.Listeners = cloneListeners(conf.Listeners)

	logger := log.With().Str("com", "server").Logger()

	// Load TLS certificates
	if err := ownedConfig.TLS.LoadCertificates(); err != nil {
		return nil, fmt.Errorf("load certificates: %w", err)
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
	handshakes := make(map[string]*handshakeStats, len(ownedConfig.Listeners))
	for _, listener := range ownedConfig.Listeners {
		var balancer pool.LoadBalancer
		switch ownedConfig.LoadBalancer {
		case "round-robin":
			balancer = pool.NewRoundRobinBalancer()
		default:
			balancer = pool.NewLeastConnectionsBalancer()
		}
		p := pool.NewWithLimits(listener.QuicAddr, balancer, logger, poolLimitsFromCapacity(listener.Capacity))

		pools[listener.QuicAddr] = p
		handshakes[listener.QuicAddr] = &handshakeStats{}
		logger.Info().
			Str("quic_addr", listener.QuicAddr).
			Str("balancer", balancer.Name()).
			Msg("created connection pool")
	}

	srv := &Server{
		config:               &ownedConfig,
		pools:                pools,
		handshakes:           handshakes,
		authenticator:        authenticator,
		registrationTimeout:  registrationTimeout,
		writeRegistrationAck: protocol.WriteRegisterAckWithAuth,
		logger:               logger,
	}
	srv.trafficManager = traffic.NewManager(srv.config, srv.pools, srv.logger)
	return srv, nil
}

func poolLimitsFromCapacity(capacity config.ListenerCapacity) pool.Limits {
	return pool.Limits{
		MaxClientGenerations:             int64(capacity.MaxClientGenerations),
		MaxPendingRegistrations:          int64(capacity.MaxPendingRegistrations),
		MaxTCPConnectionsPerGeneration:   int64(capacity.MaxTCPConnectionsPerGeneration),
		MaxPendingTCPSetupsPerGeneration: int64(capacity.MaxPendingTCPSetupsPerGeneration),
		MaxUDPSessionsPerGeneration:      int64(capacity.MaxUDPSessionsPerGeneration),
	}
}

func cloneListeners(listeners []config.QuicListener) []config.QuicListener {
	cloned := slices.Clone(listeners)
	for i := range cloned {
		if value := listeners[i].UDP.EnableFragmentation; value != nil {
			copied := *value
			cloned[i].UDP.EnableFragmentation = &copied
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
	var tcpAdmission []traffic.TCPAdmissionSnapshot
	var udpAdmission []traffic.UDPAdmissionSnapshot
	if s.trafficManager != nil {
		tcpAdmission = s.trafficManager.TCPAdmissionSnapshots()
		udpAdmission = s.trafficManager.UDPAdmissionSnapshots()
	}
	snapshot := Snapshot{
		Routes: make([]RouteSnapshot, 0, len(s.config.Listeners)),
		Ready:  len(s.config.Listeners) > 0,
	}
	for i, listener := range s.config.Listeners {
		route := RouteSnapshot{
			QuicAddr:    listener.QuicAddr,
			TrafficAddr: listener.TrafficAddr,
			Protocol:    listener.Protocol,
			Listening:   listening,
		}
		if i < len(tcpAdmission) {
			route.TCPAdmission = tcpAdmission[i]
		}
		if i < len(udpAdmission) {
			route.UDPAdmission = udpAdmission[i]
		}
		if connectionPool := s.pools[listener.QuicAddr]; connectionPool != nil {
			route.PoolCapacity = connectionPool.Snapshot()
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
		if handshakes := s.handshakes[listener.QuicAddr]; handshakes != nil {
			route.Handshake = handshakes.snapshot()
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
	handshakes := s.handshakes[listenerConf.QuicAddr]
	if handshakes == nil {
		handshakes = &handshakeStats{}
	}
	quicConf.Tracer = handshakes.tracer

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
	// Accept connections
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept connection: %w", err)
		}
		poolInst := s.pools[listenerConf.QuicAddr]
		pending := poolInst.BeginPending()
		if pending == nil {
			logger.Warn().Str("remote", conn.RemoteAddr().String()).Msg("pending registration limit reached")
			_ = conn.CloseWithError(registrationErrorCode, registrationFailureReason)
			continue
		}

		connectionWG.Go(func() {
			s.handleConnectionPending(ctx, conn, listenerConf.QuicAddr, pending)
		})
	}
}

// handleConnection handles a new QUIC connection
func (s *Server) handleConnection(
	ctx context.Context,
	conn *quic.Conn,
	quicAddr string,
) {
	poolInst := s.pools[quicAddr]
	pending := poolInst.BeginPending()
	if pending == nil {
		_ = conn.CloseWithError(registrationErrorCode, registrationFailureReason)
		return
	}
	s.handleConnectionPending(ctx, conn, quicAddr, pending)
}

func (s *Server) handleConnectionPending(
	ctx context.Context,
	conn *quic.Conn,
	quicAddr string,
	pending *pool.Reservation,
) {
	poolInst := s.pools[quicAddr]
	if pending == nil {
		_ = conn.CloseWithError(registrationErrorCode, registrationFailureReason)
		return
	}
	var registered *pool.ClientConn
	var retirement *pool.Retirement
	closeReason := registrationFailureReason
	defer func() {
		poolInst.Abort(pending)
		if registered != nil && retirement == nil {
			retirement = poolInst.BeginRetire(registered)
		}
		code := registrationErrorCode
		if registered != nil {
			code = 1
		}
		_ = conn.CloseWithError(code, closeReason)
		if retirement != nil {
			<-conn.Context().Done()
			retirement.Done()
		}
	}()
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
	if err := pending.Reserve(clientConn); err != nil {
		logger.Error().Err(err).Msg("reserve pool entry failed")
		_ = s.writeRegisterAck(controlStream, false, "registration unavailable", protocol.ProtocolVersion, nil, "")
		return
	}

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
	if err := poolInst.Commit(pending); err != nil {
		logger.Error().Err(err).Msg("commit pool entry failed")
		return
	}
	registered = clientConn
	closeReason = "control stream ended"

	// Stop registration cancellation before clearing the transaction deadline.
	stopWatch()
	cancelRegistration()
	if err := controlStream.SetDeadline(time.Time{}); err != nil {
		logger.Error().Err(err).Msg("clear registration stream deadline failed")
		return
	}
	closeReason, retirement = s.handleControlStream(ctx, poolInst, clientConn, quicAddr)
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
) (closeReason string, retirement *pool.Retirement) {
	closeReason = "control stream ended"
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
	defer func() { retirement = poolInst.BeginRetire(clientConn) }()

	now := time.Now()
	healthExpiry := now.Add(s.config.HealthTimeout)
	heartbeatDeadline := time.After(time.Until(healthExpiry))
	for {
		select {
		case <-ctx.Done():
			return

		case <-clientConn.Conn.Context().Done():
			// QUIC connection closed
			return

		case <-heartBeatTicker.C:
			now := time.Now()
			writeDeadline := now.Add(s.config.HeartbeatInterval)
			if healthExpiry.Before(writeDeadline) {
				writeDeadline = healthExpiry
			}
			if !writeDeadline.After(now) {
				continue
			}
			err := clientConn.ControlStream.SetWriteDeadline(writeDeadline)
			if err == nil {
				err = protocol.WriteHeartbeat(clientConn.ControlStream, now.Unix())
			}
			if err != nil {
				logger.Debug().Err(err).Msg("failed to send heartbeat to client")
				if !poolInst.MarkUnhealthy(clientConn) {
					logger.Debug().Msg("ignored stale heartbeat write failure")
				}
				closeReason = "heartbeat write failed"
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
				healthExpiry = time.Now().Add(s.config.HealthTimeout)
				heartbeatDeadline = time.After(time.Until(healthExpiry))
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
			closeReason = "heartbeat timeout"
			return
		}
	}
}
