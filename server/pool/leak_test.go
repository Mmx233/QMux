package pool

import (
	"fmt"
	"sync"
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
	for i := 0; i < 10; i++ {
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
	for i := 0; i < 50; i++ {
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

	// Add and remove many clients
	for i := 0; i < 100; i++ {
		clientID := "client-" + string(rune('0'+i%10))
		conn := &ClientConn{
			ID:           clientID,
			RegisteredAt: time.Now(),
			LastSeen:     time.Now(),
		}

		// Remove first if exists (to allow re-add)
		pool.Remove(clientID)

		if err := pool.Add(clientID, conn); err != nil {
			t.Logf("add client %s: %v (expected for duplicates)", clientID, err)
		}
	}

	// Remove all
	for i := 0; i < 10; i++ {
		clientID := "client-" + string(rune('0'+i))
		pool.Remove(clientID)
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

	// Concurrent adds
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				clientID := "client-" + string(rune('A'+id))
				conn := &ClientConn{
					ID:           clientID,
					RegisteredAt: time.Now(),
					LastSeen:     time.Now(),
				}
				pool.Remove(clientID)
				pool.Add(clientID, conn)
			}
		}(i)
	}

	// Concurrent selects
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				pool.Select()
			}
		}()
	}

	// Concurrent health updates
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			clientID := "client-" + string(rune('A'+id))
			for j := 0; j < opsPerGoroutine; j++ {
				pool.MarkHealthy(clientID)
				pool.MarkUnhealthy(clientID)
				pool.UpdateLastSeen(clientID)
			}
		}(i)
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
	pool.Add("test-client", conn)

	// Rapid health transitions
	for i := 0; i < 1000; i++ {
		pool.MarkHealthy("test-client")
		pool.MarkUnhealthy("test-client")
	}

	pool.Remove("test-client")
}
