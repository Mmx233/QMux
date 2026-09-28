package mesh

import (
	"context"
	"errors"
	"slices"

	"github.com/Mmx233/QMux/config"
	"github.com/quic-go/quic-go"
)

var (
	ErrNoMeshCandidate              = errors.New("no eligible mesh next hop")
	ErrGenerationConnectionCapacity = errors.New("mesh generation connection capacity reached")
	ErrGenerationSetupCapacity      = errors.New("mesh generation setup capacity reached")
	ErrLocalTransportBudget         = errors.New("mesh local QUIC stream budget reached")
)

// ForwardingEligibility separates facts published by future declaration,
// version, and session owners; current alone grants no traffic.
type ForwardingEligibility struct {
	SessionReady     bool
	DeclarationReady bool
	L4Healthy        bool
}

type InstanceL7Health uint8

const (
	InstanceL7Unknown InstanceL7Health = iota
	InstanceL7Healthy
	InstanceL7Failed
)

type generationTraffic struct {
	pending   int
	active    int
	high      int
	setupHigh int
}

type generationTrafficSnapshot struct {
	Pending, Active, High, SetupHigh, Inflight int
}

func (r *Registry) trafficSnapshot(g *Generation, source *tcpSource) generationTrafficSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g == nil || g.registry != r {
		return generationTrafficSnapshot{}
	}
	usage := g.traffic[source]
	if usage == nil {
		return generationTrafficSnapshot{Inflight: g.inflight}
	}
	return generationTrafficSnapshot{usage.pending, usage.active, usage.high, usage.setupHigh, g.inflight}
}

func (r *Registry) GroupAvailability(groupID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.groupAvailable[groupID]
}

// GroupChanges is closed on a 0->1/last-qualified-instance edge or Stop.
// Subscribe before reading availability to avoid missing an edge.
func (r *Registry) GroupChanges() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.groupChanged
}

func (r *Registry) PublishForwarding(g *Generation, eligibility ForwardingEligibility) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.isCurrentLocked(g) {
		return false
	}
	if g.forwarding == eligibility {
		return true
	}
	g.forwarding = eligibility
	if g.role == RoleClient {
		r.refreshGroupLocked(g.groupID)
	}
	r.signalLocked()
	return true
}

func (r *Registry) publishL4Healthy(g *Generation) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.isCurrentLocked(g) || g.role != RoleClient {
		return false
	}
	if !g.forwarding.L4Healthy {
		g.forwarding.L4Healthy = true
		r.refreshGroupLocked(g.groupID)
		r.signalLocked()
	}
	return true
}

func (r *Registry) PublishInstanceL7Health(g *Generation, health InstanceL7Health) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.isCurrentLocked(g) || g.role != RoleClient || health > InstanceL7Failed {
		return false
	}
	g.l7Health = health
	return true
}

func (r *Registry) isCurrentLocked(g *Generation) bool {
	if r.closed || g == nil || g.registry != r || r.all[g.id] != g || g.phase != PhaseCurrent {
		return false
	}
	switch g.role {
	case RoleClient:
		entry := r.clients[g.instanceID]
		return entry != nil && entry.current.Is(g)
	case RolePeer:
		entry := r.peers[g.peerID]
		return entry != nil && entry.current.Is(g)
	default:
		return false
	}
}

func (r *Registry) isForwardingReadyLocked(g *Generation) bool {
	if !r.isCurrentLocked(g) {
		return false
	}
	e := g.forwarding
	if !e.SessionReady || !e.DeclarationReady || !e.L4Healthy {
		return false
	}
	if g.role != RoleClient {
		return true
	}
	rule, ok := r.groupRules[g.groupID]
	return ok && g.ruleVersion != 0 &&
		(g.ruleVersion >= rule.version || rule.policy == config.MeshOutdatedClientPolicyApplyLatestRules)
}

func (r *Registry) refreshGroupLocked(groupID string) {
	available := false
	retained := false
	for _, entry := range r.clients {
		if entry.groupID == groupID {
			retained = true
			if !entry.current.Empty() && r.isForwardingReadyLocked(entry.current.Load()) {
				available = true
			}
		}
	}
	if r.groupAvailable[groupID] != available {
		if available {
			r.groupAvailable[groupID] = true
		} else {
			delete(r.groupAvailable, groupID)
		}
		close(r.groupChanged)
		r.groupChanged = make(chan struct{})
	}
	if !retained {
		delete(r.roundRobin, groupID)
	}
}

