package run

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
			identityNotAfter := time.Date(2500, time.January, 1, 0, 0, 0, 0, time.UTC)
			caNotAfter := time.Date(2501, time.January, 1, 0, 0, 0, 0, time.UTC)
			traffic := stats.TransportSnapshot{
				SentBytes: 42, ReceivedBytes: 84,
				SentPackets: 7, ReceivedPackets: 14,
				LostBytes: 21, LostPackets: 3,
				Connections:    2,
				SmoothedRTTSum: 60 * time.Millisecond, SmoothedRTTMax: 40 * time.Millisecond,
				RTTDeviationMax: 5 * time.Millisecond,
			}
			var collector prometheus.Collector
			var labels string
			if role == "server" {
				collector = newServerCollector(func() server.Snapshot {
					reads.Add(1)
					return server.Snapshot{TLSCertificateNotAfter: identityNotAfter, TLSCANotAfter: caNotAfter, Routes: []server.RouteSnapshot{{QuicAddr: ":8443", TrafficAddr: ":8080", Protocol: "both", PoolCapacity: pool.CapacitySnapshot{QUIC: traffic}}}}
				})
				labels = `{listener=":8443"}`
			} else {
				collector = newClientCollector(func() client.Snapshot {
					reads.Add(1)
					return client.Snapshot{
						TLSCertificateNotAfter: identityNotAfter,
						TLSCANotAfter:          caNotAfter,
						Endpoints:              []client.EndpointSnapshot{{Endpoint: "server:8443", QUIC: traffic}},
						UDPSessions:            client.UDPSessionSnapshot{PendingDrops: 17},
					}
				})
				labels = `{endpoint="server:8443"}`
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
				for name, value := range map[string]string{
					"sent_bytes_total":          "42",
					"received_bytes_total":      "84",
					"sent_packets_total":        "7",
					"received_packets_total":    "14",
					"lost_bytes":                "21",
					"lost_packets":              "3",
					"connections":               "2",
					"smoothed_rtt_seconds":      "0.03",
					"smoothed_rtt_max_seconds":  "0.04",
					"rtt_deviation_max_seconds": "0.005",
				} {
					want := "\nqmux_" + role + "_quic_" + name + labels + " " + value + "\n"
					if !strings.Contains(response.Body.String(), want) {
						t.Errorf("missing sample %q", want)
					}
				}
				for _, part := range []string{"# TYPE qmux_" + role + "_quic_lost_packets gauge\n", "# TYPE qmux_" + role + "_registration_duration_seconds histogram\n", "# TYPE go_goroutines gauge\n"} {
					if !strings.Contains(response.Body.String(), part) {
						t.Errorf("missing %q", part)
					}
				}
				if role == "client" {
					want := "\nqmux_client_udp_drops_total{reason=\"pending_setup\"} 17\n"
					if !strings.Contains(response.Body.String(), want) {
						t.Errorf("missing sample %q", want)
					}
				}
				for kind, notAfter := range map[string]time.Time{"identity": identityNotAfter, "ca": caNotAfter} {
					value := strconv.FormatFloat(float64(notAfter.Unix()), 'g', -1, 64)
					want := fmt.Sprintf("\nqmux_%s_tls_certificate_not_after_timestamp_seconds{kind=%q} %s\n", role, kind, value)
					if !strings.Contains(response.Body.String(), want) {
						t.Errorf("missing sample %q", want)
					}
				}
			}
			if reads.Load() != 2 {
				t.Fatalf("snapshot reads=%d, want one per scrape", reads.Load())
			}
		})
	}
}

func TestTLSCertificateMetricsOmitMissingKinds(t *testing.T) {
	notAfter := time.Unix(42, 0)
	tests := []struct {
		name      string
		role      string
		collector prometheus.Collector
		present   string
		absent    string
	}{
		{
			name: "server identity only", role: "server",
			collector: newServerCollector(func() server.Snapshot {
				return server.Snapshot{TLSCertificateNotAfter: notAfter}
			}),
			present: "identity", absent: "ca",
		},
		{
			name: "client CA only", role: "client",
			collector: newClientCollector(func() client.Snapshot {
				return client.Snapshot{TLSCANotAfter: notAfter}
			}),
			present: "ca", absent: "identity",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			newAdminHandler(func() bool { return true }, test.collector).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			metric := "qmux_" + test.role + "_tls_certificate_not_after_timestamp_seconds"
			if sample := metric + `{kind="` + test.present + `"} 42`; !strings.Contains(response.Body.String(), sample) {
				t.Errorf("missing sample %q", sample)
			}
			if sample := metric + `{kind="` + test.absent + `"}`; strings.Contains(response.Body.String(), sample) {
				t.Errorf("unexpected sample %q", sample)
			}
		})
	}
}

func TestAdminMetricsRegistryIsolation(t *testing.T) {
	handlers := make(map[string]http.Handler)
	samples := make(map[string]string)
	for i, scope := range []string{"first", "second", "global"} {
		metric := prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "qmux_test_admin_isolation",
			Help:        "Test marker for admin registry isolation.",
			ConstLabels: prometheus.Labels{"scope": scope},
		})
		metric.Set(float64(i + 1))
		samples[scope] = fmt.Sprintf("\nqmux_test_admin_isolation{scope=%q} %d\n", scope, i+1)
		if scope == "global" {
			if err := prometheus.Register(metric); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { prometheus.Unregister(metric) })
		} else {
			handlers[scope] = newAdminHandler(func() bool { return true }, metric)
		}
	}
	for scope, handler := range handlers {
		t.Run(scope, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("scrape status=%d: %s", response.Code, response.Body.String())
			}
			for source, sample := range samples {
				if got, want := strings.Contains(response.Body.String(), sample), source == scope; got != want {
					t.Errorf("sample %q present=%t, want %t", sample, got, want)
				}
			}
		})
	}
}
