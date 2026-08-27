package pool

import (
	"sync"
	"testing"
)

type recordingBalancer struct {
	selected *ClientConn
	calls    int
}

func (b *recordingBalancer) Select(_ []*ClientConn) (*ClientConn, error) {
	b.calls++
	return b.selected, nil
}

func (*recordingBalancer) Name() string { return "recording" }

func addTCPAdmissionClient(t *testing.T, p *ConnectionPool, id string, capabilities ...string) *ClientConn {
	t.Helper()
	client := &ClientConn{ID: id, Metadata: ClientMetadata{Capabilities: capabilities}}
	if err := p.Add(client); err != nil {
		t.Fatalf("Add(%s) error = %v", id, err)
	}
	return client
}

func TestTCPAdmissionSnapshotAndRevalidation(t *testing.T) {
	balancer := &recordingBalancer{}
	p := New("test", balancer, newTestLogger())
	defer p.Stop()

	selected := addTCPAdmissionClient(t, p, "selected", "tcp")
	stale := addTCPAdmissionClient(t, p, "stale", "tcp")
	_ = addTCPAdmissionClient(t, p, "udp-only", "udp")
	balancer.selected = selected

	admission, err := p.BeginTCPAdmission()
	if err != nil {
		t.Fatalf("BeginTCPAdmission() error = %v", err)
	}
	if balancer.calls != 0 {
		t.Fatalf("balancer calls before reservation = %d, want 0", balancer.calls)
	}

	late := addTCPAdmissionClient(t, p, "late", "tcp")
	if !p.Remove(stale) {
		t.Fatal("Remove(stale) = false")
	}
	replacement := addTCPAdmissionClient(t, p, stale.ID, "tcp")

	first, err := admission.Next()
	if err != nil || first == nil || first.Client() != selected {
		t.Fatalf("first Next() = (%v, %v), want selected %p", first, err, selected)
	}
	if second, err := admission.Next(); err != nil || second != nil {
		t.Fatalf("second Next() = (%p, %v), want exhausted snapshot", second, err)
	}
	if balancer.calls != 1 {
		t.Fatalf("balancer calls after cursor walk = %d, want 1", balancer.calls)
	}
	if first.Client() == late || first.Client() == replacement {
		t.Fatal("snapshot admitted a later generation")
	}
	if !first.Release() || first.Release() {
		t.Fatal("pending lease release was not idempotent")
	}
	if selected.tcpPending.Load() != 0 || replacement.tcpPending.Load() != 0 {
		t.Fatalf("pending counts after release: selected=%d replacement=%d, want 0/0", selected.tcpPending.Load(), replacement.tcpPending.Load())
	}
}

func TestTCPAdmissionPendingBoundConcurrentAndStaleLease(t *testing.T) {
	p := New("test", NewLeastConnectionsBalancer(), newTestLogger())
	defer p.Stop()
	stale := addTCPAdmissionClient(t, p, "client", "tcp")

	const workers = 500
	start := make(chan struct{})
	leases := make(chan *TCPLease, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			admission, err := p.BeginTCPAdmission()
			if err != nil {
				t.Errorf("BeginTCPAdmission() error = %v", err)
				return
			}
			lease, err := admission.Next()
			if err != nil {
				t.Errorf("Next() error = %v", err)
				return
			}
			if lease != nil {
				leases <- lease
			}
		})
	}
	close(start)
	wg.Wait()
	close(leases)

	held := make([]*TCPLease, 0, maxPendingTCPSetupsPerClient)
	for lease := range leases {
		held = append(held, lease)
	}
	if len(held) != maxPendingTCPSetupsPerClient {
		t.Fatalf("reserved leases = %d, want %d", len(held), maxPendingTCPSetupsPerClient)
	}
	if pending := stale.tcpPending.Load(); pending != maxPendingTCPSetupsPerClient {
		t.Fatalf("pending setups = %d, want %d", pending, maxPendingTCPSetupsPerClient)
	}

	if !p.Remove(stale) {
		t.Fatal("Remove(stale) = false")
	}
	current := addTCPAdmissionClient(t, p, stale.ID, "tcp")
	if !held[0].Commit() || held[0].Commit() {
		t.Fatal("stale lease commit was not exact and idempotent")
	}
	if stale.ActiveConns.Load() != 1 || stale.TotalConns.Load() != 1 || current.ActiveConns.Load() != 0 {
		t.Fatalf("stale commit counters: stale active/total=%d/%d current active=%d", stale.ActiveConns.Load(), stale.TotalConns.Load(), current.ActiveConns.Load())
	}
	for _, lease := range held {
		if !lease.Release() || lease.Release() {
			t.Fatal("lease release was not exact and idempotent")
		}
	}
	if stale.tcpPending.Load() != 0 || stale.ActiveConns.Load() != 0 || current.tcpPending.Load() != 0 || current.ActiveConns.Load() != 0 {
		t.Fatalf("final stale pending/active=%d/%d current pending/active=%d/%d, want all zero", stale.tcpPending.Load(), stale.ActiveConns.Load(), current.tcpPending.Load(), current.ActiveConns.Load())
	}
}

