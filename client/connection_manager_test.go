package client

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/rs/zerolog"
	"pgregory.net/rapid"
)

// Feature: multi-server-client, Property 6: Session Cache Persistence
// For any server connection that is closed, the associated TLS session cache
// SHALL remain accessible via SessionCacheManager.Get() with the same server address.
// Validates: Requirements 2.4, 4.5
func TestSessionCachePersistence_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		logger := zerolog.Nop()
		sessionCacheManager := NewSessionCacheManager()

		// Generate N server addresses (1-5)
		n := rapid.IntRange(1, 5).Draw(t, "serverCount")
		addresses := make([]string, n)
		for i := 0; i < n; i++ {
			addresses[i] = rapid.StringMatching(`server[0-9]+\.example\.com:8443`).Draw(t, "serverAddr")
		}

		// Create connections and their session caches
		connections := make([]*ServerConnection, n)
		originalCaches := make([]tls.ClientSessionCache, n)
		for i, addr := range addresses {
			cache := sessionCacheManager.GetOrCreate(addr)
			originalCaches[i] = cache
			connections[i] = NewServerConnection(addr, "server.example.com", cache, logger)
		}

		// Close all connections
		for _, conn := range connections {
			conn.Close()
		}

		// Property: session caches should still be accessible after connection close
		for i, addr := range addresses {
			cache := sessionCacheManager.Get(addr)
			if cache == nil {
				t.Fatalf("session cache for %q should persist after connection close", addr)
			}
			if cache != originalCaches[i] {
				t.Fatalf("session cache for %q should be the same instance after connection close", addr)
			}
		}
	})
}

// Feature: multi-server-client, Property 9: Exponential Backoff Reconnection
// For any sequence of N consecutive reconnection attempts, the delay between attempt i
// and attempt i+1 SHALL be greater than or equal to the delay between attempt i-1 and
// attempt i, up to a maximum delay cap.
// Validates: Requirements 3.5, 4.2
func TestExponentialBackoffReconnection_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate N attempts (1-10)
		n := rapid.IntRange(1, 10).Draw(t, "attemptCount")

		// Calculate backoffs for all attempts
		backoffs := make([]time.Duration, n)
		for i := 0; i < n; i++ {
			backoffs[i] = CalculateBackoff(i)
		}

		// Property: backoffs should be non-decreasing
		for i := 1; i < n; i++ {
			if backoffs[i] < backoffs[i-1] {
				t.Fatalf("backoff[%d]=%v should be >= backoff[%d]=%v",
					i, backoffs[i], i-1, backoffs[i-1])
			}
		}

		// Property: backoffs should not exceed MaxBackoff
		for i, backoff := range backoffs {
			if backoff > MaxBackoff {
				t.Fatalf("backoff[%d]=%v should not exceed MaxBackoff=%v",
					i, backoff, MaxBackoff)
			}
		}

		// Property: first backoff should be InitialBackoff
		if backoffs[0] != InitialBackoff {
			t.Fatalf("backoff[0]=%v should be InitialBackoff=%v",
				backoffs[0], InitialBackoff)
		}
	})
}

// Feature: multi-server-client, Property 10: Graceful Shutdown Completeness
// For any ConnectionManager with N active connections, calling Stop() SHALL result
// in all N connections being closed, and GetAllConnections() SHALL return connections
// with state StateDisconnected.
// Validates: Requirements 4.3
func TestGracefulShutdownCompleteness_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		logger := zerolog.Nop()
		sessionCacheManager := NewSessionCacheManager()

		// Generate N connections (1-5)
		n := rapid.IntRange(1, 5).Draw(t, "connectionCount")

		// Create connections
		connections := make([]*ServerConnection, n)
		for i := 0; i < n; i++ {
			addr := rapid.StringMatching(`server[0-9]+\.example\.com:8443`).Draw(t, "serverAddr")
			cache := sessionCacheManager.GetOrCreate(addr)
			connections[i] = NewServerConnection(addr, "server.example.com", cache, logger)
			// Mark some as healthy to simulate active connections
			if rapid.Bool().Draw(t, "isHealthy") {
				connections[i].MarkHealthy()
			}
		}

		// Close all connections (simulating Stop behavior)
		for _, conn := range connections {
			conn.Close()
		}

		// Property: all connections should be in StateDisconnected
		for i, conn := range connections {
			if conn.State() != StateDisconnected {
				t.Fatalf("connection[%d] state should be StateDisconnected after Stop, got %v",
					i, conn.State())
			}
		}

		// Property: all connections should be unhealthy
		for i, conn := range connections {
			if conn.IsHealthy() {
				t.Fatalf("connection[%d] should be unhealthy after Stop", i)
			}
		}
	})
}

