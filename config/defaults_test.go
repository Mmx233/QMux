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
	})

	rapid.Check(t, func(t *rapid.T) {
		// Test Server.ApplyDefaults() with zero values
		server := &Server{
			HeartbeatInterval: 0,
			HealthTimeout:     0,
		}

		server.ApplyDefaults()

		// Property: HeartbeatInterval should equal DefaultServerHeartbeatInterval
		if server.HeartbeatInterval != DefaultServerHeartbeatInterval {
			t.Fatalf("expected HeartbeatInterval=%v, got %v", DefaultServerHeartbeatInterval, server.HeartbeatInterval)
		}

		// Property: HealthTimeout should equal DefaultServerHealthTimeout
		if server.HealthTimeout != DefaultServerHealthTimeout {
			t.Fatalf("expected HealthTimeout=%v, got %v", DefaultServerHealthTimeout, server.HealthTimeout)
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
	})

	// Test Server.ApplyDefaults() preserves non-zero values
	rapid.Check(t, func(t *rapid.T) {
		originalHeartbeatInterval := nonZeroDurationGen.Draw(t, "originalHeartbeatInterval")
		originalHealthTimeout := nonZeroDurationGen.Draw(t, "originalHealthTimeout")

		server := &Server{
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
