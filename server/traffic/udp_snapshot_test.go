package traffic

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/rs/zerolog"
)

func TestUDPAdmissionSnapshotOwnedSendState(t *testing.T) {
	assembler := protocol.NewShardedFragmentAssembler(1)
	defer assembler.Close()
	if _, err := assembler.AddFragment(1, 1, 0, 2, []byte("a")); err != nil {
		t.Fatal(err)
	}

	queued := &udpSender{ownedFrames: 2}
	selected := &udpSender{ownedFrames: 3}
	handler := &UDPHandler{
		senders: map[*pool.ClientConn]*udpSender{
			{}: queued,
			{}: selected,
		},
		fragmentAssembler: assembler,
	}
	handler.senderStats.workers.Store(2)
	handler.senderStats.sendErrors.Store(1)
	handler.senderStats.queueFullDrops.Store(2)
	handler.senderStats.noEligibleDrops.Store(3)
	handler.senderStats.fragmentDrops.Store(4)
	handler.senderStats.decodeDrops.Store(5)
	handler.senderStats.unknownSession.Store(6)
	handler.senderStats.publicWriteDrops.Store(7)

	snapshot := handler.snapshot()
	wantBacking := int64(5 * protocol.DatagramBufferSize)
	if snapshot.DSendItems != 5 || snapshot.DSendBackingBytes != wantBacking ||
		snapshot.DSendItemsHighWater != 5 || snapshot.DSendBackingBytesHighWater != wantBacking ||
		snapshot.DSendWorkers != 2 {
		t.Fatalf("Dsend current/high/workers = %d/%d/%d/%d/%d, want 5/%d/5/%d/2",
			snapshot.DSendItems, snapshot.DSendBackingBytes, snapshot.DSendItemsHighWater,
			snapshot.DSendBackingBytesHighWater, snapshot.DSendWorkers, wantBacking, wantBacking)
	}
	if snapshot.DSendErrors != 1 || snapshot.QueueFullDrops != 2 || snapshot.NoEligibleDrops != 3 ||
		snapshot.FragmentDrops != 4 || snapshot.DecodeDrops != 5 || snapshot.UnknownSessionDrops != 6 ||
		snapshot.PublicWriteDrops != 7 {
		t.Fatalf("Dsend errors/reasons = %+v", snapshot)
	}
	if snapshot.Fragment.RetainedGroups != 1 || snapshot.Fragment.RetainedBackingBytes != int64(protocol.FragmentBufferSize) {
		t.Fatalf("fragment snapshot = %+v", snapshot.Fragment)
	}
}

func TestUDPAdmissionSnapshotDsendHighWaterSurvivesRelease(t *testing.T) {
	client := &pool.ClientConn{}
	sender := &udpSender{
		client: client,
		queue:  make(chan udpSendBatch, 1),
		done:   make(chan struct{}),
	}
	handler := &UDPHandler{senders: map[*pool.ClientConn]*udpSender{client: sender}}
	batch := fragmentUDPSenderBatch(t, 1, make([]byte, protocol.MaxUDPPayload+1))
	wantItems := int64(len(batch.datagrams))
	wantBacking := wantItems * int64(protocol.DatagramBufferSize)
	if got := handler.enqueueSender(sender, batch); got != udpEnqueued {
		t.Fatalf("enqueue result = %v, want %v", got, udpEnqueued)
	}
	if got := handler.snapshot(); got.DSendItems != wantItems || got.DSendBackingBytes != wantBacking ||
		got.DSendItemsHighWater != wantItems || got.DSendBackingBytesHighWater != wantBacking {
		t.Fatalf("owned Dsend snapshot = %+v", got)
	}

	handler.releaseSenderBatch(sender, <-sender.queue)
	if got := handler.snapshot(); got.DSendItems != 0 || got.DSendBackingBytes != 0 ||
		got.DSendItemsHighWater != wantItems || got.DSendBackingBytesHighWater != wantBacking {
		t.Fatalf("released Dsend snapshot = %+v", got)
	}
	handler.lifecycleMu.Lock()
	delete(handler.senders, client)
	handler.lifecycleMu.Unlock()
	if got := handler.snapshot(); got.DSendItems != 0 || got.DSendBackingBytes != 0 ||
		got.DSendItemsHighWater != wantItems || got.DSendBackingBytesHighWater != wantBacking {
		t.Fatalf("deleted-sender Dsend snapshot = %+v", got)
	}
}

