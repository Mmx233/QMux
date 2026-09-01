package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
	"github.com/Mmx233/QMux/server/auth"
	"github.com/Mmx233/QMux/server/auth/mtls"
	"github.com/Mmx233/QMux/server/auth/tokenauth"
)

type Server struct {
	Listeners []QuicListener `yaml:"listeners"`
	Auth      ServerAuth     `yaml:"auth"`
	TLS       ServerTLS      `yaml:"tls"`

	// Load balancer algorithm: "least-connections" (default) or "round-robin"
	LoadBalancer string `yaml:"load_balancer"`

	// Heartbeat configuration
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"` // Interval between server heartbeats to clients, default 10s
	HealthTimeout     time.Duration `yaml:"health_timeout"`     // Time without heartbeat before marking unhealthy, default 30s
}

type QuicListener struct {
	QuicAddr    string           `yaml:"quic_addr"`    // Address for QUIC control connections (e.g., "0.0.0.0:8443")
	TrafficAddr string           `yaml:"traffic_addr"` // Address for forwarded traffic (e.g., "0.0.0.0:8080")
	Protocol    string           `yaml:"protocol"`     // "tcp", "udp", or "both"
	Capacity    ListenerCapacity `yaml:"capacity"`
	Quic        `yaml:",inline"`
	UDP         UDPConfig `yaml:"udp"` // UDP-specific configuration
}

// ListenerCapacity bounds resources owned by one server listener.
type ListenerCapacity struct {
	MaxClientGenerations             int `yaml:"max_client_generations"`
	MaxPendingRegistrations          int `yaml:"max_pending_registrations"`
	MaxTCPConnections                int `yaml:"max_tcp_connections"`
	MaxPendingTCPSetups              int `yaml:"max_pending_tcp_setups"`
	MaxTCPConnectionsPerGeneration   int `yaml:"max_tcp_connections_per_generation"`
	MaxPendingTCPSetupsPerGeneration int `yaml:"max_pending_tcp_setups_per_generation"`
	MaxUDPSessions                   int `yaml:"max_udp_sessions"`
	MaxUDPSessionsPerGeneration      int `yaml:"max_udp_sessions_per_generation"`
}

// ApplyDefaults fills omitted or explicitly zero capacity limits.
//
//goland:noinspection GoMixedReceiverTypes
func (c *ListenerCapacity) ApplyDefaults() {
	if c.MaxClientGenerations == 0 {
		c.MaxClientGenerations = DefaultMaxClientGenerations
	}
	if c.MaxPendingRegistrations == 0 {
		c.MaxPendingRegistrations = DefaultMaxPendingRegistrations
	}
	if c.MaxTCPConnections == 0 {
		c.MaxTCPConnections = DefaultMaxTCPConnections
	}
	if c.MaxPendingTCPSetups == 0 {
		c.MaxPendingTCPSetups = DefaultMaxPendingTCPSetups
	}
	if c.MaxTCPConnectionsPerGeneration == 0 {
		c.MaxTCPConnectionsPerGeneration = DefaultMaxTCPConnectionsPerGeneration
	}
	if c.MaxPendingTCPSetupsPerGeneration == 0 {
		c.MaxPendingTCPSetupsPerGeneration = DefaultMaxPendingTCPSetupsPerGeneration
	}
	if c.MaxUDPSessions == 0 {
		c.MaxUDPSessions = DefaultMaxUDPSessions
	}
	if c.MaxUDPSessionsPerGeneration == 0 {
		c.MaxUDPSessionsPerGeneration = DefaultMaxUDPSessionsPerGeneration
	}
}

// Validate rejects negative limits. Zero means use the default.
//
//goland:noinspection GoMixedReceiverTypes
func (c ListenerCapacity) Validate(path string) error {
	limits := []struct {
		name  string
		value int
	}{
		{"max_client_generations", c.MaxClientGenerations},
		{"max_pending_registrations", c.MaxPendingRegistrations},
		{"max_tcp_connections", c.MaxTCPConnections},
		{"max_pending_tcp_setups", c.MaxPendingTCPSetups},
		{"max_tcp_connections_per_generation", c.MaxTCPConnectionsPerGeneration},
		{"max_pending_tcp_setups_per_generation", c.MaxPendingTCPSetupsPerGeneration},
		{"max_udp_sessions", c.MaxUDPSessions},
		{"max_udp_sessions_per_generation", c.MaxUDPSessionsPerGeneration},
	}
	for _, limit := range limits {
		if limit.value < 0 {
			return fmt.Errorf("%s.%s must not be negative", path, limit.name)
		}
	}
	return nil
}

