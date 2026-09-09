package server

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/Mmx233/QMux/server/traffic"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type serverCopyBufferObserver struct {
	size int
}

func (r *serverCopyBufferObserver) Read(p []byte) (int, error) {
	r.size = len(p)
	return 0, io.EOF
}

func snapshotServerTLSFiles(t *testing.T) config.ServerTLS {
	t.Helper()
	certificate, _ := registrationTestCertificate(t)
	privateKey, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatalf("marshal server private key: %v", err)
	}
	directory := t.TempDir()
	certificateFile := filepath.Join(directory, "server.crt")
	privateKeyFile := filepath.Join(directory, "server.key")
	if err := os.WriteFile(certificateFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0o600); err != nil {
		t.Fatalf("write server certificate: %v", err)
	}
	if err := os.WriteFile(privateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}), 0o600); err != nil {
		t.Fatalf("write server private key: %v", err)
	}
	return config.ServerTLS{ServerCertFile: certificateFile, ServerKeyFile: privateKeyFile}
}

func TestNewUsesConfiguredCopyBufferPool(t *testing.T) {
	const copyBufferSize = 48 << 10
	srv, err := New(&config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr:    "127.0.0.1:8443",
			TrafficAddr: "127.0.0.1:8080",
			Protocol:    "tcp",
		}},
		Auth:              config.ServerAuth{Method: "token", Token: "0123456789abcdef"},
		TLS:               snapshotServerTLSFiles(t),
		TCPCopyBufferSize: copyBufferSize,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, connectionPool := range srv.pools {
		t.Cleanup(connectionPool.Stop)
	}

	observer := &serverCopyBufferObserver{}
	if _, err := srv.copyBufferPool.CopyBuffered(io.Discard, observer, true); err != nil {
		t.Fatalf("CopyBuffered: %v", err)
	}
	if observer.size != copyBufferSize {
		t.Fatalf("copy buffer size = %d, want %d", observer.size, copyBufferSize)
	}
}

func TestNewLoadsAndLogsInitialTLSState(t *testing.T) {
	var output bytes.Buffer
	previousLogger := log.Logger
	log.Logger = zerolog.New(&output)
	t.Cleanup(func() { log.Logger = previousLogger })
	tlsFiles := snapshotServerTLSFiles(t)
	srv, err := New(&config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp",
		}},
		Auth: config.ServerAuth{Method: "token", Token: "0123456789abcdef"},
		TLS:  tlsFiles,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		srv.tlsReloader.Stop()
		for _, connectionPool := range srv.pools {
			connectionPool.Stop()
		}
	})
	if state := srv.tlsState.Load(); state == nil || len(state.certificate.Certificate) == 0 || state.clientCAs != nil {
		t.Fatalf("initial TLS state = %+v, want token server certificate only", state)
	}
	logs := output.String()
	if strings.Count(logs, `"phase":"initial"`) != 1 || !strings.Contains(logs, `"level":"info"`) ||
		!strings.Contains(logs, `"changed":true`) {
		t.Fatalf("initial TLS result log = %s", logs)
	}
}

func TestNewRejectsRequiredCAFromLoggedInitialLoad(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T) string
	}{
		{name: "missing", prepare: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing-ca.pem") }},
		{name: "invalid", prepare: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "invalid-ca.pem")
			if err := os.WriteFile(path, []byte("invalid CA"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			previousLogger := log.Logger
			log.Logger = zerolog.New(&output)
			t.Cleanup(func() { log.Logger = previousLogger })
			tlsFiles := snapshotServerTLSFiles(t)
			srv, err := New(&config.Server{
				Listeners: []config.QuicListener{{
					QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp",
				}},
				Auth: config.ServerAuth{Method: "mtls", CACertFile: test.prepare(t)},
				TLS:  tlsFiles,
			})
			if err == nil || srv != nil || !strings.Contains(err.Error(), "load initial TLS material") {
				t.Fatalf("New() = (%v, %v), want logged initial CA failure", srv, err)
			}
			logs := output.String()
			if strings.Count(logs, `"phase":"initial"`) != 1 || !strings.Contains(logs, `"level":"error"`) {
				t.Fatalf("initial failure log = %s", logs)
			}
		})
	}
}

