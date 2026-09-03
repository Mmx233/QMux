package traffic

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
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
		sessionLimit: int64(limit),
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

func TestCanonicalUDPAddrPort(t *testing.T) {
	mapped := netip.MustParseAddrPort("[::ffff:192.0.2.1]:53")
	derivedMapped := (&net.UDPAddr{IP: mapped.Addr().AsSlice(), Port: int(mapped.Port())}).AddrPort()
	tests := []struct {
		name string
		in   netip.AddrPort
		want netip.AddrPort
	}{
		{name: "native IPv4", in: netip.MustParseAddrPort("192.0.2.1:53"), want: netip.MustParseAddrPort("192.0.2.1:53")},
		{name: "mapped IPv4", in: mapped, want: netip.MustParseAddrPort("192.0.2.1:53")},
		{name: "UDPAddr mapped IPv4", in: derivedMapped, want: netip.MustParseAddrPort("192.0.2.1:53")},
		{name: "zoned IPv6", in: netip.MustParseAddrPort("[fe80::1%en0]:53"), want: netip.MustParseAddrPort("[fe80::1%en0]:53")},
		{name: "distinct port", in: netip.MustParseAddrPort("192.0.2.1:54"), want: netip.MustParseAddrPort("192.0.2.1:54")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := canonicalUDPAddrPort(test.in); got != test.want {
				t.Fatalf("canonicalUDPAddrPort(%v) = %v, want %v", test.in, got, test.want)
			}
		})
	}
	if canonicalUDPAddrPort(tests[0].in) == canonicalUDPAddrPort(tests[4].in) {
		t.Fatal("canonical UDP addresses with distinct ports compare equal")
	}
}

func TestUDPProcessPacketCanonicalMappedHit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := newUDPSenderQUICPair(t, ctx)
	handlerCtx, cancelHandler := context.WithCancel(ctx)
	defer cancelHandler()
	client := &pool.ClientConn{ID: "mapped", Conn: pair.server}
	sender := &udpSender{
		client: client,
		queue:  make(chan udpSendBatch, 1),
		done:   make(chan struct{}),
	}
	native := netip.MustParseAddrPort("192.0.2.1:12345")
	session := &UDPSession{id: 7, clientAddr: native, client: client, sender: sender}
	session.lastActive.Store(1)
	client.ActiveConns.Store(1)
	handler := &UDPHandler{ctx: handlerCtx, logger: zerolog.Nop(), enableFragmentation: true}
	handler.nextSessionID.Store(19)
	handler.sessions.Store(native, session)
	handler.sessionsByID.Store(session.id, session)
	handler.sessionStats.publish()

	handler.processPacket([]byte("mapped hit"), netip.MustParseAddrPort("[::ffff:192.0.2.1]:12345"))

	if session.lastActive.Load() == 1 {
		t.Fatal("mapped packet did not hit and refresh the native-key session")
	}
	if got := handler.nextSessionID.Load(); got != 19 {
		t.Fatalf("next session ID = %d, want unchanged 19", got)
	}
	if got, ok := handler.sessions.Load(native); !ok || got != session || syncMapLen(&handler.sessions) != 1 || syncMapLen(&handler.sessionsByID) != 1 {
		t.Fatalf("canonical mapped hit published a duplicate: address=(%p, %v), maps=%d/%d",
			got, ok, syncMapLen(&handler.sessions), syncMapLen(&handler.sessionsByID))
	}
	handler.failSender(sender)
	if snapshot := handler.snapshot(); snapshot.SessionsCurrent != 0 || snapshot.DSendItems != 0 || snapshot.DSendBackingBytes != 0 || client.ActiveConns.Load() != 0 {
		t.Fatalf("mapped-hit cleanup = %+v, active=%d", snapshot, client.ActiveConns.Load())
	}
}

