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

// Select chooses a healthy client using round-robin order.
func (r *RoundRobinBalancer) Select(clients []*ClientConn) (*ClientConn, error) {
	if len(clients) == 0 {
		return nil, ErrNoClientsAvailable
	}

	healthyCount := 0
	for _, c := range clients {
		if c.healthy.Load() {
			healthyCount++
		}
	}
	if healthyCount == 0 {
		return nil, ErrNoHealthyClients
	}

	idx := int(r.counter.Add(1) % uint64(healthyCount))
	if healthyCount == len(clients) {
		return clients[idx], nil
	}

	// Health may change between scans; fallback still returns a client observed
	// healthy during the selection scan.
	var fallback *ClientConn
	for _, c := range clients {
		if !c.healthy.Load() {
			continue
		}
		if fallback == nil {
			fallback = c
		}
		if idx == 0 {
			return c, nil
		}
		idx--
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, ErrNoHealthyClients
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

// Select chooses the healthy client with fewest active connections.
func (l *LeastConnectionsBalancer) Select(clients []*ClientConn) (*ClientConn, error) {
	if len(clients) == 0 {
		return nil, ErrNoClientsAvailable
	}

	var selected *ClientConn
	var minConns int64

	for _, c := range clients {
		if !c.healthy.Load() {
			continue
		}
		// Commit increments active before decrementing pending. Read in the
		// opposite order so selection may briefly overcount, but never undercount.
		conns := c.tcpPending.Load() + c.ActiveConns.Load()
		if selected == nil || conns < minConns {
			minConns = conns
			selected = c
		}
	}

	if selected == nil {
		return nil, ErrNoHealthyClients
	}
	return selected, nil
}

// Name returns the balancer name
func (l *LeastConnectionsBalancer) Name() string {
	return "least-connections"
}
