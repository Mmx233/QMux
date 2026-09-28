package mesh

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/internal/outbound"
	"github.com/Mmx233/QMux/internal/tlsreload"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/auth"
	"github.com/Mmx233/QMux/server/tls/stek"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	meshRegistrationTimeout = 10 * time.Second
	meshInitialTimeout      = 30 * time.Second
)

var (
	ErrMeshServerAlreadyStarted = errors.New("mesh server is already started")
	ErrMeshServerStopped        = errors.New("mesh server is stopped")
)

type Server struct {
	config         config.MeshServer
	registry       *Registry
	declarations   *declarationLedger
	paths          *pathBudget
	controlState   *controlState
	tunnelSource   *tcpSource
	ingressSources []*tcpSource
	authenticator  auth.Auth
	logger         zerolog.Logger

	inboundTLS       atomic.Pointer[inboundTLSState]
	outboundTLS      atomic.Pointer[outboundTLSState]
	inboundReloader  *tlsreload.Reloader
	outboundReloader *tlsreload.Reloader

	registrationTimeout   time.Duration
	initialTimeout        time.Duration
	stableGrace           time.Duration
	reconnectDelay        func(int) time.Duration
	beforePeerDial        func(string)
	beforeReject          func(protocol.MeshRegister, error)
	beforeSuccessAck      func(protocol.MeshRegister, *quic.Conn)
	afterSuccessAck       func(protocol.MeshRegister, *quic.Conn)
	beforeInboundDelivery func(*Session)
	sessions              chan *Session

	lifecycleMu  sync.Mutex
	started      bool
	stopping     bool
	cancel       context.CancelCauseFunc
	listener     *quic.Listener
	address      string
	ready        chan struct{}
	done         chan struct{}
	readyOnce    sync.Once
	doneOnce     sync.Once
	sessionsOnce sync.Once

	acceptDone chan struct{}
	handlerWG  sync.WaitGroup
	workerWG   sync.WaitGroup

	endpointWorkers       atomic.Int64
	reconnectAttempts     atomic.Uint64
	postAckCommitFailures atomic.Uint64
}

type ServerSnapshot struct {
	Registry              RegistrySnapshot
	EndpointWorkers       int64
	ReconnectAttempts     uint64
	PostAckCommitFailures uint64
}

func NewServer(conf *config.MeshServer) (*Server, error) {
	if conf == nil {
		return nil, errors.New("mesh server config is nil")
	}
	owned := *conf
	owned.Tunnel.Peering.Peers = slices.Clone(conf.Tunnel.Peering.Peers)
	owned.Ingress.Listeners = slices.Clone(conf.Ingress.Listeners)
	for i := range owned.Ingress.Listeners {
		if limit := owned.Ingress.Listeners[i].MaxInflightRequests; limit != nil {
			value := *limit
			owned.Ingress.Listeners[i].MaxInflightRequests = &value
		}
	}
	owned.ApplyDefaults()
	if err := owned.Validate(); err != nil {
		return nil, fmt.Errorf("invalid mesh server config: %w", err)
	}
	authenticator, err := owned.Tunnel.Listen.Auth.CreateAuthenticator()
	if err != nil {
		return nil, fmt.Errorf("create mesh authenticator: %w", err)
	}
	logger := log.With().Str("com", "mesh-server").Str("server_id", owned.ServerID).Logger()
	server := &Server{
		config:              owned,
		registry:            NewRegistry(owned.Limits.MaxPendingRegistrations, owned.Limits.MaxClientGenerations, owned.Limits.MaxPeers),
		declarations:        newDeclarationLedger(owned.Limits),
		paths:               newPathBudget(owned.Limits),
		authenticator:       authenticator,
		logger:              logger,
		registrationTimeout: meshRegistrationTimeout,
		initialTimeout:      meshInitialTimeout,
		stableGrace:         defaultStableGrace(),
		reconnectDelay:      func(stage int) time.Duration { return outbound.ReconnectDelay(stage, rand.Int64N) },
		sessions:            make(chan *Session, max(1, owned.Limits.MaxClientGenerations+owned.Limits.MaxPeers)),
		ready:               make(chan struct{}),
		done:                make(chan struct{}),
		acceptDone:          make(chan struct{}),
		tunnelSource:        newTCPSource(owned.Tunnel.Capacity, 0),
	}
	for _, listener := range owned.Ingress.Listeners {
		requestLimit := 0
		if listener.MaxInflightRequests != nil {
			requestLimit = *listener.MaxInflightRequests
		}
		server.ingressSources = append(server.ingressSources, newTCPSource(listener.Capacity, requestLimit))
	}
	server.controlState = newControlState(server.declarations, owned.Limits)
	server.inboundReloader, err = newInboundTLSReloader(
		"mesh-server-inbound",
		owned.Tunnel.Listen.TLS,
		owned.Tunnel.Listen.Auth,
		&server.inboundTLS,
		logger,
	)
	if err != nil {
		return nil, fmt.Errorf("load mesh server inbound TLS material: %w", err)
	}
	if server.hasDialPeers() {
		server.outboundReloader, err = newOutboundTLSReloader(
			"mesh-server-peer-outbound",
			owned.Tunnel.Peering.TLS,
			owned.Tunnel.Peering.Auth,
			&server.outboundTLS,
			logger,
		)
		if err != nil {
			server.inboundReloader.Stop()
			return nil, fmt.Errorf("load mesh peer outbound TLS material: %w", err)
		}
	}
	return server, nil
}

