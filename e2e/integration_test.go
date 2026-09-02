package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/cmd/generate/certs"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server"
)

// generateTestCertificates generates test certificates for integration tests
func generateTestCertificates(t testing.TB) string {
	t.Helper()
	tempDir := t.TempDir()

	// Generate certificates using the existing logic
	caKey, caCert, err := certs.GenerateCA(1)
	if err != nil {
		t.Fatalf("failed to generate CA: %v", err)
	}

	serverKey, serverCert, err := certs.GenerateServerCert(caKey, caCert, 1, []string{"localhost"})
	if err != nil {
		t.Fatalf("failed to generate server cert: %v", err)
	}

	clientKey, clientCert, err := certs.GenerateClientCert(caKey, caCert, 1)
	if err != nil {
		t.Fatalf("failed to generate client cert: %v", err)
	}

	// Write certificates to files
	certFiles := map[string][]byte{
		"ca.crt":     certs.EncodeCertificate(caCert),
		"ca.key":     certs.EncodePrivateKey(caKey),
		"server.crt": certs.EncodeCertificate(serverCert),
		"server.key": certs.EncodePrivateKey(serverKey),
		"client.crt": certs.EncodeCertificate(clientCert),
		"client.key": certs.EncodePrivateKey(clientKey),
	}

	for name, data := range certFiles {
		path := filepath.Join(tempDir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}

	return tempDir
}

func closeOnCleanup(t testing.TB, closer io.Closer) {
	t.Helper()
	t.Cleanup(func() {
		_ = closer.Close()
	})
}

func serveTCPEcho(listener net.Listener) {
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
}

func serveUDPEcho(conn net.PacketConn, stopOnWriteError bool) {
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := conn.WriteTo(buf[:n], addr); err != nil && stopOnWriteError {
				return
			}
		}
	}()
}

func assertTCPEcho(t testing.TB, addr string, data []byte) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial TCP echo server: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set TCP echo deadline: %v", err)
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write TCP echo request: %v", err)
	}

	buf := make([]byte, len(data))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read TCP echo response: %v", err)
	}
	if !bytes.Equal(buf, data) {
		t.Fatalf("TCP echo data mismatch: got %q, want %q", buf, data)
	}
}

func startTestServer(ctx context.Context, cfg *config.Server) {
	go func() {
		srv, err := server.New(cfg)
		if err == nil {
			_ = srv.Start(ctx)
		}
	}()
}

func startTestClient(ctx context.Context, c *client.Client) {
	go func() {
		_ = c.Start(ctx)
	}()
}

func startTestServerReporting(ctx context.Context, cfg *config.Server) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		srv, err := server.New(cfg)
		if err == nil {
			err = srv.Start(ctx)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
		}
	}()
	return errCh
}

func startTestClientReporting(ctx context.Context, c *client.Client) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		if err := c.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
		}
	}()
	return errCh
}

func startTCPEchoListener(t testing.TB) (net.Listener, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start local TCP echo server: %v", err)
	}
	closeOnCleanup(t, listener)
	serveTCPEcho(listener)
	return listener, listener.Addr().(*net.TCPAddr).Port
}

func startUDPEchoListener(t testing.TB) (net.PacketConn, int) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start local UDP echo server: %v", err)
	}
	closeOnCleanup(t, conn)
	serveUDPEcho(conn, false)
	return conn, conn.LocalAddr().(*net.UDPAddr).Port
}

func newMTLSServerConfig(
	certDir, protocol string,
	quicPort, trafficPort int,
	heartbeatInterval, healthTimeout time.Duration,
) *config.Server {
	return &config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
			TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
			Protocol:    protocol,
		}},
		Auth: config.ServerAuth{
			Method:     "mtls",
			CACertFile: filepath.Join(certDir, "ca.crt"),
		},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
		HeartbeatInterval: heartbeatInterval,
		HealthTimeout:     healthTimeout,
	}
}

