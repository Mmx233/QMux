package traffic

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

type dropWritesPacketConn struct {
	net.PacketConn
	drop    atomic.Bool
	dropped atomic.Uint64
}

func (c *dropWritesPacketConn) WriteTo(payload []byte, addr net.Addr) (int, error) {
	if c.drop.Load() {
		c.dropped.Add(1)
		return len(payload), nil
	}
	return c.PacketConn.WriteTo(payload, addr)
}

type scriptedUDPBalancer struct {
	mu   sync.Mutex
	ids  []string
	next int
}

func (b *scriptedUDPBalancer) Select(clients []*pool.ClientConn) (*pool.ClientConn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.next >= len(b.ids) {
		return nil, fmt.Errorf("UDP balancer script exhausted")
	}
	want := b.ids[b.next]
	b.next++
	for _, client := range clients {
		if client.ID == want {
			return client, nil
		}
	}
	return nil, fmt.Errorf("UDP balancer client %q unavailable", want)
}

func (*scriptedUDPBalancer) Name() string { return "scripted-udp-test" }

type udpSenderQUICPair struct {
	server  *quic.Conn
	peer    *quic.Conn
	dropper *dropWritesPacketConn
}

func newUDPSenderQUICPair(t *testing.T, ctx context.Context) *udpSenderQUICPair {
	t.Helper()
	serverTLS, peerTLS := relayLifecycleTLSConfigs(t)
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen UDP for QUIC pair: %v", err)
	}
	dropper := &dropWritesPacketConn{PacketConn: udpConn}
	transport := &quic.Transport{Conn: dropper}
	quicConfig := &quic.Config{
		EnableDatagrams:      true,
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       time.Minute,
	}
	listener, err := transport.Listen(serverTLS, quicConfig)
	if err != nil {
		_ = transport.Close()
		_ = udpConn.Close()
		t.Fatalf("listen for UDP sender QUIC pair: %v", err)
	}

	accepted := make(chan acceptedQUICConn, 1)
	go func() {
		conn, acceptErr := listener.Accept(ctx)
		accepted <- acceptedQUICConn{conn: conn, err: acceptErr}
	}()
	peer, err := quic.DialAddr(ctx, listener.Addr().String(), peerTLS, quicConfig)
	if err != nil {
		_ = listener.Close()
		_ = transport.Close()
		_ = udpConn.Close()
		t.Fatalf("dial UDP sender QUIC pair: %v", err)
	}
	var server *quic.Conn
	t.Cleanup(func() {
		_ = peer.CloseWithError(0, "test complete")
		if server != nil {
			_ = server.CloseWithError(0, "test complete")
		}
		_ = listener.Close()
		_ = transport.Close()
		_ = udpConn.Close()
	})
	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatalf("accept UDP sender QUIC pair: %v", result.err)
		}
		server = result.conn
	case <-ctx.Done():
		t.Fatalf("accept UDP sender QUIC pair: %v", context.Cause(ctx))
	}
	return &udpSenderQUICPair{server: server, peer: peer, dropper: dropper}
}

func awaitUDPCondition(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", description)
		case <-ticker.C:
		}
	}
}

func fragmentUDPSenderBatch(t *testing.T, sessionID uint32, payload []byte) udpSendBatch {
	t.Helper()
	var counter atomic.Uint32
	datagrams, err := protocol.FragmentUDPPooled(sessionID, payload, &counter, true)
	if err != nil {
		t.Fatalf("fragment UDP sender batch: %v", err)
	}
	return udpSendBatch{datagrams: datagrams, backing: datagramBackingBytes(datagrams)}
}