func (s *Server) Sessions() <-chan *Session { return s.sessions }
func (s *Server) Ready() <-chan struct{}    { return s.ready }
func (s *Server) Done() <-chan struct{}     { return s.done }

func (s *Server) Address() string {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.address
}

func (s *Server) Snapshot() ServerSnapshot {
	return ServerSnapshot{
		Registry:              s.registry.Snapshot(),
		EndpointWorkers:       s.endpointWorkers.Load(),
		ReconnectAttempts:     s.reconnectAttempts.Load(),
		PostAckCommitFailures: s.postAckCommitFailures.Load(),
	}
}

func (s *Server) Start(ctx context.Context) (runErr error) {
	s.lifecycleMu.Lock()
	if s.stopping {
		s.lifecycleMu.Unlock()
		return ErrMeshServerStopped
	}
	if s.started {
		s.lifecycleMu.Unlock()
		return ErrMeshServerAlreadyStarted
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	s.started = true
	s.cancel = cancel
	s.lifecycleMu.Unlock()

	registryStopped := false
	defer func() {
		s.cancelRun(ErrMeshServerStopped)
		s.closeSources()
		if !registryStopped {
			s.registry.Stop()
		}
		s.controlState.close()
		s.stopTLS()
		s.readyOnce.Do(func() { close(s.ready) })
		s.sessionsOnce.Do(func() { close(s.sessions) })
		s.doneOnce.Do(func() { close(s.done) })
	}()

	fatal := make(chan error, 4)
	if err := s.prepareTLS(runCtx, fatal); err != nil {
		return err
	}

	tlsConfig, ticketManager, err := s.serverTLSConfig()
	if err != nil {
		return err
	}
	if ticketManager != nil {
		ticketManager.Start(runCtx)
		defer ticketManager.Stop()
	}
	listener, err := quic.ListenAddr(s.config.Tunnel.Listen.Address, tlsConfig, s.config.Tunnel.Quic.GetConfig())
	if err != nil {
		return fmt.Errorf("listen for mesh QUIC: %w", err)
	}
	s.lifecycleMu.Lock()
	s.listener = listener
	s.address = listener.Addr().String()
	s.lifecycleMu.Unlock()
	s.readyOnce.Do(func() { close(s.ready) })

	go s.acceptLoop(runCtx, listener, fatal)
	for _, peer := range s.config.Tunnel.Peering.Peers {
		if peer.Address == "" {
			continue
		}
		s.workerWG.Go(func() { s.peerWorker(runCtx, peer) })
	}

	select {
	case <-runCtx.Done():
		runErr = context.Cause(runCtx)
	case runErr = <-fatal:
		cancel(runErr)
	}

	s.cancelRun(ErrMeshServerStopped)
	_ = listener.Close()
	<-s.acceptDone
	s.closeSources()
	s.registry.Stop()
	registryStopped = true
	s.workerWG.Wait()
	s.handlerWG.Wait()
	if errors.Is(runErr, ErrMeshServerStopped) {
		return nil
	}
	return runErr
}

func (s *Server) Stop() error {
	s.lifecycleMu.Lock()
	if s.stopping {
		done := s.done
		s.lifecycleMu.Unlock()
		<-done
		return nil
	}
	s.stopping = true
	started := s.started
	cancel := s.cancel
	listener := s.listener
	s.lifecycleMu.Unlock()
	s.closeSources()
	if !started {
		s.registry.Stop()
		s.controlState.close()
		s.stopTLS()
		s.readyOnce.Do(func() { close(s.ready) })
		s.sessionsOnce.Do(func() { close(s.sessions) })
		s.doneOnce.Do(func() { close(s.done) })
		return nil
	}
	if cancel != nil {
		cancel(ErrMeshServerStopped)
	}
	if listener != nil {
		_ = listener.Close()
	}
	<-s.done
	return nil
}

func (s *Server) closeSources() {
	s.tunnelSource.close()
	for _, source := range s.ingressSources {
		source.close()
	}
}

func (s *Server) prepareTLS(ctx context.Context, fatal chan<- error) error {
	if err := s.inboundReloader.PrepareStart(ctx, s.config.Tunnel.Listen.TLS.AutoReload); err != nil {
		return fmt.Errorf("prepare mesh server inbound TLS: %w", err)
	}
	if s.config.Tunnel.Listen.TLS.AutoReload {
		go reportTLSFailure("mesh server inbound TLS", s.inboundReloader, fatal)
	}
	if s.outboundReloader == nil {
		return nil
	}
	if err := s.outboundReloader.PrepareStart(ctx, s.config.Tunnel.Peering.TLS.AutoReload); err != nil {
		return fmt.Errorf("prepare mesh peer outbound TLS: %w", err)
	}
	if s.config.Tunnel.Peering.TLS.AutoReload {
		go reportTLSFailure("mesh peer outbound TLS", s.outboundReloader, fatal)
	}
	return nil
}

func reportTLSFailure(role string, reloader *tlsreload.Reloader, fatal chan<- error) {
	if err := reloader.Wait(); err != nil {
		select {
		case fatal <- fmt.Errorf("%s: %w", role, err):
		default:
		}
	}
}

func (s *Server) stopTLS() {
	if s.inboundReloader != nil {
		s.inboundReloader.Stop()
		_ = s.inboundReloader.Wait()
	}
	if s.outboundReloader != nil {
		s.outboundReloader.Stop()
		_ = s.outboundReloader.Wait()
	}
}

func (s *Server) serverTLSConfig() (*tls.Config, *stek.RotateManager, error) {
	base := &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		NextProtos: []string{meshALPN},
	}
	var ticketManager *stek.RotateManager
	if interval := s.config.Tunnel.Listen.TLS.SessionTicketEncryptionKeyRotationInterval; interval != 0 {
		var err error
		ticketManager, err = stek.NewRotateManager(interval, s.config.Tunnel.Listen.TLS.RotationOldKeyLimit())
		if err != nil {
			return nil, nil, fmt.Errorf("initialize mesh session ticket rotation: %w", err)
		}
		base.SetSessionTicketKeys(*ticketManager.Keys.Load())
	}
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		state := s.inboundTLS.Load()
		if state == nil {
			return nil, errors.New("mesh inbound TLS state is unavailable")
		}
		cfg := base.Clone()
		cfg.GetConfigForClient = nil
		cfg.Certificates = []tls.Certificate{state.certificate}
		if s.config.Tunnel.Listen.Auth.Method == "" || s.config.Tunnel.Listen.Auth.Method == config.ClientAuthMethodMTLS {
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
			cfg.ClientCAs = state.clientCAs
		}
		if ticketManager != nil {
			cfg.SetSessionTicketKeys(*ticketManager.Keys.Load())
		}
		return cfg, nil
	}
	return base, ticketManager, nil
}

