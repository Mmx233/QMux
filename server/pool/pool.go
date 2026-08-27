package pool

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

// ConnectionPool manages client connections for a QUIC listener
type ConnectionPool struct {
	mu           sync.RWMutex
	clients      map[string]*ClientConn // clientID -> selectable connection
	reservations map[string]*Reservation
	quicAddr     string       // QUIC listen address this pool serves
	balancer     LoadBalancer // Load balancing strategy
	logger       zerolog.Logger

	// Cached client slice to avoid allocation on Select
	// Using atomic.Pointer for lock-free reads on the hot path
	cachedClients atomic.Pointer[[]*ClientConn]

	ctx    context.Context
	cancel context.CancelFunc
}

// ClientConn represents one connection generation for a client ID.
// A pointer may be added to a pool only once. ID must not change while a
// reservation is pending or after Add/Commit succeeds.
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
	tcpPending  atomic.Int64

	// Health
	healthy atomic.Bool

	// Registration tracks whether this pointer has been consumed as a generation.
	added atomic.Bool
}

// TCPAdmission walks a stable snapshot of TCP-capable connection generations.
// It is owned by one traffic handler.
type TCPAdmission struct {
	pool       *ConnectionPool
	candidates []*ClientConn
	next       int
	balanced   bool
	balanceErr error
}

// TCPLease accounts for one TCP setup against an exact connection generation.
// It is owned by one traffic handler and remains valid if that generation is replaced.
type TCPLease struct {
	pool  *ConnectionPool
	conn  *ClientConn
	state tcpLeaseState
}

// Reservation holds an unpublished client generation while registration is
// acknowledged. A reservation belongs to exactly one pool and client pointer.
type Reservation struct {
	pool *ConnectionPool
	conn *ClientConn
	id   string

	state atomic.Uint32
}

const (
	reservationPending uint32 = iota
	reservationCommitted
	reservationAborted
)

type tcpLeaseState uint8

const (
	tcpLeasePending tcpLeaseState = iota
	tcpLeaseActive
	tcpLeaseReleased
)

const maxPendingTCPSetupsPerClient = 16

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
		clients:      make(map[string]*ClientConn),
		reservations: make(map[string]*Reservation),
		quicAddr:     quicAddr,
		balancer:     balancer,
		logger:       logger.With().Str("quic_addr", quicAddr).Logger(),
		ctx:          ctx,
		cancel:       cancel,
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
	reservation, err := p.Reserve(conn)
	if err != nil {
		return err
	}
	if err := p.Commit(reservation); err != nil {
		p.Abort(reservation)
		return err
	}
	return nil
}

