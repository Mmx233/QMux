package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/internal/testutil"
	"github.com/Mmx233/QMux/internal/tlsreload"
	"github.com/rs/zerolog"
)

const serverResolverChild = "QMUX_SERVER_RESOLVER_CHILD"

type lifecycleTraffic struct {
	mu       sync.Mutex
	events   []string
	startErr error
	start    func(context.Context) error
	onClose  func()
}

func (m *lifecycleTraffic) Start(ctx context.Context) error {
	m.record("traffic-start")
	if m.start != nil {
		return m.start(ctx)
	}
	return m.startErr
}

func (m *lifecycleTraffic) Close() {
	m.record("traffic-close")
	if m.onClose != nil {
		m.onClose()
	}
}

func (m *lifecycleTraffic) Wait() {
	m.record("traffic-wait")
}

func (m *lifecycleTraffic) record(event string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
}

func (m *lifecycleTraffic) snapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events)
}

func TestSuperviseServerCancellationJoinsQUICBeforeTrafficWait(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	manager := &lifecycleTraffic{}
	ready := make(chan struct{}, 2)
	listeners := []config.QuicListener{
		{QuicAddr: "listener-1"},
		{QuicAddr: "listener-2"},
	}
	startListener := func(ctx context.Context, listener config.QuicListener) error {
		manager.record(listener.QuicAddr + "-start")
		ready <- struct{}{}
		<-ctx.Done()
		manager.record(listener.QuicAddr + "-exit")
		return net.ErrClosed
	}

	result := make(chan error, 1)
	go func() {
		result <- superviseServer(ctx, manager, listeners, startListener, nil, false)
	}()
	waitForLifecycleSignals(t, ready, len(listeners))

	cancelCause := errors.New("caller requested shutdown")
	cancel(cancelCause)
	if err := waitForLifecycleResult(t, result); !errors.Is(err, cancelCause) {
		t.Fatalf("superviseServer() error = %v, want cancellation cause %v", err, cancelCause)
	}

	events := manager.snapshot()
	assertLifecycleEventBefore(t, events, "traffic-close", "listener-1-exit")
	assertLifecycleEventBefore(t, events, "traffic-close", "listener-2-exit")
	assertLifecycleEventBefore(t, events, "listener-1-exit", "traffic-wait")
	assertLifecycleEventBefore(t, events, "listener-2-exit", "traffic-wait")
}

func TestSuperviseServerPreservesListenerErrorAndFiltersCloseNoise(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	manager := &lifecycleTraffic{}
	manager.onClose = func() { cancel(errors.New("concurrent caller cancellation")) }
	ready := make(chan struct{}, 2)
	releaseFailure := make(chan struct{})
	listenerFailure := errors.New("listener failed")
	listeners := []config.QuicListener{
		{QuicAddr: "failing-listener"},
		{QuicAddr: "sibling-listener"},
	}
	startListener := func(ctx context.Context, listener config.QuicListener) error {
		ready <- struct{}{}
		if listener.QuicAddr == "failing-listener" {
			<-releaseFailure
			manager.record("failing-listener-exit")
			return listenerFailure
		}
		<-ctx.Done()
		manager.record("sibling-listener-exit")
		return net.ErrClosed
	}

	result := make(chan error, 1)
	go func() {
		result <- superviseServer(ctx, manager, listeners, startListener, nil, false)
	}()
	waitForLifecycleSignals(t, ready, len(listeners))
	close(releaseFailure)

	if err := waitForLifecycleResult(t, result); !errors.Is(err, listenerFailure) {
		t.Fatalf("superviseServer() error = %v, want listener failure %v", err, listenerFailure)
	}
	events := manager.snapshot()
	assertLifecycleEventBefore(t, events, "failing-listener-exit", "traffic-close")
	assertLifecycleEventBefore(t, events, "traffic-close", "sibling-listener-exit")
	assertLifecycleEventBefore(t, events, "sibling-listener-exit", "traffic-wait")
}

