package pool

import (
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
// properly terminates the health check goroutine.
func TestConnectionPool_Stop_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	logger := zerolog.Nop()
	balancer := NewRoundRobinBalancer()

	// Create and stop multiple pools
	for i := 0; i < 10; i++ {
		pool := New(8443+i, balancer, logger)
		// Give health check goroutine time to start
		time.Sleep(10 * time.Millisecond)
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
		pool := New(8443, balancer, logger)
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
	pool := New(8443, balancer, logger)
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
	pool := New(8443, balancer, logger)
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

// TestConnectionPool_HealthCheckLoop_NoLeak verifies the health check loop
// properly terminates when the pool is stopped.
func TestConnectionPool_HealthCheckLoop_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	logger := zerolog.Nop()
	balancer := NewRoundRobinBalancer()

	pool := New(8443, balancer, logger)

	// Set short health check interval
	pool.SetHealthCheckInterval(10 * time.Millisecond)

	// Add some clients
	for i := 0; i < 5; i++ {
		clientID := "client-" + string(rune('0'+i))
		conn := &ClientConn{
			ID:           clientID,
			RegisteredAt: time.Now(),
			LastSeen:     time.Now(),
		}
		pool.Add(clientID, conn)
	}

	// Let health checks run a few times
	time.Sleep(50 * time.Millisecond)

	// Stop pool
	pool.Stop()

	// Allow goroutine to terminate
	time.Sleep(50 * time.Millisecond)
}

// TestConnectionPool_MultipleStops_NoLeak verifies that calling Stop multiple times
// doesn't cause issues or leaks.
func TestConnectionPool_MultipleStops_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	logger := zerolog.Nop()
	balancer := NewRoundRobinBalancer()
	pool := New(8443, balancer, logger)

	// Multiple stops should be safe
	pool.Stop()
	pool.Stop()
	pool.Stop()

	// Allow goroutines to terminate
	time.Sleep(50 * time.Millisecond)
}

// TestBalancer_NoLeak verifies balancer operations don't leak goroutines.
func TestBalancer_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	balancer := NewRoundRobinBalancer()

	// Create test clients
	clients := make([]*ClientConn, 10)
	for i := 0; i < 10; i++ {
		clients[i] = &ClientConn{
			ID:           "client-" + string(rune('0'+i)),
			RegisteredAt: time.Now(),
			LastSeen:     time.Now(),
		}
		clients[i].healthy.Store(true)
	}

	// Many select operations
	for i := 0; i < 10000; i++ {
		balancer.Select(clients)
	}
}

// TestConnectionPool_ClientHealthTransitions_NoLeak tests client health state
// transitions don't cause leaks.
func TestConnectionPool_ClientHealthTransitions_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	logger := zerolog.Nop()
	balancer := NewRoundRobinBalancer()
	pool := New(8443, balancer, logger)
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
