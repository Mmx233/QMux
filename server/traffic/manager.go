package traffic

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/rs/zerolog"
)

var (
	// ErrAlreadyStarted is returned when Start is called more than once on a
	// manager whose run has not stopped.
	ErrAlreadyStarted = errors.New("traffic manager already started")
	// ErrManagerStopped is returned when Start is called after shutdown or a
	// failed startup. Managers are deliberately single-run.
	ErrManagerStopped = errors.New("traffic manager stopped")
	// ErrMissingPool indicates that a configured listener has no connection pool.
	ErrMissingPool = errors.New("traffic listener connection pool missing")
	// ErrDuplicateDatagramRoute indicates that multiple UDP listeners would read
	// datagrams from the same pool of QUIC connections.
	ErrDuplicateDatagramRoute = errors.New("duplicate QUIC datagram route")
)

type managerState uint8

const (
	managerNew managerState = iota
	managerStarting
	managerRunning
	managerClosing
	managerStopped
)

// Manager manages traffic listeners.
type Manager struct {
	config *config.Server
	pools  map[string]*pool.ConnectionPool // quicAddr -> pool
	logger zerolog.Logger

	mu        sync.Mutex
	state     managerState
	listeners []*Listener
	cancel    context.CancelFunc
	watchDone chan struct{}
	done      chan struct{}
	doneOnce  sync.Once

	// beforeCommit is an internal synchronization seam. Production leaves it nil.
	beforeCommit func()
}

// Listener represents a traffic listener.
type Listener struct {
	Addr                string
	Protocol            string // "tcp", "udp", or "both"
	EnableFragmentation bool   // UDP fragmentation enabled
	TCPListener         net.Listener
	UDPConn             net.PacketConn
	Pool                *pool.ConnectionPool

	ctx    context.Context
	cancel context.CancelFunc
	logger zerolog.Logger

	closeOnce sync.Once
	fixedWG   sync.WaitGroup
	handlerWG sync.WaitGroup

	flowsMu      sync.Mutex
	flowsClosing bool
	flows        map[*tcpFlow]struct{}
	udpHandler   *UDPHandler
}

// NewManager creates a new traffic manager.
func NewManager(conf *config.Server, pools map[string]*pool.ConnectionPool, logger zerolog.Logger) *Manager {
	return &Manager{
		config:    conf,
		pools:     pools,
		listeners: make([]*Listener, 0),
		logger:    logger.With().Str("com", "traffic").Logger(),
		state:     managerNew,
		done:      make(chan struct{}),
	}
}

// Start transactionally binds and starts all configured traffic listeners.
// A Manager is single-run, including after a failed Start.
func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	switch m.state {
	case managerStarting, managerRunning:
		m.mu.Unlock()
		return ErrAlreadyStarted
	case managerClosing, managerStopped:
		m.mu.Unlock()
		return ErrManagerStopped
	case managerNew:
		runCtx, cancel := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		m.state = managerStarting
		m.cancel = cancel
		m.watchDone = watchDone
		m.mu.Unlock()

		go func() {
			defer close(watchDone)
			<-runCtx.Done()
			m.Close()
		}()

		return m.start(runCtx)
	}
	panic("unreachable")
}

func (m *Manager) start(runCtx context.Context) error {
	configs := []config.QuicListener(nil)
	if m.config != nil {
		configs = m.config.Listeners
	}
	if err := m.validate(configs); err != nil {
		m.rollback(nil)
		return err
	}

	staged := make([]*Listener, 0, len(configs))
	for _, listenerConf := range configs {
		listener := m.newListener(runCtx, listenerConf, m.pools[listenerConf.QuicAddr])
		staged = append(staged, listener)

		if listenerConf.Protocol == "tcp" || listenerConf.Protocol == "both" {
			if err := listener.bindTCP(); err != nil {
				m.rollback(staged)
				return fmt.Errorf("start TCP listener on %s: %w", listenerConf.TrafficAddr, err)
			}
		}

		if listenerConf.Protocol == "udp" || listenerConf.Protocol == "both" {
			if err := listener.bindUDP(); err != nil {
				m.rollback(staged)
				return fmt.Errorf("start UDP listener on %s: %w", listenerConf.TrafficAddr, err)
			}
		}
	}
	if m.beforeCommit != nil {
		m.beforeCommit()
	}

	// Commit while holding the manager lock. Listener serve methods register all
	// fixed goroutines before Close or Wait can observe the running state.
	m.mu.Lock()
	if m.state != managerStarting || runCtx.Err() != nil {
		m.mu.Unlock()
		m.rollback(staged)
		if err := runCtx.Err(); err != nil {
			return err
		}
		return ErrManagerStopped
	}
	m.listeners = staged
	for _, listener := range staged {
		listener.serve()
	}
	m.state = managerRunning
	m.mu.Unlock()

	for i, listener := range staged {
		listenerConf := configs[i]
		m.logger.Info().
			Str("quic_addr", listenerConf.QuicAddr).
			Str("traffic_addr", listener.Addr).
			Str("protocol", listener.Protocol).
			Msg("traffic listener started")
	}
	m.logger.Info().Int("count", len(staged)).Msg("all traffic listeners started")
	return nil
}

