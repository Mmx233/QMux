package traffic

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
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

func newUDPSenderQUICPair(t testing.TB, ctx context.Context) *udpSenderQUICPair {
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
	return udpSendBatch{
		datagrams: datagrams,
	}
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

func TestUDPSenderWholeBatchAdmissionAndOwnership(t *testing.T) {
	t.Run("frame limit", func(t *testing.T) {
		handler := &UDPHandler{logger: zerolog.Nop()}
		sender := &udpSender{
			queue: make(chan udpSendBatch, maxUDPSenderQueuedFrames),
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
		handler.failSender(sender)
		if sender.ownedFrames != 0 {
			t.Fatalf("ownership after drain = %d frames, want zero", sender.ownedFrames)
		}
		for i := range 4 {
			for j := range batches[i].datagrams {
				if batches[i].datagrams[j].Buffer != nil {
					t.Fatalf("batch %d datagram %d retained pooled ownership after drain", i, j)
				}
			}
		}
	})

	t.Run("backing limit uses configured pooled capacity", func(t *testing.T) {
		originalDatagramSize := protocol.DatagramBufferSize
		originalReadSize := protocol.ReadBufferSize
		originalFragmentSize := protocol.FragmentBufferSize
		t.Cleanup(func() {
			if err := protocol.InitBufferPool(originalDatagramSize, originalReadSize, originalFragmentSize); err != nil {
				t.Errorf("restore UDP buffer pool: %v", err)
			}
		})
		if err := protocol.InitBufferPool(64<<10, originalReadSize, originalFragmentSize); err != nil {
			t.Fatalf("initialize large UDP datagram pool: %v", err)
		}

		handler := &UDPHandler{logger: zerolog.Nop()}
		sender := &udpSender{
			queue: make(chan udpSendBatch, maxUDPSenderQueuedFrames),
		}
		accepted := make([]udpSendBatch, 0, 8)
		for i := range 8 {
			batch := fragmentUDPSenderBatch(t, uint32(i+1), []byte("pooled byte cap"))
			if result := handler.enqueueSender(sender, batch); result != udpEnqueued {
				t.Fatalf("enqueue pooled batch %d at backing limit = %v, want accepted", i, result)
			}
			accepted = append(accepted, batch)
		}
		rejected := fragmentUDPSenderBatch(t, 9, []byte("over pooled byte cap"))
		if result := handler.enqueueSender(sender, rejected); result != udpQueueFull {
			t.Fatalf("enqueue batch over backing limit = %v, want queue full", result)
		}
		protocol.ReleaseDatagramResults(rejected.datagrams)
		if sender.ownedFrames != 8 || sender.ownedFrames*int64(protocol.DatagramBufferSize) != maxUDPSenderQueuedBacking {
			t.Fatalf("ownership after backing rejection = %d frames/%d bytes, want 8/%d",
				sender.ownedFrames, sender.ownedFrames*int64(protocol.DatagramBufferSize), maxUDPSenderQueuedBacking)
		}
		handler.failSender(sender)
		if sender.ownedFrames != 0 {
			t.Fatalf("ownership after backing drain = %d frames, want zero", sender.ownedFrames)
		}
		for i := range accepted {
			for j := range accepted[i].datagrams {
				if accepted[i].datagrams[j].Buffer != nil {
					t.Fatalf("accepted batch %d datagram %d retained pooled ownership after drain", i, j)
				}
			}
		}
	})
}

func TestUDPSenderFragmentsBeforeAdmissionLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := newUDPSenderQUICPair(t, ctx)
	handlerCtx, cancelHandler := context.WithCancel(ctx)
	handler := &UDPHandler{
		ctx:                 handlerCtx,
		logger:              zerolog.Nop(),
		enableFragmentation: true,
		senders:             make(map[*pool.ClientConn]*udpSender),
	}
	defer func() {
		cancelHandler()
		handler.wait()
	}()
	client := &pool.ClientConn{ID: "fragment-before-lock", Conn: pair.server}
	sender := handler.senderFor(client)
	if sender == nil {
		t.Fatal("create UDP sender")
	}
	session := &UDPSession{id: 60, client: client, sender: sender}

	sender.mu.Lock()
	locked := true
	defer func() {
		if locked {
			sender.mu.Unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		handler.sendDatagrams(session, make([]byte, protocol.MaxUDPPayload+1))
		close(done)
	}()
	awaitUDPCondition(t, time.Second, "fragmentation before sender admission lock", func() bool {
		return session.fragIDCounter.Load() != 0
	})
	if sender.ownedFrames != 0 {
		t.Fatalf("ownership while admission lock is held = %d frames, want zero", sender.ownedFrames)
	}
	sender.mu.Unlock()
	locked = false
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("send after releasing admission lock: %v", context.Cause(ctx))
	}
	awaitUDPCondition(t, time.Second, "fragmented batch release", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return sender.ownedFrames == 0
	})
}

