package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var (
	// ErrClientAlreadyStarted is returned when Start is called more than once.
	ErrClientAlreadyStarted = errors.New("client already started")
	// ErrClientStopped is returned when Start is called after shutdown.
	ErrClientStopped = errors.New("client stopped")
)

// Client represents the QMux client
type Client struct {
	config      *config.Client
	connMgr     *ConnectionManager
	udpHandlers sync.Map // serverAddr -> *UDPHandler
	localConns  sync.Map // connID -> net.Conn
	udpBudget   *udpSessionBudget
	dsendStats  *clientDsendStats
	logger      zerolog.Logger

	udpMu            sync.Mutex
	liveUDPHandlers  map[*UDPHandler]struct{}
	retiredFragments protocol.FragmentSnapshot

	lifecycleMu  sync.Mutex
	started      bool
	stopped      bool
	runCancel    context.CancelFunc
	shutdownOnce sync.Once
	shutdownErr  error
	wg           sync.WaitGroup
}

// Snapshot is a value-only view of client capacity ownership.
type Snapshot struct {
	Endpoints      []EndpointSnapshot
	UDPSessions    UDPSessionSnapshot
	DSend          DSendSnapshot
	Fragments      protocol.FragmentSnapshot
	LiveAssemblers int
}

// EndpointSnapshot is one configured endpoint's generation phases.
type EndpointSnapshot struct {
	Endpoint            string
	Handshaking         int64
	Pending             int64
	Registered          int64
	Retiring            int64
	GenerationHighWater int64
	AccountingFaults    uint64
}

// UDPSessionSnapshot describes process-wide client UDP session admission.
type UDPSessionSnapshot struct {
	Current          int64
	Permits          int64
	HighWater        int64
	Limit            int64
	CapacityDrops    uint64
	AccountingFaults uint64
}

// DSendSnapshot describes client-owned application datagram sends.
type DSendSnapshot struct {
	OwnedItems            int64
	OwnedBacking          int64
	OwnedItemsHighWater   int64
	OwnedBackingHighWater int64
	Workers               int64
	SendErrors            uint64
	FragmentDrops         uint64
}

// New creates a new client
func New(conf *config.Client) (*Client, error) {
	if conf == nil {
		return nil, errors.New("client config is nil")
	}

	conf.ApplyDefaults()
	logger := log.With().
		Str("com", "client").
		Str("client_id", conf.ClientID).
		Logger()
	deduplicated, hasDuplicates := conf.Server.DeduplicateServers()
	if hasDuplicates {
		conf.Server.Servers = deduplicated
		logger.Warn().Msg("duplicate server addresses detected and removed")
	}
	if err := conf.Validate(); err != nil {
		return nil, fmt.Errorf("invalid client config: %w", err)
	}

	// Load the credentials required by the selected authentication mode.
	if err := conf.LoadCredentials(); err != nil {
		return nil, fmt.Errorf("load credentials: %w", err)
	}

	// Create connection manager
	connMgr, err := NewConnectionManager(conf, logger)
	if err != nil {
		return nil, fmt.Errorf("create connection manager: %w", err)
	}

	return &Client{
		config:          conf,
		connMgr:         connMgr,
		udpBudget:       newUDPSessionBudget(conf.Capacity.MaxLocalUDPSessions),
		dsendStats:      &clientDsendStats{},
		liveUDPHandlers: make(map[*UDPHandler]struct{}),
		logger:          logger,
	}, nil
}

