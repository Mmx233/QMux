package run

import (
	"github.com/spf13/cobra"
)

var (
	clientCmd = &cobra.Command{
		Use:   "client",
		Short: "Start client",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) {},
	}
)
