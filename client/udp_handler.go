package client

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const (
	// Session timeout for inactive UDP sessions
	udpSessionTimeout = 5 * time.Minute
	// Cleanup interval for expired sessions
	udpCleanupInterval = 30 * time.Second
)

// UDPSession represents a client-side UDP session
type UDPSession struct {
	id            uint32
	localConn     *net.UDPConn
	quicConn      *quic.Conn
	lastActive    atomic.Int64
	fragIDCounter atomic.Uint32 // Changed from uint16 + mutex for lock-free operation
}

func (s *UDPSession) updateLastActive() {
	s.lastActive.Store(time.Now().UnixNano())
}

func (s *UDPSession) isExpired(timeout time.Duration) bool {
	last := time.Unix(0, s.lastActive.Load())
	return time.Since(last) > timeout
}

// UDPHandler handles UDP datagram forwarding on the client side
type UDPHandler struct {
	// Sessions indexed by session ID
	sessions sync.Map // uint32 -> *UDPSession

	localHost           string
	localPort           int
	enableFragmentation bool
	logger              zerolog.Logger
	ctx                 context.Context
	cancel              context.CancelFunc

	// Fragment assembler for reassembling fragmented packets (sharded for reduced lock contention)
	fragmentAssembler *protocol.ShardedFragmentAssembler
}

// NewUDPHandler creates a new UDP handler
func NewUDPHandler(localHost string, localPort int, enableFragmentation bool, logger zerolog.Logger) *UDPHandler {
	ctx, cancel := context.WithCancel(context.Background())
	return &UDPHandler{
		localHost:           localHost,
		localPort:           localPort,
		enableFragmentation: enableFragmentation,
		logger:              logger.With().Str("component", "udp_handler").Logger(),
		ctx:                 ctx,
		cancel:              cancel,
		fragmentAssembler:   protocol.NewShardedFragmentAssembler(protocol.DefaultShardCount),
	}
}

// Start starts the UDP handler for a QUIC connection
func (h *UDPHandler) Start(ctx context.Context, quicConn quic.Conn) {
	go h.receiveDatagrams(ctx, &quicConn)
	go h.cleanupLoop()
}

// Stop stops the UDP handler
func (h *UDPHandler) Stop() {
	h.cancel()

	// Close all local connections
	h.sessions.Range(func(key, value interface{}) bool {
		session := value.(*UDPSession)
		session.localConn.Close()
		return true
	})
}

// receiveDatagrams receives QUIC datagrams and forwards to local UDP service
func (h *UDPHandler) receiveDatagrams(ctx context.Context, quicConn *quic.Conn) {
	for {
		dgram, err := quicConn.ReceiveDatagram(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return
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

		// Get or create session
		session, err := h.getOrCreateSession(sessionID, quicConn)
		if err != nil {
			h.logger.Error().Err(err).Uint32("session_id", sessionID).Msg("get session failed")
			continue
		}

		session.updateLastActive()

		// Forward to local service
		if _, err := session.localConn.Write(payload); err != nil {
			h.logger.Debug().Err(err).Uint32("session_id", sessionID).Msg("write to local failed")
			h.closeSession(sessionID)
			continue
		}
	}
}

// getOrCreateSession gets an existing session or creates a new one
func (h *UDPHandler) getOrCreateSession(sessionID uint32, quicConn *quic.Conn) (*UDPSession, error) {
	// Fast path: existing session
	if sessionI, ok := h.sessions.Load(sessionID); ok {
		return sessionI.(*UDPSession), nil
	}

	// Slow path: create new session
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", h.localHost, h.localPort))
	if err != nil {
		return nil, err
	}

	localConn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, err
	}

	// Increase UDP buffer sizes to handle large packets
	if err := localConn.SetReadBuffer(4 * 1024 * 1024); err != nil {
		h.logger.Warn().Err(err).Msg("set UDP read buffer failed")
	}
	if err := localConn.SetWriteBuffer(4 * 1024 * 1024); err != nil {
		h.logger.Warn().Err(err).Msg("set UDP write buffer failed")
	}

	session := &UDPSession{
		id:        sessionID,
		localConn: localConn,
		quicConn:  quicConn,
	}
	session.updateLastActive()

	// Store session
	actual, loaded := h.sessions.LoadOrStore(sessionID, session)
	if loaded {
		// Another goroutine created the session first
		localConn.Close()
		return actual.(*UDPSession), nil
	}

	h.logger.Debug().Uint32("session_id", sessionID).Str("local_addr", addr.String()).Msg("UDP session created")

	// Start reading responses from local service
	go h.readLocalResponses(session)

	return session, nil
}

// readLocalResponses reads responses from local UDP service and sends back via datagram
func (h *UDPHandler) readLocalResponses(session *UDPSession) {
	for {
		// Get buffer from pool at start of each iteration
		bufPtr := protocol.GetReadBuffer()
		buf := *bufPtr

		session.localConn.SetReadDeadline(time.Now().Add(udpSessionTimeout))
		n, err := session.localConn.Read(buf)
		if err != nil {
			select {
			case <-h.ctx.Done():
				protocol.PutReadBuffer(bufPtr)
				return
			default:
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Timeout - check if session is still active
					if session.isExpired(udpSessionTimeout) {
						protocol.PutReadBuffer(bufPtr)
						h.closeSession(session.id)
						return
					}
					protocol.PutReadBuffer(bufPtr)
					continue
				}
				h.logger.Debug().Err(err).Uint32("session_id", session.id).Msg("read from local failed")
				protocol.PutReadBuffer(bufPtr)
				h.closeSession(session.id)
				return
			}
		}

		session.updateLastActive()

		// Fragment and send datagrams using pooled fragmentation (no mutex needed - atomic counter)
		datagrams, err := protocol.FragmentUDPPooled(session.id, buf[:n], &session.fragIDCounter, h.enableFragmentation)

		if err != nil {
			h.logger.Debug().Err(err).Uint32("session_id", session.id).Int("size", n).Msg("fragment UDP failed")
			protocol.PutReadBuffer(bufPtr)
			continue
		}

		for _, dgram := range datagrams {
			if err := session.quicConn.SendDatagram(dgram.Data); err != nil {
				h.logger.Debug().Err(err).Uint32("session_id", session.id).Msg("send datagram failed")
				protocol.ReleaseDatagramResults(datagrams)
				protocol.PutReadBuffer(bufPtr)
				h.closeSession(session.id)
				return
			}
		}

		// Return datagram buffers to pool after sending
		protocol.ReleaseDatagramResults(datagrams)
		// Return read buffer to pool after processing
		protocol.PutReadBuffer(bufPtr)
	}
}

// closeSession closes a UDP session
func (h *UDPHandler) closeSession(sessionID uint32) {
	if sessionI, ok := h.sessions.LoadAndDelete(sessionID); ok {
		session := sessionI.(*UDPSession)
		session.localConn.Close()
		h.logger.Debug().Uint32("session_id", sessionID).Msg("UDP session closed")
	}
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
					h.logger.Debug().Uint32("session_id", session.id).Msg("cleaning up expired UDP session")
					h.closeSession(session.id)
				}
				return true
			})
		}
	}
}
