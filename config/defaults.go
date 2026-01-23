package config

import (
	"time"

	"github.com/google/uuid"
)

// Default timeout and interval values
const (
	// DefaultHeartbeatInterval is the default interval between heartbeat messages (client)
	DefaultHeartbeatInterval = 30 * time.Second

	// DefaultHealthTimeout is the default timeout for determining connection health
	// based on received heartbeats (client-side). Must be greater than HeartbeatInterval.
	DefaultHealthTimeout = 90 * time.Second

	// DefaultMaxIdleTimeout is the default QUIC connection idle timeout
	DefaultMaxIdleTimeout = 5 * time.Minute

	// DefaultServerHeartbeatInterval is the interval between server heartbeats to clients
	DefaultServerHeartbeatInterval = 10 * time.Second

	// DefaultServerHealthTimeout is the time without heartbeat before marking client unhealthy (server-side)
	// Must be greater than client's heartbeat interval
	DefaultServerHealthTimeout = 30 * time.Second

	// DefaultReadBufferSize is the default size for UDP read buffers (max UDP packet size)
	DefaultReadBufferSize = 65535

	// DefaultDatagramBufferSize is the default size for QUIC datagram buffers
	DefaultDatagramBufferSize = 1200

	// DefaultLoadBalancer is the default load balancing algorithm
	DefaultLoadBalancer = "least-connections"
)

// DefaultCapabilities lists the default supported protocols
var DefaultCapabilities = []string{"tcp", "udp"}

// GenerateClientID generates a new UUID for use as a client identifier.
// This is useful for K8s deployments where multiple pods share the same ConfigMap.
func GenerateClientID() string {
	return uuid.New().String()
}
