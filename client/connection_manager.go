package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/internal/outbound"
	"github.com/Mmx233/QMux/internal/stats"
	"github.com/Mmx233/QMux/internal/tlsreload"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const (
	initialReconnectDelay           = outbound.InitialReconnectDelay
	maxReconnectDelay               = outbound.MaxReconnectDelay
	reconnectStableGrace            = outbound.ReconnectStableGrace
	maxReconnectStage               = outbound.MaxReconnectStage
	defaultConnectionAttemptTimeout = outbound.AttemptTimeout
)

// ConnectionManager manages connections to multiple servers.
// It orchestrates ServerConnection instances and handles lifecycle management.
type ConnectionManager struct {
	config *config.Client
	auth   config.ClientAuth
	logger zerolog.Logger

	// TLS and QUIC configuration
	tlsState      atomic.Pointer[clientTLSState]
	tlsReloader   *tlsreload.Reloader
	tlsAutoReload bool
	tlsConfig     config.ClientTLS
	quicConfig    *quic.Config

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Publication is the commit point for a registered connection. Stop closes
	// this gate before canceling attempts so a late acknowledgment cannot commit.
	publishMu         sync.Mutex
	closed            bool
	endpoints         []clientEndpointPhases
	newConnsCloseOnce sync.Once

	// Internal test seam; the production default remains fixed and is not config.
	attemptTimeout time.Duration

	// NewConns delivers newly established ServerConnections (initial + reconnected)
	// to the Client layer for stream acceptance and UDP handler setup.
	NewConns chan *ServerConnection
}

type clientTLSState struct {
	baseTLSConfig       *tls.Config
	sessionCaches       *SessionCacheManager
	certificateNotAfter time.Time
	caNotAfter          time.Time
}

type clientGenerationPhase uint8

const (
	clientGenerationNone clientGenerationPhase = iota
	clientGenerationHandshaking
	clientGenerationPending
	clientGenerationRegistered
	clientGenerationRetiring
	clientGenerationDone
)

type clientEndpointPhases struct {
	endpoint            string
	handshaking         int64
	pending             int64
	registered          int64
	retiring            int64
	generationHighWater int64
	accountingFaults    uint64
	transport           stats.Transport
	connect             stats.Operation
	registration        stats.Operation
	lifecycle           outbound.Endpoint[*ServerConnection]
}

