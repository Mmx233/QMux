package pool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

// ConnectionPool manages client connections for a QUIC listener
type ConnectionPool struct {
	mu       sync.RWMutex
	clients  map[string]*ClientConn // clientID -> connection
	quicAddr string                 // QUIC listen address this pool serves
	balancer LoadBalancer           // Load balancing strategy
	logger   zerolog.Logger

	// Cached client slice to avoid allocation on Select
	// Using atomic.Pointer for lock-free reads on the hot path
	cachedClients atomic.Pointer[[]*ClientConn]

	ctx    context.Context
	cancel context.CancelFunc
}

// ClientConn represents a connected client
type ClientConn struct {
	ID            string
	Conn          *quic.Conn
	ControlStream *quic.Stream
	RegisteredAt  time.Time
	LastSeen      time.Time
	Metadata      ClientMetadata

	// Connection tracking
	ActiveConns atomic.Int64
	TotalConns  atomic.Uint64

	// Health
	healthy atomic.Bool
}

// ClientMetadata contains client information
type ClientMetadata struct {
	Version      string
	Capabilities []string
	Labels       map[string]string // For future filtering
}

// New creates a new connection pool
func New(quicAddr string, balancer LoadBalancer, logger zerolog.Logger) *ConnectionPool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &ConnectionPool{
		clients:  make(map[string]*ClientConn),
		quicAddr: quicAddr,
		balancer: balancer,
		logger:   logger.With().Str("quic_addr", quicAddr).Logger(),
		ctx:      ctx,
		cancel:   cancel,
	}

	return p
}

// Stop stops the connection pool
func (p *ConnectionPool) Stop() {
	p.cancel()
}

// Add registers a new client connection
func (p *ConnectionPool) Add(clientID string, conn *ClientConn) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.clients[clientID]; exists {
		return fmt.Errorf("client %s already exists in pool", clientID)
	}

	conn.healthy.Store(true)
	p.clients[clientID] = conn

	// Invalidate cache by setting to nil
	p.cachedClients.Store(nil)

	p.logger.Info().
		Str("client_id", clientID).
		Str("version", conn.Metadata.Version).
		Strs("capabilities", conn.Metadata.Capabilities).
		Msg("client added to pool")

	return nil
}

// Remove removes a client from the pool
func (p *ConnectionPool) Remove(clientID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if conn, exists := p.clients[clientID]; exists {
		conn.healthy.Store(false)
		delete(p.clients, clientID)

		// Invalidate cache by setting to nil
		p.cachedClients.Store(nil)

		p.logger.Info().
			Str("client_id", clientID).
			Int64("active_conns", conn.ActiveConns.Load()).
			Uint64("total_conns", conn.TotalConns.Load()).
			Msg("client removed from pool")
	}
}

// Select chooses a client using the load balancer
func (p *ConnectionPool) Select() (*ClientConn, error) {
	// Fast path: use cached slice if available (lock-free read)
	clientsPtr := p.cachedClients.Load()
	if clientsPtr != nil {
		clients := *clientsPtr
		if len(clients) == 0 {
			return nil, ErrNoClientsAvailable
		}
		return p.balancer.Select(clients)
	}

	// Slow path: rebuild cache (rare)
	clients := p.rebuildClientSlice()
	if len(clients) == 0 {
		return nil, ErrNoClientsAvailable
	}

	return p.balancer.Select(clients)
}

// rebuildClientSlice rebuilds the cached client slice from the map
func (p *ConnectionPool) rebuildClientSlice() []*ClientConn {
	// Use write lock to prevent multiple goroutines from rebuilding simultaneously
	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check if another goroutine already rebuilt while we waited for the lock
	clientsPtr := p.cachedClients.Load()
	if clientsPtr != nil {
		return *clientsPtr
	}

	clients := make([]*ClientConn, 0, len(p.clients))
	for _, conn := range p.clients {
		clients = append(clients, conn)
	}

	// Store the new slice atomically
	p.cachedClients.Store(&clients)

	return clients
}

// Get retrieves a specific client by ID
func (p *ConnectionPool) Get(clientID string) (*ClientConn, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	conn, exists := p.clients[clientID]
	return conn, exists
}

// List returns all clients in the pool
func (p *ConnectionPool) List() []*ClientConn {
	p.mu.RLock()
	defer p.mu.RUnlock()

	clients := make([]*ClientConn, 0, len(p.clients))
	for _, conn := range p.clients {
		clients = append(clients, conn)
	}
	return clients
}

// Count returns the number of clients in the pool
func (p *ConnectionPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.clients)
}

// HealthyCount returns the number of healthy clients
func (p *ConnectionPool) HealthyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0
	for _, conn := range p.clients {
		if conn.healthy.Load() {
			count++
		}
	}
	return count
}

// UpdateLastSeen updates the last seen timestamp for a client
func (p *ConnectionPool) UpdateLastSeen(clientID string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if conn, exists := p.clients[clientID]; exists {
		conn.LastSeen = time.Now()
	}
}

// MarkUnhealthy marks a client as unhealthy
func (p *ConnectionPool) MarkUnhealthy(clientID string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if conn, exists := p.clients[clientID]; exists {
		conn.healthy.Store(false)
		p.logger.Warn().Str("client_id", clientID).Msg("client marked unhealthy")
	}
}

// MarkHealthy marks a client as healthy
func (p *ConnectionPool) MarkHealthy(clientID string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if conn, exists := p.clients[clientID]; exists {
		conn.healthy.Store(true)
		p.logger.Info().Str("client_id", clientID).Msg("client marked healthy")
	}
}

// Errors
var (
	ErrNoClientsAvailable = fmt.Errorf("no clients available in pool")
	ErrNoHealthyClients   = fmt.Errorf("no healthy clients available")
)
