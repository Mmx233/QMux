package config

import (
	"bytes"
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

	var cfg config.Server
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true) // Error on unknown fields
	err = decoder.Decode(&cfg)
	require.NoError(t, err, "server.yaml contains unknown fields or invalid YAML")

	// Verify listeners
	assert.NotEmpty(t, cfg.Listeners, "listeners should not be empty")
	assert.NotEmpty(t, cfg.Listeners[0].IP, "listener IP should not be empty")
	assert.Greater(t, cfg.Listeners[0].Port, 0, "listener port should be greater than 0")
	assert.Greater(t, cfg.Listeners[0].TrafficPort, 0, "traffic port should be greater than 0")
	assert.NotEmpty(t, cfg.Listeners[0].Protocol, "protocol should not be empty")

	// Verify auth
	assert.NotEmpty(t, cfg.Auth.Method, "auth method should not be empty")

	// Verify TLS
	assert.NotEmpty(t, cfg.TLS.CACertFile, "TLS CA cert file should not be empty")
	assert.NotEmpty(t, cfg.TLS.ServerCertFile, "TLS server cert file should not be empty")
	assert.NotEmpty(t, cfg.TLS.ServerKeyFile, "TLS server key file should not be empty")

	// Verify defaults match config/defaults.go
	assert.Equal(t, config.DefaultHealthCheckInterval, cfg.HealthCheckInterval,
		"health_check_interval should match DefaultHealthCheckInterval")
	assert.Equal(t, config.DefaultHealthCheckTimeout, cfg.HealthCheckTimeout,
		"health_check_timeout should match DefaultHealthCheckTimeout")
}

// TestClientConfigTemplateFields verifies that the embedded client.yaml template:
// - Parses into config.Client without unknown fields
// - Contains all required fields with valid values
// - Uses default values from config/defaults.go
// Validates: Requirements 4.1, 2.2, 2.3
func TestClientConfigTemplateFields(t *testing.T) {
	content, err := examples.ClientConfig()
	require.NoError(t, err, "failed to load client config template")

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

	// Verify TLS
	assert.NotEmpty(t, cfg.TLS.CACertFile, "TLS CA cert file should not be empty")
	assert.NotEmpty(t, cfg.TLS.ClientCertFile, "TLS client cert file should not be empty")
	assert.NotEmpty(t, cfg.TLS.ClientKeyFile, "TLS client key file should not be empty")

	// Verify defaults match config/defaults.go
	assert.Equal(t, config.DefaultHeartbeatInterval, cfg.HeartbeatInterval,
		"heartbeat_interval should match DefaultHeartbeatInterval")
}
