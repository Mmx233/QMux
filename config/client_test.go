package config

import (
	"fmt"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// Feature: multi-server-client, Property 2: Server Count Validation
// Validates: Requirements 1.3, 6.1
func TestServerCountValidation_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random server count between 0 and 15
		count := rapid.IntRange(0, 15).Draw(t, "serverCount")

		// Create servers with valid addresses
		servers := make([]ServerEndpoint, count)
		for i := 0; i < count; i++ {
			servers[i] = ServerEndpoint{
				Address:    fmt.Sprintf("server%d.example.com:%d", i, 8443+i),
				ServerName: fmt.Sprintf("server%d.example.com", i),
			}
		}

		cs := &ClientServer{Servers: servers}
		err := cs.Validate()

		// Property: validation should fail if count < 1 or count > 10
		if count < MinServers || count > MaxServers {
			if err == nil {
				t.Fatalf("expected validation error for count %d, got nil", count)
			}
		} else {
			if err != nil {
				t.Fatalf("expected validation success for count %d, got error: %v", count, err)
			}
		}
	})
}

// Feature: multi-server-client, Property 14: Address Format Validation
// Validates: Requirements 6.2, 6.3
func TestAddressFormatValidation_Property(t *testing.T) {
	// Generator for valid addresses
	validAddressGen := rapid.Custom(func(t *rapid.T) string {
		// Generate valid hostname or IP
		hostType := rapid.IntRange(0, 2).Draw(t, "hostType")
		var host string
		switch hostType {
		case 0: // hostname
			host = rapid.StringMatching(`[a-z][a-z0-9\-]{0,10}\.[a-z]{2,4}`).Draw(t, "hostname")
		case 1: // IPv4
			host = fmt.Sprintf("%d.%d.%d.%d",
				rapid.IntRange(1, 255).Draw(t, "ip1"),
				rapid.IntRange(0, 255).Draw(t, "ip2"),
				rapid.IntRange(0, 255).Draw(t, "ip3"),
				rapid.IntRange(1, 254).Draw(t, "ip4"))
		case 2: // localhost
			host = "localhost"
		}
		port := rapid.IntRange(1, 65535).Draw(t, "port")
		return fmt.Sprintf("%s:%d", host, port)
	})

	// Test valid addresses
	rapid.Check(t, func(t *rapid.T) {
		addr := validAddressGen.Draw(t, "validAddress")
		err := ValidateAddress(addr)
		if err != nil {
			t.Fatalf("expected valid address %q to pass validation, got error: %v", addr, err)
		}
	})
}

// Feature: multi-server-client, Property 14: Address Format Validation (Invalid)
// Validates: Requirements 6.2, 6.3
func TestAddressFormatValidation_Invalid_Property(t *testing.T) {
	// Generator for invalid addresses
	invalidAddresses := []string{
		"",                // empty
		"noport",          // missing port
		":8080",           // missing host
		"host:",           // missing port number
		"host:0",          // port out of range (0)
		"host:65536",      // port out of range (>65535)
		"host:abc",        // non-numeric port
		"host:-1",         // negative port
		"host:8080:extra", // too many colons (not IPv6)
	}

	for _, addr := range invalidAddresses {
		err := ValidateAddress(addr)
		if err == nil {
			t.Errorf("expected invalid address %q to fail validation, got nil", addr)
		}
	}
}

