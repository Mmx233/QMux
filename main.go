package main

import (
	"os"

	"github.com/Mmx233/QMux/cmd"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func init() {
	log.Logger = log.Output(zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: zerolog.TimeFormatUnix,
		NoColor:    false,
	})
}

func main() {
	cmd.Execute()
}
