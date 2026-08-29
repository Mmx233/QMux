package traffic

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Mmx233/QMux/server/pool"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

func newUDPAdmissionUnitHandler(p *pool.ConnectionPool, limit int) *UDPHandler {
	ctx, cancel := context.WithCancel(context.Background())
	return &UDPHandler{
		pool:         p,
		ctx:          ctx,
		cancel:       cancel,
		logger:       zerolog.Nop(),
		sessionSlots: make(chan struct{}, limit),
		receivers:    make(map[*quic.Conn]struct{}),
	}
}

func addTrafficUDPClient(t *testing.T, p *pool.ConnectionPool, id string) *pool.ClientConn {
	t.Helper()
	client := &pool.ClientConn{ID: id, Metadata: pool.ClientMetadata{Capabilities: []string{"udp"}}}
	if err := p.Add(client); err != nil {
		t.Fatalf("Add(%s) error = %v", id, err)
	}
	return client
}

func TestUDPAdmissionListenerLimitWiring(t *testing.T) {
	connectionPool := pool.New("test", pool.NewRoundRobinBalancer(), zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen UDP for admission wiring: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	listener := &Listener{
		Addr:            conn.LocalAddr().String(),
		UDPConn:         conn,
		Pool:            connectionPool,
		ctx:             ctx,
		cancel:          cancel,
		logger:          zerolog.Nop(),
		udpSessionLimit: 2,
	}
	listener.startUDPHandler()
	t.Cleanup(func() {
		listener.close()
		listener.wait()
	})
	if got := cap(listener.udpHandler.sessionSlots); got != listener.udpSessionLimit {
		t.Fatalf("listener UDP session capacity = %d, want %d", got, listener.udpSessionLimit)
	}
}

func TestUDPAdmissionListenerAndGenerationDrops(t *testing.T) {
	t.Run("listener", func(t *testing.T) {
		handler := newUDPAdmissionUnitHandler(nil, 1)
		defer handler.cancel()
		if !handler.acquireSessionSlot() {
			t.Fatal("failed to fill listener capacity")
		}
		if _, err := handler.createSession(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}); !errors.Is(err, errUDPListenerCapacity) {
			t.Fatalf("createSession() error = %v, want listener capacity", err)
		}
		if handler.sessionStats.listenerCapacityDrops.Load() != 1 || handler.sessionStats.generationCapacityDrops.Load() != 0 {
			t.Fatalf("listener/generation drops = %d/%d, want 1/0",
				handler.sessionStats.listenerCapacityDrops.Load(), handler.sessionStats.generationCapacityDrops.Load())
		}
		if !handler.releaseSessionSlot() || handler.sessionStats.held.Load() != 0 || len(handler.sessionSlots) != 0 {
			t.Fatal("listener capacity did not return to zero")
		}
	})

	t.Run("generation", func(t *testing.T) {
		connectionPool := pool.New("test", pool.NewRoundRobinBalancer(), zerolog.Nop())
		defer connectionPool.Stop()
		client := addTrafficUDPClient(t, connectionPool, "client")
		for range 256 {
			selected, err := connectionPool.ReserveUDP()
			if err != nil || selected != client {
				t.Fatalf("fill generation capacity = (%p, %v), want %p", selected, err, client)
			}
		}
		handler := newUDPAdmissionUnitHandler(connectionPool, 1)
		defer handler.cancel()
		if _, err := handler.createSession(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2}); !errors.Is(err, pool.ErrUDPGenerationCapacity) {
			t.Fatalf("createSession() error = %v, want generation capacity", err)
		}
		if handler.sessionStats.listenerCapacityDrops.Load() != 0 || handler.sessionStats.generationCapacityDrops.Load() != 1 {
			t.Fatalf("listener/generation drops = %d/%d, want 0/1",
				handler.sessionStats.listenerCapacityDrops.Load(), handler.sessionStats.generationCapacityDrops.Load())
		}
		if handler.sessionStats.held.Load() != 0 || len(handler.sessionSlots) != 0 || handler.sessionStats.accountingFaults.Load() != 0 {
			t.Fatal("generation rejection leaked listener admission")
		}
		for range 256 {
			if !connectionPool.ReleaseUDP(client) {
				t.Fatal("ReleaseUDP() rejected a filled generation reservation")
			}
		}
	})
}

