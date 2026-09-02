package client

import (
	"crypto/tls"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func newTestServerConnection() *ServerConnection {
	return NewServerConnection(
		"server.example.com:8443",
		"server.example.com",
		tls.NewLRUClientSessionCache(0),
		zerolog.Nop(),
	)
}

func TestNewServerConnection(t *testing.T) {
	conn := newTestServerConnection()

	if conn.ServerAddr() != "server.example.com:8443" {
		t.Fatalf("ServerAddr = %q", conn.ServerAddr())
	}
	if conn.ServerName() != "server.example.com" {
		t.Fatalf("ServerName = %q", conn.ServerName())
	}
	if conn.State() != StateDisconnected || conn.IsHealthy() || conn.Connection() != nil {
		t.Fatalf("new connection state = %s, healthy = %v, connection = %p", conn.State(), conn.IsHealthy(), conn.Connection())
	}

	info := conn.Info()
	if info.Address != conn.ServerAddr() || info.ServerName != conn.ServerName() || info.State != StateDisconnected || info.Healthy {
		t.Fatalf("Info = %+v", info)
	}
}

func TestServerConnectionHealthTransitions(t *testing.T) {
	first := newTestServerConnection()
	second := newTestServerConnection()

	before := time.Now()
	first.MarkHealthy()
	second.MarkHealthy()
	after := time.Now()
	if !first.IsHealthy() || first.State() != StateConnected {
		t.Fatalf("MarkHealthy state = %s, healthy = %v", first.State(), first.IsHealthy())
	}
	if heartbeat := first.LastHeartbeat(); heartbeat.Before(before) || heartbeat.After(after) {
		t.Fatalf("LastHeartbeat = %v, want between %v and %v", heartbeat, before, after)
	}
	if !first.CheckHealth(time.Second) {
		t.Fatal("recent heartbeat reported unhealthy")
	}

	first.MarkUnhealthy()
	if first.IsHealthy() || first.State() != StateUnhealthy {
		t.Fatalf("MarkUnhealthy state = %s, healthy = %v", first.State(), first.IsHealthy())
	}
	if !second.IsHealthy() {
		t.Fatal("changing one connection affected another")
	}

	first.MarkHealthy()
	first.lastHeartbeat.Store(time.Now().Add(-time.Second).UnixNano())
	if first.CheckHealth(10*time.Millisecond) || first.IsHealthy() {
		t.Fatal("expired heartbeat reported healthy")
	}
	if newTestServerConnection().CheckHealth(time.Second) {
		t.Fatal("missing heartbeat reported healthy")
	}
}

func TestServerConnectionReceivedHealth(t *testing.T) {
	conn := newTestServerConnection()
	conn.SetHealthConfig(time.Second)
	if conn.CheckReceivedHealth() {
		t.Fatal("missing server heartbeat reported healthy")
	}

	conn.UpdateLastReceivedFromServer()
	if !conn.CheckReceivedHealth() {
		t.Fatal("recent server heartbeat reported unhealthy")
	}
	conn.lastReceivedFromServer.Store(time.Now().Add(-2 * time.Second).UnixNano())
	if conn.CheckReceivedHealth() {
		t.Fatal("expired server heartbeat reported healthy")
	}
}

func TestServerConnectionReceivedHealthConcurrent(t *testing.T) {
	conn := newTestServerConnection()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				conn.UpdateLastReceivedFromServer()
				_ = conn.LastReceivedFromServer()
			}
		})
	}
	wg.Wait()
	if conn.LastReceivedFromServer().IsZero() {
		t.Fatal("concurrent updates left the timestamp unset")
	}
}

func TestConnectionStateString(t *testing.T) {
	tests := []struct {
		state ConnectionState
		want  string
	}{
		{StateDisconnected, "disconnected"},
		{StateConnecting, "connecting"},
		{StateConnected, "connected"},
		{StateUnhealthy, "unhealthy"},
		{ConnectionState(99), "unknown"},
	}
	for _, test := range tests {
		if got := test.state.String(); got != test.want {
			t.Errorf("ConnectionState(%d).String() = %q, want %q", test.state, got, test.want)
		}
	}
}

func TestServerConnectionHeartbeatWriteFailure(t *testing.T) {
	for _, withCallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "without callback", true: "with callback"}[withCallback], func(t *testing.T) {
			conn := newTestServerConnection()
			conn.MarkHealthy()

			called := false
			if withCallback {
				conn.SetReconnectCallback(func(address string) {
					called = true
					if address != conn.ServerAddr() {
						t.Errorf("callback address = %q, want %q", address, conn.ServerAddr())
					}
				})
			}

			err := conn.SendHeartbeat()
			if err == nil || !strings.Contains(err.Error(), "no control stream") {
				t.Fatalf("SendHeartbeat error = %v", err)
			}
			if conn.IsHealthy() || conn.State() != StateUnhealthy {
				t.Fatalf("write failure state = %s, healthy = %v", conn.State(), conn.IsHealthy())
			}
			if called != withCallback {
				t.Fatalf("callback called = %v, want %v", called, withCallback)
			}
		})
	}
}
