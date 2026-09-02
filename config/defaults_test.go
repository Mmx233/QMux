package config

import (
	"testing"
	"time"
)

func TestApplyDefaults(t *testing.T) {
	client := Client{}
	client.ApplyDefaults()
	if client.ClientID == "" || client.Auth.Method != ClientAuthMethodMTLS ||
		client.HeartbeatInterval != DefaultHeartbeatInterval || client.HealthTimeout != DefaultHealthTimeout ||
		client.Capacity != defaultClientCapacity() {
		t.Fatalf("client defaults = %+v", client)
	}

	server := Server{Listeners: []QuicListener{{}}}
	server.ApplyDefaults()
	if server.HeartbeatInterval != DefaultHeartbeatInterval || server.HealthTimeout != DefaultHealthTimeout ||
		server.LoadBalancer != DefaultLoadBalancer || server.Listeners[0].Capacity != defaultListenerCapacity() {
		t.Fatalf("server defaults = %+v", server)
	}

	if got := (Quic{}).GetConfig().MaxIdleTimeout; got != DefaultMaxIdleTimeout {
		t.Fatalf("QUIC idle timeout = %v, want %v", got, DefaultMaxIdleTimeout)
	}
}

func TestApplyDefaultsPreservesValues(t *testing.T) {
	client := Client{
		ClientID:          "client-id",
		Auth:              ClientAuth{Method: ClientAuthMethodToken},
		Capacity:          ClientCapacity{MaxLocalUDPSessions: 7},
		HeartbeatInterval: time.Second,
		HealthTimeout:     2 * time.Second,
	}
	client.ApplyDefaults()
	if client.ClientID != "client-id" || client.Auth.Method != ClientAuthMethodToken ||
		client.HeartbeatInterval != time.Second || client.HealthTimeout != 2*time.Second ||
		client.Capacity.MaxLocalUDPSessions != 7 {
		t.Fatalf("client values changed: %+v", client)
	}

	wantCapacity := ListenerCapacity{
		MaxClientGenerations: 1, MaxPendingRegistrations: 2, MaxTCPConnections: 3, MaxPendingTCPSetups: 4,
		MaxTCPConnectionsPerGeneration: 5, MaxPendingTCPSetupsPerGeneration: 6,
		MaxUDPSessions: 7, MaxUDPSessionsPerGeneration: 8,
	}
	server := Server{
		Listeners: []QuicListener{{Capacity: wantCapacity}}, LoadBalancer: "round-robin",
		HeartbeatInterval: time.Second, HealthTimeout: 2 * time.Second,
	}
	server.ApplyDefaults()
	if server.HeartbeatInterval != time.Second || server.HealthTimeout != 2*time.Second ||
		server.LoadBalancer != "round-robin" || server.Listeners[0].Capacity != wantCapacity {
		t.Fatalf("server values changed: %+v", server)
	}

	if got := (Quic{MaxIdleTimeout: time.Second}).GetConfig().MaxIdleTimeout; got != time.Second {
		t.Fatalf("QUIC idle timeout changed to %v", got)
	}
}

func TestCapacityValidation(t *testing.T) {
	tests := []struct {
		name string
		set  func(*ListenerCapacity)
	}{
		{"max_client_generations", func(c *ListenerCapacity) { c.MaxClientGenerations = -1 }},
		{"max_pending_registrations", func(c *ListenerCapacity) { c.MaxPendingRegistrations = -1 }},
		{"max_tcp_connections", func(c *ListenerCapacity) { c.MaxTCPConnections = -1 }},
		{"max_pending_tcp_setups", func(c *ListenerCapacity) { c.MaxPendingTCPSetups = -1 }},
		{"max_tcp_connections_per_generation", func(c *ListenerCapacity) { c.MaxTCPConnectionsPerGeneration = -1 }},
		{"max_pending_tcp_setups_per_generation", func(c *ListenerCapacity) { c.MaxPendingTCPSetupsPerGeneration = -1 }},
		{"max_udp_sessions", func(c *ListenerCapacity) { c.MaxUDPSessions = -1 }},
		{"max_udp_sessions_per_generation", func(c *ListenerCapacity) { c.MaxUDPSessionsPerGeneration = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var capacity ListenerCapacity
			test.set(&capacity)
			want := "listeners[3].capacity." + test.name + " must not be negative"
			if err := capacity.Validate("listeners[3].capacity"); err == nil || err.Error() != want {
				t.Fatalf("Validate() error = %v, want %q", err, want)
			}
		})
	}

	if err := (ListenerCapacity{}).Validate("listeners[0].capacity"); err != nil {
		t.Fatalf("zero listener capacity validation: %v", err)
	}
	if err := (ClientCapacity{}).Validate("capacity"); err != nil {
		t.Fatalf("zero client capacity validation: %v", err)
	}
	if err := (ClientCapacity{MaxLocalUDPSessions: -1}).Validate("capacity"); err == nil ||
		err.Error() != "capacity.max_local_udp_sessions must not be negative" {
		t.Fatalf("client capacity validation error = %v", err)
	}
}

func defaultListenerCapacity() ListenerCapacity {
	return ListenerCapacity{
		MaxClientGenerations:             DefaultMaxClientGenerations,
		MaxPendingRegistrations:          DefaultMaxPendingRegistrations,
		MaxTCPConnections:                DefaultMaxTCPConnections,
		MaxPendingTCPSetups:              DefaultMaxPendingTCPSetups,
		MaxTCPConnectionsPerGeneration:   DefaultMaxTCPConnectionsPerGeneration,
		MaxPendingTCPSetupsPerGeneration: DefaultMaxPendingTCPSetupsPerGeneration,
		MaxUDPSessions:                   DefaultMaxUDPSessions,
		MaxUDPSessionsPerGeneration:      DefaultMaxUDPSessionsPerGeneration,
	}
}

func defaultClientCapacity() ClientCapacity {
	return ClientCapacity{MaxLocalUDPSessions: DefaultMaxLocalUDPSessions}
}