func newMTLSClientConfig(
	certDir, clientID string,
	localPort int,
	heartbeatInterval, healthTimeout time.Duration,
	quicPorts ...int,
) *config.Client {
	servers := make([]config.ServerEndpoint, len(quicPorts))
	for i, port := range quicPorts {
		servers[i] = config.ServerEndpoint{
			Address:    fmt.Sprintf("127.0.0.1:%d", port),
			ServerName: "localhost",
		}
	}
	return &config.Client{
		ClientID: clientID,
		Server:   config.ClientServer{Servers: servers},
		Local:    config.LocalService{Host: "127.0.0.1", Port: localPort},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		HeartbeatInterval: heartbeatInterval,
		HealthTimeout:     healthTimeout,
	}
}

func newTestClient(t testing.TB, cfg *config.Client) *client.Client {
	t.Helper()
	c, err := client.New(cfg)
	if err != nil {
		t.Fatalf("create test client %q: %v", cfg.ClientID, err)
	}
	return c
}

func TestTCPBackendDialFailureReturnsNoTunnelBytes(t *testing.T) {
	certDir := generateTestCertificates(t)
	backend, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("reserve unavailable backend port: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	backendPort := backend.Addr().(*net.TCPAddr).Port
	quicPort, trafficPort := getFreePort(t), getFreePort(t)
	trafficAddr := fmt.Sprintf("127.0.0.1:%d", trafficPort)
	testCtx, cancelTest := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelTest()
	timeline := newFaultTimeline(t, "TCP backend dial failure")

	serverRun := startFaultServer(t, testCtx, "dial-failure server",
		newMTLSServerConfig(certDir, "tcp", quicPort, trafficPort, time.Second, 10*time.Second), timeline)
	clientRun := startFaultClient(testCtx, "dial-failure client", newTestClient(t,
		newMTLSClientConfig(certDir, "dial-failure-client", backendPort, time.Second, 0, quicPort)), timeline)
	t.Cleanup(func() {
		if err := clientRun.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop dial-failure client: %v", err)
		}
		if err := serverRun.run.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop dial-failure server: %v", err)
		}
	})

	if err := waitForFault(testCtx, 15*time.Second, func() string {
		return fmt.Sprintf("one ready TCP route; snapshot=%+v", serverRun.Snapshot())
	}, func(time.Duration) bool {
		snapshot := serverRun.Snapshot()
		return len(snapshot.Routes) == 1 && snapshot.Routes[0].TrafficAddr == trafficAddr &&
			snapshot.Routes[0].Ready && snapshot.Routes[0].TCPEligibleClients == 1
	}, serverRun.run, clientRun); err != nil {
		t.Fatal(err)
	}

	if err := backend.Close(); err != nil {
		t.Fatalf("release unavailable backend port: %v", err)
	}
	before := serverRun.Snapshot()
	if len(before.Routes) != 1 || before.Routes[0].TrafficAddr != trafficAddr {
		t.Fatalf("target TCP route before dial = %+v, want %s", before.Routes, trafficAddr)
	}
	targetQuicAddr := before.Routes[0].QuicAddr
	committedBefore := before.Routes[0].TCPAdmission.Committed

	publicConn, err := net.DialTimeout("tcp", trafficAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial public TCP route: %v", err)
	}
	defer func() { _ = publicConn.Close() }()
	if err := publicConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set public TCP read deadline: %v", err)
	}
	var response [1]byte
	n, readErr := publicConn.Read(response[:])
	if n != 0 {
		t.Fatalf("backend dial failure returned %d tunnel bytes, first byte %#x", n, response[0])
	}
	if readErr == nil {
		t.Fatal("backend dial failure returned no bytes without terminating the public read")
	}
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Fatalf("public read timed out after backend dial failure: %v", readErr)
	}
	if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, syscall.ECONNRESET) {
		t.Fatalf("public read error = %T %v, want EOF or ECONNRESET", readErr, readErr)
	}

	if err := waitForFault(testCtx, 5*time.Second, func() string {
		return fmt.Sprintf("dial-failure TCP ownership release; snapshot=%+v", serverRun.Snapshot())
	}, func(time.Duration) bool {
		snapshot := serverRun.Snapshot()
		if len(snapshot.Routes) != 1 {
			return false
		}
		route := snapshot.Routes[0]
		return route.QuicAddr == targetQuicAddr && route.TrafficAddr == trafficAddr &&
			route.TCPAdmission.Committed >= committedBefore+1 &&
			route.TCPAdmission.FlowCurrent == 0 && route.TCPAdmission.SetupCurrent == 0 &&
			route.TCPAdmission.ActiveCurrent == 0 && route.PoolCapacity.TCPPending == 0 &&
			route.PoolCapacity.TCPActive == 0 && route.TCPEligibleClients == 1
	}, serverRun.run, clientRun); err != nil {
		t.Fatal(err)
	}
}

