package cmd

import (
	"github.com/Mmx233/QMux/cmd/run"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	rootCmd = &cobra.Command{
		Use:   "qmux",
		Short: "A high available NAT traversal tool base on QUIC protocol",
		Args:  cobra.NoArgs,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}
)

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatal().Err(err).Msg("failed to execute")
	}
}

func init() {
	rootCmd.AddCommand(run.Cmd)
}