func TestUDPSenderWorkerSendsWithoutAdmissionLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := newUDPSenderQUICPair(t, ctx)
	handlerCtx, cancelHandler := context.WithCancel(ctx)
	handler := &UDPHandler{
		ctx:     handlerCtx,
		logger:  zerolog.Nop(),
		senders: make(map[*pool.ClientConn]*udpSender),
	}
	defer func() {
		cancelHandler()
		handler.wait()
	}()
	client := &pool.ClientConn{ID: "worker-without-admission-lock", Conn: pair.server}
	sender := handler.senderFor(client)
	if sender == nil {
		t.Fatal("create UDP sender")
	}
	batch := fragmentUDPSenderBatch(t, 61, []byte("worker lock independence"))

	sender.mu.Lock()
	locked := true
	defer func() {
		if locked {
			sender.mu.Unlock()
		}
	}()
	if result := handler.enqueueSenderLocked(sender, batch); result != udpEnqueued {
		t.Fatalf("enqueue result = %v, want %v", result, udpEnqueued)
	}
	receiveCtx, cancelReceive := context.WithTimeout(ctx, time.Second)
	defer cancelReceive()
	if _, err := pair.peer.ReceiveDatagram(receiveCtx); err != nil {
		t.Fatalf("receive datagram while admission lock is held: %v", err)
	}
	if sender.ownedFrames != int64(len(batch.datagrams)) {
		t.Fatalf("selected ownership while release awaits lock = %d frames, want %d", sender.ownedFrames, len(batch.datagrams))
	}
	sender.mu.Unlock()
	locked = false
	awaitUDPCondition(t, time.Second, "selected batch release", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return sender.ownedFrames == 0
	})
	cancelHandler()
	handler.wait()
	for i := range batch.datagrams {
		if batch.datagrams[i].Buffer != nil {
			t.Fatalf("datagram %d retained pooled ownership after send", i)
		}
	}
}

