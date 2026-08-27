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
	udpSessionTimeout  = 5 * time.Minute
	udpCleanupInterval = 30 * time.Second
)

// UDPSession represents a UDP session using QUIC datagrams.
type UDPSession struct {
	id            uint32
	clientAddr    *net.UDPAddr
	quicConn      *quic.Conn
	lastActive    atomic.Int64
	client        *pool.ClientConn
	fragIDCounter atomic.Uint32
}

func (s *UDPSession) updateLastActive() {
	s.lastActive.Store(time.Now().UnixNano())
}

func (s *UDPSession) isExpired(timeout time.Duration) bool {
	last := time.Unix(0, s.lastActive.Load())
	return time.Since(last) > timeout
}

// UDPHandler handles UDP traffic using QUIC datagrams.
type UDPHandler struct {
	sessions     sync.Map // string -> *UDPSession
	sessionsByID sync.Map // uint32 -> *UDPSession

	pool                *pool.ConnectionPool
	packetConn          *net.UDPConn
	addr                string
	enableFragmentation bool
	logger              zerolog.Logger
	ctx                 context.Context
	cancel              context.CancelFunc

	nextSessionID atomic.Uint32

	fragmentAssembler *protocol.ShardedFragmentAssembler
	closeOnce         sync.Once

	// lifecycleMu is the receiver registry gate. Once closed is set, no new
	// session or receiver can be registered, making receiverWG safe to join.
	lifecycleMu sync.Mutex
	closed      bool
	receivers   map[*quic.Conn]struct{}
	receiverWG  sync.WaitGroup
}

// bindUDP stages a UDP socket without starting handler goroutines.
func (l *Listener) bindUDP() error {
	addr, err := net.ResolveUDPAddr("udp", l.Addr)
	if err != nil {
		return fmt.Errorf("resolve UDP addr: %w", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen UDP: %w", err)
	}

	bufferSetters := []struct {
		name string
		set  func(int) error
	}{
		{name: "read", set: conn.SetReadBuffer},
		{name: "write", set: conn.SetWriteBuffer},
	}
	for _, setter := range bufferSetters {
		if err := setter.set(4 * 1024 * 1024); err != nil {
			l.logger.Warn().Err(err).Msg("set UDP " + setter.name + " buffer failed")
		}
	}

	l.UDPConn = conn
	return nil
}

func (l *Listener) startUDPHandler() {
	conn := l.UDPConn.(*net.UDPConn)
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
		receivers:           make(map[*quic.Conn]struct{}),
	}
	l.udpHandler = handler

	l.fixedWG.Add(2)
	go handler.readLoop(&l.fixedWG)
	go handler.cleanupLoop(&l.fixedWG)
	l.logger.Info().Str("protocol", "udp").Msg("UDP listener started with datagram support")
}

// readLoop reads UDP packets using pooled buffers.
func (h *UDPHandler) readLoop(wg *sync.WaitGroup) {
	defer wg.Done()
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

		h.processPacket(buf[:n], addr)
		protocol.PutReadBuffer(bufPtr)
	}
}

func (h *UDPHandler) processPacket(data []byte, addr *net.UDPAddr) {
	if h.ctx.Err() != nil {
		return
	}

	key := addr.String()
	if sessionI, ok := h.sessions.Load(key); ok {
		session := sessionI.(*UDPSession)
		session.updateLastActive()
		h.sendDatagrams(session, data)
		return
	}

	session, err := h.createSession(addr)
	if err != nil {
		if h.ctx.Err() == nil {
			h.logger.Error().Err(err).Str("addr", key).Msg("create UDP session failed")
		}
		return
	}
	h.sendDatagrams(session, data)
}

func (h *UDPHandler) sendDatagrams(session *UDPSession, data []byte) {
	if h.ctx.Err() != nil {
		return
	}
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
	defer protocol.ReleaseDatagramResults(datagrams)

	for _, dgram := range datagrams {
		if err := session.quicConn.SendDatagram(dgram.Data); err != nil {
			h.logger.Debug().Err(err).Uint32("session_id", session.id).Msg("send datagram failed")
			h.closeSession(session)
			return
		}
	}
}

func (h *UDPHandler) createSession(addr *net.UDPAddr) (*UDPSession, error) {
	client, err := h.pool.Select()
	if err != nil {
		return nil, fmt.Errorf("select client: %w", err)
	}

	session := &UDPSession{
		id:         h.nextSessionID.Add(1),
		clientAddr: addr,
		quicConn:   client.Conn,
		client:     client,
	}
	session.updateLastActive()

	h.lifecycleMu.Lock()
	if h.closed {
		h.lifecycleMu.Unlock()
		return nil, context.Canceled
	}
	h.sessions.Store(addr.String(), session)
	h.sessionsByID.Store(session.id, session)
	client.ActiveConns.Add(1)
	client.TotalConns.Add(1)

	_, receiverExists := h.receivers[client.Conn]
	if !receiverExists {
		h.receivers[client.Conn] = struct{}{}
		h.receiverWG.Add(1)
	}
	h.lifecycleMu.Unlock()

	if !receiverExists {
		go h.receiveDatagrams(client.Conn)
	}

	h.logger.Debug().
		Str("addr", addr.String()).
		Uint32("session_id", session.id).
		Str("client_id", client.ID).
		Msg("UDP session created")
	return session, nil
}

func (h *UDPHandler) receiveDatagrams(quicConn *quic.Conn) {
	defer func() {
		h.lifecycleMu.Lock()
		delete(h.receivers, quicConn)
		h.lifecycleMu.Unlock()
		h.receiverWG.Done()
	}()

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

		sessionID, payload, complete, err := protocol.DecodeAndAssembleUDPDatagram(dgram, h.fragmentAssembler)
		if err != nil {
			if h.ctx.Err() == nil {
				h.logger.Debug().Err(err).Msg("process datagram failed")
			}
			continue
		}
		if !complete {
			continue
		}

		sessionI, ok := h.sessionsByID.Load(sessionID)
		if !ok {
			continue
		}
		session := sessionI.(*UDPSession)
		session.updateLastActive()

		if _, err := h.packetConn.WriteToUDP(payload, session.clientAddr); err != nil && h.ctx.Err() == nil {
			h.logger.Debug().Err(err).Str("addr", session.clientAddr.String()).Msg("write UDP response failed")
		}
	}
}

func (h *UDPHandler) closeSession(session *UDPSession) {
	key := session.clientAddr.String()
	if !h.sessions.CompareAndDelete(key, session) {
		return
	}
	h.sessionsByID.CompareAndDelete(session.id, session)
	session.client.ActiveConns.Add(-1)

	h.logger.Debug().
		Str("addr", key).
		Uint32("session_id", session.id).
		Msg("UDP session closed")
}

func (h *UDPHandler) cleanupLoop(wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(udpCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			h.sessions.Range(func(key, value any) bool {
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

func (h *UDPHandler) close() {
	h.closeOnce.Do(func() {
		h.lifecycleMu.Lock()
		h.closed = true
		h.cancel()
		h.lifecycleMu.Unlock()

		_ = h.packetConn.Close()
		h.fragmentAssembler.Close()
		h.sessions.Range(func(_, value any) bool {
			h.closeSession(value.(*UDPSession))
			return true
		})
	})
}

func (h *UDPHandler) wait() {
	h.receiverWG.Wait()
}