func TestServerTLSUnchangedStartupKeepsPublishedState(t *testing.T) {
	var output bytes.Buffer
	previousLogger := log.Logger
	log.Logger = zerolog.New(&output)
	t.Cleanup(func() { log.Logger = previousLogger })
	tlsFiles := snapshotServerTLSFiles(t)
	tlsFiles.AutoReload = true
	srv, err := New(&config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp",
		}},
		Auth: config.ServerAuth{Method: "token", Token: "0123456789abcdef"},
		TLS:  tlsFiles,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, connectionPool := range srv.pools {
		t.Cleanup(connectionPool.Stop)
	}
	before := srv.tlsState.Load()
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.tlsReloader.PrepareStart(ctx, true); err != nil {
		cancel()
		t.Fatalf("PrepareStart: %v", err)
	}
	if after := srv.tlsState.Load(); after != before {
		t.Fatal("unchanged startup reread replaced the published TLS state")
	}
	cancel()
	srv.tlsReloader.Stop()
	if err := srv.tlsReloader.Wait(); err != nil {
		t.Fatalf("owned watcher cancellation: %v", err)
	}
	logs := output.String()
	if strings.Count(logs, `"phase":"startup"`) != 1 || !strings.Contains(logs, `"changed":false`) {
		t.Fatalf("unchanged startup result log = %s", logs)
	}
}

func TestRouteSnapshotIncludesPoolCapacity(t *testing.T) {
	const addr = "route"
	p := pool.New(addr, pool.NewRoundRobinBalancer(), zerolog.Nop())
	defer p.Stop()
	pending := p.BeginPending()
	defer p.Abort(pending)

	s := &Server{
		config: &config.Server{Listeners: []config.QuicListener{{QuicAddr: addr, Protocol: "tcp"}}},
		pools:  map[string]*pool.ConnectionPool{addr: p},
	}
	route := s.Snapshot().Routes[0]
	if !reflect.DeepEqual(route.PoolCapacity, pool.CapacitySnapshot{
		ServerPending: 1,
		PendingRegistrations: pool.LimitSnapshot{
			Current:   1,
			HighWater: 1,
			Limit:     config.DefaultMaxPendingRegistrations,
		},
		ClientGenerations: pool.LimitSnapshot{
			Limit: config.DefaultMaxClientGenerations,
		},
		TCPConnectionsPerGeneration: pool.LimitSnapshot{
			Limit: config.DefaultMaxTCPConnectionsPerGeneration,
		},
		PendingTCPSetupsPerGeneration: pool.LimitSnapshot{
			Limit: config.DefaultMaxPendingTCPSetupsPerGeneration,
		},
		UDPSessionsPerGeneration: pool.LimitSnapshot{
			Limit: config.DefaultMaxUDPSessionsPerGeneration,
		},
	}) {
		t.Fatalf("PoolCapacity = %+v", route.PoolCapacity)
	}
}