func TestUDPSenderRejectsOversizedPacketBeforeSessionLookup(t *testing.T) {
	oversized := make([]byte, protocol.MaxUDPPayload+1)

	t.Run("without session", func(t *testing.T) {
		handler := &UDPHandler{
			ctx:     context.Background(),
			logger:  zerolog.Nop(),
			senders: make(map[*pool.ClientConn]*udpSender),
		}
		handler.processPacket(oversized, netip.MustParseAddrPort("127.0.0.1:12345"))

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
		clientAddr := netip.MustParseAddrPort("127.0.0.1:12346")
		session := &UDPSession{id: 7, clientAddr: clientAddr, client: client}
		handler := &UDPHandler{
			ctx:     context.Background(),
			logger:  zerolog.Nop(),
			senders: make(map[*pool.ClientConn]*udpSender),
		}
		handler.sessions.Store(clientAddr, session)
		handler.sessionsByID.Store(session.id, session)

		handler.processPacket(oversized, clientAddr)

		gotByAddr, okByAddr := handler.sessions.Load(clientAddr)
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
	clientAddr := netip.MustParseAddrPort("127.0.0.1:12347")
	session := &UDPSession{id: 8, clientAddr: clientAddr, client: client}
	handler := &UDPHandler{
		ctx:     context.Background(),
		logger:  zerolog.Nop(),
		senders: make(map[*pool.ClientConn]*udpSender),
	}
	handler.sessions.Store(clientAddr, session)
	handler.sessionsByID.Store(session.id, session)

	handler.sendDatagrams(session, make([]byte, protocol.MaxUDPPayload+1))

	if _, ok := handler.sessions.Load(clientAddr); ok {
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
		frames := hotSender.ownedFrames
		hotSender.mu.Unlock()
		backing := frames * int64(protocol.DatagramBufferSize)
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
	if snapshot := handler.snapshot(); snapshot.DSendItemsHighWater > maxUDPSenderQueuedFrames ||
		snapshot.DSendBackingBytesHighWater > maxUDPSenderQueuedBacking ||
		snapshot.DSendBackingBytesHighWater != snapshot.DSendItemsHighWater*int64(protocol.DatagramBufferSize) {
		t.Fatalf("hot sender observed high-water exceeded its budget or projection: %+v", snapshot)
	}

	coldPayload := []byte("cold-generation-echo")
	response := make([]byte, 256)
	for coldDeadline := time.Now().Add(5 * time.Second); ; {
		probeCtx, cancelProbe := context.WithCancel(ctx)
		coldEcho := echoOneUDPDatagram(probeCtx, coldPair.peer)
		if err := coldPublic.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			cancelProbe()
			t.Fatalf("set cold public UDP deadline: %v", err)
		}
		if _, err := coldPublic.Write(coldPayload); err != nil {
			cancelProbe()
			t.Fatalf("write cold public UDP packet: %v", err)
		}
		n, readErr := coldPublic.Read(response)
		cancelProbe()
		echoErr := <-coldEcho
		if echoErr != nil && !errors.Is(echoErr, context.Canceled) && !errors.Is(echoErr, context.DeadlineExceeded) {
			t.Fatal(echoErr)
		}
		if readErr == nil {
			if got := string(response[:n]); got != string(coldPayload) {
				t.Fatalf("cold public UDP response = %q, want %q", got, coldPayload)
			}
			break
		}
		if netErr, ok := errors.AsType[net.Error](readErr); !ok || !netErr.Timeout() || time.Now().After(coldDeadline) {
			t.Fatalf("read cold public UDP response: %v", readErr)
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
	hotSender.mu.Lock()
	frames := hotSender.ownedFrames
	hotSender.mu.Unlock()
	if !hotSender.stopped.Load() || frames != 0 {
		t.Fatalf("hot sender after Manager.Wait = stopped %v, owned %d frames; want stopped with zero ownership",
			hotSender.stopped.Load(), frames)
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
	if snapshot, slots := handler.sessionStats.snapshot(), len(handler.sessionSlots); snapshot.SessionPermits != 0 || slots != 0 || snapshot.AccountingFaults != 0 {
		t.Fatalf("UDP admission after Manager.Wait = (%d held, %d slots, %d faults), want zero",
			snapshot.SessionPermits, slots, snapshot.AccountingFaults)
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

func TestUDPReceiverUsesExactGenerationBeforeFragmentRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stalePair := newUDPSenderQUICPair(t, ctx)
	freshPair := newUDPSenderQUICPair(t, ctx)
	packetConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen UDP for exact-generation receiver: %v", err)
	}
	staleSink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		_ = packetConn.Close()
		t.Fatalf("listen stale UDP sink: %v", err)
	}
	freshSink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		_ = staleSink.Close()
		_ = packetConn.Close()
		t.Fatalf("listen fresh UDP sink: %v", err)
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
	stale := &pool.ClientConn{ID: "same-id", Conn: stalePair.server}
	fresh := &pool.ClientConn{ID: "same-id", Conn: freshPair.server}
	stale.ActiveConns.Store(1)
	fresh.ActiveConns.Store(1)
	staleSession := &UDPSession{id: 41, clientAddr: canonicalUDPAddrPort(staleSink.LocalAddr().(*net.UDPAddr).AddrPort()), client: stale}
	freshSession := &UDPSession{id: 42, clientAddr: canonicalUDPAddrPort(freshSink.LocalAddr().(*net.UDPAddr).AddrPort()), client: fresh}
	staleSession.lastActive.Store(1)
	freshSession.lastActive.Store(2)
	handler.sessions.Store(staleSession.clientAddr, staleSession)
	handler.sessions.Store(freshSession.clientAddr, freshSession)
	handler.sessionsByID.Store(staleSession.id, staleSession)
	handler.sessionsByID.Store(freshSession.id, freshSession)
	handler.sessionStats.mu.Lock()
	handler.sessionStats.current = 2
	handler.sessionStats.mu.Unlock()
	handler.receivers[stalePair.server] = struct{}{}
	handler.receivers[freshPair.server] = struct{}{}
	handler.receiverWG.Add(2)
	go handler.receiveDatagrams(stalePair.server)
	go handler.receiveDatagrams(freshPair.server)
	t.Cleanup(func() {
		handler.close()
		handler.wait()
		_ = staleSink.Close()
		_ = freshSink.Close()
	})

	wrongNormal := fragmentUDPSenderBatch(t, staleSession.id, []byte("wrong generation"))
	if _, err := handler.sendBatch(freshPair.peer, wrongNormal); err != nil {
		t.Fatalf("send wrong-generation normal datagram: %v", err)
	}
	protocol.ReleaseDatagramResults(wrongNormal.datagrams)
	awaitUDPCondition(t, 3*time.Second, "wrong normal owner drop", func() bool {
		return handler.senderStats.unknownSession.Load() == 1
	})
	if got := staleSession.lastActive.Load(); got != 1 {
		t.Fatalf("stale session lastActive after wrong normal owner = %d, want 1", got)
	}

	wrongFragment := fragmentUDPSenderBatch(t, freshSession.id, make([]byte, protocol.MaxUDPPayload+1))
	if err := stalePair.peer.SendDatagram(wrongFragment.datagrams[0].Data); err != nil {
		protocol.ReleaseDatagramResults(wrongFragment.datagrams)
		t.Fatalf("send wrong-generation fragment: %v", err)
	}
	protocol.ReleaseDatagramResults(wrongFragment.datagrams)
	awaitUDPCondition(t, 3*time.Second, "wrong fragment owner drop", func() bool {
		return handler.senderStats.unknownSession.Load() == 2
	})
	if got := handler.fragmentAssembler.Snapshot(); got.RetainedGroups != 0 || got.RetainedBackingBytes != 0 {
		t.Fatalf("fragment state after wrong owner = %+v, want no retention", got)
	}
	if got := freshSession.lastActive.Load(); got != 2 {
		t.Fatalf("fresh session lastActive after wrong fragment owner = %d, want 2", got)
	}

	unknown := fragmentUDPSenderBatch(t, 999, []byte("unknown session"))
	if _, err := handler.sendBatch(freshPair.peer, unknown); err != nil {
		t.Fatalf("send unknown-session datagram: %v", err)
	}
	protocol.ReleaseDatagramResults(unknown.datagrams)
	awaitUDPCondition(t, 3*time.Second, "unknown session drop", func() bool {
		return handler.senderStats.unknownSession.Load() == 3
	})

	freshPayload := []byte("fresh owner")
	freshNormal := fragmentUDPSenderBatch(t, freshSession.id, freshPayload)
	if _, err := handler.sendBatch(freshPair.peer, freshNormal); err != nil {
		t.Fatalf("send fresh-owner normal datagram: %v", err)
	}
	protocol.ReleaseDatagramResults(freshNormal.datagrams)
	if err := freshSink.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set fresh sink deadline: %v", err)
	}
	buf := make([]byte, 65535)
	n, _, err := freshSink.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != string(freshPayload) {
		t.Fatalf("fresh-owner public payload = %q, err %v, want %q", buf[:n], err, freshPayload)
	}

	stalePayload := make([]byte, protocol.MaxUDPPayload+1)
	for i := range stalePayload {
		stalePayload[i] = byte(i)
	}
	staleFragments := fragmentUDPSenderBatch(t, staleSession.id, stalePayload)
	if _, err := handler.sendBatch(stalePair.peer, staleFragments); err != nil {
		t.Fatalf("send stale-owner fragmented datagram: %v", err)
	}
	protocol.ReleaseDatagramResults(staleFragments.datagrams)
	if err := staleSink.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set stale sink deadline: %v", err)
	}
	n, _, err = staleSink.ReadFromUDP(buf)
	if err != nil || !slices.Equal(buf[:n], stalePayload) {
		t.Fatalf("stale-owner public payload length = %d, err %v, want %d", n, err, len(stalePayload))
	}
	if staleSession.lastActive.Load() == 1 || freshSession.lastActive.Load() == 2 {
		t.Fatalf("owner activity was not refreshed: stale=%d fresh=%d",
			staleSession.lastActive.Load(), freshSession.lastActive.Load())
	}
}

func TestUDPPooledBackingFormula(t *testing.T) {
	originalDatagramSize := protocol.DatagramBufferSize
	originalReadSize := protocol.ReadBufferSize
	originalFragmentSize := protocol.FragmentBufferSize
	t.Cleanup(func() {
		if err := protocol.InitBufferPool(originalDatagramSize, originalReadSize, originalFragmentSize); err != nil {
			t.Errorf("restore UDP buffer pool: %v", err)
		}
	})

	for _, datagramSize := range []int{protocol.DefaultDatagramBufferSize, protocol.DefaultDatagramBufferSize + 512} {
		if err := protocol.InitBufferPool(datagramSize, protocol.DefaultReadBufferSize, protocol.DefaultFragmentBufferSize); err != nil {
			t.Fatalf("initialize UDP buffer pool: %v", err)
		}
		for _, payloadSize := range []int{1000, 65535} {
			var counter atomic.Uint32
			datagrams, err := protocol.FragmentUDPPooled(1, make([]byte, payloadSize), &counter, true)
			if err != nil {
				t.Fatalf("fragment %d-byte payload with %d-byte datagrams: %v", payloadSize, datagramSize, err)
			}
			want := datagramBackingBytes(datagrams)
			got := int64(len(datagrams)) * int64(protocol.DatagramBufferSize)
			for i := range datagrams {
				if datagrams[i].Buffer == nil || len(*datagrams[i].Buffer) != datagramSize || cap(*datagrams[i].Buffer) != datagramSize {
					t.Fatalf("payload %d datagram %d buffer = %v, want exact len/cap %d", payloadSize, i, datagrams[i].Buffer, datagramSize)
				}
			}
			protocol.ReleaseDatagramResults(datagrams)
			if got != want {
				t.Fatalf("%d-byte pooled backing formula with %d-byte datagrams = %d, want scan result %d",
					payloadSize, datagramSize, got, want)
			}
		}
	}
}

func TestUDPServerSessionCachesExactSender(t *testing.T) {
	t.Run("cached hit skips lifecycle registry", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pair := newUDPSenderQUICPair(t, ctx)
		handlerCtx, cancelHandler := context.WithCancel(ctx)
		handler := &UDPHandler{ctx: handlerCtx, logger: zerolog.Nop(), enableFragmentation: true, senders: make(map[*pool.ClientConn]*udpSender)}
		defer func() {
			cancelHandler()
			handler.wait()
		}()
		client := &pool.ClientConn{ID: "cached", Conn: pair.server}
		session := &UDPSession{id: 51, clientAddr: netip.MustParseAddrPort("127.0.0.1:12351"), client: client}

		handler.sendDatagrams(session, []byte("first"))
		cached := session.sender
		if cached == nil {
			t.Fatal("first send did not cache its sender")
		}
		handler.lifecycleMu.Lock()
		if got := handler.senders[client]; got != cached {
			handler.lifecycleMu.Unlock()
			t.Fatalf("cached sender = %p, registry sender = %p", cached, got)
		}
		secondDone := make(chan struct{})
		go func() {
			handler.sendDatagrams(session, []byte("second"))
			close(secondDone)
		}()
		select {
		case <-secondDone:
		case <-time.After(time.Second):
			handler.lifecycleMu.Unlock()
			t.Fatal("cached send blocked on lifecycle registry")
		}
		handler.lifecycleMu.Unlock()
	})

	t.Run("pre-canceled connection rejects before fragmentation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stalePair := newUDPSenderQUICPair(t, ctx)
		freshPair := newUDPSenderQUICPair(t, ctx)
		handlerCtx, cancelHandler := context.WithCancel(ctx)
		handler := &UDPHandler{ctx: handlerCtx, logger: zerolog.Nop(), enableFragmentation: true, senders: make(map[*pool.ClientConn]*udpSender)}
		defer func() {
			cancelHandler()
			handler.wait()
		}()
		stale := &pool.ClientConn{ID: "same-id", Conn: stalePair.server}
		fresh := &pool.ClientConn{ID: "same-id", Conn: freshPair.server}
		staleSender := &udpSender{client: stale, queue: make(chan udpSendBatch, 1), done: make(chan struct{})}
		handler.senders[stale] = staleSender
		freshSender := handler.senderFor(fresh)
		if freshSender == nil {
			t.Fatal("create fresh-generation sender")
		}
		addr := netip.MustParseAddrPort("127.0.0.1:12352")
		session := &UDPSession{id: 52, clientAddr: addr, client: stale, sender: staleSender}
		stale.ActiveConns.Store(1)
		handler.sessions.Store(addr, session)
		handler.sessionsByID.Store(session.id, session)
		handler.sessionStats.publish()

		if err := stalePair.peer.CloseWithError(0, "pre-cancel stale generation"); err != nil {
			t.Fatalf("cancel stale generation: %v", err)
		}
		awaitUDPCondition(t, time.Second, "stale connection cancellation", func() bool {
			return stale.Conn.Context().Err() != nil
		})
		handler.sendDatagrams(session, make([]byte, protocol.MaxUDPPayload+1))

		staleSender.mu.Lock()
		frames := staleSender.ownedFrames
		staleSender.mu.Unlock()
		if staleSender.stopped.Load() || session.fragIDCounter.Load() != 0 || len(staleSender.queue) != 0 || frames != 0 {
			t.Fatalf("pre-canceled admission: stopped=%v frag=%d queue=%d owned=%d",
				staleSender.stopped.Load(), session.fragIDCounter.Load(), len(staleSender.queue), frames)
		}
		if syncMapLen(&handler.sessions) != 0 || syncMapLen(&handler.sessionsByID) != 0 || stale.ActiveConns.Load() != 0 {
			t.Fatalf("pre-canceled exact close: maps=%d/%d active=%d",
				syncMapLen(&handler.sessions), syncMapLen(&handler.sessionsByID), stale.ActiveConns.Load())
		}
		handler.lifecycleMu.Lock()
		senderCount := len(handler.senders)
		gotStale, gotFresh := handler.senders[stale], handler.senders[fresh]
		handler.lifecycleMu.Unlock()
		if senderCount != 2 || gotStale != staleSender || gotFresh != freshSender {
			t.Fatalf("pre-canceled sender registry = %d (%p/%p), want two exact generations (%p/%p)",
				senderCount, gotStale, gotFresh, staleSender, freshSender)
		}
		if freshSender.stopped.Load() || fresh.Conn.Context().Err() != nil {
			t.Fatalf("fresh generation affected: stopped=%v context=%v", freshSender.stopped.Load(), context.Cause(fresh.Conn.Context()))
		}
	})
}

