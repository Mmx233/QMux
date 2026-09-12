package client

import (
	"crypto/tls"
	"testing"
	"testing/synctest"
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
}

func TestServerConnectionHealthTransitions(t *testing.T) {
	first := newTestServerConnection()
	second := newTestServerConnection()

	first.MarkHealthy()
	second.MarkHealthy()
	if !first.IsHealthy() || first.State() != StateConnected {
		t.Fatalf("MarkHealthy state = %s, healthy = %v", first.State(), first.IsHealthy())
	}

	first.MarkUnhealthy()
	if first.IsHealthy() || first.State() != StateUnhealthy {
		t.Fatalf("MarkUnhealthy state = %s, healthy = %v", first.State(), first.IsHealthy())
	}
	if !second.IsHealthy() {
		t.Fatal("changing one connection affected another")
	}
}

func TestServerConnectionReconnectStabilityGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := newTestServerConnection()
		second := newTestServerConnection()
		controlStartedAt := time.Now()

		first.MarkHealthy()
		first.markReconnectStable(controlStartedAt)
		if first.reconnectStable.Load() {
			t.Fatal("new connection became retry-stable before grace")
		}

		synctest.Sleep(reconnectStableGrace - time.Nanosecond)
		first.markReconnectStable(controlStartedAt)
		if first.reconnectStable.Load() {
			t.Fatal("connection became retry-stable before exact grace boundary")
		}
		if second.reconnectStable.Load() {
			t.Fatal("one generation changed another generation's retry stability")
		}

		synctest.Sleep(time.Nanosecond)
		first.markReconnectStable(controlStartedAt)
		if !first.reconnectStable.Load() {
			t.Fatal("connection did not become retry-stable at exact grace boundary")
		}
		if second.reconnectStable.Load() {
			t.Fatal("stable generation changed another generation's retry stability")
		}
	})
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
