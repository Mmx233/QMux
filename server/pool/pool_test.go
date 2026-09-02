package pool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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

// TestConnectionPool_HAFailover tests high availability failover
func TestConnectionPool_HAFailover(t *testing.T) {
	pool := New("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	// Add 3 healthy clients
	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}
	for _, c := range clients {
		c.healthy.Store(true)
		_ = pool.Add(c)
	}

	// Verify all clients are healthy
	if pool.HealthyCount() != 3 {
		t.Fatalf("expected 3 healthy clients, got %d", pool.HealthyCount())
	}

	// Mark client1 as unhealthy
	pool.MarkUnhealthy(clients[0])

	// Selection should still work with remaining healthy clients
	selections := make(map[string]int)
	for range 10 {
		selected, err := pool.Select()
		if err != nil {
			t.Fatalf("selection failed after marking one client unhealthy: %v", err)
		}
		selections[selected.ID]++
	}

	// client1 should never be selected
	if selections["client1"] > 0 {
		t.Errorf("unhealthy client1 was selected %d times", selections["client1"])
	}

	// client2 and client3 should be selected
	if selections["client2"] == 0 {
		t.Error("healthy client2 was never selected")
	}
	if selections["client3"] == 0 {
		t.Error("healthy client3 was never selected")
	}
}

