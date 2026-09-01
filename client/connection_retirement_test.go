package client

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

func newRetirementManager(t *testing.T, address string) *ConnectionManager {
	t.Helper()
	cm, err := NewConnectionManager(&config.Client{
		ClientID:          "retirement-client",
		HeartbeatInterval: time.Hour,
		HealthTimeout:     2 * time.Hour,
		Server: config.ClientServer{Servers: []config.ServerEndpoint{{
			Address:    address,
			ServerName: "lifecycle.test",
		}}},
	}, zerolog.Nop())
	if err != nil {
		t.Fatalf("create retirement manager: %v", err)
	}
	t.Cleanup(func() { _ = cm.Stop() })
	return cm
}

func newDisconnectedRetirementConnection(address string) *ServerConnection {
	sc := NewServerConnection(address, "lifecycle.test", tls.NewLRUClientSessionCache(1), zerolog.Nop())
	sc.controlOnce.Do(func() {})
	return sc
}

func awaitRetirementCondition(t *testing.T, event string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", event)
		case <-ticker.C:
		}
	}
}

type firstErrGateContext struct {
	context.Context
	checked chan struct{}
	release chan struct{}
	once    sync.Once
}

func (ctx *firstErrGateContext) Err() error {
	err := ctx.Context.Err()
	ctx.once.Do(func() {
		close(ctx.checked)
		<-ctx.release
	})
	return err
}

func TestConnectionPublicationRetiresExactPreviousAndRejectsStaleCallback(t *testing.T) {
	const address = "127.0.0.1:8443"
	cm := newRetirementManager(t, address)
	old := newDisconnectedRetirementConnection(address)
	fresh := newDisconnectedRetirementConnection(address)

	if !cm.publishServerConnection(context.Background(), old) {
		t.Fatal("publish old connection failed")
	}
	if got := awaitLifecycle(t, cm.NewConns, "old connection delivery"); got != old {
		t.Fatalf("old delivery = %p, want %p", got, old)
	}
	if !cm.publishServerConnection(context.Background(), fresh) {
		t.Fatal("publish fresh connection failed")
	}
	if got := awaitLifecycle(t, cm.NewConns, "fresh connection delivery"); got != fresh {
		t.Fatalf("fresh delivery = %p, want %p", got, fresh)
	}
	if old.State() != StateDisconnected || old.IsHealthy() {
		t.Fatalf("retired old state = %s healthy=%t", old.State(), old.IsHealthy())
	}
	cm.rollbackPublication(fresh)
	if got := cm.GetConnection(address); got != nil {
		t.Fatalf("rollback retained connection %p", got)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteCloseWaitsForReconnectCallbackBeforeRuntimeCleanup(t *testing.T) {
	peer := newLifecyclePeer(t)
	cm := newLifecycleManager(t, peer)
	closePeer := make(chan struct{})
	peerDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(stream); err != nil {
			return err
		}
		<-closePeer
		return conn.CloseWithError(0, "test remote close")
	})
	runCtx, cancelRun := context.WithCancel(context.Background())
	forceCtx, cancelForce := context.WithCancel(context.Background())
	client := &Client{
		config:          cm.config,
		connMgr:         cm,
		udpBudget:       newUDPSessionBudget(0),
		dsendStats:      &clientDsendStats{},
		liveUDPHandlers: make(map[*UDPHandler]struct{}),
		forceCtx:        forceCtx,
		forceCancel:     cancelForce,
		runtimes:        make(map[*ServerConnection]*connectionRuntime),
		logger:          zerolog.Nop(),
	}
	callbackRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(callbackRelease) }) }
	t.Cleanup(func() {
		releaseCallback()
		cancelRun()
		_ = cm.Stop()
		cancelForce()
		client.watcherWG.Wait()
	})

	old, err := cm.connectAndRegister(runCtx, peer.endpoint())
	if err != nil {
		t.Fatalf("connect old generation: %v", err)
	}
	callbackEntered := make(chan struct{})
	var callbackOnce sync.Once
	old.controlMu.Lock()
	controlLocked := true
	defer func() {
		if controlLocked {
			old.controlMu.Unlock()
		}
	}()
	published := make(chan bool, 1)
	go func() { published <- cm.publishServerConnection(runCtx, old) }()
	got := awaitLifecycle(t, cm.NewConns, "old generation delivery")
	originalReconnect := old.reconnectCallback
	if originalReconnect == nil {
		old.controlMu.Unlock()
		controlLocked = false
		t.Fatal("publication did not install the reconnect callback")
	}
	old.SetReconnectCallback(func(address string) {
		callbackOnce.Do(func() { close(callbackEntered) })
		<-callbackRelease
		originalReconnect(address)
	})
	old.controlMu.Unlock()
	controlLocked = false
	if got != old {
		t.Fatalf("delivered generation = %p, want %p", got, old)
	}
	client.installRuntime(old)
	if !awaitLifecycle(t, published, "old generation publication") {
		t.Fatal("publish old generation failed")
	}
	client.runtimesMu.Lock()
	runtime := client.runtimes[old]
	client.runtimesMu.Unlock()
	if runtime == nil {
		t.Fatal("old generation runtime was not installed")
	}

	conn := old.Connection()
	close(closePeer)
	awaitLifecycle(t, callbackEntered, "remote-close reconnect callback")
	awaitLifecycle(t, conn.Context().Done(), "remote QUIC close")
	select {
	case <-old.closeDone:
		t.Fatal("runtime cleanup closed the generation before reconnect callback completed")
	default:
	}
	if current := cm.GetConnection(old.ServerAddr()); current != old {
		t.Fatalf("manager current = %p, want blocked old generation %p", current, old)
	}
	before := cm.endpointSnapshot()[0]
	if before.Registered != 1 || before.Retiring != 0 || before.AccountingFaults != 0 {
		t.Fatalf("blocked callback accounting = %+v, want one registered generation", before)
	}

	releaseCallback()
	awaitLifecycle(t, old.closeDone, "old generation close completion")
	awaitLifecycle(t, runtime.cleanupDone, "old runtime cleanup")
	cancelRun()
	if current := cm.GetConnection(old.ServerAddr()); current != nil {
		t.Fatalf("manager retained closed generation %p", current)
	}
	if old.Connection() != nil || old.State() != StateDisconnected {
		t.Fatalf("old generation survived close: conn=%p state=%s", old.Connection(), old.State())
	}
	after := cm.endpointSnapshot()[0]
	if after.Registered != 0 || after.Retiring != 0 || after.AccountingFaults != 0 {
		t.Fatalf("closed callback accounting = %+v, want all generation counts closed", after)
	}
	if err := awaitLifecycle(t, peerDone, "remote-close peer exit"); err != nil {
		t.Fatal(err)
	}
}

