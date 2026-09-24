package mesh

import (
	"context"
	"errors"
	"sync"

	"github.com/Mmx233/QMux/internal/outbound"
)

var (
	ErrRegistryStopped     = errors.New("mesh registry is stopped")
	ErrPendingCapacity     = errors.New("mesh pending registration capacity reached")
	ErrClientCapacity      = errors.New("mesh client generation capacity reached")
	ErrPeerCapacity        = errors.New("mesh peer generation capacity reached")
	ErrStaleGeneration     = errors.New("stale mesh generation")
	ErrClientGroupConflict = errors.New("mesh client instance belongs to another group")
	ErrClientPrepared      = errors.New("mesh client instance already has a prepared generation")
	ErrClientCurrent       = errors.New("mesh client instance already has a current generation")
	ErrPeerPrepared        = errors.New("mesh peer already has a prepared generation")
	ErrPeerCurrent         = errors.New("mesh peer already has a current generation")
	ErrPeerSuppressed      = errors.New("mesh peer outbound worker is suppressed by arbitration")
)

type Role string

const (
	RoleUnknown Role = ""
	RoleClient  Role = "client"
	RolePeer    Role = "peer"
)

type Direction uint8

const (
	DirectionInbound Direction = iota + 1
	DirectionOutbound
)

type Phase uint8

const (
	PhasePending Phase = iota + 1
	PhasePrepared
	PhaseCurrent
	PhaseRetiring
	PhaseDone
)

// Owner supplies the lock-free shutdown boundary for one exact generation.
// Stop must unblock its I/O; Wait joins all goroutines and resource owners.
type Owner struct {
	Stop func()
	Wait func()
}

type Generation struct {
	registry   *Registry
	id         uint64
	role       Role
	direction  Direction
	phase      Phase
	instanceID string
	groupID    string
	peerID     string
	owner      Owner
	forwarding ForwardingEligibility
	l7Health   InstanceL7Health
	inflight   int
	traffic    map[*tcpSource]*generationTraffic
}

func (g *Generation) ID() uint64 { return g.id }

func (g *Generation) Role() Role {
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	return g.role
}

func (g *Generation) Direction() Direction { return g.direction }

func (g *Generation) Phase() Phase {
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	return g.phase
}

func (g *Generation) InstanceID() string {
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	return g.instanceID
}

func (g *Generation) GroupID() string {
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	return g.groupID
}

func (g *Generation) PeerID() string {
	g.registry.mu.Lock()
	defer g.registry.mu.Unlock()
	return g.peerID
}

// StopOwner and WaitOwner are deliberately separate so callers can stop every
// owner before joining any of them.
func (g *Generation) StopOwner() {
	if g.owner.Stop != nil {
		g.owner.Stop()
	}
}

func (g *Generation) WaitOwner() {
	if g.owner.Wait != nil {
		g.owner.Wait()
	}
}

type clientEntry struct {
	groupID  string
	prepared *Generation
	current  outbound.Current[*Generation]
	retiring map[uint64]*Generation
}

type peerEntry struct {
	prepared *Generation
	current  outbound.Current[*Generation]
	retiring map[uint64]*Generation
}

type Arbitration struct {
	registry      *Registry
	id            uint64
	peerID        string
	inbound       *Generation
	loser         *Generation
	loserReleased bool
}

func (a *Arbitration) ID() uint64           { return a.id }
func (a *Arbitration) PeerID() string       { return a.peerID }
func (a *Arbitration) Inbound() *Generation { return a.inbound }
func (a *Arbitration) Loser() *Generation   { return a.loser }

type Registry struct {
	// ponytail: one process-wide lock keeps exact-generation transitions atomic;
	// shard by identity only if registration contention is measured.
	mu sync.Mutex

	closed         bool
	stopDone       chan struct{}
	changed        chan struct{}
	nextID         uint64
	nextArbID      uint64
	maxPending     int
	maxClients     int
	maxPeers       int
	clientHigh     int
	peerHigh       int
	pendingHigh    int
	all            map[uint64]*Generation
	clients        map[string]*clientEntry
	peers          map[string]*peerEntry
	arbitrations   map[string]*Arbitration
	groupAvailable map[string]bool
	groupChanged   chan struct{}
	roundRobin     map[string]uint64
}