func (s *Server) acceptLoop(ctx context.Context, listener *quic.Listener, fatal chan<- error) {
	defer close(s.acceptDone)
	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() == nil {
				select {
				case fatal <- fmt.Errorf("accept mesh QUIC connection: %w", err):
				default:
				}
			}
			return
		}
		acceptedAt := time.Now()
		ownerCtx, cancel := context.WithCancel(ctx)
		owner := outbound.NewOwner(cancel)
		installConnectionOwner(owner, conn)
		pending, err := s.registry.BeginInbound(registryOwner(owner))
		if err != nil {
			owner.Finish()
			continue
		}
		s.handlerWG.Go(func() { s.handleInbound(ownerCtx, owner, pending, conn, acceptedAt) })
	}
}

func (s *Server) handleInbound(
	ctx context.Context,
	owner *outbound.Owner,
	generation *Generation,
	conn *quic.Conn,
	acceptedAt time.Time,
) {
	ownedGeneration := generation
	var state *stagedState
	var link *peerLink
	defer func() {
		s.registry.BeginRetire(ownedGeneration)
		owner.Finish()
		s.controlState.unsubscribe(link)
		state.close()
		s.registry.Release(ownedGeneration)
	}()
	headerCtx, cancelHeader := context.WithDeadline(ctx, acceptedAt.Add(s.registrationTimeout))
	defer cancelHeader()
	registrationCtx, cancel := context.WithDeadline(ctx, acceptedAt.Add(s.initialTimeout))
	defer cancel()
	if err := waitForMeshHandshake(headerCtx, conn); err != nil {
		return
	}
	stream, err := conn.AcceptStream(headerCtx)
	if err != nil {
		return
	}
	stream.SetPriority(0, true)
	if deadline, ok := headerCtx.Deadline(); ok {
		if err := stream.SetDeadline(deadline); err != nil {
			return
		}
	}
	registration, err := protocol.ReadMeshRegister(stream)
	if err != nil {
		return
	}
	// Authentication intentionally precedes every diagnostic acknowledgment.
	if err := authenticateInbound(s.authenticator, conn, registration); err != nil {
		return
	}
	if headerCtx.Err() != nil {
		return
	}
	cancelHeader()
	if deadline, ok := registrationCtx.Deadline(); ok {
		if err := stream.SetDeadline(deadline); err != nil {
			return
		}
	}
	reject := func(err error) {
		if s.beforeReject != nil {
			s.beforeReject(registration, err)
		}
		deadline := time.Now().Add(time.Second)
		if total, ok := registrationCtx.Deadline(); ok && total.Before(deadline) {
			deadline = total
		}
		if stream.SetWriteDeadline(deadline) != nil || writeMeshAck(stream, false, err.Error(), "", "", "") != nil || stream.Close() != nil {
			return
		}
		select {
		case <-registrationCtx.Done():
		case <-conn.Context().Done():
		case <-time.After(max(0, time.Until(deadline))):
		}
	}
	if err := protocol.ValidateMeshRegistration(registration.Version, registration.Capabilities); err != nil {
		reject(err)
		return
	}
	if registration.TargetServerID != s.config.ServerID {
		reject(fmt.Errorf("mesh registration targets server %q", registration.TargetServerID))
		return
	}

	switch registration.Role {
	case protocol.MeshRoleClient:
		generation, err = s.registry.PrepareClient(generation, registration.InstanceID, registration.GroupID)
	case protocol.MeshRolePeer:
		generation, err = s.prepareInboundPeer(registrationCtx, generation, registration.PeerServerID)
	default:
		err = fmt.Errorf("unsupported mesh role %q", registration.Role)
	}
	if err != nil {
		reject(err)
		return
	}
	if registration.Role == protocol.MeshRoleClient {
		state, err = receiveInitial(registrationCtx, stream, s.declarations, s.paths, s.config.Limits, registration.Role, registration.GroupID)
	} else {
		snapshot := s.controlState.subscribe(func() { _ = conn.CloseWithError(meshApplicationError, "mesh control queue full") })
		link = snapshot.link
		written := make(chan error, 1)
		go func() { written <- sendPeerSnapshot(registrationCtx, stream, snapshot) }()
		state, err = receiveInitial(registrationCtx, stream, s.declarations, s.paths, s.config.Limits, registration.Role, "")
		if err != nil {
			select {
			case <-written:
			default:
				stream.CancelWrite(meshStreamError)
				<-written
				return
			}
			reject(err)
			return
		}
		writeErr := <-written
		if err == nil {
			err = writeErr
		}
		if err == nil {
			var ready any
			ready, err = protocol.ReadMeshControl(stream)
			if err == nil {
				if result, ok := ready.(protocol.MeshReady); !ok || result.State != protocol.MeshStateStaged {
					err = fmt.Errorf("mesh peer Ready is not staged: %v", ready)
				}
			}
		}
	}
	if err != nil {
		reject(err)
		return
	}

	if s.beforeSuccessAck != nil {
		s.beforeSuccessAck(registration, conn)
	}
	if err := writeMeshAck(
		stream,
		true,
		"registered",
		s.config.ServerID,
		registration.Role,
		s.authenticator.SelectedMeshScheme(),
	); err != nil {
		return
	}
	if s.afterSuccessAck != nil {
		s.afterSuccessAck(registration, conn)
	}
	committed, stop := s.registry.CommitReceiver(generation)
	if !committed {
		s.postAckCommitFailures.Add(1)
		return
	}
	if stop {
		s.registry.BeginRetire(generation)
		return
	}
	if generation.role == RoleClient {
		s.registry.PublishForwarding(generation, ForwardingEligibility{SessionReady: true})
	}
	cancel()
	if err := stream.SetDeadline(time.Time{}); err != nil {
		return
	}
	session := newSession(
		ctx,
		generation.role,
		DirectionInbound,
		s.config.ServerID,
		registration.InstanceID,
		registration.GroupID,
		registration.PeerServerID,
		conn,
		stream,
		s.stableGrace,
	)
	if link != nil {
		session.configurePeerControl(link.queue, state, s.declarations, s.paths, s.config.Limits)
	}
	installSessionOwner(owner, session)
	if s.beforeInboundDelivery != nil {
		s.beforeInboundDelivery(session)
	}
	outbound.DeliverThenStart(ctx, conn.Context(), s.sessions, session, func() {
		_ = session.run(s.config.Tunnel.HeartbeatInterval, s.config.Tunnel.HealthTimeout)
	})
}

