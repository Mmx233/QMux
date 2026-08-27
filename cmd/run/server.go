package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

const (
	adminReadHeaderTimeout = 5 * time.Second
	adminShutdownTimeout   = 5 * time.Second
)

var (
	adminAddress string
	serverCmd    = &cobra.Command{
		Use:   "server",
		Short: "Start server",
		Args:  cobra.NoArgs,
		RunE:  runServer,
	}
)

func init() {
	serverCmd.Flags().StringVar(&adminAddress, "admin-address", "", "admin health listener address")
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
	err = runServerComponents(ctx, srv.Start, srv.Snapshot, adminAddress)
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
	snapshot func() server.Snapshot,
	adminAddr string,
) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}

	var adminServer *http.Server
	var adminListener net.Listener
	var err error
	if adminAddr != "" {
		adminListener, err = net.Listen("tcp", adminAddr)
		if err != nil {
			return fmt.Errorf("listen admin on %s: %w", adminAddr, err)
		}
		adminServer = &http.Server{
			Handler:           newAdminHandler(snapshot),
			ReadHeaderTimeout: adminReadHeaderTimeout,
		}
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
	if adminServer != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), adminShutdownTimeout)
		shutdownErr := adminServer.Shutdown(shutdownCtx)
		shutdownCancel()
		if shutdownErr != nil {
			unexpectedErr = errors.Join(unexpectedErr, fmt.Errorf("shutdown admin: %w", shutdownErr))
		}
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

func newAdminHandler(snapshot func() server.Snapshot) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthyz", func(w http.ResponseWriter, _ *http.Request) {
		writeAdminResponse(w, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if snapshot().Ready {
			writeAdminResponse(w, http.StatusOK, "ok\n")
			return
		}
		writeAdminResponse(w, http.StatusServiceUnavailable, "not ready\n")
	})
	return mux
}

func writeAdminResponse(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