func TestBlockedReplacementPublicationStopClosesBothGenerations(t *testing.T) {
	const address = "127.0.0.1:8443"
	cm := newRetirementManager(t, address)
	old := newDisconnectedRetirementConnection(address)
	old.MarkHealthy()
	if !cm.publishServerConnection(context.Background(), old) {
		t.Fatal("publish old generation failed")
	}
	awaitLifecycle(t, cm.NewConns, "old generation delivery")

	cm.NewConns = make(chan *ServerConnection)
	fresh := newDisconnectedRetirementConnection(address)
	published := make(chan bool, 1)
	cm.publishMu.Lock()
	cm.wg.Go(func() {
		committed := cm.publishServerConnection(context.Background(), fresh)
		if !committed {
			_ = fresh.Close()
		}
		published <- committed
	})
	cm.publishMu.Unlock()
	awaitRetirementCondition(t, "blocked replacement publication", func() bool {
		return cm.GetConnection(address) == fresh
	})
	awaitLifecycle(t, old.closeDone, "old generation retirement")
	if old.State() != StateDisconnected || old.IsHealthy() {
		t.Fatalf("blocked replacement retained old generation: state=%s healthy=%t", old.State(), old.IsHealthy())
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- cm.Stop() }()
	if err := awaitLifecycle(t, stopDone, "Stop during replacement delivery"); err != nil {
		t.Fatal(err)
	}
	if <-published {
		t.Fatal("blocked replacement committed during Stop")
	}
	if fresh.State() != StateDisconnected || fresh.IsHealthy() {
		t.Fatalf("blocked replacement retained fresh generation: state=%s healthy=%t", fresh.State(), fresh.IsHealthy())
	}
	if got := cm.GetConnection(address); got != nil {
		t.Fatalf("Stop retained connection %p", got)
	}
}