func (s *Server) prepareInboundPeer(
	ctx context.Context,
	pending *Generation,
	peerID string,
) (*Generation, error) {
	if peerID == s.config.ServerID {
		return nil, errors.New("mesh peer must not use the local server_id")
	}
	peer, ok := s.configuredPeer(peerID)
	if !ok {
		return nil, fmt.Errorf("mesh peer %q is not configured", peerID)
	}
	preferred := true
	if peer.Address != "" {
		preferred = peerID < s.config.ServerID
		if !preferred {
			return nil, fmt.Errorf("mesh peer %q used the non-preferred dial direction", peerID)
		}
	}
	prepared, arbitration, err := s.registry.PreparePeer(pending, peerID, preferred)
	if err != nil || arbitration == nil {
		return prepared, err
	}
	arbitration.Loser().StopOwner()
	arbitration.Loser().WaitOwner()
	if ctx.Err() != nil {
		s.registry.CancelArbitration(arbitration, true)
		return nil, context.Cause(ctx)
	}
	prepared, err = s.registry.CompleteArbitration(arbitration)
	if err != nil {
		s.registry.CancelArbitration(arbitration, false)
		return nil, err
	}
	return prepared, nil
}

func (s *Server) peerWorker(ctx context.Context, peer config.MeshPeer) {
	s.endpointWorkers.Add(1)
	defer s.endpointWorkers.Add(-1)
	var retryState outbound.Retry
	retry := false
	for {
		if !s.registry.WaitPeerDialable(ctx, peer.ServerID) {
			return
		}
		if retry {
			delay := s.reconnectDelay(retryState.RetryStage())
			if !outbound.WaitReconnect(ctx, ctx, delay) {
				return
			}
			if !s.registry.WaitPeerDialable(ctx, peer.ServerID) {
				return
			}
		}
		attemptCtx, cancel := outbound.AttemptContext(ctx, ctx, outbound.AttemptTimeout)
		owner := outbound.NewOwner(cancel)
		attemptDone := make(chan struct{})
		generation, err := s.registry.BeginOutboundPeer(peer.ServerID, Owner{
			Stop: owner.Stop,
			Wait: func() { <-attemptDone },
		})
		if err != nil {
			owner.Finish()
			retry = true
			continue
		}
		if s.beforePeerDial != nil {
			s.beforePeerDial(peer.ServerID)
		}
		if retry {
			retryState.AdvanceRetry(retryState.RetryStage(), outbound.MaxReconnectStage)
			s.reconnectAttempts.Add(1)
		}
		stable, err := s.runOutboundPeerAttempt(ctx, attemptCtx, owner, generation, peer, attemptDone)
		if err != nil && ctx.Err() == nil {
			s.logger.Debug().Str("peer_id", peer.ServerID).Err(err).Msg("mesh peer connection ended")
		}
		if stable {
			retryState.ResetRetry()
		}
		retry = true
	}
}

