package run

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/internal/stats"
	"github.com/Mmx233/QMux/server"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/prometheus/client_golang/prometheus"
)

func TestSnapshotMetrics(t *testing.T) {
	for _, role := range []string{"server", "client"} {
		t.Run(role, func(t *testing.T) {
			var reads atomic.Int64
			traffic := stats.TransportSnapshot{SentBytes: 42, LostPackets: 3}
			var collector prometheus.Collector
			var want string
			if role == "server" {
				collector = newServerCollector(func() server.Snapshot {
					reads.Add(1)
					return server.Snapshot{Routes: []server.RouteSnapshot{{QuicAddr: ":8443", TrafficAddr: ":8080", Protocol: "both", PoolCapacity: pool.CapacitySnapshot{QUIC: traffic}}}}
				})
				want = `qmux_server_quic_sent_bytes_total{listener=":8443"} 42`
			} else {
				collector = newClientCollector(func() client.Snapshot {
					reads.Add(1)
					return client.Snapshot{Endpoints: []client.EndpointSnapshot{{Endpoint: "server:8443", QUIC: traffic}}}
				})
				want = `qmux_client_quic_sent_bytes_total{endpoint="server:8443"} 42`
			}
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(collector)
			if _, err := registry.Gather(); err != nil {
				t.Fatal(err)
			}
			reads.Store(0)
			handler := newAdminHandler(func() bool { return false }, collector)
			for _, path := range []string{"/healthz", "/readyz"} {
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
			}
			if reads.Load() != 0 {
				t.Fatal("health probes collected the full snapshot")
			}
			for range 2 {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				if response.Code != http.StatusOK {
					t.Fatalf("scrape status=%d: %s", response.Code, response.Body.String())
				}
				for _, part := range []string{want, "# TYPE qmux_" + role + "_quic_lost_packets gauge\n", "# TYPE qmux_" + role + "_registration_duration_seconds histogram\n", "# TYPE go_goroutines gauge\n"} {
					if !strings.Contains(response.Body.String(), part) {
						t.Errorf("missing %q", part)
					}
				}
			}
			if reads.Load() != 2 {
				t.Fatalf("snapshot reads=%d, want one per scrape", reads.Load())
			}
			// A second admin can use the same names without global registration conflicts.
			other := newAdminHandler(func() bool { return true }, collector)
			response := httptest.NewRecorder()
			other.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("second admin failed: %s", response.Body.String())
			}
		})
	}
}
