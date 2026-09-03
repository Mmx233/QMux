package client

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/rs/zerolog"
)

type observedDoneContext struct {
	context.Context
	observed chan<- struct{}
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
}

func TestNewConnectionManagerValidatesConfig(t *testing.T) {
	logger := zerolog.Nop()
	if _, err := NewConnectionManager(nil, logger); err == nil || !strings.Contains(err.Error(), "client config is nil") {
		t.Fatalf("NewConnectionManager(nil) error = %v", err)
	}

	cfg := &config.Client{ClientID: "test-client"}
	if _, err := NewConnectionManager(cfg, logger); err == nil {
		t.Fatal("NewConnectionManager accepted an empty server list")
	}
}

func TestConnectionManagerStartValidatesSemanticsBeforeCredentials(t *testing.T) {
	cfg := &config.Client{
		Server:            config.ClientServer{Servers: []config.ServerEndpoint{{Address: "server.example.com:8443"}}},
		HeartbeatInterval: time.Second,
		HealthTimeout:     2 * time.Second,
	}
	cm, err := NewConnectionManager(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cm.Stop() })

	err = cm.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "local.host") || strings.Contains(err.Error(), "credentials") {
		t.Fatalf("Start error = %v, want local.host before credentials", err)
	}
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
	cm, err := NewConnectionManager(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	cm.NewConns = make(chan *ServerConnection)

	sc := NewServerConnection(
		cfg.Server.Servers[0].Address,
		cfg.Server.Servers[0].ServerName,
		cm.sessionCaches.GetOrCreate(cfg.Server.Servers[0].Address),
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