func TestTCPAdmissionLeastConnectionsSelectionAndReservationAreAtomic(t *testing.T) {
	p := New("test", NewLeastConnectionsBalancer(), newTestLogger())
	defer p.Stop()
	first := addTCPAdmissionClient(t, p, "first", "tcp")
	second := addTCPAdmissionClient(t, p, "second", "tcp")

	firstAdmission, err := p.BeginTCPAdmission()
	if err != nil {
		t.Fatalf("BeginTCPAdmission(first) error = %v", err)
	}
	secondAdmission, err := p.BeginTCPAdmission()
	if err != nil {
		t.Fatalf("BeginTCPAdmission(second) error = %v", err)
	}
	firstLease, err := firstAdmission.Next()
	if err != nil {
		t.Fatalf("Next(first) error = %v", err)
	}
	secondLease, err := secondAdmission.Next()
	if err != nil {
		t.Fatalf("Next(second) error = %v", err)
	}
	if firstLease == nil || secondLease == nil {
		t.Fatalf("leases = (%v, %v), want two reservations", firstLease, secondLease)
	}
	if firstLease.Client() == secondLease.Client() {
		t.Fatalf("both admissions selected %s, want distinct LC clients", firstLease.Client().ID)
	}
	if first.tcpPending.Load() != 1 || second.tcpPending.Load() != 1 {
		t.Fatalf("pending counts = %d/%d, want 1/1", first.tcpPending.Load(), second.tcpPending.Load())
	}
	if !firstLease.Release() || !secondLease.Release() {
		t.Fatal("Release() = false for held lease")
	}
}

func TestTCPAdmissionRevalidatesFallbackCapacity(t *testing.T) {
	balancer := &recordingBalancer{}
	p := New("test", balancer, newTestLogger())
	defer p.Stop()
	first := addTCPAdmissionClient(t, p, "first", "tcp")
	second := addTCPAdmissionClient(t, p, "second", "tcp")
	second.tcpPending.Store(maxPendingTCPSetupsPerClient)
	balancer.selected = first

	admission, err := p.BeginTCPAdmission()
	if err != nil {
		t.Fatalf("BeginTCPAdmission() error = %v", err)
	}
	firstLease, err := admission.Next()
	if err != nil || firstLease == nil || firstLease.Client() != first {
		t.Fatalf("first Next() = (%v, %v), want first client", firstLease, err)
	}
	if !firstLease.Release() {
		t.Fatal("Release(first) = false")
	}
	second.tcpPending.Store(0)

	secondLease, err := admission.Next()
	if err != nil || secondLease == nil || secondLease.Client() != second {
		t.Fatalf("second Next() = (%v, %v), want newly available fallback", secondLease, err)
	}
	if balancer.calls != 1 {
		t.Fatalf("balancer calls = %d, want 1", balancer.calls)
	}
	if !secondLease.Release() {
		t.Fatal("Release(second) = false")
	}
}

func TestLeastConnectionsIncludesPendingWithoutCommitUndercount(t *testing.T) {
	balancer := NewLeastConnectionsBalancer()
	busy := &ClientConn{ID: "busy"}
	idle := &ClientConn{ID: "idle"}
	busy.healthy.Store(true)
	idle.healthy.Store(true)
	busy.tcpPending.Store(1)

	selected, err := balancer.Select([]*ClientConn{busy, idle})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selected != idle {
		t.Fatalf("Select() = %s, want idle client", selected.ID)
	}

	// Match TCPLease.Commit's publication order. The read order in the
	// balancer must see either one pending, one active, or a brief score of two.
	busy.ActiveConns.Add(1)
	if score := busy.tcpPending.Load() + busy.ActiveConns.Load(); score < 1 {
		t.Fatalf("score during commit = %d, want at least 1", score)
	}
	busy.tcpPending.Add(-1)
	if score := busy.tcpPending.Load() + busy.ActiveConns.Load(); score != 1 {
		t.Fatalf("score after commit = %d, want 1", score)
	}
}