func TestUDPAdmissionDuplicateRollbackAndPostPublishRecheck(t *testing.T) {
	connectionPool := pool.New("test", pool.NewRoundRobinBalancer(), zerolog.Nop())
	defer connectionPool.Stop()
	client := addTrafficUDPClient(t, connectionPool, "client")
	handler := newUDPAdmissionUnitHandler(connectionPool, 2)
	defer handler.cancel()

	published := make(chan struct{})
	resume := make(chan struct{})
	resumed := false
	defer func() {
		if !resumed {
			close(resume)
		}
	}()
	handler.afterSessionPublish = func() {
		close(published)
		<-resume
	}

	type result struct {
		session *UDPSession
		err     error
	}
	firstResult := make(chan result, 1)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3}
	go func() {
		session, err := handler.createSession(addr)
		firstResult <- result{session: session, err: err}
	}()
	<-published

	duplicate, err := handler.createSession(addr)
	if err != nil || duplicate == nil {
		t.Fatalf("duplicate createSession() = (%p, %v), want published session", duplicate, err)
	}
	if handler.sessionStats.held.Load() != 1 || len(handler.sessionSlots) != 1 {
		t.Fatalf("duplicate rollback held/channel = %d/%d, want 1/1", handler.sessionStats.held.Load(), len(handler.sessionSlots))
	}
	if client.ActiveConns.Load() != 1 || client.TotalConns.Load() != 1 {
		t.Fatalf("duplicate active/total = %d/%d, want 1/1", client.ActiveConns.Load(), client.TotalConns.Load())
	}
	if syncMapLen(&handler.sessions) != 1 || syncMapLen(&handler.sessionsByID) != 1 {
		t.Fatal("duplicate publication changed the primary or secondary session map")
	}

	close(resume)
	resumed = true
	first := <-firstResult
	if first.session != nil || !errors.Is(first.err, pool.ErrNoEligibleClients) {
		t.Fatalf("post-publish recheck = (%p, %v), want retired exact session", first.session, first.err)
	}
	if syncMapLen(&handler.sessions) != 0 || syncMapLen(&handler.sessionsByID) != 0 {
		t.Fatal("post-publish exact close retained session maps")
	}
	if handler.sessionStats.held.Load() != 0 || len(handler.sessionSlots) != 0 || client.ActiveConns.Load() != 0 {
		t.Fatalf("post-publish close held/channel/active = %d/%d/%d, want 0/0/0",
			handler.sessionStats.held.Load(), len(handler.sessionSlots), client.ActiveConns.Load())
	}
	if handler.sessionStats.highWater.Load() != 2 || handler.sessionStats.accountingFaults.Load() != 0 {
		t.Fatalf("high-water/faults = %d/%d, want 2/0",
			handler.sessionStats.highWater.Load(), handler.sessionStats.accountingFaults.Load())
	}
}

func TestUDPAdmissionCompositeReleaseAccountingFaults(t *testing.T) {
	connectionPool := pool.New("test", pool.NewRoundRobinBalancer(), zerolog.Nop())
	defer connectionPool.Stop()
	client := addTrafficUDPClient(t, connectionPool, "client")
	handler := newUDPAdmissionUnitHandler(connectionPool, 1)
	defer handler.cancel()

	release := handler.newSessionAdmissionRelease(client)
	release()
	release()
	if handler.sessionStats.accountingFaults.Load() != 2 {
		t.Fatalf("accounting faults = %d, want one listener and one generation fault", handler.sessionStats.accountingFaults.Load())
	}
	if handler.sessionStats.held.Load() != 0 || len(handler.sessionSlots) != 0 {
		t.Fatal("faulting composite release made listener accounting negative")
	}
}

func TestUDPAdmissionPostPublishRetirementIsExact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := newUDPSenderQUICPair(t, ctx)
	connectionPool := pool.New("test", pool.NewRoundRobinBalancer(), zerolog.Nop())
	defer connectionPool.Stop()
	stale := &pool.ClientConn{
		ID:       "same-id",
		Conn:     pair.server,
		Metadata: pool.ClientMetadata{Capabilities: []string{"udp"}},
	}
	if err := connectionPool.Add(stale); err != nil {
		t.Fatalf("Add(stale) error = %v", err)
	}
	handler := newUDPAdmissionUnitHandler(connectionPool, 1)
	defer func() {
		handler.cancel()
		handler.wait()
	}()

	var fresh *pool.ClientConn
	handler.afterSessionPublish = func() {
		if !connectionPool.Remove(stale) {
			t.Error("Remove(stale) = false")
			return
		}
		fresh = addTrafficUDPClient(t, connectionPool, stale.ID)
	}
	session, err := handler.createSession(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4})
	if session != nil || !errors.Is(err, pool.ErrNoEligibleClients) {
		t.Fatalf("createSession() after exact retirement = (%p, %v)", session, err)
	}
	if fresh == nil {
		t.Fatal("replacement generation was not installed")
	}
	if syncMapLen(&handler.sessions) != 0 || syncMapLen(&handler.sessionsByID) != 0 || handler.sessionStats.held.Load() != 0 {
		t.Fatal("retired publication retained session ownership")
	}
	if stale.ActiveConns.Load() != 0 || stale.TotalConns.Load() != 1 || fresh.ActiveConns.Load() != 0 || fresh.TotalConns.Load() != 0 {
		t.Fatalf("stale/fresh active/total = %d/%d and %d/%d, want 0/1 and 0/0",
			stale.ActiveConns.Load(), stale.TotalConns.Load(), fresh.ActiveConns.Load(), fresh.TotalConns.Load())
	}
	selected, err := connectionPool.ReserveUDP()
	if err != nil || selected != fresh {
		t.Fatalf("ReserveUDP() after stale release = (%p, %v), want fresh %p", selected, err, fresh)
	}
	if !connectionPool.ReleaseUDP(selected) {
		t.Fatal("ReleaseUDP(fresh) = false")
	}
}
