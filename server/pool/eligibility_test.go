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
			if len(test.wantIDs) == 0 {
				if _, err := p.SelectProtocol(test.protocol); !errors.Is(err, ErrNoEligibleClients) {
					t.Fatalf("SelectProtocol(%q) error = %v, want %v", test.protocol, err, ErrNoEligibleClients)
				}
				return
			}
			for range 10 {
				selected, err := p.SelectProtocol(test.protocol)
				if err != nil {
					t.Fatalf("SelectProtocol(%q) error = %v", test.protocol, err)
				}
				if !test.wantIDs[selected.ID] {
					t.Fatalf("SelectProtocol(%q) selected ineligible client %q", test.protocol, selected.ID)
				}
			}
		})
	}

	if !p.MarkHealthy(clients[5]) || p.EligibleCount("tcp") != 3 || p.EligibleCount("udp") != 3 {
		t.Fatal("healthy transition did not restore protocol eligibility")
	}
	if !p.Remove(clients[5]) || p.EligibleCount("tcp") != 2 || p.EligibleCount("udp") != 2 {
		t.Fatal("removal did not clear protocol eligibility")
	}
}
