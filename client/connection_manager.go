package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

// Default backoff configuration
const (
	InitialBackoff                  = 5 * time.Second
	MaxBackoff                      = 60 * time.Second
	BackoffFactor                   = 2
	defaultConnectionAttemptTimeout = 30 * time.Second
)

// ConnectionManager manages connections to multiple servers.
// It orchestrates ServerConnection instances and handles lifecycle management.
type ConnectionManager struct {
	config        *config.Client
	connections   sync.Map // map[string]*ServerConnection (key: server address)
	sessionCaches *SessionCacheManager
	logger        zerolog.Logger

	// TLS and QUIC configuration
	baseTLSConfig *tls.Config
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

	// Reconnection tracking
	reconnectMu  sync.Mutex
	reconnecting map[string]bool

	// NewConns delivers newly established ServerConnections (initial + reconnected)
	// to the Client layer for stream acceptance and UDP handler setup.
	NewConns chan *ServerConnection
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
}

// NewConnectionManager creates a new ConnectionManager instance.
func NewConnectionManager(cfg *config.Client, logger zerolog.Logger) (*ConnectionManager, error) {
	if cfg == nil {
		return nil, errors.New("client config is nil")
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
		sessionCaches:  NewSessionCacheManager(),
		logger:         logger.With().Str("component", "connection_manager").Logger(),
		ctx:            ctx,
		cancel:         cancel,
		attemptTimeout: defaultConnectionAttemptTimeout,
		reconnecting:   make(map[string]bool),
		NewConns:       make(chan *ServerConnection, max(16, len(cfg.Server.GetServers()))),
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
	if err := cm.config.Validate(); err != nil {
		return fmt.Errorf("invalid client configuration: %w", err)
	}

	// Load the credentials required by the selected authentication mode.
	if err := cm.config.LoadCredentials(); err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}

	// Create base TLS config
	cm.baseTLSConfig = &tls.Config{
		RootCAs: cm.config.TLS.CACertPool,
	}
	if cm.config.Auth.Method != config.ClientAuthMethodToken {
		cm.baseTLSConfig.Certificates = []tls.Certificate{cm.config.TLS.ClientCert}
	}

	// Get QUIC config
	cm.quicConfig = cm.config.Quic.GetConfig()

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
				_ = sc.Close()
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

	sc := NewServerConnection(
		endpoint.Address,
		endpoint.ServerName,
		cm.sessionCaches.GetOrCreate(endpoint.Address),
		cm.logger,
	)
	cm.publishMu.Lock()
	cm.trackGenerationLocked(sc, clientGenerationHandshaking)
	cm.publishMu.Unlock()
	if err := sc.Connect(attemptCtx, cm.baseTLSConfig, cm.quicConfig); err != nil {
		_ = sc.Close()
		return nil, err
	}
	cm.publishMu.Lock()
	cm.moveGenerationLocked(sc, clientGenerationHandshaking, clientGenerationPending)
	cm.publishMu.Unlock()
	if err := sc.RegisterWithAuth(attemptCtx, cm.config.ClientID, cm.config.Auth); err != nil {
		_ = sc.Close()
		return nil, err
	}
	return sc, nil
}

func (cm *ConnectionManager) newAttemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	attemptCtx, cancelAttempt := context.WithTimeout(ctx, cm.attemptTimeout)
	stopManagerCancellation := context.AfterFunc(cm.ctx, cancelAttempt)
	if cm.ctx.Err() != nil {
		cancelAttempt()
	}
	return attemptCtx, func() {
		stopManagerCancellation()
		cancelAttempt()
	}
}

// publishServerConnection is the formal commit point for a registered connection.
func (cm *ConnectionManager) publishServerConnection(ctx context.Context, sc *ServerConnection) bool {
	cm.publishMu.Lock()
	if cm.closed || ctx.Err() != nil || cm.ctx.Err() != nil {
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
	previousI, replaced := cm.connections.Swap(sc.ServerAddr(), sc)
	cm.moveGenerationLocked(sc, clientGenerationPending, clientGenerationRegistered)
	if replaced && previousI != sc {
		cm.detachGenerationLocked(previousI.(*ServerConnection))
	}
	cm.publishMu.Unlock()
	// Keep this Close before delivery: the consumer's old stopAndWait relies on
	// closing its owning QUIC connection to unblock context-free SendDatagram.
	if replaced {
		previous := previousI.(*ServerConnection)
		if previous != sc {
			_ = previous.Close()
		}
	}

	select {
	case cm.NewConns <- sc:
	case <-ctx.Done():
		cm.rollbackPublication(sc)
		return false
	case <-cm.ctx.Done():
		cm.rollbackPublication(sc)
		return false
	}

	// Delivery commits ownership. Control ownership starts even if shutdown
	// closes the publication gate immediately after the send.
	sc.StartHeartbeatLoops(cm.config.HeartbeatInterval)
	return true
}

func (cm *ConnectionManager) rollbackPublication(sc *ServerConnection) {
	cm.publishMu.Lock()
	removed := cm.connections.CompareAndDelete(sc.ServerAddr(), sc)
	if removed {
		cm.detachGenerationLocked(sc)
	}
	cm.publishMu.Unlock()
	if removed {
		sc.MarkUnhealthy()
	}
}

// CalculateBackoff calculates the backoff duration for a given attempt number.
// The backoff follows: 5s, 10s, 20s, 40s, 60s (max)
func CalculateBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return InitialBackoff
	}

	backoff := InitialBackoff
	for range attempt {
		backoff *= BackoffFactor
		if backoff > MaxBackoff {
			return MaxBackoff
		}
	}
	return backoff
}

