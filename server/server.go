package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/auth"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/Mmx233/QMux/server/tls/stek"
	"github.com/Mmx233/QMux/server/traffic"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Server represents the QMux server
type Server struct {
	config         *config.Server
	pools          map[int]*pool.ConnectionPool // quicPort -> pool
	trafficManager *traffic.Manager
	authenticator  auth.Auth
	logger         zerolog.Logger
}

// New creates a new server
func New(conf *config.Server) (*Server, error) {
	logger := log.With().Str("com", "server").Logger()

	// Load TLS certificates
	if err := conf.TLS.LoadCertificates(); err != nil {
		return nil, fmt.Errorf("load certificates: %w", err)
	}

	// Validate auth config
	if err := conf.Auth.Validate(); err != nil {
		return nil, fmt.Errorf("invalid auth config: %w", err)
	}

	// Create authenticator using factory
	authenticator, err := conf.Auth.CreateAuthenticator()
	if err != nil {
		return nil, fmt.Errorf("create authenticator: %w", err)
	}

	// Log the auth method being used
	method := conf.Auth.Method
	if method == "" {
		method = "mtls"
	}
	logger.Info().Str("method", method).Msg("authentication enabled")

	// Create connection pools for each listener
	pools := make(map[int]*pool.ConnectionPool)
	for _, listener := range conf.Listeners {
		balancer := pool.NewRoundRobinBalancer()
		p := pool.New(listener.Port, balancer, logger)

		// Configure health check intervals if specified
		if conf.HealthCheckInterval > 0 {
			p.SetHealthCheckInterval(conf.HealthCheckInterval)
		}
		if conf.HealthCheckTimeout > 0 {
			p.SetHealthCheckTimeout(conf.HealthCheckTimeout)
		}

		pools[listener.Port] = p
		logger.Info().
			Int("port", listener.Port).
			Str("balancer", balancer.Name()).
			Msg("created connection pool")
	}

	return &Server{
		config:        conf,
		pools:         pools,
		authenticator: authenticator,
		logger:        logger,
	}, nil
}

// Start starts the server
func Start(ctx context.Context, conf *config.Server) error {
	srv, err := New(conf)
	if err != nil {
		return err
	}

	// Start traffic manager
	srv.trafficManager = traffic.NewManager(conf, srv.pools, srv.logger)
	if err := srv.trafficManager.Start(ctx); err != nil {
		return fmt.Errorf("start traffic manager: %w", err)
	}

	// Start QUIC listeners
	errCh := make(chan error, len(conf.Listeners))
	for _, listenerConf := range conf.Listeners {
		go func(lc config.QuicListener) {
			if err := srv.startListener(ctx, lc); err != nil {
				errCh <- fmt.Errorf("listener on port %d: %w", lc.Port, err)
			}
		}(listenerConf)
	}

	// Wait for first error or context cancellation
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		srv.logger.Info().Msg("server shutting down")
		return ctx.Err()
	}
}

// startListener starts a QUIC listener
func (s *Server) startListener(ctx context.Context, listenerConf config.QuicListener) error {
	logger := s.logger.With().Int("port", listenerConf.Port).Logger()

	ip, err := listenerConf.GetIP()
	if err != nil {
		return fmt.Errorf("get IP: %w", err)
	}

	// Create UDP listener
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   ip,
		Port: listenerConf.Port,
	})
	if err != nil {
		return fmt.Errorf("listen UDP: %w", err)
	}
	defer udpConn.Close()

	// Initialize session ticket key rotation
	var stekManager *stek.RotateManager
	if s.config.TLS.SessionTicketEncryptionKeyRotationInterval > 0 {
		overlap := s.config.TLS.SessionTicketEncryptionKeyRotationOverlap
		if overlap == 0 {
			overlap = 2
		}

		stekManager, err = stek.NewRotateManager(
			s.config.TLS.SessionTicketEncryptionKeyRotationInterval,
			overlap,
		)
		if err != nil {
			return fmt.Errorf("initialize session ticket key rotation: %w", err)
		}

		logger.Info().
			Dur("rotation_interval", s.config.TLS.SessionTicketEncryptionKeyRotationInterval).
			Uint8("key_overlap", overlap).
			Msg("session ticket key rotation enabled")
	}

	// Configure TLS based on auth method
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{s.config.TLS.ServerCert},
	}

	// For mTLS, require and verify client certificates
	// For token auth, no client cert verification is needed
	if s.config.Auth.Method == "" || s.config.Auth.Method == "mtls" {
		tlsConf.ClientAuth = tls.RequireAndVerifyClientCert
		tlsConf.ClientCAs = s.config.Auth.CACertPool
	} else {
		// Token-based auth doesn't require client certificates
		tlsConf.ClientAuth = tls.NoClientCert
	}

	// Configure session ticket keys with automatic rotation
	if stekManager != nil {
		tlsConf.SetSessionTicketKeys(*stekManager.Keys.Load())
		tlsConf.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			cfg := tlsConf.Clone()
			cfg.SetSessionTicketKeys(*stekManager.Keys.Load())
			return cfg, nil
		}
	}

	// Get QUIC config
	quicConf := listenerConf.GetConfig()

	// Create QUIC transport
	tr := quic.Transport{
		Conn: udpConn,
	}

	ln, err := tr.Listen(tlsConf, quicConf)
	if err != nil {
		return fmt.Errorf("listen QUIC: %w", err)
	}
	defer ln.Close()

	// Start session ticket key rotation
	if stekManager != nil {
		stekManager.Start(ctx)
		defer stekManager.Stop()
	}

	logger.Info().
		Str("ip", ip.String()).
		Int("quic_port", listenerConf.Port).
		Int("traffic_port", listenerConf.TrafficPort).
		Str("protocol", listenerConf.Protocol).
		Msg("QUIC listener started")

	// Accept connections
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.Error().Err(err).Msg("accept connection failed")
			continue
		}

		go s.handleConnection(ctx, conn, listenerConf.Port)
	}
}