func TestUDPCleanupExpiredCanonicalSession(t *testing.T) {
	handler := newUDPAdmissionUnitHandler(nil, 1)
	defer handler.cancel()
	if !handler.acquireSessionSlot() {
		t.Fatal("acquire cleanup fixture slot")
	}
	client := &pool.ClientConn{ID: "expired"}
	client.ActiveConns.Store(1)
	addr := netip.MustParseAddrPort("127.0.0.1:12345")
	session := &UDPSession{id: 8, clientAddr: addr, client: client}
	session.lastActive.Store(1)
	session.releaseAdmission = func() { handler.releaseSessionSlot() }
	handler.sessions.Store(addr, session)
	handler.sessionsByID.Store(session.id, session)
	handler.sessionStats.publish()

	handler.cleanupExpiredSessions()

	snapshot := handler.snapshot()
	if syncMapLen(&handler.sessions) != 0 || syncMapLen(&handler.sessionsByID) != 0 ||
		snapshot.SessionsCurrent != 0 || snapshot.SessionPermits != 0 || len(handler.sessionSlots) != 0 ||
		client.ActiveConns.Load() != 0 {
		t.Fatalf("expired canonical cleanup: maps=%d/%d snapshot=%+v slots=%d active=%d",
			syncMapLen(&handler.sessions), syncMapLen(&handler.sessionsByID), snapshot,
			len(handler.sessionSlots), client.ActiveConns.Load())
	}
}

func TestUDPAdmissionListenerLimitWiring(t *testing.T) {
	const quicAddr = "udp-limit-test"
	connectionPool := pool.New(quicAddr, pool.NewRoundRobinBalancer(), zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	ctx, cancel := context.WithCancel(context.Background())
	manager := NewManager(&config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    quicAddr,
		TrafficAddr: "127.0.0.1:0",
		Protocol:    "udp",
		Capacity:    config.ListenerCapacity{MaxUDPSessions: 2},
	}}}, map[string]*pool.ConnectionPool{quicAddr: connectionPool}, zerolog.Nop())
	if err := manager.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start UDP admission manager: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		manager.Stop()
	})
	listener := manager.listeners[0]
	if got := cap(listener.udpHandler.sessionSlots); got != 2 {
		t.Fatalf("listener UDP session capacity = %d, want 2", got)
	}
	if snapshot := manager.UDPAdmissionSnapshots()[0]; snapshot.SessionLimit != 2 {
		t.Fatalf("UDP snapshot session limit = %d, want 2", snapshot.SessionLimit)
	}
}

