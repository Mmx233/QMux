package client

import (
	"bytes"
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
	cm, err := NewConnectionManager(completeConnectionManagerTestConfig(t, &config.Client{
		ClientID:          "retirement-client",
		HeartbeatInterval: time.Hour,
		HealthTimeout:     2 * time.Hour,
		Server: config.ClientServer{Servers: []config.ServerEndpoint{{
			Address:    address,
			ServerName: "lifecycle.test",
		}}},
	}), zerolog.Nop())
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

func TestConnectionPublicationRetiresExactPrevious(t *testing.T) {
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
	stale.reconnectStable.Store(true)
	cm.connections.Store(address, fresh)
	cm.reconnectMu.Lock()
	cm.endpoints[0].nextReconnectStage = maxReconnectStage
	cm.reconnectMu.Unlock()
	before := cm.endpointSnapshot()[0]

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
	cm.reconnectMu.Lock()
	stage := cm.endpoints[0].nextReconnectStage
	attempts := cm.endpoints[0].reconnectAttempts.Load()
	cm.reconnectMu.Unlock()
	if stage != maxReconnectStage || attempts != 0 {
		t.Fatalf("stale stable generation changed retry state to stage=%d attempts=%d", stage, attempts)
	}
	after := cm.endpointSnapshot()[0]
	if after.Handshaking != before.Handshaking || after.Pending != before.Pending ||
		after.Registered != before.Registered || after.Retiring != before.Retiring ||
		after.GenerationHighWater != before.GenerationHighWater || after.AccountingFaults != before.AccountingFaults {
		t.Fatalf("stale workers changed endpoint accounting from %+v to %+v", before, after)
	}

	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestStableExactGenerationResetsOnlyOwnEndpointStage(t *testing.T) {
	const first, second = "127.0.0.1:8443", "127.0.0.1:9443"
	cm := newCapacitySnapshotManager(t, first, second)
	stable := newCapacitySnapshotConnection(first)
	if !cm.publishServerConnection(context.Background(), stable) {
		t.Fatal("publish stable generation")
	}
	if got := awaitLifecycle(t, cm.NewConns, "stable generation delivery"); got != stable {
		t.Fatalf("delivered generation = %p, want %p", got, stable)
	}
	stable.reconnectStable.Store(true)
	stable.MarkUnhealthy()
	cm.reconnectMu.Lock()
	cm.endpoints[0].nextReconnectStage = maxReconnectStage
	cm.endpoints[1].nextReconnectStage = 3
	cm.reconnectMu.Unlock()

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	cm.startReconnection(runCtx, first, stable)
	awaitRetirementCondition(t, "stable exact generation detach", func() bool {
		return cm.GetConnection(first) == nil
	})
	cm.publishMu.Lock()
	cm.reconnectMu.Lock()
	firstStage := cm.endpoints[0].nextReconnectStage
	secondStage := cm.endpoints[1].nextReconnectStage
	firstAttempts := cm.endpoints[0].reconnectAttempts.Load()
	secondAttempts := cm.endpoints[1].reconnectAttempts.Load()
	slotHeld := cm.reconnecting[first]
	cm.reconnectMu.Unlock()
	cm.publishMu.Unlock()
	if firstStage != 0 || secondStage != 3 {
		t.Fatalf("stable reset left endpoint stages %d/%d, want 0/3", firstStage, secondStage)
	}
	if firstAttempts != 0 || secondAttempts != 0 || !slotHeld {
		t.Fatalf("stable reset dispatched retry or lost slot: attempts=%d/%d slot=%t", firstAttempts, secondAttempts, slotHeld)
	}

	cancelRun()
	cm.wg.Wait()
	cm.reconnectMu.Lock()
	firstStage = cm.endpoints[0].nextReconnectStage
	secondStage = cm.endpoints[1].nextReconnectStage
	firstAttempts = cm.endpoints[0].reconnectAttempts.Load()
	secondAttempts = cm.endpoints[1].reconnectAttempts.Load()
	_, slotHeld = cm.reconnecting[first]
	cm.reconnectMu.Unlock()
	if firstStage != 0 || secondStage != 3 || firstAttempts != 0 || secondAttempts != 0 || slotHeld {
		t.Fatalf("canceled reset worker left stages=%d/%d attempts=%d/%d slot=%t", firstStage, secondStage, firstAttempts, secondAttempts, slotHeld)
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
	cm.reconnectMu.Lock()
	firstStage := cm.endpoints[0].nextReconnectStage
	firstAttempts := cm.endpoints[0].reconnectAttempts.Load()
	cm.reconnectMu.Unlock()
	if firstStage != 1 || firstAttempts != 1 {
		t.Fatalf("stage-0 retry registration left stage=%d attempts=%d, want 1/1", firstStage, firstAttempts)
	}

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
	cm.reconnectMu.Lock()
	secondStage := cm.endpoints[0].nextReconnectStage
	secondAttempts := cm.endpoints[0].reconnectAttempts.Load()
	cm.reconnectMu.Unlock()
	if secondStage != 1 || secondAttempts != 1 {
		t.Fatalf("unstable successor changed stage/attempts to %d/%d, want 1/1", secondStage, secondAttempts)
	}

	cancelRun()
	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
	cm.reconnectMu.Lock()
	finalStage := cm.endpoints[0].nextReconnectStage
	finalAttempts := cm.endpoints[0].reconnectAttempts.Load()
	cm.reconnectMu.Unlock()
	if finalStage != 1 || finalAttempts != 1 {
		t.Fatalf("canceled successor changed stage/attempts to %d/%d, want 1/1", finalStage, finalAttempts)
	}
	if err := awaitLifecycle(t, freshServerDone, "fresh callback generation retirement"); err != nil {
		t.Fatal(err)
	}
}

func TestClientRetiresSuccessiveExactGenerationsAndNoSuccessor(t *testing.T) {
	if runClientUDPSetupSubprocess(t) {
		return
	}

	t.Run("collecting replacement", func(t *testing.T) {
		gate := installClientUDPResolverGate(t)
		gate.stall.Store(true)
		peer := newLifecyclePeer(t)
		cm := newLifecycleManager(t, peer)
		cm.config.HeartbeatInterval = time.Hour
		cm.config.HealthTimeout = 2 * time.Hour
		backend := newClientUDPBackend(t)
		cm.config.Local = config.LocalService{
			Host: "udp-retirement.qmux.invalid",
			Port: backend.LocalAddr().(*net.UDPAddr).Port,
		}

		forceCtx, cancelForce := context.WithCancel(context.Background())
		runCtx, cancelRun := context.WithCancel(context.Background())
		budget := newUDPSessionBudget(1)
		client := &Client{
			config:          cm.config,
			connMgr:         cm,
			udpBudget:       budget,
			dsendStats:      &clientDsendStats{},
			liveUDPHandlers: make(map[*UDPHandler]struct{}),
			forceCtx:        forceCtx,
			forceCancel:     cancelForce,
			runtimes:        make(map[*ServerConnection]*connectionRuntime),
			logger:          zerolog.Nop(),
		}
		client.producerWG.Go(client.handleNewConnections)
		t.Cleanup(func() {
			gate.unblock()
			cancelRun()
			_ = cm.Stop()
			cancelForce()
			client.producerWG.Wait()
			for _, runtime := range client.runtimeSnapshot() {
				_ = client.cleanupRuntime(runtime)
			}
			client.watcherWG.Wait()
		})

		connectGeneration := func(label string) (*ServerConnection, *quic.Conn, <-chan error) {
			t.Helper()
			remote := make(chan *quic.Conn, 1)
			serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
				if err := writeSuccessfulLifecycleAck(stream); err != nil {
					return err
				}
				remote <- conn
				<-conn.Context().Done()
				return nil
			})
			sc, err := cm.connectAndRegister(runCtx, peer.endpoint())
			if err != nil {
				t.Fatalf("connect %s generation: %v", label, err)
			}
			publishCtx, cancelPublish := context.WithTimeout(runCtx, 10*time.Second)
			published := cm.publishServerConnection(publishCtx, sc)
			cancelPublish()
			if !published {
				_ = sc.Close()
				t.Fatalf("publish %s generation", label)
			}
			return sc, awaitLifecycle(t, remote, label+" remote QUIC connection"), serverDone
		}
		lookupRuntime := func(sc *ServerConnection, description string) *connectionRuntime {
			t.Helper()
			var runtime *connectionRuntime
			awaitRetirementCondition(t, description, func() bool {
				client.runtimesMu.Lock()
				runtime = client.runtimes[sc]
				client.runtimesMu.Unlock()
				return runtime != nil && runtime.udp != nil
			})
			return runtime
		}

		const sessionID = uint32(70)
		oldSC, oldRemote, oldServerDone := connectGeneration("old")
		oldRuntime := lookupRuntime(oldSC, "old generation runtime")
		oldHandler := oldRuntime.udp
		sendClientUDPDatagram(t, oldRemote, sessionID, []byte("retire-while-collecting"))
		awaitUDPHandler(t, gate.entered, "old generation DNS gate")
		oldState, oldPending := awaitUDPState(t, oldHandler, sessionID, udpSessionPhaseCollecting)
		if oldPending.packetCount != 1 || oldPending.candidate != nil || oldHandler.epochAllocator.Load() != 0 {
			t.Fatalf("old collecting state = %+v/epoch %d", oldPending, oldHandler.epochAllocator.Load())
		}
		if _, err := oldHandler.fragmentAssembler.AddFragment(sessionID, 1, 0, 2, []byte("old-fragment")); err != nil {
			t.Fatal(err)
		}
		if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 1 || oldHandler.dsendStats.load().Workers != 0 {
			t.Fatalf("old collecting accounting = budget %+v/workers %d", snapshot, oldHandler.dsendStats.load().Workers)
		}

		freshSC, freshRemote, freshServerDone := connectGeneration("fresh")
		freshRuntime := lookupRuntime(freshSC, "fresh generation runtime")
		freshHandler := freshRuntime.udp
		awaitUDPHandler(t, oldRuntime.cleanupDone, "old collecting runtime cleanup")
		if err := awaitLifecycle(t, oldServerDone, "old collecting server close"); err != nil {
			t.Fatal(err)
		}
		if oldSC.Connection() != nil || oldSC.State() != StateDisconnected || cm.GetConnection(oldSC.ServerAddr()) != freshSC {
			t.Fatal("replacement did not retire only the old ServerConnection")
		}
		if oldHandler.loadSessionState(sessionID) != nil || snapshotUDPState(oldState).phase != udpSessionPhaseClosed {
			t.Fatal("old collecting state survived generation retirement")
		}
		assertNoUDPSessions(t, oldHandler)
		if _, err := oldHandler.fragmentAssembler.AddFragment(1, 1, 0, 2, []byte("closed")); !errors.Is(err, protocol.ErrFragmentAssemblerClosed) {
			t.Fatalf("old collecting assembler error = %v", err)
		}
		if current, ok := client.udpHandlers.Load(oldSC.ServerAddr()); !ok || current != freshHandler {
			t.Fatalf("old cleanup changed fresh handler mapping = (%p, %v), want (%p, true)", current, ok, freshHandler)
		}
		if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 0 || snapshot.AccountingFaults != 0 {
			t.Fatalf("old collecting cleanup = %+v", snapshot)
		}

		gate.unblock()
		sendClientUDPDatagram(t, freshRemote, sessionID, []byte("fresh-generation"))
		if payload, _ := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(payload, []byte("fresh-generation")) {
			t.Fatalf("fresh generation payload = %q", payload)
		}
		_, freshReady := awaitUDPState(t, freshHandler, sessionID, udpSessionPhaseReady)
		awaitClientUDPCondition(t, "fresh generation response worker", func() bool {
			return freshHandler.dsendStats.load().Workers == 1
		})
		if freshReady.session == nil || freshReady.session.epoch != 1 || oldHandler.epochAllocator.Load() != 0 {
			t.Fatalf("fresh ready state = %+v; old epoch = %d", freshReady, oldHandler.epochAllocator.Load())
		}

		if err := client.cleanupRuntime(freshRuntime); err != nil {
			t.Fatal(err)
		}
		if err := awaitLifecycle(t, freshServerDone, "fresh generation server close"); err != nil {
			t.Fatal(err)
		}
		assertNoUDPSessions(t, freshHandler)
		if _, err := freshReady.session.localConn.Write([]byte("closed")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("fresh socket after cleanup = %v", err)
		}
		if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 0 || snapshot.AccountingFaults != 0 || freshHandler.dsendStats.load().Workers != 0 {
			t.Fatalf("replacement cleanup = budget %+v/workers %d", snapshot, freshHandler.dsendStats.load().Workers)
		}
	})

	t.Run("draining no successor", func(t *testing.T) {
		peer := newLifecyclePeer(t)
		cm := newLifecycleManager(t, peer)
		cm.config.HeartbeatInterval = time.Hour
		cm.config.HealthTimeout = 2 * time.Hour
		backend := newClientUDPBackend(t)

		remote := make(chan *quic.Conn, 1)
		serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
			if err := writeSuccessfulLifecycleAck(stream); err != nil {
				return err
			}
			remote <- conn
			<-conn.Context().Done()
			return nil
		})
		sc, err := cm.connectAndRegister(context.Background(), peer.endpoint())
		if err != nil {
			t.Fatal(err)
		}
		if !cm.publishServerConnection(context.Background(), sc) {
			_ = sc.Close()
			t.Fatal("publish draining generation")
		}
		oldRemote := awaitLifecycle(t, remote, "draining remote QUIC connection")

		forceCtx, cancelForce := context.WithCancel(context.Background())
		budget := newUDPSessionBudget(1)
		stats := &clientDsendStats{}
		client := &Client{
			config:          cm.config,
			connMgr:         cm,
			udpBudget:       budget,
			dsendStats:      stats,
			liveUDPHandlers: make(map[*UDPHandler]struct{}),
			forceCtx:        forceCtx,
			forceCancel:     cancelForce,
			runtimes:        make(map[*ServerConnection]*connectionRuntime),
			logger:          zerolog.Nop(),
		}
		oldHandler := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true,
			config.DefaultMaxUDPFragmentGroupsPerHandler,
			config.DefaultMaxUDPFragmentBackingBytesPerHandler,
			zerolog.Nop(), budget, stats)
		publishEntered := make(chan struct{})
		publishRelease := make(chan struct{})
		var publishOnce sync.Once
		releasePublish := func() { publishOnce.Do(func() { close(publishRelease) }) }
		t.Cleanup(releasePublish)
		const sessionID = uint32(71)
		oldHandler.beforeSessionPublish = func() {
			oldHandler.lifecycleMu.Lock()
			oldHandler.lifecycleMu.Unlock()
			oldHandler.sessionsMu.Lock()
			state := oldHandler.sessions[sessionID]
			oldHandler.sessionsMu.Unlock()
			if state == nil {
				panic("draining state disappeared before publish hook")
			}
			state.mu.Lock()
			phase := state.phase
			state.mu.Unlock()
			if phase != udpSessionPhaseDraining {
				panic("publish hook did not observe draining state")
			}
			close(publishEntered)
			<-publishRelease
		}
		runtimeForceCtx, cancelRuntimeForce := context.WithCancel(forceCtx)
		acceptCtx, cancelAccept := context.WithCancel(runtimeForceCtx)
		runtime := &connectionRuntime{
			sc: sc, conn: sc.Connection(), forceCtx: runtimeForceCtx, cancelForce: cancelRuntimeForce,
			acceptCtx: acceptCtx, cancelAccept: cancelAccept,
			acceptDone: make(chan struct{}), acceptErr: make(chan error, 1),
			acceptedHigh: -1, cleanupDone: make(chan struct{}), udp: oldHandler,
		}
		oldHandler.Start(runtimeForceCtx, sc.Connection())
		client.udpMu.Lock()
		client.liveUDPHandlers[oldHandler] = struct{}{}
		client.udpHandlers.Store(sc.ServerAddr(), oldHandler)
		client.udpMu.Unlock()
		client.runtimesMu.Lock()
		client.runtimes[sc] = runtime
		client.runtimesMu.Unlock()
		t.Cleanup(func() {
			releasePublish()
			_ = client.cleanupRuntime(runtime)
			cancelForce()
			_ = cm.Stop()
		})

		if _, err := oldHandler.fragmentAssembler.AddFragment(sessionID, 1, 0, 2, []byte("old-fragment")); err != nil {
			t.Fatal(err)
		}
		sendClientUDPDatagram(t, oldRemote, sessionID, []byte("old-draining"))
		awaitUDPHandler(t, publishEntered, "old draining publish gate")
		if payload, _ := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(payload, []byte("old-draining")) {
			t.Fatalf("old draining payload = %q", payload)
		}
		oldState, oldPending := awaitUDPState(t, oldHandler, sessionID, udpSessionPhaseDraining)
		if oldPending.candidate == nil || oldPending.session != nil || budget.snapshot().Permits != 1 || stats.load().Workers != 0 {
			t.Fatalf("old draining state = %+v/budget %+v/workers %d", oldPending, budget.snapshot(), stats.load().Workers)
		}

		successorClientConn, successorRemote := newUDPHandlerQUICPair(t)
		successor := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true,
			config.DefaultMaxUDPFragmentGroupsPerHandler,
			config.DefaultMaxUDPFragmentBackingBytesPerHandler,
			zerolog.Nop(), budget, stats)
		successor.Start(forceCtx, successorClientConn)
		client.udpMu.Lock()
		client.liveUDPHandlers[successor] = struct{}{}
		client.udpHandlers.Store(sc.ServerAddr(), successor)
		client.udpMu.Unlock()
		t.Cleanup(func() {
			_ = successorClientConn.CloseWithError(0, "test cleanup")
			successor.stopAndWait()
			client.retireUDPHandler(sc.ServerAddr(), successor)
		})

		cleanupResult := make(chan error, 1)
		go func() { cleanupResult <- client.cleanupRuntime(runtime) }()
		awaitRetirementCondition(t, "closed draining state", func() bool {
			return oldHandler.loadSessionState(sessionID) == nil && snapshotUDPState(oldState).phase == udpSessionPhaseClosed && sc.Connection() == nil
		})
		if cm.GetConnection(sc.ServerAddr()) != nil {
			t.Fatal("no-successor cleanup retained the ServerConnection")
		}
		if _, err := oldPending.candidate.Write([]byte("closed")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("old draining socket after close = %v", err)
		}
		select {
		case err := <-cleanupResult:
			t.Fatalf("runtime cleanup returned before draining worker exit: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 1 || snapshot.AccountingFaults != 0 {
			t.Fatalf("closed draining ownership = %+v, want worker-held permit", snapshot)
		}

		sendClientUDPDatagram(t, successorRemote, sessionID, []byte("capacity-drop"))
		awaitClientUDPCondition(t, "successor capacity drop", func() bool {
			return budget.snapshot().CapacityDrops == 1
		})
		if successor.loadSessionState(sessionID) != nil || successor.epochAllocator.Load() != 0 {
			t.Fatal("capacity-dropped successor started session setup")
		}

		releasePublish()
		if err := awaitLifecycle(t, cleanupResult, "draining runtime worker exit"); err != nil {
			t.Fatal(err)
		}
		if err := awaitLifecycle(t, serverDone, "draining generation server close"); err != nil {
			t.Fatal(err)
		}
		assertNoUDPSessions(t, oldHandler)
		if _, err := oldHandler.fragmentAssembler.AddFragment(1, 1, 0, 2, []byte("closed")); !errors.Is(err, protocol.ErrFragmentAssemblerClosed) {
			t.Fatalf("old draining assembler error = %v", err)
		}
		if current, ok := client.udpHandlers.Load(sc.ServerAddr()); !ok || current != successor {
			t.Fatalf("old exact cleanup changed successor mapping = (%p, %v), want (%p, true)", current, ok, successor)
		}
		if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 0 || snapshot.CapacityDrops != 1 || snapshot.AccountingFaults != 0 {
			t.Fatalf("old draining worker cleanup = %+v", snapshot)
		}

		sendClientUDPDatagram(t, successorRemote, sessionID, []byte("successor-ready"))
		if payload, _ := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(payload, []byte("successor-ready")) {
			t.Fatalf("successor payload after permit release = %q", payload)
		}
		_, successorReady := awaitUDPState(t, successor, sessionID, udpSessionPhaseReady)
		awaitClientUDPCondition(t, "successor response worker", func() bool {
			return stats.load().Workers == 1
		})
		if successorReady.session == nil || oldHandler.epochAllocator.Load() != 1 || successor.epochAllocator.Load() != 1 {
			t.Fatalf("isolated handler epochs = old %d/successor %d", oldHandler.epochAllocator.Load(), successor.epochAllocator.Load())
		}
		oldHandler.closeStateExact(oldState)
		if current, ok := client.udpHandlers.Load(sc.ServerAddr()); !ok || current != successor {
			t.Fatal("stale old state close removed the successor handler")
		}
		if _, err := successor.fragmentAssembler.AddFragment(sessionID, 2, 0, 2, []byte("successor-fragment")); err != nil {
			t.Fatal(err)
		}

		_ = successorClientConn.CloseWithError(0, "successor cleanup")
		successor.stopAndWait()
		client.retireUDPHandler(sc.ServerAddr(), successor)
		if _, err := successorReady.session.localConn.Write([]byte("closed")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("successor socket after cleanup = %v", err)
		}
		if _, ok := client.udpHandlers.Load(sc.ServerAddr()); ok {
			t.Fatal("successor handler mapping survived cleanup")
		}
		if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 0 || snapshot.AccountingFaults != 0 || stats.load().Workers != 0 {
			t.Fatalf("no-successor final cleanup = budget %+v/workers %d", snapshot, stats.load().Workers)
		}
		cancelForce()
		if err := cm.Stop(); err != nil {
			t.Fatal(err)
		}
	})
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
