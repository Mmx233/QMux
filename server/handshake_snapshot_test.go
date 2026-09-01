package server

import (
	"context"
	"testing"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/rs/zerolog"
)

func TestHandshakeTraceStartsAndEndsOnce(t *testing.T) {
	stats := &handshakeStats{}
	trace := stats.tracer(context.Background(), false, quic.ConnectionID{})
	first := trace.AddProducer()
	second := trace.AddProducer()
	if got := stats.snapshot(); got != (HandshakeSnapshot{Current: 1, HighWater: 1}) {
		t.Fatalf("after AddProducer snapshot = %+v", got)
	}

	first.RecordEvent(qlog.ALPNInformation{ChosenALPN: "qmux"})
	if got := stats.snapshot(); got != (HandshakeSnapshot{HighWater: 1}) {
		t.Fatalf("after ALPN snapshot = %+v", got)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if got := stats.snapshot(); got != (HandshakeSnapshot{HighWater: 1}) {
		t.Fatalf("after duplicate terminals snapshot = %+v", got)
	}

	closed := stats.tracer(context.Background(), false, quic.ConnectionID{}).AddProducer()
	if err := closed.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := closed.Close(); err != nil {
		t.Fatalf("duplicate Close() error = %v", err)
	}
	if got := stats.snapshot(); got != (HandshakeSnapshot{HighWater: 1}) {
		t.Fatalf("close-terminal snapshot = %+v", got)
	}
}

func TestRouteSnapshotIncludesHandshakeAndPoolCapacity(t *testing.T) {
	const addr = "route"
	p := pool.New(addr, pool.NewRoundRobinBalancer(), zerolog.Nop())
	defer p.Stop()
	stats := &handshakeStats{}
	producer := stats.tracer(context.Background(), false, quic.ConnectionID{}).AddProducer()
	t.Cleanup(func() {
		if err := producer.Close(); err != nil {
			t.Errorf("Close producer: %v", err)
		}
	})
	pending := p.BeginPending()
	defer p.Abort(pending)

	s := &Server{
		config:     &config.Server{Listeners: []config.QuicListener{{QuicAddr: addr, Protocol: "tcp"}}},
		pools:      map[string]*pool.ConnectionPool{addr: p},
		handshakes: map[string]*handshakeStats{addr: stats},
	}
	route := s.Snapshot().Routes[0]
	if route.Handshake != (HandshakeSnapshot{Current: 1, HighWater: 1}) {
		t.Fatalf("Handshake = %+v", route.Handshake)
	}
	if route.PoolCapacity != (pool.CapacitySnapshot{
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