// Start starts the client
func (c *Client) Start(ctx context.Context) error {
	c.lifecycleMu.Lock()
	if c.stopped {
		c.lifecycleMu.Unlock()
		return ErrClientStopped
	}
	if c.started {
		c.lifecycleMu.Unlock()
		return ErrClientAlreadyStarted
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.started = true
	c.runCancel = cancel
	c.wg.Go(func() {
		c.handleNewConnections(runCtx)
	})
	c.lifecycleMu.Unlock()

	servers := c.config.Server.GetServers()
	serverAddrs := make([]string, len(servers))
	for i, s := range servers {
		serverAddrs[i] = s.Address
	}

	c.logger.Info().
		Strs("servers", serverAddrs).
		Str("local", net.JoinHostPort(c.config.Local.Host, strconv.Itoa(c.config.Local.Port))).
		Msg("starting client")

	// Start connection manager (handles connecting to all servers)
	if err := c.connMgr.Start(runCtx); err != nil {
		_ = c.shutdown()
		return fmt.Errorf("start connection manager: %w", err)
	}

	c.logger.Info().
		Int("healthy", c.connMgr.HealthyCount()).
		Int("total", c.connMgr.TotalCount()).
		Msg("client started successfully")

	// Wait for caller cancellation or an external Stop.
	<-runCtx.Done()
	c.logger.Info().Msg("client shutting down")

	return c.shutdown()
}

// handleNewConnections listens on the connection manager's NewConns channel
// and starts stream acceptance and UDP handler for each new connection.
func (c *Client) handleNewConnections(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case sc, ok := <-c.connMgr.NewConns:
			if !ok {
				return
			}
			c.wg.Go(func() {
				c.acceptStreamsFromConnection(ctx, sc)
			})

			conn := sc.Connection()
			if conn != nil {
				endpoint := sc.ServerAddr()
				c.udpMu.Lock()
				if c.dsendStats == nil {
					c.dsendStats = &clientDsendStats{}
				}
				dsendStats := c.dsendStats
				c.udpMu.Unlock()
				udpHandler := newUDPHandler(
					c.config.Local.Host,
					c.config.Local.Port,
					c.config.UDP.IsFragmentationEnabled(),
					c.logger,
					c.udpBudget,
					dsendStats,
				)
				c.udpMu.Lock()
				if c.liveUDPHandlers == nil {
					c.liveUDPHandlers = make(map[*UDPHandler]struct{})
				}
				c.liveUDPHandlers[udpHandler] = struct{}{}
				udpHandler.Start(ctx, conn)
				previousI, replaced := c.udpHandlers.Swap(endpoint, udpHandler)
				c.udpMu.Unlock()
				go func() {
					<-udpHandler.done
					c.retireUDPHandler(endpoint, udpHandler)
				}()
				if replaced {
					previous := previousI.(*UDPHandler)
					if previous != udpHandler {
						previous.stopAndWait()
						c.retireUDPHandler(endpoint, previous)
					}
				}
			}
		}
	}
}

// acceptStreamsFromConnection accepts incoming streams from a specific server connection
func (c *Client) acceptStreamsFromConnection(ctx context.Context, sc *ServerConnection) {
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
	relayOwnsStream := false
	defer func() {
		if !relayOwnsStream {
			_ = stream.Close()
		}
	}()

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
		Str("dest_addr", msg.DestAddr).
		Str("server", sc.ServerAddr()).
		Logger()

	logger.Info().Msg("new connection from server")

	// Connect to local service
	localAddr := net.JoinHostPort(c.config.Local.Host, strconv.Itoa(c.config.Local.Port))
	localConn, err := net.DialTimeout(msg.Protocol, localAddr, 5*time.Second)
	if err != nil {
		logger.Error().Err(err).Str("local_addr", localAddr).Msg("dial local service failed")
		_ = protocol.WriteConnClose(stream, msg.ConnID, fmt.Sprintf("dial failed: %v", err))
		return
	}
	defer func() {
		_ = localConn.Close()
	}()

	tcpConn, ok := localConn.(*net.TCPConn)
	if !ok {
		logger.Error().Str("local_type", fmt.Sprintf("%T", localConn)).Msg("unsupported local connection")
		return
	}

	// Optimize TCP connection
	if err := tcpConn.SetNoDelay(true); err != nil {
		logger.Warn().Err(err).Msg("set TCP_NODELAY failed")
	}
	if err := tcpConn.SetReadBuffer(512 * 1024); err != nil {
		logger.Warn().Err(err).Msg("set read buffer failed")
	}
	if err := tcpConn.SetWriteBuffer(512 * 1024); err != nil {
		logger.Warn().Err(err).Msg("set write buffer failed")
	}

	c.localConns.Store(msg.ConnID, localConn)
	defer c.localConns.Delete(msg.ConnID)

	logger.Info().Str("local_addr", localAddr).Msg("connected to local service")

	var streamMu sync.Mutex
	aborted := false
	abort := sync.OnceFunc(func() {
		_ = localConn.Close()
		streamMu.Lock()
		aborted = true
		stream.CancelRead(0)
		stream.CancelWrite(0)
		streamMu.Unlock()
	})
	localToQUICComplete := func(copyErr error) error {
		if copyErr != nil {
			abort()
			return nil
		}
		streamMu.Lock()
		if !aborted {
			copyErr = stream.Close()
		}
		streamMu.Unlock()
		if copyErr != nil {
			abort()
		}
		return copyErr
	}
	quicToLocalComplete := func(copyErr error) error {
		if copyErr != nil {
			abort()
			return nil
		}
		copyErr = tcpConn.CloseWrite()
		if copyErr != nil {
			abort()
		}
		return copyErr
	}

	relayOwnsStream = true
	relay := protocol.StartRelay(localConn, stream, localToQUICComplete, quicToLocalComplete)
	stopAbort := context.AfterFunc(ctx, abort)
	err = relay.Wait()
	if !stopAbort() {
		abort()
	}
	if err != nil {
		logger.Debug().Err(err).Msg("connection closed with error")
	} else {
		logger.Debug().Msg("connection closed")
	}

	// Send close message
	_ = protocol.WriteConnClose(stream, msg.ConnID, "closed")
}

