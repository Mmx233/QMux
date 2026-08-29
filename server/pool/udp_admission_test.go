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
	p := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer p.Stop()
	p.udpSessionLimit = 2
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
}

func TestUDPAdmissionNonPositiveLimitUsesDefault(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		p := New("test", NewRoundRobinBalancer(), newTestLogger())
		p.udpSessionLimit = limit
		client := addUDPAdmissionClient(t, p, "client")
		client.udpSessions.Store(defaultUDPSessionsPerGeneration - 1)

		if selected, err := p.ReserveUDP(); err != nil || selected != client {
			t.Fatalf("limit %d ReserveUDP(default cap) = (%p, %v), want client %p", limit, selected, err, client)
		}
		if _, err := p.ReserveUDP(); !errors.Is(err, ErrUDPGenerationCapacity) {
			t.Fatalf("limit %d ReserveUDP(default cap+1) error = %v", limit, err)
		}
		p.Stop()
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
			p := New("test", test.new(), newTestLogger())
			defer p.Stop()
			p.udpSessionLimit = 1
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
	p := New("test", NewLeastConnectionsBalancer(), newTestLogger())
	defer p.Stop()
	p.udpSessionLimit = 8
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

	held := make([]*ClientConn, 0, p.udpSessionLimit)
	for selected := range reserved {
		held = append(held, selected)
	}
	if got := len(held); got != int(p.udpSessionLimit) {
		t.Fatalf("successful reservations = %d, want %d", got, p.udpSessionLimit)
	}
	if got := client.udpSessions.Load(); got != p.udpSessionLimit {
		t.Fatalf("generation high water = %d, want %d", got, p.udpSessionLimit)
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
