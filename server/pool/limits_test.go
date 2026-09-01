package pool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestDefaultLimitsAreReported(t *testing.T) {
	p := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer p.Stop()

	got := p.Snapshot()
	if got.PendingRegistrations != (LimitSnapshot{Limit: defaultMaxPendingRegistrations}) ||
		got.ClientGenerations != (LimitSnapshot{Limit: defaultMaxClientGenerations}) ||
		got.TCPConnectionsPerGeneration != (LimitSnapshot{Limit: defaultMaxTCPConnectionsPerGeneration}) ||
		got.PendingTCPSetupsPerGeneration != (LimitSnapshot{Limit: defaultMaxPendingTCPSetupsPerGeneration}) ||
		got.UDPSessionsPerGeneration != (LimitSnapshot{Limit: defaultMaxUDPSessionsPerGeneration}) {
		t.Fatalf("default limit snapshots = %+v", got)
	}
}

func TestPendingRegistrationLimitCapRecoveryAndSnapshot(t *testing.T) {
	limits := defaultLimits()
	limits.MaxPendingRegistrations = 2
	p := NewWithLimits("test", NewRoundRobinBalancer(), newTestLogger(), limits)
	defer p.Stop()

	first := p.BeginPending()
	second := p.BeginPending()
	if first == nil || second == nil || p.BeginPending() != nil {
		t.Fatal("BeginPending() did not enforce cap/cap+1")
	}
	if reservation, err := p.Reserve(&ClientConn{ID: "over-cap"}); reservation != nil || !errors.Is(err, ErrPendingRegistrationCapacity) {
		t.Fatalf("Reserve(over-cap) = (%v, %v), want ErrPendingRegistrationCapacity", reservation, err)
	}
	if got := p.Snapshot().PendingRegistrations; got != (LimitSnapshot{Current: 2, HighWater: 2, Limit: 2, CapacityDrops: 2}) {
		t.Fatalf("pending registrations = %+v", got)
	}

	if !p.Abort(first) {
		t.Fatal("Abort(first) = false")
	}
	recovered := p.BeginPending()
	if recovered == nil {
		t.Fatal("BeginPending() did not recover released capacity")
	}
	if !p.Abort(second) || !p.Abort(recovered) {
		t.Fatal("Abort() rejected held pending registration")
	}
	if got := p.Snapshot().PendingRegistrations; got.Current != 0 || got.HighWater != 2 {
		t.Fatalf("pending registrations after recovery = %+v", got)
	}
}

func TestPendingRegistrationLimitConcurrentNoOvershoot(t *testing.T) {
	const (
		limit   int64 = 8
		workers       = 64
	)
	limits := defaultLimits()
	limits.MaxPendingRegistrations = limit
	p := NewWithLimits("test", NewRoundRobinBalancer(), newTestLogger(), limits)
	defer p.Stop()

	start := make(chan struct{})
	reservations := make(chan *Reservation, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			if reservation := p.BeginPending(); reservation != nil {
				reservations <- reservation
			}
		})
	}
	close(start)
	wg.Wait()
	close(reservations)

	held := make([]*Reservation, 0, limit)
	for reservation := range reservations {
		held = append(held, reservation)
	}
	if len(held) != int(limit) {
		t.Fatalf("pending registrations = %d, want %d", len(held), limit)
	}
	if got := p.Snapshot().PendingRegistrations; got != (LimitSnapshot{Current: limit, HighWater: limit, Limit: limit, CapacityDrops: workers - uint64(limit)}) {
		t.Fatalf("pending registration snapshot = %+v", got)
	}
	for _, reservation := range held {
		if !p.Abort(reservation) {
			t.Fatal("Abort() rejected concurrent reservation")
		}
	}
}

