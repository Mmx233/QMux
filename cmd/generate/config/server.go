package config

import (
	"fmt"
	"os"

	"github.com/Mmx233/QMux/examples"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

// ServerCmd is the server subcommand for generating server configuration files
var ServerCmd = &cobra.Command{
	Use:   "server",
	Short: "Generate server configuration file",
	RunE:  runServerGenerate,
}

func runServerGenerate(cmd *cobra.Command, args []string) error {
	logger := log.With().Str("com", "generate").Logger()
	outputPath := GetConfigFile()

	// Check if file exists
	if _, err := os.Stat(outputPath); err == nil {
		return fmt.Errorf("file already exists: %s", outputPath)
	}

	// Load embedded template
	content, err := examples.ServerConfig()
	if err != nil {
		return fmt.Errorf("load server config template: %w", err)
	}

	// Write to file
	if err := os.WriteFile(outputPath, content, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	logger.Info().Str("file", outputPath).Msg("generated server configuration")
	return nil
}
