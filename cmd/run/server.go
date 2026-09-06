package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start server",
	Args:  cobra.NoArgs,
	RunE:  runServer,
}

func runServer(_ *cobra.Command, _ []string) error {
	logger := log.With().Str("com", "server-cmd").Logger()

	logger.Info().Str("config", configFile).Msg("loading configuration")
	cfg, err := config.LoadServerConfig(configFile)
	if err != nil {
		return err
	}
	srv, err := server.New(cfg)
	if err != nil {
		return err
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	logger.Info().Msg("starting QMux server")
	err = runServerComponents(ctx, srv.Start, srv.Ready, cfg.AdminAddress, newServerCollector(srv.Snapshot))
	if ctx.Err() != nil && errors.Is(err, context.Cause(ctx)) {
		err = nil
	}
	if err != nil {
		logger.Error().Err(err).Msg("server error")
		return err
	}
	logger.Info().Msg("server stopped")
	return nil
}

func runServerComponents(
	ctx context.Context,
	start func(context.Context) error,
	ready func() bool,
	adminAddr string,
	collector prometheus.Collector,
) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}

	adminServer, adminListener, err := newAdminServer(adminAddr, ready, collector)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	coreDone := make(chan error, 1)
	go func() { coreDone <- start(runCtx) }()

	var adminDone chan error
	var adminResult <-chan error
	if adminServer != nil {
		adminDone = make(chan error, 1)
		adminResult = adminDone
		go func() { adminDone <- adminServer.Serve(adminListener) }()
	}

	var result error
	var unexpectedErr error
	coreJoined := false
	adminJoined := false
	select {
	case <-ctx.Done():
		result = context.Cause(ctx)
	case err := <-coreDone:
		coreJoined = true
		if context.Cause(ctx) != nil {
			result = context.Cause(ctx)
			if err != nil && !errors.Is(err, context.Cause(ctx)) && !errors.Is(err, context.Canceled) {
				unexpectedErr = fmt.Errorf("server core: %w", err)
			}
		} else if err != nil {
			result = fmt.Errorf("server core: %w", err)
		}
	case err := <-adminResult:
		adminJoined = true
		if context.Cause(ctx) != nil {
			result = context.Cause(ctx)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				unexpectedErr = fmt.Errorf("serve admin: %w", err)
			}
		} else if err != nil && !errors.Is(err, http.ErrServerClosed) {
			result = fmt.Errorf("serve admin: %w", err)
		}
	}

	cancel()
	if shutdownErr := shutdownAdmin(adminServer); shutdownErr != nil {
		unexpectedErr = errors.Join(unexpectedErr, fmt.Errorf("shutdown admin: %w", shutdownErr))
	}

	if !coreJoined {
		if err := <-coreDone; err != nil && !errors.Is(err, context.Canceled) &&
			(context.Cause(ctx) == nil || !errors.Is(err, context.Cause(ctx))) {
			unexpectedErr = errors.Join(unexpectedErr, fmt.Errorf("server core: %w", err))
		}
	}
	if adminDone != nil && !adminJoined {
		if err := <-adminDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			unexpectedErr = errors.Join(unexpectedErr, fmt.Errorf("serve admin: %w", err))
		}
	}
	if unexpectedErr != nil {
		if cause := context.Cause(ctx); cause != nil && errors.Is(result, cause) {
			return unexpectedErr
		}
		return errors.Join(result, unexpectedErr)
	}
	return result
}
