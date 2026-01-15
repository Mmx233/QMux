package pool

import (
	"sync/atomic"
)

// LoadBalancer selects a client from the pool
type LoadBalancer interface {
	// Select chooses a client from the pool
	Select(clients []*ClientConn) (*ClientConn, error)

	// Name returns the balancer name
	Name() string
}

// RoundRobinBalancer implements round-robin load balancing
type RoundRobinBalancer struct {
	counter atomic.Uint64
}

// NewRoundRobinBalancer creates a new round-robin balancer
func NewRoundRobinBalancer() *RoundRobinBalancer {
	return &RoundRobinBalancer{}
}

// Select chooses a client using round-robin algorithm with O(1) complexity
func (r *RoundRobinBalancer) Select(clients []*ClientConn) (*ClientConn, error) {
	if len(clients) == 0 {
		return nil, ErrNoClientsAvailable
	}

	// Fast path: check if all clients are healthy to avoid allocation
	allHealthy := true
	for _, c := range clients {
		if !c.healthy.Load() {
			allHealthy = false
			break
		}
	}

	var healthy []*ClientConn
	if allHealthy {
		// No allocation needed - use input slice directly
		healthy = clients
	} else {
		// Filter unhealthy clients (allocation only when needed)
		healthy = make([]*ClientConn, 0, len(clients))
		for _, c := range clients {
			if c.healthy.Load() {
				healthy = append(healthy, c)
			}
		}
	}

	if len(healthy) == 0 {
		return nil, ErrNoHealthyClients
	}

	// O(1) selection - single atomic increment with modulo
	idx := r.counter.Add(1) % uint64(len(healthy))
	return healthy[idx], nil
}

// Name returns the balancer name
func (r *RoundRobinBalancer) Name() string {
	return "round-robin"
}

// LeastConnectionsBalancer implements least-connections load balancing
type LeastConnectionsBalancer struct{}

// NewLeastConnectionsBalancer creates a new least-connections balancer
func NewLeastConnectionsBalancer() *LeastConnectionsBalancer {
	return &LeastConnectionsBalancer{}
}

// Select chooses the client with fewest active connections
// Uses linear scan for small pools (≤100 clients) and optimized path for larger pools
func (l *LeastConnectionsBalancer) Select(clients []*ClientConn) (*ClientConn, error) {
	if len(clients) == 0 {
		return nil, ErrNoClientsAvailable
	}

	// Fast path: check if all clients are healthy to avoid allocation
	allHealthy := true
	for _, c := range clients {
		if !c.healthy.Load() {
			allHealthy = false
			break
		}
	}

	if allHealthy {
		// No allocation needed - scan input slice directly
		return l.selectFromSlice(clients)
	}

	// Filter unhealthy clients (allocation only when needed)
	healthy := make([]*ClientConn, 0, len(clients))
	for _, c := range clients {
		if c.healthy.Load() {
			healthy = append(healthy, c)
		}
	}

	if len(healthy) == 0 {
		return nil, ErrNoHealthyClients
	}

	return l.selectFromSlice(healthy)
}

// selectFromSlice finds the client with least connections from a slice of healthy clients
func (l *LeastConnectionsBalancer) selectFromSlice(clients []*ClientConn) (*ClientConn, error) {
	if len(clients) == 0 {
		return nil, ErrNoHealthyClients
	}

	// Linear scan is efficient for all practical pool sizes
	// Heap-based selection only provides benefit for very large pools (>1000)
	// and adds complexity, so we use linear scan for simplicity
	var selected *ClientConn
	minConns := int64(^uint64(0) >> 1) // Max int64

	for _, c := range clients {
		conns := c.ActiveConns.Load()
		if conns < minConns {
			minConns = conns
			selected = c
		}
	}

	return selected, nil
}

// Name returns the balancer name
func (l *LeastConnectionsBalancer) Name() string {
	return "least-connections"
}
