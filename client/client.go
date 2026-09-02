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
	// ErrPeerGracefulShutdownUnsupported is returned when a live peer can't drain.
	ErrPeerGracefulShutdownUnsupported = errors.New("peer graceful shutdown unsupported")
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

	lifecycleMu        sync.Mutex
	started            bool
	stopping           bool
	runCancel          context.CancelFunc
	forceCtx           context.Context
	forceCancel        context.CancelFunc
	coordinatorStarted bool
	terminalSelected   bool
	terminalSemantic   error
	terminalTeardown   error
	terminalDone       chan struct{}
	startupWG          sync.WaitGroup
	producerWG         sync.WaitGroup
	watcherWG          sync.WaitGroup
	runtimesMu         sync.Mutex
	runtimes           map[*ServerConnection]*connectionRuntime
}

type connectionRuntime struct {
	sc           *ServerConnection
	conn         *quic.Conn
	forceCtx     context.Context
	cancelForce  context.CancelFunc
	acceptCtx    context.Context
	cancelAccept context.CancelFunc
	acceptWG     sync.WaitGroup
	handlerWG    sync.WaitGroup
	acceptDone   chan struct{}
	acceptErr    chan error
	udp          *UDPHandler
	localConns   sync.Map
	mu           sync.Mutex
	acceptedHigh int64
	fence        int64
	fenceSet     bool
	cleanupOnce  sync.Once
	cleanupDone  chan struct{}
	cleanupErr   error
}

type targetResult struct {
	semantic error
	teardown error
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

	forceCtx, forceCancel := context.WithCancel(context.Background())
	return &Client{
		config:          conf,
		connMgr:         connMgr,
		udpBudget:       newUDPSessionBudget(conf.Capacity.MaxLocalUDPSessions),
		dsendStats:      &clientDsendStats{},
		liveUDPHandlers: make(map[*UDPHandler]struct{}),
		runtimes:        make(map[*ServerConnection]*connectionRuntime),
		logger:          logger,
		terminalDone:    make(chan struct{}),
		forceCtx:        forceCtx,
		forceCancel:     forceCancel,
	}, nil
}

