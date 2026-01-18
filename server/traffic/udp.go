package traffic

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const (
	// Session timeout for inactive UDP sessions
	udpSessionTimeout = 5 * time.Minute
	// Cleanup interval for expired sessions
	udpCleanupInterval = 30 * time.Second
)

// UDPSession represents a UDP session using QUIC datagrams
type UDPSession struct {
	id            uint32
	clientAddr    *net.UDPAddr
	quicConn      *quic.Conn
	lastActive    atomic.Int64
	client        *pool.ClientConn
	fragIDCounter atomic.Uint32 // Changed from uint16 + mutex for lock-free operation
}

func (s *UDPSession) updateLastActive() {
	s.lastActive.Store(time.Now().UnixNano())
}

func (s *UDPSession) isExpired(timeout time.Duration) bool {
	last := time.Unix(0, s.lastActive.Load())
	return time.Since(last) > timeout
}

// UDPHandler handles UDP traffic using QUIC datagrams
type UDPHandler struct {
	// Sessions indexed by client address string
	sessions sync.Map // string -> *UDPSession
	// Sessions indexed by session ID (for reverse lookup from datagrams)
	sessionsByID sync.Map // uint32 -> *UDPSession

	pool                *pool.ConnectionPool
	packetConn          *net.UDPConn
	addr                string
	enableFragmentation bool
	logger              zerolog.Logger
	ctx                 context.Context
	cancel              context.CancelFunc

	nextSessionID atomic.Uint32

	// Fragment assembler for reassembling fragmented packets (sharded for reduced lock contention)
	fragmentAssembler *protocol.ShardedFragmentAssembler
}