func TestSuperviseServerTrafficStartupFailureIsJoined(t *testing.T) {
	startupFailure := errors.New("traffic bind failed")
	manager := &lifecycleTraffic{startErr: startupFailure}
	listenerCalled := false
	err := superviseServer(
		context.Background(),
		manager,
		[]config.QuicListener{{QuicAddr: "unused"}},
		func(context.Context, config.QuicListener) error {
			listenerCalled = true
			return nil
		},
		nil,
		false,
	)
	if !errors.Is(err, startupFailure) {
		t.Fatalf("superviseServer() error = %v, want startup failure %v", err, startupFailure)
	}
	if listenerCalled {
		t.Fatal("QUIC listener started after traffic startup failed")
	}
	if events := manager.snapshot(); !slices.Equal(events, []string{"traffic-start", "traffic-close", "traffic-wait"}) {
		t.Fatalf("lifecycle events = %v, want traffic startup rollback and join", events)
	}
}

func TestSuperviseServerStopsWatcherAfterTrafficStartupFailure(t *testing.T) {
	reloader, _ := newServerLifecycleReloader(t)
	startupFailure := errors.New("traffic bind failed")
	manager := &lifecycleTraffic{startErr: startupFailure}
	err := superviseServer(
		context.Background(), manager, nil,
		func(context.Context, config.QuicListener) error { return nil },
		reloader, true,
	)
	if !errors.Is(err, startupFailure) {
		t.Fatalf("superviseServer() error = %v, want startup failure %v", err, startupFailure)
	}
	if waitErr := reloader.Wait(); waitErr != nil {
		t.Fatalf("watcher completion error = %v, want owned cancellation", waitErr)
	}
	if events := manager.snapshot(); !slices.Equal(events, []string{"traffic-start", "traffic-close", "traffic-wait"}) {
		t.Fatalf("lifecycle events = %v, want traffic startup rollback and join", events)
	}
}