type RegistrySnapshot struct {
	Closed bool

	Pending          int
	PendingHighWater int

	ClientPrepared  int
	ClientCurrent   int
	ClientRetiring  int
	ClientTotal     int
	ClientHighWater int
	ClientBindings  int

	PeerPrepared  int
	PeerCurrent   int
	PeerRetiring  int
	PeerTotal     int
	PeerHighWater int

	Arbitrations int
}

func NewRegistry(maxPending, maxClients, maxPeers int) *Registry {
	return &Registry{
		maxPending:     maxPending,
		maxClients:     maxClients,
		maxPeers:       maxPeers,
		stopDone:       make(chan struct{}),
		changed:        make(chan struct{}),
		all:            make(map[uint64]*Generation),
		clients:        make(map[string]*clientEntry),
		peers:          make(map[string]*peerEntry),
		arbitrations:   make(map[string]*Arbitration),
		groupAvailable: make(map[string]bool),
		groupChanged:   make(chan struct{}),
		roundRobin:     make(map[string]uint64),
	}
}

func (r *Registry) BeginInbound(owner Owner) (*Generation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrRegistryStopped
	}
	if r.countPhaseLocked(RoleUnknown, PhasePending) >= r.maxPending {
		return nil, ErrPendingCapacity
	}
	generation := r.newGenerationLocked(RoleUnknown, DirectionInbound, PhasePending, owner)
	pending := r.countPhaseLocked(RoleUnknown, PhasePending)
	r.pendingHigh = max(r.pendingHigh, pending)
	r.signalLocked()
	return generation, nil
}

func (r *Registry) BeginOutboundPeer(peerID string, owner Owner) (*Generation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrRegistryStopped
	}
	if r.arbitrations[peerID] != nil {
		return nil, ErrPeerSuppressed
	}
	entry := r.peers[peerID]
	if entry != nil {
		if !entry.current.Empty() {
			return nil, ErrPeerCurrent
		}
		if entry.prepared != nil {
			return nil, ErrPeerPrepared
		}
	}
	if r.countRoleLocked(RolePeer) >= r.maxPeers {
		return nil, ErrPeerCapacity
	}
	if entry == nil {
		entry = &peerEntry{retiring: make(map[uint64]*Generation)}
		r.peers[peerID] = entry
	}
	generation := r.newGenerationLocked(RolePeer, DirectionOutbound, PhasePrepared, owner)
	generation.peerID = peerID
	entry.prepared = generation
	r.updateRoleHighWaterLocked(RolePeer)
	r.signalLocked()
	return generation, nil
}

func (r *Registry) PrepareClient(pending *Generation, instanceID, groupID string) (*Generation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrRegistryStopped
	}
	if !r.isExactLocked(pending, RoleUnknown, PhasePending) {
		return nil, ErrStaleGeneration
	}
	entry := r.clients[instanceID]
	if entry != nil {
		if entry.groupID != groupID {
			return nil, ErrClientGroupConflict
		}
		if !entry.current.Empty() {
			return nil, ErrClientCurrent
		}
		if entry.prepared != nil {
			return nil, ErrClientPrepared
		}
	}
	if r.countRoleLocked(RoleClient) >= r.maxClients {
		return nil, ErrClientCapacity
	}
	if entry == nil {
		entry = &clientEntry{groupID: groupID, retiring: make(map[uint64]*Generation)}
		r.clients[instanceID] = entry
	}
	r.removePendingLocked(pending)
	pending.role = RoleClient
	pending.phase = PhasePrepared
	pending.instanceID = instanceID
	pending.groupID = groupID
	entry.prepared = pending
	r.updateRoleHighWaterLocked(RoleClient)
	r.signalLocked()
	return pending, nil
}

// PreparePeer either atomically transfers pending into a peer prepared slot or
// returns an arbitration whose loser must be stopped and joined outside the lock.
func (r *Registry) PreparePeer(pending *Generation, peerID string, preferredInbound bool) (*Generation, *Arbitration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, ErrRegistryStopped
	}
	if !r.isExactLocked(pending, RoleUnknown, PhasePending) {
		return nil, nil, ErrStaleGeneration
	}
	if r.arbitrations[peerID] != nil {
		return nil, nil, ErrPeerPrepared
	}
	entry := r.peers[peerID]
	if entry != nil {
		if !entry.current.Empty() {
			return nil, nil, ErrPeerCurrent
		}
		if entry.prepared != nil {
			if preferredInbound && entry.prepared.direction == DirectionOutbound {
				r.nextArbID++
				arbitration := &Arbitration{
					registry: r,
					id:       r.nextArbID,
					peerID:   peerID,
					inbound:  pending,
					loser:    entry.prepared,
				}
				r.arbitrations[peerID] = arbitration
				r.signalLocked()
				return nil, arbitration, nil
			}
			return nil, nil, ErrPeerPrepared
		}
	}
	if r.countRoleLocked(RolePeer) >= r.maxPeers {
		return nil, nil, ErrPeerCapacity
	}
	return r.transferPendingToPeerLocked(pending, peerID), nil, nil
}