func TestUDPAdmissionSnapshotDsendHighWaterSharesOwnedCut(t *testing.T) {
	client := &pool.ClientConn{}
	sender := &udpSender{
		client: client,
		queue:  make(chan udpSendBatch, 1),
		done:   make(chan struct{}),
	}
	handler := &UDPHandler{senders: map[*pool.ClientConn]*udpSender{client: sender}}

	sender.mu.Lock()
	snapshotDone := make(chan UDPAdmissionSnapshot, 1)
	go func() { snapshotDone <- handler.snapshot() }()
	deadline := time.Now().Add(time.Second)
	for handler.lifecycleMu.TryLock() {
		handler.lifecycleMu.Unlock()
		if time.Now().After(deadline) {
			sender.mu.Unlock()
			t.Fatal("snapshot did not reach the sender ownership cut")
		}
		time.Sleep(time.Millisecond)
	}

	batch := fragmentUDPSenderBatch(t, 1, []byte("snapshot cut"))
	if got := handler.enqueueSenderLocked(sender, batch); got != udpEnqueued {
		sender.mu.Unlock()
		t.Fatalf("enqueue result = %v, want %v", got, udpEnqueued)
	}
	sender.mu.Unlock()

	snapshot := <-snapshotDone
	wantBacking := int64(len(batch.datagrams)) * int64(protocol.DatagramBufferSize)
	if snapshot.DSendItems != int64(len(batch.datagrams)) || snapshot.DSendBackingBytes != wantBacking ||
		snapshot.DSendItems > snapshot.DSendItemsHighWater ||
		snapshot.DSendBackingBytes > snapshot.DSendBackingBytesHighWater ||
		snapshot.DSendBackingBytesHighWater != snapshot.DSendItemsHighWater*int64(protocol.DatagramBufferSize) {
		t.Fatalf("Dsend current/high-water snapshot = %+v", snapshot)
	}
	handler.releaseSenderBatch(sender, <-sender.queue)
}

func TestUDPAdmissionSnapshotSenderDeleteIsOneExactCut(t *testing.T) {
	client := &pool.ClientConn{}
	sender := &udpSender{client: client, done: make(chan struct{})}
	deleted := make(chan struct{})
	release := make(chan struct{})
	handler := &UDPHandler{
		senders: map[*pool.ClientConn]*udpSender{client: sender},
		afterSenderDelete: func() {
			close(deleted)
			<-release
		},
	}
	handler.senderStats.workers.Store(1)
	handler.senderWG.Add(1)

	go handler.finishSender(sender)
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	select {
	case <-deleted:
	case <-time.After(time.Second):
		t.Fatal("sender delete transition was not reached")
	}
	if got := handler.senderStats.workers.Load(); got != 1 {
		t.Fatalf("workers inside delete transition = %d, want 1", got)
	}
	if handler.lifecycleMu.TryLock() {
		handler.lifecycleMu.Unlock()
		t.Fatal("sender delete transition released lifecycle lock before worker decrement")
	}

	snapshotDone := make(chan UDPAdmissionSnapshot, 1)
	go func() { snapshotDone <- handler.snapshot() }()
	close(release)
	released = true
	if got := <-snapshotDone; got.DSendWorkers != 0 || got.DSendItems != 0 || got.DSendBackingBytes != 0 {
		t.Fatalf("snapshot after sender delete transition = %+v, want no owned sender state", got)
	}
	<-sender.done
}

