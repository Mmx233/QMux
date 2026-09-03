package client

import (
	"context"
	"crypto/tls"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/rs/zerolog"
)

func newCapacitySnapshotManager(t *testing.T, endpoints ...string) *ConnectionManager {
	t.Helper()
	servers := make([]config.ServerEndpoint, len(endpoints))
	for i, endpoint := range endpoints {
		servers[i] = config.ServerEndpoint{Address: endpoint, ServerName: "snapshot.test"}
	}
	manager, err := NewConnectionManager(&config.Client{
		Server:            config.ClientServer{Servers: servers},
		HeartbeatInterval: time.Hour,
		HealthTimeout:     2 * time.Hour,
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop() })
	return manager
}

func newCapacitySnapshotConnection(endpoint string) *ServerConnection {
	sc := NewServerConnection(endpoint, "snapshot.test", tls.NewLRUClientSessionCache(1), zerolog.Nop())
	sc.controlOnce.Do(func() {})
	return sc
}

func TestClientEndpointSnapshotTracksGenerationPhasesInConfigOrder(t *testing.T) {
	const first, second = "127.0.0.1:8443", "127.0.0.1:9443"
	manager := newCapacitySnapshotManager(t, first, second)
	client := &Client{connMgr: manager}
	wantEmpty := []EndpointSnapshot{{Endpoint: first}, {Endpoint: second}}
	if got := client.Snapshot().Endpoints; !reflect.DeepEqual(got, wantEmpty) {
		t.Fatalf("initial endpoints = %+v, want %+v", got, wantEmpty)
	}

	failed := newCapacitySnapshotConnection(first)
	manager.publishMu.Lock()
	manager.trackGenerationLocked(failed, clientGenerationHandshaking)
	manager.publishMu.Unlock()
	if got := manager.endpointSnapshot()[0]; got.Handshaking != 1 {
		t.Fatalf("handshaking snapshot = %+v", got)
	}
	manager.publishMu.Lock()
	manager.moveGenerationLocked(failed, clientGenerationHandshaking, clientGenerationPending)
	manager.publishMu.Unlock()
	if got := manager.endpointSnapshot()[0]; got.Pending != 1 || got.Handshaking != 0 {
		t.Fatalf("pending snapshot = %+v", got)
	}
	if err := failed.Close(); err != nil {
		t.Fatal(err)
	}
	if got := manager.endpointSnapshot()[0]; got.Pending != 0 {
		t.Fatalf("failed attempt snapshot = %+v", got)
	}

	old := newCapacitySnapshotConnection(first)
	if !manager.publishServerConnection(context.Background(), old) {
		t.Fatal("publish synthetic old generation")
	}
	<-manager.NewConns
	if got := manager.endpointSnapshot()[0]; got.Registered != 1 {
		t.Fatalf("registered snapshot = %+v", got)
	}

	old.closeMu.Lock()
	closeMuLocked := true
	defer func() {
		if closeMuLocked {
			old.closeMu.Unlock()
		}
	}()
	fresh := newCapacitySnapshotConnection(first)
	published := make(chan bool, 1)
	go func() { published <- manager.publishServerConnection(context.Background(), fresh) }()
	awaitRetirementCondition(t, "replacement phase transfer", func() bool {
		got := manager.endpointSnapshot()[0]
		return got.Registered == 1 && got.Retiring == 1 && got.GenerationHighWater == 2
	})
	old.closeMu.Unlock()
	closeMuLocked = false
	if !<-published {
		t.Fatal("publish synthetic fresh generation")
	}
	<-manager.NewConns
	if got := manager.endpointSnapshot()[0]; got.Registered != 1 || got.Retiring != 0 || got.GenerationHighWater != 2 {
		t.Fatalf("replacement completion snapshot = %+v", got)
	}

	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	wantStopped := []EndpointSnapshot{{Endpoint: first, GenerationHighWater: 2}, {Endpoint: second}}
	if got := manager.endpointSnapshot(); !reflect.DeepEqual(got, wantStopped) {
		t.Fatalf("stopped endpoints = %+v, want %+v", got, wantStopped)
	}
}