// handleConnection handles a new QUIC connection
func (s *Server) handleConnection(ctx context.Context, conn *quic.Conn, quicPort int) {
	logger := s.logger.With().
		Str("remote", conn.RemoteAddr().String()).
		Int("quic_port", quicPort).
		Logger()

	logger.Info().Msg("new connection")

	// Authenticate connection
	valid, err := s.authenticator.VerifyConn(ctx, conn)
	if err != nil || !valid {
		logger.Error().Err(err).Msg("authentication failed")
		conn.CloseWithError(1, "authentication failed")
		return
	}

	// Accept control stream (first stream from client)
	controlStream, err := conn.AcceptStream(ctx)
	if err != nil {
		logger.Error().Err(err).Msg("accept control stream failed")
		conn.CloseWithError(1, "control stream error")
		return
	}

	// Read registration message
	var regMsg protocol.RegisterMsg
	if err := protocol.ReadTypedMessage(controlStream, protocol.MsgTypeRegister, &regMsg); err != nil {
		logger.Error().Err(err).Msg("read registration failed")
		conn.CloseWithError(1, "registration error")
		return
	}

	logger = logger.With().
		Str("client_id", regMsg.ClientID).
		Str("version", regMsg.Version).
		Logger()

	logger.Info().
		Strs("capabilities", regMsg.Capabilities).
		Msg("client registered")

	// Create client connection
	clientConn := &pool.ClientConn{
		ID:            regMsg.ClientID,
		Conn:          conn,
		ControlStream: controlStream,
		RegisteredAt:  time.Now(),
		LastSeen:      time.Now(),
		Metadata: pool.ClientMetadata{
			Version:      regMsg.Version,
			Capabilities: regMsg.Capabilities,
		},
	}

	// Add to pool
	poolInst := s.pools[quicPort]
	if err := poolInst.Add(regMsg.ClientID, clientConn); err != nil {
		logger.Error().Err(err).Msg("add to pool failed")
		protocol.WriteRegisterAck(controlStream, false, err.Error())
		conn.CloseWithError(1, "pool error")
		return
	}

	// Send acknowledgment
	if err := protocol.WriteRegisterAck(controlStream, true, "registered"); err != nil {
		logger.Error().Err(err).Msg("send ack failed")
		poolInst.Remove(regMsg.ClientID)
		return
	}

	// Start heartbeat handler
	go s.handleHeartbeat(ctx, controlStream, regMsg.ClientID, quicPort)

	// Wait for connection to close
	<-conn.Context().Done()

	// Remove from pool
	poolInst.Remove(regMsg.ClientID)
	logger.Info().Msg("client disconnected")
}

// handleHeartbeat handles heartbeat messages
func (s *Server) handleHeartbeat(ctx context.Context, stream *quic.Stream, clientID string, quicPort int) {
	logger := s.logger.With().
		Str("client_id", clientID).
		Int("quic_port", quicPort).
		Logger()

	poolInst := s.pools[quicPort]

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Read message with timeout
		msgType, _, err := protocol.ReadMessage(stream)
		if err != nil {
			logger.Debug().Err(err).Msg("read heartbeat failed")
			return
		}

		if msgType == protocol.MsgTypeHeartbeat {
			poolInst.UpdateLastSeen(clientID)
			logger.Debug().Msg("heartbeat received")
		}
	}
}