func TestUDPServerSenderCancellationRaceDrainsExactly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pair := newUDPSenderQUICPair(t, ctx)
	handlerCtx, cancelHandler := context.WithCancel(ctx)
	handler := &UDPHandler{ctx: handlerCtx, logger: zerolog.Nop(), enableFragmentation: true, senders: make(map[*pool.ClientConn]*udpSender)}
	defer func() {
		cancelHandler()
		handler.wait()
	}()
	client := &pool.ClientConn{ID: "racing", Conn: pair.server}
	sender := handler.senderFor(client)
	if sender == nil {
		t.Fatal("create racing sender")
	}
	addr := netip.MustParseAddrPort("127.0.0.1:12353")
	session := &UDPSession{id: 53, clientAddr: addr, client: client, sender: sender}
	client.ActiveConns.Store(1)
	handler.sessions.Store(addr, session)
	handler.sessionsByID.Store(session.id, session)
	handler.sessionStats.publish()
	payload := make([]byte, 65535)

	handler.lifecycleMu.Lock()
	locked := true
	defer func() {
		if locked {
			handler.lifecycleMu.Unlock()
		}
	}()
	started := make(chan struct{})
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for i := range 128 {
			handler.sendDatagrams(session, payload)
			if i == 0 {
				close(started)
			}
		}
	}()
	<-started
	if err := pair.peer.CloseWithError(0, "race sender cancellation"); err != nil {
		t.Fatalf("cancel racing generation: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		sender.mu.Lock()
		frames := sender.ownedFrames
		sender.mu.Unlock()
		backing := frames * int64(protocol.DatagramBufferSize)
		if frames < 0 || frames > maxUDPSenderQueuedFrames || backing > maxUDPSenderQueuedBacking {
			t.Fatalf("racing sender exceeded cap: owned=%d/%d", frames, backing)
		}
		if sender.stopped.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("racing sender did not stop")
		}
		time.Sleep(time.Millisecond)
	}
	<-producerDone
	if handler.senders[client] != sender {
		t.Fatal("sender deleted before the lifecycle gate was released")
	}
	if syncMapLen(&handler.sessions) != 0 || syncMapLen(&handler.sessionsByID) != 0 || client.ActiveConns.Load() != 0 {
		t.Fatalf("racing sender session residue: maps=%d/%d active=%d",
			syncMapLen(&handler.sessions), syncMapLen(&handler.sessionsByID), client.ActiveConns.Load())
	}
	handler.lifecycleMu.Unlock()
	locked = false
	select {
	case <-sender.done:
	case <-ctx.Done():
		t.Fatalf("racing sender did not finish: %v", context.Cause(ctx))
	}
	handler.lifecycleMu.Lock()
	got := handler.senders[client]
	handler.lifecycleMu.Unlock()
	sender.mu.Lock()
	frames := sender.ownedFrames
	sender.mu.Unlock()
	if got != nil || handler.senderStats.workers.Load() != 0 || frames != 0 || len(sender.queue) != 0 {
		t.Fatalf("racing sender final residue: sender=%p workers=%d owned=%d queue=%d",
			got, handler.senderStats.workers.Load(), frames, len(sender.queue))
	}
}

