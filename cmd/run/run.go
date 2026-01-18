package run

import (
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/tools"
	"github.com/spf13/cobra"
)

var (
	configFile = tools.GetenvDefault(config.EnvPrefix+"CONFIG", "config.yaml")
	Cmd        = &cobra.Command{
		Use:   "run",
		Short: "Run qmux server or client",
		Args:  cobra.NoArgs,
	}
)

func init() {
	Cmd.PersistentFlags().StringVarP(&configFile, "config", "c", configFile, "path of config file")
	Cmd.AddCommand(serverCmd)
	Cmd.AddCommand(clientCmd)
}