type generationLeaseState uint8

const (
	generationLeasePending generationLeaseState = iota
	generationLeaseActive
	generationLeaseReleased
)

// One lease represents one actual next-hop business stream, not a public
// socket or an entire HTTP keep-alive connection.
type generationLease struct {
	registry   *Registry
	generation *Generation
	source     *tcpSource
	setup      *setupLease
	probe      bool
	terminal   bool
	failOpen   bool
	state      generationLeaseState // guarded by registry.mu
}

func (l *generationLease) Generation() *Generation {
	if l == nil {
		return nil
	}
	return l.generation
}

func (l *generationLease) FailOpen() bool { return l != nil && l.failOpen }

// acknowledge releases the source setup slot at target-ready ACK while the
// generation and any HTTP request lease remain held until the flow completes.
func (l *generationLease) acknowledge() bool {
	if l == nil || l.registry == nil || l.probe || l.setup == nil {
		return false
	}
	r := l.registry
	r.mu.Lock()
	if l.state != generationLeasePending || !l.setup.claim() {
		r.mu.Unlock()
		return false
	}
	l.activateLocked()
	r.mu.Unlock()
	l.setup.settle()
	return true
}

func (l *generationLease) acknowledgeProbe() bool {
	if l == nil || l.registry == nil || !l.probe {
		return false
	}
	r := l.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.state != generationLeasePending {
		return false
	}
	l.activateLocked()
	return true
}

// activateLocked requires registry.mu and a pending lease.
func (l *generationLease) activateLocked() {
	usage := l.generation.traffic[l.source]
	usage.pending--
	usage.active++
	l.state = generationLeaseActive
}

func (l *generationLease) release() bool {
	if l == nil || l.registry == nil {
		return false
	}
	r := l.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.state == generationLeaseReleased {
		return false
	}
	usage := l.generation.traffic[l.source]
	if l.state == generationLeasePending {
		usage.pending--
	} else {
		usage.active--
	}
	if l.terminal {
		l.generation.inflight--
	}
	l.state = generationLeaseReleased
	return true
}

func (r *Registry) reserveLocked(g *Generation, source *tcpSource, setup *setupLease, terminal, probe, failOpen bool) (*generationLease, error) {
	if source == nil || source.closed.Load() {
		return nil, ErrSourceClosed
	}
	if !probe && (setup == nil || setup.source != source || !setup.live()) {
		return nil, ErrSourceSetupReleased
	}
	if !r.isForwardingReadyLocked(g) {
		return nil, ErrNoMeshCandidate
	}
	usage := g.traffic[source]
	if usage == nil {
		usage = &generationTraffic{}
		if g.traffic == nil {
			g.traffic = make(map[*tcpSource]*generationTraffic)
		}
		g.traffic[source] = usage
	}
	if usage.pending+usage.active >= source.capacity.MaxTCPConnectionsPerGeneration {
		return nil, ErrGenerationConnectionCapacity
	}
	if usage.pending >= source.capacity.MaxPendingTCPSetupsPerGeneration {
		return nil, ErrGenerationSetupCapacity
	}
	usage.pending++
	usage.setupHigh = max(usage.setupHigh, usage.pending)
	usage.high = max(usage.high, usage.pending+usage.active)
	if terminal {
		g.inflight++
	}
	return &generationLease{registry: r, generation: g, source: source, setup: setup, terminal: terminal, probe: probe, failOpen: failOpen}, nil
}