// TestTCPReverseProxy_MTLS tests TCP reverse proxy functionality with mTLS authentication
func TestTCPReverseProxy_MTLS(t *testing.T) {
	certDir := generateTestCertificates(t)

	localListener, localPort := startTCPEchoListener(t)
	t.Logf("Local echo server listening on %s", localListener.Addr())

	// Start QMux server
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	serverConfig := newMTLSServerConfig(certDir, "tcp", quicPort, trafficPort, time.Second, 3*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start server
	serverErrCh := startTestServerReporting(ctx, serverConfig)

	// Give server time to start
	time.Sleep(500 * time.Millisecond)

	// Start QMux client
	clientConfig := newMTLSClientConfig(certDir, "test-client", localPort, time.Second, 0, quicPort)
	c := newTestClient(t, clientConfig)

	clientErrCh := startTestClientReporting(ctx, c)

	// Give client time to connect
	time.Sleep(500 * time.Millisecond)

	// Test TCP connection through tunnel
	testData := []string{
		"Hello, World!",
		"This is a test",
		strings.Repeat("A", 1024),  // 1KB
		strings.Repeat("B", 10240), // 10KB
	}

	for i, data := range testData {
		t.Run(fmt.Sprintf("Message_%d", i), func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
			if err != nil {
				t.Fatalf("failed to connect to traffic port: %v", err)
			}
			closeOnCleanup(t, conn)

			// Set deadline
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatalf("set deadline: %v", err)
			}

			// Send data
			n, err := conn.Write([]byte(data))
			if err != nil {
				t.Fatalf("failed to write: %v", err)
			}
			if n != len(data) {
				t.Fatalf("wrote %d bytes, expected %d", n, len(data))
			}

			// Read echo
			buf := make([]byte, len(data))
			n, err = io.ReadFull(conn, buf)
			if err != nil {
				t.Fatalf("failed to read: %v", err)
			}
			if n != len(data) {
				t.Fatalf("read %d bytes, expected %d", n, len(data))
			}

			// Verify data
			if string(buf) != data {
				t.Fatalf("data mismatch: got %d bytes, expected %d bytes", len(buf), len(data))
			}
		})
	}

	// Check for errors
	select {
	case err := <-serverErrCh:
		t.Fatalf("server error: %v", err)
	case err := <-clientErrCh:
		t.Fatalf("client error: %v", err)
	default:
	}
}

// TestUDPReverseProxy_MTLS tests UDP reverse proxy functionality with mTLS authentication
func TestUDPReverseProxy_MTLS(t *testing.T) {
	certDir := generateTestCertificates(t)

	localConn, localPort := startUDPEchoListener(t)
	t.Logf("Local UDP echo server listening on %s", localConn.LocalAddr())

	// Start QMux server
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	serverConfig := newMTLSServerConfig(certDir, "udp", quicPort, trafficPort, time.Second, 3*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start server
	serverErrCh := startTestServerReporting(ctx, serverConfig)

	time.Sleep(500 * time.Millisecond)

	// Start QMux client
	clientConfig := newMTLSClientConfig(certDir, "test-client-udp", localPort, time.Second, 0, quicPort)
	c := newTestClient(t, clientConfig)

	clientErrCh := startTestClientReporting(ctx, c)

	time.Sleep(500 * time.Millisecond)

	// Test UDP through tunnel
	testData := [][]byte{
		[]byte("UDP Test 1"),
		[]byte("UDP Test 2"),
		[]byte(strings.Repeat("U", 512)),
		{0x01, 0x02, 0x80, 0x00, 0x02, 0x03},
		bytes.Repeat([]byte{0x00, 0xff, 0x80, 0x21}, 512),
	}

	for i, data := range testData {
		t.Run(fmt.Sprintf("UDPMessage_%d", i), func(t *testing.T) {
			conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
			if err != nil {
				t.Fatalf("failed to dial UDP: %v", err)
			}
			closeOnCleanup(t, conn)

			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatalf("set deadline: %v", err)
			}

			// Send data
			_, err = conn.Write(data)
			if err != nil {
				t.Fatalf("failed to write UDP: %v", err)
			}

			// Read echo
			buf := make([]byte, 65535)
			n, err := conn.Read(buf)
			if err != nil {
				t.Fatalf("failed to read UDP: %v", err)
			}

			if !bytes.Equal(buf[:n], data) {
				t.Fatalf("UDP data mismatch: got % x, expected % x", buf[:n], data)
			}
		})
	}

	// Check for errors
	select {
	case err := <-serverErrCh:
		t.Fatalf("server error: %v", err)
	case err := <-clientErrCh:
		t.Fatalf("client error: %v", err)
	default:
	}
}

