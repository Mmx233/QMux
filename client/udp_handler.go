package client

import (
	"context"
	"errors"
	"net"
	"strconv"
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
	udpCleanupInterval           = 30 * time.Second
	udpSocketBufferSize          = 4 * 1024 * 1024
	defaultClientUDPSessionLimit = 256
)

var errClientUDPSessionLimit = errors.New("client UDP session limit reached")

func setUDPSocketBuffer(logger zerolog.Logger, name string, setter func(int) error) {
	if err := setter(udpSocketBufferSize); err != nil {
		logger.Warn().Err(err).Msg("set UDP " + name + " buffer failed")
	}
}

type udpSessionBudget struct {
	slots            chan struct{}
	permitsHeld      atomic.Int64
	publishedActive  atomic.Int64
	maxPermitsHeld   atomic.Int64
	limitDrops       atomic.Uint64
	accountingFaults atomic.Uint64
}

func newUDPSessionBudget(limit int) *udpSessionBudget {
	if limit <= 0 {
		limit = defaultClientUDPSessionLimit
	}
	return &udpSessionBudget{slots: make(chan struct{}, limit)}
}

func (b *udpSessionBudget) acquire() (func(), bool) {
	select {
	case b.slots <- struct{}{}:
		held := b.permitsHeld.Add(1)
		updateUDPMax(&b.maxPermitsHeld, held)
		return sync.OnceFunc(func() {
			select {
			case <-b.slots:
				if b.permitsHeld.Add(-1) < 0 {
					b.permitsHeld.Add(1)
					b.accountingFaults.Add(1)
				}
			default:
				b.accountingFaults.Add(1)
			}
		}), true
	default:
		b.limitDrops.Add(1)
		return nil, false
	}
}

func (b *udpSessionBudget) publish() {
	b.publishedActive.Add(1)
}

func (b *udpSessionBudget) unpublish() {
	if b.publishedActive.Add(-1) < 0 {
		b.publishedActive.Add(1)
		b.accountingFaults.Add(1)
	}
}

func updateUDPMax(counter *atomic.Int64, value int64) {
	for current := counter.Load(); value > current; current = counter.Load() {
		if counter.CompareAndSwap(current, value) {
			return
		}
	}
}

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

	localHost            string
	localPort            int
	enableFragmentation  bool
	logger               zerolog.Logger
	ctx                  context.Context
	cancel               context.CancelFunc
	lifecycleMu          sync.Mutex
	started              bool
	closed               bool
	closeOnce            sync.Once
	fixedWG              sync.WaitGroup
	readerWG             sync.WaitGroup
	sessionBudget        *udpSessionBudget
	beforeSessionPublish func()

	// Fragment assembler for reassembling fragmented packets (sharded for reduced lock contention)
	fragmentAssembler *protocol.ShardedFragmentAssembler
}

// NewUDPHandler creates a new UDP handler
func NewUDPHandler(localHost string, localPort int, enableFragmentation bool, logger zerolog.Logger) *UDPHandler {
	return newUDPHandler(localHost, localPort, enableFragmentation, logger, newUDPSessionBudget(0))
}

func newUDPHandler(
	localHost string,
	localPort int,
	enableFragmentation bool,
	logger zerolog.Logger,
	budget *udpSessionBudget,
) *UDPHandler {
	if budget == nil {
		budget = newUDPSessionBudget(0)
	}
	return &UDPHandler{
		localHost:           localHost,
		localPort:           localPort,
		enableFragmentation: enableFragmentation,
		logger:              logger.With().Str("component", "udp_handler").Logger(),
		fragmentAssembler:   protocol.NewShardedFragmentAssembler(protocol.DefaultShardCount),
		sessionBudget:       budget,
	}
}

// Start starts the UDP handler for a QUIC connection
func (h *UDPHandler) Start(ctx context.Context, quicConn *quic.Conn) {
	h.lifecycleMu.Lock()
	if h.started || h.closed {
		h.lifecycleMu.Unlock()
		return
	}
	h.ctx, h.cancel = context.WithCancel(ctx)
	h.started = true
	h.fixedWG.Add(2)
	h.lifecycleMu.Unlock()

	go func() {
		defer h.fixedWG.Done()
		h.receiveDatagrams(quicConn)
	}()
	go func() {
		defer h.fixedWG.Done()
		h.cleanupLoop()
	}()
}

// Stop stops the UDP handler
func (h *UDPHandler) Stop() {
	h.closeOnce.Do(func() {
		h.lifecycleMu.Lock()
		h.closed = true
		cancel := h.cancel
		h.lifecycleMu.Unlock()

		if cancel != nil {
			cancel()
		}
		h.fragmentAssembler.Close()
		h.sessions.Range(func(_, value any) bool {
			h.closeSession(value.(*UDPSession))
			return true
		})
	})
}

