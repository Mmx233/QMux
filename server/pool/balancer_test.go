package pool

import (
	"errors"
	"sync"
	"testing"

	"pgregory.net/rapid"
)

// TestRoundRobinBalancer_Basic tests basic round-robin selection
func TestRoundRobinBalancer_Basic(t *testing.T) {
	balancer := NewRoundRobinBalancer()

	// Create 3 healthy clients
	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}
	for _, c := range clients {
		c.healthy.Store(true)
	}

	// Test round-robin distribution
	selections := make(map[string]int)
	for i := 0; i < 9; i++ {
		selected, err := balancer.Select(clients)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		selections[selected.ID]++
	}

	// Each client should be selected 3 times
	for _, c := range clients {
		if selections[c.ID] != 3 {
			t.Errorf("client %s selected %d times, want 3", c.ID, selections[c.ID])
		}
	}
}

// TestRoundRobinBalancer_SkipUnhealthy tests that unhealthy clients are skipped
func TestRoundRobinBalancer_SkipUnhealthy(t *testing.T) {
	balancer := NewRoundRobinBalancer()

	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}

	// Mark client1 and client3 as healthy, client2 as unhealthy
	clients[0].healthy.Store(true)
	clients[1].healthy.Store(false)
	clients[2].healthy.Store(true)

	selections := make(map[string]int)
	for i := 0; i < 10; i++ {
		selected, err := balancer.Select(clients)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		selections[selected.ID]++

		// Should never select unhealthy client
		if selected.ID == "client2" {
			t.Error("selected unhealthy client2")
		}
	}

	// client2 should never be selected
	if selections["client2"] != 0 {
		t.Errorf("unhealthy client2 selected %d times, want 0", selections["client2"])
	}

	// client1 and client3 should be selected
	if selections["client1"] == 0 {
		t.Error("healthy client1 never selected")
	}
	if selections["client3"] == 0 {
		t.Error("healthy client3 never selected")
	}
}

// TestRoundRobinBalancer_AllUnhealthy tests error when all clients are unhealthy
func TestRoundRobinBalancer_AllUnhealthy(t *testing.T) {
	balancer := NewRoundRobinBalancer()

	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
	}

	// Mark all clients as unhealthy
	for _, c := range clients {
		c.healthy.Store(false)
	}

	_, err := balancer.Select(clients)
	if !errors.Is(err, ErrNoHealthyClients) {
		t.Errorf("expected ErrNoHealthyClients, got %v", err)
	}
}

// TestRoundRobinBalancer_NoClients tests error when no clients exist
func TestRoundRobinBalancer_NoClients(t *testing.T) {
	balancer := NewRoundRobinBalancer()

	_, err := balancer.Select([]*ClientConn{})
	if !errors.Is(err, ErrNoClientsAvailable) {
		t.Errorf("expected ErrNoClientsAvailable, got %v", err)
	}
}

// TestRoundRobinBalancer_Concurrent tests thread-safety
func TestRoundRobinBalancer_Concurrent(t *testing.T) {
	balancer := NewRoundRobinBalancer()

	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}
	for _, c := range clients {
		c.healthy.Store(true)
	}

	// Run 100 goroutines selecting simultaneously
	var wg sync.WaitGroup
	errCh := make(chan error, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, err := balancer.Select(clients)
				if err != nil {
					errCh <- err
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)

	// Check for any errors
	for err := range errCh {
		t.Errorf("concurrent selection error: %v", err)
	}
}

// TestRoundRobinBalancer_DynamicHealth tests handling of clients becoming unhealthy
func TestRoundRobinBalancer_DynamicHealth(t *testing.T) {
	balancer := NewRoundRobinBalancer()

	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}
	for _, c := range clients {
		c.healthy.Store(true)
	}

	// Select a few times
	for i := 0; i < 5; i++ {
		_, err := balancer.Select(clients)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	// Mark client2 as unhealthy mid-test
	clients[1].healthy.Store(false)

	// Continue selecting - should skip client2
	selections := make(map[string]int)
	for i := 0; i < 10; i++ {
		selected, err := balancer.Select(clients)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		selections[selected.ID]++
	}

	if selections["client2"] != 0 {
		t.Errorf("unhealthy client2 selected %d times after becoming unhealthy", selections["client2"])
	}
}

