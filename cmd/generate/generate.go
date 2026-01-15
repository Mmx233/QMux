package generate

import (
	"github.com/Mmx233/QMux/cmd/generate/certs"
	"github.com/spf13/cobra"
)

var (
	Cmd = &cobra.Command{
		Use:   "generate",
		Short: "Generate various resources (certificates, etc.)",
		Args:  cobra.NoArgs,
	}
)

func init() {
	Cmd.AddCommand(certs.Cmd)
}
