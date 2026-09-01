package client

import (
	"context"
	"crypto/tls"
	"errors"
	"reflect"
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

func TestClientCapacityMapsToProcessUDPSessionBudget(t *testing.T) {
	client, err := New(&config.Client{
		ClientID: "capacity-map",
		Server: config.ClientServer{Servers: []config.ServerEndpoint{{
			Address: "127.0.0.1:1", ServerName: "snapshot.test",
		}}},
		Local:    config.LocalService{Host: "127.0.0.1", Port: 1},
		TLS:      lifecycleClientTLSFiles(t),
		Capacity: config.ClientCapacity{MaxLocalUDPSessions: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop() })

	if got := client.Snapshot().UDPSessions.Limit; got != 7 {
		t.Fatalf("UDP session limit = %d, want 7", got)
	}
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

func TestClientDsendSnapshotOwnsAndReleasesBatch(t *testing.T) {
	stats := &clientDsendStats{}
	handler := newUDPHandler("127.0.0.1", 1, true, zerolog.Nop(), nil, stats)
	t.Cleanup(handler.Stop)
	var counter atomic.Uint32
	datagrams, err := handler.fragmentDatagrams(1, make([]byte, protocol.MaxUDPPayload+1), &counter)
	if err != nil {
		t.Fatal(err)
	}
	wantBacking := datagramBackingBytes(datagrams)
	if wantBacking != int64(len(datagrams)*protocol.MaxDatagramSize) {
		t.Fatalf("pooled backing = %d, want %d", wantBacking, len(datagrams)*protocol.MaxDatagramSize)
	}
	if got := stats.load(); got.OwnedItems != int64(len(datagrams)) || got.OwnedBacking != wantBacking ||
		got.OwnedItemsHighWater != int64(len(datagrams)) || got.OwnedBackingHighWater != wantBacking {
		t.Fatalf("fragmented Dsend = %+v, want %d items/%d backing", got, len(datagrams), wantBacking)
	}
	if got := datagramBackingBytes([]protocol.DatagramResult{{Data: make([]byte, 7)}}); got != 7 {
		t.Fatalf("unpooled backing = %d, want 7", got)
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
