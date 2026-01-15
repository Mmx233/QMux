package config

import (
	"fmt"
	"os"

	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

// LoadConfig reads a YAML configuration file and unmarshals it into the specified type.
// T must be a struct type that can be unmarshaled from YAML.
func LoadConfig[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg T
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	return &cfg, nil
}

// LoadClientConfig reads a client YAML configuration file, validates it,
// and applies deduplication for multi-server configurations.
// It handles both single `address` and `servers` array configurations.
func LoadClientConfig(path string) (*Client, error) {
	logger := log.With().Str("com", "config-loader").Logger()

	cfg, err := LoadConfig[Client](path)
	if err != nil {
		return nil, err
	}

	// Validate and deduplicate server configuration
	hasDuplicates, err := cfg.Server.ValidateAndDeduplicate()
	if err != nil {
		return nil, fmt.Errorf("server configuration validation failed: %w", err)
	}

	if hasDuplicates {
		logger.Warn().Msg("duplicate server addresses detected and removed from configuration")
	}

	servers := cfg.Server.GetServers()
	logger.Info().Int("server_count", len(servers)).Msg("loaded server configuration")

	return cfg, nil
}
