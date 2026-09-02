package server

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/Mmx233/QMux/server/traffic"
	"github.com/rs/zerolog"
)

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
	if snapshot := srv.Snapshot(); snapshot.Ready {
		t.Fatal("server was ready before traffic listeners started")
	}

	if err := srv.trafficManager.Start(t.Context()); err != nil {
		t.Fatalf("start traffic manager: %v", err)
	}
	defer srv.trafficManager.Stop()

	addSnapshotClient(t, srv.pools["route-tcp"], "tcp", "tcp")
	if snapshot := srv.Snapshot(); snapshot.Ready || !snapshot.Routes[0].Ready || snapshot.Routes[1].Ready {
		t.Fatalf("one eligible route snapshot = %+v, want global not ready", snapshot)
	}

	bothTCP := addSnapshotClient(t, srv.pools["route-both"], "both-tcp", "tcp")
	if snapshot := srv.Snapshot(); snapshot.Ready || snapshot.Routes[1].Ready {
		t.Fatalf("both route with only TCP snapshot = %+v, want not ready", snapshot)
	}
	bothUDP := addSnapshotClient(t, srv.pools["route-both"], "both-udp", "udp")
	if snapshot := srv.Snapshot(); !snapshot.Ready || !snapshot.Routes[1].Ready {
		t.Fatalf("all eligible routes snapshot = %+v, want ready", snapshot)
	}

	if !srv.pools["route-both"].MarkUnhealthy(bothUDP) {
		t.Fatal("mark UDP client unhealthy")
	}
	if snapshot := srv.Snapshot(); snapshot.Ready {
		t.Fatalf("snapshot = %+v after UDP became unhealthy, want not ready", snapshot)
	}
	if !srv.pools["route-both"].MarkHealthy(bothUDP) {
		t.Fatal("mark UDP client healthy")
	}
	if !srv.Snapshot().Ready {
		t.Fatal("server did not become ready after eligibility recovered")
	}

	if !srv.pools["route-both"].Remove(bothTCP) || srv.Snapshot().Ready {
		t.Fatal("removing the only eligible TCP client did not clear readiness")
	}
	addSnapshotClient(t, srv.pools["route-both"], "both-tcp-replacement", "tcp")
	if !srv.Snapshot().Ready {
		t.Fatal("server did not become ready before traffic listeners closed")
	}

	srv.trafficManager.Close()
	if snapshot := srv.Snapshot(); snapshot.Ready || snapshot.Routes[0].Listening || snapshot.Routes[1].Listening {
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
		trafficManager: traffic.NewManager(serverConfig, pools, zerolog.Nop()),
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
