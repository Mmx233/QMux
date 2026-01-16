package config

import (
	"github.com/spf13/cobra"
)

var (
	configFile string // --config flag value

	Cmd = &cobra.Command{
		Use:   "config",
		Short: "Generate configuration files",
		Args:  cobra.NoArgs,
	}
)

func init() {
	Cmd.PersistentFlags().StringVarP(&configFile, "config", "c", "config.yaml", "output config file path")
	Cmd.AddCommand(ServerCmd)
	Cmd.AddCommand(ClientCmd)
}

// GetConfigFile returns the value of the --config flag
func GetConfigFile() string {
	return configFile
}