// Start starts the client
func (c *Client) Start(ctx context.Context) error {
	c.lifecycleMu.Lock()
	if c.stopping {
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
	c.startupWG.Add(1)
	c.producerWG.Go(c.handleNewConnections)
	c.lifecycleMu.Unlock()
	callerWatchDone := make(chan struct{})
	stopCallerWatch := context.AfterFunc(ctx, func() {
		defer close(callerWatchDone)
		c.selectTerminal(context.Cause(ctx), true)
	})
	defer func() {
		if stopCallerWatch() {
			close(callerWatchDone)
		}
		<-callerWatchDone
	}()

	servers := c.config.Server.GetServers()
	serverAddrs := make([]string, len(servers))
	for i, s := range servers {
		serverAddrs[i] = s.Address
	}

	c.logger.Info().
		Strs("servers", serverAddrs).
		Str("local", net.JoinHostPort(c.config.Local.Host, strconv.Itoa(c.config.Local.Port))).
		Msg("starting client")

	var startupErr error
	if err := c.connMgr.Start(runCtx); err != nil {
		coordinatorCancellation := runCtx.Err() != nil && errors.Is(err, runCtx.Err())
		if !coordinatorCancellation {
			startupErr = fmt.Errorf("start connection manager: %w", err)
			c.selectTerminal(startupErr, true)
		}
	}
	c.startupWG.Done()

	c.logger.Info().
		Int("healthy", c.connMgr.HealthyCount()).
		Int("total", c.connMgr.TotalCount()).
		Msg("client started successfully")

	select {
	case <-ctx.Done():
		c.selectTerminal(context.Cause(ctx), true)
	case <-c.terminalDone:
	}
	c.logger.Info().Msg("client shutting down")
	<-c.terminalDone
	c.lifecycleMu.Lock()
	teardown := c.terminalTeardown
	c.lifecycleMu.Unlock()
	return errors.Join(startupErr, teardown)
}

// handleNewConnections listens on the connection manager's NewConns channel
// and starts stream acceptance and UDP handler for each new connection.

func (c *Client) handleNewConnections() {
	for sc := range c.connMgr.NewConns {
		c.installRuntime(sc)
	}
}

func (c *Client) installRuntime(sc *ServerConnection) {
	conn := sc.Connection()
	forceCtx, cancelForce := context.WithCancel(c.forceCtx)
	acceptCtx, cancelAccept := context.WithCancel(forceCtx)
	runtime := &connectionRuntime{
		sc: sc, conn: conn, forceCtx: forceCtx, cancelForce: cancelForce,
		acceptCtx: acceptCtx, cancelAccept: cancelAccept,
		acceptDone: make(chan struct{}), acceptErr: make(chan error, 1),
		acceptedHigh: -1, cleanupDone: make(chan struct{}),
	}
	if conn != nil {
		c.udpMu.Lock()
		if c.dsendStats == nil {
			c.dsendStats = &clientDsendStats{}
		}
		dsendStats := c.dsendStats
		c.udpMu.Unlock()
		runtime.udp = newUDPHandler(c.config.Local.Host, c.config.Local.Port,
			c.config.UDP.IsFragmentationEnabled(), c.logger, c.udpBudget, dsendStats)
	}
	if conn == nil {
		close(runtime.acceptDone)
		c.runtimesMu.Lock()
		c.runtimes[sc] = runtime
		c.runtimesMu.Unlock()
		return
	}
	runtime.acceptWG.Go(func() { c.acceptStreamsFromConnection(runtime) })
	c.udpMu.Lock()
	if c.liveUDPHandlers == nil {
		c.liveUDPHandlers = make(map[*UDPHandler]struct{})
	}
	c.liveUDPHandlers[runtime.udp] = struct{}{}
	runtime.udp.Start(forceCtx, conn)
	c.udpHandlers.Store(sc.ServerAddr(), runtime.udp)
	c.udpMu.Unlock()
	c.runtimesMu.Lock()
	c.runtimes[sc] = runtime
	c.runtimesMu.Unlock()
	c.watcherWG.Go(func() {
		<-runtime.udp.done
		c.retireUDPHandler(sc.ServerAddr(), runtime.udp)
	})
	c.watcherWG.Go(func() {
		<-conn.Context().Done()
		<-sc.closeDone
		_ = c.cleanupRuntime(runtime)
	})
}

// acceptStreamsFromConnection accepts streams through the server's drain fence.
func (c *Client) acceptStreamsFromConnection(runtime *connectionRuntime) {
	defer close(runtime.acceptDone)
	logger := c.logger.With().Str("server", runtime.sc.ServerAddr()).Logger()

	for {
		stream, err := runtime.sc.AcceptStream(runtime.acceptCtx)
		if err != nil {
			runtime.mu.Lock()
			graceful := runtime.fenceSet && (runtime.fence == -1 || runtime.acceptedHigh >= runtime.fence)
			runtime.mu.Unlock()
			if graceful || runtime.forceCtx.Err() != nil {
				return
			}
			runtime.failAccept(fmt.Errorf("accept stream: %w", err))
			return
		}
		streamID := int64(stream.StreamID())
		if err := protocol.ValidateDrainFence(streamID); err != nil {
			stream.CancelRead(0)
			stream.CancelWrite(0)
			runtime.failAccept(err)
			return
		}
		runtime.mu.Lock()
		if runtime.fenceSet && streamID > runtime.fence {
			runtime.mu.Unlock()
			stream.CancelRead(0)
			stream.CancelWrite(0)
			runtime.failAccept(fmt.Errorf("accepted stream %d above drain fence %d", streamID, runtime.fence))
			return
		}
		runtime.handlerWG.Go(func() {
			c.handleStream(runtime.forceCtx, stream, runtime.sc, runtime)
		})
		runtime.acceptedHigh = max(runtime.acceptedHigh, streamID)
		stop := runtime.fenceSet && runtime.acceptedHigh >= runtime.fence
		runtime.mu.Unlock()
		if stop {
			runtime.cancelAccept()
		}
		logger.Debug().Int64("stream_id", streamID).Msg("accepted TCP stream")
	}
}

func (runtime *connectionRuntime) failAccept(err error) {
	select {
	case runtime.acceptErr <- err:
	default:
	}
}

func (runtime *connectionRuntime) setFence(fence int64) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.fenceSet && runtime.fence != fence {
		return fmt.Errorf("conflicting drain fence %d after %d", fence, runtime.fence)
	}
	if runtime.acceptedHigh > fence {
		return fmt.Errorf("drain fence %d below accepted stream %d", fence, runtime.acceptedHigh)
	}
	runtime.fenceSet = true
	runtime.fence = fence
	if fence == -1 || runtime.acceptedHigh >= fence {
		runtime.cancelAccept()
	}
	return nil
}