func TestReconnectWorkerStaleExpectedAndInitialFailureAreNoOps(t *testing.T) {
	const address = "127.0.0.1:8443"
	cm := newRetirementManager(t, address)
	stale := newDisconnectedRetirementConnection(address)
	fresh := newDisconnectedRetirementConnection(address)
	fresh.MarkHealthy()
	cm.connections.Store(address, fresh)

	cm.startReconnection(context.Background(), address, stale)
	cm.startReconnection(context.Background(), address, nil)
	cm.reconnectMu.Lock()
	if len(cm.reconnecting) != 0 {
		cm.reconnectMu.Unlock()
		t.Fatal("stale or initial callback acquired a reconnect slot")
	}
	cm.reconnectMu.Unlock()

	cm.reconnectMu.Lock()
	cm.reconnecting[address] = true
	cm.reconnectMu.Unlock()
	cm.reconnectionLoop(context.Background(), address, stale)
	if got := cm.GetConnection(address); got != fresh {
		t.Fatalf("stale worker changed current connection to %p", got)
	}
	if !fresh.IsHealthy() || fresh.State() != StateConnected {
		t.Fatalf("stale worker retired fresh connection: state=%s healthy=%t", fresh.State(), fresh.IsHealthy())
	}
	cm.reconnectMu.Lock()
	remaining := len(cm.reconnecting)
	cm.reconnectMu.Unlock()
	if remaining != 0 {
		t.Fatalf("stale worker retained %d reconnect slots", remaining)
	}
	cm.reconnectMu.Lock()
	cm.reconnecting[address] = true
	cm.reconnectMu.Unlock()
	cm.reconnectionLoop(context.Background(), address, nil)
	if got := cm.GetConnection(address); got != fresh {
		t.Fatalf("initial-failure worker changed current connection to %p", got)
	}
	cm.reconnectMu.Lock()
	remaining = len(cm.reconnecting)
	cm.reconnectMu.Unlock()
	if remaining != 0 {
		t.Fatalf("initial-failure worker retained %d reconnect slots", remaining)
	}

	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestReconnectReleasesSlotBeforeFreshPublicationCallback(t *testing.T) {
	peer := newLifecyclePeer(t)
	cm := newLifecycleManager(t, peer)
	t.Cleanup(func() { _ = cm.Stop() })
	cm.config.HeartbeatInterval = time.Hour
	cm.config.HealthTimeout = 2 * time.Hour
	cm.NewConns = make(chan *ServerConnection)
	address := peer.endpoint().Address

	oldServerDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(stream); err != nil {
			return err
		}
		<-conn.Context().Done()
		return nil
	})
	old, err := cm.connectAndRegister(context.Background(), peer.endpoint())
	if err != nil {
		t.Fatalf("connect old generation: %v", err)
	}
	oldPublished := make(chan bool, 1)
	go func() { oldPublished <- cm.publishServerConnection(context.Background(), old) }()
	if got := awaitLifecycle(t, cm.NewConns, "old reconnect generation delivery"); got != old {
		t.Fatalf("old delivery = %p, want %p", got, old)
	}
	if !awaitLifecycle(t, oldPublished, "old reconnect generation publication") {
		t.Fatal("publish old generation failed")
	}

	freshRegistered := make(chan struct{})
	freshServerDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(stream); err != nil {
			return err
		}
		close(freshRegistered)
		<-conn.Context().Done()
		return nil
	})
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	cm.startReconnection(runCtx, address, old)
	if err := awaitLifecycle(t, oldServerDone, "old generation retirement before backoff"); err != nil {
		t.Fatal(err)
	}
	awaitLifecycle(t, freshRegistered, "fresh reconnect registration")

	var fresh *ServerConnection
	awaitRetirementCondition(t, "fresh generation blocked at NewConns delivery", func() bool {
		fresh = cm.GetConnection(address)
		return fresh != nil && fresh != old
	})
	cm.reconnectMu.Lock()
	firstSlotHeld := cm.reconnecting[address]
	cm.reconnectMu.Unlock()
	if firstSlotHeld {
		t.Fatal("successful Register retained the old reconnect slot before publication")
	}

	controlStream := fresh.controlStream.Swap(nil)
	if controlStream == nil {
		t.Fatal("fresh registration did not install a control stream")
	}
	if err := controlStream.Close(); err != nil {
		t.Fatalf("close fresh control stream: %v", err)
	}
	if err := fresh.SendHeartbeat(); err == nil {
		t.Fatal("heartbeat with retired control stream unexpectedly succeeded")
	}
	awaitRetirementCondition(t, "fresh callback reconnect intent", func() bool {
		cm.reconnectMu.Lock()
		defer cm.reconnectMu.Unlock()
		return cm.reconnecting[address] && cm.GetConnection(address) == nil
	})

	cancelRun()
	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := awaitLifecycle(t, freshServerDone, "fresh callback generation retirement"); err != nil {
		t.Fatal(err)
	}
}

