package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

// Default backoff configuration
const (
	InitialBackoff = 5 * time.Second
	MaxBackoff     = 60 * time.Second
	BackoffFactor  = 2
)

// ConnectionManager manages connections to multiple servers.
// It orchestrates ServerConnection instances, handles lifecycle management,
// and provides load balancing across healthy connections.
type ConnectionManager struct {
	config        *config.Client
	connections   sync.Map // map[string]*ServerConnection (key: server address)
	sessionCaches *SessionCacheManager
	balancer      *LoadBalancer
	logger        zerolog.Logger

	// TLS and QUIC configuration
	baseTLSConfig *tls.Config
	quicConfig    *quic.Config

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Reconnection tracking
	reconnectMu  sync.Mutex
	reconnecting map[string]bool
}

// NewConnectionManager creates a new ConnectionManager instance.
func NewConnectionManager(cfg *config.Client, logger zerolog.Logger) (*ConnectionManager, error) {
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
		config:        cfg,
		sessionCaches: NewSessionCacheManager(),
		balancer:      NewLoadBalancer(),
		logger:        logger.With().Str("component", "connection_manager").Logger(),
		ctx:           ctx,
		cancel:        cancel,
		reconnecting:  make(map[string]bool),
	}

	return cm, nil
}

// Start initiates connections to all configured servers concurrently.
// It uses goroutines for each server and waits for all connection attempts.
// Partial failures are handled - the manager continues with successful connections.
func (cm *ConnectionManager) Start(ctx context.Context) error {
	// Load TLS certificates
	if err := cm.config.TLS.LoadCertificates(); err != nil {
		return fmt.Errorf("load certificates: %w", err)
	}

	// Create base TLS config
	cm.baseTLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cm.config.TLS.ClientCert},
		RootCAs:      cm.config.TLS.CACertPool,
	}

	// Get QUIC config
	cm.quicConfig = cm.config.Quic.GetConfig()

	servers := cm.config.Server.GetServers()
	cm.logger.Info().Int("server_count", len(servers)).Msg("starting connections to servers")

	// Create connections concurrently
	var wg sync.WaitGroup
	var mu sync.Mutex
	var connectedServers []*ServerConnection
	var connectionErrors []error

	for _, server := range servers {
		wg.Add(1)
		go func(endpoint config.ServerEndpoint) {
			defer wg.Done()

			// Get or create session cache for this server
			sessionCache := cm.sessionCaches.GetOrCreate(endpoint.Address)

			// Create server connection
			sc := NewServerConnection(
				endpoint.Address,
				endpoint.ServerName,
				sessionCache,
				cm.logger,
			)

			// Store connection in map
			cm.connections.Store(endpoint.Address, sc)

			// Attempt to connect
			connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			if err := sc.Connect(connectCtx, cm.baseTLSConfig, cm.quicConfig); err != nil {
				cm.logger.Error().
					Str("server", endpoint.Address).
					Err(err).
					Msg("failed to connect to server")

				mu.Lock()
				connectionErrors = append(connectionErrors, fmt.Errorf("server %s: %w", endpoint.Address, err))
				mu.Unlock()

				// Start reconnection goroutine for failed connection
				cm.startReconnection(endpoint.Address)
				return
			}

			// Register with server
			if err := sc.Register(cm.config.ClientID); err != nil {
				cm.logger.Error().
					Str("server", endpoint.Address).
					Err(err).
					Msg("failed to register with server")

				mu.Lock()
				connectionErrors = append(connectionErrors, fmt.Errorf("server %s registration: %w", endpoint.Address, err))
				mu.Unlock()

				sc.Close()
				cm.startReconnection(endpoint.Address)
				return
			}

			mu.Lock()
			connectedServers = append(connectedServers, sc)
			mu.Unlock()

			cm.logger.Info().
				Str("server", endpoint.Address).
				Msg("successfully connected and registered")
		}(server)
	}

	// Wait for all connection attempts to complete
	wg.Wait()

	// Update load balancer with all connections (including unhealthy ones)
	allConns := cm.GetAllConnections()
	cm.balancer.UpdateConnections(allConns)

	// Log summary
	cm.logger.Info().
		Int("connected", len(connectedServers)).
		Int("failed", len(connectionErrors)).
		Int("total", len(servers)).
		Msg("connection startup complete")

	// Start heartbeat goroutines for connected servers
	for _, sc := range connectedServers {
		cm.wg.Add(1)
		go cm.heartbeatLoop(sc)
	}

	return nil
}

// heartbeatLoop sends periodic heartbeats on a server connection.
func (cm *ConnectionManager) heartbeatLoop(sc *ServerConnection) {
	defer cm.wg.Done()

	interval := cm.config.HeartbeatInterval
	if interval == 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-cm.ctx.Done():
			return
		case <-ticker.C:
			if err := sc.SendHeartbeat(); err != nil {
				cm.logger.Error().
					Str("server", sc.ServerAddr()).
					Err(err).
					Msg("heartbeat failed, starting reconnection")

				// Start reconnection
				cm.startReconnection(sc.ServerAddr())
				return
			}
		}
	}
}

