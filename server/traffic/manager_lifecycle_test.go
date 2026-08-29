package traffic

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/rs/zerolog"
	"go.uber.org/goleak"
)

func testPool(t *testing.T, quicAddr string) *pool.ConnectionPool {
	t.Helper()
	p := pool.New(quicAddr, pool.NewRoundRobinBalancer(), zerolog.Nop())
	t.Cleanup(p.Stop)
	return p
}

func waitManager(t *testing.T, manager *Manager) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		manager.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for traffic manager shutdown")
	}
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate TCP address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release TCP address: %v", err)
	}
	return addr
}

func bindTestTCPAndUDP(t *testing.T, addr string) (net.Listener, *net.UDPConn) {
	t.Helper()
	tcpListener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("bind test TCP address %s: %v", addr, err)
	}
	tcpAddr := tcpListener.Addr().(*net.TCPAddr)
	udpListener, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   tcpAddr.IP,
		Port: tcpAddr.Port,
		Zone: tcpAddr.Zone,
	})
	if err != nil {
		if closeErr := tcpListener.Close(); closeErr != nil {
			t.Fatalf("bind test UDP address %s: %v; close TCP reservation: %v", tcpAddr, err, closeErr)
		}
		t.Fatalf("bind test UDP address %s: %v", tcpAddr, err)
	}
	return tcpListener, udpListener
}

func reserveTestTCPAndUDP(t *testing.T) (net.Listener, *net.UDPConn) {
	t.Helper()
	const maxAttempts = 20
	var lastUDPErr error
	for attempt := range maxAttempts {
		tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("attempt %d allocate test TCP address: %v", attempt, err)
		}
		tcpAddr := tcpListener.Addr().(*net.TCPAddr)
		udpListener, err := net.ListenUDP("udp", &net.UDPAddr{
			IP:   tcpAddr.IP,
			Port: tcpAddr.Port,
			Zone: tcpAddr.Zone,
		})
		if err == nil {
			return tcpListener, udpListener
		}
		lastUDPErr = err
		if closeErr := tcpListener.Close(); closeErr != nil {
			t.Fatalf("attempt %d UDP collision: %v; close TCP reservation: %v", attempt, err, closeErr)
		}
	}
	t.Fatalf("could not reserve one TCP+UDP test port after %d attempts: %v", maxAttempts, lastUDPErr)
	return nil, nil
}

func closeTestTCPAndUDP(t *testing.T, tcpListener net.Listener, udpListener *net.UDPConn) {
	t.Helper()
	udpErr := udpListener.Close()
	tcpErr := tcpListener.Close()
	if udpErr != nil || tcpErr != nil {
		t.Fatalf("close test TCP+UDP sockets: TCP=%v, UDP=%v", tcpErr, udpErr)
	}
}

func TestManagerEmptyLifecycleAndStableStartErrors(t *testing.T) {
	manager := NewManager(nil, nil, zerolog.Nop())
	if manager.Running() {
		t.Fatal("new manager reported running")
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("start empty manager: %v", err)
	}
	if !manager.Running() {
		t.Fatal("committed manager did not report running")
	}
	if err := manager.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start error = %v, want ErrAlreadyStarted", err)
	}

	manager.Stop()
	if manager.Running() {
		t.Fatal("stopped manager reported running")
	}
	manager.Close()
	waitManager(t, manager)
	if err := manager.Start(context.Background()); !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("Start after Stop error = %v, want ErrManagerStopped", err)
	}
}

func TestNewManagerSnapshotsListeners(t *testing.T) {
	fragmentation := true
	conf := &config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    "quic-original",
		TrafficAddr: "127.0.0.1:8080",
		Protocol:    "tcp",
		UDP: config.UDPConfig{
			EnableFragmentation: &fragmentation,
		},
	}}}
	manager := NewManager(conf, nil, zerolog.Nop())
	conf.Listeners[0].QuicAddr = "quic-mutated"
	fragmentation = false
	if manager.configs[0].QuicAddr != "quic-original" ||
		!*manager.configs[0].UDP.EnableFragmentation {
		t.Fatal("manager retained caller-owned listener slice")
	}
}