func TestClientRetiresOneHundredExactGenerationsAndNoSuccessor(t *testing.T) {
	peer := newLifecyclePeer(t)
	cm := newLifecycleManager(t, peer)
	cm.config.HeartbeatInterval = time.Hour
	cm.config.HealthTimeout = 2 * time.Hour
	backend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen generation UDP backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	cm.config.Local = config.LocalService{
		Host: "127.0.0.1",
		Port: backend.LocalAddr().(*net.UDPAddr).Port,
	}
	forceCtx, cancelForce := context.WithCancel(context.Background())
	client := &Client{
		config:          cm.config,
		connMgr:         cm,
		udpBudget:       newUDPSessionBudget(0),
		dsendStats:      &clientDsendStats{},
		liveUDPHandlers: make(map[*UDPHandler]struct{}),
		forceCtx:        forceCtx,
		forceCancel:     cancelForce,
		runtimes:        make(map[*ServerConnection]*connectionRuntime),
		logger:          zerolog.Nop(),
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	client.producerWG.Go(client.handleNewConnections)
	t.Cleanup(func() {
		cancelRun()
		_ = cm.Stop()
		client.producerWG.Wait()
		cancelForce()
		client.udpHandlers.Range(func(_, value any) bool {
			value.(*UDPHandler).stopAndWait()
			return true
		})
	})

	const generations = 100
	var currentSC *ServerConnection
	var currentHandler *UDPHandler
	var currentSession *UDPSession
	var currentServerDone <-chan error
	for generation := range generations {
		accepted := make(chan struct{})
		serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
			if err := writeSuccessfulLifecycleAck(stream); err != nil {
				return err
			}
			close(accepted)
			<-conn.Context().Done()
			return nil
		})
		sc, err := cm.connectAndRegister(context.Background(), peer.endpoint())
		if err != nil {
			t.Fatalf("generation %d connect: %v", generation, err)
		}
		awaitLifecycle(t, accepted, "generation registration acknowledgment")
		publishCtx, cancelPublish := context.WithTimeout(runCtx, 10*time.Second)
		published := cm.publishServerConnection(publishCtx, sc)
		cancelPublish()
		if !published {
			_ = sc.Close()
			t.Fatalf("generation %d publication failed", generation)
		}

		var handler *UDPHandler
		awaitRetirementCondition(t, "generation UDP handler installation", func() bool {
			value, ok := client.udpHandlers.Load(sc.ServerAddr())
			if !ok {
				return false
			}
			handler = value.(*UDPHandler)
			return handler != currentHandler
		})
		if currentSC != nil {
			if currentHandler == nil || currentSession == nil || currentServerDone == nil {
				t.Fatal("old generation resources were not recorded")
			}
			if err := awaitLifecycle(t, currentServerDone, "exact old QUIC generation close"); err != nil {
				t.Fatalf("generation %d old server connection: %v", generation, err)
			}
			oldWait := make(chan struct{})
			go func(old *UDPHandler) {
				old.wait()
				close(oldWait)
			}(currentHandler)
			awaitUDPHandler(t, oldWait, "exact old UDP handler retirement")
			if currentSC.Connection() != nil || currentSC.State() != StateDisconnected {
				t.Fatalf("generation %d retained old ServerConnection", generation)
			}
			assertNoUDPSessions(t, currentHandler)
			if _, err := currentHandler.fragmentAssembler.AddFragment(1, 1, 0, 2, []byte("closed")); !errors.Is(err, protocol.ErrFragmentAssemblerClosed) {
				t.Fatalf("generation %d old assembler error = %v", generation, err)
			}
			_ = currentSession.localConn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := currentSession.localConn.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("generation %d old socket error = %v", generation, err)
			}
		}

		if _, err := handler.fragmentAssembler.AddFragment(uint32(generation+1), 1, 0, 2, []byte("pending")); err != nil {
			t.Fatalf("generation %d pending fragment: %v", generation, err)
		}
		session, err := handler.getOrCreateSession(uint32(generation+1), sc.Connection())
		if err != nil {
			t.Fatalf("generation %d active UDP session: %v", generation, err)
		}
		currentSC, currentHandler, currentSession, currentServerDone = sc, handler, session, serverDone
	}

	if currentSC == nil || currentHandler == nil || currentSession == nil || currentServerDone == nil {
		t.Fatal("final generation resources were not recorded")
	}
	cm.startReconnection(runCtx, currentSC.ServerAddr(), currentSC)
	awaitRetirementCondition(t, "no-successor connection retirement", func() bool {
		return cm.GetConnection(currentSC.ServerAddr()) == nil && currentSC.Connection() == nil
	})
	if err := awaitLifecycle(t, currentServerDone, "no-successor server connection close"); err != nil {
		t.Fatal(err)
	}
	lastWait := make(chan struct{})
	go func() {
		currentHandler.wait()
		close(lastWait)
	}()
	awaitUDPHandler(t, lastWait, "no-successor UDP handler retirement")
	assertNoUDPSessions(t, currentHandler)
	if _, err := currentHandler.fragmentAssembler.AddFragment(1, 1, 0, 2, []byte("closed")); !errors.Is(err, protocol.ErrFragmentAssemblerClosed) {
		t.Fatalf("no-successor assembler error = %v", err)
	}
	_ = currentSession.localConn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := currentSession.localConn.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("no-successor socket error = %v", err)
	}

	cancelRun()
	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
	client.producerWG.Wait()
}

