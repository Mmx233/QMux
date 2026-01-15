package cmd

import (
	"fmt"

	"github.com/Mmx233/QMux/cmd/generate"
	"github.com/Mmx233/QMux/cmd/run"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	Version = "dev"

	showVersion bool

	rootCmd = &cobra.Command{
		Use:   "qmux",
		Short: "A high available NAT traversal tool base on QUIC protocol",
		Args:  cobra.NoArgs,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		Run: func(cmd *cobra.Command, args []string) {
			if showVersion {
				fmt.Println(Version)
				return
			}
			cmd.Help()
		},
	}
)

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatal().Err(err).Msg("failed to execute")
	}
}

func init() {
	rootCmd.Flags().BoolVarP(&showVersion, "version", "v", false, "Print version information")
	rootCmd.AddCommand(run.Cmd)
	rootCmd.AddCommand(generate.Cmd)
}