func (s *Server) runOutboundPeerAttempt(
	workerCtx context.Context,
	attemptCtx context.Context,
	owner *outbound.Owner,
	generation *Generation,
	peer config.MeshPeer,
	attemptDone chan struct{},
) (stable bool, resultErr error) {
	var session *Session
	var initialState *stagedState
	var link *peerLink
	defer func() {
		stable = s.registry.IsCurrent(generation) && session != nil && session.ReconnectStable()
		s.registry.BeginRetire(generation)
		owner.Finish()
		s.controlState.unsubscribe(link)
		initialState.close()
		s.registry.Release(generation)
		close(attemptDone)
	}()
	state := s.outboundTLS.Load()
	if state == nil {
		return false, errors.New("mesh peer outbound TLS state is unavailable")
	}
	conn, err := outbound.Dial(
		attemptCtx,
		peer.Address,
		peer.ServerName,
		state.baseTLSConfig,
		state.sessionCaches.GetOrCreate(peer.Address),
		s.config.Tunnel.Quic.GetConfig(),
	)
	if err != nil {
		return false, err
	}
	installConnectionOwner(owner, conn)
	registration := protocol.MeshRegister{
		Version:        protocol.MeshProtocolVersion,
		Capabilities:   protocol.MeshCapabilities(),
		Role:           protocol.MeshRolePeer,
		TargetServerID: peer.ServerID,
		PeerServerID:   s.config.ServerID,
	}
	snapshot := s.controlState.subscribe(func() { _ = conn.CloseWithError(meshApplicationError, "mesh control queue full") })
	link = snapshot.link
	defer snapshot.release()
	stream, initialState, err := outboundRegistration(attemptCtx, conn, registration, s.config.Tunnel.Peering.Auth, nil, s.declarations, s.paths, s.config.Limits, snapshot)
	if err != nil {
		return false, err
	}
	// The attempt deadline ends at exact ACK validation; the published session
	// remains owned by the endpoint worker lifetime.
	owner.Cancel()
	if !s.registry.CommitOutboundPeer(generation) {
		return false, errors.New("mesh outbound peer lost exact-current publication")
	}
	session = newSession(
		workerCtx,
		RolePeer,
		DirectionOutbound,
		peer.ServerID,
		"",
		"",
		peer.ServerID,
		conn,
		stream,
		s.stableGrace,
	)
	session.configurePeerControl(link.queue, initialState, s.declarations, s.paths, s.config.Limits)
	installSessionOwner(owner, session)
	if !outbound.DeliverThenStart(workerCtx, conn.Context(), s.sessions, session, func() {
		resultErr = session.run(s.config.Tunnel.HeartbeatInterval, s.config.Tunnel.HealthTimeout)
	}) {
		return false, context.Cause(workerCtx)
	}
	return false, resultErr
}

func (s *Server) configuredPeer(peerID string) (config.MeshPeer, bool) {
	for _, peer := range s.config.Tunnel.Peering.Peers {
		if peer.ServerID == peerID {
			return peer, true
		}
	}
	return config.MeshPeer{}, false
}

func (s *Server) hasDialPeers() bool {
	for _, peer := range s.config.Tunnel.Peering.Peers {
		if peer.Address != "" {
			return true
		}
	}
	return false
}

func (s *Server) cancelRun(cause error) {
	s.lifecycleMu.Lock()
	cancel := s.cancel
	s.lifecycleMu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}
