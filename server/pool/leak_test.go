package pool

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.uber.org/goleak"
)

// TestMain ensures no goroutine leaks across all tests in this package
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// Ignore known background goroutines from dependencies
		goleak.IgnoreTopFunction("github.com/quic-go/quic-go.(*packetHandlerMap).runCloseQueue"),
	)
}

// TestConnectionPool_Stop_NoGoroutineLeak verifies that stopping a ConnectionPool
// properly terminates cleanly.
func TestConnectionPool_Stop_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	logger := zerolog.Nop()
	balancer := NewRoundRobinBalancer()

	// Create and stop multiple pools
	for i := range 10 {
		pool := New(fmt.Sprintf("127.0.0.1:%d", 8443+i), balancer, logger)
		pool.Stop()
	}

	// Allow goroutines to fully terminate
	time.Sleep(50 * time.Millisecond)
}

// TestConnectionPool_RapidCreateStop_NoLeak tests rapid creation and stopping
// of ConnectionPools to ensure no goroutine accumulation.
func TestConnectionPool_RapidCreateStop_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	logger := zerolog.Nop()
	balancer := NewRoundRobinBalancer()

	// Rapid create/stop cycle
	for range 50 {
		pool := New("127.0.0.1:8443", balancer, logger)
		pool.Stop()
	}

	// Allow goroutines to fully terminate
	time.Sleep(100 * time.Millisecond)
}

// TestConnectionPool_AddRemove_NoLeak verifies that adding and removing clients
// doesn't cause goroutine leaks.
func TestConnectionPool_AddRemove_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	logger := zerolog.Nop()
	balancer := NewRoundRobinBalancer()
	pool := New("127.0.0.1:8443", balancer, logger)
	defer pool.Stop()

	clients := make(map[string]*ClientConn)

	// Add and remove many clients
	for i := range 100 {
		clientID := fmt.Sprintf("client-%c", '0'+i%10)
		conn := &ClientConn{
			ID:           clientID,
			RegisteredAt: time.Now(),
			LastSeen:     time.Now(),
		}

		// Remove first if exists (to allow re-add)
		pool.Remove(clients[clientID])

		if err := pool.Add(conn); err != nil {
			t.Logf("add client %s: %v (expected for duplicates)", clientID, err)
		} else {
			clients[clientID] = conn
		}
	}

	// Remove all
	for i := range 10 {
		clientID := fmt.Sprintf("client-%c", '0'+i)
		pool.Remove(clients[clientID])
	}
}

// TestConnectionPool_ConcurrentOperations_NoLeak tests concurrent pool operations
// to ensure thread-safe cleanup without goroutine leaks.
func TestConnectionPool_ConcurrentOperations_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	logger := zerolog.Nop()
	balancer := NewRoundRobinBalancer()
	pool := New("127.0.0.1:8443", balancer, logger)
	defer pool.Stop()

	var wg sync.WaitGroup
	numGoroutines := 10
	opsPerGoroutine := 50
	clients := make([]atomic.Pointer[ClientConn], numGoroutines)

	// Concurrent adds
	for i := range numGoroutines {
		wg.Go(func() {
			id := i
			for range opsPerGoroutine {
				clientID := "client-" + string(rune('A'+id))
				conn := &ClientConn{
					ID:           clientID,
					RegisteredAt: time.Now(),
					LastSeen:     time.Now(),
				}
				pool.Remove(clients[id].Load())
				if pool.Add(conn) == nil {
					clients[id].Store(conn)
				}
			}
		})
	}

	// Concurrent selects
	for range numGoroutines {
		wg.Go(func() {
			for range opsPerGoroutine {
				_, _ = pool.Select()
			}
		})
	}

	// Concurrent health updates
	for i := range numGoroutines {
		wg.Go(func() {
			id := i
			for range opsPerGoroutine {
				conn := clients[id].Load()
				pool.MarkHealthy(conn)
				pool.MarkUnhealthy(conn)
				pool.UpdateLastSeen(conn)
			}
		})
	}

	wg.Wait()
}

// TestConnectionPool_ClientHealthTransitions_NoLeak tests client health state
// transitions don't cause leaks.
func TestConnectionPool_ClientHealthTransitions_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	logger := zerolog.Nop()
	balancer := NewRoundRobinBalancer()
	pool := New("127.0.0.1:8443", balancer, logger)
	defer pool.Stop()

	// Add a client
	conn := &ClientConn{
		ID:           "test-client",
		RegisteredAt: time.Now(),
		LastSeen:     time.Now(),
	}
	_ = pool.Add(conn)

	// Rapid health transitions
	for range 1000 {
		pool.MarkHealthy(conn)
		pool.MarkUnhealthy(conn)
	}

	pool.Remove(conn)
}