func (r *Registry) CompleteArbitration(arbitration *Arbitration) (*Generation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.isExactArbitrationLocked(arbitration) {
		return nil, ErrStaleGeneration
	}
	if r.closed {
		r.finishArbitrationLocked(arbitration)
		return nil, ErrRegistryStopped
	}
	if !r.isExactLocked(arbitration.inbound, RoleUnknown, PhasePending) ||
		!arbitration.loserReleased || !r.isExactLocked(arbitration.loser, RolePeer, PhaseRetiring) {
		r.finishArbitrationLocked(arbitration)
		return nil, ErrStaleGeneration
	}
	entry := r.peers[arbitration.peerID]
	if entry != nil && (entry.prepared != nil || !entry.current.Empty()) {
		r.finishArbitrationLocked(arbitration)
		return nil, ErrPeerCurrent
	}
	if r.countRoleLocked(RolePeer) > r.maxPeers {
		r.finishArbitrationLocked(arbitration)
		return nil, ErrPeerCapacity
	}
	r.finishArbitrationLocked(arbitration)
	return r.transferPendingToPeerLocked(arbitration.inbound, arbitration.peerID), nil
}

func (r *Registry) CancelArbitration(arbitration *Arbitration, abortInbound bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.isExactArbitrationLocked(arbitration) {
		return false
	}
	r.finishArbitrationLocked(arbitration)
	if abortInbound {
		r.removeLocked(arbitration.inbound)
	}
	return true
}

func (r *Registry) finishArbitrationLocked(arbitration *Arbitration) {
	delete(r.arbitrations, arbitration.peerID)
	if arbitration.loserReleased {
		r.removeLocked(arbitration.loser)
	}
	r.signalLocked()
}

func (r *Registry) PeerSuppressed(peerID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.arbitrations[peerID] != nil
}

// WaitPeerDialable blocks an active endpoint worker while another direction is
// current/prepared or an exact arbitration owns suppression.
func (r *Registry) WaitPeerDialable(ctx context.Context, peerID string) bool {
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return false
		}
		entry := r.peers[peerID]
		blocked := r.arbitrations[peerID] != nil || entry != nil && (entry.prepared != nil || !entry.current.Empty())
		changed := r.changed
		r.mu.Unlock()
		if !blocked {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		}
	}
}

// CommitReceiver is infallible for an exact prepared token. It returns whether
// shutdown raced the commit and the owner must immediately retire it.
func (r *Registry) CommitReceiver(generation *Generation) (committed, stop bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.isExactLocked(generation, generation.role, PhasePrepared) {
		return false, r.closed
	}
	if !r.commitLocked(generation) {
		return false, r.closed
	}
	return true, r.closed
}

// CommitOutboundPeer publishes only if no inbound/current winner appeared
// while the success ACK was in flight.
func (r *Registry) CommitOutboundPeer(generation *Generation) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.isExactLocked(generation, RolePeer, PhasePrepared) {
		return false
	}
	if arbitration := r.arbitrations[generation.peerID]; arbitration != nil && arbitration.loser == generation {
		return false
	}
	entry := r.peers[generation.peerID]
	if entry == nil || entry.prepared != generation || !entry.current.Empty() {
		return false
	}
	entry.prepared = nil
	if !entry.current.Publish(generation) {
		return false
	}
	generation.phase = PhaseCurrent
	r.signalLocked()
	return true
}

func (r *Registry) BeginRetire(generation *Generation) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation == nil || generation.registry != r || r.all[generation.id] != generation {
		return false
	}
	switch generation.phase {
	case PhasePrepared, PhaseCurrent:
		r.unlinkActiveLocked(generation)
		generation.phase = PhaseRetiring
		r.addRetiringLocked(generation)
		if generation.role == RoleClient {
			r.refreshGroupLocked(generation.groupID)
		}
		r.signalLocked()
		return true
	case PhaseRetiring:
		return true
	default:
		return false
	}
}