func TestNewValidatesListenersBeforeCertificates(t *testing.T) {
	if _, err := New(nil); err == nil || !strings.Contains(err.Error(), "server config is nil") {
		t.Fatalf("New(nil) error = %v", err)
	}

	_, err := New(&config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    "127.0.0.1:8443",
		TrafficAddr: "127.0.0.1:8080",
		Protocol:    "invalid",
	}}})
	if err == nil || !strings.Contains(err.Error(), "listeners[0].protocol") {
		t.Fatalf("New() error = %v, want listener protocol validation", err)
	}

	tests := []struct {
		name string
		quic config.Quic
		path string
	}{
		{"stream max-only", config.Quic{MaxStreamReceiveWindow: 512*1024 - 1}, "listeners[0].initial_stream_receive_window"},
		{"stream initial-only", config.Quic{InitialStreamReceiveWindow: 6*1024*1024 + 1}, "listeners[0].initial_stream_receive_window"},
		{"connection max-only", config.Quic{MaxConnectionReceiveWindow: 768*1024 - 1}, "listeners[0].initial_connection_receive_window"},
		{"connection initial-only", config.Quic{InitialConnectionReceiveWindow: 15*1024*1024 + 1}, "listeners[0].initial_connection_receive_window"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(&config.Server{Listeners: []config.QuicListener{{
				QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp", Quic: test.quic,
			}}})
			if err == nil || !strings.Contains(err.Error(), test.path) || strings.Contains(err.Error(), "certificate") {
				t.Fatalf("New() error = %v, want %s before certificates", err, test.path)
			}
		})
	}

	_, err = New(&config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp",
		}},
		Auth: config.ServerAuth{Method: "token", Token: "0123456789abcdef"},
		TLS: config.ServerTLS{
			ServerCertFile: "missing.pem",
			ServerKeyFile:  "missing-key.pem",
			SessionTicketEncryptionKeyRotationOverlap: new(uint8(0)),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "tls.session_ticket_encryption_key_rotation_overlap") {
		t.Fatalf("New() error = %v, want overlap validation before certificate loading", err)
	}
}

func TestCloneServerConfigOwnsPointers(t *testing.T) {
	fragmentation := true
	overlap := uint8(2)
	original := &config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr: "127.0.0.1:8443",
			UDP: config.UDPConfig{
				EnableFragmentation: &fragmentation,
			},
		}, {
			QuicAddr: "127.0.0.1:8444",
		}},
		TLS: config.ServerTLS{SessionTicketEncryptionKeyRotationOverlap: &overlap},
	}

	cloned := cloneServerConfig(original)
	if cloned.Listeners[0].UDP.EnableFragmentation == original.Listeners[0].UDP.EnableFragmentation {
		t.Fatal("clone retained caller-owned boolean pointers")
	}
	if cloned.Listeners[1].UDP.EnableFragmentation != nil {
		t.Fatal("clone did not preserve nil boolean pointers")
	}
	if cloned.TLS.SessionTicketEncryptionKeyRotationOverlap == original.TLS.SessionTicketEncryptionKeyRotationOverlap {
		t.Fatal("clone retained caller-owned overlap pointer")
	}

	fragmentation = false
	overlap = 7
	original.Listeners[0].QuicAddr = "mutated"
	if !*cloned.Listeners[0].UDP.EnableFragmentation || cloned.Listeners[0].QuicAddr != "127.0.0.1:8443" ||
		*cloned.TLS.SessionTicketEncryptionKeyRotationOverlap != 2 {
		t.Fatal("caller mutation changed cloned listeners")
	}

	if cloneServerConfig(&config.Server{}).TLS.SessionTicketEncryptionKeyRotationOverlap != nil {
		t.Fatal("clone did not preserve omitted overlap")
	}
}

