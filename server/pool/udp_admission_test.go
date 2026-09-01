package pool

import (
	"errors"
	"sync"
	"testing"
)

func addUDPAdmissionClient(t *testing.T, p *ConnectionPool, id string) *ClientConn {
	t.Helper()
	client := &ClientConn{ID: id, Metadata: ClientMetadata{Capabilities: []string{"udp"}}}
	if err := p.Add(client); err != nil {
		t.Fatalf("Add(%s) error = %v", id, err)
	}
	return client
}

func TestUDPAdmissionCapAndReleaseUnderflow(t *testing.T) {
	limits := defaultLimits()
	limits.MaxUDPSessionsPerGeneration = 2
	p := NewWithLimits("test", NewRoundRobinBalancer(), newTestLogger(), limits)
	defer p.Stop()
	client := addUDPAdmissionClient(t, p, "client")

	for i := range 2 {
		selected, err := p.ReserveUDP()
		if err != nil || selected != client {
			t.Fatalf("ReserveUDP(%d) = (%p, %v), want client %p", i, selected, err, client)
		}
	}
	if _, err := p.ReserveUDP(); !errors.Is(err, ErrUDPGenerationCapacity) {
		t.Fatalf("ReserveUDP(cap+1) error = %v, want ErrUDPGenerationCapacity", err)
	}
	if got := p.Snapshot().UDPSessionsPerGeneration; got != (LimitSnapshot{Current: 2, HighWater: 2, Limit: 2, CapacityDrops: 1}) {
		t.Fatalf("UDP session snapshot = %+v", got)
	}
	if got := client.udpSessions.Load(); got != 2 {
		t.Fatalf("held UDP sessions = %d, want 2", got)
	}
	if !p.ReleaseUDP(client) || !p.ReleaseUDP(client) {
		t.Fatal("ReleaseUDP() rejected a held reservation")
	}
	if p.ReleaseUDP(client) {
		t.Fatal("ReleaseUDP() accepted an underflow")
	}
	if got := client.udpSessions.Load(); got != 0 {
		t.Fatalf("UDP sessions after underflow compensation = %d, want 0", got)
	}
	if got := p.Snapshot().UDPSessionsPerGeneration; got.Current != 0 || got.HighWater != 2 || got.CapacityDrops != 1 {
		t.Fatalf("UDP session snapshot after release = %+v", got)
	}
}

func TestUDPAdmissionLimitIsImmutable(t *testing.T) {
	limits := defaultLimits()
	limits.MaxUDPSessionsPerGeneration = 1
	p := NewWithLimits("test", NewRoundRobinBalancer(), newTestLogger(), limits)
	defer p.Stop()
	limits.MaxUDPSessionsPerGeneration = 2
	client := addUDPAdmissionClient(t, p, "client")

	if selected, err := p.ReserveUDP(); err != nil || selected != client {
		t.Fatalf("ReserveUDP(cap) = (%p, %v), want client %p", selected, err, client)
	}
	if _, err := p.ReserveUDP(); !errors.Is(err, ErrUDPGenerationCapacity) {
		t.Fatalf("ReserveUDP(cap+1) error = %v, want ErrUDPGenerationCapacity", err)
	}
}

func TestUDPAdmissionSelectsAvailableAlternative(t *testing.T) {
	for _, test := range []struct {
		name string
		new  func() LoadBalancer
	}{
		{name: "round-robin", new: func() LoadBalancer { return NewRoundRobinBalancer() }},
		{name: "least-connections", new: func() LoadBalancer { return NewLeastConnectionsBalancer() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := defaultLimits()
			limits.MaxUDPSessionsPerGeneration = 1
			p := NewWithLimits("test", test.new(), newTestLogger(), limits)
			defer p.Stop()
			first := addUDPAdmissionClient(t, p, "first")
			second := addUDPAdmissionClient(t, p, "second")

			selected, err := p.ReserveUDP()
			if err != nil {
				t.Fatalf("ReserveUDP(first) error = %v", err)
			}
			alternative, err := p.ReserveUDP()
			if err != nil {
				t.Fatalf("ReserveUDP(alternative) error = %v", err)
			}
			if alternative == selected {
				t.Fatalf("alternative = %s, want the other under-cap generation", alternative.ID)
			}
			if selected != first && selected != second || alternative != first && alternative != second {
				t.Fatal("ReserveUDP selected a generation outside the pool")
			}
			if !p.ReleaseUDP(selected) || !p.ReleaseUDP(alternative) {
				t.Fatal("ReleaseUDP() rejected a selected generation")
			}
		})
	}
}

