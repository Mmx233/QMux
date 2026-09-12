package client

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/rs/zerolog"
)

type observedDoneContext struct {
	context.Context
	observed chan<- struct{}
}

type cancelOnErrContext struct {
	context.Context
	cancel context.CancelFunc
}

func (c cancelOnErrContext) Err() error {
	c.cancel()
	return c.Context.Err()
}

func (c observedDoneContext) Done() <-chan struct{} {
	select {
	case c.observed <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestReconnectDelayBounds(t *testing.T) {
	tests := []struct {
		attempt int
		cap     time.Duration
	}{
		{-1, 5 * time.Second},
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 60 * time.Second},
		{10, 60 * time.Second},
		{int(^uint(0) >> 1), 60 * time.Second},
	}

	for _, test := range tests {
		for _, boundary := range []int64{0, -1} {
			var injectedN int64
			got := reconnectDelay(test.attempt, func(n int64) int64 {
				injectedN = n
				if boundary < 0 {
					return n - 1
				}
				return boundary
			})
			half := test.cap / 2
			if injectedN != int64(half) {
				t.Errorf("reconnectDelay(%d) injected n = %d, want %d for cap %v", test.attempt, injectedN, half, test.cap)
			}
			if got < half || got >= test.cap {
				t.Errorf("reconnectDelay(%d) = %v, want [%v, %v)", test.attempt, got, half, test.cap)
			}
			want := half
			if boundary < 0 {
				want = test.cap - time.Nanosecond
			}
			if got != want {
				t.Errorf("reconnectDelay(%d) boundary result = %v, want %v", test.attempt, got, want)
			}
		}
	}
}

func TestReconnectDelayDistribution(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 9))
	for _, test := range []struct {
		attempt int
		cap     time.Duration
	}{
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 60 * time.Second},
		{10, 60 * time.Second},
	} {
		half := test.cap / 2
		var bins [10]int
		for range 1000 {
			delay := reconnectDelay(test.attempt, rng.Int64N)
			if delay < half || delay >= test.cap {
				t.Fatalf("reconnectDelay(%d) = %v, want [%v, %v)", test.attempt, delay, half, test.cap)
			}
			bins[int((delay-half)*10/half)]++
		}
		for bin, count := range bins {
			if count < 50 || count > 150 {
				t.Errorf("reconnectDelay(%d) bin %d count = %d, want 50..150", test.attempt, bin, count)
			}
		}
	}
}

func TestWaitForReconnect(t *testing.T) {
	assertCanceled := func(t *testing.T, callerCtx, managerCtx context.Context, entered <-chan struct{}, cancel context.CancelFunc) {
		t.Helper()
		result := make(chan bool, 1)
		go func() {
			result <- waitForReconnect(callerCtx, managerCtx, time.Hour)
		}()
		<-entered
		cancel()

		select {
		case waited := <-result:
			if waited {
				t.Fatal("canceled reconnect wait reported timer delivery")
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatal("canceled reconnect wait did not return within 250ms")
		}
	}

	t.Run("caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{}, 1)
		callerCtx := observedDoneContext{Context: ctx, observed: entered}
		assertCanceled(t, callerCtx, context.Background(), entered, cancel)
	})
	t.Run("manager cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{}, 1)
		managerCtx := observedDoneContext{Context: ctx, observed: entered}
		assertCanceled(t, context.Background(), managerCtx, entered, cancel)
	})
	t.Run("timer delivery", func(t *testing.T) {
		if !waitForReconnect(context.Background(), context.Background(), time.Millisecond) {
			t.Fatal("timer delivery did not complete reconnect wait")
		}
	})
	for _, test := range []struct {
		name          string
		cancelManager bool
	}{
		{name: "caller cancellation after timer wake"},
		{name: "manager cancellation after timer wake", cancelManager: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			canceling := cancelOnErrContext{Context: ctx, cancel: cancel}
			callerCtx, managerCtx := context.Context(canceling), context.Background()
			if test.cancelManager {
				callerCtx, managerCtx = managerCtx, callerCtx
			}
			if waitForReconnect(callerCtx, managerCtx, 0) {
				t.Fatal("reconnect wait admitted cancellation observed after timer wake")
			}
		})
	}
	for _, test := range []struct {
		name          string
		cancelManager bool
	}{
		{name: "caller and timer already ready"},
		{name: "manager and timer already ready", cancelManager: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			callerCtx, managerCtx := ctx, context.Background()
			if test.cancelManager {
				callerCtx, managerCtx = managerCtx, callerCtx
			}
			if waitForReconnect(callerCtx, managerCtx, 0) {
				t.Fatal("reconnect wait admitted with cancellation and timer both ready")
			}
		})
	}
}