func TestManagerCloseBeforeStartIsTerminal(t *testing.T) {
	manager := NewManager(nil, nil, zerolog.Nop())
	manager.Close()
	waitManager(t, manager)
	if err := manager.Start(context.Background()); !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("Start after Close error = %v, want ErrManagerStopped", err)
	}
}

func TestManagerContextCancellationInitiatesShutdown(t *testing.T) {
	quicAddr := "quic-context"
	trafficAddr := freeTCPAddr(t)
	conf := &config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    quicAddr,
		TrafficAddr: trafficAddr,
		Protocol:    "tcp",
	}}}
	manager := NewManager(conf, map[string]*pool.ConnectionPool{
		quicAddr: testPool(t, quicAddr),
	}, zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	if manager.Running() {
		t.Fatal("manager with canceled run context reported running")
	}
	waitManager(t, manager)

	listener, err := net.Listen("tcp", trafficAddr)
	if err != nil {
		t.Fatalf("traffic port remained bound after context cancellation: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close rebound traffic port: %v", err)
	}
}

func TestManagerBothContextCancellationAllowsRepeatedExactRebind(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	reservationTCP, reservationUDP := reserveTestTCPAndUDP(t)
	trafficAddr := reservationTCP.Addr().String()
	closeTestTCPAndUDP(t, reservationTCP, reservationUDP)

	const cycles = 10
	for cycle := range cycles {
		func() {
			quicAddr := "quic-both-rebind"
			connectionPool := pool.New(quicAddr, pool.NewRoundRobinBalancer(), zerolog.Nop())
			defer connectionPool.Stop()
			manager := NewManager(&config.Server{Listeners: []config.QuicListener{{
				QuicAddr:    quicAddr,
				TrafficAddr: trafficAddr,
				Protocol:    "both",
			}}}, map[string]*pool.ConnectionPool{quicAddr: connectionPool}, zerolog.Nop())

			ctx, cancel := context.WithCancel(context.Background())
			if err := manager.Start(ctx); err != nil {
				cancel()
				t.Fatalf("cycle %d Start: %v", cycle, err)
			}
			cancel()
			waitManager(t, manager)
		}()

		reboundTCP, reboundUDP := bindTestTCPAndUDP(t, trafficAddr)
		if got := reboundTCP.Addr().String(); got != trafficAddr {
			closeTestTCPAndUDP(t, reboundTCP, reboundUDP)
			t.Fatalf("cycle %d rebound address = %s, want %s", cycle, got, trafficAddr)
		}
		closeTestTCPAndUDP(t, reboundTCP, reboundUDP)
	}
}

func TestManagerCancelWhileStartingRollsBackStagedSockets(t *testing.T) {
	reservationTCP, reservationUDP := reserveTestTCPAndUDP(t)
	trafficAddr := reservationTCP.Addr().String()
	closeTestTCPAndUDP(t, reservationTCP, reservationUDP)

	quicAddr := "quic-starting-rollback"
	manager := NewManager(&config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    quicAddr,
		TrafficAddr: trafficAddr,
		Protocol:    "both",
	}}}, map[string]*pool.ConnectionPool{
		quicAddr: testPool(t, quicAddr),
	}, zerolog.Nop())
	staged := make(chan struct{})
	releaseCommit := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCommit) }) }
	defer release()
	manager.beforeCommit = func() {
		close(staged)
		<-releaseCommit
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDone := make(chan error, 1)
	go func() { startDone <- manager.Start(ctx) }()
	select {
	case <-staged:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not reach the staged pre-commit state")
	}

	cancel()
	closeDone := make(chan struct{})
	go func() {
		manager.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close blocked while Start was staged before commit")
	}
	manager.mu.Lock()
	stateWhileStaged := manager.state
	manager.mu.Unlock()
	if stateWhileStaged != managerClosing {
		t.Fatalf("manager state while staged = %v, want managerClosing", stateWhileStaged)
	}
	if manager.Running() {
		t.Fatal("manager reported running while staged shutdown was in progress")
	}
	release()

	select {
	case err := <-startDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not finish staged rollback")
	}
	waitManager(t, manager)
	manager.mu.Lock()
	terminalState := manager.state
	manager.mu.Unlock()
	if terminalState != managerStopped {
		t.Fatalf("terminal manager state = %v, want managerStopped", terminalState)
	}
	if err := manager.Start(context.Background()); !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("Start after staged rollback error = %v, want ErrManagerStopped", err)
	}

	reboundTCP, reboundUDP := bindTestTCPAndUDP(t, trafficAddr)
	closeTestTCPAndUDP(t, reboundTCP, reboundUDP)
}

