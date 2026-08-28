package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
)

func TestBidirectionalHeartbeatsKeepExactGenerationHealthy(t *testing.T) {
	const (
		heartbeatInterval = 20 * time.Millisecond
		healthTimeout     = 400 * time.Millisecond
	)
	certDir := generateTestCertificates(t)
	_, localPort := startTCPEchoListener(t)
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverErr := startTestServerReporting(ctx, newMTLSServerConfig(
		certDir,
		"tcp",
		quicPort,
		trafficPort,
		heartbeatInterval,
		healthTimeout,
	))
	clientConfig := newMTLSClientConfig(
		certDir,
		"lif006-sustained-heartbeats",
		localPort,
		heartbeatInterval,
		healthTimeout,
		quicPort,
	)
	waitForQUICListener(t, ctx, clientConfig, serverErr)

	clientInstance := newTestClient(t, clientConfig)
	clientDone := make(chan error, 1)
	go func() { clientDone <- clientInstance.Start(ctx) }()
	trafficAddress := fmt.Sprintf("127.0.0.1:%d", trafficPort)
	assertTCPEchoEventually(
		t,
		ctx,
		trafficAddress,
		[]byte("heartbeat generation before sustained observation"),
		clientDone,
		serverErr,
	)

	serverAddress := clientConfig.Server.Servers[0].Address
	manager := clientInstance.ConnectionManager()
	expected := manager.GetConnection(serverAddress)
	if expected == nil {
		t.Fatal("registered heartbeat generation was not published")
	}
	assertExactGeneration := func() {
		t.Helper()
		if current := manager.GetConnection(serverAddress); current != expected {
			t.Fatalf("current heartbeat generation = %p, want exact %p", current, expected)
		}
		if !expected.IsHealthy() || expected.Connection() == nil {
			t.Fatalf("exact heartbeat generation healthy=%t connection=%p, want true/non-nil",
				expected.IsHealthy(), expected.Connection())
		}
		if healthy, total := clientInstance.HealthyConnectionCount(), clientInstance.TotalConnectionCount(); healthy != 1 || total != 1 {
			t.Fatalf("heartbeat connection counts = healthy %d, total %d; want 1, 1", healthy, total)
		}
	}
	assertExactGeneration()

	observation := time.NewTimer(2*healthTimeout + 2*heartbeatInterval)
	defer observation.Stop()
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
observe:
	for {
		select {
		case err := <-clientDone:
			t.Fatalf("client returned during sustained heartbeats: %v", err)
		case err := <-serverErr:
			t.Fatalf("server returned during sustained heartbeats: %v", err)
		case <-ticker.C:
			assertExactGeneration()
		case <-observation.C:
			break observe
		case <-ctx.Done():
			t.Fatalf("heartbeat observation context ended: %v", context.Cause(ctx))
		}
	}
	assertExactGeneration()
	assertTCPEchoEventually(
		t,
		ctx,
		trafficAddress,
		[]byte("same heartbeat generation after sustained observation"),
		clientDone,
		serverErr,
	)
	assertExactGeneration()

	cancel()
	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatalf("client shutdown after sustained heartbeats failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not stop after sustained heartbeat observation")
	}
}

func TestHealthyEndpointPublishesWhileRegistrationPeerStalls(t *testing.T) {
	certDir := generateTestCertificates(t)
	_, localPort := startTCPEchoListener(t)
	healthyQUICPort := getFreePort(t)
	trafficPort := getFreePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverErr := startTestServerReporting(ctx, newMTLSServerConfig(
		certDir,
		"tcp",
		healthyQUICPort,
		trafficPort,
		20*time.Millisecond,
		500*time.Millisecond,
	))

	clientConfig := newMTLSClientConfig(
		certDir,
		"lif002-independent-publication",
		localPort,
		20*time.Millisecond,
		500*time.Millisecond,
		healthyQUICPort,
	)
	waitForQUICListener(t, ctx, clientConfig, serverErr)

	stalledAddr, stalledRegistration, stalledErr := startStalledRegistrationPeer(t, certDir)
	clientConfig.Server.Servers = append(clientConfig.Server.Servers, config.ServerEndpoint{
		Address:    stalledAddr,
		ServerName: "localhost",
	})

	clientInstance := newTestClient(t, clientConfig)
	clientDone := make(chan error, 1)
	go func() {
		clientDone <- clientInstance.Start(ctx)
	}()

	select {
	case <-stalledRegistration:
	case err := <-stalledErr:
		t.Fatalf("stalled peer failed before registration: %v", err)
	case err := <-serverErr:
		t.Fatalf("healthy server failed: %v", err)
	case err := <-clientDone:
		t.Fatalf("client returned before stalled registration was released: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("stalled peer did not receive registration")
	}

	payload := []byte("healthy endpoint carried bytes before the stalled attempt completed")
	assertTCPEchoEventually(t, ctx, fmt.Sprintf("127.0.0.1:%d", trafficPort), payload, clientDone, serverErr)

	select {
	case err := <-clientDone:
		t.Fatalf("Client.Start returned before its bounded stalled attempt completed: %v", err)
	default:
	}

	cancel()
	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatalf("client shutdown failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not stop after caller cancellation")
	}
}