func TestClientGenerationLimitCountsAllPhasesAndRecovers(t *testing.T) {
	limits := defaultLimits()
	limits.MaxClientGenerations = 2
	p := NewWithLimits("test", NewRoundRobinBalancer(), newTestLogger(), limits)
	defer p.Stop()

	registered := &ClientConn{ID: "registered"}
	if err := p.Add(registered); err != nil {
		t.Fatalf("Add(registered) error = %v", err)
	}
	reserved, err := p.Reserve(&ClientConn{ID: "reserved"})
	if err != nil {
		t.Fatalf("Reserve(reserved) error = %v", err)
	}
	if overCap, err := p.Reserve(&ClientConn{ID: "over-cap"}); overCap != nil || !errors.Is(err, ErrClientGenerationCapacity) {
		t.Fatalf("Reserve(over-cap) = (%v, %v), want ErrClientGenerationCapacity", overCap, err)
	}
	if err := p.Commit(reserved); err != nil {
		t.Fatalf("Commit(reserved) error = %v", err)
	}

	retirement := p.BeginRetire(registered)
	if retirement == nil {
		t.Fatal("BeginRetire(registered) = nil")
	}
	pending := p.BeginPending()
	if pending == nil {
		t.Fatal("BeginPending() = nil below registration cap")
	}
	if err := pending.Reserve(&ClientConn{ID: "retiring-over-cap"}); !errors.Is(err, ErrClientGenerationCapacity) {
		t.Fatalf("Reserve(retiring-over-cap) error = %v, want ErrClientGenerationCapacity", err)
	}
	if !p.Abort(pending) || !retirement.Done() {
		t.Fatal("capacity holders did not release")
	}
	if err := p.Add(&ClientConn{ID: "recovered"}); err != nil {
		t.Fatalf("Add(recovered) error = %v", err)
	}
	if got := p.Snapshot().ClientGenerations; got != (LimitSnapshot{Current: 2, HighWater: 2, Limit: 2, CapacityDrops: 2}) {
		t.Fatalf("client generations = %+v", got)
	}
}

func TestClientGenerationLimitConcurrentNoOvershoot(t *testing.T) {
	const (
		limit   int64 = 8
		workers       = 64
	)
	limits := defaultLimits()
	limits.MaxClientGenerations = limit
	limits.MaxPendingRegistrations = workers
	p := NewWithLimits("test", NewRoundRobinBalancer(), newTestLogger(), limits)
	defer p.Stop()

	start := make(chan struct{})
	reservations := make(chan *Reservation, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			<-start
			reservation, err := p.Reserve(&ClientConn{ID: fmt.Sprintf("client-%d", i)})
			if err != nil {
				errs <- err
				return
			}
			reservations <- reservation
		})
	}
	close(start)
	wg.Wait()
	close(reservations)
	close(errs)

	held := make([]*Reservation, 0, limit)
	for reservation := range reservations {
		held = append(held, reservation)
	}
	for err := range errs {
		if !errors.Is(err, ErrClientGenerationCapacity) {
			t.Errorf("Reserve() error = %v, want ErrClientGenerationCapacity", err)
		}
	}
	if len(held) != int(limit) {
		t.Fatalf("client generation reservations = %d, want %d", len(held), limit)
	}
	if got := p.Snapshot().ClientGenerations; got != (LimitSnapshot{Current: limit, HighWater: limit, Limit: limit, CapacityDrops: workers - uint64(limit)}) {
		t.Fatalf("client generation snapshot = %+v", got)
	}
	for _, reservation := range held {
		if !p.Abort(reservation) {
			t.Fatal("Abort() rejected concurrent generation reservation")
		}
	}
	if err := p.Add(&ClientConn{ID: "recovered"}); err != nil {
		t.Fatalf("Add(recovered) error = %v", err)
	}
}

