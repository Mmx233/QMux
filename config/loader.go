package config

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

// LoadConfig reads exactly one YAML document into the specified type and rejects unknown fields.
func LoadConfig[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg T
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil && err != io.EOF {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		return nil, fmt.Errorf("parse config: multiple YAML documents are not allowed")
	}

	return &cfg, nil
}

// LoadServerConfig reads, defaults, and validates a server YAML configuration file.
func LoadServerConfig(path string) (*Server, error) {
	cfg, err := LoadConfig[Server](path)
	if err != nil {
		return nil, err
	}

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("server configuration validation failed: %w", err)
	}

	return cfg, nil
}

// LoadClientConfig reads a client YAML configuration file, validates it,
// and applies deduplication for multi-server configurations.
func LoadClientConfig(path string) (*Client, error) {
	logger := log.With().Str("com", "config-loader").Logger()

	cfg, err := LoadConfig[Client](path)
	if err != nil {
		return nil, err
	}

	cfg.ApplyDefaults()

	deduplicated, hasDuplicates := cfg.Server.DeduplicateServers()
	if hasDuplicates {
		cfg.Server.Servers = deduplicated
		logger.Warn().Msg("duplicate server addresses detected and removed from configuration")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("client configuration validation failed: %w", err)
	}

	servers := cfg.Server.GetServers()
	logger.Info().Int("server_count", len(servers)).Msg("loaded server configuration")

	return cfg, nil
}
