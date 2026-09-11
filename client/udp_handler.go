package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const (
	// Session timeout for inactive UDP sessions
	udpSessionTimeout = 5 * time.Minute
	// Cleanup interval for expired sessions
	udpCleanupInterval     = 30 * time.Second
	udpSessionSetupTimeout = 5 * time.Second
	udpSocketBufferSize    = 4 * 1024 * 1024
	udpPendingPacketLimit  = 8
	udpPendingBackingLimit = 255 * protocol.MaxFragPayload
)

var (
	errClientUDPEpochExhausted = errors.New("client UDP session epoch exhausted")
	errClientUDPStateChanged   = errors.New("client UDP session state changed during setup")
)

func setUDPSocketBuffer(logger zerolog.Logger, name string, setter func(int) error) {
	if err := setter(udpSocketBufferSize); err != nil {
		logger.Warn().Err(err).Msg("set UDP " + name + " buffer failed")
	}
}

type udpSessionBudget struct {
	mu               sync.Mutex
	slots            chan struct{}
	permitsHeld      int64
	publishedActive  int64
	maxPermitsHeld   int64
	limitDrops       uint64
	accountingFaults uint64
	decodeDrops      atomic.Uint64
	pendingDrops     atomic.Uint64
	createErrors     atomic.Uint64
	readErrors       atomic.Uint64
	writeErrors      atomic.Uint64
}

func (b *udpSessionBudget) snapshot() UDPSessionSnapshot {
	if b == nil {
		return UDPSessionSnapshot{}
	}
	b.mu.Lock()
	snapshot := UDPSessionSnapshot{
		Current:          b.publishedActive,
		Permits:          b.permitsHeld,
		HighWater:        b.maxPermitsHeld,
		Limit:            int64(cap(b.slots)),
		CapacityDrops:    b.limitDrops,
		AccountingFaults: b.accountingFaults,
	}
	b.mu.Unlock()
	snapshot.DecodeDrops = b.decodeDrops.Load()
	snapshot.PendingDrops = b.pendingDrops.Load()
	snapshot.CreateErrors = b.createErrors.Load()
	snapshot.ReadErrors = b.readErrors.Load()
	snapshot.WriteErrors = b.writeErrors.Load()
	return snapshot
}

type clientDsendStats struct {
	ownedItems          atomic.Int64
	ownedItemsHighWater atomic.Int64
	workers             atomic.Int64
	sendErrors          atomic.Uint64
	fragmentDrops       atomic.Uint64
}

func (s *clientDsendStats) releaseDatagrams(datagrams []protocol.DatagramResult, items int64) {
	protocol.ReleaseDatagramResults(datagrams)
	s.ownedItems.Add(-items)
}

func (s *clientDsendStats) worker(delta int64) {
	s.workers.Add(delta)
}

func (s *clientDsendStats) sendError() {
	s.sendErrors.Add(1)
}

func (s *clientDsendStats) load() DSendSnapshot {
	if s == nil {
		return DSendSnapshot{}
	}
	ownedItems := s.ownedItems.Load()
	ownedItemsHighWater := max(s.ownedItemsHighWater.Load(), ownedItems)
	bufferSize := int64(protocol.DatagramBufferSize)
	return DSendSnapshot{
		OwnedItems:            ownedItems,
		OwnedBacking:          ownedItems * bufferSize,
		OwnedItemsHighWater:   ownedItemsHighWater,
		OwnedBackingHighWater: ownedItemsHighWater * bufferSize,
		Workers:               s.workers.Load(),
		SendErrors:            s.sendErrors.Load(),
		FragmentDrops:         s.fragmentDrops.Load(),
	}
}

func newUDPSessionBudget(limit int) *udpSessionBudget {
	if limit <= 0 {
		limit = config.DefaultMaxLocalUDPSessions
	}
	return &udpSessionBudget{slots: make(chan struct{}, limit)}
}

