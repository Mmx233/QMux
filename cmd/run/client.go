package run

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/config"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

const clientShutdownTimeout = 30 * time.Second

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

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	logger.Info().Msg("starting QMux client")
	err = coordinateClientSignals(c.Start, c.Shutdown, c.Stop, signals, func() { signal.Stop(signals) })
	if err != nil {
		return err
	}

	logger.Info().Msg("client stopped")
	return nil
}

func coordinateClientSignals(
	start func(context.Context) error,
	shutdown func(context.Context) error,
	stop func() error,
	signals <-chan os.Signal,
	stopSignals func(),
) error {
	startCtx, cancelStart := context.WithCancel(context.Background())
	defer cancelStart()
	startDone := make(chan error, 1)
	go func() { startDone <- start(startCtx) }()

	var shutdownDone chan error
	var startErr error
	for {
		select {
		case err := <-startDone:
			startDone = nil
			startErr = err
			if shutdownDone == nil {
				return err
			}
		case <-signals:
			if shutdownDone == nil {
				shutdownDone = make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), clientShutdownTimeout)
					defer cancel()
					shutdownDone <- shutdown(ctx)
				}()
				continue
			}
			stopSignals()
			stopErr := stop()
			<-shutdownDone
			if startDone != nil {
				startErr = <-startDone
			}
			startErr = signalStartError(startErr)
			if stopErr != nil {
				return stopErr
			}
			return startErr
		case err := <-shutdownDone:
			shutdownDone = nil
			return finishClientSignalResult(err, signals, startDone, startErr, stop, stopSignals)
		}
	}
}

func finishClientSignalResult(
	shutdownErr error,
	signals <-chan os.Signal,
	startDone <-chan error,
	startErr error,
	stop func() error,
	stopSignals func(),
) error {
	select {
	case <-signals:
		stopSignals()
		stopErr := stop()
		if startDone != nil {
			startErr = <-startDone
		}
		startErr = signalStartError(startErr)
		if stopErr != nil {
			return stopErr
		}
		return startErr
	default:
	}
	if startDone != nil {
		startErr = <-startDone
	}
	return errors.Join(shutdownErr, signalStartError(startErr))
}

func signalStartError(err error) error {
	// Only the bare Stop sentinel is expected after a signal; joined startup errors survive.
	//goland:noinspection GoDirectComparisonOfErrors
	if err == client.ErrClientStopped {
		return nil
	}
	return err
}
