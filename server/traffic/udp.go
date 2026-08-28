package traffic

import (
	"context"
	"errors"
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
	udpSessionTimeout         = 5 * time.Minute
	udpCleanupInterval        = 30 * time.Second
	maxUDPSenderQueuedFrames  = 256
	maxUDPSenderQueuedBacking = 512 << 10
)

// UDPSession represents a UDP session using QUIC datagrams.
type UDPSession struct {
	id            uint32
	clientAddr    *net.UDPAddr
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

type udpSendBatch struct {
	datagrams []protocol.DatagramResult
	backing   int64
}

type udpSender struct {
	client *pool.ClientConn
	queue  chan udpSendBatch
	done   chan struct{}

	mu             sync.Mutex
	accepting      bool
	ownedFrames    int64
	ownedBacking   int64
	inFlightFrames int64
	inFlightSeq    uint64
}

type udpSenderStats struct {
	queueFullDrops   atomic.Uint64
	sendErrors       atomic.Uint64
	noEligibleDrops  atomic.Uint64
	fragmentDrops    atomic.Uint64
	decodeDrops      atomic.Uint64
	unknownSession   atomic.Uint64
	publicWriteDrops atomic.Uint64
	queuedFrames     atomic.Int64
	queuedBacking    atomic.Int64
	workers          atomic.Int64
	maxQueuedFrames  atomic.Int64
	maxQueuedBytes   atomic.Int64
}

type udpEnqueueResult uint8

const (
	udpEnqueued udpEnqueueResult = iota
	udpQueueFull
	udpSenderUnavailable
)

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

	// lifecycleMu is the worker registry gate. Once closed is set, no new
	// session, receiver, or sender can be registered, making the wait groups safe to join.
	lifecycleMu sync.Mutex
	closed      bool
	receivers   map[*quic.Conn]struct{}
	senders     map[*pool.ClientConn]*udpSender
	receiverWG  sync.WaitGroup
	senderWG    sync.WaitGroup
	senderStats udpSenderStats
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
		senders:             make(map[*pool.ClientConn]*udpSender),
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
	if !h.enableFragmentation && len(data) > protocol.MaxUDPPayload {
		h.senderStats.fragmentDrops.Add(1)
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
		if h.ctx.Err() == nil && (errors.Is(err, pool.ErrNoClientsAvailable) ||
			errors.Is(err, pool.ErrNoEligibleClients) || errors.Is(err, pool.ErrNoHealthyClients)) {
			h.senderStats.noEligibleDrops.Add(1)
		} else if h.ctx.Err() == nil {
			h.logger.Debug().Err(err).Str("addr", key).Msg("create UDP session failed")
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
		h.senderStats.fragmentDrops.Add(1)
		h.logger.Debug().Err(err).Uint32("session_id", session.id).Int("size", len(data)).Msg("fragment UDP failed")
		h.closeSession(session)
		return
	}

	batch := udpSendBatch{
		datagrams: datagrams,
		backing:   datagramBackingBytes(datagrams),
	}
	sender := h.senderFor(session.client)
	if sender == nil {
		protocol.ReleaseDatagramResults(datagrams)
		h.closeSession(session)
		return
	}
	switch h.enqueueSender(sender, batch) {
	case udpEnqueued:
		return
	case udpQueueFull:
		h.senderStats.queueFullDrops.Add(1)
	case udpSenderUnavailable:
		h.closeSession(session)
	}
	protocol.ReleaseDatagramResults(datagrams)
}

func (h *UDPHandler) createSession(addr *net.UDPAddr) (*UDPSession, error) {
	client, err := h.pool.SelectProtocol("udp")
	if err != nil {
		return nil, fmt.Errorf("select client: %w", err)
	}

	session := &UDPSession{
		id:         h.nextSessionID.Add(1),
		clientAddr: addr,
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

func datagramBackingBytes(datagrams []protocol.DatagramResult) int64 {
	var total int64
	for i := range datagrams {
		if datagrams[i].Buffer != nil {
			total += int64(cap(*datagrams[i].Buffer))
		} else {
			total += int64(cap(datagrams[i].Data))
		}
	}
	return total
}

func (h *UDPHandler) senderFor(client *pool.ClientConn) *udpSender {
	if client == nil || client.Conn == nil || client.Conn.Context().Err() != nil {
		return nil
	}

	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if h.closed || h.ctx.Err() != nil || client.Conn.Context().Err() != nil {
		return nil
	}
	if sender := h.senders[client]; sender != nil {
		return sender
	}

	sender := &udpSender{
		client:    client,
		queue:     make(chan udpSendBatch, maxUDPSenderQueuedFrames),
		done:      make(chan struct{}),
		accepting: true,
	}
	h.senders[client] = sender
	h.senderWG.Add(1)
	h.senderStats.workers.Add(1)
	go h.runSender(sender)
	return sender
}

func (h *UDPHandler) enqueueSender(sender *udpSender, batch udpSendBatch) udpEnqueueResult {
	frames := int64(len(batch.datagrams))
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if !sender.accepting {
		return udpSenderUnavailable
	}
	if sender.ownedFrames+frames > maxUDPSenderQueuedFrames ||
		sender.ownedBacking+batch.backing > maxUDPSenderQueuedBacking {
		return udpQueueFull
	}

	sender.ownedFrames += frames
	sender.ownedBacking += batch.backing
	queuedFrames := h.senderStats.queuedFrames.Add(frames)
	queuedBacking := h.senderStats.queuedBacking.Add(batch.backing)
	updateAtomicMax(&h.senderStats.maxQueuedFrames, queuedFrames)
	updateAtomicMax(&h.senderStats.maxQueuedBytes, queuedBacking)
	select {
	case sender.queue <- batch:
		return udpEnqueued
	default:
		sender.ownedFrames -= frames
		sender.ownedBacking -= batch.backing
		h.senderStats.queuedFrames.Add(-frames)
		h.senderStats.queuedBacking.Add(-batch.backing)
		return udpQueueFull
	}
}

func updateAtomicMax(counter *atomic.Int64, value int64) {
	for current := counter.Load(); value > current; current = counter.Load() {
		if counter.CompareAndSwap(current, value) {
			return
		}
	}
}

func (h *UDPHandler) runSender(sender *udpSender) {
	defer h.finishSender(sender)
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-sender.client.Conn.Context().Done():
			return
		case batch := <-sender.queue:
			sender.mu.Lock()
			accepting := sender.accepting
			if accepting {
				sender.inFlightFrames = int64(len(batch.datagrams))
				sender.inFlightSeq++
			}
			sender.mu.Unlock()
			if !accepting {
				h.releaseSenderBatch(sender, batch)
				return
			}
			terminal, err := h.sendBatch(sender.client.Conn, batch)
			h.releaseSenderBatch(sender, batch)
			if err == nil {
				continue
			}
			h.senderStats.sendErrors.Add(1)
			if !terminal {
				continue
			}
			h.logger.Debug().Err(err).Str("client_id", sender.client.ID).Msg("UDP sender stopped")
			h.failSender(sender)
			select {
			case <-h.ctx.Done():
			case <-sender.client.Conn.Context().Done():
			}
			return
		}
	}
}

func (h *UDPHandler) sendBatch(conn *quic.Conn, batch udpSendBatch) (bool, error) {
	for i := range batch.datagrams {
		if err := conn.SendDatagram(batch.datagrams[i].Data); err != nil {
			var tooLarge *quic.DatagramTooLargeError
			return !errors.As(err, &tooLarge), err
		}
	}
	return false, nil
}

func (h *UDPHandler) releaseSenderBatch(sender *udpSender, batch udpSendBatch) {
	protocol.ReleaseDatagramResults(batch.datagrams)
	frames := int64(len(batch.datagrams))
	sender.mu.Lock()
	sender.inFlightFrames = 0
	sender.ownedFrames -= frames
	sender.ownedBacking -= batch.backing
	h.senderStats.queuedFrames.Add(-frames)
	h.senderStats.queuedBacking.Add(-batch.backing)
	sender.mu.Unlock()
}

func (h *UDPHandler) failSender(sender *udpSender) {
	sender.mu.Lock()
	sender.accepting = false
	queued := make([]udpSendBatch, 0, len(sender.queue))
	for {
		select {
		case batch := <-sender.queue:
			sender.ownedFrames -= int64(len(batch.datagrams))
			sender.ownedBacking -= batch.backing
			h.senderStats.queuedFrames.Add(-int64(len(batch.datagrams)))
			h.senderStats.queuedBacking.Add(-batch.backing)
			queued = append(queued, batch)
		default:
			sender.mu.Unlock()
			for i := range queued {
				protocol.ReleaseDatagramResults(queued[i].datagrams)
			}
			h.closeClientSessions(sender.client)
			return
		}
	}
}

func (h *UDPHandler) finishSender(sender *udpSender) {
	h.failSender(sender)
	h.lifecycleMu.Lock()
	if h.senders[sender.client] == sender {
		delete(h.senders, sender.client)
	}
	h.lifecycleMu.Unlock()
	h.senderStats.workers.Add(-1)
	close(sender.done)
	h.senderWG.Done()
}

func (h *UDPHandler) closeClientSessions(client *pool.ClientConn) {
	h.sessions.Range(func(_, value any) bool {
		session := value.(*UDPSession)
		if session.client == client {
			h.closeSession(session)
		}
		return true
	})
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
			h.senderStats.decodeDrops.Add(1)
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
			h.senderStats.unknownSession.Add(1)
			continue
		}
		session := sessionI.(*UDPSession)
		session.updateLastActive()

		if _, err := h.packetConn.WriteToUDP(payload, session.clientAddr); err != nil && h.ctx.Err() == nil {
			h.senderStats.publicWriteDrops.Add(1)
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
		for _, sender := range h.senders {
			h.failSender(sender)
		}
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
	h.senderWG.Wait()
}
