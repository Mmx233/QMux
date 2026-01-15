package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

type Client struct {
	ClientID          string        `yaml:"client_id"`
	Server            ClientServer  `yaml:"server"`
	Local             LocalService  `yaml:"local"`
	Quic              Quic          `yaml:"quic"`
	TLS               ClientTLS     `yaml:"tls"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"` // Heartbeat interval, default 30s
}

// ServerEndpoint represents a single server endpoint
type ServerEndpoint struct {
	Address    string `yaml:"address"`     // host:port
	ServerName string `yaml:"server_name"` // TLS server name for verification
}

type ClientServer struct {
	Servers []ServerEndpoint `yaml:"servers"`
}

// GetServers returns all configured server endpoints.
func (cs *ClientServer) GetServers() []ServerEndpoint {
	return cs.Servers
}

type LocalService struct {
	Host string `yaml:"host"` // localhost or 127.0.0.1
	Port int    `yaml:"port"` // local service port
}

type ClientTLS struct {
	CACertFile     string `yaml:"ca_cert_file"`
	ClientCertFile string `yaml:"client_cert_file"`
	ClientKeyFile  string `yaml:"client_key_file"`

	// Loaded certificates (not from YAML)
	CACertPool *x509.CertPool  `yaml:"-"`
	ClientCert tls.Certificate `yaml:"-"`
}

// LoadCertificates loads TLS certificates from files
func (t *ClientTLS) LoadCertificates() error {
	// Load CA certificate
	caCertPEM, err := os.ReadFile(t.CACertFile)
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}

	t.CACertPool = x509.NewCertPool()
	if !t.CACertPool.AppendCertsFromPEM(caCertPEM) {
		return fmt.Errorf("failed to parse CA certificate")
	}

	// Load client certificate and key
	cert, err := tls.LoadX509KeyPair(t.ClientCertFile, t.ClientKeyFile)
	if err != nil {
		return fmt.Errorf("load client cert/key: %w", err)
	}
	t.ClientCert = cert

	return nil
}

const (
	MinServers = 1
	MaxServers = 10
)

// ValidateAddress validates that an address is in valid host:port format.
// Returns an error if the address is invalid.
func ValidateAddress(addr string) error {
	if addr == "" {
		return fmt.Errorf("address cannot be empty")
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address format %q: %w", addr, err)
	}

	if host == "" {
		return fmt.Errorf("host cannot be empty in address %q", addr)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("invalid port in address %q: %w", addr, err)
	}

	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d in address %q", port, addr)
	}

	return nil
}

// Validate validates the ClientServer configuration.
// It checks server count, address format, and deduplicates servers.
func (cs *ClientServer) Validate() error {
	servers := cs.GetServers()

	// Validate server count
	if len(servers) < MinServers {
		return fmt.Errorf("at least %d server address must be provided", MinServers)
	}
	if len(servers) > MaxServers {
		return fmt.Errorf("maximum %d server addresses allowed, got %d", MaxServers, len(servers))
	}

	// Validate each server address
	for i, server := range servers {
		if err := ValidateAddress(server.Address); err != nil {
			return fmt.Errorf("server[%d]: %w", i, err)
		}
	}

	return nil
}

// DeduplicateServers removes duplicate server addresses from the configuration.
// It returns the deduplicated list and a boolean indicating if duplicates were found.
func (cs *ClientServer) DeduplicateServers() ([]ServerEndpoint, bool) {
	servers := cs.GetServers()
	if len(servers) == 0 {
		return nil, false
	}

	seen := make(map[string]bool)
	deduplicated := make([]ServerEndpoint, 0, len(servers))
	hasDuplicates := false

	for _, server := range servers {
		if !seen[server.Address] {
			seen[server.Address] = true
			deduplicated = append(deduplicated, server)
		} else {
			hasDuplicates = true
		}
	}

	return deduplicated, hasDuplicates
}

// ValidateAndDeduplicate validates the configuration and deduplicates servers.
// It updates the Servers field with deduplicated values if duplicates are found.
// Returns an error if validation fails, and a boolean indicating if duplicates were removed.
func (cs *ClientServer) ValidateAndDeduplicate() (bool, error) {
	// First deduplicate
	deduplicated, hasDuplicates := cs.DeduplicateServers()
	if hasDuplicates {
		cs.Servers = deduplicated
	}

	// Then validate
	if err := cs.Validate(); err != nil {
		return hasDuplicates, err
	}

	return hasDuplicates, nil
}
