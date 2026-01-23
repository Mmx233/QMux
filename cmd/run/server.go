package run

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	serverCmd = &cobra.Command{
		Use:   "server",
		Short: "Start server",
		Args:  cobra.NoArgs,
		RunE:  runServer,
	}
)

func runServer(cmd *cobra.Command, args []string) error {
	logger := log.With().Str("com", "server-cmd").Logger()

	// Load configuration
	logger.Info().Str("config", configFile).Msg("loading configuration")
	cfg, err := config.LoadServerConfig(configFile)
	if err != nil {
		return err
	}

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start server in goroutine
	errCh := make(chan error, 1)
	go func() {
		logger.Info().Msg("starting QMux server")
		if err := server.Start(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
		}
	}()

	// Wait for signal or error
	select {
	case sig := <-sigCh:
		logger.Info().Str("signal", sig.String()).Msg("received shutdown signal")
		cancel()
	case err := <-errCh:
		logger.Error().Err(err).Msg("server error")
		return err
	}

	logger.Info().Msg("server stopped")
	return nil
}