// handleStream handles a single stream from server
func (c *Client) handleStream(ctx context.Context, stream *quic.Stream, sc *ServerConnection, runtimes ...*connectionRuntime) {
	relayOwnsStream := false
	defer func() {
		if !relayOwnsStream {
			stream.CancelRead(0)
			stream.CancelWrite(0)
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
	dialCtx, cancelDial := context.WithTimeout(ctx, 5*time.Second)
	localConn, err := (&net.Dialer{}).DialContext(dialCtx, msg.Protocol, localAddr)
	cancelDial()
	if err != nil {
		logger.Error().Err(err).Str("local_addr", localAddr).Msg("dial local service failed")
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
	if len(runtimes) != 0 {
		runtimes[0].localConns.Store(msg.ConnID, localConn)
		defer runtimes[0].localConns.Delete(msg.ConnID)
	}

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
}

// Shutdown drains supported TCP generations and force-closes only failures.
func (c *Client) Shutdown(ctx context.Context) error {
	c.lifecycleMu.Lock()
	c.startCoordinatorLocked()
	done := c.terminalDone
	c.lifecycleMu.Unlock()

	select {
	case <-done:
		return c.sharedResult()
	default:
	}
	select {
	case <-done:
		return c.sharedResult()
	case <-ctx.Done():
		cause := context.Cause(ctx)
		won := c.selectTerminal(cause, true)
		<-done
		if won {
			return c.sharedResult()
		}
		return errors.Join(cause, c.sharedResult())
	}
}

func (c *Client) startCoordinatorLocked() {
	if c.coordinatorStarted {
		return
	}
	c.stopping = true
	c.coordinatorStarted = true
	go c.runCoordinator()
}

func (c *Client) selectTerminal(semantic error, force bool) bool {
	c.lifecycleMu.Lock()
	c.startCoordinatorLocked()
	if c.terminalSelected {
		c.lifecycleMu.Unlock()
		return false
	}
	c.terminalSelected = true
	c.terminalSemantic = semantic
	forceCancel, runCancel := c.forceCancel, c.runCancel
	c.lifecycleMu.Unlock()

	if force {
		if forceCancel != nil {
			forceCancel()
		}
		if runCancel != nil {
			runCancel()
		}
		c.forceOwned()
	} else if runCancel != nil {
		runCancel()
	}
	return true
}

func (c *Client) forceOwned() {
	runtimes := c.runtimeSnapshot()
	for _, runtime := range runtimes {
		if c.connMgr != nil {
			c.connMgr.retireConnection(runtime.sc)
		}
	}
	for _, runtime := range runtimes {
		runtime.cancelForce()
		runtime.cancelAccept()
		_ = runtime.sc.Close()
		if runtime.udp != nil {
			runtime.udp.Stop()
		}
		runtime.localConns.Range(func(_, value any) bool {
			_ = value.(net.Conn).Close()
			return true
		})
	}
	c.localConns.Range(func(_, value any) bool {
		_ = value.(net.Conn).Close()
		return true
	})
}

func (c *Client) runCoordinator() {
	if c.connMgr != nil {
		c.connMgr.stopPublishing()
	}
	c.startupWG.Wait()
	c.producerWG.Wait()

	runtimes := c.runtimeSnapshot()
	results := make(chan targetResult, len(runtimes))
	var targets sync.WaitGroup
	for _, runtime := range runtimes {
		targets.Go(func() { results <- c.processRuntime(runtime) })
	}
	targets.Wait()
	close(results)
	var semanticErrors, teardownErrors []error
	for result := range results {
		semanticErrors = append(semanticErrors, result.semantic)
		teardownErrors = append(teardownErrors, result.teardown)
	}
	c.selectTerminal(errors.Join(semanticErrors...), false)
	if c.connMgr != nil {
		teardownErrors = append(teardownErrors, c.connMgr.Stop())
	}
	c.watcherWG.Wait()

	c.lifecycleMu.Lock()
	c.terminalTeardown = errors.Join(teardownErrors...)
	close(c.terminalDone)
	c.lifecycleMu.Unlock()
	c.logger.Info().Msg("client shutdown complete")
}

func (c *Client) processRuntime(runtime *connectionRuntime) targetResult {
	current := c.connMgr != nil && c.connMgr.isCurrent(runtime.sc)
	if !current || runtime.conn == nil || runtime.transportDone() {
		return targetResult{teardown: c.cleanupRuntime(runtime)}
	}
	fail := func(err error) targetResult {
		return targetResult{
			semantic: fmt.Errorf("server %s: %w", runtime.sc.ServerAddr(), err),
			teardown: c.cleanupRuntime(runtime),
		}
	}
	if !runtime.sc.IsHealthy() {
		return fail(fmt.Errorf("connection is unhealthy"))
	}
	if !runtime.sc.controlAlive() {
		return fail(runtime.sc.controlResult())
	}
	if !runtime.sc.HasCapability(protocol.CapabilityTCPDrainV1) {
		return fail(ErrPeerGracefulShutdownUnsupported)
	}
	if err := runtime.sc.requestDrain(c.forceCtx); err != nil {
		if c.forceCtx.Err() != nil {
			return targetResult{teardown: c.cleanupRuntime(runtime)}
		}
		return fail(err)
	}

	var fence int64
	select {
	case <-c.forceCtx.Done():
		return targetResult{teardown: c.cleanupRuntime(runtime)}
	case err := <-runtime.acceptErr:
		return fail(err)
	case <-runtime.sc.controlDone:
		return fail(runtime.sc.controlResult())
	case <-runtime.conn.Context().Done():
		return fail(fmt.Errorf("connection closed before drain complete: %w", context.Cause(runtime.conn.Context())))
	case fence = <-runtime.sc.drainComplete:
	}
	if err := runtime.setFence(fence); err != nil {
		return fail(err)
	}

	select {
	case <-c.forceCtx.Done():
		return targetResult{teardown: c.cleanupRuntime(runtime)}
	case err := <-runtime.acceptErr:
		return fail(err)
	case <-runtime.sc.controlDone:
		return fail(runtime.sc.controlResult())
	case <-runtime.conn.Context().Done():
		return fail(fmt.Errorf("connection closed before accept fence: %w", context.Cause(runtime.conn.Context())))
	case <-runtime.acceptDone:
	}
	handlersDone := make(chan struct{})
	go func() {
		runtime.handlerWG.Wait()
		close(handlersDone)
	}()
	defer func() { <-handlersDone }()
	select {
	case <-c.forceCtx.Done():
		return targetResult{teardown: c.cleanupRuntime(runtime)}
	case err := <-runtime.acceptErr:
		return fail(err)
	case <-runtime.sc.controlDone:
		return fail(runtime.sc.controlResult())
	case <-runtime.conn.Context().Done():
		return fail(fmt.Errorf("connection closed before handlers completed: %w", context.Cause(runtime.conn.Context())))
	case <-handlersDone:
		if err := c.waitControlQuiescent(runtime); err != nil {
			if c.forceCtx.Err() != nil {
				return targetResult{teardown: c.cleanupRuntime(runtime)}
			}
			return fail(err)
		}
		if runtime.transportDone() {
			return fail(fmt.Errorf("control or transport stopped before target success"))
		}
		select {
		case err := <-runtime.acceptErr:
			return fail(err)
		default:
		}
	}
	return targetResult{teardown: c.cleanupRuntime(runtime)}
}

func (c *Client) waitControlQuiescent(runtime *connectionRuntime) error {
	for runtime.sc.controlPending.Load() != 0 {
		select {
		case <-c.forceCtx.Done():
			return context.Cause(c.forceCtx)
		case <-runtime.sc.controlDone:
			return runtime.sc.controlResult()
		case <-runtime.sc.controlProgress:
		}
	}
	if !runtime.sc.controlAlive() || runtime.sc.controlPending.Load() != 0 {
		return fmt.Errorf("control stopped before target success")
	}
	return nil
}

func (runtime *connectionRuntime) transportDone() bool {
	select {
	case <-runtime.conn.Context().Done():
		return true
	default:
		return false
	}
}

func (c *Client) cleanupRuntime(runtime *connectionRuntime) error {
	runtime.cleanupOnce.Do(func() {
		runtime.cancelForce()
		runtime.cancelAccept()
		if c.connMgr != nil {
			c.connMgr.retireConnection(runtime.sc)
		}
		runtime.cleanupErr = runtime.sc.Close()
		runtime.localConns.Range(func(_, value any) bool {
			_ = value.(net.Conn).Close()
			return true
		})
		if runtime.udp != nil {
			runtime.udp.Stop()
		}
		runtime.acceptWG.Wait()
		runtime.handlerWG.Wait()
		if runtime.udp != nil {
			runtime.udp.wait()
			c.retireUDPHandler(runtime.sc.ServerAddr(), runtime.udp)
		}
		runtime.sc.waitControl()
		c.runtimesMu.Lock()
		delete(c.runtimes, runtime.sc)
		c.runtimesMu.Unlock()
		close(runtime.cleanupDone)
	})
	<-runtime.cleanupDone
	return runtime.cleanupErr
}

func (c *Client) runtimeSnapshot() []*connectionRuntime {
	c.runtimesMu.Lock()
	defer c.runtimesMu.Unlock()
	runtimes := make([]*connectionRuntime, 0, len(c.runtimes))
	for _, runtime := range c.runtimes {
		runtimes = append(runtimes, runtime)
	}
	return runtimes
}

func (c *Client) sharedResult() error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	return errors.Join(c.terminalSemantic, c.terminalTeardown)
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
	c.selectTerminal(ErrClientStopped, true)
	<-c.terminalDone
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	return c.terminalTeardown
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
