package client

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"pgregory.net/rapid"
)

// Feature: multi-server-client, Property 7: Health Status Independence
// For any set of N server connections, each connection SHALL have an independent health status.
// Marking connection A as unhealthy SHALL NOT affect the health status of connection B.
// Validates: Requirements 3.1
func TestHealthStatusIndependence_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate N connections (2-5)
		n := rapid.IntRange(2, 5).Draw(t, "connectionCount")

		logger := zerolog.Nop()
		sessionCacheManager := NewSessionCacheManager()

		// Create N server connections
		connections := make([]*ServerConnection, n)
		for i := 0; i < n; i++ {
			addr := rapid.SampledFrom([]string{
				"server1.example.com:8443",
				"server2.example.com:8443",
				"server3.example.com:8443",
				"server4.example.com:8443",
				"server5.example.com:8443",
			}).Draw(t, "serverAddr")
			cache := sessionCacheManager.GetOrCreate(addr)
			connections[i] = NewServerConnection(addr, "server.example.com", cache, logger)
		}

		// Mark all connections as healthy initially
		for _, conn := range connections {
			conn.MarkHealthy()
		}

		// Verify all are healthy
		for i, conn := range connections {
			if !conn.IsHealthy() {
				t.Fatalf("connection %d should be healthy after MarkHealthy()", i)
			}
		}

		// Pick a random connection to mark unhealthy
		unhealthyIdx := rapid.IntRange(0, n-1).Draw(t, "unhealthyIndex")
		connections[unhealthyIdx].MarkUnhealthy()

		// Property: only the marked connection should be unhealthy
		for i, conn := range connections {
			if i == unhealthyIdx {
				if conn.IsHealthy() {
					t.Fatalf("connection %d should be unhealthy after MarkUnhealthy()", i)
				}
			} else {
				if !conn.IsHealthy() {
					t.Fatalf("connection %d should still be healthy (independence violated)", i)
				}
			}
		}
	})
}

// Feature: multi-server-client, Property 8: Health State Transitions
// For any server connection, if the time since last successful heartbeat exceeds the configured
// timeout, the connection SHALL be marked unhealthy. For any server connection that successfully
// sends a heartbeat, it SHALL be marked healthy.
// Validates: Requirements 3.2, 3.3
func TestHealthStateTransitions_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		logger := zerolog.Nop()
		cache := tls.NewLRUClientSessionCache(0)
		conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)

		// Generate a timeout duration (100ms - 5s)
		timeoutMs := rapid.IntRange(100, 5000).Draw(t, "timeoutMs")
		timeout := time.Duration(timeoutMs) * time.Millisecond

		// Initially unhealthy (no heartbeat yet)
		if conn.IsHealthy() {
			t.Fatal("new connection should be unhealthy (no heartbeat yet)")
		}

		// Mark healthy (simulates successful heartbeat)
		conn.MarkHealthy()

		// Property: after MarkHealthy, connection should be healthy
		if !conn.IsHealthy() {
			t.Fatal("connection should be healthy after MarkHealthy()")
		}

		// Property: LastHeartbeat should be set
		lastHB := conn.LastHeartbeat()
		if lastHB.IsZero() {
			t.Fatal("LastHeartbeat should be set after MarkHealthy()")
		}

		// Property: CheckHealth with recent heartbeat should return true
		if !conn.CheckHealth(timeout) {
			t.Fatal("CheckHealth should return true for recent heartbeat")
		}

		// Mark unhealthy
		conn.MarkUnhealthy()

		// Property: after MarkUnhealthy, connection should be unhealthy
		if conn.IsHealthy() {
			t.Fatal("connection should be unhealthy after MarkUnhealthy()")
		}

		// Property: state should be StateUnhealthy
		if conn.State() != StateUnhealthy {
			t.Fatalf("state should be StateUnhealthy, got %v", conn.State())
		}
	})
}