// Feature: multi-server-client, Property 15: Duplicate Deduplication
// Validates: Requirements 6.4
func TestDuplicateDeduplication_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate a base set of unique addresses
		uniqueCount := rapid.IntRange(1, 5).Draw(t, "uniqueCount")
		baseAddresses := make([]string, uniqueCount)
		for i := 0; i < uniqueCount; i++ {
			baseAddresses[i] = fmt.Sprintf("server%d.example.com:%d", i, 8443+i)
		}

		// Generate servers - first include all unique addresses, then add duplicates
		duplicateCount := rapid.IntRange(0, 5).Draw(t, "duplicateCount")
		totalCount := uniqueCount + duplicateCount
		servers := make([]ServerEndpoint, totalCount)

		// First, add all unique addresses
		for i := 0; i < uniqueCount; i++ {
			servers[i] = ServerEndpoint{
				Address:    baseAddresses[i],
				ServerName: fmt.Sprintf("server%d.example.com", i),
			}
		}

		// Then add duplicates
		for i := uniqueCount; i < totalCount; i++ {
			idx := rapid.IntRange(0, uniqueCount-1).Draw(t, fmt.Sprintf("dupIdx%d", i))
			servers[i] = ServerEndpoint{
				Address:    baseAddresses[idx],
				ServerName: fmt.Sprintf("server%d.example.com", idx),
			}
		}

		cs := &ClientServer{Servers: servers}
		deduplicated, hasDuplicates := cs.DeduplicateServers()

		// Property 1: deduplicated list should have unique addresses
		seen := make(map[string]bool)
		for _, s := range deduplicated {
			if seen[s.Address] {
				t.Fatalf("deduplicated list contains duplicate address: %s", s.Address)
			}
			seen[s.Address] = true
		}

		// Property 2: deduplicated count should equal unique count
		if len(deduplicated) != uniqueCount {
			t.Fatalf("expected %d unique addresses, got %d", uniqueCount, len(deduplicated))
		}

		// Property 3: hasDuplicates should be true if duplicateCount > 0
		if duplicateCount > 0 && !hasDuplicates {
			t.Fatalf("expected hasDuplicates=true when duplicateCount(%d) > 0", duplicateCount)
		}

		// Property 4: all original unique addresses should be present
		for _, addr := range baseAddresses {
			if !seen[addr] {
				t.Fatalf("deduplicated list missing address: %s", addr)
			}
		}
	})
}

// Unit test for multi-server configuration
func TestGetServers_MultiServer(t *testing.T) {
	cs := &ClientServer{
		Servers: []ServerEndpoint{
			{Address: "server1.example.com:8443", ServerName: "server1.example.com"},
			{Address: "server2.example.com:8443", ServerName: "server2.example.com"},
		},
	}

	servers := cs.GetServers()
	if len(servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(servers))
	}
}

// Unit test for empty configuration
func TestGetServers_Empty(t *testing.T) {
	cs := &ClientServer{}
	servers := cs.GetServers()
	if len(servers) != 0 {
		t.Fatalf("expected 0 for empty config, got %d", len(servers))
	}
}

// Feature: bidirectional-heartbeat, Property 16: Configuration Validation
// Validates: Requirements 8.5
// For any valid configuration, the healthTimeout should be greater than the heartbeatInterval.
func TestConfigurationValidation_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random heartbeat interval (1-60 seconds)
		heartbeatIntervalSec := rapid.IntRange(1, 60).Draw(t, "heartbeatIntervalSec")
		heartbeatInterval := time.Duration(heartbeatIntervalSec) * time.Second

		// Generate random health timeout (1-120 seconds)
		healthTimeoutSec := rapid.IntRange(1, 120).Draw(t, "healthTimeoutSec")
		healthTimeout := time.Duration(healthTimeoutSec) * time.Second

		client := &Client{
			HeartbeatInterval: heartbeatInterval,
			HealthTimeout:     healthTimeout,
		}

		err := client.Validate()

		// Property: validation should pass if and only if healthTimeout > heartbeatInterval
		if healthTimeout > heartbeatInterval {
			if err != nil {
				t.Fatalf("expected validation success when healthTimeout (%v) > heartbeatInterval (%v), got error: %v",
					healthTimeout, heartbeatInterval, err)
			}
		} else {
			if err == nil {
				t.Fatalf("expected validation error when healthTimeout (%v) <= heartbeatInterval (%v), got nil",
					healthTimeout, heartbeatInterval)
			}
		}
	})
}