// Release removes only the exact token supplied. Repeated and stale callbacks are no-ops.
func (r *Registry) Release(generation *Generation) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != nil && generation.registry == r && r.all[generation.id] == generation {
		if arbitration := r.arbitrations[generation.peerID]; arbitration != nil && arbitration.loser == generation {
			if arbitration.loserReleased {
				return false
			}
			if generation.phase != PhaseRetiring {
				r.unlinkActiveLocked(generation)
				generation.phase = PhaseRetiring
				r.addRetiringLocked(generation)
			}
			// Cleanup is complete, but the loser still owns its counted slot until transfer.
			arbitration.loserReleased = true
			r.signalLocked()
			return true
		}
	}
	groupID := ""
	if generation != nil && generation.registry == r && generation.role == RoleClient {
		groupID = generation.groupID
	}
	removed := r.removeLocked(generation)
	if removed {
		if groupID != "" {
			r.refreshGroupLocked(groupID)
		}
		r.signalLocked()
	}
	return removed
}

func (r *Registry) IsCurrent(generation *Generation) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation == nil || generation.registry != r || generation.phase != PhaseCurrent {
		return false
	}
	switch generation.role {
	case RoleClient:
		entry := r.clients[generation.instanceID]
		return entry != nil && entry.current.Is(generation)
	case RolePeer:
		entry := r.peers[generation.peerID]
		return entry != nil && entry.current.Is(generation)
	default:
		return false
	}
}

func (r *Registry) HasPeerCurrent(peerID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.peers[peerID]
	return entry != nil && !entry.current.Empty()
}

func (r *Registry) Snapshot() RegistrySnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := RegistrySnapshot{
		Closed:           r.closed,
		PendingHighWater: r.pendingHigh,
		ClientHighWater:  r.clientHigh,
		ClientBindings:   len(r.clients),
		PeerHighWater:    r.peerHigh,
		Arbitrations:     len(r.arbitrations),
	}
	for _, generation := range r.all {
		switch generation.role {
		case RoleUnknown:
			if generation.phase == PhasePending {
				snapshot.Pending++
			}
		case RoleClient:
			snapshot.ClientTotal++
			switch generation.phase {
			case PhasePrepared:
				snapshot.ClientPrepared++
			case PhaseCurrent:
				snapshot.ClientCurrent++
			case PhaseRetiring:
				snapshot.ClientRetiring++
			}
		case RolePeer:
			snapshot.PeerTotal++
			switch generation.phase {
			case PhasePrepared:
				snapshot.PeerPrepared++
			case PhaseCurrent:
				snapshot.PeerCurrent++
			case PhaseRetiring:
				snapshot.PeerRetiring++
			}
		}
	}
	return snapshot
}

// Stop closes the publication gate, stops every exact owner, joins outside the
// registry lock, and finally releases any token not already cleaned by its owner.
func (r *Registry) Stop() {
	r.mu.Lock()
	if r.closed {
		stopDone := r.stopDone
		r.mu.Unlock()
		<-stopDone
		return
	}
	r.closed = true
	clear(r.groupAvailable)
	close(r.groupChanged)
	clear(r.arbitrations)
	r.signalLocked()
	generations := make([]*Generation, 0, len(r.all))
	for _, generation := range r.all {
		generations = append(generations, generation)
	}
	r.mu.Unlock()

	for _, generation := range generations {
		generation.StopOwner()
	}
	for _, generation := range generations {
		generation.WaitOwner()
	}

	r.mu.Lock()
	for _, generation := range generations {
		r.removeLocked(generation)
	}
	r.mu.Unlock()
	close(r.stopDone)
}

func (r *Registry) newGenerationLocked(role Role, direction Direction, phase Phase, owner Owner) *Generation {
	r.nextID++
	generation := &Generation{
		registry:  r,
		id:        r.nextID,
		role:      role,
		direction: direction,
		phase:     phase,
		owner:     owner,
	}
	r.all[generation.id] = generation
	return generation
}

func (r *Registry) transferPendingToPeerLocked(pending *Generation, peerID string) *Generation {
	entry := r.peers[peerID]
	if entry == nil {
		entry = &peerEntry{retiring: make(map[uint64]*Generation)}
		r.peers[peerID] = entry
	}
	r.removePendingLocked(pending)
	pending.role = RolePeer
	pending.phase = PhasePrepared
	pending.peerID = peerID
	entry.prepared = pending
	r.updateRoleHighWaterLocked(RolePeer)
	r.signalLocked()
	return pending
}

