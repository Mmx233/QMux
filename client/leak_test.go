package client

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/rs/zerolog"
	"go.uber.org/goleak"
	"pgregory.net/rapid"
)

// TestMain ensures no goroutine leaks across all tests in this package
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// Ignore known background goroutines from dependencies
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*receiveStream).readImpl"),
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*sendStream).Write"),
	)
}

// TestServerConnection_Close_NoGoroutineLeak verifies that closing a ServerConnection
// does not leak goroutines from its internal context.
func TestServerConnection_Close_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
	)

	logger := zerolog.Nop()
	cache := tls.NewLRUClientSessionCache(0)

	// Create and close multiple connections
	for i := 0; i < 10; i++ {
		conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)
		conn.MarkHealthy()
		conn.Close()
	}
}

// TestServerConnection_RapidCreateClose_NoLeak tests rapid creation and closure
// of ServerConnections to ensure no goroutine accumulation.
func TestServerConnection_RapidCreateClose_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
	)

	logger := zerolog.Nop()
	cache := tls.NewLRUClientSessionCache(0)

	// Rapid create/close cycle
	for i := 0; i < 100; i++ {
		conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)
		conn.Close()
	}
}

// TestConnectionManager_Stop_NoGoroutineLeak verifies that stopping a ConnectionManager
// properly cleans up all internal goroutines (heartbeat loops, reconnection loops).
func TestConnectionManager_Stop_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
	)

	logger := zerolog.Nop()

	cfg := &config.Client{
		ClientID: "test-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: "server1.example.com:8443", ServerName: "server1"},
				{Address: "server2.example.com:8443", ServerName: "server2"},
			},
		},
	}

	cm, err := NewConnectionManager(cfg, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Stop without starting - should still clean up properly
	cm.Stop()
}

// TestConnectionManager_CreateDestroy_NoLeak tests multiple create/destroy cycles
// of ConnectionManager to ensure no goroutine accumulation.
func TestConnectionManager_CreateDestroy_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
	)

	logger := zerolog.Nop()

	for i := 0; i < 5; i++ {
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

		cm.Stop()
	}
}

// TestSessionCacheManager_NoLeak verifies SessionCacheManager doesn't leak resources.
func TestSessionCacheManager_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	scm := NewSessionCacheManager()

	// Create many caches
	for i := 0; i < 100; i++ {
		scm.GetOrCreate("server" + string(rune('0'+i%10)) + ".example.com:8443")
	}

	// Access existing caches
	for i := 0; i < 100; i++ {
		scm.Get("server" + string(rune('0'+i%10)) + ".example.com:8443")
	}
}

// TestConcurrentConnectionOperations_NoLeak tests concurrent operations
// on connections to ensure thread-safe cleanup.
func TestConcurrentConnectionOperations_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
	)

	logger := zerolog.Nop()
	cache := tls.NewLRUClientSessionCache(0)

	done := make(chan struct{})
	conns := make(chan *ServerConnection, 100)

	// Producer: create connections
	go func() {
		for i := 0; i < 50; i++ {
			conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)
			conns <- conn
		}
		close(conns)
	}()

	// Consumer: close connections
	go func() {
		for conn := range conns {
			conn.MarkHealthy()
			conn.MarkUnhealthy()
			conn.Close()
		}
		close(done)
	}()

	<-done
}

// TestConnectionManager_ContextCancellation_NoLeak verifies that context cancellation
// properly stops all goroutines in ConnectionManager.
func TestConnectionManager_ContextCancellation_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
	)

	logger := zerolog.Nop()

	cfg := &config.Client{
		ClientID:          "test-client",
		HeartbeatInterval: 100 * time.Millisecond,
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

	// Create a context that we'll cancel
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel immediately (simulating early shutdown)
	cancel()

	// Start should handle cancelled context gracefully
	// Note: This will fail to connect but should not leak goroutines
	_ = cm.Start(ctx)

	// Stop to ensure cleanup
	cm.Stop()
}

// TestServerConnection_StateTransitions_NoLeak verifies state transitions
// don't cause goroutine leaks.
func TestServerConnection_StateTransitions_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
	)

	logger := zerolog.Nop()
	cache := tls.NewLRUClientSessionCache(0)

	conn := NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)

	// Rapid state transitions
	for i := 0; i < 1000; i++ {
		conn.MarkHealthy()
		conn.MarkUnhealthy()
		conn.CheckHealth(time.Second)
	}

	conn.Close()
}