// TestLeastConnectionsBalancer_Basic tests basic least-connections selection
func TestLeastConnectionsBalancer_Basic(t *testing.T) {
	balancer := NewLeastConnectionsBalancer()

	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}
	for _, c := range clients {
		c.healthy.Store(true)
	}

	// Set different connection counts
	clients[0].ActiveConns.Store(5)
	clients[1].ActiveConns.Store(2)
	clients[2].ActiveConns.Store(8)

	// Should select client2 with lowest connections
	selected, err := balancer.Select(clients)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if selected.ID != "client2" {
		t.Errorf("expected client2 with least connections, got %s", selected.ID)
	}
}

// TestLeastConnectionsBalancer_SkipUnhealthy tests skipping unhealthy clients
func TestLeastConnectionsBalancer_SkipUnhealthy(t *testing.T) {
	balancer := NewLeastConnectionsBalancer()

	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}

	// client1 has least connections but is unhealthy
	clients[0].ActiveConns.Store(1)
	clients[0].healthy.Store(false)

	clients[1].ActiveConns.Store(5)
	clients[1].healthy.Store(true)

	clients[2].ActiveConns.Store(3)
	clients[2].healthy.Store(true)

	// Should select client3 (healthy with fewer connections than client2)
	selected, err := balancer.Select(clients)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if selected.ID != "client3" {
		t.Errorf("expected client3, got %s", selected.ID)
	}
}

// TestLeastConnectionsBalancer_AllUnhealthy tests error when all clients are unhealthy
func TestLeastConnectionsBalancer_AllUnhealthy(t *testing.T) {
	balancer := NewLeastConnectionsBalancer()

	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
	}

	// Mark all clients as unhealthy
	for _, c := range clients {
		c.healthy.Store(false)
	}

	_, err := balancer.Select(clients)
	if !errors.Is(err, ErrNoHealthyClients) {
		t.Errorf("expected ErrNoHealthyClients, got %v", err)
	}
}

// TestLeastConnectionsBalancer_NoClients tests error when no clients exist
func TestLeastConnectionsBalancer_NoClients(t *testing.T) {
	balancer := NewLeastConnectionsBalancer()

	_, err := balancer.Select([]*ClientConn{})
	if !errors.Is(err, ErrNoClientsAvailable) {
		t.Errorf("expected ErrNoClientsAvailable, got %v", err)
	}
}

// TestLeastConnectionsBalancer_Concurrent tests thread-safety
func TestLeastConnectionsBalancer_Concurrent(t *testing.T) {
	balancer := NewLeastConnectionsBalancer()

	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}
	for i, c := range clients {
		c.healthy.Store(true)
		c.ActiveConns.Store(int64(i * 10))
	}

	// Run 100 goroutines selecting simultaneously
	var wg sync.WaitGroup
	errCh := make(chan error, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, err := balancer.Select(clients)
				if err != nil {
					errCh <- err
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)

	// Check for any errors
	for err := range errCh {
		t.Errorf("concurrent selection error: %v", err)
	}
}

// TestLeastConnectionsBalancer_DynamicHealth tests handling of clients becoming unhealthy
func TestLeastConnectionsBalancer_DynamicHealth(t *testing.T) {
	balancer := NewLeastConnectionsBalancer()

	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}
	for i, c := range clients {
		c.healthy.Store(true)
		c.ActiveConns.Store(int64((i + 1) * 10)) // 10, 20, 30
	}

	// Initially client1 should be selected (least connections)
	selected, err := balancer.Select(clients)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.ID != "client1" {
		t.Errorf("expected client1, got %s", selected.ID)
	}

	// Mark client1 as unhealthy mid-test
	clients[0].healthy.Store(false)

	// Now client2 should be selected (least connections among healthy)
	selected, err = balancer.Select(clients)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.ID != "client2" {
		t.Errorf("expected client2 after client1 became unhealthy, got %s", selected.ID)
	}
}