func TestSuperviseServerWatcherFailureCancelsPendingTrafficStartup(t *testing.T) {
	reloader, parent := newServerLifecycleReloader(t)
	started := make(chan struct{})
	manager := &lifecycleTraffic{start: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	result := make(chan error, 1)
	go func() {
		result <- superviseServer(
			context.Background(), manager, nil,
			func(context.Context, config.QuicListener) error { return nil },
			reloader, true,
		)
	}()
	<-started
	assertServerLifecycleWatcherFailure(t, reloader, parent, result)
	if events := manager.snapshot(); !slices.Equal(events, []string{"traffic-start", "traffic-close", "traffic-wait"}) {
		t.Fatalf("lifecycle events = %v, want blocked startup rollback and join", events)
	}
}

func TestSuperviseServerWatcherFailurePreservesRuntimeShutdownOrder(t *testing.T) {
	reloader, parent := newServerLifecycleReloader(t)
	manager := &lifecycleTraffic{}
	listenerReady := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() {
		result <- superviseServer(
			context.Background(), manager, []config.QuicListener{{QuicAddr: "listener"}},
			func(ctx context.Context, _ config.QuicListener) error {
				listenerReady <- struct{}{}
				<-ctx.Done()
				manager.record("listener-exit")
				return ctx.Err()
			},
			reloader, true,
		)
	}()
	waitForLifecycleSignals(t, listenerReady, 1)
	assertServerLifecycleWatcherFailure(t, reloader, parent, result)
	events := manager.snapshot()
	assertLifecycleEventBefore(t, events, "traffic-close", "listener-exit")
	assertLifecycleEventBefore(t, events, "listener-exit", "traffic-wait")
}

func assertServerLifecycleWatcherFailure(
	t *testing.T,
	reloader *tlsreload.Reloader,
	parent string,
	result <-chan error,
) {
	t.Helper()
	removed := parent + "-removed"
	if err := os.Rename(parent, removed); err != nil {
		t.Fatalf("rename watched parent: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(removed) })

	err := waitForLifecycleResult(t, result)
	watcherErr := reloader.Wait()
	if watcherErr == nil || !errors.Is(err, watcherErr) {
		t.Fatalf("superviseServer() error = %v, want watcher cause %v", err, watcherErr)
	}
}

func newServerLifecycleReloader(t *testing.T) (*tlsreload.Reloader, string) {
	t.Helper()
	tlsFiles := snapshotServerTLSFiles(t)
	reloader, err := tlsreload.New("server", tlsreload.Paths{
		CertFile: tlsFiles.ServerCertFile,
		KeyFile:  tlsFiles.ServerKeyFile,
	}, zerolog.Nop(), func(*tlsreload.Bundle) error { return nil })
	if err != nil {
		t.Fatalf("create TLS reloader: %v", err)
	}
	if err := reloader.LoadInitial(); err != nil {
		t.Fatalf("load initial TLS material: %v", err)
	}
	t.Cleanup(reloader.Stop)
	return reloader, filepath.Dir(tlsFiles.ServerCertFile)
}

func TestSuperviseServerCancelsRealListenerHostnameResolve(t *testing.T) {
	if os.Getenv(serverResolverChild) != "" {
		runServerResolverChild(t)
		return
	}
	testutil.RunResolverCancellationProcess(t, serverResolverChild)
}

func TestStartListenerPreservesLiteralAndEmptyHost(t *testing.T) {
	srv := &Server{
		config: &config.Server{
			Auth: config.ServerAuth{Method: "token"},
		},
		logger: zerolog.Nop(),
	}
	for _, test := range testutil.UDPLiteralBindCases(t) {
		t.Run(test.Name, func(t *testing.T) {
			addr, err := net.ResolveUDPAddr("udp", test.Address)
			if err != nil {
				t.Fatal(err)
			}
			probe, err := net.ListenUDP("udp", addr)
			if err != nil {
				if test.Optional {
					t.Skipf("address family unavailable: %v", err)
				}
				t.Fatal(err)
			}
			if err := probe.Close(); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := srv.startListener(ctx, config.QuicListener{QuicAddr: test.Address}); err != nil {
				t.Fatalf("start canceled-context listener on %s: %v", test.Address, err)
			}
		})
	}
}

func runServerResolverChild(t *testing.T) {
	var entered sync.Once
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			entered.Do(func() { _, _ = fmt.Fprintln(os.Stdout, "resolver-entered") })
			<-ctx.Done()
			return nil, context.Cause(ctx)
		},
	}

	manager := &lifecycleTraffic{}
	srv := &Server{logger: zerolog.Nop()}
	listener := config.QuicListener{QuicAddr: "lif002-server.qmux.invalid:9"}
	ctx, cancel := context.WithCancelCause(context.Background())
	want := errors.New("caller requested resolver shutdown")
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel(want)
	}()
	err := superviseServer(ctx, manager, []config.QuicListener{listener}, func(ctx context.Context, listener config.QuicListener) error {
		manager.record("listener-start")
		err := srv.startListener(ctx, listener)
		manager.record("listener-exit")
		return err
	}, nil, false)
	if !errors.Is(err, want) {
		t.Fatalf("superviseServer error = %v, want caller cause %v", err, want)
	}
	events := manager.snapshot()
	assertLifecycleEventBefore(t, events, "traffic-close", "listener-exit")
	assertLifecycleEventBefore(t, events, "listener-exit", "traffic-wait")
}

func waitForLifecycleSignals(t *testing.T, signals <-chan struct{}, count int) {
	t.Helper()
	for range count {
		select {
		case <-signals:
		case <-time.After(time.Second):
			t.Fatal("listener did not start")
		}
	}
}

func waitForLifecycleResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("server supervisor did not finish")
		return nil
	}
}

func assertLifecycleEventBefore(t *testing.T, events []string, first, second string) {
	t.Helper()
	firstIndex := slices.Index(events, first)
	secondIndex := slices.Index(events, second)
	if firstIndex == -1 || secondIndex == -1 || firstIndex >= secondIndex {
		t.Fatalf("lifecycle events = %v, want %q before %q", events, first, second)
	}
}
