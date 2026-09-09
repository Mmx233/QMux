package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const clientLifecycleTimeout = 3 * time.Second

type clientCopyBufferObserver struct {
	size int
}

type clientLifecycleLogGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *clientLifecycleLogGate) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"message":"starting client"`)) {
		g.once.Do(func() {
			close(g.entered)
			<-g.release
		})
	}
	return len(p), nil
}

func (r *clientCopyBufferObserver) Read(p []byte) (int, error) {
	r.size = len(p)
	return 0, io.EOF
}

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

func newClientLifecycleClient(t *testing.T, clientID string, endpoints ...config.ServerEndpoint) *Client {
	t.Helper()
	c, err := New(&config.Client{
		ClientID: clientID,
		Server:   config.ClientServer{Servers: endpoints},
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

func TestNewUsesConfiguredCopyBufferPool(t *testing.T) {
	const copyBufferSize = 32 << 10
	c, err := New(&config.Client{
		ClientID:          "copy-buffer-config",
		Server:            config.ClientServer{Servers: []config.ServerEndpoint{{Address: "server.example.com:8443"}}},
		Local:             config.LocalService{Host: "127.0.0.1", Port: 8080},
		TLS:               lifecycleClientTLSFiles(t),
		TCPCopyBufferSize: copyBufferSize,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	observer := &clientCopyBufferObserver{}
	if _, err := c.copyBufferPool.CopyBuffered(io.Discard, observer, true); err != nil {
		t.Fatalf("CopyBuffered: %v", err)
	}
	if observer.size != copyBufferSize {
		t.Fatalf("copy buffer size = %d, want %d", observer.size, copyBufferSize)
	}
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
	tests := []struct {
		name string
		quic config.Quic
		path string
	}{
		{"stream max-only", config.Quic{MaxStreamReceiveWindow: 512*1024 - 1}, "quic.initial_stream_receive_window"},
		{"stream initial-only", config.Quic{InitialStreamReceiveWindow: 6*1024*1024 + 1}, "quic.initial_stream_receive_window"},
		{"connection max-only", config.Quic{MaxConnectionReceiveWindow: 768*1024 - 1}, "quic.initial_connection_receive_window"},
		{"connection initial-only", config.Quic{InitialConnectionReceiveWindow: 15*1024*1024 + 1}, "quic.initial_connection_receive_window"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(&config.Client{
				Server: config.ClientServer{Servers: []config.ServerEndpoint{{Address: "server.example.com:8443"}}},
				Local:  config.LocalService{Host: "127.0.0.1", Port: 8080},
				Quic:   test.quic,
			})
			if err == nil || !strings.Contains(err.Error(), test.path) || strings.Contains(err.Error(), "credentials") {
				t.Fatalf("New() error = %v, want %s before credentials", err, test.path)
			}
		})
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

	t.Run("owned termination before TLS preparation is not a startup failure", func(t *testing.T) {
		for _, test := range []struct {
			name      string
			terminate func(*Client) error
		}{
			{name: "Stop", terminate: (*Client).Stop},
			{name: "Shutdown", terminate: func(c *Client) error { return c.Shutdown(context.Background()) }},
		} {
			t.Run(test.name, func(t *testing.T) {
				c := newClientLifecycleClient(t, "client-stop-before-tls-prepare", offlineEndpoint)
				gate := &clientLifecycleLogGate{entered: make(chan struct{}), release: make(chan struct{})}
				c.logger = zerolog.New(gate)
				release := sync.OnceFunc(func() { close(gate.release) })
				defer release()

				startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
				awaitClientLifecycle(t, gate.entered, "Client.Start TLS preparation barrier")
				terminationDone := callClientLifecycle(func() error { return test.terminate(c) })
				if err := awaitClientLifecycle(
					t,
					callClientLifecycle(c.connMgr.tlsReloader.Wait),
					"owned TLS reloader stop",
				); err != nil {
					t.Fatalf("owned TLS reloader stop returned error: %v", err)
				}
				release()

				if err := awaitClientLifecycle(t, terminationDone, "Client."+test.name); err != nil {
					t.Errorf("%s returned error: %v", test.name, err)
				}
				if err := awaitClientLifecycle(t, startDone, "concurrent Client.Start"); err != nil {
					t.Errorf("%s made Client.Start fail: %v", test.name, err)
				}
			})
		}
	})

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
		if err := os.Remove(c.connMgr.tlsConfig.CACertFile); err != nil {
			t.Fatalf("remove frozen CA file: %v", err)
		}

		err := awaitClientLifecycle(
			t,
			callClientLifecycle(func() error { return c.Start(context.Background()) }),
			"failed Client.Start teardown",
		)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Start error = %v, want fs.ErrNotExist in chain", err)
		}
		if prefix := "start connection manager: prepare TLS material: read TLS ca file:"; !strings.HasPrefix(err.Error(), prefix) {
			t.Fatalf("Start error = %q, want prefix %q", err, prefix)
		}
		if err := c.Shutdown(context.Background()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("post-terminal Shutdown error = %v, want fs.ErrNotExist in chain", err)
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

func TestClientStartReturnsFatalTLSWatcherCause(t *testing.T) {
	peer := newLifecycleStartPeer(t)
	serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(stream); err != nil {
			return err
		}
		<-conn.Context().Done()
		return nil
	})
	tlsFiles := lifecycleClientTLSFiles(t)
	tlsFiles.AutoReload = true
	c, err := New(&config.Client{
		ClientID:          "fatal-tls-watcher",
		Server:            config.ClientServer{Servers: []config.ServerEndpoint{peer.endpoint()}},
		Local:             config.LocalService{Host: "127.0.0.1", Port: 1},
		TLS:               tlsFiles,
		HeartbeatInterval: time.Hour,
		HealthTimeout:     2 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
	deadline := time.NewTimer(clientLifecycleTimeout)
	defer deadline.Stop()
	for c.HealthyConnectionCount() != 1 {
		select {
		case err := <-startDone:
			t.Fatalf("Client.Start returned before watcher failure: %v", err)
		case <-deadline.C:
			t.Fatal("client did not become healthy")
		case <-time.After(time.Millisecond):
		}
	}
	parent := filepath.Dir(tlsFiles.CACertFile)
	removed := parent + "-removed"
	if err := os.Rename(parent, removed); err != nil {
		t.Fatalf("rename watched TLS parent: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(removed) })

	err = awaitClientLifecycle(t, startDone, "fatal TLS watcher teardown")
	watcherErr := c.connMgr.tlsReloader.Wait()
	if watcherErr == nil || !errors.Is(err, watcherErr) {
		t.Fatalf("Client.Start error = %v, want watcher cause %v", err, watcherErr)
	}
	if err := awaitLifecycle(t, serverDone, "fatal watcher connection close"); err != nil {
		t.Fatal(err)
	}
}