// NewConnectionManager creates a new ConnectionManager instance.
func NewConnectionManager(cfg *config.Client, logger zerolog.Logger) (*ConnectionManager, error) {
	if cfg == nil {
		return nil, errors.New("client config is nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid client configuration: %w", err)
	}

	// Validate and deduplicate servers
	hasDuplicates, err := cfg.Server.ValidateAndDeduplicate()
	if err != nil {
		return nil, fmt.Errorf("invalid server configuration: %w", err)
	}
	if hasDuplicates {
		logger.Warn().Msg("duplicate server addresses detected and removed")
	}

	ctx, cancel := context.WithCancel(context.Background())

	cm := &ConnectionManager{
		config:         cfg,
		auth:           cfg.Auth,
		logger:         logger.With().Str("component", "connection_manager").Logger(),
		tlsAutoReload:  cfg.TLS.AutoReload,
		tlsConfig:      cfg.TLS,
		quicConfig:     cfg.Quic.GetConfig(),
		ctx:            ctx,
		cancel:         cancel,
		attemptTimeout: defaultConnectionAttemptTimeout,
		NewConns:       make(chan *ServerConnection, max(16, len(cfg.Server.GetServers()))),
	}
	paths := tlsreload.Paths{CAFile: cfg.TLS.CACertFile}
	if cfg.Auth.Method != config.ClientAuthMethodToken {
		paths.CertFile = cfg.TLS.ClientCertFile
		paths.KeyFile = cfg.TLS.ClientKeyFile
	}
	reloader, err := tlsreload.New("client", paths, cm.logger, func(bundle *tlsreload.Bundle) error {
		baseTLSConfig := &tls.Config{RootCAs: bundle.CAPool}
		if bundle.Certificate != nil {
			baseTLSConfig.Certificates = []tls.Certificate{*bundle.Certificate}
		}
		cm.tlsState.Store(&clientTLSState{
			baseTLSConfig:       baseTLSConfig,
			sessionCaches:       NewSessionCacheManager(),
			certificateNotAfter: bundle.CertificateNotAfter,
			caNotAfter:          bundle.CANotAfter,
		})
		return nil
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("initialize TLS reloader: %w", err)
	}
	cm.tlsReloader = reloader
	if err := reloader.LoadInitial(); err != nil {
		reloader.Stop()
		cancel()
		return nil, fmt.Errorf("load initial TLS material: %w", err)
	}
	for _, endpoint := range cfg.Server.GetServers() {
		cm.endpoints = append(cm.endpoints, clientEndpointPhases{endpoint: endpoint.Address})
	}

	return cm, nil
}

// Start initiates connections to all configured servers concurrently.
// It uses goroutines for each server and waits for all connection attempts.
// Partial failures are handled - the manager continues with successful connections.
func (cm *ConnectionManager) Start(ctx context.Context) error {
	validationConfig := *cm.config
	validationConfig.Auth = cm.auth
	validationConfig.TLS = cm.tlsConfig
	if err := validationConfig.Validate(); err != nil {
		return fmt.Errorf("invalid client configuration: %w", err)
	}
	cm.quicConfig = cm.config.Quic.GetConfig()
	if err := cm.tlsReloader.PrepareStart(ctx, cm.tlsAutoReload); err != nil {
		return fmt.Errorf("prepare TLS material: %w", err)
	}

	servers := cm.config.Server.GetServers()
	cm.logger.Info().Int("server_count", len(servers)).Msg("starting connections to servers")

	// Create connections concurrently
	var initialWG sync.WaitGroup
	var mu sync.Mutex
	connectedServers := 0
	var connectionErrors []error

	for _, server := range servers {
		cm.publishMu.Lock()
		if cm.closed {
			cm.publishMu.Unlock()
			continue
		}
		initialWG.Add(1)
		cm.wg.Go(func() {
			defer initialWG.Done()
			endpoint := server
			sc, err := cm.connectAndRegister(ctx, endpoint)
			if err != nil {
				cm.logger.Error().
					Str("server", endpoint.Address).
					Err(err).
					Msg("failed to connect and register with server")

				mu.Lock()
				connectionErrors = append(connectionErrors, fmt.Errorf("server %s: %w", endpoint.Address, err))
				mu.Unlock()

				cm.startReconnection(ctx, endpoint.Address, nil)
				return
			}

			if !cm.publishServerConnection(ctx, sc) {
				sc.owner.Stop()
				return
			}

			mu.Lock()
			connectedServers++
			mu.Unlock()

			cm.logger.Info().
				Str("server", endpoint.Address).
				Msg("successfully connected and registered")
		})
		cm.publishMu.Unlock()
	}

	// Wait for all connection attempts to complete
	initialWG.Wait()

	// Log summary
	cm.logger.Info().
		Int("connected", connectedServers).
		Int("failed", len(connectionErrors)).
		Int("total", len(servers)).
		Msg("connection startup complete")

	return nil
}

func (cm *ConnectionManager) connectAndRegister(ctx context.Context, endpoint config.ServerEndpoint) (*ServerConnection, error) {
	attemptCtx, cancel := cm.newAttemptContext(ctx)
	defer cancel()
	state := cm.tlsState.Load()
	if state == nil {
		return nil, errors.New("TLS state is unavailable")
	}

	sc := NewServerConnection(
		endpoint.Address,
		endpoint.ServerName,
		state.sessionCaches.GetOrCreate(endpoint.Address),
		cm.logger,
	)
	cm.publishMu.Lock()
	cm.trackGenerationLocked(sc, clientGenerationHandshaking)
	cm.publishMu.Unlock()
	observed := &cm.endpoints[sc.capacityEndpoint]
	started := observed.connect.Start()
	err := sc.Connect(attemptCtx, state.baseTLSConfig, cm.quicConfig)
	observed.connect.Finish(started, stats.Result(err, "dial_error"))
	if err != nil {
		sc.owner.Stop()
		return nil, err
	}
	cm.publishMu.Lock()
	cm.moveGenerationLocked(sc, clientGenerationHandshaking, clientGenerationPending)
	cm.publishMu.Unlock()
	started = observed.registration.Start()
	err = sc.RegisterWithAuth(attemptCtx, cm.config.ClientID, cm.auth)
	observed.registration.Finish(started, stats.Result(err, "protocol_error"))
	if err != nil {
		sc.owner.Stop()
		return nil, err
	}
	return sc, nil
}

func (cm *ConnectionManager) newAttemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return outbound.AttemptContext(ctx, cm.ctx, cm.attemptTimeout)
}

