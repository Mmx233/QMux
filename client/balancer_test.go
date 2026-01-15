package client

import (
	"crypto/tls"
	"testing"

	"github.com/rs/zerolog"
	"pgregory.net/rapid"
)

// Feature: multi-server-client, Property 11: Healthy-Only Selection
// For any LoadBalancer with a mix of healthy and unhealthy connections,
// Select() SHALL never return an unhealthy connection.
// If no healthy connections exist, Select() SHALL return an error.
// Validates: Requirements 5.1
func TestHealthyOnlySelection_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		logger := zerolog.Nop()
		lb := NewLoadBalancer()

		// Generate N connections (1-10)
		n := rapid.IntRange(1, 10).Draw(t, "connectionCount")

		// Create connections
		connections := make([]*ServerConnection, n)
		for i := 0; i < n; i++ {
			addr := rapid.StringMatching(`server[0-9]+\.example\.com:8443`).Draw(t, "serverAddr")
			cache := tls.NewLRUClientSessionCache(0)
			connections[i] = NewServerConnection(addr, "server.example.com", cache, logger)
		}

		// Generate random health states
		healthyCount := 0
		for i := 0; i < n; i++ {
			isHealthy := rapid.Bool().Draw(t, "isHealthy")
			if isHealthy {
				connections[i].MarkHealthy()
				healthyCount++
			}
		}

		lb.UpdateConnections(connections)

		// Property: If no healthy connections, Select() should return error
		if healthyCount == 0 {
			_, err := lb.Select()
			if err != ErrNoHealthyConnections {
				t.Fatalf("expected ErrNoHealthyConnections when no healthy connections, got %v", err)
			}
			return
		}

		// Property: Select() should only return healthy connections
		// Make multiple selections to verify
		numSelections := rapid.IntRange(1, 20).Draw(t, "numSelections")
		for i := 0; i < numSelections; i++ {
			selected, err := lb.Select()
			if err != nil {
				t.Fatalf("Select() returned error with %d healthy connections: %v", healthyCount, err)
			}
			if !selected.IsHealthy() {
				t.Fatalf("Select() returned unhealthy connection %s", selected.ServerAddr())
			}
		}
	})
}

// Feature: multi-server-client, Property 12: Round-Robin Distribution
// For any LoadBalancer with M healthy connections, making N×M selections (where N ≥ 1)
// SHALL result in each healthy connection being selected exactly N times.
// Validates: Requirements 5.2
func TestRoundRobinDistribution_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		logger := zerolog.Nop()
		lb := NewLoadBalancer()

		// Generate M healthy connections (2-5 for reasonable test size)
		m := rapid.IntRange(2, 5).Draw(t, "healthyCount")

		// Create all healthy connections
		connections := make([]*ServerConnection, m)
		for i := 0; i < m; i++ {
			addr := rapid.StringMatching(`server[0-9]+\.example\.com:8443`).Draw(t, "serverAddr")
			cache := tls.NewLRUClientSessionCache(0)
			connections[i] = NewServerConnection(addr, "server.example.com", cache, logger)
			connections[i].MarkHealthy()
		}

		lb.UpdateConnections(connections)

		// Generate N (1-5 rounds)
		n := rapid.IntRange(1, 5).Draw(t, "rounds")
		totalSelections := n * m

		// Track selection counts per connection
		selectionCounts := make(map[*ServerConnection]int)
		for i := 0; i < totalSelections; i++ {
			selected, err := lb.Select()
			if err != nil {
				t.Fatalf("Select() returned error: %v", err)
			}
			selectionCounts[selected]++
		}

		// Property: Each healthy connection should be selected exactly N times
		for _, conn := range connections {
			count := selectionCounts[conn]
			if count != n {
				t.Fatalf("connection %s selected %d times, expected %d (round-robin violated)",
					conn.ServerAddr(), count, n)
			}
		}
	})
}