func BenchmarkUDPAddressKey(b *testing.B) {
	udpAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	addrPort := udpAddr.AddrPort()
	canonicalAddrPort := netip.AddrPortFrom(addrPort.Addr().Unmap(), addrPort.Port())
	session := &UDPSession{}
	var stringSessions sync.Map
	var addrPortSessions sync.Map
	stringSessions.Store(udpAddr.String(), session)
	addrPortSessions.Store(canonicalAddrPort, session)

	b.Run("string", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			value, ok := stringSessions.Load(udpAddr.String())
			if !ok || value.(*UDPSession) != session {
				b.Fatal("string session lookup missed")
			}
		}
	})
	b.Run("addrport", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			key := netip.AddrPortFrom(addrPort.Addr().Unmap(), addrPort.Port())
			value, ok := addrPortSessions.Load(key)
			if !ok || value.(*UDPSession) != session {
				b.Fatal("AddrPort session lookup missed")
			}
		}
	})
}

func BenchmarkUDPSenderResolution(b *testing.B) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	b.Cleanup(cancel)
	pair := newUDPSenderQUICPair(b, ctx)
	handlerCtx, cancelHandler := context.WithCancel(ctx)
	handler := &UDPHandler{
		ctx:     handlerCtx,
		senders: make(map[*pool.ClientConn]*udpSender),
	}
	client := &pool.ClientConn{ID: "benchmark", Conn: pair.server}
	cached := handler.senderFor(client)
	if cached == nil {
		b.Fatal("create benchmark sender")
	}
	b.Cleanup(func() {
		cancelHandler()
		handler.wait()
	})
	addr := netip.MustParseAddrPort("127.0.0.1:12345")
	session := &UDPSession{client: client}
	handler.sessions.Store(addr, session)

	b.Run("registry", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			value, ok := handler.sessions.Load(addr)
			if !ok || handler.senderFor(value.(*UDPSession).client) != cached {
				b.Fatal("registry sender lookup missed")
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			value, ok := handler.sessions.Load(addr)
			if !ok || value.(*UDPSession).client != client || cached == nil {
				b.Fatal("cached sender lookup missed")
			}
		}
	})
}

func BenchmarkUDPServerDsendAccounting(b *testing.B) {
	handler := &UDPHandler{}
	sender := &udpSender{
		queue: make(chan udpSendBatch, 1),
	}
	batch := udpSendBatch{
		datagrams: []protocol.DatagramResult{{Data: make([]byte, 1, protocol.DatagramBufferSize)}},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if result := handler.enqueueSender(sender, batch); result != udpEnqueued {
			b.Fatalf("enqueue result = %v, want %v", result, udpEnqueued)
		}
		handler.releaseSenderBatch(sender, <-sender.queue)
	}
	b.StopTimer()
	if sender.ownedFrames != 0 {
		b.Fatalf("Dsend ownership after benchmark = %d frames, want zero", sender.ownedFrames)
	}
}