// publishServerConnection is the formal commit point for a registered connection.
func (cm *ConnectionManager) publishServerConnection(ctx context.Context, sc *ServerConnection) bool {
	cm.publishMu.Lock()
	if cm.closed || ctx.Err() != nil || cm.ctx.Err() != nil {
		cm.publishMu.Unlock()
		return false
	}
	endpoint := cm.endpointLocked(sc.ServerAddr())
	if endpoint == nil {
		cm.publishMu.Unlock()
		return false
	}
	if sc.capacityPhase == clientGenerationNone {
		cm.trackGenerationLocked(sc, clientGenerationPending)
	}
	if sc.capacityPhase != clientGenerationPending {
		cm.generationFaultLocked(sc)
		cm.publishMu.Unlock()
		return false
	}

	sc.SetHealthConfig(cm.config.HealthTimeout)
	sc.SetReconnectCallback(func(serverAddr string) {
		cm.startReconnection(ctx, serverAddr, sc)
	})
	sc.MarkHealthy()
	previous, replaced := endpoint.lifecycle.Replace(sc)
	cm.moveGenerationLocked(sc, clientGenerationPending, clientGenerationRegistered)
	if replaced && previous != sc {
		cm.detachGenerationLocked(previous)
	}
	cm.publishMu.Unlock()
	// Keep this Close before delivery: the consumer's old stopAndWait relies on
	// closing its owning QUIC connection to unblock context-free SendDatagram.
	if replaced && previous != sc {
		previous.owner.Stop()
	}

	if !outbound.DeliverThenStart(ctx, cm.ctx, cm.NewConns, sc, func() {
		sc.StartHeartbeatLoops(cm.config.HeartbeatInterval)
	}) {
		cm.rollbackPublication(sc)
		return false
	}
	return true
}

func (cm *ConnectionManager) rollbackPublication(sc *ServerConnection) {
	cm.publishMu.Lock()
	endpoint := cm.endpointLocked(sc.ServerAddr())
	removed := endpoint != nil && endpoint.lifecycle.Retire(sc)
	if removed {
		cm.detachGenerationLocked(sc)
	}
	cm.publishMu.Unlock()
	if removed {
		sc.MarkUnhealthy()
	}
}

func reconnectDelay(attempt int, int64n func(int64) int64) time.Duration {
	return outbound.ReconnectDelay(attempt, int64n)
}

func waitForReconnect(ctx, managerCtx context.Context, delay time.Duration) bool {
	return outbound.WaitReconnect(ctx, managerCtx, delay)
}

// startReconnection starts a reconnection goroutine for a server if not already reconnecting.
func (cm *ConnectionManager) startReconnection(ctx context.Context, serverAddr string, expected *ServerConnection) {
	cm.publishMu.Lock()
	defer cm.publishMu.Unlock()
	if cm.closed || ctx.Err() != nil || cm.ctx.Err() != nil {
		return
	}
	endpoint := cm.endpointLocked(serverAddr)
	if endpoint == nil || !endpoint.lifecycle.ClaimReconnect(expected) {
		return
	}

	cm.wg.Go(func() {
		cm.reconnectionLoop(ctx, serverAddr, expected)
	})
}

