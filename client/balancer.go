package client

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrNoHealthyConnections is returned when no healthy connections are available
var ErrNoHealthyConnections = errors.New("no healthy connections available")

// LoadBalancer selects healthy connections for traffic distribution.
// It uses round-robin selection among healthy server connections.
type LoadBalancer struct {
	connections []*ServerConnection
	index       atomic.Uint64
	mu          sync.RWMutex
}

// NewLoadBalancer creates a new LoadBalancer instance.
func NewLoadBalancer() *LoadBalancer {
	return &LoadBalancer{}
}

// Select returns the next healthy connection using round-robin selection.
// Returns ErrNoHealthyConnections if no healthy connections are available.
func (lb *LoadBalancer) Select() (*ServerConnection, error) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	healthy := lb.healthyConnectionsLocked()
	if len(healthy) == 0 {
		return nil, ErrNoHealthyConnections
	}

	// Round-robin selection among healthy connections
	idx := lb.index.Add(1) - 1
	selected := healthy[idx%uint64(len(healthy))]

	return selected, nil
}

// UpdateConnections updates the list of available connections.
func (lb *LoadBalancer) UpdateConnections(conns []*ServerConnection) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.connections = conns
}

// HealthyConnections returns only healthy connections.
func (lb *LoadBalancer) HealthyConnections() []*ServerConnection {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	return lb.healthyConnectionsLocked()
}

// healthyConnectionsLocked returns healthy connections (caller must hold lock).
func (lb *LoadBalancer) healthyConnectionsLocked() []*ServerConnection {
	var healthy []*ServerConnection
	for _, conn := range lb.connections {
		if conn.IsHealthy() {
			healthy = append(healthy, conn)
		}
	}
	return healthy
}

// ConnectionCount returns the total number of connections.
func (lb *LoadBalancer) ConnectionCount() int {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	return len(lb.connections)
}

// HealthyCount returns the number of healthy connections.
func (lb *LoadBalancer) HealthyCount() int {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	return len(lb.healthyConnectionsLocked())
}