// TestConnectionPool_MinimalDowntime tests that downtime is minimal during failover
func TestConnectionPool_MinimalDowntime(t *testing.T) {
	pool := New("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	// Add 2 clients
	client1 := &ClientConn{ID: "client1"}
	client2 := &ClientConn{ID: "client2"}
	client1.healthy.Store(true)
	client2.healthy.Store(true)

	_ = pool.Add(client1)
	_ = pool.Add(client2)

	// Simulate continuous traffic while marking a client unhealthy
	var wg sync.WaitGroup
	errCh := make(chan error, 100)
	stopCh := make(chan struct{})

	// Start continuous selection
	wg.Go(func() {
		for {
			select {
			case <-stopCh:
				return
			default:
				_, err := pool.Select()
				if err != nil {
					errCh <- err
				}
				time.Sleep(1 * time.Millisecond)
			}
		}
	})

	// Let traffic run for a bit
	time.Sleep(50 * time.Millisecond)

	// Mark client1 as unhealthy mid-flight
	pool.MarkUnhealthy(client1)

	// Continue traffic for a bit longer
	time.Sleep(50 * time.Millisecond)

	// Stop traffic
	close(stopCh)
	wg.Wait()
	close(errCh)

	// Check that there were no errors (all selections succeeded)
	errorCount := 0
	for err := range errCh {
		t.Errorf("selection error during failover: %v", err)
		errorCount++
	}

	if errorCount > 0 {
		t.Errorf("had %d errors during failover, expected 0 (minimal downtime violated)", errorCount)
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

	// Concurrent selections
	errCh := make(chan error, 100)
	for range 100 {
		wg.Go(func() {
			for range 10 {
				_, err := pool.Select()
				if err != nil && !errors.Is(err, ErrNoClientsAvailable) {
					errCh <- err
				}
				time.Sleep(1 * time.Millisecond)
			}
		})
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent operation error: %v", err)
	}
}

// TestConnectionPool_AllClientsDown tests behavior when all clients go down
func TestConnectionPool_AllClientsDown(t *testing.T) {
	pool := New("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	// Add 2 healthy clients
	for i := range 2 {
		client := &ClientConn{
			ID: fmt.Sprintf("%c", 'A'+i),
		}
		client.healthy.Store(true)
		_ = pool.Add(client)
	}

	// Mark all clients as unhealthy
	pool.MarkUnhealthy(currentTestClient(pool, "A"))
	pool.MarkUnhealthy(currentTestClient(pool, "B"))

	// Selection should fail gracefully
	_, err := pool.Select()
	if !errors.Is(err, ErrNoHealthyClients) {
		t.Errorf("expected ErrNoHealthyClients when all clients down, got %v", err)
	}

	// Recover one client
	pool.MarkHealthy(currentTestClient(pool, "A"))

	// Selection should work again
	selected, err := pool.Select()
	if err != nil {
		t.Errorf("selection failed after recovering one client: %v", err)
	}
	if selected.ID != "A" {
		t.Errorf("expected client A, got %s", selected.ID)
	}
}

// TestConnectionPool_RapidFailover tests rapid client failures
func TestConnectionPool_RapidFailover(t *testing.T) {
	pool := New("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	// Add 5 clients
	for i := range 5 {
		client := &ClientConn{
			ID: fmt.Sprintf("%c", 'A'+i),
		}
		client.healthy.Store(true)
		_ = pool.Add(client)
	}

	successCount := 0
	// Simulate rapid failures while selecting
	for i := range 100 {
		// Mark random clients as unhealthy/healthy
		if i%10 == 0 {
			clientID := fmt.Sprintf("%c", 'A'+(i/10%5))
			if i%20 == 0 {
				pool.MarkUnhealthy(currentTestClient(pool, clientID))
			} else {
				pool.MarkHealthy(currentTestClient(pool, clientID))
			}
		}

		// Try to select
		_, err := pool.Select()
		if err == nil {
			successCount++
		}
	}

	// Should have mostly succeeded
	if successCount < 80 {
		t.Errorf("only %d/100 selections succeeded during rapid failover, expected >80", successCount)
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

// Feature: bidirectional-heartbeat
// TestConnectionPool_SelectExcludesUnhealthy verifies that pool.Select() never returns
// unhealthy clients, ensuring the load balancer respects the healthy flag.
// Validates: Requirements 6.5
func TestConnectionPool_SelectExcludesUnhealthy(t *testing.T) {
	pool := New("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	// Add 3 clients
	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}

	// Mark all as healthy initially
	for _, c := range clients {
		c.healthy.Store(true)
		_ = pool.Add(c)
	}

	// Mark client2 as unhealthy (simulating heartbeat timeout)
	pool.MarkUnhealthy(clients[1])

	// Perform many selections and verify client2 is never selected
	selections := make(map[string]int)
	for i := range 100 {
		selected, err := pool.Select()
		if err != nil {
			t.Fatalf("Select failed: %v", err)
		}
		selections[selected.ID]++

		// Property: unhealthy client should never be selected
		if selected.ID == "client2" {
			t.Errorf("Select returned unhealthy client2 on iteration %d", i)
		}
	}

	// Verify client2 was never selected
	if selections["client2"] != 0 {
		t.Errorf("unhealthy client2 was selected %d times, expected 0", selections["client2"])
	}

	// Verify healthy clients were selected
	if selections["client1"] == 0 {
		t.Error("healthy client1 was never selected")
	}
	if selections["client3"] == 0 {
		t.Error("healthy client3 was never selected")
	}
}

// TestConnectionPool_SelectAllUnhealthy verifies that pool.Select() returns
// ErrNoHealthyClients when all clients are unhealthy.
// Validates: Requirements 6.5
func TestConnectionPool_SelectAllUnhealthy(t *testing.T) {
	pool := New("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	// Add 3 clients
	for i := range 3 {
		client := &ClientConn{
			ID: fmt.Sprintf("client%d", i),
		}
		client.healthy.Store(true)
		_ = pool.Add(client)
	}

	// Mark all clients as unhealthy (simulating heartbeat timeout for all)
	pool.MarkUnhealthy(currentTestClient(pool, "client0"))
	pool.MarkUnhealthy(currentTestClient(pool, "client1"))
	pool.MarkUnhealthy(currentTestClient(pool, "client2"))

	// Select should return ErrNoHealthyClients
	_, err := pool.Select()
	if !errors.Is(err, ErrNoHealthyClients) {
		t.Errorf("expected ErrNoHealthyClients when all clients unhealthy, got %v", err)
	}
}

// Feature: bidirectional-heartbeat, Property 14: Unhealthy Excluded from Load Balancer
// *For any* load balancer selection operation, clients marked as unhealthy should never
// be returned as the selected client.
// **Validates: Requirements 6.5**
func TestUnhealthyExcludedFromLoadBalancer_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		pool := NewWithLimits("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger(), largeTestLimits())
		defer pool.Stop()

		// Generate random number of clients (2-20)
		clientCount := rapid.IntRange(2, 20).Draw(t, "clientCount")

		// Generate random number of unhealthy clients (1 to clientCount-1)
		// Ensure at least one healthy client remains
		unhealthyCount := rapid.IntRange(1, clientCount-1).Draw(t, "unhealthyCount")

		// Create clients
		clientIDs := make([]string, clientCount)
		unhealthyIDs := make(map[string]bool)

		for i := range clientCount {
			clientIDs[i] = fmt.Sprintf("client-%d", i)
			client := &ClientConn{
				ID: clientIDs[i],
			}
			client.healthy.Store(true)
			_ = pool.Add(client)
		}

		// Mark some clients as unhealthy
		for i := range unhealthyCount {
			pool.MarkUnhealthy(currentTestClient(pool, clientIDs[i]))
			unhealthyIDs[clientIDs[i]] = true
		}

		// Generate number of selections to perform (10-100)
		selectionCount := rapid.IntRange(10, 100).Draw(t, "selectionCount")

		// Perform selections and verify no unhealthy client is ever selected
		for i := range selectionCount {
			selected, err := pool.Select()
			if err != nil {
				t.Fatalf("Select failed unexpectedly: %v", err)
			}

			// Property: selected client should never be unhealthy
			if unhealthyIDs[selected.ID] {
				t.Errorf("iteration %d: Select returned unhealthy client %s", i, selected.ID)
			}

			// Property: selected client's healthy flag should be true
			if !selected.healthy.Load() {
				t.Errorf("iteration %d: Select returned client %s with healthy=false", i, selected.ID)
			}
		}
	})
}

// Feature: bidirectional-heartbeat, Property 14: Unhealthy Excluded from Load Balancer - Dynamic Health
// Tests that clients becoming unhealthy during selection are properly excluded.
// **Validates: Requirements 6.5**
func TestUnhealthyExcludedFromLoadBalancer_DynamicHealth_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		pool := New("127.0.0.1:8080", NewRoundRobinBalancer(), newTestLogger())
		defer pool.Stop()

		// Generate random number of clients (3-10)
		clientCount := rapid.IntRange(3, 10).Draw(t, "clientCount")

		// Create all healthy clients
		clientIDs := make([]string, clientCount)
		for i := range clientCount {
			clientIDs[i] = fmt.Sprintf("client-%d", i)
			client := &ClientConn{
				ID: clientIDs[i],
			}
			client.healthy.Store(true)
			_ = pool.Add(client)
		}

		// Track which clients are currently unhealthy
		unhealthyIDs := make(map[string]bool)

		// Perform selections with dynamic health changes
		for i := range 50 {
			// Randomly mark a client unhealthy or healthy
			if i%5 == 0 && len(unhealthyIDs) < clientCount-1 {
				// Mark a healthy client as unhealthy (keep at least one healthy)
				idx := rapid.IntRange(0, clientCount-1).Draw(t, fmt.Sprintf("unhealthyIdx%d", i))
				if !unhealthyIDs[clientIDs[idx]] {
					pool.MarkUnhealthy(currentTestClient(pool, clientIDs[idx]))
					unhealthyIDs[clientIDs[idx]] = true
				}
			} else if i%7 == 0 && len(unhealthyIDs) > 0 {
				// Mark an unhealthy client as healthy
				for id := range unhealthyIDs {
					pool.MarkHealthy(currentTestClient(pool, id))
					delete(unhealthyIDs, id)
					break
				}
			}

			// Perform selection
			selected, err := pool.Select()
			if err != nil {
				// This can happen if all clients become unhealthy
				if len(unhealthyIDs) == clientCount {
					continue
				}
				t.Fatalf("Select failed unexpectedly: %v", err)
			}

			// Property: selected client should never be in the unhealthy set
			if unhealthyIDs[selected.ID] {
				t.Errorf("iteration %d: Select returned unhealthy client %s", i, selected.ID)
			}
		}
	})
}

// Feature: bidirectional-heartbeat, Property 14: Unhealthy Excluded from Load Balancer - LeastConnections
// Tests that the LeastConnectionsBalancer also excludes unhealthy clients.
// **Validates: Requirements 6.5**
func TestUnhealthyExcludedFromLoadBalancer_LeastConnections_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		pool := New("127.0.0.1:8080", NewLeastConnectionsBalancer(), newTestLogger())
		defer pool.Stop()

		// Generate random number of clients (2-15)
		clientCount := rapid.IntRange(2, 15).Draw(t, "clientCount")

		// Generate random number of unhealthy clients (1 to clientCount-1)
		unhealthyCount := rapid.IntRange(1, clientCount-1).Draw(t, "unhealthyCount")

		// Create clients with varying connection counts
		clientIDs := make([]string, clientCount)
		unhealthyIDs := make(map[string]bool)

		for i := range clientCount {
			clientIDs[i] = fmt.Sprintf("client-%d", i)
			client := &ClientConn{
				ID: clientIDs[i],
			}
			client.healthy.Store(true)
			// Set varying connection counts - unhealthy clients might have lowest connections
			client.ActiveConns.Store(int64(i * 10))
			_ = pool.Add(client)
		}

		// Mark the first N clients as unhealthy (these have lowest connection counts)
		// This tests that even clients with lowest connections are excluded if unhealthy
		for i := range unhealthyCount {
			pool.MarkUnhealthy(currentTestClient(pool, clientIDs[i]))
			unhealthyIDs[clientIDs[i]] = true
		}

		// Generate number of selections to perform (10-50)
		selectionCount := rapid.IntRange(10, 50).Draw(t, "selectionCount")

		// Perform selections and verify no unhealthy client is ever selected
		for i := range selectionCount {
			selected, err := pool.Select()
			if err != nil {
				t.Fatalf("Select failed unexpectedly: %v", err)
			}

			// Property: selected client should never be unhealthy
			// Even though unhealthy clients have lower connection counts
			if unhealthyIDs[selected.ID] {
				t.Errorf("iteration %d: LeastConnections selected unhealthy client %s", i, selected.ID)
			}
		}
	})
}
