package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/internal/stats"
	"github.com/Mmx233/QMux/internal/tlsreload"
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
	registrationErrorCode       = quic.ApplicationErrorCode(1)
	registrationStreamErrorCode = quic.StreamErrorCode(1)
	registrationFailureReason   = "registration failed"
)

// Server represents the QMux server
type Server struct {
	config               *config.Server
	pools                map[string]*pool.ConnectionPool // quicAddr -> pool
	copyBufferPool       *protocol.CopyBufferPool
	trafficManager       *traffic.Manager
	authenticator        auth.Auth
	registrationTimeout  time.Duration
	writeRegistrationAck registrationAckWriter
	logger               zerolog.Logger
	tlsState             atomic.Pointer[serverTLSState]
	tlsReloader          *tlsreload.Reloader
}

type serverTLSState struct {
	certificate         tls.Certificate
	clientCAs           *x509.CertPool
	certificateNotAfter time.Time
	caNotAfter          time.Time
}

// Snapshot is a point-in-time, value-only view of server state.
type Snapshot struct {
	Routes                 []RouteSnapshot
	Ready                  bool
	TLSCertificateNotAfter time.Time
	TLSCANotAfter          time.Time
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
	PoolCapacity       pool.CapacitySnapshot
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

	conf.ApplyDefaults()
	if err := conf.Validate(); err != nil {
		return nil, fmt.Errorf("invalid server config: %w", err)
	}

	ownedConfig := cloneServerConfig(conf)

	logger := log.With().Str("com", "server").Logger()
	srv := &Server{config: &ownedConfig, logger: logger}
	paths := tlsreload.Paths{
		CertFile: ownedConfig.TLS.ServerCertFile,
		KeyFile:  ownedConfig.TLS.ServerKeyFile,
	}
	if ownedConfig.Auth.Method == "" || ownedConfig.Auth.Method == "mtls" {
		paths.CAFile = ownedConfig.Auth.CACertFile
	}
	reloader, err := tlsreload.New("server", paths, logger, func(bundle *tlsreload.Bundle) error {
		srv.tlsState.Store(&serverTLSState{
			certificate:         *bundle.Certificate,
			clientCAs:           bundle.CAPool,
			certificateNotAfter: bundle.CertificateNotAfter,
			caNotAfter:          bundle.CANotAfter,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("initialize TLS reloader: %w", err)
	}
	srv.tlsReloader = reloader
	if err := reloader.LoadInitial(); err != nil {
		reloader.Stop()
		return nil, fmt.Errorf("load initial TLS material: %w", err)
	}

	// Create authenticator using factory
	authenticator, err := ownedConfig.Auth.CreateAuthenticator()
	if err != nil {
		reloader.Stop()
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
		p := pool.NewWithLimits(listener.QuicAddr, balancer, logger, poolLimitsFromCapacity(listener.Capacity))

		pools[listener.QuicAddr] = p
		logger.Info().
			Str("quic_addr", listener.QuicAddr).
			Str("balancer", balancer.Name()).
			Msg("created connection pool")
	}

	srv.pools = pools
	srv.copyBufferPool = protocol.NewCopyBufferPool(ownedConfig.TCPCopyBufferSize)
	srv.authenticator = authenticator
	srv.registrationTimeout = registrationTimeout
	srv.writeRegistrationAck = protocol.WriteRegisterAckWithAuth
	srv.trafficManager = traffic.NewManager(srv.config, srv.pools, srv.copyBufferPool, srv.logger)
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

func cloneServerConfig(conf *config.Server) config.Server {
	cloned := *conf
	cloned.Listeners = cloneListeners(conf.Listeners)
	if value := conf.TLS.SessionTicketEncryptionKeyRotationOverlap; value != nil {
		copied := *value
		cloned.TLS.SessionTicketEncryptionKeyRotationOverlap = &copied
	}
	return cloned
}

// Start runs the server until cancellation or a component failure.
func (s *Server) Start(ctx context.Context) error {
	defer s.tlsReloader.Stop()
	defer func() {
		for _, connectionPool := range s.pools {
			connectionPool.Stop()
		}
	}()

	return superviseServer(ctx, s.trafficManager, s.config.Listeners, s.startListener, s.tlsReloader, s.config.TLS.AutoReload)
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
	if state := s.tlsState.Load(); state != nil {
		snapshot.TLSCertificateNotAfter = state.certificateNotAfter
		snapshot.TLSCANotAfter = state.caNotAfter
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
		snapshot.Routes = append(snapshot.Routes, route)
		snapshot.Ready = snapshot.Ready && route.Ready
	}
	return snapshot
}

// Ready checks route availability without collecting traffic or transport metrics.
func (s *Server) Ready() bool {
	if s.trafficManager == nil || !s.trafficManager.Running() || len(s.config.Listeners) == 0 {
		return false
	}
	for _, listener := range s.config.Listeners {
		p := s.pools[listener.QuicAddr]
		if p == nil {
			return false
		}
		switch listener.Protocol {
		case "tcp":
			if p.EligibleCount("tcp") == 0 {
				return false
			}
		case "udp":
			if p.EligibleCount("udp") == 0 {
				return false
			}
		case "both":
			if p.EligibleCount("tcp") == 0 || p.EligibleCount("udp") == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func superviseServer(
	ctx context.Context,
	trafficManager trafficLifecycle,
	listenerConfs []config.QuicListener,
	startListener listenerStartFunc,
	tlsReloader *tlsreload.Reloader,
	watchTLS bool,
) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	componentCtx, cancelComponents := context.WithCancelCause(context.WithoutCancel(ctx))
	errorState := newListenerErrorState()
	var startupMu sync.Mutex
	startupPending := true
	cancelDuringStartup := func(cause error) {
		startupMu.Lock()
		defer startupMu.Unlock()
		if startupPending {
			cancelComponents(cause)
		}
	}
	watcherDone := make(chan struct{})
	if watchTLS {
		if tlsReloader == nil {
			cancelComponents(context.Canceled)
			return errors.New("TLS reloader is unavailable")
		}
		if err := tlsReloader.PrepareStart(componentCtx, true); err != nil {
			cancelComponents(err)
			tlsReloader.Stop()
			return fmt.Errorf("start TLS watcher: %w", err)
		}
		go func() {
			defer close(watcherDone)
			if err := tlsReloader.Wait(); err != nil {
				err = fmt.Errorf("TLS watcher: %w", err)
				errorState.report(err)
				cancelDuringStartup(err)
			}
		}()
	}
	stopComponents := func(cause error) {
		cancelComponents(cause)
		if watchTLS {
			tlsReloader.Stop()
			<-watcherDone
		}
	}

	// Traffic Start is a startup transaction. Runtime ownership begins only
	// after it has successfully bound and launched all configured listeners.
	stopStartupCancellation := context.AfterFunc(ctx, func() {
		cancelDuringStartup(context.Cause(ctx))
	})
	trafficStartErr := trafficManager.Start(componentCtx)
	startupMu.Lock()
	startupPending = false
	startupMu.Unlock()
	if trafficStartErr != nil {
		stopStartupCancellation()
		trafficManager.Close()
		trafficManager.Wait()
		firstErr := errorState.beginShutdown()
		shutdownCause := firstErr
		if shutdownCause == nil {
			shutdownCause = context.Cause(ctx)
		}
		if shutdownCause == nil {
			shutdownCause = trafficStartErr
		}
		stopComponents(shutdownCause)
		if firstErr != nil && (errors.Is(trafficStartErr, componentCtx.Err()) || errors.Is(trafficStartErr, context.Cause(componentCtx))) {
			return firstErr
		}
		if cause := context.Cause(ctx); cause != nil &&
			(errors.Is(trafficStartErr, cause) || errors.Is(trafficStartErr, ctx.Err()) || errors.Is(trafficStartErr, componentCtx.Err())) {
			return cause
		}
		return fmt.Errorf("start traffic manager: %w", trafficStartErr)
	}
	stopStartupCancellation()

	var listenerWG sync.WaitGroup
	for _, listenerConf := range listenerConfs {
		listenerWG.Add(1)
		go func(lc config.QuicListener) {
			defer listenerWG.Done()
			err := startListener(componentCtx, lc)
			if err == nil && componentCtx.Err() == nil {
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
	cancelComponents(shutdownCause)
	if watchTLS {
		tlsReloader.Stop()
	}
	listenerWG.Wait()
	if watchTLS {
		<-watcherDone
	}
	trafficManager.Wait()

	if firstErr != nil {
		return firstErr
	}
	return context.Cause(ctx)
}

func configureSessionTicketKeyRotation(
	tlsConf *tls.Config,
	interval time.Duration,
	oldKeyLimit uint8,
) (*stek.RotateManager, error) {
	if interval == 0 {
		return nil, nil
	}

	manager, err := stek.NewRotateManager(interval, oldKeyLimit)
	if err != nil {
		return nil, err
	}
	tlsConf.SetSessionTicketKeys(*manager.Keys.Load())
	return manager, nil
}

// startListener starts a QUIC listener
func (s *Server) startListener(ctx context.Context, listenerConf config.QuicListener) error {
	logger := s.logger.With().Str("quic_addr", listenerConf.QuicAddr).Logger()

	// Create UDP listener
	var udpConn *net.UDPConn
	host, _, splitErr := net.SplitHostPort(listenerConf.QuicAddr)
	isHostname := false
	if splitErr == nil && host != "" {
		_, parseErr := netip.ParseAddr(host)
		isHostname = parseErr != nil
	}
	if isHostname {
		listenConfig := net.ListenConfig{}
		packetConn, err := listenConfig.ListenPacket(ctx, "udp", listenerConf.QuicAddr)
		if err != nil {
			return fmt.Errorf("listen UDP: %w", err)
		}
		var ok bool
		udpConn, ok = packetConn.(*net.UDPConn)
		if !ok {
			_ = packetConn.Close()
			return fmt.Errorf("listen UDP returned %T, want *net.UDPConn", packetConn)
		}
	} else {
		addr, err := net.ResolveUDPAddr("udp", listenerConf.QuicAddr)
		if err != nil {
			return fmt.Errorf("resolve QUIC address: %w", err)
		}
		udpConn, err = net.ListenUDP("udp", addr)
		if err != nil {
			return fmt.Errorf("listen UDP: %w", err)
		}
	}
	defer func() { _ = udpConn.Close() }()

	// Each ClientHello receives one immutable TLS material snapshot.
	tlsConf := &tls.Config{}

	oldKeyLimit := s.config.TLS.RotationOldKeyLimit()
	stekManager, err := configureSessionTicketKeyRotation(
		tlsConf,
		s.config.TLS.SessionTicketEncryptionKeyRotationInterval,
		oldKeyLimit,
	)
	if err != nil {
		return fmt.Errorf("initialize session ticket key rotation: %w", err)
	}
	tlsConf.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		state := s.tlsState.Load()
		if state == nil {
			return nil, errors.New("TLS state is unavailable")
		}
		cfg := tlsConf.Clone()
		cfg.GetConfigForClient = nil
		cfg.Certificates = []tls.Certificate{state.certificate}
		if s.config.Auth.Method == "" || s.config.Auth.Method == "mtls" {
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
			cfg.ClientCAs = state.clientCAs
		} else {
			cfg.ClientAuth = tls.NoClientCert
			cfg.ClientCAs = nil
		}
		if stekManager != nil {
			cfg.SetSessionTicketKeys(*stekManager.Keys.Load())
		}
		return cfg, nil
	}
	if stekManager == nil {
		logger.Info().Msg("using Go automatic session ticket key rotation")
	} else {
		logger.Info().
			Dur("rotation_interval", s.config.TLS.SessionTicketEncryptionKeyRotationInterval).
			Uint8("old_key_limit", oldKeyLimit).
			Int("max_total_keys", int(oldKeyLimit)+1).
			Msg("session ticket key rotation enabled")
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
		poolInst.Transport.Add(conn)
		pending := poolInst.BeginPending()
		if pending == nil {
			logger.Warn().Str("remote", conn.RemoteAddr().String()).Msg("pending registration limit reached")
			_ = conn.CloseWithError(registrationErrorCode, registrationFailureReason)
			poolInst.Transport.Remove(conn)
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
	poolInst.Transport.Add(conn)
	defer poolInst.Transport.Remove(conn)
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
	registrationStarted := poolInst.Registrations.Start()
	registrationResult := "protocol_error"
	finishRegistration := sync.OnceFunc(func() {
		if registrationResult != "success" && registrationCtx.Err() != nil {
			registrationResult = stats.Result(registrationCtx.Err(), registrationResult)
		}
		poolInst.Registrations.Finish(registrationStarted, registrationResult)
	})
	defer finishRegistration()
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
		registrationResult = stats.Result(err, "protocol_error")
		logger.Error().Err(err).Msg("accept control stream failed")
		return
	}
	controlStream.SetPriority(0, true)
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
		registrationResult = stats.Result(err, "protocol_error")
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
		registrationResult = "auth_error"
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
		Metadata: pool.ClientMetadata{
			Version:      regMsg.Version,
			Capabilities: slices.Clone(selectedCapabilities),
		},
	}

	// Reserve the client ID without publishing it to traffic selection. The
	// connection becomes visible only after the success Ack is on the wire.
	if err := pending.Reserve(clientConn); err != nil {
		registrationResult = "error"
		if errors.Is(err, pool.ErrClientGenerationCapacity) {
			registrationResult = "capacity"
		}
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
		registrationResult = stats.Result(err, "ack_error")
		logger.Error().Err(err).Msg("send ack failed")
		return
	}
	if err := poolInst.Commit(pending); err != nil {
		registrationResult = "error"
		logger.Error().Err(err).Msg("commit pool entry failed")
		return
	}
	registered = clientConn
	registrationResult = "success"
	finishRegistration()
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
// receives heartbeats from the client to refresh the loop-local health expiry,
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
		payload []byte
		err     error
	}
	readCh := make(chan readResult, 1)
	readerDone := make(chan struct{})

	// Start a goroutine to read messages
	go func(ctx context.Context, stream *quic.Stream, readCh chan readResult, conn *quic.Conn) {
		defer close(readerDone)
		for {
			msgType, payload, err := protocol.ReadMessage(stream)
			select {
			case readCh <- readResult{msgType: msgType, payload: payload, err: err}:
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
	defer func() {
		if retirement == nil {
			retirement = poolInst.BeginRetire(clientConn)
		}
	}()

	now := time.Now()
	healthExpiry := now.Add(s.config.HealthTimeout)
	heartbeatDeadline := time.After(time.Until(healthExpiry))
	var tcpDrained <-chan int64
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

			switch result.msgType {
			case protocol.MsgTypeHeartbeat:
				if retirement == nil {
					current, ok := poolInst.Get(clientConn.ID)
					if !ok || current != clientConn {
						logger.Debug().Msg("ignored heartbeat from stale client generation")
						return
					}
				}
				logger.Debug().Msg("heartbeat received from client")
				clientConn.LastHeartbeat.Store(time.Now().UnixNano())
				healthExpiry = time.Now().Add(s.config.HealthTimeout)
				heartbeatDeadline = time.After(time.Until(healthExpiry))
			case protocol.MsgTypeDrainRequest:
				if !protocol.HasCapability(clientConn.Metadata.Capabilities, protocol.CapabilityTCPDrainV1) {
					closeReason = "unnegotiated drain request"
					return
				}
				if err := protocol.DecodeDrainRequest(result.payload); err != nil {
					closeReason = "invalid drain request"
					logger.Debug().Err(err).Msg(closeReason)
					return
				}
				if retirement == nil {
					retirement = poolInst.BeginRetire(clientConn)
					if retirement == nil {
						closeReason = "stale drain request"
						return
					}
					tcpDrained = retirement.TCPDrained()
				}
			case protocol.MsgTypeDrainComplete:
				closeReason = "wrong-direction drain complete"
				return
			}

		case fence := <-tcpDrained:
			tcpDrained = nil
			if err := protocol.ValidateDrainFence(fence); err != nil {
				closeReason = "invalid drain fence"
				logger.Debug().Err(err).Msg(closeReason)
				return
			}
			now := time.Now()
			writeDeadline, fresh := drainCompleteWriteDeadline(now, s.config.HeartbeatInterval, healthExpiry)
			if !fresh {
				closeReason = "drain complete deadline expired"
				return
			}
			if err := clientConn.ControlStream.SetWriteDeadline(writeDeadline); err != nil {
				closeReason = "set drain complete deadline failed"
				return
			}
			if err := protocol.WriteDrainComplete(clientConn.ControlStream, fence); err != nil {
				closeReason = "write drain complete failed"
				logger.Debug().Err(err).Msg(closeReason)
				return
			}

		case <-heartbeatDeadline:
			timeSinceLastSeen := time.Since(healthExpiry.Add(-s.config.HealthTimeout))
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

func drainCompleteWriteDeadline(now time.Time, interval time.Duration, healthExpiry time.Time) (time.Time, bool) {
	deadline := now.Add(interval)
	if healthExpiry.Before(deadline) {
		deadline = healthExpiry
	}
	return deadline, deadline.After(now)
}