func TestUDPSenderWholeBatchAdmissionAndOwnership(t *testing.T) {
	t.Run("frame limit", func(t *testing.T) {
		handler := &UDPHandler{logger: zerolog.Nop()}
		sender := &udpSender{
			queue:     make(chan udpSendBatch, maxUDPSenderQueuedFrames),
			accepting: true,
		}
		batches := make([]udpSendBatch, 0, 5)
		for i := range 5 {
			batch := fragmentUDPSenderBatch(t, uint32(i+1), make([]byte, 65535))
			batches = append(batches, batch)
			result := handler.enqueueSender(sender, batch)
			if i < 4 && result != udpEnqueued {
				t.Fatalf("enqueue max-size batch %d = %v, want accepted", i, result)
			}
			if i == 4 {
				if result != udpQueueFull {
					t.Fatalf("enqueue over frame limit = %v, want queue full", result)
				}
				protocol.ReleaseDatagramResults(batch.datagrams)
			}
		}
		if got, want := len(batches[0].datagrams), 56; got != want {
			t.Fatalf("fragments per max-size packet = %d, want %d", got, want)
		}
		if sender.ownedFrames != 224 {
			t.Fatalf("owned frames after whole-batch rejection = %d, want 224", sender.ownedFrames)
		}
		if sender.ownedBacking != 224*protocol.DefaultDatagramBufferSize {
			t.Fatalf("owned backing after whole-batch rejection = %d, want %d", sender.ownedBacking, 224*protocol.DefaultDatagramBufferSize)
		}

		handler.failSender(sender)
		if sender.ownedFrames != 0 || sender.ownedBacking != 0 {
			t.Fatalf("ownership after drain = (%d frames, %d bytes), want zero", sender.ownedFrames, sender.ownedBacking)
		}
		if handler.senderStats.queuedFrames.Load() != 0 || handler.senderStats.queuedBacking.Load() != 0 {
			t.Fatalf("global queued accounting after drain = (%d frames, %d bytes), want zero",
				handler.senderStats.queuedFrames.Load(), handler.senderStats.queuedBacking.Load())
		}
		for i := range 4 {
			for j := range batches[i].datagrams {
				if batches[i].datagrams[j].Buffer != nil {
					t.Fatalf("batch %d datagram %d retained pooled ownership after drain", i, j)
				}
			}
		}
	})

	t.Run("backing limit uses capacity", func(t *testing.T) {
		handler := &UDPHandler{logger: zerolog.Nop()}
		sender := &udpSender{
			queue:     make(chan udpSendBatch, maxUDPSenderQueuedFrames),
			accepting: true,
		}
		first := udpSendBatch{datagrams: []protocol.DatagramResult{{Data: make([]byte, 1, maxUDPSenderQueuedBacking)}}}
		first.backing = datagramBackingBytes(first.datagrams)
		if result := handler.enqueueSender(sender, first); result != udpEnqueued {
			t.Fatalf("enqueue batch at backing limit = %v, want accepted", result)
		}
		second := udpSendBatch{datagrams: []protocol.DatagramResult{{Data: make([]byte, 1, 2)}}}
		second.backing = datagramBackingBytes(second.datagrams)
		if result := handler.enqueueSender(sender, second); result != udpQueueFull {
			t.Fatalf("enqueue batch over backing limit = %v, want queue full", result)
		}
		if sender.ownedFrames != 1 || sender.ownedBacking != maxUDPSenderQueuedBacking {
			t.Fatalf("ownership after backing rejection = (%d frames, %d bytes)", sender.ownedFrames, sender.ownedBacking)
		}
		handler.failSender(sender)
		if sender.ownedFrames != 0 || sender.ownedBacking != 0 {
			t.Fatalf("ownership after backing drain = (%d frames, %d bytes), want zero", sender.ownedFrames, sender.ownedBacking)
		}
	})
}

