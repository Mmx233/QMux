package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// Property-based test for round-trip consistency
// Feature: config-load-refactor, Property 1: Config Round-Trip Consistency
// Validates: Requirements 1.2, 1.5

func TestLoadConfig_RoundTrip_Property(t *testing.T) {
	// Property: For any valid config struct, writing to YAML and loading back
	// should produce an equivalent struct.
	for i := 0; i < 100; i++ {
		// Generate random config values
		original := testConfig{
			Name:    randomString(i),
			Port:    (i * 17) % 65535, // Vary port values
			Enabled: i%2 == 0,
		}

		// Write to YAML file
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.yaml")

		yamlData, err := yaml.Marshal(&original)
		if err != nil {
			t.Fatalf("iteration %d: failed to marshal config: %v", i, err)
		}

		if err := os.WriteFile(configPath, yamlData, 0644); err != nil {
			t.Fatalf("iteration %d: failed to write config: %v", i, err)
		}

		// Load back using LoadConfig
		loaded, err := LoadConfig[testConfig](configPath)
		if err != nil {
			t.Fatalf("iteration %d: LoadConfig failed: %v", i, err)
		}

		// Verify equivalence
		if loaded.Name != original.Name {
			t.Errorf("iteration %d: Name mismatch: got %q, want %q", i, loaded.Name, original.Name)
		}
		if loaded.Port != original.Port {
			t.Errorf("iteration %d: Port mismatch: got %d, want %d", i, loaded.Port, original.Port)
		}
		if loaded.Enabled != original.Enabled {
			t.Errorf("iteration %d: Enabled mismatch: got %v, want %v", i, loaded.Enabled, original.Enabled)
		}
	}
}

// randomString generates a deterministic string based on seed for reproducibility
func randomString(seed int) string {
	chars := "abcdefghijklmnopqrstuvwxyz0123456789-_"
	length := (seed % 20) + 1
	result := make([]byte, length)
	for i := 0; i < length; i++ {
		result[i] = chars[(seed+i*7)%len(chars)]
	}
	return string(result)
}

func TestLoadClientConfig_SingleServer(t *testing.T) {
	content := `client_id: test-client
server:
  servers:
    - address: "server.example.com:8443"
      server_name: "server.example.com"
local:
  host: "127.0.0.1"
  port: 8080
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
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	if servers[0].Address != "server.example.com:8443" {
		t.Errorf("expected address 'server.example.com:8443', got %q", servers[0].Address)
	}
	if servers[0].ServerName != "server.example.com" {
		t.Errorf("expected server name 'server.example.com', got %q", servers[0].ServerName)
	}
}

// Test multi-server configuration loading
// Validates: Requirements 1.1
func TestLoadClientConfig_MultiServer(t *testing.T) {
	content := `client_id: test-client
server:
  servers:
    - address: "server1.example.com:8443"
      server_name: "server1.example.com"
    - address: "server2.example.com:8443"
      server_name: "server2.example.com"
    - address: "server3.example.com:8443"
      server_name: "server3.example.com"
local:
  host: "127.0.0.1"
  port: 8080
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
	if len(servers) != 3 {
		t.Fatalf("expected 3 servers, got %d", len(servers))
	}

	expectedAddresses := []string{
		"server1.example.com:8443",
		"server2.example.com:8443",
		"server3.example.com:8443",
	}
	for i, expected := range expectedAddresses {
		if servers[i].Address != expected {
			t.Errorf("server[%d]: expected address %q, got %q", i, expected, servers[i].Address)
		}
	}
}

// Test validation error for no servers configured
// Validates: Requirements 6.1
func TestLoadClientConfig_NoServers(t *testing.T) {
	content := `client_id: test-client
server: {}
local:
  host: "127.0.0.1"
  port: 8080
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := LoadClientConfig(configPath)
	if err == nil {
		t.Fatal("expected error for no servers configured, got nil")
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Errorf("expected error about minimum servers, got: %v", err)
	}
}

// Test validation error for too many servers
// Validates: Requirements 1.3
func TestLoadClientConfig_TooManyServers(t *testing.T) {
	// Create config with 11 servers (exceeds max of 10)
	content := `client_id: test-client
server:
  servers:
`
	for i := 0; i < 11; i++ {
		content += "    - address: \"server" + string(rune('a'+i)) + ".example.com:8443\"\n"
		content += "      server_name: \"server" + string(rune('a'+i)) + ".example.com\"\n"
	}
	content += `local:
  host: "127.0.0.1"
  port: 8080
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := LoadClientConfig(configPath)
	if err == nil {
		t.Fatal("expected error for too many servers, got nil")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Errorf("expected error about maximum servers, got: %v", err)
	}
}

// Test validation error for invalid address format
// Validates: Requirements 6.2, 6.3
func TestLoadClientConfig_InvalidAddress(t *testing.T) {
	content := `client_id: test-client
server:
  servers:
    - address: "invalid-no-port"
      server_name: "invalid"
local:
  host: "127.0.0.1"
  port: 8080
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := LoadClientConfig(configPath)
	if err == nil {
		t.Fatal("expected error for invalid address, got nil")
	}
	if !strings.Contains(err.Error(), "invalid address") {
		t.Errorf("expected error about invalid address, got: %v", err)
	}
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
      server_name: "server1.example.com"
local:
  host: "127.0.0.1"
  port: 8080
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

	// Verify unique addresses
	seen := make(map[string]bool)
	for _, s := range servers {
		if seen[s.Address] {
			t.Errorf("duplicate address found after deduplication: %s", s.Address)
		}
		seen[s.Address] = true
	}
}

// Test file not found error
func TestLoadClientConfig_FileNotFound(t *testing.T) {
	_, err := LoadClientConfig("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
	if !strings.Contains(err.Error(), "read config file") {
		t.Errorf("expected error to contain 'read config file', got: %v", err)
	}
}

// Test invalid YAML error
func TestLoadClientConfig_InvalidYAML(t *testing.T) {
	content := `client_id: [invalid yaml
server: not closed`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := LoadClientConfig(configPath)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("expected error to contain 'parse config', got: %v", err)
	}
}