func TestSnapshotRequiresEveryRoute(t *testing.T) {
	listeners := []config.QuicListener{
		{QuicAddr: "route-tcp", TrafficAddr: "127.0.0.1:0", Protocol: "tcp"},
		{QuicAddr: "route-both", TrafficAddr: "127.0.0.1:0", Protocol: "both"},
	}
	srv := newSnapshotTestServer(t, listeners)
	readSnapshot := func() Snapshot {
		snapshot := srv.Snapshot()
		if got := srv.Ready(); got != snapshot.Ready {
			t.Fatalf("Ready() = %v, snapshot readiness = %v", got, snapshot.Ready)
		}
		return snapshot
	}
	if snapshot := readSnapshot(); snapshot.Ready {
		t.Fatal("server was ready before traffic listeners started")
	}

	if err := srv.trafficManager.Start(t.Context()); err != nil {
		t.Fatalf("start traffic manager: %v", err)
	}
	defer srv.trafficManager.Stop()

	addSnapshotClient(t, srv.pools["route-tcp"], "tcp", "tcp")
	if snapshot := readSnapshot(); snapshot.Ready || !snapshot.Routes[0].Ready || snapshot.Routes[1].Ready {
		t.Fatalf("one eligible route snapshot = %+v, want global not ready", snapshot)
	}

	bothTCP := addSnapshotClient(t, srv.pools["route-both"], "both-tcp", "tcp")
	if snapshot := readSnapshot(); snapshot.Ready || snapshot.Routes[1].Ready {
		t.Fatalf("both route with only TCP snapshot = %+v, want not ready", snapshot)
	}
	bothUDP := addSnapshotClient(t, srv.pools["route-both"], "both-udp", "udp")
	if snapshot := readSnapshot(); !snapshot.Ready || !snapshot.Routes[1].Ready {
		t.Fatalf("all eligible routes snapshot = %+v, want ready", snapshot)
	}

	if !srv.pools["route-both"].MarkUnhealthy(bothUDP) {
		t.Fatal("mark UDP client unhealthy")
	}
	if snapshot := readSnapshot(); snapshot.Ready {
		t.Fatalf("snapshot = %+v after UDP became unhealthy, want not ready", snapshot)
	}
	if !srv.pools["route-both"].MarkHealthy(bothUDP) {
		t.Fatal("mark UDP client healthy")
	}
	if !readSnapshot().Ready {
		t.Fatal("server did not become ready after eligibility recovered")
	}

	if !srv.pools["route-both"].Remove(bothTCP) || readSnapshot().Ready {
		t.Fatal("removing the only eligible TCP client did not clear readiness")
	}
	addSnapshotClient(t, srv.pools["route-both"], "both-tcp-replacement", "tcp")
	if !readSnapshot().Ready {
		t.Fatal("server did not become ready before traffic listeners closed")
	}

	srv.trafficManager.Close()
	if snapshot := readSnapshot(); snapshot.Ready || snapshot.Routes[0].Listening || snapshot.Routes[1].Listening {
		t.Fatalf("closing snapshot = %+v, want not listening and not ready", snapshot)
	}
}

func TestSnapshotFailsClosedForEmptyAndUnknownRoutes(t *testing.T) {
	empty := &Server{config: &config.Server{}, pools: map[string]*pool.ConnectionPool{}}
	if empty.Snapshot().Ready {
		t.Fatal("empty server snapshot was vacuously ready")
	}

	unknown := newSnapshotTestServer(t, []config.QuicListener{{
		QuicAddr: "unknown",
		Protocol: "future",
	}})
	if err := unknown.trafficManager.Start(t.Context()); err != nil {
		t.Fatalf("start unknown-protocol traffic manager: %v", err)
	}
	defer unknown.trafficManager.Stop()
	addSnapshotClient(t, unknown.pools["unknown"], "client", "tcp", "udp")
	if snapshot := unknown.Snapshot(); snapshot.Ready || !snapshot.Routes[0].Listening {
		t.Fatalf("unknown protocol snapshot = %+v, want listening but not ready", snapshot)
	}
}

func TestSnapshotConcurrentHealthUpdates(t *testing.T) {
	listeners := []config.QuicListener{{QuicAddr: "route", TrafficAddr: "127.0.0.1:0", Protocol: "tcp"}}
	srv := newSnapshotTestServer(t, listeners)
	if err := srv.trafficManager.Start(t.Context()); err != nil {
		t.Fatalf("start traffic manager: %v", err)
	}
	defer srv.trafficManager.Stop()
	client := addSnapshotClient(t, srv.pools["route"], "client", "tcp")

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 1_000 {
			_ = srv.Snapshot()
		}
	})
	wg.Go(func() {
		for range 1_000 {
			srv.pools["route"].MarkUnhealthy(client)
			srv.pools["route"].MarkHealthy(client)
		}
	})
	wg.Wait()
}

