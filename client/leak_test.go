package client

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/rs/zerolog"
	"go.uber.org/goleak"
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

// TestLoadBalancer_NoLeak verifies LoadBalancer operations don't leak goroutines.
func TestLoadBalancer_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	lb := NewLoadBalancer()

	logger := zerolog.Nop()
	cache := tls.NewLRUClientSessionCache(0)

	// Create connections
	conns := make([]*ServerConnection, 10)
	for i := 0; i < 10; i++ {
		conns[i] = NewServerConnection("server.example.com:8443", "server.example.com", cache, logger)
		conns[i].MarkHealthy()
	}

	// Update balancer multiple times
	for i := 0; i < 100; i++ {
		lb.UpdateConnections(conns)
		lb.Select()
	}

	// Clean up connections
	for _, conn := range conns {
		conn.Close()
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