type ServerAuth struct {
	Method     string `yaml:"method"`       // "mtls", "token", etc.
	CACertFile string `yaml:"ca_cert_file"` // Path to CA certificate file (for mTLS)
	Token      string `yaml:"token"`        // Secret for exporter-bound token auth

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
		if len(a.Token) < sharedtoken.MinSecretSize {
			return fmt.Errorf("token must be at least %d bytes", sharedtoken.MinSecretSize)
		}
		return nil
	default:
		return fmt.Errorf("unknown auth method: %s", a.Method)
	}
}

// CreateAuthenticator creates and returns the appropriate authenticator based on the configured method.
// For mTLS (or empty method): loads the CA certificate and creates an mTLS authenticator.
// For token method: creates an exporter-bound registration authenticator.
// Returns an error if authenticator creation fails.
func (a *ServerAuth) CreateAuthenticator() (auth.Auth, error) {
	switch a.Method {
	case "", "mtls":
		if err := a.LoadCACertificate(); err != nil {
			return nil, fmt.Errorf("load CA certificate: %w", err)
		}
		return mtls.New(a.CACertPool), nil
	case "token":
		return tokenauth.New([]byte(a.Token))
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
// It sets HeartbeatInterval and HealthTimeout if not specified.
func (s *Server) ApplyDefaults() {
	for i := range s.Listeners {
		s.Listeners[i].Capacity.ApplyDefaults()
	}
	if s.HeartbeatInterval == 0 {
		s.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if s.HealthTimeout == 0 {
		s.HealthTimeout = DefaultHealthTimeout
	}
	if s.LoadBalancer == "" {
		s.LoadBalancer = DefaultLoadBalancer
	}
}

// Validate validates the complete server configuration after defaults are applied.
func (s *Server) Validate() error {
	if len(s.Listeners) == 0 {
		return errors.New("listeners must contain at least one listener")
	}

	type socketClaim struct {
		network string
		address string
	}
	claims := make(map[socketClaim]string, len(s.Listeners)*2)
	claim := func(network, address, field string) error {
		key := socketClaim{network: network, address: address}
		if existing, ok := claims[key]; ok {
			return fmt.Errorf("%s conflicts with %s on %s socket %q", field, existing, network, address)
		}
		claims[key] = field
		return nil
	}

	for i, listener := range s.Listeners {
		path := fmt.Sprintf("listeners[%d]", i)
		if err := listener.Capacity.Validate(path + ".capacity"); err != nil {
			return err
		}
		if err := validateListenerAddress(listener.QuicAddr); err != nil {
			return fmt.Errorf("%s.quic_addr: %w", path, err)
		}
		if err := validateListenerAddress(listener.TrafficAddr); err != nil {
			return fmt.Errorf("%s.traffic_addr: %w", path, err)
		}
		switch listener.Protocol {
		case "tcp", "udp", "both":
		default:
			return fmt.Errorf("%s.protocol must be tcp, udp, or both", path)
		}
		if err := listener.Quic.Validate(path); err != nil {
			return err
		}

		quicField := path + ".quic_addr"
		if err := claim("udp", listener.QuicAddr, quicField); err != nil {
			return err
		}
		trafficField := path + ".traffic_addr"
		if listener.Protocol == "tcp" || listener.Protocol == "both" {
			if err := claim("tcp", listener.TrafficAddr, trafficField); err != nil {
				return err
			}
		}
		if listener.Protocol == "udp" || listener.Protocol == "both" {
			if err := claim("udp", listener.TrafficAddr, trafficField); err != nil {
				return err
			}
		}
	}

	switch s.LoadBalancer {
	case "least-connections", "round-robin":
	default:
		return fmt.Errorf("load_balancer must be least-connections or round-robin, got %q", s.LoadBalancer)
	}
	if s.HeartbeatInterval <= 0 {
		return errors.New("heartbeat_interval must be positive")
	}
	if s.HealthTimeout <= 0 {
		return errors.New("health_timeout must be positive")
	}
	if s.HealthTimeout <= s.HeartbeatInterval {
		return fmt.Errorf("health_timeout (%v) must be greater than heartbeat_interval (%v)", s.HealthTimeout, s.HeartbeatInterval)
	}
	if s.TLS.ServerCertFile == "" {
		return errors.New("tls.server_cert_file is required")
	}
	if s.TLS.ServerKeyFile == "" {
		return errors.New("tls.server_key_file is required")
	}
	if s.TLS.SessionTicketEncryptionKeyRotationInterval < 0 {
		return errors.New("tls.session_ticket_encryption_key_rotation_interval must not be negative")
	}
	if err := s.Auth.Validate(); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	return nil
}

func validateListenerAddress(address string) error {
	if address == "" {
		return errors.New("address cannot be empty")
	}
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid address format %q: %w", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return fmt.Errorf("invalid port in address %q: %w", address, err)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d in address %q", port, address)
	}
	return nil
}