// shutdown gracefully shuts down the client
func (c *Client) shutdown() error {
	c.shutdownOnce.Do(func() {
		c.lifecycleMu.Lock()
		c.stopped = true
		cancel := c.runCancel
		c.lifecycleMu.Unlock()

		if cancel != nil {
			cancel()
		}

		// Join every tracked user of ServerConnection.conn before Stop clears it.
		c.wg.Wait()

		if c.connMgr != nil {
			if err := c.connMgr.Stop(); err != nil {
				c.logger.Error().Err(err).Msg("error stopping connection manager")
				c.shutdownErr = fmt.Errorf("stop connection manager: %w", err)
			}
		}

		c.udpHandlers.Range(func(key, value any) bool {
			if handler, ok := value.(*UDPHandler); ok {
				handler.Stop()
			}
			return true
		})
		c.udpHandlers.Range(func(key, value any) bool {
			if handler, ok := value.(*UDPHandler); ok {
				handler.wait()
				c.retireUDPHandler(key.(string), handler)
			}
			return true
		})

		c.localConns.Range(func(key, value any) bool {
			if conn, ok := value.(net.Conn); ok {
				_ = conn.Close()
			}
			return true
		})

		c.logger.Info().Msg("client shutdown complete")
	})

	return c.shutdownErr
}

func (c *Client) retireUDPHandler(endpoint string, handler *UDPHandler) {
	c.udpMu.Lock()
	defer c.udpMu.Unlock()
	if _, live := c.liveUDPHandlers[handler]; !live {
		return
	}
	fragment := handler.fragmentAssembler.Snapshot()
	c.retiredFragments.GroupCapacityDrops += fragment.GroupCapacityDrops
	c.retiredFragments.ByteCapacityDrops += fragment.ByteCapacityDrops
	delete(c.liveUDPHandlers, handler)
	c.udpHandlers.CompareAndDelete(endpoint, handler)
}

// Snapshot takes exact subsystem-local cuts. Cross-subsystem reconciliation is
// exact only after the client is quiescent.
func (c *Client) Snapshot() Snapshot {
	var snapshot Snapshot
	if c.connMgr != nil {
		snapshot.Endpoints = c.connMgr.endpointSnapshot()
	}
	snapshot.UDPSessions = c.udpBudget.snapshot()
	snapshot.DSend = c.dsendStats.load()

	c.udpMu.Lock()
	snapshot.LiveAssemblers = len(c.liveUDPHandlers)
	snapshot.Fragments = c.retiredFragments
	for handler := range c.liveUDPHandlers {
		fragment := handler.fragmentAssembler.Snapshot()
		snapshot.Fragments.RetainedGroups += fragment.RetainedGroups
		snapshot.Fragments.RetainedBackingBytes += fragment.RetainedBackingBytes
		snapshot.Fragments.GroupCapacityDrops += fragment.GroupCapacityDrops
		snapshot.Fragments.ByteCapacityDrops += fragment.ByteCapacityDrops
	}
	c.udpMu.Unlock()
	return snapshot
}

// Stop stops the client
func (c *Client) Stop() error {
	return c.shutdown()
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
