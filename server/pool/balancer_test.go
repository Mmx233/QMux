package pool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestRoundRobinBalancer(t *testing.T) {
	balancer := NewRoundRobinBalancer()
	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}
	for _, c := range clients {
		c.healthy.Store(true)
	}

	selections := make(map[string]int)
	for range 9 {
		selected, err := balancer.Select(clients)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		selections[selected.ID]++
	}

	for _, c := range clients {
		if selections[c.ID] != 3 {
			t.Errorf("client %s selected %d times, want 3", c.ID, selections[c.ID])
		}
	}
	clients[1].healthy.Store(false)
	for range 6 {
		selected, err := balancer.Select(clients)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if selected.ID == "client2" {
			t.Error("selected unhealthy client2")
		}
	}
}

func TestLeastConnectionsBalancer(t *testing.T) {
	balancer := NewLeastConnectionsBalancer()
	clients := []*ClientConn{
		{ID: "client1"},
		{ID: "client2"},
		{ID: "client3"},
	}
	for i, connections := range []int64{5, 2, 8} {
		clients[i].healthy.Store(true)
		clients[i].ActiveConns.Store(connections)
	}
	selected, err := balancer.Select(clients)
	if err != nil || selected.ID != "client2" {
		t.Fatalf("least-connections selection = %v, %v", selected, err)
	}

	clients[1].healthy.Store(false)
	selected, err = balancer.Select(clients)
	if err != nil || selected.ID != "client1" {
		t.Fatalf("selection after client2 became unhealthy = %v, %v", selected, err)
	}

	for _, client := range clients {
		client.healthy.Store(true)
		client.ActiveConns.Store(5)
	}
	selected, err = balancer.Select(clients)
	if err != nil || selected != clients[0] {
		t.Fatalf("equal-count selection = %v, %v; want first client", selected, err)
	}
}

func TestBalancersUnavailable(t *testing.T) {
	for name, newBalancer := range map[string]func() LoadBalancer{
		"round robin":       func() LoadBalancer { return NewRoundRobinBalancer() },
		"least connections": func() LoadBalancer { return NewLeastConnectionsBalancer() },
	} {
		t.Run(name, func(t *testing.T) {
			balancer := newBalancer()
			if _, err := balancer.Select(nil); !errors.Is(err, ErrNoClientsAvailable) {
				t.Fatalf("empty clients error = %v", err)
			}
			if _, err := balancer.Select([]*ClientConn{{ID: "unhealthy"}}); !errors.Is(err, ErrNoHealthyClients) {
				t.Fatalf("all-unhealthy error = %v", err)
			}
		})
	}
}

func TestBalancersConcurrent(t *testing.T) {
	for name, balancer := range map[string]LoadBalancer{
		"round robin":       NewRoundRobinBalancer(),
		"least connections": NewLeastConnectionsBalancer(),
	} {
		t.Run(name, func(t *testing.T) {
			clients := []*ClientConn{{ID: "client1"}, {ID: "client2"}, {ID: "client3"}}
			for _, client := range clients {
				client.healthy.Store(true)
			}
			var wg sync.WaitGroup
			errs := make(chan error, 16)
			for range 16 {
				wg.Go(func() {
					for range 100 {
						if _, err := balancer.Select(clients); err != nil {
							errs <- err
							return
						}
					}
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
		})
	}
}

// BenchmarkRoundRobinBalancer benchmarks round-robin selection
func BenchmarkRoundRobinBalancer(b *testing.B) {
	balancer := NewRoundRobinBalancer()

	clients := make([]*ClientConn, 10)
	for i := range 10 {
		clients[i] = &ClientConn{ID: fmt.Sprintf("%c", 'A'+i)}
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
	for i := range 10 {
		clients[i] = &ClientConn{ID: fmt.Sprintf("%c", 'A'+i)}
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
	for i := range n {
		clients[i] = &ClientConn{ID: string(rune('A' + (i % 26)))}
		clients[i].healthy.Store(true)
		clients[i].ActiveConns.Store(int64(i % 100))
	}
	return clients
}

// BenchmarkRoundRobinBalancer_Sizes benchmarks round-robin selection with varying client counts
func BenchmarkRoundRobinBalancer_Sizes(b *testing.B) {
	benchmarkBalancerSizes(b, func() LoadBalancer { return NewRoundRobinBalancer() })
}

// BenchmarkLeastConnectionsBalancer_Sizes benchmarks least-connections selection with varying client counts
func BenchmarkLeastConnectionsBalancer_Sizes(b *testing.B) {
	benchmarkBalancerSizes(b, func() LoadBalancer { return NewLeastConnectionsBalancer() })
}

func benchmarkBalancerSizes(b *testing.B, newBalancer func() LoadBalancer) {
	for _, size := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("%d_clients", size), func(b *testing.B) {
			balancer := newBalancer()
			clients := createBenchmarkClients(size)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = balancer.Select(clients)
			}
		})
	}
}

// createMixedHealthClients generates n clients with 50% healthy/unhealthy
func createMixedHealthClients(n int) []*ClientConn {
	clients := make([]*ClientConn, n)
	for i := range n {
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
