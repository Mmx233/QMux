package config

import (
	"fmt"
	"os"

	"github.com/Mmx233/QMux/examples"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

// ClientCmd is the client subcommand for generating client configuration files
var ClientCmd = &cobra.Command{
	Use:   "client",
	Short: "Generate client configuration file",
	RunE:  runClientGenerate,
}

func runClientGenerate(cmd *cobra.Command, args []string) error {
	logger := log.With().Str("com", "generate").Logger()
	outputPath := GetConfigFile()

	// Check if file exists
	if _, err := os.Stat(outputPath); err == nil {
		return fmt.Errorf("file already exists: %s", outputPath)
	}

	// Load embedded template
	content, err := examples.ClientConfig()
	if err != nil {
		return fmt.Errorf("load client config template: %w", err)
	}

	// Write to file
	if err := os.WriteFile(outputPath, content, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	logger.Info().Str("file", outputPath).Msg("generated client configuration")
	return nil
}