// reconnectionLoop attempts to reconnect to a server with exponential backoff.
func (cm *ConnectionManager) reconnectionLoop(ctx context.Context, serverAddr string, expected *ServerConnection) {
	ownsSlot := true
	releaseSlot := func() {
		if !ownsSlot {
			return
		}
		cm.publishMu.Lock()
		if endpoint := cm.endpointLocked(serverAddr); endpoint != nil {
			endpoint.lifecycle.ReleaseReconnect()
		}
		cm.publishMu.Unlock()
		ownsSlot = false
	}
	defer releaseSlot()

	// Find the immutable endpoint configuration and its persistent retry state.
	var endpoint *config.ServerEndpoint
	for _, s := range cm.config.Server.GetServers() {
		if s.Address == serverAddr {
			endpoint = &s
			break
		}
	}
	if endpoint == nil {
		cm.logger.Error().Str("server", serverAddr).Msg("server not found in configuration")
		return
	}
	cm.publishMu.Lock()
	endpointState := cm.endpointLocked(endpoint.Address)
	cm.publishMu.Unlock()
	if endpointState == nil {
		cm.logger.Error().Str("server", serverAddr).Msg("server retry state not found")
		return
	}

	cm.publishMu.Lock()
	if expected != nil {
		detached := endpointState.lifecycle.RetireForReconnect(expected, expected.reconnectStable.Load())
		if !detached {
			cm.publishMu.Unlock()
			return
		}
		cm.detachGenerationLocked(expected)
		cm.publishMu.Unlock()
		expected.owner.Stop()
	} else {
		exists := !endpointState.lifecycle.Empty()
		cm.publishMu.Unlock()
		if exists {
			return
		}
	}

	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-cm.ctx.Done():
			return
		default:
		}

		cm.publishMu.Lock()
		stage := endpointState.lifecycle.RetryStage()
		cm.publishMu.Unlock()
		backoff := reconnectDelay(stage, rand.Int64N)
		cm.logger.Info().
			Str("server", serverAddr).
			Int("attempt", attempt+1).
			Int("backoff_stage", stage).
			Dur("backoff", backoff).
			Msg("scheduling reconnection attempt")

		if !waitForReconnect(ctx, cm.ctx, backoff) {
			return
		}

		cm.publishMu.Lock()
		endpointState.lifecycle.AdvanceRetry(stage, maxReconnectStage)
		cm.publishMu.Unlock()
		sc, err := cm.connectAndRegister(ctx, *endpoint)
		if err != nil {
			cm.logger.Warn().
				Str("server", serverAddr).
				Int("attempt", attempt+1).
				Int("backoff_stage", stage).
				Err(err).
				Msg("reconnection attempt failed")
			attempt++
			continue
		}

		// Release before publication and heartbeat startup so an immediate failure
		// callback can claim the next reconnect intent.
		releaseSlot()
		if !cm.publishServerConnection(ctx, sc) {
			sc.owner.Stop()
			return
		}

		cm.logger.Info().
			Str("server", serverAddr).
			Int("attempts", attempt+1).
			Int("backoff_stage", stage).
			Msg("reconnection successful")

		return
	}
}

// Stop abruptly shuts down all connections.
func (cm *ConnectionManager) Stop() error {
	cm.logger.Info().Msg("stopping connection manager")
	cm.stopPublishing()
	cm.logger.Debug().Msg("all goroutines stopped")

	cm.publishMu.Lock()
	var published []*ServerConnection
	for i := range cm.endpoints {
		if sc := cm.endpoints[i].lifecycle.Take(); sc != nil {
			published = append(published, sc)
			cm.detachGenerationLocked(sc)
		}
	}
	cm.publishMu.Unlock()

	var closeErrors []error
	for _, sc := range published {
		sc.owner.Stop()
		if sc.closeErr != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close %s: %w", sc.ServerAddr(), sc.closeErr))
		}
	}
	for _, sc := range published {
		sc.owner.Join()
	}

	if len(closeErrors) > 0 {
		cm.logger.Warn().Int("errors", len(closeErrors)).Msg("errors during shutdown")
	}

	cm.logger.Info().Msg("connection manager stopped")
	return errors.Join(closeErrors...)
}

