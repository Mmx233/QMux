package pool

import (
	"context"
	"errors"
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
	mu                   sync.RWMutex
	clients              map[string]*ClientConn // clientID -> selectable connection
	reservations         map[string]*Reservation
	retiring             map[*ClientConn]*Retirement
	serverPending        int64
	accountingFaults     uint64
	limits               Limits
	pendingRegistrations capacityState
	clientGenerations    capacityState
	tcpConnections       capacityState
	pendingTCPSetups     capacityState
	udpSessions          capacityState
	quicAddr             string       // QUIC listen address this pool serves
	balancer             LoadBalancer // Load balancing strategy
	logger               zerolog.Logger

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
	Metadata      ClientMetadata

	// Connection tracking
	ActiveConns atomic.Int64
	TotalConns  atomic.Uint64
	tcpPending  atomic.Int64
	tcpActive   atomic.Int64
	udpSessions atomic.Int64
	tcpStreamID atomic.Int64
	tcpOpened   atomic.Bool

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

// Reservation holds one accepted connection from pending through registration.
type Reservation struct {
	pool *ConnectionPool
	conn *ClientConn
	id   string

	state atomic.Uint32
}

// Retirement holds an exact generation out of selection until owner cleanup
// and all traffic leases finish.
type Retirement struct {
	pool       *ConnectionPool
	conn       *ClientConn
	done       bool
	tcpDrained chan int64
	drainSent  bool
}

const (
	reservationPending uint32 = iota
	reservationReserved
	reservationCommitted
	reservationAborted
)

type tcpLeaseState uint8

const (
	tcpLeasePending tcpLeaseState = iota
	tcpLeaseActive
	tcpLeaseReleased
)

const (
	defaultMaxClientGenerations             int64 = 16
	defaultMaxPendingRegistrations          int64 = 128
	defaultMaxTCPConnectionsPerGeneration   int64 = 100
	defaultMaxPendingTCPSetupsPerGeneration int64 = 16
	defaultMaxUDPSessionsPerGeneration      int64 = 256
)

// Limits are immutable capacity bounds owned by one QUIC listener pool.
// NewWithLimits callers must pass positive, fully defaulted values.
type Limits struct {
	MaxClientGenerations             int64
	MaxPendingRegistrations          int64
	MaxTCPConnectionsPerGeneration   int64
	MaxPendingTCPSetupsPerGeneration int64
	MaxUDPSessionsPerGeneration      int64
}

type capacityState struct {
	highWater int64
	drops     uint64
}

func (s *capacityState) observe(current int64) {
	if current > s.highWater {
		s.highWater = current
	}
}

func (s *capacityState) snapshot(current, limit int64) LimitSnapshot {
	return LimitSnapshot{
		Current:       current,
		HighWater:     max(current, s.highWater),
		Limit:         limit,
		CapacityDrops: s.drops,
	}
}

// ClientMetadata contains client information
type ClientMetadata struct {
	Version      string
	Capabilities []string
	Labels       map[string]string // For future filtering
}

// LimitSnapshot reports one capacity owner's current and lifetime counters.
type LimitSnapshot struct {
	Current       int64
	HighWater     int64
	Limit         int64
	CapacityDrops uint64
}

// CapacitySnapshot is a point-in-time, value-only view of generation and
// traffic accounting for one QUIC listener.
type CapacitySnapshot struct {
	ServerPending                 int64
	Reservations                  int
	Registered                    int
	ServerRetiring                int
	TCPPending                    int64
	TCPActive                     int64
	UDPSessions                   int64
	AccountingFaults              uint64
	PendingRegistrations          LimitSnapshot
	ClientGenerations             LimitSnapshot
	TCPConnectionsPerGeneration   LimitSnapshot
	PendingTCPSetupsPerGeneration LimitSnapshot
	UDPSessionsPerGeneration      LimitSnapshot
}

// New creates a new connection pool
func New(quicAddr string, balancer LoadBalancer, logger zerolog.Logger) *ConnectionPool {
	return NewWithLimits(quicAddr, balancer, logger, defaultLimits())
}

func defaultLimits() Limits {
	return Limits{
		MaxClientGenerations:             defaultMaxClientGenerations,
		MaxPendingRegistrations:          defaultMaxPendingRegistrations,
		MaxTCPConnectionsPerGeneration:   defaultMaxTCPConnectionsPerGeneration,
		MaxPendingTCPSetupsPerGeneration: defaultMaxPendingTCPSetupsPerGeneration,
		MaxUDPSessionsPerGeneration:      defaultMaxUDPSessionsPerGeneration,
	}
}

// NewWithLimits creates a connection pool with immutable capacity limits.
func NewWithLimits(quicAddr string, balancer LoadBalancer, logger zerolog.Logger, limits Limits) *ConnectionPool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &ConnectionPool{
		clients:      make(map[string]*ClientConn),
		reservations: make(map[string]*Reservation),
		retiring:     make(map[*ClientConn]*Retirement),
		limits:       limits,
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

// BeginPending starts accounting for one accepted, post-handshake server
// connection before its registration identity is known.
func (p *ConnectionPool) BeginPending() *Reservation {
	reservation, _ := p.beginPending()
	return reservation
}

func (p *ConnectionPool) beginPending() (*Reservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accountingFaults != 0 {
		return nil, ErrAccountingFault
	}
	if p.serverPending >= p.limits.MaxPendingRegistrations {
		p.pendingRegistrations.drops++
		return nil, ErrPendingRegistrationCapacity
	}
	p.serverPending++
	p.pendingRegistrations.observe(p.serverPending)
	return &Reservation{pool: p}, nil
}

// Reserve atomically claims conn.ID without making conn selectable. Current
// and pending registrations share the same ID namespace. A successful
// reservation does not consume conn as a generation until Commit succeeds.
func (p *ConnectionPool) Reserve(conn *ClientConn) (*Reservation, error) {
	reservation, err := p.beginPending()
	if err != nil {
		return nil, err
	}
	if err := reservation.Reserve(conn); err != nil {
		p.Abort(reservation)
		return nil, err
	}
	return reservation, nil
}

// Reserve binds an accepted pending connection to an exact client generation.
func (r *Reservation) Reserve(conn *ClientConn) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("invalid pool reservation")
	}
	if conn == nil {
		return fmt.Errorf("client connection is nil")
	}
	if conn.ID == "" {
		return fmt.Errorf("client ID is empty")
	}

	p := r.pool
	p.mu.Lock()
	defer p.mu.Unlock()

	if r.state.Load() != reservationPending {
		return fmt.Errorf("client reservation is no longer pending")
	}
	if p.accountingFaults != 0 {
		return ErrAccountingFault
	}
	if _, exists := p.clients[conn.ID]; exists {
		return fmt.Errorf("client %s already exists in pool", conn.ID)
	}
	if _, exists := p.reservations[conn.ID]; exists {
		return fmt.Errorf("client %s already has a pending registration", conn.ID)
	}
	if conn.added.Load() {
		return fmt.Errorf("client connection %s was already added to a pool", conn.ID)
	}
	if p.clientGenerationCountLocked() >= p.limits.MaxClientGenerations {
		p.clientGenerations.drops++
		return ErrClientGenerationCapacity
	}

	r.conn = conn
	r.id = conn.ID
	r.state.Store(reservationReserved)
	p.reservations[conn.ID] = r
	p.clientGenerations.observe(p.clientGenerationCountLocked())
	return nil
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

	if reservation.state.Load() != reservationReserved || p.reservations[reservation.id] != reservation {
		return fmt.Errorf("client reservation is no longer pending")
	}
	if p.accountingFaults != 0 {
		return ErrAccountingFault
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
	p.serverPending--
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

	state := reservation.state.Load()
	if state != reservationPending && state != reservationReserved {
		return false
	}
	if state == reservationReserved {
		if p.reservations[reservation.id] != reservation {
			return false
		}
		delete(p.reservations, reservation.id)
	}
	p.serverPending--
	reservation.state.Store(reservationAborted)
	return true
}

// BeginRetire removes the exact current generation from selection and holds it
// in retiring accounting until Done. Repeated or stale calls are harmless.
func (p *ConnectionPool) BeginRetire(expected *ClientConn) *Retirement {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.isCurrentLocked(expected) {
		return nil
	}
	expected.healthy.Store(false)
	delete(p.clients, expected.ID)
	retirement := &Retirement{pool: p, conn: expected, tcpDrained: make(chan int64, 1)}
	p.retiring[expected] = retirement
	p.cachedClients.Store(nil)
	p.signalTCPDrainedLocked(retirement)

	p.logger.Info().
		Str("client_id", expected.ID).
		Time("registered_at", expected.RegisteredAt).
		Int64("active_conns", expected.ActiveConns.Load()).
		Uint64("total_conns", expected.TotalConns.Load()).
		Msg("client retiring from pool")
	return retirement
}

// TCPDrained reports the last successfully opened server TCP stream exactly once.
func (r *Retirement) TCPDrained() <-chan int64 {
	if r == nil {
		return nil
	}
	return r.tcpDrained
}

func (p *ConnectionPool) signalTCPDrainedLocked(retirement *Retirement) {
	if retirement == nil || retirement.drainSent || retirement.conn.tcpPending.Load()+retirement.conn.tcpActive.Load() != 0 {
		return
	}
	fence := int64(-1)
	if retirement.conn.tcpOpened.Load() {
		fence = retirement.conn.tcpStreamID.Load()
	}
	retirement.tcpDrained <- fence
	retirement.drainSent = true
}

// Done idempotently completes retirement of the exact generation.
func (r *Retirement) Done() bool {
	if r == nil || r.pool == nil {
		return false
	}
	r.pool.mu.Lock()
	defer r.pool.mu.Unlock()
	if r.done || r.pool.retiring[r.conn] != r {
		return false
	}
	r.done = true
	r.pool.finalizeRetirementLocked(r.conn)
	return true
}

func (p *ConnectionPool) finalizeRetirementLocked(conn *ClientConn) {
	retirement := p.retiring[conn]
	if retirement != nil && retirement.done && conn.tcpPending.Load() == 0 &&
		conn.tcpActive.Load() == 0 && conn.udpSessions.Load() == 0 {
		delete(p.retiring, conn)
	}
}

// Remove removes the expected client generation from the pool.
// It reports whether expected was still current and the removal was applied.
func (p *ConnectionPool) Remove(expected *ClientConn) bool {
	retirement := p.BeginRetire(expected)
	if retirement == nil {
		return false
	}
	return retirement.Done()
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

// ReserveUDP selects and reserves one exact current UDP generation.
func (p *ConnectionPool) ReserveUDP() (*ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.accountingFaults != 0 {
		return nil, ErrAccountingFault
	}
	if len(p.clients) == 0 {
		return nil, ErrNoClientsAvailable
	}
	eligible := make([]*ClientConn, 0, len(p.clients))
	hasEligible := false
	for _, conn := range p.clients {
		if !isEligible(conn, "udp") {
			continue
		}
		hasEligible = true
		if conn.udpSessions.Load() < p.limits.MaxUDPSessionsPerGeneration {
			eligible = append(eligible, conn)
		}
	}
	if !hasEligible {
		return nil, ErrNoEligibleClients
	}
	if len(eligible) == 0 {
		p.udpSessions.drops++
		return nil, ErrUDPGenerationCapacity
	}

	selected, err := p.balancer.Select(eligible)
	if err != nil {
		return nil, err
	}
	if slices.Index(eligible, selected) < 0 {
		return nil, fmt.Errorf("load balancer selected a client outside the UDP candidate set")
	}
	current := selected.udpSessions.Add(1)
	p.udpSessions.observe(current)
	return selected, nil
}

// ReleaseUDP releases one reservation from the exact generation pointer.
func (p *ConnectionPool) ReleaseUDP(conn *ClientConn) bool {
	if conn == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if conn.udpSessions.Load() <= 0 {
		p.accountingFaults++
		return false
	}
	conn.udpSessions.Add(-1)
	p.finalizeRetirementLocked(conn)
	return true
}

// IsCurrentEligible reports whether conn is still the exact selectable generation.
func (p *ConnectionPool) IsCurrentEligible(conn *ClientConn, protocol string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.isCurrentLocked(conn) && isEligible(conn, protocol)
}

// BeginTCPAdmission snapshots eligible generations. Generations added later are
// intentionally ignored.
func (p *ConnectionPool) BeginTCPAdmission() (*TCPAdmission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.accountingFaults != 0 {
		return nil, ErrAccountingFault
	}
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
// invoking the balancer again. A nil lease means the snapshot was otherwise
// exhausted.
func (a *TCPAdmission) Next() (*TCPLease, error) {
	if a == nil || a.pool == nil {
		return nil, nil
	}

	a.pool.mu.Lock()
	defer a.pool.mu.Unlock()
	if a.pool.accountingFaults != 0 {
		return nil, ErrAccountingFault
	}
	if !a.balanced {
		a.balanced = true
		eligible := make([]*ClientConn, 0, len(a.candidates))
		for _, conn := range a.candidates {
			if a.pool.isCurrentLocked(conn) &&
				isEligible(conn, "tcp") &&
				a.pool.tcpCapacityErrorLocked(conn) == nil {
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

	atConnectionCapacity := false
	atSetupCapacity := false
	for a.next < len(a.candidates) {
		conn := a.candidates[a.next]
		a.next++

		if !a.pool.isCurrentLocked(conn) || !isEligible(conn, "tcp") {
			continue
		}
		capacityErr := a.pool.tcpCapacityErrorLocked(conn)
		if errors.Is(capacityErr, ErrTCPGenerationConnectionCapacity) {
			atConnectionCapacity = true
			continue
		}
		if errors.Is(capacityErr, ErrTCPGenerationSetupCapacity) {
			atSetupCapacity = true
			continue
		}
		pending := conn.tcpPending.Add(1)
		a.pool.pendingTCPSetups.observe(pending)
		a.pool.tcpConnections.observe(pending + conn.tcpActive.Load())
		return &TCPLease{pool: a.pool, conn: conn, state: tcpLeasePending}, nil
	}
	if atConnectionCapacity {
		a.pool.tcpConnections.drops++
		return nil, ErrTCPGenerationConnectionCapacity
	}
	if atSetupCapacity {
		a.pool.pendingTCPSetups.drops++
		return nil, ErrTCPGenerationSetupCapacity
	}
	return nil, nil
}

func (p *ConnectionPool) tcpCapacityErrorLocked(conn *ClientConn) error {
	if conn.tcpPending.Load()+conn.tcpActive.Load() >= p.limits.MaxTCPConnectionsPerGeneration {
		return ErrTCPGenerationConnectionCapacity
	}
	if conn.tcpPending.Load() >= p.limits.MaxPendingTCPSetupsPerGeneration {
		return ErrTCPGenerationSetupCapacity
	}
	return nil
}

// Client returns the exact generation held by the lease.
func (l *TCPLease) Client() *ClientConn {
	if l == nil {
		return nil
	}
	return l.conn
}

// RecordStream records a successfully opened server-initiated TCP stream.
func (l *TCPLease) RecordStream(streamID int64) {
	if l == nil || l.conn == nil {
		return
	}
	for current := l.conn.tcpStreamID.Load(); streamID > current; current = l.conn.tcpStreamID.Load() {
		if l.conn.tcpStreamID.CompareAndSwap(current, streamID) {
			break
		}
	}
	l.conn.tcpOpened.Store(true)
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
	l.conn.tcpActive.Add(1)
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
		l.conn.tcpActive.Add(-1)
	case tcpLeaseReleased:
		return false
	}
	l.state = tcpLeaseReleased
	l.pool.signalTCPDrainedLocked(l.pool.retiring[l.conn])
	l.pool.finalizeRetirementLocked(l.conn)
	return true
}

// Snapshot returns generation and traffic counters, including exact
// generations held in retirement.
func (p *ConnectionPool) Snapshot() CapacitySnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	clientGenerations := p.clientGenerationCountLocked()
	snapshot := CapacitySnapshot{
		ServerPending:        p.serverPending,
		Reservations:         len(p.reservations),
		Registered:           len(p.clients),
		ServerRetiring:       len(p.retiring),
		AccountingFaults:     p.accountingFaults,
		PendingRegistrations: p.pendingRegistrations.snapshot(p.serverPending, p.limits.MaxPendingRegistrations),
		ClientGenerations:    p.clientGenerations.snapshot(clientGenerations, p.limits.MaxClientGenerations),
	}
	var maxTCPConnections, maxPendingTCPSetups, maxUDPSessions int64
	add := func(conn *ClientConn) {
		tcpPending := conn.tcpPending.Load()
		tcpActive := conn.tcpActive.Load()
		udpSessions := conn.udpSessions.Load()
		snapshot.TCPPending += tcpPending
		snapshot.TCPActive += tcpActive
		snapshot.UDPSessions += udpSessions
		maxTCPConnections = max(maxTCPConnections, tcpPending+tcpActive)
		maxPendingTCPSetups = max(maxPendingTCPSetups, tcpPending)
		maxUDPSessions = max(maxUDPSessions, udpSessions)
	}
	for _, conn := range p.clients {
		add(conn)
	}
	for conn := range p.retiring {
		add(conn)
	}
	snapshot.TCPConnectionsPerGeneration = p.tcpConnections.snapshot(maxTCPConnections, p.limits.MaxTCPConnectionsPerGeneration)
	snapshot.PendingTCPSetupsPerGeneration = p.pendingTCPSetups.snapshot(maxPendingTCPSetups, p.limits.MaxPendingTCPSetupsPerGeneration)
	snapshot.UDPSessionsPerGeneration = p.udpSessions.snapshot(maxUDPSessions, p.limits.MaxUDPSessionsPerGeneration)
	return snapshot
}

func (p *ConnectionPool) clientGenerationCountLocked() int64 {
	return int64(len(p.reservations) + len(p.clients) + len(p.retiring))
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
	ErrNoClientsAvailable              = fmt.Errorf("no clients available in pool")
	ErrNoHealthyClients                = fmt.Errorf("no healthy clients available")
	ErrNoEligibleClients               = fmt.Errorf("no eligible clients available")
	ErrAccountingFault                 = fmt.Errorf("connection pool accounting fault")
	ErrPendingRegistrationCapacity     = fmt.Errorf("pending client registration capacity reached")
	ErrClientGenerationCapacity        = fmt.Errorf("client generation capacity reached")
	ErrTCPGenerationConnectionCapacity = fmt.Errorf("all eligible TCP generations are at connection capacity")
	ErrTCPGenerationSetupCapacity      = fmt.Errorf("all eligible TCP generations are at pending setup capacity")
	ErrUDPGenerationCapacity           = fmt.Errorf("all eligible UDP generations are at capacity")
)