func TestServerConnectionCloseCannotBeReanimatedByHeartbeatCompletion(t *testing.T) {
	connection := newDisconnectedRetirementConnection("127.0.0.1:8443")
	gate := &firstErrGateContext{
		Context: connection.ctx,
		checked: make(chan struct{}),
		release: make(chan struct{}),
	}
	connection.ctx = gate
	marked := make(chan struct{})
	go func() {
		connection.MarkHealthy()
		close(marked)
	}()

	awaitUDPHandler(t, gate.checked, "MarkHealthy initial context check")
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	awaitUDPHandler(t, marked, "MarkHealthy cancellation recheck")

	if connection.Connection() != nil {
		t.Fatal("Close retained the QUIC connection")
	}
	if connection.IsHealthy() || connection.State() != StateDisconnected {
		t.Fatalf("Close was reanimated: state=%s healthy=%t", connection.State(), connection.IsHealthy())
	}
}

func TestServerConnectionCloseRacesConnectionAndAcceptStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientConn, _ := newUDPHandlerQUICPair(t)
	connection := newDisconnectedRetirementConnection("127.0.0.1:8443")
	connection.conn.Store(clientConn)
	connection.MarkHealthy()

	start := make(chan struct{})
	acceptDone := make(chan error, 32)
	for range cap(acceptDone) {
		go func() {
			<-start
			_, err := connection.AcceptStream(ctx)
			acceptDone <- err
		}()
	}
	closeDone := make(chan error, 1)
	go func() {
		<-start
		closeDone <- connection.Close()
	}()
	close(start)

	if err := <-closeDone; err != nil {
		t.Fatalf("Close error = %v", err)
	}
	for range cap(acceptDone) {
		if err := <-acceptDone; err == nil {
			t.Fatal("AcceptStream succeeded without a peer stream")
		}
	}
	if connection.Connection() != nil || connection.State() != StateDisconnected {
		t.Fatalf("connection survived Close: conn=%p state=%s", connection.Connection(), connection.State())
	}
}
