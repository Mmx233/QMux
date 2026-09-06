package client

import (
	"testing"

	"github.com/Mmx233/QMux/config"
	"github.com/rs/zerolog"
)

func TestClientReady(t *testing.T) {
	client := &Client{
		config: &config.Client{Server: config.ClientServer{Servers: []config.ServerEndpoint{
			{Address: "server-a:8443"},
			{Address: "server-b:8443"},
		}}},
		connMgr: &ConnectionManager{},
	}
	client.started = true

	first := NewServerConnection("server-a:8443", "", nil, zerolog.Nop())
	second := NewServerConnection("server-b:8443", "", nil, zerolog.Nop())
	t.Cleanup(func() {
		_ = first.Close()
		_ = second.Close()
	})
	first.MarkHealthy()
	client.connMgr.connections.Store(first.ServerAddr(), first)
	if client.Ready() {
		t.Fatal("client was ready with one configured server disconnected")
	}

	second.MarkHealthy()
	client.connMgr.connections.Store(second.ServerAddr(), second)
	if !client.Ready() {
		t.Fatal("client was not ready with every configured server healthy")
	}

	client.stopping = true
	if client.Ready() {
		t.Fatal("stopping client was ready")
	}
}