// TestConcurrentConnections_MTLS tests multiple concurrent connections with mTLS authentication
func TestConcurrentConnections_MTLS(t *testing.T) {
	certDir := generateTestCertificates(t)

	_, localPort := startTCPEchoListener(t)

	// Start QMux infrastructure
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	serverConfig := newMTLSServerConfig(certDir, "tcp", quicPort, trafficPort, 0, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	startTestServer(ctx, serverConfig)
	time.Sleep(500 * time.Millisecond)

	clientConfig := newMTLSClientConfig(certDir, "test-client-concurrent", localPort, 0, 0, quicPort)
	c := newTestClient(t, clientConfig)

	startTestClient(ctx, c)
	time.Sleep(500 * time.Millisecond)

	// Test 10 concurrent connections
	var wg sync.WaitGroup
	errCh := make(chan error, 10)

	for i := range 10 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
			if err != nil {
				errCh <- fmt.Errorf("conn %d: dial failed: %w", id, err)
				return
			}
			defer func() { _ = conn.Close() }()

			data := fmt.Sprintf("Connection %d: %s", id, strings.Repeat("X", 100))
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				errCh <- fmt.Errorf("conn %d: set deadline: %w", id, err)
				return
			}

			if _, err := conn.Write([]byte(data)); err != nil {
				errCh <- fmt.Errorf("conn %d: write failed: %w", id, err)
				return
			}

			buf := make([]byte, len(data))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errCh <- fmt.Errorf("conn %d: read failed: %w", id, err)
				return
			}

			if string(buf) != data {
				errCh <- fmt.Errorf("conn %d: data mismatch", id)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}
}

// TestClientReconnection_MTLS tests client reconnection and failover with mTLS authentication
func TestClientReconnection_MTLS(t *testing.T) {
	certDir := generateTestCertificates(t)
	_, localPort := startTCPEchoListener(t)
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)
	serverConfig := newMTLSServerConfig(certDir, "tcp", quicPort, trafficPort, 500*time.Millisecond, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	timeline := newFaultTimeline(t, "same-ID reconnect")
	serverRun := startFaultServer(t, ctx, "same-ID server", serverConfig, timeline)
	defer func() {
		if err := serverRun.run.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop same-ID server: %v", err)
		}
	}()

	clientConfig := newMTLSClientConfig(certDir, "test-client-reconnect", localPort, 500*time.Millisecond, 0, quicPort)
	c1 := newTestClient(t, clientConfig)
	c1Run := startFaultClient(ctx, "same-ID client generation 1", c1, timeline)
	defer func() {
		if err := c1Run.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop same-ID client generation 1: %v", err)
		}
	}()

	trafficAddr := fmt.Sprintf("127.0.0.1:%d", trafficPort)
	waitEligibleAndEcho := func(sequence uint64, runs ...*faultRun) {
		t.Helper()
		if err := waitForFault(ctx, 10*time.Second, func() string {
			return fmt.Sprintf("one TCP-eligible client and echo; snapshot=%+v", serverRun.Snapshot())
		}, func(remaining time.Duration) bool {
			snapshot := serverRun.Snapshot()
			return len(snapshot.Routes) == 1 && snapshot.Routes[0].TCPEligibleClients == 1 &&
				remaining > 0 && probeSequencedTCP(trafficAddr, sequence, min(250*time.Millisecond, remaining)) == nil
		}, runs...); err != nil {
			t.Fatalf("same-ID generation did not become usable: %v", err)
		}
		phase := "recovery"
		if sequence == 1 {
			phase = "baseline"
		}
		timeline.add("same-ID %s: snapshot TCP eligible=1, echo %d ok", phase, sequence)
	}
	waitEligibleAndEcho(1, serverRun.run, c1Run)

	timeline.add("same-ID fault injection: cancel generation 1")
	c1Run.cancel()
	if err := c1Run.join(5 * time.Second); err != nil {
		t.Fatalf("join same-ID client generation 1: %v", err)
	}
	if err := waitForFault(ctx, 5*time.Second, func() string {
		return fmt.Sprintf("zero eligible clients after generation 1 exit; snapshot=%+v", serverRun.Snapshot())
	}, func(time.Duration) bool {
		snapshot := serverRun.Snapshot()
		return len(snapshot.Routes) == 1 && snapshot.Routes[0].TCPEligibleClients == 0
	}, serverRun.run); err != nil {
		t.Fatalf("same-ID generation 1 was not retired: %v", err)
	}
	timeline.add("same-ID detection: snapshot TCP eligible transitioned 1->0")

	c2 := newTestClient(t, clientConfig)
	c2Run := startFaultClient(ctx, "same-ID client generation 2", c2, timeline)
	defer func() {
		if err := c2Run.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop same-ID client generation 2: %v", err)
		}
	}()
	waitEligibleAndEcho(2, serverRun.run, c2Run)
}

