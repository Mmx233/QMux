package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Client represents the QMux client
type Client struct {
	config      *config.Client
	connMgr     *ConnectionManager
	udpHandlers sync.Map // serverAddr -> *UDPHandler
	localConns  sync.Map // connID -> net.Conn
	logger      zerolog.Logger
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// New creates a new client
func New(conf *config.Client) (*Client, error) {
	// Apply defaults to ensure all required fields have values
	conf.ApplyDefaults()

	logger := log.With().
		Str("com", "client").
		Str("client_id", conf.ClientID).
		Logger()

	// Load TLS certificates
	if err := conf.TLS.LoadCertificates(); err != nil {
		return nil, fmt.Errorf("load certificates: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Create connection manager
	connMgr, err := NewConnectionManager(conf, logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create connection manager: %w", err)
	}

	return &Client{
		config:  conf,
		connMgr: connMgr,
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
	}, nil
}

// Start starts the client
func (c *Client) Start(ctx context.Context) error {
	servers := c.config.Server.GetServers()
	serverAddrs := make([]string, len(servers))
	for i, s := range servers {
		serverAddrs[i] = s.Address
	}

	c.logger.Info().
		Strs("servers", serverAddrs).
		Str("local", fmt.Sprintf("%s:%d", c.config.Local.Host, c.config.Local.Port)).
		Msg("starting client")

	// Start connection manager (handles connecting to all servers)
	if err := c.connMgr.Start(ctx); err != nil {
		return fmt.Errorf("start connection manager: %w", err)
	}

	// Accept streams from all server connections
	c.startAcceptingStreams(ctx)

	c.logger.Info().
		Int("healthy", c.connMgr.HealthyCount()).
		Int("total", c.connMgr.TotalCount()).
		Msg("client started successfully")

	// Wait for context cancellation
	<-ctx.Done()
	c.logger.Info().Msg("client shutting down")

	return c.shutdown()
}

// startAcceptingStreams starts goroutines to accept streams and datagrams from all connections
func (c *Client) startAcceptingStreams(ctx context.Context) {
	// Get all connections and start accepting streams from each
	connections := c.connMgr.GetAllConnections()
	for _, sc := range connections {
		c.wg.Add(1)
		go c.acceptStreamsFromConnection(ctx, sc)

		// Start UDP datagram handler for this connection
		if sc.Connection() != nil {
			udpHandler := NewUDPHandler(
				c.config.Local.Host,
				c.config.Local.Port,
				c.config.UDP.IsFragmentationEnabled(),
				c.logger,
			)
			c.udpHandlers.Store(sc.ServerAddr(), udpHandler)
			udpHandler.Start(ctx, *sc.Connection())
		}
	}
}

// acceptStreamsFromConnection accepts incoming streams from a specific server connection
func (c *Client) acceptStreamsFromConnection(ctx context.Context, sc *ServerConnection) {
	defer c.wg.Done()

	logger := c.logger.With().Str("server", sc.ServerAddr()).Logger()

	for {
		stream, err := sc.AcceptStream(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Connection may have been closed or become unhealthy
			if !sc.IsHealthy() {
				logger.Debug().Msg("stopping stream acceptance - connection unhealthy")
				return
			}
			logger.Error().Err(err).Msg("accept stream failed")
			return
		}

		go c.handleStream(ctx, stream, sc)
	}
}

// handleStream handles a single stream from server
func (c *Client) handleStream(ctx context.Context, stream *quic.Stream, sc *ServerConnection) {
	defer stream.Close()

	// Read NewConn message
	var msg protocol.NewConnMsg
	if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeNewConn, &msg); err != nil {
		c.logger.Error().Err(err).Str("server", sc.ServerAddr()).Msg("read NewConn message failed")
		return
	}

	logger := c.logger.With().
		Uint64("conn_id", msg.ConnID).
		Str("protocol", msg.Protocol).
		Str("source", msg.SourceAddr).
		Uint16("dest_port", msg.DestPort).
		Str("server", sc.ServerAddr()).
		Logger()

	logger.Info().Msg("new connection from server")

	// Connect to local service
	localAddr := fmt.Sprintf("%s:%d", c.config.Local.Host, c.config.Local.Port)
	localConn, err := net.DialTimeout(msg.Protocol, localAddr, 5*time.Second)
	if err != nil {
		logger.Error().Err(err).Str("local_addr", localAddr).Msg("dial local service failed")
		protocol.WriteConnClose(stream, msg.ConnID, fmt.Sprintf("dial failed: %v", err))
		return
	}
	defer localConn.Close()

	// Optimize TCP connection
	if tc, ok := localConn.(*net.TCPConn); ok {
		if err := tc.SetNoDelay(true); err != nil {
			logger.Warn().Err(err).Msg("set TCP_NODELAY failed")
		}
		if err := tc.SetReadBuffer(512 * 1024); err != nil {
			logger.Warn().Err(err).Msg("set read buffer failed")
		}
		if err := tc.SetWriteBuffer(512 * 1024); err != nil {
			logger.Warn().Err(err).Msg("set write buffer failed")
		}
	}

	c.localConns.Store(msg.ConnID, localConn)
	defer c.localConns.Delete(msg.ConnID)

	logger.Info().Str("local_addr", localAddr).Msg("connected to local service")

	// Use optimized relay
	err = protocol.Relay(localConn, stream)
	if err != nil && !errors.Is(err, io.EOF) {
		logger.Debug().Err(err).Msg("connection closed with error")
	} else {
		logger.Debug().Msg("connection closed")
	}

	// Send close message
	protocol.WriteConnClose(stream, msg.ConnID, "closed")
}

// shutdown gracefully shuts down the client
func (c *Client) shutdown() error {
	c.cancel()

	// Stop all UDP handlers
	c.udpHandlers.Range(func(key, value interface{}) bool {
		if handler, ok := value.(*UDPHandler); ok {
			handler.Stop()
		}
		return true
	})

	// Close all local connections
	c.localConns.Range(func(key, value interface{}) bool {
		if conn, ok := value.(net.Conn); ok {
			conn.Close()
		}
		return true
	})

	// Wait for stream handlers to finish
	c.wg.Wait()

	// Stop connection manager (closes all server connections)
	if c.connMgr != nil {
		if err := c.connMgr.Stop(); err != nil {
			c.logger.Error().Err(err).Msg("error stopping connection manager")
		}
	}

	c.logger.Info().Msg("client shutdown complete")
	return nil
}

// Stop stops the client
func (c *Client) Stop() error {
	return c.shutdown()
}

// GetHealthyConnection returns a healthy server connection using load balancing.
// This can be used for outgoing stream selection.
func (c *Client) GetHealthyConnection() (*ServerConnection, error) {
	return c.connMgr.GetHealthyConnection()
}

// HealthyConnectionCount returns the number of healthy server connections.
func (c *Client) HealthyConnectionCount() int {
	return c.connMgr.HealthyCount()
}

// TotalConnectionCount returns the total number of server connections.
func (c *Client) TotalConnectionCount() int {
	return c.connMgr.TotalCount()
}

// ConnectionManager returns the underlying connection manager.
// This is useful for advanced use cases and testing.
func (c *Client) ConnectionManager() *ConnectionManager {
	return c.connMgr
}