func (b *udpSessionBudget) acquire() (func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.accountingFaults != 0 {
		return nil, false
	}
	select {
	case b.slots <- struct{}{}:
		b.permitsHeld++
		b.maxPermitsHeld = max(b.maxPermitsHeld, b.permitsHeld)
		return sync.OnceFunc(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			select {
			case <-b.slots:
				b.permitsHeld--
				if b.permitsHeld < 0 {
					b.permitsHeld++
					b.accountingFaults++
				}
			default:
				b.accountingFaults++
			}
		}), true
	default:
		b.limitDrops++
		return nil, false
	}
}

func (b *udpSessionBudget) publish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishedActive++
}

func (b *udpSessionBudget) unpublish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishedActive--
	if b.publishedActive < 0 {
		b.publishedActive++
		b.accountingFaults++
	}
}

// UDPSession represents a client-side UDP session
type UDPSession struct {
	id               uint32
	epoch            uint32
	localConn        *net.UDPConn
	quicConn         *quic.Conn
	lastActive       atomic.Int64
	fragmentSequence atomic.Uint32
}

type udpSessionPhase uint8

const (
	udpSessionPhaseCollecting udpSessionPhase = iota + 1
	udpSessionPhaseDraining
	udpSessionPhaseReady
	udpSessionPhaseClosed
)

type udpPendingPacket struct {
	payload       []byte
	retainedBytes int
}

type udpSessionState struct {
	id            uint32
	mu            sync.Mutex
	phase         udpSessionPhase
	fifo          []udpPendingPacket
	retainedBytes int
	candidate     *net.UDPConn
	session       *UDPSession
	releasePermit func()
}

type udpDispatchDecision uint8

const (
	udpDispatchQueued udpDispatchDecision = iota
	udpDispatchPendingDrop
	udpDispatchRetry
	udpDispatchReadyWrite
)

func (s *UDPSession) updateLastActive() {
	s.lastActive.Store(time.Now().UnixNano())
}

func (s *UDPSession) isExpired(timeout time.Duration) bool {
	last := time.Unix(0, s.lastActive.Load())
	return time.Since(last) > timeout
}

func (s *udpSessionState) acceptAndSnapshot(packet udpPendingPacket) (udpDispatchDecision, *UDPSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case udpSessionPhaseCollecting:
		if len(s.fifo) >= udpPendingPacketLimit || packet.retainedBytes < 0 || packet.retainedBytes > udpPendingBackingLimit-s.retainedBytes {
			return udpDispatchPendingDrop, nil
		}
		s.fifo = append(s.fifo, packet)
		s.retainedBytes += packet.retainedBytes
		return udpDispatchQueued, nil
	case udpSessionPhaseDraining:
		return udpDispatchPendingDrop, nil
	case udpSessionPhaseReady:
		if s.session != nil {
			return udpDispatchReadyWrite, s.session
		}
		return udpDispatchRetry, nil
	default:
		return udpDispatchRetry, nil
	}
}

func (s *udpSessionState) attachCandidate(candidate *net.UDPConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != udpSessionPhaseCollecting || s.candidate != nil {
		return false
	}
	s.candidate = candidate
	return true
}

func (s *udpSessionState) sealCollectingFIFO() ([]udpPendingPacket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != udpSessionPhaseCollecting {
		return nil, false
	}
	s.phase = udpSessionPhaseDraining
	batch := s.fifo
	s.fifo = nil
	s.retainedBytes = 0
	return batch, true
}

func (s *udpSessionState) publishReady(session *UDPSession, budget *udpSessionBudget) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != udpSessionPhaseDraining || s.candidate != session.localConn {
		return false
	}
	budget.publish()
	s.phase = udpSessionPhaseReady
	s.candidate = nil
	s.session = session
	return true
}

func (s *udpSessionState) readySession() *UDPSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != udpSessionPhaseReady {
		return nil
	}
	return s.session
}

func (s *udpSessionState) closeLocked() (candidate *net.UDPConn, published bool, closed bool) {
	if s.phase == udpSessionPhaseClosed {
		return nil, false, false
	}
	candidate = s.candidate
	if s.session != nil {
		candidate = s.session.localConn
		published = s.phase == udpSessionPhaseReady
	}
	clear(s.fifo)
	s.fifo = nil
	s.retainedBytes = 0
	s.candidate = nil
	s.session = nil
	s.phase = udpSessionPhaseClosed
	return candidate, published, true
}

