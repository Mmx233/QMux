package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testConfig is a simple struct for testing the generic loader
type testConfig struct {
	Name    string `yaml:"name"`
	Port    int    `yaml:"port"`
	Enabled bool   `yaml:"enabled"`
}

func TestLoadConfig_Success(t *testing.T) {
	// Create a temporary YAML file
	content := `name: test-service
port: 8080
enabled: true
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadConfig[testConfig](configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Name != "test-service" {
		t.Errorf("expected Name 'test-service', got '%s'", cfg.Name)
	}
	if cfg.Port != 8080 {
		t.Errorf("expected Port 8080, got %d", cfg.Port)
	}
	if !cfg.Enabled {
		t.Errorf("expected Enabled true, got false")
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig[testConfig]("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
	if !strings.Contains(err.Error(), "read config file") {
		t.Errorf("expected error to contain 'read config file', got: %v", err)
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	// Create a temporary file with invalid YAML
	content := `name: [invalid yaml
port: not closed`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := LoadConfig[testConfig](configPath)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("expected error to contain 'parse config', got: %v", err)
	}
}

func TestLoadConfigRejectsUnknownField(t *testing.T) {
	path := writeTestConfig(t, "name: test\nunknown: true\n")

	_, err := LoadConfig[testConfig](path)
	if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("LoadConfig error = %v, want unknown field error", err)
	}
}

func TestLoadConfigRejectsMultipleDocuments(t *testing.T) {
	path := writeTestConfig(t, "name: first\n---\nname: second\n")

	_, err := LoadConfig[testConfig](path)
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("LoadConfig error = %v, want multiple document error", err)
	}
}

func TestTypedLoadersAddSemanticValidation(t *testing.T) {
	clientPath := writeTestConfig(t, "{}\n")
	if _, err := LoadConfig[Client](clientPath); err != nil {
		t.Fatalf("generic client load: %v", err)
	}
	if _, err := LoadClientConfig(clientPath); err == nil || !strings.Contains(err.Error(), "server.servers:") {
		t.Fatalf("typed client load error = %v, want server.servers path", err)
	}

	serverPath := writeTestConfig(t, "{}\n")
	if _, err := LoadConfig[Server](serverPath); err != nil {
		t.Fatalf("generic server load: %v", err)
	}
	if _, err := LoadServerConfig(serverPath); err == nil || !strings.Contains(err.Error(), "listeners") {
		t.Fatalf("typed server load error = %v, want listeners path", err)
	}
}

func TestLoadClientConfigCanonicalQUIC(t *testing.T) {
	want := testQuicConfig()
	path := writeTestConfig(t, `client_id: test-client
server:
  servers:
    - address: "server.example.com:8443"
      server_name: "server.example.com"
local:
  host: "127.0.0.1"
  port: 8080
quic:
  initial_stream_receive_window: 1024
  max_stream_receive_window: 2048
  initial_connection_receive_window: 4096
  max_connection_receive_window: 8192
  max_incoming_streams: 42
  keep_alive_period: 13s
  handshake_idle_timeout: 7s
  max_idle_timeout: 31s
tls:
  ca_cert_file: "ca.pem"
  client_cert_file: "client.pem"
  client_key_file: "client-key.pem"
`)

	cfg, err := LoadClientConfig(path)
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}
	if cfg.Quic != want {
		t.Fatalf("QUIC config = %+v, want %+v", cfg.Quic, want)
	}
	assertQUICRuntime(t, cfg.Quic, want)

}

func TestLoadServerConfigCanonicalQUIC(t *testing.T) {
	want := testQuicConfig()
	path := writeTestConfig(t, `listeners:
  - quic_addr: "0.0.0.0:8443"
    traffic_addr: "0.0.0.0:8080"
    protocol: "both"
    initial_stream_receive_window: 1024
    max_stream_receive_window: 2048
    initial_connection_receive_window: 4096
    max_connection_receive_window: 8192
    max_incoming_streams: 42
    keep_alive_period: 13s
    handshake_idle_timeout: 7s
    max_idle_timeout: 31s
auth:
  method: token
  token: "0123456789abcdef"
tls:
  server_cert_file: "server.pem"
  server_key_file: "server-key.pem"
`)

	cfg, err := LoadServerConfig(path)
	if err != nil {
		t.Fatalf("LoadServerConfig: %v", err)
	}
	if len(cfg.Listeners) != 1 || cfg.Listeners[0].Quic != want {
		t.Fatalf("listeners = %+v, want one listener with QUIC %+v", cfg.Listeners, want)
	}
	assertQUICRuntime(t, cfg.Listeners[0].Quic, want)

}

func TestLoadConfigCapacity(t *testing.T) {
	serverPath := writeTestConfig(t, `listeners:
  - quic_addr: "127.0.0.1:8443"
    traffic_addr: "127.0.0.1:8080"
    protocol: tcp
    capacity:
      max_client_generations: 1
      max_pending_registrations: 2
      max_tcp_connections: 3
      max_pending_tcp_setups: 4
      max_tcp_connections_per_generation: 5
      max_pending_tcp_setups_per_generation: 6
      max_udp_sessions: 7
      max_udp_sessions_per_generation: 8
auth:
  method: token
  token: "0123456789abcdef"
tls:
  server_cert_file: "server.pem"
  server_key_file: "server-key.pem"
`)
	serverConfig, err := LoadServerConfig(serverPath)
	if err != nil {
		t.Fatalf("LoadServerConfig: %v", err)
	}
	wantServer := ListenerCapacity{
		MaxClientGenerations:             1,
		MaxPendingRegistrations:          2,
		MaxTCPConnections:                3,
		MaxPendingTCPSetups:              4,
		MaxTCPConnectionsPerGeneration:   5,
		MaxPendingTCPSetupsPerGeneration: 6,
		MaxUDPSessions:                   7,
		MaxUDPSessionsPerGeneration:      8,
	}
	if got := serverConfig.Listeners[0].Capacity; got != wantServer {
		t.Fatalf("server capacity = %+v, want %+v", got, wantServer)
	}

	clientPath := writeTestConfig(t, "capacity:\n  max_local_udp_sessions: 9\n")
	clientConfig, err := LoadConfig[Client](clientPath)
	if err != nil {
		t.Fatalf("LoadConfig[Client]: %v", err)
	}
	if got := clientConfig.Capacity.MaxLocalUDPSessions; got != 9 {
		t.Fatalf("client max local UDP sessions = %d, want 9", got)
	}

}

func TestLoadConfigRejectsNegativeCapacity(t *testing.T) {
	serverPath := writeTestConfig(t, `listeners:
  - capacity:
      max_udp_sessions: -1
`)
	if _, err := LoadServerConfig(serverPath); err == nil ||
		!strings.Contains(err.Error(), "listeners[0].capacity.max_udp_sessions must not be negative") {
		t.Fatalf("LoadServerConfig error = %v, want full capacity path", err)
	}

	clientPath := writeTestConfig(t, `client_id: test-client
server:
  servers:
    - address: "server.example.com:8443"
      server_name: "server.example.com"
local:
  host: "127.0.0.1"
  port: 8080
capacity:
  max_local_udp_sessions: -1
tls:
  ca_cert_file: "ca.pem"
  client_cert_file: "client.pem"
  client_key_file: "client-key.pem"
`)
	if _, err := LoadClientConfig(clientPath); err == nil ||
		!strings.Contains(err.Error(), "capacity.max_local_udp_sessions must not be negative") {
		t.Fatalf("LoadClientConfig error = %v, want full capacity path", err)
	}
}

func TestLoadConfigUDPFragmentation(t *testing.T) {
	client, err := LoadConfig[Client](writeTestConfig(t, "udp:\n  enable_fragmentation: false\n"))
	if err != nil {
		t.Fatalf("load client config: %v", err)
	}
	if client.UDP.EnableFragmentation == nil || *client.UDP.EnableFragmentation {
		t.Fatalf("client enable_fragmentation = %v", client.UDP.EnableFragmentation)
	}

	server, err := LoadConfig[Server](writeTestConfig(t, "listeners:\n  - udp:\n      enable_fragmentation: true\n"))
	if err != nil {
		t.Fatalf("load server config: %v", err)
	}
	if server.Listeners[0].UDP.EnableFragmentation == nil || !*server.Listeners[0].UDP.EnableFragmentation {
		t.Fatalf("server enable_fragmentation = %v", server.Listeners[0].UDP.EnableFragmentation)
	}

	var defaults UDPConfig
	if !defaults.IsFragmentationEnabled() {
		t.Fatal("omitted enable_fragmentation did not default to true")
	}
}

func TestLoadServerConfigSessionTicketRotationOverlap(t *testing.T) {
	tests := []struct {
		name        string
		interval    string
		overlap     string
		wantPresent bool
		wantLimit   uint8
		wantTotal   int
	}{
		{name: "omitted", interval: "  session_ticket_encryption_key_rotation_interval: 24h\n", wantLimit: 7, wantTotal: 8},
		{name: "null", interval: "  session_ticket_encryption_key_rotation_interval: 24h\n", overlap: "  session_ticket_encryption_key_rotation_overlap: null\n", wantLimit: 7, wantTotal: 8},
		{name: "zero", interval: "  session_ticket_encryption_key_rotation_interval: 24h\n", overlap: "  session_ticket_encryption_key_rotation_overlap: 0\n", wantPresent: true, wantLimit: 0, wantTotal: 1},
		{name: "one", interval: "  session_ticket_encryption_key_rotation_interval: 24h\n", overlap: "  session_ticket_encryption_key_rotation_overlap: 1\n", wantPresent: true, wantLimit: 1, wantTotal: 2},
		{name: "two", interval: "  session_ticket_encryption_key_rotation_interval: 24h\n", overlap: "  session_ticket_encryption_key_rotation_overlap: 2\n", wantPresent: true, wantLimit: 2, wantTotal: 3},
		{name: "six", interval: "  session_ticket_encryption_key_rotation_interval: 24h\n", overlap: "  session_ticket_encryption_key_rotation_overlap: 6\n", wantPresent: true, wantLimit: 6, wantTotal: 7},
		{name: "seven", interval: "  session_ticket_encryption_key_rotation_interval: 24h\n", overlap: "  session_ticket_encryption_key_rotation_overlap: 7\n", wantPresent: true, wantLimit: 7, wantTotal: 8},
		{name: "automatic omitted", wantLimit: 7},
		{name: "automatic null", overlap: "  session_ticket_encryption_key_rotation_overlap: null\n", wantLimit: 7},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeTestConfig(t, `listeners:
  - quic_addr: "127.0.0.1:8443"
    traffic_addr: "127.0.0.1:8080"
    protocol: tcp
auth:
  method: token
  token: "0123456789abcdef"
tls:
  server_cert_file: "server.pem"
  server_key_file: "server-key.pem"
`+test.interval+test.overlap)

			raw, err := LoadConfig[Server](path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			loaded, err := LoadServerConfig(path)
			if err != nil {
				t.Fatalf("LoadServerConfig: %v", err)
			}
			if got := raw.TLS.SessionTicketEncryptionKeyRotationOverlap != nil; got != test.wantPresent {
				t.Fatalf("raw overlap presence = %t, want %t", got, test.wantPresent)
			}
			if got := loaded.TLS.SessionTicketEncryptionKeyRotationOverlap != nil; got != test.wantPresent {
				t.Fatalf("typed overlap presence = %t, want %t", got, test.wantPresent)
			}
			if got := loaded.TLS.RotationOldKeyLimit(); got != test.wantLimit {
				t.Fatalf("old-key limit = %d, want %d", got, test.wantLimit)
			}
			if test.wantTotal != 0 && int(loaded.TLS.RotationOldKeyLimit())+1 != test.wantTotal {
				t.Fatalf("max total keys = %d, want %d", int(loaded.TLS.RotationOldKeyLimit())+1, test.wantTotal)
			}
		})
	}
}

func testQuicConfig() Quic {
	return Quic{
		InitialStreamReceiveWindow:     1024,
		MaxStreamReceiveWindow:         2048,
		InitialConnectionReceiveWindow: 4096,
		MaxConnectionReceiveWindow:     8192,
		MaxIncomingStreams:             42,
		KeepAlivePeriod:                13 * time.Second,
		HandshakeIdleTimeout:           7 * time.Second,
		MaxIdleTimeout:                 31 * time.Second,
	}
}

func assertQUICRuntime(t *testing.T, source, want Quic) {
	t.Helper()
	got := source.GetConfig()
	if got.InitialStreamReceiveWindow != want.InitialStreamReceiveWindow ||
		got.MaxStreamReceiveWindow != want.MaxStreamReceiveWindow ||
		got.InitialConnectionReceiveWindow != want.InitialConnectionReceiveWindow ||
		got.MaxConnectionReceiveWindow != want.MaxConnectionReceiveWindow ||
		got.MaxIncomingStreams != want.MaxIncomingStreams ||
		got.KeepAlivePeriod != want.KeepAlivePeriod ||
		got.HandshakeIdleTimeout != want.HandshakeIdleTimeout ||
		got.MaxIdleTimeout != want.MaxIdleTimeout {
		t.Fatalf("runtime QUIC config = %+v, want values from %+v", got, want)
	}
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}

// Test duplicate deduplication during load
// Validates: Requirements 6.4
func TestLoadClientConfig_DuplicateDeduplication(t *testing.T) {
	content := `client_id: test-client
server:
  servers:
    - address: "server1.example.com:8443"
      server_name: "server1.example.com"
    - address: "server2.example.com:8443"
      server_name: "server2.example.com"
    - address: "server1.example.com:8443"
      server_name: "shadow.example.com"
local:
  host: "127.0.0.1"
  port: 8080
tls:
  ca_cert_file: "ca.pem"
  client_cert_file: "client.pem"
  client_key_file: "client-key.pem"
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadClientConfig(configPath)
	if err != nil {
		t.Fatalf("LoadClientConfig failed: %v", err)
	}

	servers := cfg.Server.GetServers()
	if len(servers) != 2 {
		t.Fatalf("expected 2 servers after deduplication, got %d", len(servers))
	}
	if servers[0].ServerName != "server1.example.com" {
		t.Fatalf("duplicate owner = %q, want first server name", servers[0].ServerName)
	}

	// Verify unique addresses
	seen := make(map[string]bool)
	for _, s := range servers {
		if seen[s.Address] {
			t.Errorf("duplicate address found after deduplication: %s", s.Address)
		}
		seen[s.Address] = true
	}
}
