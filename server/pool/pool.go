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

// ClientConn represents one connection generation for a client ID.
// A pointer may be added to a pool only once, and ID must not change after Add succeeds.
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

	// Registration tracks whether this pointer has been consumed as a generation.
	added atomic.Bool
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

// Add registers a new client connection.
//
// conn must be non-nil and have a non-empty ID. After Add succeeds, conn is a
// single-use generation token and its ID must remain immutable. A duplicate ID
// is rejected while it has a current registration; pointers already used by any
// pool are also rejected. On success, conn is healthy and eligible for
// selection. Validation and duplicate-ID failures do not consume an otherwise
// unused pointer, so callers may correct it and retry.
func (p *ConnectionPool) Add(conn *ClientConn) error {
	if conn == nil {
		return fmt.Errorf("client connection is nil")
	}
	if conn.ID == "" {
		return fmt.Errorf("client ID is empty")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.clients[conn.ID]; exists {
		return fmt.Errorf("client %s already exists in pool", conn.ID)
	}
	if !conn.added.CompareAndSwap(false, true) {
		return fmt.Errorf("client connection %s was already added to a pool", conn.ID)
	}

	conn.healthy.Store(true)
	p.clients[conn.ID] = conn

	// Invalidate cache by setting to nil
	p.cachedClients.Store(nil)

	p.logger.Info().
		Str("client_id", conn.ID).
		Time("registered_at", conn.RegisteredAt).
		Str("version", conn.Metadata.Version).
		Strs("capabilities", conn.Metadata.Capabilities).
		Msg("client added to pool")

	return nil
}

// Remove removes the expected client generation from the pool.
// It reports whether expected was still current and the removal was applied.
func (p *ConnectionPool) Remove(expected *ClientConn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.isCurrentLocked(expected) {
		return false
	}

	expected.healthy.Store(false)
	delete(p.clients, expected.ID)
	p.cachedClients.Store(nil)

	p.logger.Info().
		Str("client_id", expected.ID).
		Time("registered_at", expected.RegisteredAt).
		Int64("active_conns", expected.ActiveConns.Load()).
		Uint64("total_conns", expected.TotalConns.Load()).
		Msg("client removed from pool")

	return true
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

// UpdateLastSeen updates the last seen timestamp for the expected client generation.
// It reports whether expected was still current and the update was applied.
func (p *ConnectionPool) UpdateLastSeen(expected *ClientConn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.isCurrentLocked(expected) {
		return false
	}
	expected.LastSeen = time.Now()
	return true
}

// MarkUnhealthy marks the expected client generation as unhealthy.
// It reports whether expected was still current and the update was applied.
func (p *ConnectionPool) MarkUnhealthy(expected *ClientConn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.isCurrentLocked(expected) {
		return false
	}
	expected.healthy.Store(false)
	p.cachedClients.Store(nil)
	p.logger.Warn().
		Str("client_id", expected.ID).
		Time("registered_at", expected.RegisteredAt).
		Msg("client marked unhealthy")
	return true
}

// MarkHealthy marks the expected client generation as healthy.
// It reports whether expected was still current and the update was applied.
func (p *ConnectionPool) MarkHealthy(expected *ClientConn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.isCurrentLocked(expected) {
		return false
	}
	expected.healthy.Store(true)
	p.cachedClients.Store(nil)
	p.logger.Info().
		Str("client_id", expected.ID).
		Time("registered_at", expected.RegisteredAt).
		Msg("client marked healthy")
	return true
}

func (p *ConnectionPool) isCurrentLocked(expected *ClientConn) bool {
	if expected == nil || expected.ID == "" {
		return false
	}
	current, exists := p.clients[expected.ID]
	return exists && current == expected
}

// Errors
var (
	ErrNoClientsAvailable = fmt.Errorf("no clients available in pool")
	ErrNoHealthyClients   = fmt.Errorf("no healthy clients available")
)
