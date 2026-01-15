package run

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/config"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	clientCmd = &cobra.Command{
		Use:   "client",
		Short: "Start client",
		Args:  cobra.NoArgs,
		RunE:  runClient,
	}

	reconnectDelay    = 5 * time.Second
	maxReconnectDelay = 60 * time.Second
)

func runClient(cmd *cobra.Command, args []string) error {
	logger := log.With().Str("com", "client-cmd").Logger()

	// Load configuration with validation
	logger.Info().Str("config", configFile).Msg("loading configuration")
	cfg, err := config.LoadClientConfig(configFile)
	if err != nil {
		return err
	}

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start client with automatic reconnection (for K8s HA)
	go func() {
		currentDelay := reconnectDelay
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			logger.Info().Msg("starting QMux client")

			// Create new client instance
			c, err := client.New(cfg)
			if err != nil {
				logger.Error().Err(err).Msg("failed to create client")
				time.Sleep(currentDelay)
				// Exponential backoff
				currentDelay *= 2
				if currentDelay > maxReconnectDelay {
					currentDelay = maxReconnectDelay
				}
				continue
			}

			// Start client
			if err := c.Start(ctx); err != nil {
				if ctx.Err() != nil {
					// Context cancelled, exit gracefully
					return
				}
				logger.Error().Err(err).Msg("client stopped with error, reconnecting")
				time.Sleep(currentDelay)
				// Exponential backoff
				currentDelay *= 2
				if currentDelay > maxReconnectDelay {
					currentDelay = maxReconnectDelay
				}
				continue
			}

			// Reset delay on successful connection
			currentDelay = reconnectDelay
		}
	}()

	// Wait for shutdown signal
	sig := <-sigCh
	logger.Info().Str("signal", sig.String()).Msg("received shutdown signal")
	cancel()

	// Give some time for graceful shutdown
	time.Sleep(time.Second)
	logger.Info().Msg("client stopped")
	return nil
}