// Unit test: NewServerConnection initializes correctly
func TestNewServerConnection(t *testing.T) {
	logger := zerolog.Nop()
	cache := tls.NewLRUClientSessionCache(0)

	conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)

	if conn.ServerAddr() != "server.example.com:8443" {
		t.Errorf("expected server addr 'server.example.com:8443', got %q", conn.ServerAddr())
	}

	if conn.ServerName() != "server.example.com" {
		t.Errorf("expected server name 'server.example.com', got %q", conn.ServerName())
	}

	if conn.State() != StateDisconnected {
		t.Errorf("expected state StateDisconnected, got %v", conn.State())
	}

	if conn.IsHealthy() {
		t.Error("new connection should not be healthy")
	}

	if conn.Connection() != nil {
		t.Error("new connection should have nil QUIC connection")
	}
}

// Unit test: MarkHealthy updates state and timestamp
func TestServerConnection_MarkHealthy(t *testing.T) {
	logger := zerolog.Nop()
	cache := tls.NewLRUClientSessionCache(0)
	conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)

	before := time.Now()
	conn.MarkHealthy()
	after := time.Now()

	if !conn.IsHealthy() {
		t.Error("connection should be healthy after MarkHealthy()")
	}

	lastHB := conn.LastHeartbeat()
	if lastHB.Before(before) || lastHB.After(after) {
		t.Errorf("LastHeartbeat %v should be between %v and %v", lastHB, before, after)
	}
}

// Unit test: MarkUnhealthy updates state
func TestServerConnection_MarkUnhealthy(t *testing.T) {
	logger := zerolog.Nop()
	cache := tls.NewLRUClientSessionCache(0)
	conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)

	// First mark healthy
	conn.MarkHealthy()
	if !conn.IsHealthy() {
		t.Fatal("connection should be healthy after MarkHealthy()")
	}

	// Then mark unhealthy
	conn.MarkUnhealthy()

	if conn.IsHealthy() {
		t.Error("connection should be unhealthy after MarkUnhealthy()")
	}

	if conn.State() != StateUnhealthy {
		t.Errorf("expected state StateUnhealthy, got %v", conn.State())
	}
}

// Unit test: CheckHealth with expired timeout
func TestServerConnection_CheckHealth_Expired(t *testing.T) {
	logger := zerolog.Nop()
	cache := tls.NewLRUClientSessionCache(0)
	conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)

	// Mark healthy
	conn.MarkHealthy()

	// Wait a bit and check with very short timeout
	time.Sleep(10 * time.Millisecond)

	// CheckHealth with 1ms timeout should fail
	if conn.CheckHealth(1 * time.Millisecond) {
		t.Error("CheckHealth should return false when timeout exceeded")
	}

	// Connection should now be marked unhealthy
	if conn.IsHealthy() {
		t.Error("connection should be unhealthy after CheckHealth timeout")
	}
}

// Unit test: CheckHealth with no heartbeat
func TestServerConnection_CheckHealth_NoHeartbeat(t *testing.T) {
	logger := zerolog.Nop()
	cache := tls.NewLRUClientSessionCache(0)
	conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)

	// CheckHealth without any heartbeat should return false
	if conn.CheckHealth(time.Second) {
		t.Error("CheckHealth should return false when no heartbeat has been sent")
	}
}

// Unit test: ConnectionState String()
func TestConnectionState_String(t *testing.T) {
	tests := []struct {
		state    ConnectionState
		expected string
	}{
		{StateDisconnected, "disconnected"},
		{StateConnecting, "connecting"},
		{StateConnected, "connected"},
		{StateUnhealthy, "unhealthy"},
		{ConnectionState(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.expected {
			t.Errorf("ConnectionState(%d).String() = %q, want %q", tt.state, got, tt.expected)
		}
	}
}

// Unit test: Info returns correct information
func TestServerConnection_Info(t *testing.T) {
	logger := zerolog.Nop()
	cache := tls.NewLRUClientSessionCache(0)
	conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)

	info := conn.Info()

	if info.Address != "server.example.com:8443" {
		t.Errorf("expected address 'server.example.com:8443', got %q", info.Address)
	}

	if info.ServerName != "server.example.com" {
		t.Errorf("expected server name 'server.example.com', got %q", info.ServerName)
	}

	if info.State != StateDisconnected {
		t.Errorf("expected state StateDisconnected, got %v", info.State)
	}

	if info.Healthy {
		t.Error("expected healthy to be false")
	}
}