func (cm *ConnectionManager) stopPublishing() {
	cm.tlsReloader.Stop()
	cm.publishMu.Lock()
	cm.closed = true
	cm.cancel()
	cm.publishMu.Unlock()
	cm.wg.Wait()
	cm.newConnsCloseOnce.Do(func() { close(cm.NewConns) })
}

func (cm *ConnectionManager) isCurrent(sc *ServerConnection) bool {
	cm.publishMu.Lock()
	defer cm.publishMu.Unlock()
	endpoint := cm.endpointLocked(sc.ServerAddr())
	return endpoint != nil && endpoint.lifecycle.Is(sc)
}

func (cm *ConnectionManager) retireConnection(sc *ServerConnection) {
	cm.publishMu.Lock()
	endpoint := cm.endpointLocked(sc.ServerAddr())
	if endpoint != nil && endpoint.lifecycle.Retire(sc) {
		cm.detachGenerationLocked(sc)
	}
	cm.publishMu.Unlock()
}

func (cm *ConnectionManager) endpointLocked(serverAddr string) *clientEndpointPhases {
	for i := range cm.endpoints {
		if cm.endpoints[i].endpoint == serverAddr {
			return &cm.endpoints[i]
		}
	}
	return nil
}

func (cm *ConnectionManager) trackGenerationLocked(sc *ServerConnection, phase clientGenerationPhase) {
	if sc.capacityPhase != clientGenerationNone {
		cm.generationFaultLocked(sc)
		return
	}
	for i := range cm.endpoints {
		if cm.endpoints[i].endpoint == sc.ServerAddr() {
			sc.capacityEndpoint = i
			sc.transportStats.Store(&cm.endpoints[i].transport)
			sc.capacityPhase = phase
			cm.addGenerationLocked(sc, phase, 1)
			if !sc.setOnClosed(func() { cm.generationClosed(sc) }) {
				cm.generationClosedLocked(sc)
			}
			return
		}
	}
}

func (cm *ConnectionManager) moveGenerationLocked(sc *ServerConnection, from, to clientGenerationPhase) {
	if sc.capacityPhase != from {
		cm.generationFaultLocked(sc)
		return
	}
	cm.addGenerationLocked(sc, from, -1)
	sc.capacityPhase = to
	cm.addGenerationLocked(sc, to, 1)
}

func (cm *ConnectionManager) detachGenerationLocked(sc *ServerConnection) {
	if sc.capacityPhase == clientGenerationNone {
		cm.trackGenerationLocked(sc, clientGenerationRegistered)
	}
	if sc.capacityPhase == clientGenerationRetiring || sc.capacityPhase == clientGenerationDone {
		return
	}
	cm.moveGenerationLocked(sc, clientGenerationRegistered, clientGenerationRetiring)
}

func (cm *ConnectionManager) generationClosed(sc *ServerConnection) {
	cm.publishMu.Lock()
	defer cm.publishMu.Unlock()
	cm.generationClosedLocked(sc)
}

func (cm *ConnectionManager) generationClosedLocked(sc *ServerConnection) {
	switch sc.capacityPhase {
	case clientGenerationHandshaking, clientGenerationPending, clientGenerationRetiring:
		cm.addGenerationLocked(sc, sc.capacityPhase, -1)
		sc.capacityPhase = clientGenerationDone
	case clientGenerationRegistered:
		cm.generationFaultLocked(sc)
		cm.addGenerationLocked(sc, clientGenerationRegistered, -1)
		sc.capacityPhase = clientGenerationDone
	case clientGenerationNone, clientGenerationDone:
	}
}