func (m *Manager) validate(configs []config.QuicListener) error {
	udpRoutes := make(map[string]struct{})
	for _, listenerConf := range configs {
		if m.pools[listenerConf.QuicAddr] == nil {
			return fmt.Errorf("%w for QUIC address %q", ErrMissingPool, listenerConf.QuicAddr)
		}

		if listenerConf.Protocol != "udp" && listenerConf.Protocol != "both" {
			continue
		}
		if _, exists := udpRoutes[listenerConf.QuicAddr]; exists {
			return fmt.Errorf("%w for %q", ErrDuplicateDatagramRoute, listenerConf.QuicAddr)
		}
		udpRoutes[listenerConf.QuicAddr] = struct{}{}
	}
	return nil
}

func (m *Manager) newListener(ctx context.Context, listenerConf config.QuicListener, poolInst *pool.ConnectionPool) *Listener {
	listenerCtx, listenerCancel := context.WithCancel(ctx)
	return &Listener{
		Addr:                listenerConf.TrafficAddr,
		Protocol:            listenerConf.Protocol,
		EnableFragmentation: listenerConf.UDP.IsFragmentationEnabled(),
		Pool:                poolInst,
		ctx:                 listenerCtx,
		cancel:              listenerCancel,
		flows:               make(map[*tcpFlow]struct{}),
		logger: m.logger.With().
			Str("traffic_addr", listenerConf.TrafficAddr).
			Str("quic_addr", listenerConf.QuicAddr).
			Logger(),
	}
}

// rollback closes all staged resources and joins every startup-owned goroutine
// before returning the startup error.
func (m *Manager) rollback(staged []*Listener) {
	m.mu.Lock()
	cancel := m.cancel
	watchDone := m.watchDone
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, listener := range staged {
		listener.close()
	}
	for _, listener := range staged {
		listener.wait()
	}
	if watchDone != nil {
		<-watchDone
	}
	m.complete()
}

// Close initiates shutdown. It is safe to call concurrently and does not join
// listener or connection goroutines, though it may synchronously finish local
// resource cleanup before returning. Use Wait to join the lifecycle.
func (m *Manager) Close() {
	m.mu.Lock()
	switch m.state {
	case managerStopped, managerClosing:
		m.mu.Unlock()
		return
	case managerNew:
		m.state = managerStopped
		m.mu.Unlock()
		m.doneOnce.Do(func() { close(m.done) })
		return
	case managerStarting:
		m.state = managerClosing
		cancel := m.cancel
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return // Start owns rollback until the bind transaction finishes.
	case managerRunning:
		m.state = managerClosing
		cancel := m.cancel
		watchDone := m.watchDone
		listeners := append([]*Listener(nil), m.listeners...)
		m.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		for _, listener := range listeners {
			listener.close()
		}
		go func() {
			for _, listener := range listeners {
				listener.wait()
			}
			if watchDone != nil {
				<-watchDone
			}
			m.complete()
		}()
	}
}

func (m *Manager) complete() {
	m.mu.Lock()
	m.state = managerStopped
	m.mu.Unlock()
	m.logger.Info().Msg("traffic listeners stopped")
	m.doneOnce.Do(func() { close(m.done) })
}

// Wait joins manager shutdown. It is safe to call concurrently.
func (m *Manager) Wait() {
	<-m.done
}

// Stop initiates shutdown and waits for it to complete.
func (m *Manager) Stop() {
	m.Close()
	m.Wait()
}

func (l *Listener) serve() {
	if l.TCPListener != nil {
		l.fixedWG.Add(1)
		go l.acceptTCP()
	}
	if l.UDPConn != nil {
		l.startUDPHandler()
	}
}

func (l *Listener) close() {
	l.closeOnce.Do(func() {
		l.cancel()
		if l.TCPListener != nil {
			_ = l.TCPListener.Close()
		}
		if l.UDPConn != nil {
			_ = l.UDPConn.Close()
		}
		if l.udpHandler != nil {
			l.udpHandler.close()
		}

		l.flowsMu.Lock()
		l.flowsClosing = true
		flows := make([]*tcpFlow, 0, len(l.flows))
		for flow := range l.flows {
			flows = append(flows, flow)
		}
		l.flowsMu.Unlock()
		for _, flow := range flows {
			flow.abort()
		}
	})
}

func (l *Listener) wait() {
	// The fixed accept/read loops are the only producers for the dynamic wait
	// groups, so drain them before waiting for connection workers.
	l.fixedWG.Wait()
	l.handlerWG.Wait()
	if l.udpHandler != nil {
		l.udpHandler.wait()
	}
}