// startUDP starts the UDP listener with datagram support
func (l *Listener) startUDP() error {
	addr, err := net.ResolveUDPAddr("udp", l.Addr)
	if err != nil {
		return fmt.Errorf("resolve UDP addr: %w", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen UDP: %w", err)
	}

	// Increase UDP buffer sizes
	if err := conn.SetReadBuffer(4 * 1024 * 1024); err != nil {
		l.logger.Warn().Err(err).Msg("set UDP read buffer failed")
	}
	if err := conn.SetWriteBuffer(4 * 1024 * 1024); err != nil {
		l.logger.Warn().Err(err).Msg("set UDP write buffer failed")
	}

	l.UDPConn = conn
	l.logger.Info().Str("protocol", "udp").Msg("UDP listener started with datagram support")

	ctx, cancel := context.WithCancel(l.ctx)
	handler := &UDPHandler{
		pool:                l.Pool,
		packetConn:          conn,
		addr:                l.Addr,
		enableFragmentation: l.EnableFragmentation,
		logger:              l.logger,
		ctx:                 ctx,
		cancel:              cancel,
		fragmentAssembler:   protocol.NewShardedFragmentAssembler(protocol.DefaultShardCount),
	}

	go handler.readLoop()
	go handler.cleanupLoop()

	return nil
}

// readLoop reads UDP packets using pooled buffers
func (h *UDPHandler) readLoop() {
	for {
		bufPtr := protocol.GetReadBuffer()
		buf := *bufPtr

		n, addr, err := h.packetConn.ReadFromUDP(buf)
		if err != nil {
			protocol.PutReadBuffer(bufPtr)
			select {
			case <-h.ctx.Done():
				return
			default:
				h.logger.Error().Err(err).Msg("read UDP packet failed")
				continue
			}
		}

		// Process packet (makes a copy of data if needed)
		h.processPacket(buf[:n], addr)

		// Return buffer to pool
		protocol.PutReadBuffer(bufPtr)
	}
}

// processPacket handles a single UDP packet
func (h *UDPHandler) processPacket(data []byte, addr *net.UDPAddr) {
	key := addr.String()

	// Fast path: existing session
	if sessionI, ok := h.sessions.Load(key); ok {
		session := sessionI.(*UDPSession)
		session.updateLastActive()
		h.sendDatagrams(session, data)
		return
	}

	// Slow path: create new session
	session, err := h.createSession(addr)
	if err != nil {
		h.logger.Error().Err(err).Str("addr", key).Msg("create UDP session failed")
		return
	}

	h.sendDatagrams(session, data)
}

// sendDatagrams sends data via QUIC datagrams using pooled buffers
func (h *UDPHandler) sendDatagrams(session *UDPSession, data []byte) {
	// No mutex needed - atomic counter is used for thread-safe fragment ID generation
	datagrams, err := protocol.FragmentUDPPooled(
		session.id,
		data,
		&session.fragIDCounter,
		h.enableFragmentation,
	)
	if err != nil {
		h.logger.Debug().Err(err).Uint32("session_id", session.id).Int("size", len(data)).Msg("fragment UDP failed")
		return
	}

	// Ensure buffers are returned to pool
	defer protocol.ReleaseDatagramResults(datagrams)

	for _, dgram := range datagrams {
		if err := session.quicConn.SendDatagram(dgram.Data); err != nil {
			h.logger.Debug().Err(err).Uint32("session_id", session.id).Msg("send datagram failed")
			h.closeSession(session)
			return
		}
	}
}

// createSession creates a new UDP session
func (h *UDPHandler) createSession(addr *net.UDPAddr) (*UDPSession, error) {
	// Select client from pool
	client, err := h.pool.Select()
	if err != nil {
		return nil, fmt.Errorf("select client: %w", err)
	}

	sessionID := h.nextSessionID.Add(1)

	session := &UDPSession{
		id:         sessionID,
		clientAddr: addr,
		quicConn:   client.Conn,
		client:     client,
	}
	session.updateLastActive()

	// Store session
	key := addr.String()
	h.sessions.Store(key, session)
	h.sessionsByID.Store(sessionID, session)

	// Track connection
	client.ActiveConns.Add(1)
	client.TotalConns.Add(1)

	// Start receiving datagrams for this client connection (if not already started)
	h.startDatagramReceiver(client)

	h.logger.Debug().
		Str("addr", key).
		Uint32("session_id", sessionID).
		Str("client_id", client.ID).
		Msg("UDP session created")

	return session, nil
}

// datagramReceivers tracks which client connections have datagram receivers running
var datagramReceivers sync.Map // *quic.Conn -> bool

// startDatagramReceiver starts a datagram receiver for a client connection if not already running
func (h *UDPHandler) startDatagramReceiver(client *pool.ClientConn) {
	if _, loaded := datagramReceivers.LoadOrStore(client.Conn, true); loaded {
		return // Already running
	}

	go h.receiveDatagrams(client.Conn)
}

// receiveDatagrams receives QUIC datagrams and sends them back to UDP clients
func (h *UDPHandler) receiveDatagrams(quicConn *quic.Conn) {
	defer datagramReceivers.Delete(quicConn)

	for {
		dgram, err := quicConn.ReceiveDatagram(h.ctx)
		if err != nil {
			select {
			case <-h.ctx.Done():
				return
			default:
				h.logger.Debug().Err(err).Msg("receive datagram failed")
				return
			}
		}

		// Parse datagram
		sessionID, isFragmented, fragID, fragIndex, fragTotal, payload, err := protocol.ParseUDPDatagram(dgram)
		if err != nil {
			h.logger.Debug().Err(err).Msg("parse datagram failed")
			continue
		}

		// Handle fragmented packets
		if isFragmented {
			payload, err = h.fragmentAssembler.AddFragment(sessionID, fragID, fragIndex, fragTotal, payload)
			if err != nil {
				h.logger.Debug().Err(err).Msg("add fragment failed")
				continue
			}
			if payload == nil {
				// More fragments needed
				continue
			}
		}

		// Find session by ID
		sessionI, ok := h.sessionsByID.Load(sessionID)
		if !ok {
			continue
		}

		session := sessionI.(*UDPSession)
		session.updateLastActive()

		// Send back to UDP client
		if _, err := h.packetConn.WriteToUDP(payload, session.clientAddr); err != nil {
			h.logger.Debug().Err(err).Str("addr", session.clientAddr.String()).Msg("write UDP response failed")
		}
	}
}

// closeSession closes a UDP session
func (h *UDPHandler) closeSession(session *UDPSession) {
	key := session.clientAddr.String()

	// Remove from maps
	h.sessions.Delete(key)
	h.sessionsByID.Delete(session.id)

	// Update connection count
	session.client.ActiveConns.Add(-1)

	h.logger.Debug().
		Str("addr", key).
		Uint32("session_id", session.id).
		Msg("UDP session closed")
}

// cleanupLoop periodically cleans up expired sessions
func (h *UDPHandler) cleanupLoop() {
	ticker := time.NewTicker(udpCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			h.sessions.Range(func(key, value interface{}) bool {
				session := value.(*UDPSession)
				if session.isExpired(udpSessionTimeout) {
					h.logger.Debug().Str("addr", key.(string)).Msg("cleaning up expired UDP session")
					h.closeSession(session)
				}
				return true
			})
		}
	}
}
