package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Mmx233/QMux/server/auth"
	"github.com/Mmx233/QMux/server/auth/challenge"
	"github.com/Mmx233/QMux/server/auth/mtls"
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
	TrafficPort int       `yaml:"traffic_port"` // 1:1 mapped traffic port for this listener
	Protocol    string    `yaml:"protocol"`     // "tcp", "udp", or "both"
	UDP         UDPConfig `yaml:"udp"`          // UDP-specific configuration
}

type ServerAuth struct {
	Method     string `yaml:"method"`       // "mtls", "token", etc.
	CACertFile string `yaml:"ca_cert_file"` // Path to CA certificate file (for mTLS)
	Token      string `yaml:"token"`        // Token for challenge-response auth

	// Loaded certificate (not from YAML)
	CACertPool *x509.CertPool `yaml:"-"`
}

// LoadCACertificate loads the CA certificate from file into the CACertPool
func (a *ServerAuth) LoadCACertificate() error {
	caCertPEM, err := os.ReadFile(a.CACertFile)
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}

	a.CACertPool = x509.NewCertPool()
	if !a.CACertPool.AppendCertsFromPEM(caCertPEM) {
		return fmt.Errorf("failed to parse CA certificate")
	}
	return nil
}

// Validate validates the auth configuration based on the selected method.
// It defaults to "mtls" when Method is empty.
// For mTLS: requires non-empty CACertFile.
// For token: requires non-empty token with minimum 16 bytes length.
// Returns an error for unknown auth methods.
func (a *ServerAuth) Validate() error {
	switch a.Method {
	case "", "mtls":
		if a.CACertFile == "" {
			return errors.New("ca_cert_file is required for mTLS authentication")
		}
		return nil
	case "token":
		if a.Token == "" {
			return errors.New("token is required for token authentication")
		}
		if len(a.Token) < challenge.MinTokenSize {
			return fmt.Errorf("token must be at least %d bytes", challenge.MinTokenSize)
		}
		return nil
	default:
		return fmt.Errorf("unknown auth method: %s", a.Method)
	}
}

// CreateAuthenticator creates and returns the appropriate authenticator based on the configured method.
// For mTLS (or empty method): loads the CA certificate and creates an mTLS authenticator.
// For token method: creates a challenge-response authenticator with the configured token.
// Returns an error if authenticator creation fails.
func (a *ServerAuth) CreateAuthenticator() (auth.Auth, error) {
	switch a.Method {
	case "", "mtls":
		if err := a.LoadCACertificate(); err != nil {
			return nil, fmt.Errorf("load CA certificate: %w", err)
		}
		return mtls.New(a.CACertPool), nil
	case "token":
		return challenge.New([]byte(a.Token))
	default:
		return nil, fmt.Errorf("unknown auth method: %s", a.Method)
	}
}

type ServerTLS struct {
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

	// Loaded certificate (not from YAML)
	ServerCert tls.Certificate `yaml:"-"`
}

// LoadCertificates loads server TLS certificate and key from files
func (t *ServerTLS) LoadCertificates() error {
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
