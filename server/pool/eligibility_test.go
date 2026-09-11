package pool

import (
	"errors"
	"testing"
)

func TestProtocolEligibility(t *testing.T) {
	p := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer p.Stop()

	clients := []*ClientConn{
		{ID: "tcp", Metadata: ClientMetadata{Capabilities: []string{"tcp"}}},
		{ID: "udp", Metadata: ClientMetadata{Capabilities: []string{"udp"}}},
		{ID: "both", Metadata: ClientMetadata{Capabilities: []string{"tcp", "udp"}}},
		{ID: "empty"},
		{ID: "unknown", Metadata: ClientMetadata{Capabilities: []string{"sctp"}}},
		{ID: "unhealthy", Metadata: ClientMetadata{Capabilities: []string{"tcp", "udp"}}},
	}
	for _, client := range clients {
		if err := p.Add(client); err != nil {
			t.Fatalf("Add(%q) error = %v", client.ID, err)
		}
	}
	if !p.MarkUnhealthy(clients[5]) {
		t.Fatal("MarkUnhealthy() = false")
	}

	tests := []struct {
		protocol string
		wantIDs  map[string]bool
	}{
		{protocol: "tcp", wantIDs: map[string]bool{"tcp": true, "both": true}},
		{protocol: "udp", wantIDs: map[string]bool{"udp": true, "both": true}},
		{protocol: "both"},
		{protocol: ""},
		{protocol: "sctp"},
	}
	for _, test := range tests {
		t.Run(test.protocol, func(t *testing.T) {
			if got := p.EligibleCount(test.protocol); got != len(test.wantIDs) {
				t.Fatalf("EligibleCount(%q) = %d, want %d", test.protocol, got, len(test.wantIDs))
			}
		})
	}

	tcpIDs := map[string]bool{"tcp": true, "both": true}
	udpIDs := map[string]bool{"udp": true, "both": true}
	for range 10 {
		admission, err := p.BeginTCPAdmission()
		if err != nil {
			t.Fatalf("BeginTCPAdmission() error = %v", err)
		}
		lease, err := admission.Next()
		if err != nil || lease == nil {
			t.Fatalf("Next() = (%v, %v), want TCP lease", lease, err)
		}
		if selected := lease.Client(); !tcpIDs[selected.ID] {
			t.Fatalf("Next() selected TCP-ineligible client %q", selected.ID)
		}
		if !lease.Release() {
			t.Fatal("TCP lease Release() = false")
		}

		selected, err := p.ReserveUDP()
		if err != nil {
			t.Fatalf("ReserveUDP() error = %v", err)
		}
		if !udpIDs[selected.ID] {
			t.Fatalf("ReserveUDP() selected UDP-ineligible client %q", selected.ID)
		}
		if !p.ReleaseUDP(selected) {
			t.Fatal("ReleaseUDP() = false")
		}
	}

	if !p.MarkHealthy(clients[5]) || p.EligibleCount("tcp") != 3 || p.EligibleCount("udp") != 3 {
		t.Fatal("healthy transition did not restore protocol eligibility")
	}
	if !p.Remove(clients[5]) || p.EligibleCount("tcp") != 2 || p.EligibleCount("udp") != 2 {
		t.Fatal("removal did not clear protocol eligibility")
	}

	for _, client := range clients[:3] {
		if !p.MarkUnhealthy(client) {
			t.Fatalf("MarkUnhealthy(%q) = false", client.ID)
		}
	}
	if _, err := p.BeginTCPAdmission(); !errors.Is(err, ErrNoEligibleClients) {
		t.Fatalf("BeginTCPAdmission() with no eligible client error = %v, want %v", err, ErrNoEligibleClients)
	}
	if _, err := p.ReserveUDP(); !errors.Is(err, ErrNoEligibleClients) {
		t.Fatalf("ReserveUDP() with no eligible client error = %v, want %v", err, ErrNoEligibleClients)
	}
}
