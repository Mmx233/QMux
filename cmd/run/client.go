package run

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

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
)

func runClient(_ *cobra.Command, _ []string) error {
	logger := log.With().Str("com", "client-cmd").Logger()

	// Load configuration with validation
	logger.Info().Str("config", configFile).Msg("loading configuration")
	cfg, err := config.LoadClientConfig(configFile)
	if err != nil {
		return err
	}

	c, err := client.New(cfg)
	if err != nil {
		return err
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	} else {
		logger.Info().Msg("starting QMux client")
		err = c.Start(ctx)
	}

	if cause := context.Cause(ctx); cause != nil && errors.Is(err, cause) {
		err = nil
	}
	if err != nil {
		return err
	}

	logger.Info().Msg("client stopped")
	return nil
}