func TestClientStopWithLiveBackgroundStart(t *testing.T) {
	certDir := generateTestCertificates(t)
	_, localPort := startTCPEchoListener(t)
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	serverCtx := t.Context()
	serverErr := startTestServerReporting(serverCtx, newMTLSServerConfig(
		certDir,
		"tcp",
		quicPort,
		trafficPort,
		20*time.Millisecond,
		500*time.Millisecond,
	))
	clientConfig := newMTLSClientConfig(
		certDir,
		"lif004-background-stop",
		localPort,
		20*time.Millisecond,
		500*time.Millisecond,
		quicPort,
	)
	waitForQUICListener(t, serverCtx, clientConfig, serverErr)

	clientInstance := newTestClient(t, clientConfig)
	clientDone := make(chan error, 1)
	go func() {
		clientDone <- clientInstance.Start(context.Background())
	}()
	assertTCPEchoEventually(
		t,
		serverCtx,
		fmt.Sprintf("127.0.0.1:%d", trafficPort),
		[]byte("live client stopped without caller cancellation"),
		clientDone,
		serverErr,
	)
	if got := clientInstance.HealthyConnectionCount(); got != 1 {
		t.Fatalf("healthy connections before Stop = %d, want 1", got)
	}
	select {
	case err := <-clientDone:
		t.Fatalf("Client.Start returned before Stop: %v", err)
	default:
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- clientInstance.Stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Client.Stop did not return with a live caller context")
	}
	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatalf("Start returned error after Stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Client.Start did not return after Stop")
	}
	if got := clientInstance.TotalConnectionCount(); got != 0 {
		t.Fatalf("connections after Stop = %d, want 0", got)
	}
}

func waitForQUICListener(
	t *testing.T,
	ctx context.Context,
	clientConfig *config.Client,
	serverErr <-chan error,
) {
	t.Helper()
	if err := clientConfig.TLS.LoadCertificates(); err != nil {
		t.Fatalf("load probe certificates: %v", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{clientConfig.TLS.ClientCert},
		RootCAs:      clientConfig.TLS.CACertPool,
		ServerName:   clientConfig.Server.Servers[0].ServerName,
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, 200*time.Millisecond)
		conn, err := quic.DialAddr(
			attemptCtx,
			clientConfig.Server.Servers[0].Address,
			tlsConfig,
			clientConfig.Quic.GetConfig(),
		)
		cancelAttempt()
		if err == nil {
			_ = conn.CloseWithError(0, "readiness probe complete")
			return
		}

		select {
		case serverStartErr := <-serverErr:
			t.Fatalf("healthy server failed during readiness probe: %v", serverStartErr)
		case <-deadline.C:
			t.Fatalf("healthy QUIC listener did not become ready: %v", err)
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("readiness context ended: %v", context.Cause(ctx))
		}
	}
}

func startStalledRegistrationPeer(t *testing.T, certDir string) (string, <-chan struct{}, <-chan error) {
	t.Helper()
	serverCertificate, err := tls.LoadX509KeyPair(
		filepath.Join(certDir, "server.crt"),
		filepath.Join(certDir, "server.key"),
	)
	if err != nil {
		t.Fatalf("load stalled peer certificate: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(certDir, "ca.crt"))
	if err != nil {
		t.Fatalf("read stalled peer CA: %v", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		t.Fatal("parse stalled peer CA")
	}

	listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}, config.Quic{}.GetConfig())
	if err != nil {
		t.Fatalf("listen for stalled QUIC registration: %v", err)
	}

	peerCtx, cancelPeer := context.WithCancel(context.Background())
	registered := make(chan struct{})
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	var connMu sync.Mutex
	var acceptedConn *quic.Conn
	wg.Go(func() {
		conn, acceptErr := listener.Accept(peerCtx)
		if acceptErr != nil {
			if peerCtx.Err() == nil {
				errCh <- acceptErr
			}
			return
		}
		connMu.Lock()
		acceptedConn = conn
		connMu.Unlock()

		stream, streamErr := conn.AcceptStream(peerCtx)
		if streamErr != nil {
			if peerCtx.Err() == nil {
				errCh <- streamErr
			}
			return
		}
		var registration protocol.RegisterMsg
		if readErr := protocol.ReadTypedMessage(stream, protocol.MsgTypeRegister, &registration); readErr != nil {
			if peerCtx.Err() == nil {
				errCh <- readErr
			}
			return
		}
		close(registered)
		select {
		case <-conn.Context().Done():
		case <-peerCtx.Done():
		}
	})

	t.Cleanup(func() {
		cancelPeer()
		_ = listener.Close()
		connMu.Lock()
		if acceptedConn != nil {
			_ = acceptedConn.CloseWithError(0, "test cleanup")
		}
		connMu.Unlock()
		wg.Wait()
	})

	return listener.Addr().String(), registered, errCh
}

func assertTCPEchoEventually(
	t *testing.T,
	ctx context.Context,
	address string,
	payload []byte,
	clientDone <-chan error,
	serverErr <-chan error,
) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
			if _, err = conn.Write(payload); err == nil {
				response := make([]byte, len(payload))
				_, err = io.ReadFull(conn, response)
				if err == nil && bytes.Equal(response, payload) {
					_ = conn.Close()
					return
				}
			}
			_ = conn.Close()
		}
		lastErr = err

		select {
		case err := <-clientDone:
			t.Fatalf("client returned before healthy endpoint carried traffic: %v", err)
		case err := <-serverErr:
			t.Fatalf("healthy server failed before carrying traffic: %v", err)
		case <-deadline.C:
			t.Fatalf("healthy endpoint did not echo within candidate bound: %v", lastErr)
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("echo context ended: %v", context.Cause(ctx))
		}
	}
}