func (s *udpSessionState) closePending() (*net.UDPConn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != udpSessionPhaseCollecting && s.phase != udpSessionPhaseDraining {
		return nil, false
	}
	candidate, _, closed := s.closeLocked()
	return candidate, closed
}

func (s *udpSessionState) closeReady(session *UDPSession) (*net.UDPConn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != udpSessionPhaseReady || s.session != session {
		return nil, false
	}
	candidate, _, closed := s.closeLocked()
	return candidate, closed
}

func (s *udpSessionState) close() (*net.UDPConn, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

// UDPHandler handles UDP datagram forwarding on the client side
type UDPHandler struct {
	// Sessions indexed by session ID.
	sessionsMu sync.Mutex
	sessions   map[uint32]*udpSessionState

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
	sessionWG            sync.WaitGroup
	sessionBudget        *udpSessionBudget
	epochAllocator       atomic.Uint32
	beforeSessionPublish func()
	dsendStats           *clientDsendStats
	done                 chan struct{}
	doneOnce             sync.Once

	// Fragment assembler for reassembling fragmented packets (sharded for reduced lock contention)
	fragmentAssembler *protocol.ShardedFragmentAssembler
}

// NewUDPHandler creates a new UDP handler
func NewUDPHandler(
	localHost string,
	localPort int,
	enableFragmentation bool,
	maxFragmentGroups int,
	maxFragmentBackingBytes int64,
	logger zerolog.Logger,
) *UDPHandler {
	return newUDPHandler(localHost, localPort, enableFragmentation, maxFragmentGroups, maxFragmentBackingBytes, logger, newUDPSessionBudget(0))
}

func newUDPHandler(
	localHost string,
	localPort int,
	enableFragmentation bool,
	maxFragmentGroups int,
	maxFragmentBackingBytes int64,
	logger zerolog.Logger,
	budget *udpSessionBudget,
	dsendStats ...*clientDsendStats,
) *UDPHandler {
	if budget == nil {
		budget = newUDPSessionBudget(0)
	}
	stats := &clientDsendStats{}
	if len(dsendStats) > 0 && dsendStats[0] != nil {
		stats = dsendStats[0]
	}
	return &UDPHandler{
		localHost:           localHost,
		localPort:           localPort,
		enableFragmentation: enableFragmentation,
		logger:              logger.With().Str("component", "udp_handler").Logger(),
		fragmentAssembler:   protocol.NewShardedFragmentAssembler(protocol.DefaultShardCount, maxFragmentGroups, maxFragmentBackingBytes),
		sessionBudget:       budget,
		sessions:            make(map[uint32]*udpSessionState),
		dsendStats:          stats,
		done:                make(chan struct{}),
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
	go func() {
		h.wait()
		h.doneOnce.Do(func() { close(h.done) })
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
		for _, state := range h.snapshotSessionStates() {
			h.closeStateExact(state)
		}
		if !h.started {
			h.doneOnce.Do(func() { close(h.done) })
		}
	})
}

func (h *UDPHandler) wait() {
	// Join the sole receiver producer before its session workers. The caller must first
	// close the owning QUIC connection because SendDatagram has no context;
	// otherwise a blocked worker also retains its shared-budget permit.
	h.fixedWG.Wait()
	h.sessionWG.Wait()
}

func (h *UDPHandler) stopAndWait() {
	h.Stop()
	h.wait()
}

// receiveDatagrams is only started after Start initializes h.ctx and is the
// sole production producer of session workers.
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
			h.recordDecodeError(err)
			h.logger.Debug().Err(err).Msg("process datagram failed")
			continue
		}
		if !complete {
			continue
		}

		retainedBytes := cap(payload)
		if dgram[0] == protocol.UDPDatagramTypeNormal {
			retainedBytes = len(dgram)
		}
		h.dispatchPayload(sessionID, udpPendingPacket{payload: payload, retainedBytes: retainedBytes}, quicConn)
	}
}

func (h *UDPHandler) recordDecodeError(err error) {
	if !errors.Is(err, protocol.ErrFragmentAssemblerFull) && !errors.Is(err, protocol.ErrFragmentAssemblerClosed) {
		h.sessionBudget.decodeDrops.Add(1)
	}
}

