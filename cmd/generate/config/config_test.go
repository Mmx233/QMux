package config

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/examples"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestServerConfigTemplateFields verifies that the embedded server.yaml template:
// - Parses into config.Server without unknown fields
// - Contains all required fields with valid values
// - Uses default values from config/defaults.go
// Validates: Requirements 4.1, 1.2, 1.3
func TestServerConfigTemplateFields(t *testing.T) {
	content, err := examples.ServerConfig()
	require.NoError(t, err, "failed to load server config template")
	assertCanonicalQUICKeys(t, string(content))
	assertUDPTemplateKeys(t, string(content))

	var cfg config.Server
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true) // Error on unknown fields
	err = decoder.Decode(&cfg)
	require.NoError(t, err, "server.yaml contains unknown fields or invalid YAML")

	// Verify listeners
	assert.NotEmpty(t, cfg.Listeners, "listeners should not be empty")
	assert.NotEmpty(t, cfg.Listeners[0].QuicAddr, "listener quic_addr should not be empty")
	assert.NotEmpty(t, cfg.Listeners[0].TrafficAddr, "traffic_addr should not be empty")
	assert.NotEmpty(t, cfg.Listeners[0].Protocol, "protocol should not be empty")
	assert.Equal(t, config.ListenerCapacity{
		MaxClientGenerations:             config.DefaultMaxClientGenerations,
		MaxPendingRegistrations:          config.DefaultMaxPendingRegistrations,
		MaxTCPConnections:                config.DefaultMaxTCPConnections,
		MaxPendingTCPSetups:              config.DefaultMaxPendingTCPSetups,
		MaxTCPConnectionsPerGeneration:   config.DefaultMaxTCPConnectionsPerGeneration,
		MaxPendingTCPSetupsPerGeneration: config.DefaultMaxPendingTCPSetupsPerGeneration,
		MaxUDPSessions:                   config.DefaultMaxUDPSessions,
		MaxUDPSessionsPerGeneration:      config.DefaultMaxUDPSessionsPerGeneration,
	}, cfg.Listeners[0].Capacity)

	// Verify auth
	assert.NotEmpty(t, cfg.Auth.Method, "auth method should not be empty")
	if cfg.Auth.Method == "mtls" || cfg.Auth.Method == "" {
		assert.NotEmpty(t, cfg.Auth.CACertFile, "auth.ca_cert_file should not be empty for mTLS auth")
	}

	// Verify TLS
	assert.NotEmpty(t, cfg.TLS.ServerCertFile, "TLS server cert file should not be empty")
	assert.NotEmpty(t, cfg.TLS.ServerKeyFile, "TLS server key file should not be empty")

	// Verify defaults match config/defaults.go
	assert.Equal(t, config.DefaultHeartbeatInterval, cfg.HeartbeatInterval,
		"heartbeat_interval should match DefaultHeartbeatInterval")
	assert.Equal(t, config.DefaultHealthTimeout, cfg.HealthTimeout,
		"health_timeout should match DefaultHealthTimeout")

	uncommented := decodeUncommentedUDP[config.Server](t, string(content))
	require.NotNil(t, uncommented.Listeners[0].UDP.EnableFragmentation)
	assert.True(t, *uncommented.Listeners[0].UDP.EnableFragmentation)
}

// TestClientConfigTemplateFields verifies that the embedded client.yaml template:
// - Parses into config.Client without unknown fields
// - Contains all required fields with valid values
// - Uses default values from config/defaults.go
// Validates: Requirements 4.1, 2.2, 2.3
func TestClientConfigTemplateFields(t *testing.T) {
	content, err := examples.ClientConfig()
	require.NoError(t, err, "failed to load client config template")
	assertCanonicalQUICKeys(t, string(content))
	assertUDPTemplateKeys(t, string(content))

	var cfg config.Client
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true) // Error on unknown fields
	err = decoder.Decode(&cfg)
	require.NoError(t, err, "client.yaml contains unknown fields or invalid YAML")

	// Verify server endpoints
	assert.NotEmpty(t, cfg.Server.Servers, "server.servers should not be empty")
	assert.NotEmpty(t, cfg.Server.Servers[0].Address, "server address should not be empty")

	// Verify local service
	assert.NotEmpty(t, cfg.Local.Host, "local host should not be empty")
	assert.Greater(t, cfg.Local.Port, 0, "local port should be greater than 0")
	assert.Equal(t, config.ClientCapacity{MaxLocalUDPSessions: config.DefaultMaxLocalUDPSessions}, cfg.Capacity)

	// Verify TLS
	assert.NotEmpty(t, cfg.TLS.CACertFile, "TLS CA cert file should not be empty")
	assert.NotEmpty(t, cfg.TLS.ClientCertFile, "TLS client cert file should not be empty")
	assert.NotEmpty(t, cfg.TLS.ClientKeyFile, "TLS client key file should not be empty")

	// Verify defaults match config/defaults.go
	assert.Equal(t, config.DefaultHeartbeatInterval, cfg.HeartbeatInterval,
		"heartbeat_interval should match DefaultHeartbeatInterval")

	uncommented := decodeUncommentedUDP[config.Client](t, string(content))
	require.NotNil(t, uncommented.UDP.EnableFragmentation)
	assert.True(t, *uncommented.UDP.EnableFragmentation)
}

func assertCanonicalQUICKeys(t *testing.T, content string) {
	t.Helper()
	canonical := []string{
		"initial_stream_receive_window", "max_stream_receive_window",
		"initial_connection_receive_window", "max_connection_receive_window",
		"max_incoming_streams", "keep_alive_period", "handshake_idle_timeout",
		"max_idle_timeout",
	}
	for _, key := range canonical {
		assert.Contains(t, content, key+":", "template should contain canonical QUIC key")
	}
}

func assertUDPTemplateKeys(t *testing.T, content string) {
	t.Helper()
	if !strings.Contains(content, "enable_fragmentation:") {
		t.Error("template should contain enable_fragmentation")
	}
}

func decodeUncommentedUDP[T any](t *testing.T, content string) T {
	t.Helper()
	const commented = "# enable_fragmentation: true"
	if strings.Count(content, commented) != 1 {
		t.Fatalf("template should contain one commented enable_fragmentation setting")
	}
	content = strings.Replace(content, commented, "enable_fragmentation: true", 1)
	var cfg T
	decoder := yaml.NewDecoder(strings.NewReader(content))
	decoder.KnownFields(true)
	require.NoError(t, decoder.Decode(&cfg), "uncommented enable_fragmentation should strict-decode")
	return cfg
}