func TestManagerWaitJoinsCommittedListenerHandlers(t *testing.T) {
	quicAddr := "quic-handler-latch"
	manager := NewManager(&config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    quicAddr,
		TrafficAddr: "127.0.0.1:0",
		Protocol:    "tcp",
	}}}, map[string]*pool.ConnectionPool{
		quicAddr: testPool(t, quicAddr),
	}, zerolog.Nop())
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(manager.listeners) != 1 {
		manager.Stop()
		t.Fatalf("committed listeners = %d, want 1", len(manager.listeners))
	}
	listener := manager.listeners[0]

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseHandler) }) }
	defer func() {
		release()
		manager.Stop()
	}()

	listener.handlerWG.Go(func() {
		close(handlerStarted)
		<-releaseHandler
	})
	<-handlerStarted

	manager.Close()
	if manager.Running() {
		t.Fatal("closing manager reported running")
	}
	waitAttempted := make(chan struct{})
	waitDone := make(chan struct{})
	go func() {
		close(waitAttempted)
		manager.Wait()
		close(waitDone)
	}()
	<-waitAttempted

	select {
	case <-waitDone:
		release()
		t.Fatal("Wait returned while a committed listener handler was blocked")
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-waitDone:
		release()
		t.Fatal("Wait returned while a committed listener handler was blocked")
	default:
	}

	release()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after the committed listener handler exited")
	}
}

func TestManagerMissingPoolFailsBeforeBinding(t *testing.T) {
	firstAddr := freeTCPAddr(t)
	conf := &config.Server{Listeners: []config.QuicListener{
		{QuicAddr: "present", TrafficAddr: firstAddr, Protocol: "tcp"},
		{QuicAddr: "missing", TrafficAddr: freeTCPAddr(t), Protocol: "tcp"},
	}}
	manager := NewManager(conf, map[string]*pool.ConnectionPool{
		"present": testPool(t, "present"),
	}, zerolog.Nop())

	err := manager.Start(context.Background())
	if !errors.Is(err, ErrMissingPool) {
		t.Fatalf("Start error = %v, want ErrMissingPool", err)
	}
	waitManager(t, manager)
	if err := manager.Start(context.Background()); !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("Start after failed Start error = %v, want ErrManagerStopped", err)
	}

	listener, err := net.Listen("tcp", firstAddr)
	if err != nil {
		t.Fatalf("validation bound an earlier traffic port: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close validation probe listener: %v", err)
	}
}

func TestManagerRollsBackTCPWhenUDPBindFails(t *testing.T) {
	reservationTCP, occupiedUDP := reserveTestTCPAndUDP(t)
	trafficAddr := reservationTCP.Addr().String()
	if err := reservationTCP.Close(); err != nil {
		if udpErr := occupiedUDP.Close(); udpErr != nil {
			t.Fatalf("release TCP reservation: %v; release UDP reservation: %v", err, udpErr)
		}
		t.Fatalf("release TCP reservation: %v", err)
	}
	defer func() {
		if err := occupiedUDP.Close(); err != nil {
			t.Errorf("release occupied UDP address: %v", err)
		}
	}()

	quicAddr := "quic-both"
	conf := &config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    quicAddr,
		TrafficAddr: trafficAddr,
		Protocol:    "both",
	}}}
	manager := NewManager(conf, map[string]*pool.ConnectionPool{
		quicAddr: testPool(t, quicAddr),
	}, zerolog.Nop())
	startErr := manager.Start(context.Background())
	if startErr == nil {
		t.Fatal("Start succeeded with occupied UDP address")
	}
	if want := "start UDP listener on " + trafficAddr + ":"; !strings.Contains(startErr.Error(), want) {
		t.Fatalf("Start error = %q, want UDP startup context containing %q", startErr, want)
	}
	waitManager(t, manager)

	listener, err := net.Listen("tcp", trafficAddr)
	if err != nil {
		t.Fatalf("TCP socket was not rolled back after UDP bind failure: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close rollback probe listener: %v", err)
	}
}