// CalculateBackoff calculates the backoff duration for a given attempt number.
// The backoff follows: 5s, 10s, 20s, 40s, 60s (max)
func CalculateBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return InitialBackoff
	}

	backoff := InitialBackoff
	for i := 0; i < attempt; i++ {
		backoff *= BackoffFactor
		if backoff > MaxBackoff {
			return MaxBackoff
		}
	}
	return backoff
}

// startReconnection starts a reconnection goroutine for a server if not already reconnecting.
func (cm *ConnectionManager) startReconnection(serverAddr string) {
	cm.reconnectMu.Lock()
	if cm.reconnecting[serverAddr] {
		cm.reconnectMu.Unlock()
		return
	}
	cm.reconnecting[serverAddr] = true
	cm.reconnectMu.Unlock()

	cm.wg.Add(1)
	go cm.reconnectionLoop(serverAddr)
}

// reconnectionLoop attempts to reconnect to a server with exponential backoff.
func (cm *ConnectionManager) reconnectionLoop(serverAddr string) {
	defer cm.wg.Done()
	defer func() {
		cm.reconnectMu.Lock()
		delete(cm.reconnecting, serverAddr)
		cm.reconnectMu.Unlock()
	}()

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

	attempt := 0
	for {
		select {
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
		case <-cm.ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Reuse existing session cache for reconnection
		sessionCache := cm.sessionCaches.GetOrCreate(serverAddr)

		// Create new server connection
		sc := NewServerConnection(
			endpoint.Address,
			endpoint.ServerName,
			sessionCache,
			cm.logger,
		)

		// Attempt to connect
		connectCtx, cancel := context.WithTimeout(cm.ctx, 30*time.Second)
		err := sc.Connect(connectCtx, cm.baseTLSConfig, cm.quicConfig)
		cancel()

		if err != nil {
			cm.logger.Warn().
				Str("server", serverAddr).
				Int("attempt", attempt+1).
				Err(err).
				Msg("reconnection attempt failed")
			attempt++
			continue
		}

		// Register with server
		if err := sc.Register(cm.config.ClientID); err != nil {
			cm.logger.Warn().
				Str("server", serverAddr).
				Int("attempt", attempt+1).
				Err(err).
				Msg("registration failed during reconnection")
			sc.Close()
			attempt++
			continue
		}

		// Success - update connection map and balancer
		cm.connections.Store(serverAddr, sc)
		cm.balancer.UpdateConnections(cm.GetAllConnections())

		cm.logger.Info().
			Str("server", serverAddr).
			Int("attempts", attempt+1).
			Msg("reconnection successful")

		// Start heartbeat loop for reconnected server
		cm.wg.Add(1)
		go cm.heartbeatLoop(sc)

		return
	}
}

// Stop gracefully shuts down all connections.
// It cancels the context to stop heartbeat and reconnection goroutines,
// then closes all server connections. Active streams can complete naturally.
func (cm *ConnectionManager) Stop() error {
	cm.logger.Info().Msg("stopping connection manager")

	// Cancel context to stop all goroutines
	cm.cancel()

	// Wait for all goroutines to finish (with timeout)
	done := make(chan struct{})
	go func() {
		cm.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		cm.logger.Debug().Msg("all goroutines stopped")
	case <-time.After(10 * time.Second):
		cm.logger.Warn().Msg("timeout waiting for goroutines to stop")
	}

	// Close all connections
	var closeErrors []error
	cm.connections.Range(func(key, value interface{}) bool {
		sc := value.(*ServerConnection)
		if err := sc.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close %s: %w", key.(string), err))
		}
		return true
	})

	if len(closeErrors) > 0 {
		cm.logger.Warn().Int("errors", len(closeErrors)).Msg("errors during shutdown")
	}

	cm.logger.Info().Msg("connection manager stopped")
	return nil
}

// GetHealthyConnection returns a healthy connection using load balancing.
func (cm *ConnectionManager) GetHealthyConnection() (*ServerConnection, error) {
	return cm.balancer.Select()
}

// GetAllConnections returns all server connections.
func (cm *ConnectionManager) GetAllConnections() []*ServerConnection {
	var conns []*ServerConnection
	cm.connections.Range(func(key, value interface{}) bool {
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
	return cm.balancer.HealthyCount()
}

// TotalCount returns the total number of connections.
func (cm *ConnectionManager) TotalCount() int {
	count := 0
	cm.connections.Range(func(_, _ interface{}) bool {
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

// Balancer returns the load balancer.
func (cm *ConnectionManager) Balancer() *LoadBalancer {
	return cm.balancer
}
