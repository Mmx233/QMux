package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/rs/zerolog"
)

func TestCalculateBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{-1, InitialBackoff},
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, MaxBackoff},
		{10, MaxBackoff},
	}

	for _, test := range tests {
		if got := CalculateBackoff(test.attempt); got != test.want {
			t.Errorf("CalculateBackoff(%d) = %v, want %v", test.attempt, got, test.want)
		}
	}
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