func TestTCPGenerationLimitsReasonsPhasesAndRecovery(t *testing.T) {
	limits := defaultLimits()
	limits.MaxTCPConnectionsPerGeneration = 2
	limits.MaxPendingTCPSetupsPerGeneration = 2
	p := NewWithLimits("test", NewRoundRobinBalancer(), newTestLogger(), limits)
	defer p.Stop()
	client := addTCPAdmissionClient(t, p, "client", "tcp")

	active := reserveTCPLease(t, p)
	if !active.Commit() {
		t.Fatal("Commit(active) = false")
	}
	pending := reserveTCPLease(t, p)
	admission, err := p.BeginTCPAdmission()
	if err != nil {
		t.Fatalf("BeginTCPAdmission(over-cap) error = %v", err)
	}
	if lease, err := admission.Next(); lease != nil || !errors.Is(err, ErrTCPGenerationConnectionCapacity) || errors.Is(err, ErrTCPGenerationSetupCapacity) {
		t.Fatalf("Next(over-cap) = (%v, %v), want connection capacity", lease, err)
	}
	if got := p.Snapshot().TCPConnectionsPerGeneration; got != (LimitSnapshot{Current: 2, HighWater: 2, Limit: 2, CapacityDrops: 1}) {
		t.Fatalf("TCP connections = %+v", got)
	}
	if !pending.Release() {
		t.Fatal("Release(pending) = false")
	}
	recovered := reserveTCPLease(t, p)
	if !recovered.Release() || !active.Release() {
		t.Fatal("Release() rejected held TCP lease")
	}
	if client.tcpPending.Load() != 0 || client.tcpActive.Load() != 0 {
		t.Fatalf("TCP current after recovery = %d", client.tcpPending.Load()+client.tcpActive.Load())
	}

	setupLimits := defaultLimits()
	setupLimits.MaxTCPConnectionsPerGeneration = 3
	setupLimits.MaxPendingTCPSetupsPerGeneration = 1
	setupPool := NewWithLimits("setup", NewRoundRobinBalancer(), newTestLogger(), setupLimits)
	defer setupPool.Stop()
	addTCPAdmissionClient(t, setupPool, "client", "tcp")
	setup := reserveTCPLease(t, setupPool)
	setupAdmission, err := setupPool.BeginTCPAdmission()
	if err != nil {
		t.Fatalf("BeginTCPAdmission(setup-cap) error = %v", err)
	}
	if lease, err := setupAdmission.Next(); lease != nil || !errors.Is(err, ErrTCPGenerationSetupCapacity) || errors.Is(err, ErrTCPGenerationConnectionCapacity) {
		t.Fatalf("Next(setup-cap) = (%v, %v), want setup capacity", lease, err)
	}
	if got := setupPool.Snapshot().PendingTCPSetupsPerGeneration; got != (LimitSnapshot{Current: 1, HighWater: 1, Limit: 1, CapacityDrops: 1}) {
		t.Fatalf("pending TCP setups = %+v", got)
	}
	if !setup.Release() {
		t.Fatal("Release(setup) = false")
	}
}

