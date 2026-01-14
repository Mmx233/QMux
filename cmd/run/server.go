package run

import (
	"github.com/spf13/cobra"
)

var (
	serverCmd = &cobra.Command{
		Use:   "server",
		Short: "Start server",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			//logger := log.With().Str("com", "quic").Logger()
			//logger.Info().Msgf("listen on %s:%d", cfg.Quic.IP, cfg.Quic.Port)
			//logger.Info().Msgf("%+v", cfg)
			//// todo
		},
	}
)