func (h *UDPHandler) wait() {
	// Join the sole reader producer before its readers. The caller must first
	// close the owning QUIC connection because SendDatagram has no context;
	// otherwise a blocked reader also retains its shared-budget permit.
	h.fixedWG.Wait()
	h.readerWG.Wait()
}

func (h *UDPHandler) stopAndWait() {
	h.Stop()
	h.wait()
}

// receiveDatagrams is only started after Start initializes h.ctx and is the
// sole production producer of session readers.
func (h *UDPHandler) receiveDatagrams(quicConn *quic.Conn) {
	// This fixedWG goroutine can stop itself, but must not wait for itself.
	defer h.Stop()
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

		// Validate and, if needed, reassemble the datagram.
		sessionID, payload, complete, err := protocol.DecodeAndAssembleUDPDatagram(dgram, h.fragmentAssembler)
		if err != nil {
			h.logger.Debug().Err(err).Msg("process datagram failed")
			continue
		}
		if !complete {
			continue
		}

		// Get or create session
		session, err := h.getOrCreateSession(sessionID, quicConn)
		if err != nil {
			if errors.Is(err, errClientUDPSessionLimit) {
				continue
			}
			h.logger.Error().Err(err).Uint32("session_id", sessionID).Msg("get session failed")
			continue
		}

		session.updateLastActive()

		// Forward to local service
		if _, err := session.localConn.Write(payload); err != nil {
			h.logger.Debug().Err(err).Uint32("session_id", sessionID).Msg("write to local failed")
			h.closeSession(session)
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
	releasePermit, ok := h.sessionBudget.acquire()
	if !ok {
		return nil, errClientUDPSessionLimit
	}
	creatorOwnsPermit := true
	defer func() {
		if creatorOwnsPermit {
			releasePermit()
		}
	}()

	// Slow path: create new session
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(h.localHost, strconv.Itoa(h.localPort)))
	if err != nil {
		return nil, err
	}

	localConn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, err
	}

	// Increase UDP buffer sizes to handle large packets
	setUDPSocketBuffer(h.logger, "read", localConn.SetReadBuffer)
	setUDPSocketBuffer(h.logger, "write", localConn.SetWriteBuffer)

	session := &UDPSession{
		id:        sessionID,
		localConn: localConn,
		quicConn:  quicConn,
	}
	session.updateLastActive()
	if h.beforeSessionPublish != nil {
		h.beforeSessionPublish()
	}

	h.lifecycleMu.Lock()
	if !h.started || h.closed || h.ctx.Err() != nil {
		h.lifecycleMu.Unlock()
		_ = localConn.Close()
		return nil, context.Canceled
	}

	// Store session and register its reader while the lifecycle gate is open.
	actual, loaded := h.sessions.LoadOrStore(sessionID, session)
	if loaded {
		h.lifecycleMu.Unlock()
		_ = localConn.Close()
		return actual.(*UDPSession), nil
	}
	h.sessionBudget.publish()
	h.readerWG.Add(1)
	creatorOwnsPermit = false
	h.lifecycleMu.Unlock()

	h.logger.Debug().Uint32("session_id", sessionID).Str("local_addr", addr.String()).Msg("UDP session created")

	// Start reading responses from local service
	go func() {
		defer h.readerWG.Done()
		defer releasePermit()
		defer h.closeSession(session)
		h.readLocalResponses(session)
	}()

	return session, nil
}

// readLocalResponses reads responses from local UDP service and sends back via datagram
func (h *UDPHandler) readLocalResponses(session *UDPSession) {
	for {
		// Get buffer from pool at start of each iteration
		bufPtr := protocol.GetReadBuffer()
		buf := *bufPtr

		_ = session.localConn.SetReadDeadline(time.Now().Add(udpSessionTimeout))
		n, err := session.localConn.Read(buf)
		if err != nil {
			select {
			case <-h.ctx.Done():
				protocol.PutReadBuffer(bufPtr)
				return
			default:
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					// Timeout - check if session is still active
					if session.isExpired(udpSessionTimeout) {
						protocol.PutReadBuffer(bufPtr)
						h.closeSession(session)
						return
					}
					protocol.PutReadBuffer(bufPtr)
					continue
				}
				h.logger.Debug().Err(err).Uint32("session_id", session.id).Msg("read from local failed")
				protocol.PutReadBuffer(bufPtr)
				h.closeSession(session)
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
				h.closeSession(session)
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
func (h *UDPHandler) closeSession(session *UDPSession) {
	if !h.sessions.CompareAndDelete(session.id, session) {
		return
	}
	_ = session.localConn.Close()
	h.sessionBudget.unpublish()
	h.logger.Debug().Uint32("session_id", session.id).Msg("UDP session closed")
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
			h.sessions.Range(func(key, value any) bool {
				session := value.(*UDPSession)
				if session.isExpired(udpSessionTimeout) {
					h.logger.Debug().Uint32("session_id", session.id).Msg("cleaning up expired UDP session")
					h.closeSession(session)
				}
				return true
			})
		}
	}
}
