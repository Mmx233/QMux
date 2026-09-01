package server

import (
	"context"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/auth/mtls"
	"github.com/quic-go/quic-go"
)

// The 1 KiB stream window leaves registration Ack headroom, while the 1 MiB
// connection window keeps the stall isolated to stream flow control.
const heartbeatTestStreamWindow = 1024

func TestHeartbeatWriteStallRetiresExactServerGeneration(t *testing.T) {
	clientCertificate, clientRoots := registrationTestClientCertificate(
		t,
		"heartbeat-deadline-client",
		false,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	)
	serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
	harness := newRegistrationHarnessWithTLSAndQUIC(
		t,
		mtls.New(clientRoots),
		time.Second,
		serverTLS,
		clientTLS,
		&config.Server{
			HeartbeatInterval: 5 * time.Millisecond,
			HealthTimeout:     5 * time.Second,
		},
		&quic.Config{MaxIdleTimeout: 10 * time.Second},
		&quic.Config{
			MaxIdleTimeout:                 10 * time.Second,
			InitialStreamReceiveWindow:     heartbeatTestStreamWindow,
			MaxStreamReceiveWindow:         heartbeatTestStreamWindow,
			InitialConnectionReceiveWindow: 1 << 20,
			MaxConnectionReceiveWindow:     1 << 20,
		},
	)

	const clientID = "heartbeat-write-stall"
	stream := registerMTLSClient(t, harness, clientID)
	stale, ok := harness.pool.Get(clientID)
	if !ok {
		t.Fatal("registered generation missing from pool")
	}

	heartbeatSenderDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-harness.client.Context().Done():
				heartbeatSenderDone <- nil
				return
			case <-ticker.C:
				if err := protocol.WriteHeartbeat(stream, time.Now().Unix()); err != nil {
					select {
					case <-harness.client.Context().Done():
						heartbeatSenderDone <- nil
					case <-time.After(time.Second):
						heartbeatSenderDone <- err
					}
					return
				}
			}
		}
	}()

	harness.waitForHandler(t)
	if err := <-heartbeatSenderDone; err != nil {
		t.Fatalf("client heartbeat sender: %v", err)
	}
	if got := harness.pool.Count(); got != 0 {
		t.Fatalf("pool Count() after heartbeat write failure = %d, want 0", got)
	}
	var applicationError *quic.ApplicationError
	if err := context.Cause(harness.client.Context()); !errors.As(err, &applicationError) || applicationError.ErrorCode != 1 ||
		applicationError.ErrorMessage != "heartbeat write failed" {
		t.Fatalf("client close cause = %T %v, want heartbeat write application close", err, err)
	}

	harness.reconnect(t, clientTLS, &quic.Config{
		MaxIdleTimeout:                 10 * time.Second,
		InitialStreamReceiveWindow:     heartbeatTestStreamWindow,
		MaxStreamReceiveWindow:         heartbeatTestStreamWindow,
		InitialConnectionReceiveWindow: 1 << 20,
		MaxConnectionReceiveWindow:     1 << 20,
	})
	freshStream := registerMTLSClient(t, harness, clientID)
	fresh, ok := harness.pool.Get(clientID)
	if !ok || fresh == stale {
		t.Fatalf("fresh generation = (%p, %t), stale=%p", fresh, ok, stale)
	}

	const heartbeatCount = 6
	serverHeartbeats := make(chan error, 1)
	go func() {
		for range heartbeatCount {
			var heartbeat protocol.HeartbeatMsg
			if err := protocol.ReadTypedMessage(freshStream, protocol.MsgTypeHeartbeat, &heartbeat); err != nil {
				serverHeartbeats <- err
				return
			}
		}
		serverHeartbeats <- nil
	}()
	for range heartbeatCount {
		if err := protocol.WriteHeartbeat(freshStream, time.Now().Unix()); err != nil {
			t.Fatalf("send fresh client heartbeat: %v", err)
		}
	}
	if err := <-serverHeartbeats; err != nil {
		t.Fatalf("read fresh server heartbeats: %v", err)
	}
	if harness.pool.MarkUnhealthy(stale) || harness.pool.Remove(stale) {
		t.Fatal("stale heartbeat generation mutated its replacement")
	}
	current, ok := harness.pool.Get(clientID)
	if !ok || current != fresh || harness.pool.HealthyCount() != 1 {
		t.Fatalf("replacement generation = (%p, %t, healthy=%d), want (%p, true, 1)",
			current, ok, harness.pool.HealthyCount(), fresh)
	}
	select {
	case <-harness.client.Context().Done():
		t.Fatalf("fresh heartbeat generation closed early: %v", context.Cause(harness.client.Context()))
	default:
	}
	if err := harness.client.CloseWithError(0, "fresh generation complete"); err != nil {
		t.Fatalf("close fresh client: %v", err)
	}
	harness.waitForHandler(t)
}
