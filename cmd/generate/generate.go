package generate

import (
	"github.com/Mmx233/QMux/cmd/generate/certs"
	"github.com/Mmx233/QMux/cmd/generate/config"
	"github.com/spf13/cobra"
)

var (
	Cmd = &cobra.Command{
		Use:   "generate",
		Short: "Generate resources",
		Args:  cobra.NoArgs,
	}
)

func init() {
	Cmd.AddCommand(certs.Cmd)
	Cmd.AddCommand(config.Cmd)
}