func (cm *ConnectionManager) addGenerationLocked(sc *ServerConnection, phase clientGenerationPhase, delta int64) {
	if sc.capacityEndpoint < 0 || sc.capacityEndpoint >= len(cm.endpoints) {
		return
	}
	endpoint := &cm.endpoints[sc.capacityEndpoint]
	var counter *int64
	switch phase {
	case clientGenerationHandshaking:
		counter = &endpoint.handshaking
	case clientGenerationPending:
		counter = &endpoint.pending
	case clientGenerationRegistered:
		counter = &endpoint.registered
	case clientGenerationRetiring:
		counter = &endpoint.retiring
	default:
		return
	}
	if *counter+delta < 0 {
		endpoint.accountingFaults++
		return
	}
	*counter += delta
	if total := endpoint.handshaking + endpoint.pending + endpoint.registered + endpoint.retiring; total > endpoint.generationHighWater {
		endpoint.generationHighWater = total
	}
}

func (cm *ConnectionManager) generationFaultLocked(sc *ServerConnection) {
	if sc.capacityEndpoint >= 0 && sc.capacityEndpoint < len(cm.endpoints) {
		cm.endpoints[sc.capacityEndpoint].accountingFaults++
	}
}

func (cm *ConnectionManager) endpointSnapshot() []EndpointSnapshot {
	cm.publishMu.Lock()
	defer cm.publishMu.Unlock()
	snapshot := make([]EndpointSnapshot, len(cm.endpoints))
	for i := range cm.endpoints {
		endpoint := &cm.endpoints[i]
		snapshot[i] = EndpointSnapshot{
			Endpoint:            endpoint.endpoint,
			Handshaking:         endpoint.handshaking,
			Pending:             endpoint.pending,
			Registered:          endpoint.registered,
			Retiring:            endpoint.retiring,
			GenerationHighWater: endpoint.generationHighWater,
			AccountingFaults:    endpoint.accountingFaults,
			ReconnectAttempts:   endpoint.lifecycle.ReconnectAttempts(),
			Reconnecting:        endpoint.lifecycle.Reconnecting(),
			QUIC:                endpoint.transport.Snapshot(),
			Connect:             endpoint.connect.Snapshot(),
			Registration:        endpoint.registration.Snapshot(),
		}
		if connection := endpoint.lifecycle.Load(); connection != nil {
			snapshot[i].Healthy = connection.IsHealthy()
			snapshot[i].LastHeartbeat = connection.LastReceivedFromServer()
		}
	}
	return snapshot
}

// GetAllConnections returns all server connections.
func (cm *ConnectionManager) GetAllConnections() []*ServerConnection {
	cm.publishMu.Lock()
	defer cm.publishMu.Unlock()
	conns := make([]*ServerConnection, 0, len(cm.endpoints))
	for i := range cm.endpoints {
		if connection := cm.endpoints[i].lifecycle.Load(); connection != nil {
			conns = append(conns, connection)
		}
	}
	return conns
}

// GetConnection returns the connection for a specific server address.
func (cm *ConnectionManager) GetConnection(serverAddr string) *ServerConnection {
	cm.publishMu.Lock()
	defer cm.publishMu.Unlock()
	if endpoint := cm.endpointLocked(serverAddr); endpoint != nil {
		return endpoint.lifecycle.Load()
	}
	return nil
}

// HealthyCount returns the number of healthy connections.
func (cm *ConnectionManager) HealthyCount() int {
	cm.publishMu.Lock()
	defer cm.publishMu.Unlock()
	count := 0
	for i := range cm.endpoints {
		if sc := cm.endpoints[i].lifecycle.Load(); sc != nil && sc.IsHealthy() {
			count++
		}
	}
	return count
}

// TotalCount returns the total number of connections.
func (cm *ConnectionManager) TotalCount() int {
	cm.publishMu.Lock()
	defer cm.publishMu.Unlock()
	count := 0
	for i := range cm.endpoints {
		if !cm.endpoints[i].lifecycle.Empty() {
			count++
		}
	}
	return count
}

// SessionCacheManager returns the session cache manager.
// This is useful for testing session cache persistence.
func (cm *ConnectionManager) SessionCacheManager() *SessionCacheManager {
	state := cm.tlsState.Load()
	if state == nil {
		return nil
	}
	return state.sessionCaches
}