// startReconnection starts a reconnection goroutine for a server if not already reconnecting.
func (cm *ConnectionManager) startReconnection(ctx context.Context, serverAddr string, expected *ServerConnection) {
	cm.publishMu.Lock()
	defer cm.publishMu.Unlock()
	if cm.closed || ctx.Err() != nil || cm.ctx.Err() != nil {
		return
	}
	current, exists := cm.connections.Load(serverAddr)
	if expected != nil {
		if !exists || current != expected {
			return
		}
	} else if exists {
		return
	}

	cm.reconnectMu.Lock()
	if cm.reconnecting[serverAddr] {
		cm.reconnectMu.Unlock()
		return
	}
	cm.reconnecting[serverAddr] = true
	cm.reconnectMu.Unlock()

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
		cm.reconnectMu.Lock()
		delete(cm.reconnecting, serverAddr)
		cm.reconnectMu.Unlock()
		ownsSlot = false
	}
	defer releaseSlot()

	// Find the server endpoint configuration
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
	if expected != nil {
		detached := cm.connections.CompareAndDelete(serverAddr, expected)
		if !detached {
			cm.publishMu.Unlock()
			return
		}
		cm.detachGenerationLocked(expected)
		cm.publishMu.Unlock()
		_ = expected.Close()
	} else {
		_, exists := cm.connections.Load(serverAddr)
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

		backoff := CalculateBackoff(attempt)
		cm.logger.Info().
			Str("server", serverAddr).
			Int("attempt", attempt+1).
			Dur("backoff", backoff).
			Msg("scheduling reconnection attempt")

		// Wait for backoff duration
		select {
		case <-ctx.Done():
			return
		case <-cm.ctx.Done():
			return
		case <-time.After(backoff):
		}

		sc, err := cm.connectAndRegister(ctx, *endpoint)
		if err != nil {
			cm.logger.Warn().
				Str("server", serverAddr).
				Int("attempt", attempt+1).
				Err(err).
				Msg("reconnection attempt failed")
			attempt++
			continue
		}

		// Release before publication and heartbeat startup so an immediate failure
		// callback can claim the next reconnect intent.
		releaseSlot()
		if !cm.publishServerConnection(ctx, sc) {
			_ = sc.Close()
			return
		}

		cm.logger.Info().
			Str("server", serverAddr).
			Int("attempts", attempt+1).
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
	cm.connections.Range(func(key, value any) bool {
		sc := value.(*ServerConnection)
		published = append(published, sc)
		cm.connections.Delete(key)
		cm.detachGenerationLocked(sc)
		return true
	})
	cm.publishMu.Unlock()

	var closeErrors []error
	for _, sc := range published {
		if err := sc.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close %s: %w", sc.ServerAddr(), err))
		}
	}
	for _, sc := range published {
		sc.waitControl()
	}

	if len(closeErrors) > 0 {
		cm.logger.Warn().Int("errors", len(closeErrors)).Msg("errors during shutdown")
	}

	cm.logger.Info().Msg("connection manager stopped")
	return errors.Join(closeErrors...)
}

func (cm *ConnectionManager) stopPublishing() {
	cm.publishMu.Lock()
	cm.closed = true
	cm.cancel()
	cm.publishMu.Unlock()
	cm.wg.Wait()
	cm.newConnsCloseOnce.Do(func() { close(cm.NewConns) })
}

func (cm *ConnectionManager) isCurrent(sc *ServerConnection) bool {
	current, ok := cm.connections.Load(sc.ServerAddr())
	return ok && current == sc
}

func (cm *ConnectionManager) retireConnection(sc *ServerConnection) {
	cm.publishMu.Lock()
	if cm.connections.CompareAndDelete(sc.ServerAddr(), sc) {
		cm.detachGenerationLocked(sc)
	}
	cm.publishMu.Unlock()
}

func (cm *ConnectionManager) trackGenerationLocked(sc *ServerConnection, phase clientGenerationPhase) {
	if sc.capacityPhase != clientGenerationNone {
		cm.generationFaultLocked(sc)
		return
	}
	for i := range cm.endpoints {
		if cm.endpoints[i].endpoint == sc.ServerAddr() {
			sc.capacityEndpoint = i
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
		}
	}
	return snapshot
}

// GetAllConnections returns all server connections.
func (cm *ConnectionManager) GetAllConnections() []*ServerConnection {
	var conns []*ServerConnection
	cm.connections.Range(func(key, value any) bool {
		conns = append(conns, value.(*ServerConnection))
		return true
	})
	return conns
}

// GetConnection returns the connection for a specific server address.
func (cm *ConnectionManager) GetConnection(serverAddr string) *ServerConnection {
	if value, ok := cm.connections.Load(serverAddr); ok {
		return value.(*ServerConnection)
	}
	return nil
}

// HealthyCount returns the number of healthy connections.
func (cm *ConnectionManager) HealthyCount() int {
	count := 0
	cm.connections.Range(func(_, value any) bool {
		if sc := value.(*ServerConnection); sc.IsHealthy() {
			count++
		}
		return true
	})
	return count
}

// TotalCount returns the total number of connections.
func (cm *ConnectionManager) TotalCount() int {
	count := 0
	cm.connections.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// SessionCacheManager returns the session cache manager.
// This is useful for testing session cache persistence.
func (cm *ConnectionManager) SessionCacheManager() *SessionCacheManager {
	return cm.sessionCaches
}