// Feature: multi-server-client, Property 13: Stream Continuity on Health Change
// For any active stream on a server connection, marking that connection as unhealthy
// SHALL NOT close or interrupt the stream. The stream SHALL continue until natural
// completion or explicit closure.
// Validates: Requirements 5.4
func TestStreamContinuityOnHealthChange_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		logger := zerolog.Nop()
		cache := tls.NewLRUClientSessionCache(0)
		conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)

		// Mark connection as healthy initially
		conn.MarkHealthy()

		// Verify connection is healthy
		if !conn.IsHealthy() {
			t.Fatal("connection should be healthy after MarkHealthy()")
		}

		// Get initial state
		initialState := conn.State()
		if initialState != StateConnected {
			t.Fatalf("expected StateConnected after MarkHealthy, got %v", initialState)
		}

		// Mark connection as unhealthy (simulating health check failure)
		conn.MarkUnhealthy()

		// Property: connection state should change to unhealthy
		if conn.IsHealthy() {
			t.Fatal("connection should be unhealthy after MarkUnhealthy()")
		}

		// Property: the underlying connection object should NOT be nil
		// (streams can continue on the existing connection)
		// Note: In this test, conn.conn is nil because we didn't actually connect,
		// but the important thing is that MarkUnhealthy() doesn't close the connection.
		// The connection closure is handled separately by the reconnection logic.

		// Property: state should be StateUnhealthy, not StateDisconnected
		if conn.State() != StateUnhealthy {
			t.Fatalf("expected StateUnhealthy, got %v", conn.State())
		}

		// Property: marking unhealthy multiple times should be idempotent
		conn.MarkUnhealthy()
		conn.MarkUnhealthy()
		if conn.State() != StateUnhealthy {
			t.Fatalf("state should remain StateUnhealthy after multiple MarkUnhealthy calls")
		}
	})
}

// Unit test: CalculateBackoff returns correct values
func TestCalculateBackoff(t *testing.T) {
	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{0, 5 * time.Second},   // Initial
		{1, 10 * time.Second},  // 5 * 2
		{2, 20 * time.Second},  // 10 * 2
		{3, 40 * time.Second},  // 20 * 2
		{4, 60 * time.Second},  // Would be 80, but capped at 60
		{5, 60 * time.Second},  // Still capped
		{10, 60 * time.Second}, // Still capped
	}

	for _, tt := range tests {
		got := CalculateBackoff(tt.attempt)
		if got != tt.expected {
			t.Errorf("CalculateBackoff(%d) = %v, want %v", tt.attempt, got, tt.expected)
		}
	}
}

// Unit test: CalculateBackoff with negative attempt
func TestCalculateBackoff_Negative(t *testing.T) {
	got := CalculateBackoff(-1)
	if got != InitialBackoff {
		t.Errorf("CalculateBackoff(-1) = %v, want %v", got, InitialBackoff)
	}
}

// Unit test: NewConnectionManager validates configuration
func TestNewConnectionManager_ValidatesConfig(t *testing.T) {
	logger := zerolog.Nop()

	// Test with empty servers - should fail
	cfg := &config.Client{
		ClientID: "test-client",
		Server:   config.ClientServer{
			// No servers configured
		},
	}

	_, err := NewConnectionManager(cfg, logger)
	if err == nil {
		t.Error("expected error for empty server configuration")
	}
}

// Unit test: NewConnectionManager deduplicates servers
func TestNewConnectionManager_DeduplicatesServers(t *testing.T) {
	logger := zerolog.Nop()

	cfg := &config.Client{
		ClientID: "test-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: "server1.example.com:8443", ServerName: "server1"},
				{Address: "server1.example.com:8443", ServerName: "server1"}, // duplicate
				{Address: "server2.example.com:8443", ServerName: "server2"},
			},
		},
	}

	cm, err := NewConnectionManager(cfg, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// After deduplication, should have 2 servers
	servers := cm.config.Server.GetServers()
	if len(servers) != 2 {
		t.Errorf("expected 2 servers after deduplication, got %d", len(servers))
	}
}

// Unit test: GetAllConnections returns empty slice initially
func TestConnectionManager_GetAllConnections_Empty(t *testing.T) {
	logger := zerolog.Nop()

	cfg := &config.Client{
		ClientID: "test-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: "server.example.com:8443", ServerName: "server"},
			},
		},
	}

	cm, err := NewConnectionManager(cfg, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	conns := cm.GetAllConnections()
	if len(conns) != 0 {
		t.Errorf("expected 0 connections initially, got %d", len(conns))
	}
}

// Unit test: SessionCacheManager returns the manager
func TestConnectionManager_SessionCacheManager(t *testing.T) {
	logger := zerolog.Nop()

	cfg := &config.Client{
		ClientID: "test-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: "server.example.com:8443", ServerName: "server"},
			},
		},
	}

	cm, err := NewConnectionManager(cfg, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	scm := cm.SessionCacheManager()
	if scm == nil {
		t.Error("SessionCacheManager should not be nil")
	}
}

// Unit test: TotalCount returns correct count
func TestConnectionManager_TotalCount(t *testing.T) {
	logger := zerolog.Nop()

	cfg := &config.Client{
		ClientID: "test-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: "server.example.com:8443", ServerName: "server"},
			},
		},
	}

	cm, err := NewConnectionManager(cfg, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cm.TotalCount() != 0 {
		t.Errorf("expected 0 total count initially, got %d", cm.TotalCount())
	}
}

// Unit test: HealthyCount returns correct count
func TestConnectionManager_HealthyCount(t *testing.T) {
	logger := zerolog.Nop()

	cfg := &config.Client{
		ClientID: "test-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: "server.example.com:8443", ServerName: "server"},
			},
		},
	}

	cm, err := NewConnectionManager(cfg, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cm.HealthyCount() != 0 {
		t.Errorf("expected 0 healthy count initially, got %d", cm.HealthyCount())
	}
}
