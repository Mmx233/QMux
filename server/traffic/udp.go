package traffic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/connid"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

// UDPSession represents a UDP session
type UDPSession struct {
	clientAddr net.Addr
	stream     *quic.Stream
	client     *pool.ClientConn
	lastActive time.Time
	connID     uint64
	mu         sync.Mutex
}

// UDPHandler handles UDP traffic
type UDPHandler struct {
	sessions   sync.Map // clientAddr.String() -> *UDPSession
	pool       *pool.ConnectionPool
	packetConn net.PacketConn
	port       int
	timeout    time.Duration
	logger     zerolog.Logger
	ctx        context.Context
}

// startUDP starts the UDP listener
func (l *Listener) startUDP() error {
	conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", l.Port))
	if err != nil {
		return fmt.Errorf("listen UDP: %w", err)
	}

	l.UDPConn = conn
	l.logger.Info().Str("protocol", "udp").Msg("UDP listener started")

	handler := &UDPHandler{
		pool:       l.Pool,
		packetConn: conn,
		port:       l.Port,
		timeout:    5 * time.Minute,
		logger:     l.logger,
		ctx:        l.ctx,
	}

	go handler.handleUDP()
	go handler.cleanupSessions()

	return nil
}

// handleUDP handles incoming UDP packets
func (h *UDPHandler) handleUDP() {
	buf := make([]byte, 65535)

	for {
		n, addr, err := h.packetConn.ReadFrom(buf)
		if err != nil {
			select {
			case <-h.ctx.Done():
				return
			default:
				h.logger.Error().Err(err).Msg("read UDP packet failed")
				continue
			}
		}

		// Make a copy of the data
		data := make([]byte, n)
		copy(data, buf[:n])

		go h.handlePacket(data, addr)
	}
}

// handlePacket handles a single UDP packet
func (h *UDPHandler) handlePacket(data []byte, addr net.Addr) {
	key := addr.String()

	// Get or create session
	sessionI, exists := h.sessions.Load(key)
	if !exists {
		session, err := h.createSession(addr)
		if err != nil {
			h.logger.Error().Err(err).Str("addr", addr.String()).Msg("create UDP session failed")
			return
		}
		sessionI = session
		h.sessions.Store(key, session)
	}

	session := sessionI.(*UDPSession)
	session.mu.Lock()
	session.lastActive = time.Now()
	session.mu.Unlock()

	// Write data to stream
	if _, err := session.stream.Write(data); err != nil {
		h.logger.Error().Err(err).Str("addr", addr.String()).Msg("write to stream failed")
		h.closeSession(key, session)
		return
	}
}

// createSession creates a new UDP session
func (h *UDPHandler) createSession(addr net.Addr) (*UDPSession, error) {
	logger := h.logger.With().
		Str("remote", addr.String()).
		Str("protocol", "udp").
		Logger()

	logger.Debug().Msg("new UDP session")

	// Select client from pool
	client, err := h.pool.Select()
	if err != nil {
		return nil, fmt.Errorf("select client: %w", err)
	}

	logger = logger.With().Str("client_id", client.ID).Logger()
	logger.Debug().Msg("selected client")

	// Open QUIC stream to client
	stream, err := client.Conn.OpenStreamSync(h.ctx)
	if err != nil {
		h.pool.MarkUnhealthy(client.ID)
		return nil, fmt.Errorf("open stream: %w", err)
	}

	// Send NewConn message
	connID := connid.Generate()
	err = protocol.WriteNewConn(
		stream,
		connID,
		"udp",
		addr.String(),
		uint16(h.port),
		time.Now().Unix(),
	)
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("send NewConn message: %w", err)
	}

	logger.Debug().Uint64("conn_id", connID).Msg("UDP session created")

	session := &UDPSession{
		clientAddr: addr,
		stream:     stream,
		client:     client,
		lastActive: time.Now(),
		connID:     connID,
	}

	// Track active connection
	client.ActiveConns.Add(1)
	client.TotalConns.Add(1)

	// Start reading responses from client
	go h.readResponses(session, addr)

	return session, nil
}

// readResponses reads responses from client and sends back to UDP client
func (h *UDPHandler) readResponses(session *UDPSession, addr net.Addr) {
	defer func() {
		session.client.ActiveConns.Add(-1)
		h.closeSession(addr.String(), session)
	}()

	buf := make([]byte, 65535)
	for {
		n, err := session.stream.Read(buf)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				h.logger.Debug().Err(err).Str("addr", addr.String()).Msg("read from stream failed")
			}
			return
		}

		session.mu.Lock()
		session.lastActive = time.Now()
		session.mu.Unlock()

		// Send response back to UDP client
		if _, err := h.packetConn.WriteTo(buf[:n], addr); err != nil {
			h.logger.Error().Err(err).Str("addr", addr.String()).Msg("write UDP response failed")
			return
		}
	}
}

// closeSession closes a UDP session
func (h *UDPHandler) closeSession(key string, session *UDPSession) {
	h.sessions.Delete(key)
	session.stream.Close()
	h.logger.Debug().Str("addr", key).Msg("UDP session closed")
}

// cleanupSessions periodically cleans up inactive sessions
func (h *UDPHandler) cleanupSessions() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			h.sessions.Range(func(key, value interface{}) bool {
				session := value.(*UDPSession)
				session.mu.Lock()
				inactive := now.Sub(session.lastActive) > h.timeout
				session.mu.Unlock()

				if inactive {
					h.logger.Debug().Str("addr", key.(string)).Msg("cleaning up inactive UDP session")
					h.closeSession(key.(string), session)
				}
				return true
			})
		}
	}
}