func TestUDPAdmissionListenerAndGenerationDrops(t *testing.T) {
	t.Run("listener", func(t *testing.T) {
		handler := newUDPAdmissionUnitHandler(nil, 1)
		defer handler.cancel()
		if !handler.acquireSessionSlot() {
			t.Fatal("failed to fill listener capacity")
		}
		if _, err := handler.createSession(netip.MustParseAddrPort("127.0.0.1:1")); !errors.Is(err, errUDPListenerCapacity) {
			t.Fatalf("createSession() error = %v, want listener capacity", err)
		}
		snapshot := handler.snapshot()
		if snapshot.ListenerCapacityDrops != 1 || snapshot.GenerationCapacityDrops != 0 {
			t.Fatalf("listener/generation drops = %d/%d, want 1/0",
				snapshot.ListenerCapacityDrops, snapshot.GenerationCapacityDrops)
		}
		if !handler.releaseSessionSlot() {
			t.Fatal("listener capacity release failed")
		}
		if snapshot := handler.sessionStats.snapshot(); snapshot.SessionPermits != 0 || len(handler.sessionSlots) != 0 {
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
		if _, err := handler.createSession(netip.MustParseAddrPort("127.0.0.1:2")); !errors.Is(err, pool.ErrUDPGenerationCapacity) {
			t.Fatalf("createSession() error = %v, want generation capacity", err)
		}
		snapshot := handler.snapshot()
		if snapshot.ListenerCapacityDrops != 0 || snapshot.GenerationCapacityDrops != 1 {
			t.Fatalf("listener/generation drops = %d/%d, want 0/1",
				snapshot.ListenerCapacityDrops, snapshot.GenerationCapacityDrops)
		}
		if snapshot := handler.sessionStats.snapshot(); snapshot.SessionPermits != 0 || len(handler.sessionSlots) != 0 || snapshot.AccountingFaults != 0 {
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
	addr := netip.MustParseAddrPort("127.0.0.1:3")
	go func() {
		session, err := handler.createSession(addr)
		firstResult <- result{session: session, err: err}
	}()
	<-published

	duplicate, err := handler.createSession(addr)
	if err != nil || duplicate == nil {
		t.Fatalf("duplicate createSession() = (%p, %v), want published session", duplicate, err)
	}
	if snapshot := handler.sessionStats.snapshot(); snapshot.SessionPermits != 1 || len(handler.sessionSlots) != 1 {
		t.Fatalf("duplicate rollback held/channel = %d/%d, want 1/1", snapshot.SessionPermits, len(handler.sessionSlots))
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
	if snapshot := handler.sessionStats.snapshot(); snapshot.SessionPermits != 0 || len(handler.sessionSlots) != 0 || client.ActiveConns.Load() != 0 {
		t.Fatalf("post-publish close held/channel/active = %d/%d/%d, want 0/0/0",
			snapshot.SessionPermits, len(handler.sessionSlots), client.ActiveConns.Load())
	}
	if snapshot := handler.sessionStats.snapshot(); snapshot.SessionHighWater != 2 || snapshot.AccountingFaults != 0 {
		t.Fatalf("high-water/faults = %d/%d, want 2/0",
			snapshot.SessionHighWater, snapshot.AccountingFaults)
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
	if got := handler.snapshot().AccountingFaults; got != 2 {
		t.Fatalf("accounting faults = %d, want one listener and one generation fault", got)
	}
	if snapshot := handler.sessionStats.snapshot(); snapshot.SessionPermits != 0 || len(handler.sessionSlots) != 0 {
		t.Fatal("faulting composite release made listener accounting negative")
	}

	t.Run("token present held underflow", func(t *testing.T) {
		handler := newUDPAdmissionUnitHandler(nil, 1)
		defer handler.cancel()
		if !handler.acquireSessionSlot() {
			t.Fatal("initial listener slot acquisition failed")
		}
		handler.sessionStats.mu.Lock()
		handler.sessionStats.held = 0
		handler.sessionStats.mu.Unlock()
		if handler.releaseSessionSlot() {
			t.Fatal("releaseSessionSlot() succeeded with zero held accounting")
		}
		snapshot := handler.sessionStats.snapshot()
		if snapshot.SessionPermits != 0 || len(handler.sessionSlots) != 0 || snapshot.AccountingFaults != 1 {
			t.Fatalf("held underflow snapshot = %+v, slots=%d, want restored zero held, empty channel, and one fault",
				snapshot, len(handler.sessionSlots))
		}
	})
}

func TestUDPAdmissionAccountingFaultFailsClosedAndDrainsHeldSlot(t *testing.T) {
	handler := newUDPAdmissionUnitHandler(nil, 2)
	defer handler.cancel()
	if !handler.acquireSessionSlot() {
		t.Fatal("initial listener slot acquisition failed")
	}
	handler.sessionStats.publish()
	handler.sessionStats.accountingFault()

	before := handler.snapshot()
	if handler.acquireSessionSlot() {
		t.Fatal("listener slot acquisition succeeded after accounting fault")
	}
	after := handler.snapshot()
	if after.ListenerCapacityDrops != before.ListenerCapacityDrops || after.SessionsCurrent != 1 || after.SessionPermits != 1 {
		t.Fatalf("fault rejection snapshot = %+v, want one existing session/permit and no capacity drop", after)
	}
	handler.sessionStats.unpublish()
	if !handler.releaseSessionSlot() {
		t.Fatal("pre-fault listener slot did not release")
	}
	final := handler.snapshot()
	if final.SessionsCurrent != 0 || final.SessionPermits != 0 || final.AccountingFaults != 1 || final.ListenerCapacityDrops != 0 {
		t.Fatalf("drained fault snapshot = %+v, want zero sessions/permits, one fault, and no capacity drops", final)
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
	session, err := handler.createSession(netip.MustParseAddrPort("127.0.0.1:4"))
	if session != nil || !errors.Is(err, pool.ErrNoEligibleClients) {
		t.Fatalf("createSession() after exact retirement = (%p, %v)", session, err)
	}
	if fresh == nil {
		t.Fatal("replacement generation was not installed")
	}
	if snapshot := handler.sessionStats.snapshot(); syncMapLen(&handler.sessions) != 0 || syncMapLen(&handler.sessionsByID) != 0 || snapshot.SessionPermits != 0 {
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