// Feature: bidirectional-heartbeat, Property 17: Goroutine Cleanup on Close
// *For any* connection close operation (client or server), all associated heartbeat
// goroutines should terminate within a bounded time, and no goroutines should be leaked.
// **Validates: Requirements 9.1, 9.2, 9.3, 9.7, 9.8**
func TestGoroutineCleanupOnClose_Property(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
	)

	rapid.Check(t, func(t *rapid.T) {
		logger := zerolog.Nop()
		cache := tls.NewLRUClientSessionCache(0)

		// Generate random number of connections (1-3)
		connCount := rapid.IntRange(1, 3).Draw(t, "connCount")

		// Use shorter timeouts for faster tests
		healthTimeout := 100 * time.Millisecond
		heartbeatInterval := 50 * time.Millisecond

		// Create connections and start heartbeat loops
		connections := make([]*ServerConnection, connCount)
		for i := 0; i < connCount; i++ {
			conn := NewServerConnection(
				"server.example.com:8443",
				"server.example.com",
				cache,
				logger,
			)
			conn.SetHealthConfig(healthTimeout)
			conn.SetReconnectCallback(func(serverAddr string) {
				// No-op callback for testing
			})

			// Simulate receiving initial heartbeat to make connection healthy
			conn.UpdateLastReceivedFromServer()

			// Start all heartbeat loops
			conn.StartHeartbeatLoops(heartbeatInterval)

			connections[i] = conn
		}

		// Let goroutines run for a short time
		time.Sleep(20 * time.Millisecond)

		// Close all connections
		for _, conn := range connections {
			conn.Close()
		}

		// Wait for goroutines to terminate (bounded time)
		time.Sleep(150 * time.Millisecond)

		// Property: all connections should be in disconnected state
		for i, conn := range connections {
			if conn.State() != StateDisconnected {
				t.Errorf("connection %d should be StateDisconnected after Close, got %v", i, conn.State())
			}
		}

		// Property: all connections should be unhealthy
		for i, conn := range connections {
			if conn.IsHealthy() {
				t.Errorf("connection %d should be unhealthy after Close", i)
			}
		}
	})
}

// Feature: bidirectional-heartbeat, Property 17: Goroutine Cleanup on Close - Rapid Close
// Tests that rapid creation and closure of connections with heartbeat loops doesn't leak goroutines.
// **Validates: Requirements 9.1, 9.2, 9.3, 9.7, 9.8**
func TestGoroutineCleanupOnClose_RapidClose_Property(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
	)

	rapid.Check(t, func(t *rapid.T) {
		logger := zerolog.Nop()
		cache := tls.NewLRUClientSessionCache(0)

		// Generate random number of rapid create/close cycles (5-20)
		cycleCount := rapid.IntRange(5, 20).Draw(t, "cycleCount")

		for i := 0; i < cycleCount; i++ {
			conn := NewServerConnection(
				"server.example.com:8443",
				"server.example.com",
				cache,
				logger,
			)
			conn.SetHealthConfig(500 * time.Millisecond)

			// Start heartbeat loops
			conn.StartHeartbeatLoops(100 * time.Millisecond)

			// Immediately close (tests rapid shutdown)
			conn.Close()
		}

		// Wait for all goroutines to terminate
		time.Sleep(100 * time.Millisecond)
	})
}

// Feature: bidirectional-heartbeat, Property 17: Goroutine Cleanup on Close - Context Cancellation
// Tests that context cancellation properly stops all heartbeat goroutines.
// **Validates: Requirements 9.1, 9.2, 9.3, 9.7, 9.8**
func TestGoroutineCleanupOnClose_ContextCancellation_Property(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
	)

	rapid.Check(t, func(t *rapid.T) {
		logger := zerolog.Nop()
		cache := tls.NewLRUClientSessionCache(0)

		// Generate random number of connections (1-3)
		connCount := rapid.IntRange(1, 3).Draw(t, "connCount")

		connections := make([]*ServerConnection, connCount)
		for i := 0; i < connCount; i++ {
			conn := NewServerConnection(
				"server.example.com:8443",
				"server.example.com",
				cache,
				logger,
			)
			conn.SetHealthConfig(500 * time.Millisecond)
			conn.UpdateLastReceivedFromServer()
			conn.StartHeartbeatLoops(100 * time.Millisecond)
			connections[i] = conn
		}

		// Let goroutines start
		time.Sleep(50 * time.Millisecond)

		// Close connections (which cancels context)
		for _, conn := range connections {
			conn.Close()
		}

		// Property: goroutines should terminate promptly after context cancellation
		// Wait for bounded time
		time.Sleep(200 * time.Millisecond)

		// Verify all connections are properly shut down
		for i, conn := range connections {
			if conn.State() != StateDisconnected {
				t.Errorf("connection %d: expected StateDisconnected, got %v", i, conn.State())
			}
		}
	})
}