func TestReconnectStagePersistsAcrossWorkers(t *testing.T) {
	lifecycleTLSConfigs(t)
	synctest.Test(t, func(t *testing.T) {
		cfg := completeConnectionManagerTestConfig(t, &config.Client{
			ClientID: "persistent-reconnect-stage",
			Server: config.ClientServer{Servers: []config.ServerEndpoint{{
				Address:    "127.0.0.1:8443",
				ServerName: "localhost",
			}}},
		})
		cm, err := NewConnectionManager(cfg, zerolog.Nop())
		if err != nil {
			t.Fatal(err)
		}
		var cancelWorker context.CancelFunc
		defer func() {
			if cancelWorker != nil {
				cancelWorker()
			}
			if err := cm.Stop(); err != nil {
				t.Error(err)
			}
		}()
		cm.tlsState.Store(nil)
		endpoint := cm.config.Server.GetServers()[0]
		state := func() (int, uint64) {
			cm.reconnectMu.Lock()
			defer cm.reconnectMu.Unlock()
			return cm.endpoints[0].nextReconnectStage, cm.endpoints[0].reconnectAttempts.Load()
		}

		if sc, err := cm.connectAndRegister(context.Background(), endpoint); err == nil || sc != nil {
			t.Fatalf("initial attempt = (%p, %v), want nil TLS-state failure", sc, err)
		}
		if stage, attempts := state(); stage != 0 || attempts != 0 {
			t.Fatalf("initial attempt left stage=%d attempts=%d, want 0/0", stage, attempts)
		}

		firstCtx, cancelFirst := context.WithTimeout(context.Background(), 75*time.Second)
		cancelWorker = cancelFirst
		cm.startReconnection(firstCtx, endpoint.Address, nil)
		cm.wg.Wait()
		cancelFirst()
		cancelWorker = nil
		stage, baseline := state()
		if stage != maxReconnectStage || baseline < uint64(maxReconnectStage) {
			t.Fatalf("first worker left stage=%d attempts=%d, want saturated stage and at least %d attempts", stage, baseline, maxReconnectStage)
		}

		secondCtx, cancelSecond := context.WithCancel(context.Background())
		cancelWorker = cancelSecond
		cm.startReconnection(secondCtx, endpoint.Address, nil)
		synctest.Wait()
		synctest.Sleep(30*time.Second - time.Nanosecond)
		if stage, attempts := state(); stage != maxReconnectStage || attempts != baseline {
			t.Fatalf("stage-4 worker admitted before 30s: stage=%d attempts=%d, want %d/%d", stage, attempts, maxReconnectStage, baseline)
		}
		synctest.Sleep(30 * time.Second)
		if stage, attempts := state(); stage != maxReconnectStage || attempts != baseline+1 {
			t.Fatalf("stage-4 worker at 60s-1ns left stage=%d attempts=%d, want %d/%d", stage, attempts, maxReconnectStage, baseline+1)
		}

		cancelSecond()
		cm.wg.Wait()
		cancelWorker = nil
		if stage, attempts := state(); stage != maxReconnectStage || attempts != baseline+1 {
			t.Fatalf("canceled worker changed stage/attempts to %d/%d, want %d/%d", stage, attempts, maxReconnectStage, baseline+1)
		}
	})
}

func TestNewConnectionManagerValidatesConfig(t *testing.T) {
	logger := zerolog.Nop()
	if _, err := NewConnectionManager(nil, logger); err == nil || !strings.Contains(err.Error(), "client config is nil") {
		t.Fatalf("NewConnectionManager(nil) error = %v", err)
	}

	cfg := &config.Client{ClientID: "test-client"}
	if _, err := NewConnectionManager(cfg, logger); err == nil || !strings.Contains(err.Error(), "server.servers") {
		t.Fatalf("NewConnectionManager error = %v, want server.servers", err)
	}
}