func (r *Registry) removePendingLocked(generation *Generation) {
	// The token remains in r.all while its role and phase are changed atomically.
	generation.phase = PhaseDone
}

func (r *Registry) commitLocked(generation *Generation) bool {
	switch generation.role {
	case RoleClient:
		entry := r.clients[generation.instanceID]
		if entry == nil || entry.prepared != generation || !entry.current.Empty() {
			return false
		}
		entry.prepared = nil
		if !entry.current.Publish(generation) {
			return false
		}
	case RolePeer:
		entry := r.peers[generation.peerID]
		if entry == nil || entry.prepared != generation || !entry.current.Empty() {
			return false
		}
		entry.prepared = nil
		if !entry.current.Publish(generation) {
			return false
		}
	default:
		return false
	}
	generation.phase = PhaseCurrent
	r.signalLocked()
	return true
}

func (r *Registry) unlinkActiveLocked(generation *Generation) {
	switch generation.role {
	case RoleClient:
		entry := r.clients[generation.instanceID]
		if entry != nil {
			if entry.prepared == generation {
				entry.prepared = nil
			}
			entry.current.Retire(generation)
		}
	case RolePeer:
		entry := r.peers[generation.peerID]
		if entry != nil {
			if entry.prepared == generation {
				entry.prepared = nil
			}
			entry.current.Retire(generation)
		}
	}
}

func (r *Registry) addRetiringLocked(generation *Generation) {
	switch generation.role {
	case RoleClient:
		if entry := r.clients[generation.instanceID]; entry != nil {
			entry.retiring[generation.id] = generation
		}
	case RolePeer:
		if entry := r.peers[generation.peerID]; entry != nil {
			entry.retiring[generation.id] = generation
		}
	}
}

func (r *Registry) removeLocked(generation *Generation) bool {
	if generation == nil || generation.registry != r || r.all[generation.id] != generation {
		return false
	}
	if arbitration := r.arbitrationForInboundLocked(generation); arbitration != nil {
		r.finishArbitrationLocked(arbitration)
	}
	r.unlinkActiveLocked(generation)
	switch generation.role {
	case RoleClient:
		entry := r.clients[generation.instanceID]
		if entry != nil {
			delete(entry.retiring, generation.id)
			if entry.prepared == nil && entry.current.Empty() && len(entry.retiring) == 0 {
				delete(r.clients, generation.instanceID)
			}
		}
	case RolePeer:
		entry := r.peers[generation.peerID]
		if entry != nil {
			delete(entry.retiring, generation.id)
			if entry.prepared == nil && entry.current.Empty() && len(entry.retiring) == 0 {
				delete(r.peers, generation.peerID)
			}
		}
	}
	delete(r.all, generation.id)
	generation.phase = PhaseDone
	return true
}

func (r *Registry) arbitrationForInboundLocked(generation *Generation) *Arbitration {
	for _, arbitration := range r.arbitrations {
		if arbitration.inbound == generation {
			return arbitration
		}
	}
	return nil
}

func (r *Registry) isExactLocked(generation *Generation, role Role, phase Phase) bool {
	return generation != nil && generation.registry == r && r.all[generation.id] == generation &&
		generation.role == role && generation.phase == phase
}

func (r *Registry) isExactArbitrationLocked(arbitration *Arbitration) bool {
	return arbitration != nil && arbitration.registry == r && r.arbitrations[arbitration.peerID] == arbitration
}

func (r *Registry) countRoleLocked(role Role) int {
	count := 0
	for _, generation := range r.all {
		if generation.role == role {
			count++
		}
	}
	return count
}

func (r *Registry) countPhaseLocked(role Role, phase Phase) int {
	count := 0
	for _, generation := range r.all {
		if generation.role == role && generation.phase == phase {
			count++
		}
	}
	return count
}

func (r *Registry) updateRoleHighWaterLocked(role Role) {
	count := r.countRoleLocked(role)
	switch role {
	case RoleClient:
		r.clientHigh = max(r.clientHigh, count)
	case RolePeer:
		r.peerHigh = max(r.peerHigh, count)
	}
}

func (r *Registry) signalLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}
