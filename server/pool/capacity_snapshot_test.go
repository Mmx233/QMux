package pool

import (
	"errors"
	"testing"
)

func TestPoolCapacityGenerationLifecycle(t *testing.T) {
	p := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer p.Stop()

	pending := p.BeginPending()
	assertCapacity(t, p, CapacitySnapshot{ServerPending: 1})

	client := &ClientConn{ID: "client", Metadata: ClientMetadata{Capabilities: []string{"tcp", "udp"}}}
	if err := pending.Reserve(client); err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	assertCapacity(t, p, CapacitySnapshot{ServerPending: 1, Reservations: 1})

	if err := p.Commit(pending); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	assertCapacity(t, p, CapacitySnapshot{Registered: 1})

	admission, err := p.BeginTCPAdmission()
	if err != nil {
		t.Fatalf("BeginTCPAdmission() error = %v", err)
	}
	lease, err := admission.Next()
	if err != nil || lease == nil {
		t.Fatalf("Next() = (%v, %v), want lease", lease, err)
	}
	assertCapacity(t, p, CapacitySnapshot{Registered: 1, TCPPending: 1})
	if !lease.Commit() {
		t.Fatal("TCP Commit() = false")
	}
	client.ActiveConns.Add(2) // Non-TCP activity must not leak into TCPActive.
	if selected, err := p.ReserveUDP(); err != nil || selected != client {
		t.Fatalf("ReserveUDP() = (%p, %v), want %p", selected, err, client)
	}
	assertCapacity(t, p, CapacitySnapshot{Registered: 1, TCPActive: 1, UDPSessions: 1})
	preFaultAdmission, err := p.BeginTCPAdmission()
	if err != nil {
		t.Fatalf("BeginTCPAdmission(pre-fault) error = %v", err)
	}
	unbound := p.BeginPending()
	bound := p.BeginPending()
	if unbound == nil || bound == nil {
		t.Fatal("BeginPending(pre-fault) = nil")
	}
	if err := bound.Reserve(&ClientConn{ID: "bound-before-fault"}); err != nil {
		t.Fatalf("Reserve(bound-before-fault) error = %v", err)
	}

	retirement := p.BeginRetire(client)
	if retirement == nil || p.BeginRetire(client) != nil {
		t.Fatal("BeginRetire() was not exact")
	}
	assertCapacity(t, p, CapacitySnapshot{ServerPending: 2, Reservations: 1, ServerRetiring: 1, TCPActive: 1, UDPSessions: 1})
	if !retirement.Done() || retirement.Done() {
		t.Fatal("Done() was not idempotent")
	}
	assertCapacity(t, p, CapacitySnapshot{ServerPending: 2, Reservations: 1, ServerRetiring: 1, TCPActive: 1, UDPSessions: 1})

	if p.ReleaseUDP(&ClientConn{}) {
		t.Fatal("unmatched UDP release succeeded")
	}
	if next, err := preFaultAdmission.Next(); next != nil || !errors.Is(err, ErrAccountingFault) {
		t.Fatalf("pre-fault TCP admission Next() = (%v, %v), want ErrAccountingFault", next, err)
	}
	if err := unbound.Reserve(&ClientConn{ID: "unbound-after-fault"}); !errors.Is(err, ErrAccountingFault) {
		t.Fatalf("Reserve(unbound-after-fault) error = %v, want ErrAccountingFault", err)
	}
	if err := p.Commit(bound); !errors.Is(err, ErrAccountingFault) {
		t.Fatalf("Commit(bound-after-fault) error = %v, want ErrAccountingFault", err)
	}
	if !p.Abort(unbound) || !p.Abort(bound) {
		t.Fatal("faulted pending reservations did not abort")
	}
	if pending := p.BeginPending(); pending != nil {
		t.Fatal("BeginPending() succeeded after accounting fault")
	}
	if reservation, err := p.Reserve(&ClientConn{ID: "new-after-fault"}); reservation != nil || !errors.Is(err, ErrAccountingFault) {
		t.Fatalf("Reserve(new-after-fault) = (%v, %v), want ErrAccountingFault", reservation, err)
	}
	if admission, err := p.BeginTCPAdmission(); admission != nil || !errors.Is(err, ErrAccountingFault) {
		t.Fatalf("BeginTCPAdmission(after-fault) = (%v, %v), want ErrAccountingFault", admission, err)
	}
	if selected, err := p.ReserveUDP(); selected != nil || !errors.Is(err, ErrAccountingFault) {
		t.Fatalf("ReserveUDP(after-fault) = (%v, %v), want ErrAccountingFault", selected, err)
	}
	snapshot := p.Snapshot()
	if snapshot.PendingRegistrations.CapacityDrops != 0 ||
		snapshot.ClientGenerations.CapacityDrops != 0 ||
		snapshot.TCPConnectionsPerGeneration.CapacityDrops != 0 ||
		snapshot.PendingTCPSetupsPerGeneration.CapacityDrops != 0 ||
		snapshot.UDPSessionsPerGeneration.CapacityDrops != 0 {
		t.Fatalf("accounting-fault rejection changed capacity drops: %+v", snapshot)
	}
	assertCapacity(t, p, CapacitySnapshot{ServerRetiring: 1, TCPActive: 1, UDPSessions: 1, AccountingFaults: 1})

	if !lease.Release() || !p.ReleaseUDP(client) {
		t.Fatal("retired generation leases did not drain after accounting fault")
	}
	assertCapacity(t, p, CapacitySnapshot{AccountingFaults: 1})
}

func TestPoolCapacityPendingAbortIsIdempotent(t *testing.T) {
	p := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer p.Stop()

	pending := p.BeginPending()
	if !p.Abort(pending) || p.Abort(pending) {
		t.Fatal("Abort() was not idempotent")
	}
	if err := p.Commit(pending); err == nil {
		t.Fatal("Commit() after Abort succeeded")
	}
	assertCapacity(t, p, CapacitySnapshot{})

	pending = p.BeginPending()
	if err := pending.Reserve(&ClientConn{}); err == nil {
		t.Fatal("Reserve(empty ID) error = nil")
	}
	if !p.Abort(pending) {
		t.Fatal("Abort() after failed Reserve = false")
	}
	assertCapacity(t, p, CapacitySnapshot{})
}

func assertCapacity(t *testing.T, p *ConnectionPool, want CapacitySnapshot) {
	t.Helper()
	got := p.Snapshot()
	got.PendingRegistrations = LimitSnapshot{}
	got.ClientGenerations = LimitSnapshot{}
	got.TCPConnectionsPerGeneration = LimitSnapshot{}
	got.PendingTCPSetupsPerGeneration = LimitSnapshot{}
	got.UDPSessionsPerGeneration = LimitSnapshot{}
	if got != want {
		t.Fatalf("Snapshot() = %+v, want %+v", got, want)
	}
}