func (h *UDPHandler) loadSessionState(sessionID uint32) *udpSessionState {
	h.sessionsMu.Lock()
	defer h.sessionsMu.Unlock()
	return h.sessions[sessionID]
}

func (h *UDPHandler) snapshotSessionStates() []*udpSessionState {
	h.sessionsMu.Lock()
	defer h.sessionsMu.Unlock()
	states := make([]*udpSessionState, 0, len(h.sessions))
	for _, state := range h.sessions {
		states = append(states, state)
	}
	return states
}

func (h *UDPHandler) isExactSessionState(state *udpSessionState) bool {
	h.sessionsMu.Lock()
	defer h.sessionsMu.Unlock()
	return h.sessions[state.id] == state
}

func (h *UDPHandler) deleteExactSessionState(state *udpSessionState) bool {
	h.sessionsMu.Lock()
	defer h.sessionsMu.Unlock()
	if h.sessions[state.id] != state {
		return false
	}
	delete(h.sessions, state.id)
	return true
}

func (h *UDPHandler) installPendingOrLoad(sessionID uint32, packet udpPendingPacket) (*udpSessionState, bool, bool) {
	h.sessionsMu.Lock()
	defer h.sessionsMu.Unlock()
	if state := h.sessions[sessionID]; state != nil {
		return state, false, true
	}
	releasePermit, ok := h.sessionBudget.acquire()
	if !ok {
		return nil, false, false
	}
	state := &udpSessionState{
		id:            sessionID,
		phase:         udpSessionPhaseCollecting,
		fifo:          []udpPendingPacket{packet},
		retainedBytes: packet.retainedBytes,
		releasePermit: releasePermit,
	}
	h.sessions[sessionID] = state
	return state, true, true
}

func (h *UDPHandler) registerSessionProducer() bool {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if !h.started || h.closed || h.ctx == nil || h.ctx.Err() != nil {
		return false
	}
	h.sessionWG.Add(1)
	return true
}

func (h *UDPHandler) dispatchPayload(sessionID uint32, packet udpPendingPacket, quicConn *quic.Conn) {
	if packet.retainedBytes < 0 || packet.retainedBytes > udpPendingBackingLimit {
		h.sessionBudget.pendingDrops.Add(1)
		return
	}
	for range 2 {
		state := h.loadSessionState(sessionID)
		if state == nil {
			var installed, admitted bool
			state, installed, admitted = h.installPendingOrLoad(sessionID, packet)
			if !admitted {
				return
			}
			if installed {
				if !h.registerSessionProducer() {
					h.failPendingExact(state)
					state.releasePermit()
					return
				}
				go h.runSession(state, quicConn)
				return
			}
		}

		decision, session := state.acceptAndSnapshot(packet)
		switch decision {
		case udpDispatchQueued:
			return
		case udpDispatchPendingDrop:
			h.sessionBudget.pendingDrops.Add(1)
			return
		case udpDispatchRetry:
			h.deleteExactSessionState(state)
		case udpDispatchReadyWrite:
			session.updateLastActive()
			if _, err := session.localConn.Write(packet.payload); err != nil {
				h.recordWriteFailure(session.quicConn)
				h.logger.Debug().Err(err).Uint32("session_id", sessionID).Msg("write to local failed")
				h.closeSessionExact(state, session)
			}
			return
		}
	}
}