// TestLeastConnectionsBalancer_EqualConnections tests selection when multiple clients have same connection count
func TestLeastConnectionsBalancer_EqualConnections(t *testing.T) {
	balancer := NewLeastConnectionsBalancer()

	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}
	for _, c := range clients {
		c.healthy.Store(true)
		c.ActiveConns.Store(5) // All have same connection count
	}

	// Should select one of them without error (deterministic: first one with min)
	selected, err := balancer.Select(clients)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should consistently select the first client when all have equal connections
	if selected.ID != "client1" {
		t.Errorf("expected client1 (first with min connections), got %s", selected.ID)
	}

	// Verify consistency across multiple calls
	for i := 0; i < 10; i++ {
		s, err := balancer.Select(clients)
		if err != nil {
			t.Fatalf("unexpected error on iteration %d: %v", i, err)
		}
		if s.ID != "client1" {
			t.Errorf("iteration %d: expected consistent selection of client1, got %s", i, s.ID)
		}
	}
}

// BenchmarkRoundRobinBalancer benchmarks round-robin selection
func BenchmarkRoundRobinBalancer(b *testing.B) {
	balancer := NewRoundRobinBalancer()

	clients := make([]*ClientConn, 10)
	for i := 0; i < 10; i++ {
		clients[i] = &ClientConn{ID: string(rune('A' + i))}
		clients[i].healthy.Store(true)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = balancer.Select(clients)
	}
}

// BenchmarkLeastConnectionsBalancer benchmarks least-connections selection
func BenchmarkLeastConnectionsBalancer(b *testing.B) {
	balancer := NewLeastConnectionsBalancer()

	clients := make([]*ClientConn, 10)
	for i := 0; i < 10; i++ {
		clients[i] = &ClientConn{ID: string(rune('A' + i))}
		clients[i].healthy.Store(true)
		clients[i].ActiveConns.Store(int64(i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = balancer.Select(clients)
	}
}

// createBenchmarkClients generates n healthy ClientConn instances for benchmarking
func createBenchmarkClients(n int) []*ClientConn {
	clients := make([]*ClientConn, n)
	for i := 0; i < n; i++ {
		clients[i] = &ClientConn{ID: string(rune('A' + (i % 26)))}
		clients[i].healthy.Store(true)
		clients[i].ActiveConns.Store(int64(i % 100))
	}
	return clients
}

// BenchmarkRoundRobinBalancer_Sizes benchmarks round-robin selection with varying client counts
func BenchmarkRoundRobinBalancer_Sizes(b *testing.B) {
	b.Run("10_clients", func(b *testing.B) {
		balancer := NewRoundRobinBalancer()
		clients := createBenchmarkClients(10)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = balancer.Select(clients)
		}
	})

	b.Run("100_clients", func(b *testing.B) {
		balancer := NewRoundRobinBalancer()
		clients := createBenchmarkClients(100)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = balancer.Select(clients)
		}
	})

	b.Run("1000_clients", func(b *testing.B) {
		balancer := NewRoundRobinBalancer()
		clients := createBenchmarkClients(1000)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = balancer.Select(clients)
		}
	})
}

// BenchmarkLeastConnectionsBalancer_Sizes benchmarks least-connections selection with varying client counts
func BenchmarkLeastConnectionsBalancer_Sizes(b *testing.B) {
	b.Run("10_clients", func(b *testing.B) {
		balancer := NewLeastConnectionsBalancer()
		clients := createBenchmarkClients(10)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = balancer.Select(clients)
		}
	})

	b.Run("100_clients", func(b *testing.B) {
		balancer := NewLeastConnectionsBalancer()
		clients := createBenchmarkClients(100)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = balancer.Select(clients)
		}
	})

	b.Run("1000_clients", func(b *testing.B) {
		balancer := NewLeastConnectionsBalancer()
		clients := createBenchmarkClients(1000)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = balancer.Select(clients)
		}
	})
}

// createMixedHealthClients generates n clients with 50% healthy/unhealthy
func createMixedHealthClients(n int) []*ClientConn {
	clients := make([]*ClientConn, n)
	for i := 0; i < n; i++ {
		clients[i] = &ClientConn{ID: string(rune('A' + (i % 26)))}
		clients[i].healthy.Store(i%2 == 0) // 50% healthy
		clients[i].ActiveConns.Store(int64(i % 100))
	}
	return clients
}

// BenchmarkBalancer_MixedHealth benchmarks balancer selection with 50% healthy/unhealthy clients
func BenchmarkBalancer_MixedHealth(b *testing.B) {
	clients := createMixedHealthClients(100)

	b.Run("RoundRobin", func(b *testing.B) {
		balancer := NewRoundRobinBalancer()

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = balancer.Select(clients)
		}
	})

	b.Run("LeastConnections", func(b *testing.B) {
		balancer := NewLeastConnectionsBalancer()

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = balancer.Select(clients)
		}
	})
}

