package traffic

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/rs/zerolog"
)

// Manager manages traffic listeners
type Manager struct {
	config    *config.Server
	pools     map[string]*pool.ConnectionPool // quicAddr -> pool
	listeners []*Listener
	logger    zerolog.Logger
	mu        sync.Mutex
}

// Listener represents a traffic listener
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
}

// NewManager creates a new traffic manager
func NewManager(conf *config.Server, pools map[string]*pool.ConnectionPool, logger zerolog.Logger) *Manager {
	return &Manager{
		config:    conf,
		pools:     pools,
		listeners: make([]*Listener, 0),
		logger:    logger.With().Str("com", "traffic").Logger(),
	}
}

// Start starts all traffic listeners
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Create 1:1 mapping between QUIC listeners and traffic addresses
	for _, listenerConf := range m.config.Listeners {
		poolInst := m.pools[listenerConf.QuicAddr]
		if poolInst == nil {
			m.logger.Warn().
				Str("quic_addr", listenerConf.QuicAddr).
				Msg("no connection pool found for QUIC listener, skipping")
			continue
		}

		listener := &Listener{
			Addr:                listenerConf.TrafficAddr,
			Protocol:            listenerConf.Protocol,
			EnableFragmentation: listenerConf.UDP.IsFragmentationEnabled(),
			Pool:                poolInst,
			logger: m.logger.With().
				Str("traffic_addr", listenerConf.TrafficAddr).
				Str("quic_addr", listenerConf.QuicAddr).
				Logger(),
		}
		listener.ctx, listener.cancel = context.WithCancel(ctx)

		// Start TCP listener
		if listenerConf.Protocol == "tcp" || listenerConf.Protocol == "both" {
			if err := listener.startTCP(); err != nil {
				return fmt.Errorf("start TCP listener on %s: %w", listenerConf.TrafficAddr, err)
			}
		}

		// Start UDP listener
		if listenerConf.Protocol == "udp" || listenerConf.Protocol == "both" {
			if err := listener.startUDP(); err != nil {
				return fmt.Errorf("start UDP listener on %s: %w", listenerConf.TrafficAddr, err)
			}
		}

		m.listeners = append(m.listeners, listener)
		m.logger.Info().
			Str("quic_addr", listenerConf.QuicAddr).
			Str("traffic_addr", listenerConf.TrafficAddr).
			Str("protocol", listenerConf.Protocol).
			Msg("traffic listener started")
	}

	m.logger.Info().Int("count", len(m.listeners)).Msg("all traffic listeners started")
	return nil
}

// Stop stops all traffic listeners
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, listener := range m.listeners {
		listener.cancel()
		if listener.TCPListener != nil {
			listener.TCPListener.Close()
		}
		if listener.UDPConn != nil {
			listener.UDPConn.Close()
		}
	}

	m.logger.Info().Msg("traffic listeners stopped")
}
