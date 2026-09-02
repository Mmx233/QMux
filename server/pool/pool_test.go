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

// TestConnectionPool_Select tests client selection
func TestConnectionPool_Select(t *testing.T) {
	pool := New("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	// Try to select when no clients exist
	_, err := pool.Select()
	if !errors.Is(err, ErrNoClientsAvailable) {
		t.Errorf("expected ErrNoClientsAvailable, got %v", err)
	}

	// Add healthy client
	client1 := &ClientConn{
		ID: "client1",
	}
	client1.healthy.Store(true)
	_ = pool.Add(client1)

	// Should select the only healthy client
	selected, err := pool.Select()
	if err != nil {
		t.Fatalf("failed to select client: %v", err)
	}
	if selected.ID != "client1" {
		t.Errorf("expected client1, got %s", selected.ID)
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

			clients := []*ClientConn{{ID: "client1"}, {ID: "client2"}, {ID: "client3"}}
			for _, client := range clients {
				if err := pool.Add(client); err != nil {
					t.Fatal(err)
				}
			}

			pool.MarkUnhealthy(clients[0])
			for range 6 {
				selected, err := pool.Select()
				if err != nil || selected == clients[0] {
					t.Fatalf("selection with one unhealthy client = %v, %v", selected, err)
				}
			}

			pool.MarkUnhealthy(clients[1])
			pool.MarkUnhealthy(clients[2])
			if _, err := pool.Select(); !errors.Is(err, ErrNoHealthyClients) {
				t.Fatalf("all-unhealthy error = %v", err)
			}

			pool.MarkHealthy(clients[2])
			selected, err := pool.Select()
			if err != nil || selected != clients[2] {
				t.Fatalf("selection after recovery = %v, %v", selected, err)
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
				ID: fmt.Sprintf("%c", 'A'+id),
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
				if _, err := pool.Select(); err != nil {
					errCh <- err
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

// BenchmarkConnectionPool_Select benchmarks client selection
func BenchmarkConnectionPool_Select(b *testing.B) {
	pool := New("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	// Add 10 clients
	for i := range 10 {
		client := &ClientConn{
			ID: fmt.Sprintf("%c", 'A'+i),
		}
		client.healthy.Store(true)
		_ = pool.Add(client)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = pool.Select()
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

// BenchmarkConnectionPool_Select_Sizes compares pool and raw-balancer selection
// on the same clients at each pool size.
func BenchmarkConnectionPool_Select_Sizes(b *testing.B) {
	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("clients_%d", size), func(b *testing.B) {
			pool := NewWithLimits("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger(), largeTestLimits())
			defer pool.Stop()

			// Populate pool with clients
			for i := range size {
				clientID := fmt.Sprintf("client-%d", i)
				client := &ClientConn{
					ID: clientID,
				}
				client.healthy.Store(true)
				_ = pool.Add(client)
			}

			clients := pool.List()
			balancer := NewRoundRobinBalancer()

			b.Run("Pool", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _ = pool.Select()
				}
			})
			b.Run("Balancer", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _ = balancer.Select(clients)
				}
			})
		})
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

func BenchmarkConnectionPool_SelectParallel(b *testing.B) {
	pool := NewWithLimits("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger(), largeTestLimits())
	defer pool.Stop()

	populateBenchmarkPool(pool, 100)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = pool.Select()
		}
	})
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
// *For any* sequence of Add/Remove operations followed by Select, the balancer SHALL
// return only clients that exist in the current pool and are healthy.
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
				ID: clientIDs[i],
			}
			client.healthy.Store(true)
			_ = pool.Add(client)
		}

		// Perform a Select to populate the cache
		_, _ = pool.Select()

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
					ID: newID,
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

		// Property: Select should only return clients that exist in currentClients
		if len(currentClients) > 0 {
			for range 10 {
				selected, err := pool.Select()
				if err != nil {
					t.Errorf("Select failed unexpectedly: %v", err)
					continue
				}

				// Verify selected client exists in current pool
				if !currentClients[selected.ID] {
					t.Errorf("Select returned client %s which is not in current pool", selected.ID)
				}

				// Verify selected client is healthy
				if !selected.healthy.Load() {
					t.Errorf("Select returned unhealthy client %s", selected.ID)
				}
			}
		}
	})
}