func TestClientEndpointSnapshotReportsAccountingFault(t *testing.T) {
	const endpoint = "127.0.0.1:8443"
	manager := newCapacitySnapshotManager(t, endpoint)
	connection := newCapacitySnapshotConnection(endpoint)
	manager.publishMu.Lock()
	manager.trackGenerationLocked(connection, clientGenerationPending)
	manager.moveGenerationLocked(connection, clientGenerationHandshaking, clientGenerationRegistered)
	manager.publishMu.Unlock()
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	got := manager.endpointSnapshot()[0]
	if got.Pending != 0 || got.AccountingFaults != 1 {
		t.Fatalf("fault snapshot = %+v", got)
	}
}

func TestConnectionManagerCleanupAfterRegisteredCloseIsIdempotent(t *testing.T) {
	const endpoint = "127.0.0.1:8443"
	manager := newCapacitySnapshotManager(t, endpoint)
	connection := newCapacitySnapshotConnection(endpoint)
	if !manager.publishServerConnection(context.Background(), connection) {
		t.Fatal("publish registered connection")
	}
	<-manager.NewConns
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}

	got := manager.endpointSnapshot()[0]
	if got.Handshaking != 0 || got.Pending != 0 || got.Registered != 0 || got.Retiring != 0 || got.AccountingFaults != 1 {
		t.Fatalf("cleanup snapshot = %+v, want zero counts and one out-of-band close fault", got)
	}
}