func (h *UDPHandler) runSession(state *udpSessionState, quicConn *quic.Conn) {
	defer h.sessionWG.Done()
	defer state.releasePermit()

	setupCtx, cancelSetup := context.WithTimeout(h.ctx, udpSessionSetupTimeout)
	defer cancelSetup()

	session, addr, err := h.createSessionCandidate(setupCtx, state, quicConn)
	if err != nil {
		h.recordCreateFailure(quicConn)
		h.logger.Debug().Err(err).Uint32("session_id", state.id).Msg("create UDP session failed")
		h.failPendingExact(state)
		return
	}
	deadline, _ := setupCtx.Deadline()
	if err := session.localConn.SetWriteDeadline(deadline); err != nil {
		h.recordCreateFailure(quicConn)
		h.logger.Debug().Err(err).Uint32("session_id", state.id).Msg("set UDP setup write deadline failed")
		h.failPendingExact(state)
		return
	}
	batch, ok := state.sealCollectingFIFO()
	if !ok {
		h.recordCreateFailure(quicConn)
		h.failPendingExact(state)
		return
	}
	defer clear(batch)
	for i := range batch {
		if _, err := session.localConn.Write(batch[i].payload); err != nil {
			h.recordWriteFailure(quicConn)
			h.logger.Debug().Err(err).Uint32("session_id", state.id).Msg("write initial UDP payload failed")
			h.failPendingExact(state)
			return
		}
		batch[i].payload = nil
		batch[i].retainedBytes = 0
		session.updateLastActive()
	}
	if err := session.localConn.SetWriteDeadline(time.Time{}); err != nil {
		h.recordCreateFailure(quicConn)
		h.logger.Debug().Err(err).Uint32("session_id", state.id).Msg("clear UDP setup write deadline failed")
		h.failPendingExact(state)
		return
	}

	if h.beforeSessionPublish != nil {
		h.beforeSessionPublish()
	}
	published, normalTeardown, err := h.publishReadyAfterGate(state, session, setupCtx)
	if !published {
		if !normalTeardown {
			h.recordCreateFailure(quicConn)
			h.logger.Debug().Err(err).Uint32("session_id", state.id).Msg("publish UDP session failed")
		}
		h.failPendingExact(state)
		return
	}

	h.dsendStats.worker(1)
	defer h.dsendStats.worker(-1)
	defer h.closeSessionExact(state, session)
	cancelSetup()
	h.logger.Debug().Uint32("session_id", state.id).Str("local_addr", addr).Msg("UDP session created")
	h.readLocalResponses(session)
}

func (h *UDPHandler) createSessionCandidate(ctx context.Context, state *udpSessionState, quicConn *quic.Conn) (*UDPSession, string, error) {
	addr, _, err := resolveServerAddress(ctx, net.DefaultResolver, net.JoinHostPort(h.localHost, strconv.Itoa(h.localPort)))
	if err != nil {
		return nil, "", err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, "", err
	}
	localConn, ok := conn.(*net.UDPConn)
	if !ok {
		_ = conn.Close()
		return nil, "", fmt.Errorf("dial UDP returned %T, want *net.UDPConn", conn)
	}
	if !state.attachCandidate(localConn) {
		_ = localConn.Close()
		return nil, "", errClientUDPStateChanged
	}
	setUDPSocketBuffer(h.logger, "read", localConn.SetReadBuffer)
	setUDPSocketBuffer(h.logger, "write", localConn.SetWriteBuffer)
	epoch, ok := protocol.AllocateUDPEpoch(&h.epochAllocator)
	if !ok {
		return nil, "", errClientUDPEpochExhausted
	}
	return &UDPSession{id: state.id, epoch: epoch, localConn: localConn, quicConn: quicConn}, addr, nil
}

func (h *UDPHandler) normalTeardownLocked(quicConn *quic.Conn) bool {
	if !h.started || h.closed || h.ctx == nil || h.ctx.Err() != nil {
		return true
	}
	return quicConn != nil && quicConn.Context().Err() != nil
}

func (h *UDPHandler) normalTeardown(quicConn *quic.Conn) bool {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	return h.normalTeardownLocked(quicConn)
}

func (h *UDPHandler) recordCreateFailure(quicConn *quic.Conn) {
	if !h.normalTeardown(quicConn) {
		h.sessionBudget.createErrors.Add(1)
	}
}

func (h *UDPHandler) recordWriteFailure(quicConn *quic.Conn) {
	if !h.normalTeardown(quicConn) {
		h.sessionBudget.writeErrors.Add(1)
	}
}

func (h *UDPHandler) publishReadyAfterGate(state *udpSessionState, session *UDPSession, setupCtx context.Context) (bool, bool, error) {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if h.normalTeardownLocked(session.quicConn) {
		return false, true, nil
	}
	if err := setupCtx.Err(); err != nil {
		return false, false, err
	}
	if !h.isExactSessionState(state) || !state.publishReady(session, h.sessionBudget) {
		return false, false, errClientUDPStateChanged
	}
	return true, false, nil
}