// Reserve atomically claims conn.ID without making conn selectable. Current
// and pending registrations share the same ID namespace. A successful
// reservation does not consume conn as a generation until Commit succeeds.
func (p *ConnectionPool) Reserve(conn *ClientConn) (*Reservation, error) {
	if conn == nil {
		return nil, fmt.Errorf("client connection is nil")
	}
	if conn.ID == "" {
		return nil, fmt.Errorf("client ID is empty")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.clients[conn.ID]; exists {
		return nil, fmt.Errorf("client %s already exists in pool", conn.ID)
	}
	if _, exists := p.reservations[conn.ID]; exists {
		return nil, fmt.Errorf("client %s already has a pending registration", conn.ID)
	}
	if conn.added.Load() {
		return nil, fmt.Errorf("client connection %s was already added to a pool", conn.ID)
	}

	reservation := &Reservation{pool: p, conn: conn, id: conn.ID}
	p.reservations[conn.ID] = reservation
	return reservation, nil
}

// Commit publishes the exact generation held by reservation. It consumes the
// ClientConn generation token only after verifying that the reservation is
// still current and that its ID wasn't mutated.
func (p *ConnectionPool) Commit(reservation *Reservation) error {
	if reservation == nil || reservation.pool != p {
		return fmt.Errorf("invalid pool reservation")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if reservation.state.Load() != reservationPending || p.reservations[reservation.id] != reservation {
		return fmt.Errorf("client reservation is no longer pending")
	}
	if reservation.conn == nil || reservation.conn.ID != reservation.id {
		return fmt.Errorf("reserved client ID changed before commit")
	}
	if _, exists := p.clients[reservation.id]; exists {
		return fmt.Errorf("client %s already exists in pool", reservation.id)
	}
	if !reservation.conn.added.CompareAndSwap(false, true) {
		return fmt.Errorf("client connection %s was already added to a pool", reservation.id)
	}

	reservation.conn.healthy.Store(true)
	p.clients[reservation.id] = reservation.conn
	delete(p.reservations, reservation.id)
	reservation.state.Store(reservationCommitted)

	// Invalidate cache by setting to nil
	p.cachedClients.Store(nil)

	p.logger.Info().
		Str("client_id", reservation.conn.ID).
		Time("registered_at", reservation.conn.RegisteredAt).
		Str("version", reservation.conn.Metadata.Version).
		Strs("capabilities", reservation.conn.Metadata.Capabilities).
		Msg("client added to pool")

	return nil
}

// Abort removes reservation only if it is still the exact pending claim.
// Repeated or stale aborts are harmless.
func (p *ConnectionPool) Abort(reservation *Reservation) bool {
	if reservation == nil || reservation.pool != p {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if reservation.state.Load() != reservationPending || p.reservations[reservation.id] != reservation {
		return false
	}
	delete(p.reservations, reservation.id)
	reservation.state.Store(reservationAborted)
	return true
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

// Select chooses a client without capability filtering. Traffic routing should
// use SelectProtocol.
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

// SelectProtocol chooses a healthy client that supports protocol.
func (p *ConnectionPool) SelectProtocol(protocol string) (*ClientConn, error) {
	clientsPtr := p.cachedClients.Load()
	var clients []*ClientConn
	if clientsPtr != nil {
		clients = *clientsPtr
	} else {
		clients = p.rebuildClientSlice()
	}
	if len(clients) == 0 {
		return nil, ErrNoClientsAvailable
	}

	eligible := make([]*ClientConn, 0, len(clients))
	for _, conn := range clients {
		if isEligible(conn, protocol) {
			eligible = append(eligible, conn)
		}
	}
	if len(eligible) == 0 {
		return nil, ErrNoEligibleClients
	}
	return p.balancer.Select(eligible)
}

// BeginTCPAdmission snapshots eligible generations. Generations added later are
// intentionally ignored.
func (p *ConnectionPool) BeginTCPAdmission() (*TCPAdmission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.clients) == 0 {
		return nil, ErrNoClientsAvailable
	}
	clients := p.clientSliceLocked()
	candidates := make([]*ClientConn, 0, len(clients))
	for _, conn := range clients {
		if isEligible(conn, "tcp") {
			candidates = append(candidates, conn)
		}
	}
	if len(candidates) == 0 {
		return nil, ErrNoEligibleClients
	}

	return &TCPAdmission{pool: p, candidates: candidates}, nil
}

// Next reserves the next still-current candidate. The first call selects and
// reserves under one pool lock; later calls walk the same snapshot without
// invoking the balancer again. A nil lease means the snapshot is exhausted or
// every remaining generation is saturated.
func (a *TCPAdmission) Next() (*TCPLease, error) {
	if a == nil || a.pool == nil {
		return nil, nil
	}

	a.pool.mu.Lock()
	defer a.pool.mu.Unlock()
	if !a.balanced {
		a.balanced = true
		eligible := make([]*ClientConn, 0, len(a.candidates))
		for _, conn := range a.candidates {
			if a.pool.isCurrentLocked(conn) &&
				isEligible(conn, "tcp") &&
				conn.tcpPending.Load() < maxPendingTCPSetupsPerClient {
				eligible = append(eligible, conn)
			}
		}
		if len(eligible) != 0 {
			selected, err := a.pool.balancer.Select(eligible)
			if err != nil {
				a.balanceErr = err
				return nil, err
			}
			if slices.Index(eligible, selected) < 0 {
				a.balanceErr = fmt.Errorf("load balancer selected a client outside the TCP candidate set")
				return nil, a.balanceErr
			}
			selectedIndex := slices.Index(a.candidates, selected)
			a.candidates[0], a.candidates[selectedIndex] = a.candidates[selectedIndex], a.candidates[0]
		}
	}
	if a.balanceErr != nil {
		return nil, a.balanceErr
	}

	for a.next < len(a.candidates) {
		conn := a.candidates[a.next]
		a.next++

		if a.pool.isCurrentLocked(conn) &&
			isEligible(conn, "tcp") &&
			conn.tcpPending.Load() < maxPendingTCPSetupsPerClient {
			conn.tcpPending.Add(1)
			return &TCPLease{pool: a.pool, conn: conn, state: tcpLeasePending}, nil
		}
	}
	return nil, nil
}

// Client returns the exact generation held by the lease.
func (l *TCPLease) Client() *ClientConn {
	if l == nil {
		return nil
	}
	return l.conn
}

// Commit moves a pending setup to the established connection counters.
func (l *TCPLease) Commit() bool {
	if l == nil || l.pool == nil {
		return false
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if l.state != tcpLeasePending {
		return false
	}

	// Lock-free load balancing may briefly overcount, but must never undercount.
	l.conn.ActiveConns.Add(1)
	l.conn.TotalConns.Add(1)
	l.conn.tcpPending.Add(-1)
	l.state = tcpLeaseActive
	return true
}

// Release idempotently balances either a pending or established lease.
func (l *TCPLease) Release() bool {
	if l == nil || l.pool == nil {
		return false
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()

	switch l.state {
	case tcpLeasePending:
		l.conn.tcpPending.Add(-1)
	case tcpLeaseActive:
		l.conn.ActiveConns.Add(-1)
	case tcpLeaseReleased:
		return false
	}
	l.state = tcpLeaseReleased
	return true
}

// rebuildClientSlice rebuilds the cached client slice from the map
func (p *ConnectionPool) rebuildClientSlice() []*ClientConn {
	// Use write lock to prevent multiple goroutines from rebuilding simultaneously
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.clientSliceLocked()
}

func (p *ConnectionPool) clientSliceLocked() []*ClientConn {
	// Double-check if another goroutine already rebuilt while we waited for the lock.
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

// EligibleCount returns the number of healthy clients that support protocol.
func (p *ConnectionPool) EligibleCount(protocol string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0
	for _, conn := range p.clients {
		if isEligible(conn, protocol) {
			count++
		}
	}
	return count
}

func isEligible(conn *ClientConn, protocol string) bool {
	if conn == nil || !conn.healthy.Load() || (protocol != "tcp" && protocol != "udp") {
		return false
	}
	return slices.Contains(conn.Metadata.Capabilities, protocol)
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
	ErrNoEligibleClients  = fmt.Errorf("no eligible clients available")
)
