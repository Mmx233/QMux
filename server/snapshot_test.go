package server

import (
	"strings"
	"sync"
	"testing"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/Mmx233/QMux/server/traffic"
	"github.com/rs/zerolog"
)

func TestValidateListeners(t *testing.T) {
	base := []config.QuicListener{{
		QuicAddr:    "127.0.0.1:8443",
		TrafficAddr: "127.0.0.1:8080",
		Protocol:    "tcp",
	}}
	tests := []struct {
		name      string
		listeners []config.QuicListener
		wantErr   bool
	}{
		{name: "valid", listeners: base},
		{name: "wildcard host", listeners: []config.QuicListener{{QuicAddr: ":8443", TrafficAddr: ":8080", Protocol: "both"}}},
		{name: "tcp and udp share address", listeners: []config.QuicListener{
			{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp"},
			{QuicAddr: "127.0.0.1:8444", TrafficAddr: "127.0.0.1:8080", Protocol: "udp"},
		}},
		{name: "zero listeners", wantErr: true},
		{name: "invalid QUIC address", listeners: []config.QuicListener{{QuicAddr: "bad", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp"}}, wantErr: true},
		{name: "invalid traffic address", listeners: []config.QuicListener{{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:0", Protocol: "tcp"}}, wantErr: true},
		{name: "invalid protocol", listeners: []config.QuicListener{{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: ""}}, wantErr: true},
		{name: "duplicate QUIC address", listeners: []config.QuicListener{
			{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp"},
			{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8081", Protocol: "tcp"},
		}, wantErr: true},
		{name: "overlapping traffic socket", listeners: []config.QuicListener{
			{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp"},
			{QuicAddr: "127.0.0.1:8444", TrafficAddr: "127.0.0.1:8080", Protocol: "both"},
		}, wantErr: true},
		{name: "QUIC overlaps UDP traffic", listeners: []config.QuicListener{
			{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp"},
			{QuicAddr: "127.0.0.1:8444", TrafficAddr: "127.0.0.1:8443", Protocol: "udp"},
		}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateListeners(test.listeners)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateListeners() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestNewValidatesListenersBeforeCertificates(t *testing.T) {
	_, err := New(&config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    "127.0.0.1:8443",
		TrafficAddr: "127.0.0.1:8080",
		Protocol:    "invalid",
	}}})
	if err == nil || !strings.Contains(err.Error(), "listeners[0].protocol") {
		t.Fatalf("New() error = %v, want listener protocol validation", err)
	}
}

func TestCloneListenersOwnsBooleanPointers(t *testing.T) {
	fragmentation := true
	original := []config.QuicListener{
		{
			QuicAddr: "127.0.0.1:8443",
			UDP: config.UDPConfig{
				EnableFragmentation: &fragmentation,
			},
		},
		{QuicAddr: "127.0.0.1:8444"},
	}

	cloned := cloneListeners(original)
	if cloned[0].UDP.EnableFragmentation == original[0].UDP.EnableFragmentation {
		t.Fatal("clone retained caller-owned boolean pointers")
	}
	if cloned[1].UDP.EnableFragmentation != nil {
		t.Fatal("clone did not preserve nil boolean pointers")
	}

	fragmentation = false
	original[0].QuicAddr = "mutated"
	if !*cloned[0].UDP.EnableFragmentation || cloned[0].QuicAddr != "127.0.0.1:8443" {
		t.Fatal("caller mutation changed cloned listeners")
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