func TestUDPSenderRejectsOversizedPacketBeforeSessionLookup(t *testing.T) {
	oversized := make([]byte, protocol.MaxUDPPayload+1)

	t.Run("without session", func(t *testing.T) {
		handler := &UDPHandler{
			ctx:     context.Background(),
			logger:  zerolog.Nop(),
			senders: make(map[*pool.ClientConn]*udpSender),
		}
		handler.processPacket(oversized, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345})

		if got := handler.senderStats.fragmentDrops.Load(); got != 1 {
			t.Fatalf("fragment drops = %d, want 1", got)
		}
		if sessions, sessionsByID := syncMapLen(&handler.sessions), syncMapLen(&handler.sessionsByID); sessions != 0 || sessionsByID != 0 {
			t.Fatalf("sessions after oversized packet = (%d addresses, %d IDs), want zero", sessions, sessionsByID)
		}
		if len(handler.senders) != 0 || handler.senderStats.workers.Load() != 0 {
			t.Fatalf("sender state after oversized packet = (%d senders, %d workers), want zero",
				len(handler.senders), handler.senderStats.workers.Load())
		}
	})

	t.Run("with existing session", func(t *testing.T) {
		client := &pool.ClientConn{ID: "fragment-client"}
		client.ActiveConns.Store(1)
		clientAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12346}
		session := &UDPSession{id: 7, clientAddr: clientAddr, client: client}
		handler := &UDPHandler{
			ctx:     context.Background(),
			logger:  zerolog.Nop(),
			senders: make(map[*pool.ClientConn]*udpSender),
		}
		handler.sessions.Store(clientAddr.String(), session)
		handler.sessionsByID.Store(session.id, session)

		handler.processPacket(oversized, clientAddr)

		gotByAddr, okByAddr := handler.sessions.Load(clientAddr.String())
		gotByID, okByID := handler.sessionsByID.Load(session.id)
		if !okByAddr || gotByAddr != session || !okByID || gotByID != session {
			t.Fatalf("existing session changed after oversized packet: address=(%p, %v), ID=(%p, %v), want %p",
				gotByAddr, okByAddr, gotByID, okByID, session)
		}
		if got := client.ActiveConns.Load(); got != 1 {
			t.Fatalf("active connections after oversized packet = %d, want 1", got)
		}
		if got := handler.senderStats.fragmentDrops.Load(); got != 1 {
			t.Fatalf("fragment drops = %d, want 1", got)
		}
		if len(handler.senders) != 0 || handler.senderStats.workers.Load() != 0 {
			t.Fatalf("sender state after oversized packet = (%d senders, %d workers), want zero",
				len(handler.senders), handler.senderStats.workers.Load())
		}
	})
}

func TestUDPSenderFragmentFailureClosesPublishedSession(t *testing.T) {
	client := &pool.ClientConn{ID: "fragment-client"}
	client.ActiveConns.Store(1)
	clientAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12347}
	session := &UDPSession{id: 8, clientAddr: clientAddr, client: client}
	handler := &UDPHandler{
		ctx:     context.Background(),
		logger:  zerolog.Nop(),
		senders: make(map[*pool.ClientConn]*udpSender),
	}
	handler.sessions.Store(clientAddr.String(), session)
	handler.sessionsByID.Store(session.id, session)

	handler.sendDatagrams(session, make([]byte, protocol.MaxUDPPayload+1))

	if _, ok := handler.sessions.Load(clientAddr.String()); ok {
		t.Fatal("fragment failure retained address session")
	}
	if _, ok := handler.sessionsByID.Load(session.id); ok {
		t.Fatal("fragment failure retained ID session")
	}
	if got := client.ActiveConns.Load(); got != 0 {
		t.Fatalf("active connections after fragment failure = %d, want 0", got)
	}
	if got := handler.senderStats.fragmentDrops.Load(); got != 1 {
		t.Fatalf("fragment drops = %d, want 1", got)
	}
	if len(handler.senders) != 0 || handler.senderStats.workers.Load() != 0 {
		t.Fatalf("sender state after fragment failure = (%d senders, %d workers), want zero",
			len(handler.senders), handler.senderStats.workers.Load())
	}
}

