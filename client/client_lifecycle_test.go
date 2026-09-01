package client

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
)

const clientLifecycleTimeout = 3 * time.Second

func callClientLifecycle(fn func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	return done
}

func awaitClientLifecycle[T any](t *testing.T, result <-chan T, event string) (value T) {
	t.Helper()
	select {
	case value = <-result:
	case <-time.After(clientLifecycleTimeout):
		t.Errorf("timed out waiting for %s", event)
	}
	return value
}

func newClientLifecycleClient(t *testing.T, clientID string, endpoint config.ServerEndpoint) *Client {
	t.Helper()
	c, err := New(&config.Client{
		ClientID: clientID,
		Server:   config.ClientServer{Servers: []config.ServerEndpoint{endpoint}},
		Local:    config.LocalService{Host: "127.0.0.1", Port: 1},
		Quic: config.Quic{
			HandshakeIdleTimeout: 10 * time.Second,
			MaxIdleTimeout:       30 * time.Second,
		},
		TLS:               lifecycleClientTLSFiles(t),
		HeartbeatInterval: time.Hour,
		HealthTimeout:     2 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	c.connMgr.attemptTimeout = 20 * time.Second
	return c
}

func TestNewRejectsNilAndSemanticErrorsBeforeCredentials(t *testing.T) {
	if _, err := New(nil); err == nil || !strings.Contains(err.Error(), "client config is nil") {
		t.Fatalf("New(nil) error = %v", err)
	}

	_, err := New(&config.Client{Server: config.ClientServer{Servers: []config.ServerEndpoint{{
		Address: "server.example.com:8443",
	}}}})
	if err == nil || !strings.Contains(err.Error(), "local.host") || strings.Contains(err.Error(), "credentials") {
		t.Fatalf("New(invalid) error = %v, want local.host before credentials", err)
	}
}

func TestNewDeduplicatesBeforeCredentialIO(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.pem")
	conf := &config.Client{
		Server: config.ClientServer{Servers: []config.ServerEndpoint{
			{Address: "server.example.com:8443", ServerName: "first.example.com"},
			{Address: "server.example.com:8443", ServerName: "duplicate.example.com"},
		}},
		Local: config.LocalService{Host: "127.0.0.1", Port: 8080},
		TLS: config.ClientTLS{
			CACertFile:     missing,
			ClientCertFile: missing,
			ClientKeyFile:  missing,
		},
	}

	if _, err := New(conf); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("New error = %v, want missing credential", err)
	}
	if len(conf.Server.Servers) != 1 || conf.Server.Servers[0].ServerName != "first.example.com" {
		t.Fatalf("deduplicated servers = %+v, want first endpoint", conf.Server.Servers)
	}
}

func stallClientLifecycleRegistration(peer *lifecyclePeer) (<-chan struct{}, <-chan error) {
	ready := make(chan struct{})
	done := peer.serveRegistration(func(conn *quic.Conn, _ *quic.Stream, _ protocol.RegisterMsg) error {
		close(ready)
		<-conn.Context().Done()
		return nil
	})
	return ready, done
}

func TestClientLifecycle(t *testing.T) {
	offlineEndpoint := config.ServerEndpoint{Address: "127.0.0.1:1", ServerName: "lifecycle.test"}

	t.Run("Stop interrupts stalled registration and duplicate Start is rejected", func(t *testing.T) {
		peer := newLifecycleStartPeer(t)
		c := newClientLifecycleClient(t, "client-stop-stalled-registration", peer.endpoint())
		ready, serverDone := stallClientLifecycleRegistration(peer)

		startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
		awaitLifecycle(t, ready, "client registration to stall")

		duplicateErr := awaitClientLifecycle(
			t,
			callClientLifecycle(func() error { return c.Start(context.Background()) }),
			"duplicate Client.Start",
		)
		if !errors.Is(duplicateErr, ErrClientAlreadyStarted) {
			t.Fatalf("duplicate Start error = %v, want ErrClientAlreadyStarted", duplicateErr)
		}
		select {
		case err := <-startDone:
			t.Fatalf("active Start returned before Stop: %v", err)
		default:
		}

		if err := awaitClientLifecycle(t, callClientLifecycle(c.Stop), "Client.Stop"); err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		if err := awaitClientLifecycle(t, startDone, "background Client.Start"); err != nil {
			t.Fatalf("Start returned error after Stop: %v", err)
		}
		if err := awaitLifecycle(t, serverDone, "stalled registration connection close"); err != nil {
			t.Fatal(err)
		}
		assertLifecycleUnpublished(t, c.connMgr)
	})

	t.Run("Stop before Start rejects Start", func(t *testing.T) {
		c := newClientLifecycleClient(t, "client-stop-before-start", offlineEndpoint)
		if err := awaitClientLifecycle(t, callClientLifecycle(c.Stop), "pre-Start Client.Stop"); err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}

		err := awaitClientLifecycle(
			t,
			callClientLifecycle(func() error { return c.Start(context.Background()) }),
			"Client.Start after Stop",
		)
		if !errors.Is(err, ErrClientStopped) {
			t.Fatalf("Start after Stop error = %v, want ErrClientStopped", err)
		}
	})

	t.Run("startup failure preserves cause and tears down once", func(t *testing.T) {
		c := newClientLifecycleClient(t, "client-startup-failure", offlineEndpoint)
		c.config.TLS.CACertFile = filepath.Join(t.TempDir(), "missing-ca.crt")

		err := awaitClientLifecycle(
			t,
			callClientLifecycle(func() error { return c.Start(context.Background()) }),
			"failed Client.Start teardown",
		)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Start error = %v, want fs.ErrNotExist in chain", err)
		}
		if prefix := "start connection manager: load credentials: read CA cert:"; !strings.HasPrefix(err.Error(), prefix) {
			t.Fatalf("Start error = %q, want prefix %q", err, prefix)
		}

		concurrentStops := [2]<-chan error{
			callClientLifecycle(c.Stop),
			callClientLifecycle(c.Stop),
		}
		for attempt, stopDone := range concurrentStops {
			if err := awaitClientLifecycle(t, stopDone, "concurrent Client.Stop"); err != nil {
				t.Fatalf("concurrent Stop %d returned error: %v", attempt+1, err)
			}
		}
		if err := awaitClientLifecycle(t, callClientLifecycle(c.Stop), "repeated Client.Stop"); err != nil {
			t.Fatalf("repeated Stop returned error: %v", err)
		}
	})

	t.Run("caller cancellation races Stop", func(t *testing.T) {
		peer := newLifecycleStartPeer(t)
		c := newClientLifecycleClient(t, "client-cancel-stop-race", peer.endpoint())
		ready, serverDone := stallClientLifecycleRegistration(peer)
		runCtx, cancelRun := context.WithCancel(context.Background())
		defer cancelRun()

		startDone := callClientLifecycle(func() error { return c.Start(runCtx) })
		awaitLifecycle(t, ready, "racing client registration to stall")

		startRace := make(chan struct{})
		cancelDone := make(chan struct{})
		stopDone := make(chan error, 1)
		go func() {
			<-startRace
			cancelRun()
			close(cancelDone)
		}()
		go func() {
			<-startRace
			stopDone <- c.Stop()
		}()
		close(startRace)

		awaitClientLifecycle(t, cancelDone, "caller cancellation")
		if err := awaitClientLifecycle(t, stopDone, "racing Client.Stop"); err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		if err := awaitClientLifecycle(t, startDone, "racing Client.Start"); err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
		if err := awaitLifecycle(t, serverDone, "racing registration connection close"); err != nil {
			t.Fatal(err)
		}
		assertLifecycleUnpublished(t, c.connMgr)
	})
}