func TestServerConnectionCloseIsIdempotent(t *testing.T) {
	connection := newCapacitySnapshotConnection("127.0.0.1:8443")
	var callbacks atomic.Int64
	if !connection.setOnClosed(func() { callbacks.Add(1) }) {
		t.Fatal("new connection was already closed")
	}
	var wait sync.WaitGroup
	for range 32 {
		wait.Go(func() {
			if err := connection.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
	wait.Wait()
	if callbacks.Load() != 1 {
		t.Fatalf("close callbacks = %d, want 1", callbacks.Load())
	}
}

func pooledDatagramBacking(t testing.TB, datagrams []protocol.DatagramResult) int64 {
	t.Helper()
	var backing int64
	for i := range datagrams {
		if datagrams[i].Buffer == nil {
			t.Fatalf("datagram %d has no pooled buffer", i)
		}
		buffer := *datagrams[i].Buffer
		if len(buffer) != protocol.DatagramBufferSize || cap(buffer) != protocol.DatagramBufferSize {
			t.Fatalf("datagram %d buffer = len %d/cap %d, want %d/%d", i,
				len(buffer), cap(buffer), protocol.DatagramBufferSize, protocol.DatagramBufferSize)
		}
		backing += int64(cap(buffer))
	}
	return backing
}

func TestClientDsendSnapshotOwnsAndReleasesBatch(t *testing.T) {
	stats := &clientDsendStats{}
	handler := newUDPHandler("127.0.0.1", 1, true, zerolog.Nop(), nil, stats)
	t.Cleanup(handler.Stop)
	var counter atomic.Uint32
	datagrams, err := handler.fragmentDatagrams(1, make([]byte, protocol.MaxUDPPayload+1), &counter)
	if err != nil {
		t.Fatal(err)
	}
	wantBacking := pooledDatagramBacking(t, datagrams)
	if wantBacking != int64(len(datagrams)*protocol.DatagramBufferSize) {
		t.Fatalf("pooled backing = %d, want %d", wantBacking, len(datagrams)*protocol.DatagramBufferSize)
	}
	if got := stats.load(); got.OwnedItems != int64(len(datagrams)) || got.OwnedBacking != wantBacking ||
		got.OwnedItemsHighWater != int64(len(datagrams)) || got.OwnedBackingHighWater != wantBacking {
		t.Fatalf("fragmented Dsend = %+v, want %d items/%d backing", got, len(datagrams), wantBacking)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		first := true
		done <- handler.sendDatagrams(datagrams, func([]byte) error {
			if first {
				first = false
				close(entered)
				<-release
			}
			return nil
		})
	}()
	<-entered
	if got := stats.load(); got.OwnedItems != int64(len(datagrams)) || got.OwnedBacking != wantBacking {
		t.Fatalf("owned Dsend = %+v, want %d items/%d backing", got, len(datagrams), wantBacking)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := stats.load(); got.OwnedItems != 0 || got.OwnedBacking != 0 ||
		got.OwnedItemsHighWater != int64(len(datagrams)) || got.OwnedBackingHighWater != wantBacking {
		t.Fatalf("released Dsend = %+v", got)
	}

	datagrams, err = handler.fragmentDatagrams(2, []byte("payload"), &counter)
	if err != nil {
		t.Fatal(err)
	}
	sendErr := errors.New("send failed")
	if err := handler.sendDatagrams(datagrams, func([]byte) error { return sendErr }); !errors.Is(err, sendErr) {
		t.Fatalf("send error = %v, want %v", err, sendErr)
	}
	if got := stats.load(); got.OwnedItems != 0 || got.OwnedBacking != 0 || got.SendErrors != 1 {
		t.Fatalf("failed Dsend = %+v", got)
	}

	handler.enableFragmentation = false
	if _, err := handler.fragmentDatagrams(3, make([]byte, protocol.MaxUDPPayload+1), &counter); !errors.Is(err, protocol.ErrFragmentationDisabled) {
		t.Fatalf("fragment error = %v", err)
	}
	if got := stats.load(); got.FragmentDrops != 1 {
		t.Fatalf("fragment drops = %+v", got)
	}
}

func TestClientDsendBackingProjectionForPooledBatches(t *testing.T) {
	originalDatagramSize := protocol.DatagramBufferSize
	originalReadSize := protocol.ReadBufferSize
	originalFragmentSize := protocol.FragmentBufferSize
	t.Cleanup(func() {
		if err := protocol.InitBufferPool(originalDatagramSize, originalReadSize, originalFragmentSize); err != nil {
			t.Errorf("restore UDP buffer pool: %v", err)
		}
	})

	for _, datagramSize := range []int{protocol.DefaultDatagramBufferSize, protocol.DefaultDatagramBufferSize + 512} {
		t.Run(strconv.Itoa(datagramSize), func(t *testing.T) {
			if err := protocol.InitBufferPool(datagramSize, protocol.DefaultReadBufferSize, protocol.DefaultFragmentBufferSize); err != nil {
				t.Fatalf("initialize UDP buffer pool: %v", err)
			}
			for _, payloadSize := range []int{1, protocol.DefaultReadBufferSize} {
				stats := &clientDsendStats{}
				handler := newUDPHandler("127.0.0.1", 1, true, zerolog.Nop(), nil, stats)
				var counter atomic.Uint32
				datagrams, err := handler.fragmentDatagrams(1, make([]byte, payloadSize), &counter)
				if err != nil {
					handler.Stop()
					t.Fatal(err)
				}
				actualBacking := pooledDatagramBacking(t, datagrams)
				wantItems := int64(len(datagrams))
				wantBacking := wantItems * int64(datagramSize)
				if snapshot := stats.load(); actualBacking != wantBacking ||
					snapshot.OwnedItems != wantItems || snapshot.OwnedBacking != wantBacking ||
					snapshot.OwnedItemsHighWater != wantItems || snapshot.OwnedBackingHighWater != wantBacking {
					handler.Stop()
					t.Fatalf("payload %d projection = %+v, actual backing %d, want %d items/%d backing",
						payloadSize, snapshot, actualBacking, wantItems, wantBacking)
				}
				if err := handler.sendDatagrams(datagrams, func([]byte) error { return nil }); err != nil {
					handler.Stop()
					t.Fatal(err)
				}
				if snapshot := stats.load(); snapshot.OwnedItems != 0 || snapshot.OwnedBacking != 0 ||
					snapshot.OwnedItemsHighWater != wantItems || snapshot.OwnedBackingHighWater != wantBacking {
					handler.Stop()
					t.Fatalf("payload %d released projection = %+v", payloadSize, snapshot)
				}
				handler.Stop()
			}
		})
	}
}

func TestClientUDPSessionSnapshotHighWater(t *testing.T) {
	budget := newUDPSessionBudget(2)
	releaseFirst, ok := budget.acquire()
	if !ok {
		t.Fatal("first acquire")
	}
	releaseSecond, ok := budget.acquire()
	if !ok {
		t.Fatal("second acquire")
	}
	budget.publish()
	budget.publish()
	if _, ok := budget.acquire(); ok {
		t.Fatal("acquire over capacity")
	}
	if got := budget.snapshot(); got != (UDPSessionSnapshot{Current: 2, Permits: 2, HighWater: 2, Limit: 2, CapacityDrops: 1}) {
		t.Fatalf("active sessions = %+v", got)
	}
	budget.unpublish()
	budget.unpublish()
	releaseFirst()
	releaseSecond()
	if got := budget.snapshot(); got.Current != 0 || got.Permits != 0 || got.HighWater != 2 {
		t.Fatalf("released sessions = %+v", got)
	}
}

func TestClientSnapshotAggregatesLiveAndRetiredAssemblers(t *testing.T) {
	client := &Client{liveUDPHandlers: make(map[*UDPHandler]struct{})}
	first := newUDPHandler("127.0.0.1", 1, true, zerolog.Nop(), nil)
	for sessionID := uint32(0); ; sessionID++ {
		_, err := first.fragmentAssembler.AddFragment(sessionID, 1, 0, 2, []byte("x"))
		if errors.Is(err, protocol.ErrFragmentAssemblerFull) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if sessionID > 5000 {
			t.Fatal("fragment group capacity was not reached")
		}
	}
	second := newUDPHandler("127.0.0.1", 1, true, zerolog.Nop(), nil)
	if _, err := second.fragmentAssembler.AddFragment(1, 1, 0, 2, []byte("second")); err != nil {
		t.Fatal(err)
	}
	firstFragment := first.fragmentAssembler.Snapshot()
	secondFragment := second.fragmentAssembler.Snapshot()
	client.udpMu.Lock()
	client.liveUDPHandlers[first] = struct{}{}
	client.liveUDPHandlers[second] = struct{}{}
	client.udpHandlers.Store("127.0.0.1:8443", first)
	client.udpHandlers.Store("127.0.0.1:9443", second)
	client.udpMu.Unlock()
	if got := client.Snapshot(); got.LiveAssemblers != 2 ||
		got.Fragments.RetainedGroups != firstFragment.RetainedGroups+secondFragment.RetainedGroups ||
		got.Fragments.RetainedBackingBytes != firstFragment.RetainedBackingBytes+secondFragment.RetainedBackingBytes ||
		got.Fragments.GroupCapacityDrops != 1 {
		t.Fatalf("live fragment snapshot = %+v", got)
	}

	first.Stop()
	first.wait()
	client.retireUDPHandler("127.0.0.1:8443", first)
	if got := client.Snapshot(); got.LiveAssemblers != 1 ||
		got.Fragments.RetainedGroups != secondFragment.RetainedGroups ||
		got.Fragments.RetainedBackingBytes != secondFragment.RetainedBackingBytes ||
		got.Fragments.GroupCapacityDrops != 1 {
		t.Fatalf("partially retired fragment snapshot = %+v", got)
	}

	second.Stop()
	second.wait()
	client.retireUDPHandler("127.0.0.1:9443", second)
	got := client.Snapshot()
	if got.LiveAssemblers != 0 || got.Fragments.RetainedGroups != 0 || got.Fragments.RetainedBackingBytes != 0 || got.Fragments.GroupCapacityDrops != 1 {
		t.Fatalf("retired fragment snapshot = %+v", got)
	}
}

func TestClientDsendSnapshotConcurrentBestEffort(t *testing.T) {
	const producers = 8
	stats := &clientDsendStats{}
	handler := newUDPHandler("127.0.0.1", 1, true, zerolog.Nop(), nil, stats)
	t.Cleanup(handler.Stop)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range producers {
		workers.Go(func() {
			var counter atomic.Uint32
			<-start
			for range 200 {
				datagrams, err := handler.fragmentDatagrams(1, make([]byte, 1000), &counter)
				if err != nil {
					t.Errorf("fragment datagrams: %v", err)
					return
				}
				if err := handler.sendDatagrams(datagrams, func([]byte) error { return nil }); err != nil {
					t.Errorf("send datagrams: %v", err)
					return
				}
			}
		})
	}
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	close(start)

	for {
		snapshot := stats.load()
		bufferSize := int64(protocol.DatagramBufferSize)
		if snapshot.OwnedItems < 0 || snapshot.OwnedBacking < 0 ||
			snapshot.OwnedBacking != snapshot.OwnedItems*bufferSize ||
			snapshot.OwnedBackingHighWater != snapshot.OwnedItemsHighWater*bufferSize ||
			snapshot.OwnedItemsHighWater < snapshot.OwnedItems ||
			snapshot.OwnedBackingHighWater < snapshot.OwnedBacking {
			t.Fatalf("invalid moving Dsend snapshot = %+v", snapshot)
		}
		select {
		case <-done:
			if snapshot = stats.load(); snapshot.OwnedItems != 0 || snapshot.OwnedBacking != 0 || snapshot.Workers != 0 ||
				snapshot.OwnedItemsHighWater == 0 || snapshot.OwnedBackingHighWater == 0 {
				t.Fatalf("quiescent Dsend snapshot = %+v", snapshot)
			}
			return
		default:
		}
	}
}

func TestClientDsendHighWaterAggregatePeak(t *testing.T) {
	const producers = 8
	stats := &clientDsendStats{}
	handler := newUDPHandler("127.0.0.1", 1, true, zerolog.Nop(), nil, stats)
	t.Cleanup(handler.Stop)
	acquired := make(chan struct{}, producers)
	release := make(chan struct{})
	var workers sync.WaitGroup
	for i := range producers {
		workers.Go(func() {
			var counter atomic.Uint32
			datagrams, err := handler.fragmentDatagrams(uint32(i+1), make([]byte, 1000), &counter)
			if err != nil {
				t.Errorf("fragment datagrams: %v", err)
				acquired <- struct{}{}
				return
			}
			acquired <- struct{}{}
			<-release
			if err := handler.sendDatagrams(datagrams, func([]byte) error { return nil }); err != nil {
				t.Errorf("send datagrams: %v", err)
			}
		})
	}
	for range producers {
		<-acquired
	}
	wantItems := int64(producers)
	wantBacking := wantItems * int64(protocol.DatagramBufferSize)
	if got := stats.load(); got.OwnedItems != wantItems || got.OwnedBacking != wantBacking ||
		got.OwnedItemsHighWater != wantItems || got.OwnedBackingHighWater != wantBacking {
		t.Fatalf("aggregate Dsend peak = %+v, want %d items/%d backing", got, wantItems, wantBacking)
	}
	if items := stats.ownedItemsHighWater.Load(); items != wantItems {
		t.Fatalf("stored Dsend item high-water = %d, want %d", items, wantItems)
	}
	close(release)
	workers.Wait()
	if got := stats.load(); got.OwnedItems != 0 || got.OwnedBacking != 0 ||
		got.OwnedItemsHighWater != wantItems || got.OwnedBackingHighWater != wantBacking {
		t.Fatalf("released aggregate Dsend peak = %+v", got)
	}
}

func BenchmarkClientDsendFragmentRelease(b *testing.B) {
	stats := &clientDsendStats{}
	handler := newUDPHandler("127.0.0.1", 1, true, zerolog.Nop(), nil, stats)
	b.Cleanup(handler.Stop)
	payload := make([]byte, 1000)
	send := func([]byte) error { return nil }

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var counter atomic.Uint32
		for pb.Next() {
			datagrams, err := handler.fragmentDatagrams(1, payload, &counter)
			if err != nil {
				b.Errorf("fragment datagrams: %v", err)
				return
			}
			if err := handler.sendDatagrams(datagrams, send); err != nil {
				b.Errorf("send datagrams: %v", err)
				return
			}
		}
	})
	b.StopTimer()
	if snapshot := stats.load(); snapshot.OwnedItems != 0 || snapshot.OwnedBacking != 0 {
		b.Fatalf("Dsend ownership after benchmark = %+v, want zero", snapshot)
	}
}