func TestNewConnectionManagerValidatesSemanticsBeforeCredentials(t *testing.T) {
	cfg := &config.Client{
		Server:            config.ClientServer{Servers: []config.ServerEndpoint{{Address: "server.example.com:8443"}}},
		HeartbeatInterval: time.Second,
		HealthTimeout:     2 * time.Second,
	}
	_, err := NewConnectionManager(cfg, zerolog.Nop())
	if err == nil || !strings.Contains(err.Error(), "local.host") || strings.Contains(err.Error(), "credentials") {
		t.Fatalf("NewConnectionManager error = %v, want local.host before credentials", err)
	}
}

func completeConnectionManagerTestConfig(t *testing.T, cfg *config.Client) *config.Client {
	t.Helper()
	if cfg.Local.Host == "" {
		cfg.Local = config.LocalService{Host: "127.0.0.1", Port: 1}
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = time.Hour
	}
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = 2 * time.Hour
	}
	if cfg.TLS.CACertFile == "" {
		cfg.TLS = lifecycleClientTLSFiles(t)
	}
	return cfg
}

func TestNewConnectionManagerDeduplicatesServers(t *testing.T) {
	cfg := &config.Client{
		ClientID: "test-client",
		Server: config.ClientServer{Servers: []config.ServerEndpoint{
			{Address: "server1.example.com:8443", ServerName: "server1"},
			{Address: "server1.example.com:8443", ServerName: "server1"},
			{Address: "server2.example.com:8443", ServerName: "server2"},
		}},
	}
	completeConnectionManagerTestConfig(t, cfg)

	cm, err := NewConnectionManager(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cm.config.Server.GetServers()); got != 2 {
		t.Fatalf("deduplicated server count = %d, want 2", got)
	}
}

func TestConnectionManagerStopJoinsBlockedPublication(t *testing.T) {
	cfg := &config.Client{
		ClientID:          "blocked-publication",
		HeartbeatInterval: time.Hour,
		HealthTimeout:     2 * time.Hour,
		Server: config.ClientServer{Servers: []config.ServerEndpoint{{
			Address:    "127.0.0.1:8443",
			ServerName: "localhost",
		}}},
	}
	completeConnectionManagerTestConfig(t, cfg)
	cm, err := NewConnectionManager(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	cm.NewConns = make(chan *ServerConnection)

	sc := NewServerConnection(
		cfg.Server.Servers[0].Address,
		cfg.Server.Servers[0].ServerName,
		cm.SessionCacheManager().GetOrCreate(cfg.Server.Servers[0].Address),
		zerolog.Nop(),
	)
	published := make(chan bool, 1)
	cm.publishMu.Lock()
	cm.wg.Go(func() {
		committed := cm.publishServerConnection(context.Background(), sc)
		if !committed {
			_ = sc.Close()
		}
		published <- committed
	})
	cm.publishMu.Unlock()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for cm.GetConnection(sc.ServerAddr()) != sc {
		select {
		case <-deadline.C:
			t.Fatal("connection did not reach the publication gate")
		case <-ticker.C:
		}
	}

	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
	if committed := <-published; committed {
		t.Fatal("blocked NewConns delivery committed during Stop")
	}
	if cm.TotalCount() != 0 {
		t.Fatalf("Stop retained %d published connections", cm.TotalCount())
	}
	if sc.State() != StateDisconnected {
		t.Fatalf("provisional connection state = %s, want disconnected", sc.State())
	}
	select {
	case got, ok := <-cm.NewConns:
		if ok {
			t.Fatalf("received connection after Stop: %p", got)
		}
	default:
	}
}

func TestStartReconnectionRejectsCanceledRunContext(t *testing.T) {
	cfg := &config.Client{
		ClientID: "canceled-run",
		Server: config.ClientServer{Servers: []config.ServerEndpoint{{
			Address:    "127.0.0.1:8443",
			ServerName: "localhost",
		}}},
	}
	completeConnectionManagerTestConfig(t, cfg)
	cm, err := NewConnectionManager(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	cancelRun()

	cm.startReconnection(runCtx, cfg.Server.Servers[0].Address, nil)

	cm.reconnectMu.Lock()
	reconnecting := len(cm.reconnecting)
	cm.reconnectMu.Unlock()
	if reconnecting != 0 {
		t.Fatalf("canceled run started %d reconnection workers", reconnecting)
	}
	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
}