func (r *Registry) reserveNextHop(g *Generation, setup *setupLease) (*generationLease, error) {
	if !setup.live() {
		return nil, ErrSourceSetupReleased
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if g == nil || g.registry != r || g.role != RolePeer {
		return nil, ErrNoMeshCandidate
	}
	return r.reserveLocked(g, setup.source, setup, false, false, false)
}

// A specified exact probe uses only next-hop generation capacity, not a
// public/tunnel source flow or setup slot or terminal instance request load.
func (r *Registry) reserveProbe(g *Generation, tunnelSource *tcpSource) (*generationLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reserveLocked(g, tunnelSource, nil, false, true, false)
}

type clientSelection struct {
	registry   *Registry
	source     *tcpSource
	setup      *setupLease
	groupID    string
	policy     string
	candidates []*Generation
	tried      map[*Generation]bool
	start      int
}

func (r *Registry) beginClientSelection(groupID string, setup *setupLease, policy string) *clientSelection {
	selection := &clientSelection{registry: r, groupID: groupID, policy: policy, tried: make(map[*Generation]bool), setup: setup}
	if setup == nil {
		return selection
	}
	selection.source = setup.source
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range r.clients {
		if entry.groupID == groupID && !entry.current.Empty() {
			selection.candidates = append(selection.candidates, entry.current.Load())
		}
	}
	slices.SortFunc(selection.candidates, func(a, b *Generation) int {
		if a.instanceID < b.instanceID {
			return -1
		}
		if a.instanceID > b.instanceID {
			return 1
		}
		return 0
	})
	if len(selection.candidates) > 0 && policy == "round-robin" {
		selectable := make([]int, 0, len(selection.candidates))
		for i, g := range selection.candidates {
			if r.isForwardingReadyLocked(g) && g.l7Health != InstanceL7Failed {
				selectable = append(selectable, i)
			}
		}
		if len(selectable) == 0 {
			for i, g := range selection.candidates {
				if r.isForwardingReadyLocked(g) {
					selectable = append(selectable, i)
				}
			}
		}
		if len(selectable) > 0 {
			selection.start = selectable[r.roundRobin[groupID]%uint64(len(selectable))]
			r.roundRobin[groupID]++
		}
	}
	return selection
}

func (s *clientSelection) next() (*generationLease, error) {
	r := s.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrRegistryStopped
	}
	if s.source == nil || !s.setup.live() {
		return nil, ErrSourceSetupReleased
	}
	if s.source.closed.Load() {
		return nil, ErrSourceClosed
	}
	failedOnly := true
	for _, g := range s.candidates {
		if r.isForwardingReadyLocked(g) && g.l7Health != InstanceL7Failed {
			failedOnly = false
			break
		}
	}
	connectionFull, setupFull := false, false
	for len(s.tried) < len(s.candidates) {
		var selected *Generation
		for i := range s.candidates {
			g := s.candidates[(s.start+i)%len(s.candidates)]
			if s.tried[g] || !r.isForwardingReadyLocked(g) || !failedOnly && g.l7Health == InstanceL7Failed {
				continue
			}
			if selected == nil || s.policy != "round-robin" && g.inflight < selected.inflight {
				selected = g
			}
			if s.policy == "round-robin" {
				break
			}
		}
		if selected == nil {
			break
		}
		s.tried[selected] = true
		lease, err := r.reserveLocked(selected, s.source, s.setup, true, false, failedOnly)
		if err == nil {
			return lease, nil
		}
		if errors.Is(err, ErrSourceClosed) || errors.Is(err, ErrSourceSetupReleased) {
			return nil, err
		}
		connectionFull = connectionFull || errors.Is(err, ErrGenerationConnectionCapacity)
		setupFull = setupFull || errors.Is(err, ErrGenerationSetupCapacity)
	}
	if connectionFull {
		return nil, ErrGenerationConnectionCapacity
	}
	if setupFull {
		return nil, ErrGenerationSetupCapacity
	}
	return nil, ErrNoMeshCandidate
}

// open retries only the fixed candidate set before business bytes are sent.
// The caller supplies the original deadline and owns a successful stream;
// cleanup closes a stream if the attempt is discarded.
func (s *clientSelection) open(ctx context.Context, openStream func(*Generation) (cleanup func(), err error)) (*generationLease, error) {
	transportLimited := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lease, err := s.next()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if transportLimited && errors.Is(err, ErrNoMeshCandidate) {
				return nil, ErrLocalTransportBudget
			}
			return nil, err
		}
		cleanup, err := openStream(lease.generation)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if cleanup != nil {
				cleanup()
			}
			lease.release()
			return nil, ctxErr
		}
		if err == nil {
			return lease, nil
		}
		if cleanup != nil {
			cleanup()
		}
		lease.release()
		if _, ok := errors.AsType[*quic.StreamLimitReachedError](err); !ok {
			return nil, err
		}
		transportLimited = true
	}
}