// getFreePort gets a free port for testing
func getFreePort(t testing.TB) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("close free-port listener: %v", err)
	}
	return port
}

// TestMultiServerFailover_MTLS tests client failover when one server goes down
func TestMultiServerFailover_MTLS(t *testing.T) {
	certDir := generateTestCertificates(t)

	_, localPort := startTCPEchoListener(t)

	// Start two QMux servers
	quicPort1 := getFreePort(t)
	trafficPort1 := getFreePort(t)
	quicPort2 := getFreePort(t)
	trafficPort2 := getFreePort(t)

	serverConfig1 := newMTLSServerConfig(certDir, "tcp", quicPort1, trafficPort1, 500*time.Millisecond, time.Second)
	serverConfig2 := newMTLSServerConfig(certDir, "tcp", quicPort2, trafficPort2, 500*time.Millisecond, time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Start server 1 with its own context so we can cancel it
	server1Ctx, server1Cancel := context.WithCancel(ctx)
	startTestServer(server1Ctx, serverConfig1)

	// Start server 2
	startTestServer(ctx, serverConfig2)
	time.Sleep(500 * time.Millisecond)

	t.Logf("Server 1 listening on QUIC port %d, traffic port %d", quicPort1, trafficPort1)
	t.Logf("Server 2 listening on QUIC port %d, traffic port %d", quicPort2, trafficPort2)

	// Start client with multi-server configuration
	clientConfig := newMTLSClientConfig(certDir, "test-client-failover", localPort, 500*time.Millisecond, 0, quicPort1, quicPort2)
	c := newTestClient(t, clientConfig)

	startTestClient(ctx, c)
	time.Sleep(1 * time.Second)

	// Verify both connections are healthy
	if c.HealthyConnectionCount() != 2 {
		t.Fatalf("expected 2 healthy connections initially, got %d", c.HealthyConnectionCount())
	}
	t.Log("Both servers connected and healthy")

	// Verify both server paths before exercising failover.
	for i, trafficPort := range []int{trafficPort1, trafficPort2} {
		assertTCPEcho(t, fmt.Sprintf("127.0.0.1:%d", trafficPort), []byte(fmt.Sprintf("Before failover %d", i+1)))
	}

	// Shutdown server 1
	t.Log("Shutting down server 1...")
	server1Cancel()
	time.Sleep(2 * time.Second) // Wait for health check to detect failure

	// Verify server 2 is still healthy (client should have detected server 1 failure)
	healthyCount := c.HealthyConnectionCount()
	t.Logf("Healthy connections after server 1 shutdown: %d", healthyCount)

	// Traffic through server 2 should still work
	assertTCPEcho(t, fmt.Sprintf("127.0.0.1:%d", trafficPort2), []byte("After failover"))

	t.Log("Failover test successful - traffic continues through remaining server")
}