func syncMapLen(m *sync.Map) int {
	count := 0
	m.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

func echoOneUDPDatagram(ctx context.Context, conn *quic.Conn) <-chan error {
	done := make(chan error, 1)
	go func() {
		datagram, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			done <- fmt.Errorf("receive QUIC datagram: %w", err)
			return
		}
		sessionID, payload, complete, err := protocol.DecodeAndAssembleUDPDatagram(datagram, nil)
		if err != nil {
			done <- fmt.Errorf("decode QUIC datagram: %w", err)
			return
		}
		if !complete {
			done <- fmt.Errorf("received incomplete QUIC datagram")
			return
		}
		var counter atomic.Uint32
		response, err := protocol.FragmentUDPPooled(sessionID, payload, &counter, true)
		if err != nil {
			done <- fmt.Errorf("fragment QUIC echo: %w", err)
			return
		}
		defer protocol.ReleaseDatagramResults(response)
		for i := range response {
			if err := conn.SendDatagram(response[i].Data); err != nil {
				done <- fmt.Errorf("send QUIC echo: %w", err)
				return
			}
		}
		done <- nil
	}()
	return done
}

func TestUDPSenderBlackholeIsolationAndManagerRetirement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hotPair := newUDPSenderQUICPair(t, ctx)
	coldPair := newUDPSenderQUICPair(t, ctx)
	balancer := &scriptedUDPBalancer{ids: []string{"hot", "cold"}}
	connectionPool := pool.New("udp-sender-test", balancer, zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	hotClient := &pool.ClientConn{ID: "hot", Conn: hotPair.server, Metadata: pool.ClientMetadata{Capabilities: []string{"udp"}}}
	coldClient := &pool.ClientConn{ID: "cold", Conn: coldPair.server, Metadata: pool.ClientMetadata{Capabilities: []string{"udp"}}}
	if err := connectionPool.Add(hotClient); err != nil {
		t.Fatalf("add hot UDP generation: %v", err)
	}
	if err := connectionPool.Add(coldClient); err != nil {
		t.Fatalf("add cold UDP generation: %v", err)
	}

	manager := NewManager(&config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    "udp-sender-test",
		TrafficAddr: "127.0.0.1:0",
		Protocol:    "udp",
	}}}, map[string]*pool.ConnectionPool{"udp-sender-test": connectionPool}, zerolog.Nop())
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start UDP sender manager: %v", err)
	}
	managerStopped := false
	t.Cleanup(func() {
		if managerStopped {
			return
		}
		manager.Close()
		_ = hotPair.peer.CloseWithError(0, "test cleanup")
		_ = coldPair.peer.CloseWithError(0, "test cleanup")
		manager.Wait()
	})
	handler := manager.listeners[0].udpHandler
	publicAddr := manager.listeners[0].UDPConn.LocalAddr().(*net.UDPAddr)
	hotPublic, err := net.DialUDP("udp", nil, publicAddr)
	if err != nil {
		t.Fatalf("dial hot public UDP flow: %v", err)
	}
	t.Cleanup(func() {
		if err := hotPublic.Close(); err != nil {
			t.Errorf("close hot public UDP flow: %v", err)
		}
	})
	coldPublic, err := net.DialUDP("udp", nil, publicAddr)
	if err != nil {
		t.Fatalf("dial cold public UDP flow: %v", err)
	}
	t.Cleanup(func() {
		if err := coldPublic.Close(); err != nil {
			t.Errorf("close cold public UDP flow: %v", err)
		}
	})
	if err := hotPublic.SetWriteDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatalf("set hot public UDP deadline: %v", err)
	}

	hotPair.dropper.drop.Store(true)
	hotPayload := make([]byte, protocol.MaxUDPPayload)
	for i := range hotPayload {
		hotPayload[i] = byte(i)
	}
	fillDeadline := time.Now().Add(8 * time.Second)
	var hotSender *udpSender
	for {
		for range 128 {
			if _, err := hotPublic.Write(hotPayload); err != nil {
				t.Fatalf("write hot public UDP packet: %v", err)
			}
		}
		if handler.senderStats.queueFullDrops.Load() > 10 {
			handler.lifecycleMu.Lock()
			hotSender = handler.senders[hotClient]
			handler.lifecycleMu.Unlock()
			if hotSender != nil {
				hotSender.mu.Lock()
				full := hotSender.ownedFrames == maxUDPSenderQueuedFrames
				hotSender.mu.Unlock()
				if full {
					break
				}
			}
		}
		if time.Now().After(fillDeadline) {
			t.Fatalf("hot sender did not reach bounded drop state: drops=%d transport_drops=%d",
				handler.senderStats.queueFullDrops.Load(), hotPair.dropper.dropped.Load())
		}
	}
	pressureStart := time.Now()
	dropsBeforePressure := handler.senderStats.queueFullDrops.Load()
	for time.Since(pressureStart) < 100*time.Millisecond ||
		handler.senderStats.queueFullDrops.Load() < dropsBeforePressure+100 {
		for range 32 {
			if _, err := hotPublic.Write(hotPayload); err != nil {
				t.Fatalf("sustain hot public UDP pressure: %v", err)
			}
		}
		if time.Now().After(fillDeadline) {
			t.Fatalf("hot sender drops did not remain stable under pressure: before=%d after=%d",
				dropsBeforePressure, handler.senderStats.queueFullDrops.Load())
		}
		hotSender.mu.Lock()
		frames, backing := hotSender.ownedFrames, hotSender.ownedBacking
		hotSender.mu.Unlock()
		if frames > maxUDPSenderQueuedFrames || backing > maxUDPSenderQueuedBacking {
			t.Fatalf("hot sender exceeded budget under pressure: %d frames, %d bytes", frames, backing)
		}
	}
	awaitUDPCondition(t, time.Second, "real QUIC blackhole writes", func() bool {
		return hotPair.dropper.dropped.Load() > 0
	})
	if hotClient.Conn.Context().Err() != nil {
		t.Fatalf("hot generation retired while only outbound packets were blackholed: %v", context.Cause(hotClient.Conn.Context()))
	}
	// The cold sender doesn't exist yet, so these listener aggregates equal the hot generation.
	if got := handler.senderStats.maxQueuedFrames.Load(); got > maxUDPSenderQueuedFrames {
		t.Fatalf("hot sender high-water frames = %d, limit %d", got, maxUDPSenderQueuedFrames)
	}
	if got := handler.senderStats.maxQueuedBytes.Load(); got > maxUDPSenderQueuedBacking {
		t.Fatalf("hot sender high-water backing = %d, limit %d", got, maxUDPSenderQueuedBacking)
	}

	coldEcho := echoOneUDPDatagram(ctx, coldPair.peer)
	coldPayload := []byte("cold-generation-echo")
	if err := coldPublic.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set cold public UDP deadline: %v", err)
	}
	if _, err := coldPublic.Write(coldPayload); err != nil {
		t.Fatalf("write cold public UDP packet: %v", err)
	}
	response := make([]byte, 256)
	n, err := coldPublic.Read(response)
	if err != nil {
		t.Fatalf("read cold public UDP response: %v", err)
	}
	if got := string(response[:n]); got != string(coldPayload) {
		t.Fatalf("cold public UDP response = %q, want %q", got, coldPayload)
	}
	select {
	case err := <-coldEcho:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("cold QUIC echo did not finish: %v", context.Cause(ctx))
	}

	parkDeadline := time.Now().Add(5 * time.Second)
	var parkedSince time.Time
	var parkedSeq uint64
	var dropsAtPark uint64
	for {
		for range 32 {
			if _, err := hotPublic.Write(hotPayload); err != nil {
				t.Fatalf("park hot QUIC sender: %v", err)
			}
		}
		hotSender.mu.Lock()
		ownedFrames := hotSender.ownedFrames
		inFlightFrames := hotSender.inFlightFrames
		inFlightSeq := hotSender.inFlightSeq
		hotSender.mu.Unlock()
		drops := handler.senderStats.queueFullDrops.Load()
		if ownedFrames == maxUDPSenderQueuedFrames && inFlightFrames > 0 && inFlightSeq != 0 {
			if parkedSince.IsZero() || parkedSeq != inFlightSeq {
				parkedSince = time.Now()
				parkedSeq = inFlightSeq
				dropsAtPark = drops
			} else if time.Since(parkedSince) >= 500*time.Millisecond && drops >= dropsAtPark+100 {
				break
			}
		} else {
			parkedSince = time.Time{}
			parkedSeq = 0
		}
		if time.Now().After(parkDeadline) {
			t.Fatalf("hot sender did not park in SendDatagram: owned=%d in_flight=%d seq=%d drops=%d",
				ownedFrames, inFlightFrames, inFlightSeq, drops)
		}
	}

	manager.Close()
	if err := hotPair.peer.CloseWithError(0, "retire blackholed generation"); err != nil {
		t.Fatalf("retire hot QUIC generation: %v", err)
	}
	waitDone := make(chan struct{})
	go func() {
		manager.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-ctx.Done():
		t.Fatalf("Manager.Wait did not return after QUIC generation retirement: %v", context.Cause(ctx))
	}
	managerStopped = true
	if got := handler.senderStats.workers.Load(); got != 0 {
		t.Fatalf("sender workers after Manager.Wait = %d, want 0", got)
	}
	if frames, backing := handler.senderStats.queuedFrames.Load(), handler.senderStats.queuedBacking.Load(); frames != 0 || backing != 0 {
		t.Fatalf("queued ownership after Manager.Wait = (%d frames, %d bytes), want zero", frames, backing)
	}
	if hotClient.ActiveConns.Load() != 0 || coldClient.ActiveConns.Load() != 0 {
		t.Fatalf("active UDP sessions after Manager.Wait = (hot %d, cold %d), want zero",
			hotClient.ActiveConns.Load(), coldClient.ActiveConns.Load())
	}
	remainingSessions := 0
	handler.sessions.Range(func(_, _ any) bool {
		remainingSessions++
		return true
	})
	if remainingSessions != 0 {
		t.Fatalf("UDP sessions after Manager.Wait = %d, want 0", remainingSessions)
	}
}

func TestUDPSenderRegistryUsesExactGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stalePair := newUDPSenderQUICPair(t, ctx)
	freshPair := newUDPSenderQUICPair(t, ctx)
	packetConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen UDP for exact-generation handler: %v", err)
	}
	handlerCtx, cancelHandler := context.WithCancel(ctx)
	handler := &UDPHandler{
		packetConn:        packetConn,
		logger:            zerolog.Nop(),
		ctx:               handlerCtx,
		cancel:            cancelHandler,
		fragmentAssembler: protocol.NewShardedFragmentAssembler(protocol.DefaultShardCount),
		receivers:         make(map[*quic.Conn]struct{}),
		senders:           make(map[*pool.ClientConn]*udpSender),
	}
	t.Cleanup(func() {
		handler.close()
		_ = stalePair.peer.CloseWithError(0, "test cleanup")
		_ = freshPair.peer.CloseWithError(0, "test cleanup")
		handler.wait()
	})
	stale := &pool.ClientConn{ID: "same-id", Conn: stalePair.server}
	fresh := &pool.ClientConn{ID: "same-id", Conn: freshPair.server}
	staleSender := handler.senderFor(stale)
	freshSender := handler.senderFor(fresh)
	if staleSender == nil || freshSender == nil || staleSender == freshSender {
		t.Fatalf("exact generation senders = (%p, %p), want distinct non-nil senders", staleSender, freshSender)
	}

	if err := stalePair.peer.CloseWithError(0, "retire stale generation"); err != nil {
		t.Fatalf("retire stale QUIC generation: %v", err)
	}
	select {
	case <-staleSender.done:
	case <-ctx.Done():
		t.Fatalf("stale sender did not exit: %v", context.Cause(ctx))
	}
	handler.lifecycleMu.Lock()
	gotStale := handler.senders[stale]
	gotFresh := handler.senders[fresh]
	handler.lifecycleMu.Unlock()
	if gotStale != nil {
		t.Fatalf("stale sender remained registered: %p", gotStale)
	}
	if gotFresh != freshSender {
		t.Fatalf("fresh same-ID sender after stale cleanup = %p, want %p", gotFresh, freshSender)
	}
	select {
	case <-fresh.Conn.Context().Done():
		t.Fatalf("fresh same-ID generation closed by stale cleanup: %v", context.Cause(fresh.Conn.Context()))
	default:
	}
}
