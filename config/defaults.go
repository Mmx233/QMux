package config

import (
	"time"

	"github.com/google/uuid"
)

// Default timeout and interval values
const (
	// DefaultHeartbeatInterval is the default interval between heartbeat messages
	DefaultHeartbeatInterval = 30 * time.Second

	// DefaultMaxIdleTimeout is the default QUIC connection idle timeout
	DefaultMaxIdleTimeout = 5 * time.Minute

	// DefaultHealthCheckInterval is how often the server checks client health
	DefaultHealthCheckInterval = 10 * time.Second

	// DefaultHealthCheckTimeout is the time without heartbeat before marking unhealthy
	DefaultHealthCheckTimeout = 30 * time.Second
)

// DefaultCapabilities lists the default supported protocols
var DefaultCapabilities = []string{"tcp", "udp"}

// GenerateClientID generates a new UUID for use as a client identifier.
// This is useful for K8s deployments where multiple pods share the same ConfigMap.
func GenerateClientID() string {
	return uuid.New().String()
}
