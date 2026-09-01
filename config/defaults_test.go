package config

import (
	"testing"
	"time"

	"pgregory.net/rapid"
)

// Feature: consolidate-defaults, Property 1: Zero-value fields receive correct defaults
// **Validates: Requirements 3.1, 4.1, 4.2, 5.1, 7.1**
func TestZeroValueDefaultsApplication_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Test Client.ApplyDefaults() with zero values
		client := &Client{
			// All defaultable fields are zero
			ClientID:          "",
			HeartbeatInterval: 0,
		}

		client.ApplyDefaults()

		// Property: ClientID should be generated (non-empty UUID)
		if client.ClientID == "" {
			t.Fatal("expected ClientID to be generated, got empty string")
		}

		// Property: HeartbeatInterval should equal DefaultHeartbeatInterval
		if client.HeartbeatInterval != DefaultHeartbeatInterval {
			t.Fatalf("expected HeartbeatInterval=%v, got %v", DefaultHeartbeatInterval, client.HeartbeatInterval)
		}
		if client.Capacity != defaultClientCapacity() {
			t.Fatalf("client capacity = %+v, want %+v", client.Capacity, defaultClientCapacity())
		}
	})

	rapid.Check(t, func(t *rapid.T) {
		// Test Server.ApplyDefaults() with zero values
		server := &Server{
			Listeners:         []QuicListener{{}},
			HeartbeatInterval: 0,
			HealthTimeout:     0,
		}

		server.ApplyDefaults()

		// Property: HeartbeatInterval should equal DefaultHeartbeatInterval
		if server.HeartbeatInterval != DefaultHeartbeatInterval {
			t.Fatalf("expected HeartbeatInterval=%v, got %v", DefaultHeartbeatInterval, server.HeartbeatInterval)
		}

		// Property: HealthTimeout should equal DefaultHealthTimeout
		if server.HealthTimeout != DefaultHealthTimeout {
			t.Fatalf("expected HealthTimeout=%v, got %v", DefaultHealthTimeout, server.HealthTimeout)
		}
		if server.Listeners[0].Capacity != defaultListenerCapacity() {
			t.Fatalf("listener capacity = %+v, want %+v", server.Listeners[0].Capacity, defaultListenerCapacity())
		}
	})

	rapid.Check(t, func(t *rapid.T) {
		// Test Quic.GetConfig() with zero MaxIdleTimeout
		quic := Quic{
			MaxIdleTimeout: 0,
		}

		cfg := quic.GetConfig()

		// Property: MaxIdleTimeout should equal DefaultMaxIdleTimeout
		if cfg.MaxIdleTimeout != DefaultMaxIdleTimeout {
			t.Fatalf("expected MaxIdleTimeout=%v, got %v", DefaultMaxIdleTimeout, cfg.MaxIdleTimeout)
		}
	})
}

// Feature: consolidate-defaults, Property 2: Non-zero fields are preserved
// **Validates: Requirements 7.2**
func TestNonZeroValuePreservation_Property(t *testing.T) {
	// Generator for non-zero durations (1ms to 1 hour)
	nonZeroDurationGen := rapid.Custom(func(t *rapid.T) time.Duration {
		ms := rapid.Int64Range(1, 3600000).Draw(t, "durationMs")
		return time.Duration(ms) * time.Millisecond
	})

	// Generator for non-empty client IDs
	nonEmptyClientIDGen := rapid.Custom(func(t *rapid.T) string {
		return rapid.StringMatching(`[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}`).Draw(t, "clientID")
	})

	// Test Client.ApplyDefaults() preserves non-zero values
	rapid.Check(t, func(t *rapid.T) {
		originalClientID := nonEmptyClientIDGen.Draw(t, "originalClientID")
		originalHeartbeat := nonZeroDurationGen.Draw(t, "originalHeartbeat")

		client := &Client{
			ClientID:          originalClientID,
			Capacity:          ClientCapacity{MaxLocalUDPSessions: 7},
			HeartbeatInterval: originalHeartbeat,
		}

		client.ApplyDefaults()

		// Property: ClientID should be preserved
		if client.ClientID != originalClientID {
			t.Fatalf("expected ClientID=%q to be preserved, got %q", originalClientID, client.ClientID)
		}

		// Property: HeartbeatInterval should be preserved
		if client.HeartbeatInterval != originalHeartbeat {
			t.Fatalf("expected HeartbeatInterval=%v to be preserved, got %v", originalHeartbeat, client.HeartbeatInterval)
		}
		if client.Capacity.MaxLocalUDPSessions != 7 {
			t.Fatalf("MaxLocalUDPSessions = %d, want 7", client.Capacity.MaxLocalUDPSessions)
		}
	})

	// Test Server.ApplyDefaults() preserves non-zero values
	rapid.Check(t, func(t *rapid.T) {
		originalHeartbeatInterval := nonZeroDurationGen.Draw(t, "originalHeartbeatInterval")
		originalHealthTimeout := nonZeroDurationGen.Draw(t, "originalHealthTimeout")

		server := &Server{
			Listeners: []QuicListener{{Capacity: ListenerCapacity{
				MaxClientGenerations:             1,
				MaxPendingRegistrations:          2,
				MaxTCPConnections:                3,
				MaxPendingTCPSetups:              4,
				MaxTCPConnectionsPerGeneration:   5,
				MaxPendingTCPSetupsPerGeneration: 6,
				MaxUDPSessions:                   7,
				MaxUDPSessionsPerGeneration:      8,
			}}},
			HeartbeatInterval: originalHeartbeatInterval,
			HealthTimeout:     originalHealthTimeout,
		}

		server.ApplyDefaults()

		// Property: HeartbeatInterval should be preserved
		if server.HeartbeatInterval != originalHeartbeatInterval {
			t.Fatalf("expected HeartbeatInterval=%v to be preserved, got %v", originalHeartbeatInterval, server.HeartbeatInterval)
		}

		// Property: HealthTimeout should be preserved
		if server.HealthTimeout != originalHealthTimeout {
			t.Fatalf("expected HealthTimeout=%v to be preserved, got %v", originalHealthTimeout, server.HealthTimeout)
		}
		wantCapacity := ListenerCapacity{
			MaxClientGenerations:             1,
			MaxPendingRegistrations:          2,
			MaxTCPConnections:                3,
			MaxPendingTCPSetups:              4,
			MaxTCPConnectionsPerGeneration:   5,
			MaxPendingTCPSetupsPerGeneration: 6,
			MaxUDPSessions:                   7,
			MaxUDPSessionsPerGeneration:      8,
		}
		if server.Listeners[0].Capacity != wantCapacity {
			t.Fatalf("listener capacity = %+v, want %+v", server.Listeners[0].Capacity, wantCapacity)
		}
	})

	// Test Quic.GetConfig() preserves non-zero MaxIdleTimeout
	rapid.Check(t, func(t *rapid.T) {
		originalMaxIdleTimeout := nonZeroDurationGen.Draw(t, "originalMaxIdleTimeout")

		quic := Quic{
			MaxIdleTimeout: originalMaxIdleTimeout,
		}

		cfg := quic.GetConfig()

		// Property: MaxIdleTimeout should be preserved
		if cfg.MaxIdleTimeout != originalMaxIdleTimeout {
			t.Fatalf("expected MaxIdleTimeout=%v to be preserved, got %v", originalMaxIdleTimeout, cfg.MaxIdleTimeout)
		}
	})
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