func TestManagerRollsBackPriorListener(t *testing.T) {
	firstAddr := freeTCPAddr(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy second TCP address: %v", err)
	}
	defer func() {
		if err := occupied.Close(); err != nil {
			t.Errorf("release occupied TCP address: %v", err)
		}
	}()

	conf := &config.Server{Listeners: []config.QuicListener{
		{QuicAddr: "first", TrafficAddr: firstAddr, Protocol: "tcp"},
		{QuicAddr: "second", TrafficAddr: occupied.Addr().String(), Protocol: "tcp"},
	}}
	manager := NewManager(conf, map[string]*pool.ConnectionPool{
		"first":  testPool(t, "first"),
		"second": testPool(t, "second"),
	}, zerolog.Nop())
	if err := manager.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded with occupied later TCP address")
	}
	waitManager(t, manager)

	listener, err := net.Listen("tcp", firstAddr)
	if err != nil {
		t.Fatalf("prior TCP listener was not rolled back: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close prior-listener rollback probe: %v", err)
	}
}

func TestManagerRejectsAmbiguousDatagramRoutes(t *testing.T) {
	quicAddr := "shared-quic"
	conf := &config.Server{Listeners: []config.QuicListener{
		{QuicAddr: quicAddr, TrafficAddr: "127.0.0.1:0", Protocol: "udp"},
		{QuicAddr: quicAddr, TrafficAddr: "127.0.0.1:0", Protocol: "both"},
	}}
	manager := NewManager(conf, map[string]*pool.ConnectionPool{
		quicAddr: testPool(t, quicAddr),
	}, zerolog.Nop())
	if err := manager.Start(context.Background()); !errors.Is(err, ErrDuplicateDatagramRoute) {
		t.Fatalf("Start error = %v, want ErrDuplicateDatagramRoute", err)
	}
	waitManager(t, manager)
}

func TestManagerUDPShutdownClosesHandlerAndAssembler(t *testing.T) {
	quicAddr := "quic-udp-shutdown"
	conf := &config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    quicAddr,
		TrafficAddr: "127.0.0.1:0",
		Protocol:    "udp",
	}}}
	manager := NewManager(conf, map[string]*pool.ConnectionPool{
		quicAddr: testPool(t, quicAddr),
	}, zerolog.Nop())
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(manager.listeners) != 1 || manager.listeners[0].udpHandler == nil {
		t.Fatal("UDP handler was not committed")
	}
	handler := manager.listeners[0].udpHandler

	manager.Stop()
	handler.lifecycleMu.Lock()
	closed := handler.closed
	handler.lifecycleMu.Unlock()
	if !closed {
		t.Fatal("UDP handler gate remained open after Stop")
	}
	if _, err := handler.fragmentAssembler.AddFragment(1, 1, 0, 2, []byte("part")); !errors.Is(err, protocol.ErrFragmentAssemblerClosed) {
		t.Fatalf("assembler AddFragment after Stop error = %v, want ErrFragmentAssemblerClosed", err)
	}
}

func TestManagerConcurrentCloseWaitStop(t *testing.T) {
	manager := NewManager(nil, nil, zerolog.Nop())
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 48 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				manager.Close()
			case 1:
				manager.Wait()
			case 2:
				manager.Stop()
			}
		}(i)
	}
	wg.Wait()
}