// readLocalResponses reads responses from local UDP service and sends back via datagram
func (h *UDPHandler) readLocalResponses(session *UDPSession) {
	bufPtr := protocol.GetReadBuffer()
	defer protocol.PutReadBuffer(bufPtr)
	buf := *bufPtr

	for {
		_ = session.localConn.SetReadDeadline(time.Now().Add(udpSessionTimeout))
		n, err := session.localConn.Read(buf)
		if err != nil {
			select {
			case <-h.ctx.Done():
				return
			default:
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					// Timeout - check if session is still active
					if session.isExpired(udpSessionTimeout) {
						return
					}
					continue
				}
				if !errors.Is(err, net.ErrClosed) && !h.normalTeardown(session.quicConn) {
					h.sessionBudget.readErrors.Add(1)
				}
				h.logger.Debug().Err(err).Uint32("session_id", session.id).Msg("read from local failed")
				return
			}
		}

		session.updateLastActive()

		// Fragment and send datagrams using pooled fragmentation (no mutex needed - atomic counter)
		datagrams, err := h.fragmentDatagrams(session.id, session.epoch, buf[:n], &session.fragmentSequence)

		if err != nil {
			h.logger.Debug().Err(err).Uint32("session_id", session.id).Int("size", n).Msg("fragment UDP failed")
			continue
		}

		if err := h.sendDatagrams(datagrams, session.quicConn.SendDatagram); err != nil {
			h.logger.Debug().Err(err).Uint32("session_id", session.id).Msg("send datagram failed")
			return
		}
	}
}

func (h *UDPHandler) fragmentDatagrams(sessionID, epoch uint32, payload []byte, sequence *atomic.Uint32) ([]protocol.DatagramResult, error) {
	datagrams, err := protocol.FragmentUDPPooled(sessionID, epoch, payload, sequence, h.enableFragmentation)
	if err != nil {
		h.dsendStats.fragmentDrops.Add(1)
		return nil, err
	}
	items := int64(len(datagrams))
	updateClientDsendMax(&h.dsendStats.ownedItemsHighWater, h.dsendStats.ownedItems.Add(items))
	return datagrams, nil
}

func updateClientDsendMax(counter *atomic.Int64, value int64) {
	for current := counter.Load(); value > current; current = counter.Load() {
		if counter.CompareAndSwap(current, value) {
			return
		}
	}
}

func (h *UDPHandler) sendDatagrams(datagrams []protocol.DatagramResult, send func([]byte) error) error {
	items := int64(len(datagrams))
	defer func() {
		h.dsendStats.releaseDatagrams(datagrams, items)
	}()
	for i := range datagrams {
		if err := send(datagrams[i].Data); err != nil {
			h.dsendStats.sendError()
			return err
		}
	}
	return nil
}

func (h *UDPHandler) failPendingExact(state *udpSessionState) {
	candidate, closed := state.closePending()
	if !closed {
		return
	}
	h.deleteExactSessionState(state)
	if candidate != nil {
		_ = candidate.Close()
	}
}

func (h *UDPHandler) closeSessionExact(state *udpSessionState, session *UDPSession) {
	candidate, closed := state.closeReady(session)
	if !closed {
		return
	}
	h.deleteExactSessionState(state)
	if candidate != nil {
		_ = candidate.Close()
	}
	h.sessionBudget.unpublish()
	h.logger.Debug().Uint32("session_id", state.id).Msg("UDP session closed")
}

func (h *UDPHandler) closeStateExact(state *udpSessionState) {
	candidate, published, closed := state.close()
	if !closed {
		return
	}
	h.deleteExactSessionState(state)
	if candidate != nil {
		_ = candidate.Close()
	}
	if published {
		h.sessionBudget.unpublish()
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
			for _, state := range h.snapshotSessionStates() {
				session := state.readySession()
				if session == nil {
					continue
				}
				if session.isExpired(udpSessionTimeout) {
					h.logger.Debug().Uint32("session_id", session.id).Msg("cleaning up expired UDP session")
					h.closeSessionExact(state, session)
				}
			}
		}
	}
}