func TestTCPPendingSetupDefaultLimitTwoXAndRecovery(t *testing.T) {
	p := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer p.Stop()
	client := addTCPAdmissionClient(t, p, "client", "tcp")

	held := make([]*TCPLease, 0, defaultMaxPendingTCPSetupsPerGeneration)
	for i := range 2 * defaultMaxPendingTCPSetupsPerGeneration {
		admission, err := p.BeginTCPAdmission()
		if err != nil {
			t.Fatalf("BeginTCPAdmission(%d) error = %v", i, err)
		}
		lease, err := admission.Next()
		if i < defaultMaxPendingTCPSetupsPerGeneration {
			if err != nil || lease == nil {
				t.Fatalf("Next(%d) = (%v, %v), want lease", i, lease, err)
			}
			held = append(held, lease)
			continue
		}
		if lease != nil || !errors.Is(err, ErrTCPGenerationSetupCapacity) || errors.Is(err, ErrTCPGenerationConnectionCapacity) {
			t.Fatalf("Next(%d) = (%v, %v), want setup capacity", i, lease, err)
		}
	}
	wantSaturated := LimitSnapshot{
		Current:       defaultMaxPendingTCPSetupsPerGeneration,
		HighWater:     defaultMaxPendingTCPSetupsPerGeneration,
		Limit:         defaultMaxPendingTCPSetupsPerGeneration,
		CapacityDrops: uint64(defaultMaxPendingTCPSetupsPerGeneration),
	}
	if snapshot := p.Snapshot(); snapshot.TCPPending != defaultMaxPendingTCPSetupsPerGeneration ||
		snapshot.PendingTCPSetupsPerGeneration != wantSaturated {
		t.Fatalf("saturated pending setup snapshot = %+v", snapshot)
	}

	if !held[0].Release() {
		t.Fatal("Release(first) = false")
	}
	recovered := reserveTCPLease(t, p)
	if snapshot := p.Snapshot(); snapshot.TCPPending != defaultMaxPendingTCPSetupsPerGeneration ||
		snapshot.PendingTCPSetupsPerGeneration != wantSaturated {
		t.Fatalf("recovered pending setup snapshot = %+v", snapshot)
	}
	if !recovered.Release() {
		t.Fatal("Release(recovered) = false")
	}
	for _, lease := range held[1:] {
		if !lease.Release() {
			t.Fatal("Release(held) = false")
		}
	}
	wantFinal := wantSaturated
	wantFinal.Current = 0
	if snapshot := p.Snapshot(); snapshot.TCPPending != 0 || snapshot.TCPActive != 0 ||
		snapshot.PendingTCPSetupsPerGeneration != wantFinal || client.tcpPending.Load() != 0 || client.tcpActive.Load() != 0 {
		t.Fatalf("final pending setup snapshot = %+v", snapshot)
	}
}

func TestTCPConnectionLimitConcurrentNoOvershoot(t *testing.T) {
	const (
		limit   int64 = 8
		workers       = 64
	)
	limits := defaultLimits()
	limits.MaxTCPConnectionsPerGeneration = limit
	limits.MaxPendingTCPSetupsPerGeneration = workers
	p := NewWithLimits("test", NewRoundRobinBalancer(), newTestLogger(), limits)
	defer p.Stop()
	addTCPAdmissionClient(t, p, "client", "tcp")

	start := make(chan struct{})
	leases := make(chan *TCPLease, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			admission, err := p.BeginTCPAdmission()
			if err == nil {
				var lease *TCPLease
				lease, err = admission.Next()
				if lease != nil {
					leases <- lease
					return
				}
			}
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(leases)
	close(errs)

	held := make([]*TCPLease, 0, limit)
	for lease := range leases {
		held = append(held, lease)
	}
	for err := range errs {
		if !errors.Is(err, ErrTCPGenerationConnectionCapacity) {
			t.Errorf("Next() error = %v, want ErrTCPGenerationConnectionCapacity", err)
		}
	}
	if len(held) != int(limit) {
		t.Fatalf("TCP leases = %d, want %d", len(held), limit)
	}
	if got := p.Snapshot().TCPConnectionsPerGeneration; got != (LimitSnapshot{Current: limit, HighWater: limit, Limit: limit, CapacityDrops: workers - uint64(limit)}) {
		t.Fatalf("TCP connection snapshot = %+v", got)
	}
	for _, lease := range held {
		if !lease.Release() {
			t.Fatal("Release() rejected concurrent TCP lease")
		}
	}
}

func reserveTCPLease(t *testing.T, p *ConnectionPool) *TCPLease {
	t.Helper()
	admission, err := p.BeginTCPAdmission()
	if err != nil {
		t.Fatalf("BeginTCPAdmission() error = %v", err)
	}
	lease, err := admission.Next()
	if err != nil || lease == nil {
		t.Fatalf("Next() = (%v, %v), want lease", lease, err)
	}
	return lease
}