// BenchmarkBalancer_Parallel benchmarks concurrent balancer selection
func BenchmarkBalancer_Parallel(b *testing.B) {
	clients := createBenchmarkClients(100)

	b.Run("RoundRobin", func(b *testing.B) {
		balancer := NewRoundRobinBalancer()

		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = balancer.Select(clients)
			}
		})
	})

	b.Run("LeastConnections", func(b *testing.B) {
		balancer := NewLeastConnectionsBalancer()

		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = balancer.Select(clients)
			}
		})
	})
}

// Feature: performance-optimizations, Property 2: Balancer Allocation Constancy
// *For any* client count n, the Load_Balancer Select operation SHALL produce a constant
// number of allocations (≤1) regardless of n.
// Validates: Requirements 2.2, 2.4
func TestBalancerAllocationConstancy_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random client count between 1 and 500
		clientCount := rapid.IntRange(1, 500).Draw(t, "clientCount")

		// Create clients with mixed health status (some unhealthy to trigger allocation path)
		clients := make([]*ClientConn, clientCount)
		for i := 0; i < clientCount; i++ {
			clients[i] = &ClientConn{ID: string(rune('A' + (i % 26)))}
			// Make 20% of clients unhealthy to test the allocation path
			clients[i].healthy.Store(i%5 != 0)
			clients[i].ActiveConns.Store(int64(i % 100))
		}

		// Ensure at least one healthy client
		clients[0].healthy.Store(true)

		// Test RoundRobinBalancer
		rrBalancer := NewRoundRobinBalancer()
		allocsBefore := testing.AllocsPerRun(100, func() {
			_, _ = rrBalancer.Select(clients)
		})

		// Property: allocations should be ≤1 regardless of client count
		// When some clients are unhealthy, we allow 1 allocation for the filtered slice
		if allocsBefore > 1 {
			t.Errorf("RoundRobinBalancer: expected ≤1 allocation, got %.0f for %d clients", allocsBefore, clientCount)
		}

		// Test LeastConnectionsBalancer
		lcBalancer := NewLeastConnectionsBalancer()
		allocsLC := testing.AllocsPerRun(100, func() {
			_, _ = lcBalancer.Select(clients)
		})

		if allocsLC > 1 {
			t.Errorf("LeastConnectionsBalancer: expected ≤1 allocation, got %.0f for %d clients", allocsLC, clientCount)
		}
	})
}

// Feature: performance-optimizations, Property 5: All-Healthy Fast Path
// *For any* pool where all clients are healthy, the balancer SHALL skip filtering
// and use the input slice directly (verified by zero additional allocations).
// Validates: Requirements 3.3
func TestAllHealthyFastPath_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random client count between 1 and 500
		clientCount := rapid.IntRange(1, 500).Draw(t, "clientCount")

		// Create all healthy clients
		clients := make([]*ClientConn, clientCount)
		for i := 0; i < clientCount; i++ {
			clients[i] = &ClientConn{ID: string(rune('A' + (i % 26)))}
			clients[i].healthy.Store(true)
			clients[i].ActiveConns.Store(int64(i % 100))
		}

		// Test RoundRobinBalancer - should have 0 allocations when all healthy
		rrBalancer := NewRoundRobinBalancer()
		allocsRR := testing.AllocsPerRun(100, func() {
			_, _ = rrBalancer.Select(clients)
		})

		// Property: zero allocations when all clients are healthy
		if allocsRR > 0 {
			t.Errorf("RoundRobinBalancer: expected 0 allocations for all-healthy pool, got %.0f for %d clients", allocsRR, clientCount)
		}

		// Test LeastConnectionsBalancer - should have 0 allocations when all healthy
		lcBalancer := NewLeastConnectionsBalancer()
		allocsLC := testing.AllocsPerRun(100, func() {
			_, _ = lcBalancer.Select(clients)
		})

		if allocsLC > 0 {
			t.Errorf("LeastConnectionsBalancer: expected 0 allocations for all-healthy pool, got %.0f for %d clients", allocsLC, clientCount)
		}
	})
}