func TestUDPAdmissionCapacityDropIsNotAlsoDecodeDrop(t *testing.T) {
	handler := &UDPHandler{}
	handler.recordDecodeError(protocol.ErrFragmentAssemblerFull)
	handler.recordDecodeError(errors.Join(errors.New("wrapped"), protocol.ErrFragmentAssemblerFull))
	if got := handler.senderStats.decodeDrops.Load(); got != 0 {
		t.Fatalf("capacity decode drops = %d, want 0", got)
	}
	handler.recordDecodeError(errors.New("malformed datagram"))
	if got := handler.senderStats.decodeDrops.Load(); got != 1 {
		t.Fatalf("malformed decode drops = %d, want 1", got)
	}
}

func TestUDPAdmissionSnapshotSessionTeardown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := newUDPSenderQUICPair(t, ctx)
	connectionPool := pool.New("test", pool.NewRoundRobinBalancer(), zerolog.Nop())
	defer connectionPool.Stop()
	client := &pool.ClientConn{
		ID:       "client",
		Conn:     pair.server,
		Metadata: pool.ClientMetadata{Capabilities: []string{"udp"}},
	}
	if err := connectionPool.Add(client); err != nil {
		t.Fatal(err)
	}
	handler := newUDPAdmissionUnitHandler(connectionPool, 2)
	defer func() {
		handler.cancel()
		handler.wait()
	}()

	first, err := handler.createSession(netip.MustParseAddrPort("127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := handler.createSession(netip.MustParseAddrPort("127.0.0.1:2"))
	if err != nil {
		t.Fatal(err)
	}
	if got := handler.snapshot(); got.SessionsCurrent != 2 || got.SessionPermits != 2 || got.SessionHighWater != 2 {
		t.Fatalf("live sessions/permits/high-water = %d/%d/%d, want 2/2/2",
			got.SessionsCurrent, got.SessionPermits, got.SessionHighWater)
	}

	handler.closeSession(first)
	handler.closeSession(second)
	if got := handler.snapshot(); got.SessionsCurrent != 0 || got.SessionPermits != 0 || got.SessionHighWater != 2 {
		t.Fatalf("torn-down sessions/permits/high-water = %d/%d/%d, want 0/0/2",
			got.SessionsCurrent, got.SessionPermits, got.SessionHighWater)
	}
}

func TestManagerUDPAdmissionSnapshotsConfiguredOrder(t *testing.T) {
	manager := NewManager(&config.Server{Listeners: []config.QuicListener{
		{Protocol: "tcp"},
		{Protocol: "udp"},
		{Protocol: "both"},
	}}, nil, zerolog.Nop())
	udp := &UDPHandler{senders: make(map[*pool.ClientConn]*udpSender)}
	both := &UDPHandler{senders: make(map[*pool.ClientConn]*udpSender)}
	udp.sessionStats.mu.Lock()
	udp.sessionStats.current = 11
	udp.sessionStats.mu.Unlock()
	both.sessionStats.mu.Lock()
	both.sessionStats.current = 22
	both.sessionStats.mu.Unlock()
	manager.listeners = []*Listener{{}, {udpHandler: udp}, {udpHandler: both}}

	snapshots := manager.UDPAdmissionSnapshots()
	if len(snapshots) != 3 {
		t.Fatalf("snapshot count = %d, want 3", len(snapshots))
	}
	if snapshots[0] != (UDPAdmissionSnapshot{}) {
		t.Fatalf("TCP-only snapshot = %+v, want zero", snapshots[0])
	}
	if snapshots[1].SessionsCurrent != 11 || snapshots[2].SessionsCurrent != 22 {
		t.Fatalf("ordered UDP sessions = %d/%d, want 11/22",
			snapshots[1].SessionsCurrent, snapshots[2].SessionsCurrent)
	}
}
