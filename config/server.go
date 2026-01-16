package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Server struct {
	Listeners []QuicListener `yaml:"listeners"`
	Auth      ServerAuth     `yaml:"auth"`
	TLS       ServerTLS      `yaml:"tls"`

	// Health check configuration
	HealthCheckInterval time.Duration `yaml:"health_check_interval"` // How often to check client health, default 10s
	HealthCheckTimeout  time.Duration `yaml:"health_check_timeout"`  // Time without heartbeat before marking unhealthy, default 30s
}

type QuicListener struct {
	Listen      `yaml:",inline"`
	Quic        `yaml:",inline"`
	TrafficPort int    `yaml:"traffic_port"` // 1:1 mapped traffic port for this listener
	Protocol    string `yaml:"protocol"`     // "tcp", "udp", or "both"
}

type ServerAuth struct {
	Method  string    `yaml:"method"` // "mtls", "token", etc.
	Content yaml.Node `yaml:",inline"`
}

type ServerTLS struct {
	CACertFile     string `yaml:"ca_cert_file"`
	ServerCertFile string `yaml:"server_cert_file"`
	ServerKeyFile  string `yaml:"server_key_file"`

	// Rotation interval for session ticket encryption keys.
	// Recommended: 24h for production, 0 to disable rotation.
	// Keys are rotated periodically to limit the exposure window if compromised.
	SessionTicketEncryptionKeyRotationInterval time.Duration `yaml:"session_ticket_encryption_key_rotation_interval"`

	// Number of keys to maintain during rotation (current + old keys).
	// Recommended: 2-3 for smooth rotation, default: 2 if not specified.
	// Higher values allow clients with older tickets to still resume sessions.
	SessionTicketEncryptionKeyRotationOverlap uint8 `yaml:"session_ticket_encryption_key_rotation_overlap"`

	// Loaded certificates (not from YAML)
	CACertPool *x509.CertPool  `yaml:"-"`
	ServerCert tls.Certificate `yaml:"-"`
}

// LoadCertificates loads TLS certificates from files
func (t *ServerTLS) LoadCertificates() error {
	// Load CA certificate
	caCertPEM, err := os.ReadFile(t.CACertFile)
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}

	t.CACertPool = x509.NewCertPool()
	if !t.CACertPool.AppendCertsFromPEM(caCertPEM) {
		return fmt.Errorf("failed to parse CA certificate")
	}

	// Load server certificate and key
	cert, err := tls.LoadX509KeyPair(t.ServerCertFile, t.ServerKeyFile)
	if err != nil {
		return fmt.Errorf("load server cert/key: %w", err)
	}
	t.ServerCert = cert

	return nil
}

// ApplyDefaults applies default values to zero-value fields.
// It sets HealthCheckInterval and HealthCheckTimeout if not specified.
func (s *Server) ApplyDefaults() {
	if s.HealthCheckInterval == 0 {
		s.HealthCheckInterval = DefaultHealthCheckInterval
	}
	if s.HealthCheckTimeout == 0 {
		s.HealthCheckTimeout = DefaultHealthCheckTimeout
	}
}
