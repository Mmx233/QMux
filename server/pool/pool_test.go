package pool

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"pgregory.net/rapid"
)

func newTestLogger() zerolog.Logger {
	return zerolog.Nop()
}

func currentTestClient(pool *ConnectionPool, clientID string) *ClientConn {
	client, _ := pool.Get(clientID)
	return client
}

func largeTestLimits() Limits {
	limits := defaultLimits()
	limits.MaxClientGenerations = 2048
	return limits
}

// TestConnectionPool_AddRemove tests adding and removing clients
func TestConnectionPool_AddRemove(t *testing.T) {
	pool := New("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	client := &ClientConn{
		ID: "test-client",
	}

	// Add client
	err := pool.Add(client)
	if err != nil {
		t.Fatalf("failed to add client: %v", err)
	}

	if pool.Count() != 1 {
		t.Errorf("expected 1 client, got %d", pool.Count())
	}

	// Try to add duplicate
	err = pool.Add(client)
	if err == nil {
		t.Error("expected error when adding duplicate client")
	}

	// Remove client
	pool.Remove(client)
	if pool.Count() != 0 {
		t.Errorf("expected 0 clients after removal, got %d", pool.Count())
	}
}

func TestConnectionPoolHealthTransitions(t *testing.T) {
	for name, newBalancer := range map[string]func() LoadBalancer{
		"round robin":       func() LoadBalancer { return NewRoundRobinBalancer() },
		"least connections": func() LoadBalancer { return NewLeastConnectionsBalancer() },
	} {
		t.Run(name, func(t *testing.T) {
			pool := New("test", newBalancer(), newTestLogger())
			defer pool.Stop()

			clients := []*ClientConn{
				{ID: "client1", Metadata: ClientMetadata{Capabilities: []string{"tcp"}}},
				{ID: "client2", Metadata: ClientMetadata{Capabilities: []string{"tcp"}}},
				{ID: "client3", Metadata: ClientMetadata{Capabilities: []string{"tcp"}}},
			}
			for _, client := range clients {
				if err := pool.Add(client); err != nil {
					t.Fatal(err)
				}
			}

			pool.MarkUnhealthy(clients[0])
			for range 6 {
				admission, err := pool.BeginTCPAdmission()
				if err != nil {
					t.Fatalf("BeginTCPAdmission() with one unhealthy client error = %v", err)
				}
				lease, err := admission.Next()
				if err != nil || lease == nil || lease.Client() == clients[0] {
					t.Fatalf("Next() with one unhealthy client = (%v, %v)", lease, err)
				}
				if !lease.Release() {
					t.Fatal("Release() with one unhealthy client = false")
				}
			}

			pool.MarkUnhealthy(clients[1])
			pool.MarkUnhealthy(clients[2])
			if _, err := pool.BeginTCPAdmission(); !errors.Is(err, ErrNoEligibleClients) {
				t.Fatalf("all-unhealthy error = %v", err)
			}

			pool.MarkHealthy(clients[2])
			admission, err := pool.BeginTCPAdmission()
			if err != nil {
				t.Fatalf("BeginTCPAdmission() after recovery error = %v", err)
			}
			lease, err := admission.Next()
			if err != nil || lease == nil || lease.Client() != clients[2] {
				t.Fatalf("Next() after recovery = (%v, %v)", lease, err)
			}
			if !lease.Release() {
				t.Fatal("Release() after recovery = false")
			}
		})
	}
}

// TestConnectionPool_ConcurrentOperations tests thread safety
func TestConnectionPool_ConcurrentOperations(t *testing.T) {
	pool := New("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	var wg sync.WaitGroup

	// Concurrent adds
	for i := range 10 {
		id := i
		wg.Go(func() {
			client := &ClientConn{
				ID:       fmt.Sprintf("%c", 'A'+id),
				Metadata: ClientMetadata{Capabilities: []string{"tcp"}},
			}
			client.healthy.Store(true)
			_ = pool.Add(client)
		})
	}

	wg.Wait()

	errCh := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			for range 100 {
				admission, err := pool.BeginTCPAdmission()
				if err != nil {
					errCh <- err
					return
				}
				lease, err := admission.Next()
				if err != nil {
					errCh <- err
					return
				}
				if lease == nil {
					errCh <- errors.New("TCP admission returned nil lease")
					return
				}
				if !lease.Release() {
					errCh <- errors.New("TCP lease release failed")
					return
				}
			}
		})
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent operation error: %v", err)
	}
}

