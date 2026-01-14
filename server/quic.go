package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	"github.com/Mmx233/QMux/config"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog/log"
)

func Run(conf *config.Server) error {
	logger := log.With().Str("com", "quic").Logger()

	ip, err := conf.Quic.GetIP()
	if err != nil {
		logger.Fatal().Err(err).Msg("start server failed")
	}

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   ip,
		Port: conf.Quic.Port,
	})
	if err != nil {
		return fmt.Errorf("listen udp failed: %w", err)
	}

	tlsConf := &tls.Config{
		Certificates:       nil,
		GetCertificate:     nil,
		GetConfigForClient: nil,
		ClientAuth:         tls.RequireAndVerifyClientCert,
		ClientCAs:          nil,
	}
	// tlsConf.SetSessionTicketKeys(stek.Keys.Load())
	// todo rotate
	quicConf := conf.Quic.GetConfig()

	tr := quic.Transport{
		Conn: udpConn,
	}
	ln, err := tr.Listen(tlsConf, quicConf)
	if err != nil {
		return fmt.Errorf("listen quic failed: %w", err)
	}

	for {
		conn, err := ln.Accept(context.Background())
		stream, err := conn.OpenStream()
		// ... error handling
		// handle the connection, usually in a new Go routine
	}
}