func TestUDPAdmissionConcurrentNoOvershoot(t *testing.T) {
	const limit int64 = 8
	limits := defaultLimits()
	limits.MaxUDPSessionsPerGeneration = limit
	p := NewWithLimits("test", NewLeastConnectionsBalancer(), newTestLogger(), limits)
	defer p.Stop()
	client := addUDPAdmissionClient(t, p, "client")

	const workers = 128
	start := make(chan struct{})
	reserved := make(chan *ClientConn, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			selected, err := p.ReserveUDP()
			if err == nil {
				reserved <- selected
				return
			}
			if !errors.Is(err, ErrUDPGenerationCapacity) {
				errs <- err
			}
		})
	}
	close(start)
	wg.Wait()
	close(reserved)
	close(errs)
	for err := range errs {
		t.Errorf("ReserveUDP() unexpected error = %v", err)
	}

	held := make([]*ClientConn, 0, limit)
	for selected := range reserved {
		held = append(held, selected)
	}
	if got := len(held); got != int(limit) {
		t.Fatalf("successful reservations = %d, want %d", got, limit)
	}
	if got := client.udpSessions.Load(); got != limit {
		t.Fatalf("generation high water = %d, want %d", got, limit)
	}
	for _, selected := range held {
		if !p.ReleaseUDP(selected) {
			t.Fatal("ReleaseUDP() rejected a held concurrent reservation")
		}
	}
	if got := client.udpSessions.Load(); got != 0 {
		t.Fatalf("UDP sessions after release = %d, want 0", got)
	}
}

func TestUDPAdmissionStaleExactGenerationAndEligibility(t *testing.T) {
	p := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer p.Stop()
	stale := addUDPAdmissionClient(t, p, "same-id")

	selected, err := p.ReserveUDP()
	if err != nil || selected != stale {
		t.Fatalf("ReserveUDP() = (%p, %v), want stale %p", selected, err, stale)
	}
	if !p.IsCurrentEligible(stale, "udp") {
		t.Fatal("current UDP generation was not eligible")
	}
	if !p.Remove(stale) {
		t.Fatal("Remove(stale) = false")
	}
	fresh := addUDPAdmissionClient(t, p, stale.ID)
	if p.IsCurrentEligible(stale, "udp") || !p.IsCurrentEligible(fresh, "udp") {
		t.Fatal("exact current eligibility did not distinguish replacement generation")
	}
	if !p.ReleaseUDP(stale) {
		t.Fatal("ReleaseUDP(stale) rejected its exact reservation")
	}
	if stale.udpSessions.Load() != 0 || fresh.udpSessions.Load() != 0 {
		t.Fatalf("stale/fresh UDP sessions = %d/%d, want 0/0", stale.udpSessions.Load(), fresh.udpSessions.Load())
	}
}

func TestUDPAdmissionRejectsBalancerResultOutsideCandidates(t *testing.T) {
	balancer := &recordingBalancer{selected: &ClientConn{ID: "outsider"}}
	p := New("test", balancer, newTestLogger())
	defer p.Stop()
	client := addUDPAdmissionClient(t, p, "client")

	if _, err := p.ReserveUDP(); err == nil {
		t.Fatal("ReserveUDP() accepted a balancer result outside the candidate set")
	}
	if client.udpSessions.Load() != 0 {
		t.Fatal("invalid balancer result consumed UDP capacity")
	}
}