func TestSnapshotCopiesTCPAdmissionDuringConcurrentReads(t *testing.T) {
	address := freeSnapshotTCPAddress(t)
	listeners := []config.QuicListener{{QuicAddr: "route", TrafficAddr: address, Protocol: "tcp"}}
	srv := newSnapshotTestServer(t, listeners)
	if err := srv.trafficManager.Start(t.Context()); err != nil {
		t.Fatalf("start traffic manager: %v", err)
	}
	defer srv.trafficManager.Stop()

	dialSnapshotRejectedTCP(t, address)
	waitForSnapshotUnavailable(t, srv, 1)
	first := srv.Snapshot()
	if got := first.Routes[0].TCPAdmission; got.Unavailable != 1 || got.SetupCurrent != 0 || got.ActiveCurrent != 0 {
		t.Fatalf("first TCP admission snapshot = %+v", got)
	}

	dialSnapshotRejectedTCP(t, address)
	waitForSnapshotUnavailable(t, srv, 2)
	if got := first.Routes[0].TCPAdmission.Unavailable; got != 1 {
		t.Fatalf("previous value snapshot changed to unavailable=%d, want 1", got)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 1_000 {
			snapshot := srv.Snapshot()
			if len(snapshot.Routes) != 1 {
				t.Errorf("route snapshots = %d, want 1", len(snapshot.Routes))
				return
			}
		}
	})
	wg.Go(func() {
		for range 32 {
			dialSnapshotRejectedTCP(t, address)
		}
	})
	wg.Wait()
	waitForSnapshotUnavailable(t, srv, 34)
	if got := srv.Snapshot().Routes[0].TCPAdmission; got.SetupCurrent != 0 || got.ActiveCurrent != 0 || got.Unavailable != 34 {
		t.Fatalf("final TCP admission snapshot = %+v", got)
	}
}

func freeSnapshotTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate snapshot TCP address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release snapshot TCP address: %v", err)
	}
	return address
}

func dialSnapshotRejectedTCP(t *testing.T, address string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("dial snapshot TCP listener: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set snapshot TCP deadline: %v", err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("unavailable snapshot TCP connection remained open")
	}
}

func waitForSnapshotUnavailable(t *testing.T, srv *Server, want uint64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := srv.Snapshot().Routes[0].TCPAdmission.Unavailable; got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("TCP unavailable terminals = %d, want %d", srv.Snapshot().Routes[0].TCPAdmission.Unavailable, want)
}

func newSnapshotTestServer(t *testing.T, listeners []config.QuicListener) *Server {
	t.Helper()
	pools := make(map[string]*pool.ConnectionPool, len(listeners))
	for _, listener := range listeners {
		connectionPool := pool.New(listener.QuicAddr, pool.NewRoundRobinBalancer(), zerolog.Nop())
		pools[listener.QuicAddr] = connectionPool
		t.Cleanup(connectionPool.Stop)
	}
	serverConfig := &config.Server{Listeners: listeners}
	return &Server{
		config:         serverConfig,
		pools:          pools,
		trafficManager: traffic.NewManager(serverConfig, pools, protocol.NewCopyBufferPool(protocol.DefaultCopyBufferSize), zerolog.Nop()),
	}
}

func addSnapshotClient(t *testing.T, connectionPool *pool.ConnectionPool, id string, capabilities ...string) *pool.ClientConn {
	t.Helper()
	client := &pool.ClientConn{
		ID: id,
		Metadata: pool.ClientMetadata{
			Capabilities: capabilities,
		},
	}
	if err := connectionPool.Add(client); err != nil {
		t.Fatalf("add client %q: %v", id, err)
	}
	return client
}