// Unit test: NewLoadBalancer creates empty balancer
func TestNewLoadBalancer(t *testing.T) {
	lb := NewLoadBalancer()

	if lb.ConnectionCount() != 0 {
		t.Errorf("expected 0 connections, got %d", lb.ConnectionCount())
	}

	if lb.HealthyCount() != 0 {
		t.Errorf("expected 0 healthy connections, got %d", lb.HealthyCount())
	}
}

// Unit test: Select with no connections returns error
func TestLoadBalancer_Select_NoConnections(t *testing.T) {
	lb := NewLoadBalancer()

	_, err := lb.Select()
	if err != ErrNoHealthyConnections {
		t.Errorf("expected ErrNoHealthyConnections, got %v", err)
	}
}

// Unit test: UpdateConnections updates the connection list
func TestLoadBalancer_UpdateConnections(t *testing.T) {
	logger := zerolog.Nop()
	lb := NewLoadBalancer()

	cache := tls.NewLRUClientSessionCache(0)
	conn1 := NewServerConnection("server1.example.com:8443", "server.example.com", cache, logger)
	conn2 := NewServerConnection("server2.example.com:8443", "server.example.com", cache, logger)

	lb.UpdateConnections([]*ServerConnection{conn1, conn2})

	if lb.ConnectionCount() != 2 {
		t.Errorf("expected 2 connections, got %d", lb.ConnectionCount())
	}
}

// Unit test: HealthyConnections returns only healthy ones
func TestLoadBalancer_HealthyConnections(t *testing.T) {
	logger := zerolog.Nop()
	lb := NewLoadBalancer()

	cache := tls.NewLRUClientSessionCache(0)
	conn1 := NewServerConnection("server1.example.com:8443", "server.example.com", cache, logger)
	conn2 := NewServerConnection("server2.example.com:8443", "server.example.com", cache, logger)
	conn3 := NewServerConnection("server3.example.com:8443", "server.example.com", cache, logger)

	conn1.MarkHealthy()
	// conn2 stays unhealthy
	conn3.MarkHealthy()

	lb.UpdateConnections([]*ServerConnection{conn1, conn2, conn3})

	healthy := lb.HealthyConnections()
	if len(healthy) != 2 {
		t.Errorf("expected 2 healthy connections, got %d", len(healthy))
	}

	// Verify the healthy ones are conn1 and conn3
	for _, h := range healthy {
		if h != conn1 && h != conn3 {
			t.Errorf("unexpected healthy connection: %s", h.ServerAddr())
		}
	}
}

// Unit test: Select returns healthy connection
func TestLoadBalancer_Select_ReturnsHealthy(t *testing.T) {
	logger := zerolog.Nop()
	lb := NewLoadBalancer()

	cache := tls.NewLRUClientSessionCache(0)
	conn1 := NewServerConnection("server1.example.com:8443", "server.example.com", cache, logger)
	conn2 := NewServerConnection("server2.example.com:8443", "server.example.com", cache, logger)

	conn1.MarkHealthy()
	// conn2 stays unhealthy

	lb.UpdateConnections([]*ServerConnection{conn1, conn2})

	selected, err := lb.Select()
	if err != nil {
		t.Fatalf("Select() returned error: %v", err)
	}

	if selected != conn1 {
		t.Errorf("expected conn1 to be selected, got %s", selected.ServerAddr())
	}
}

// Unit test: HealthyCount returns correct count
func TestLoadBalancer_HealthyCount(t *testing.T) {
	logger := zerolog.Nop()
	lb := NewLoadBalancer()

	cache := tls.NewLRUClientSessionCache(0)
	conn1 := NewServerConnection("server1.example.com:8443", "server.example.com", cache, logger)
	conn2 := NewServerConnection("server2.example.com:8443", "server.example.com", cache, logger)
	conn3 := NewServerConnection("server3.example.com:8443", "server.example.com", cache, logger)

	conn1.MarkHealthy()
	conn3.MarkHealthy()

	lb.UpdateConnections([]*ServerConnection{conn1, conn2, conn3})

	if lb.HealthyCount() != 2 {
		t.Errorf("expected 2 healthy, got %d", lb.HealthyCount())
	}
}