// BenchmarkConnectionPool_Add benchmarks adding clients to pool
func BenchmarkConnectionPool_Add(b *testing.B) {
	limits := largeTestLimits()
	limits.MaxClientGenerations = max(limits.MaxClientGenerations, int64(b.N))
	pool := NewWithLimits("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger(), limits)
	defer pool.Stop()

	// Pre-create clients to avoid allocation in the loop
	clients := make([]*ClientConn, b.N)
	clientIDs := make([]string, b.N)
	for i := 0; i < b.N; i++ {
		clientIDs[i] = fmt.Sprintf("client-%d", i)
		clients[i] = &ClientConn{
			ID: clientIDs[i],
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = pool.Add(clients[i])
	}
}

// BenchmarkConnectionPool_Remove benchmarks removing clients from pool
func BenchmarkConnectionPool_Remove(b *testing.B) {
	// Pre-populate pool with clients
	limits := largeTestLimits()
	limits.MaxClientGenerations = max(limits.MaxClientGenerations, int64(b.N))
	pool := NewWithLimits("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger(), limits)
	defer pool.Stop()

	clients := make([]*ClientConn, b.N)
	for i := 0; i < b.N; i++ {
		clients[i] = &ClientConn{
			ID: fmt.Sprintf("client-%d", i),
		}
		clients[i].healthy.Store(true)
		_ = pool.Add(clients[i])
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		pool.Remove(clients[i])
	}
}

func populateBenchmarkPool(pool *ConnectionPool, count int) []string {
	clientIDs := make([]string, count)
	for i := range count {
		clientIDs[i] = fmt.Sprintf("client-%d", i)
		client := &ClientConn{
			ID: clientIDs[i],
		}
		client.healthy.Store(true)
		_ = pool.Add(client)
	}
	return clientIDs
}

// BenchmarkConnectionPool_Get benchmarks client lookup by ID
func BenchmarkConnectionPool_Get(b *testing.B) {
	pool := NewWithLimits("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger(), largeTestLimits())
	defer pool.Stop()

	// Populate pool with 100 clients
	clientIDs := populateBenchmarkPool(pool, 100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Lookup different clients to avoid caching effects
		_, _ = pool.Get(clientIDs[i%100])
	}
}

func BenchmarkConnectionPool_HealthUpdates(b *testing.B) {
	for _, size := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("clients_%d", size), func(b *testing.B) {
			pool := NewWithLimits("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger(), largeTestLimits())
			defer pool.Stop()

			populateBenchmarkPool(pool, size)
			client := currentTestClient(pool, "client-0")

			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				if i%2 == 0 {
					pool.MarkUnhealthy(client)
				} else {
					pool.MarkHealthy(client)
				}
			}
		})
	}
}

// Feature: performance-optimizations, Property 3: Balancer Cache Invalidation
// *For any* sequence of Add/Remove operations followed by TCP admission, the pool
// SHALL return only clients that exist in the current pool and are healthy.
// Validates: Requirements 2.3
func TestCacheInvalidationCorrectness_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		pool := NewWithLimits("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger(), largeTestLimits())
		defer pool.Stop()

		// Generate initial client count (1-20)
		initialCount := rapid.IntRange(1, 20).Draw(t, "initialCount")

		// Add initial clients
		clientIDs := make([]string, initialCount)
		for i := range initialCount {
			clientIDs[i] = fmt.Sprintf("client-%d", i)
			client := &ClientConn{
				ID:       clientIDs[i],
				Metadata: ClientMetadata{Capabilities: []string{"tcp"}},
			}
			client.healthy.Store(true)
			_ = pool.Add(client)
		}

		// BeginTCPAdmission populates the stable membership cache.
		if _, err := pool.BeginTCPAdmission(); err != nil {
			t.Fatalf("BeginTCPAdmission() while warming cache error = %v", err)
		}

		// Generate number of operations (1-10)
		opCount := rapid.IntRange(1, 10).Draw(t, "opCount")

		// Track current clients in pool
		currentClients := make(map[string]bool)
		for _, id := range clientIDs {
			currentClients[id] = true
		}

		nextClientID := initialCount

		// Perform random Add/Remove operations
		for i := range opCount {
			// 0 = Add, 1 = Remove
			op := rapid.IntRange(0, 1).Draw(t, fmt.Sprintf("op%d", i))

			if op == 0 {
				// Add a new client
				newID := fmt.Sprintf("client-%d", nextClientID)
				nextClientID++
				client := &ClientConn{
					ID:       newID,
					Metadata: ClientMetadata{Capabilities: []string{"tcp"}},
				}
				client.healthy.Store(true)
				_ = pool.Add(client)
				currentClients[newID] = true
			} else {
				// Remove a random existing client (if any)
				var existingIDs []string
				for id := range currentClients {
					existingIDs = append(existingIDs, id)
				}
				if len(existingIDs) > 0 {
					idx := rapid.IntRange(0, len(existingIDs)-1).Draw(t, fmt.Sprintf("removeIdx%d", i))
					removeID := existingIDs[idx]
					pool.Remove(currentTestClient(pool, removeID))
					delete(currentClients, removeID)
				}
			}
		}

		// Property: TCP admission should only return current, healthy clients.
		if len(currentClients) > 0 {
			for range 10 {
				admission, err := pool.BeginTCPAdmission()
				if err != nil {
					t.Errorf("BeginTCPAdmission() failed unexpectedly: %v", err)
					continue
				}
				lease, err := admission.Next()
				if err != nil || lease == nil {
					t.Errorf("Next() = (%v, %v), want lease", lease, err)
					continue
				}
				selected := lease.Client()

				// Verify selected client exists in current pool
				if !currentClients[selected.ID] {
					t.Errorf("Next() returned client %s which is not in current pool", selected.ID)
				}

				// Verify selected client is healthy
				if !selected.healthy.Load() {
					t.Errorf("Next() returned unhealthy client %s", selected.ID)
				}
				if !lease.Release() {
					t.Errorf("Release() for client %s = false", selected.ID)
				}
			}
		}
	})
}
