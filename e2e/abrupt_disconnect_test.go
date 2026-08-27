package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server"
	"gopkg.in/yaml.v3"
)

// TestClientAbruptDisconnect_MTLS tests server behavior when a client disconnects abruptly
// without proper handshake, with heartbeat disabled, under different load balancing algorithms
func TestClientAbruptDisconnect_MTLS(t *testing.T) {
	t.Run("least-connections", func(t *testing.T) {
		testClientAbruptDisconnect(t, "least-connections")
	})

	t.Run("round-robin", func(t *testing.T) {
		testClientAbruptDisconnect(t, "round-robin")
	})
}

// testClientAbruptDisconnect is the core test function for abrupt disconnect scenarios
func testClientAbruptDisconnect(t *testing.T, loadBalancer string) {
	certDir := generateTestCertificates(t)

	// Start local TCP echo server
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local server: %v", err)
	}
	closeOnCleanup(t, localListener)

	localAddr := localListener.Addr().(*net.TCPAddr)
	t.Logf("Local echo server listening on %s", localAddr)

	serveTCPEcho(localListener)

	// Get free ports for QMux server
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	// Configure server with default heartbeat settings and specified load balancer
	serverConfig := &config.Server{
		Listeners: []config.QuicListener{
			{
				QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
				TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method:     "mtls",
			CACertFile: filepath.Join(certDir, "ca.crt"),
		},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
		LoadBalancer: loadBalancer,
		// Use default HeartbeatInterval (10s) and HealthTimeout (30s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Start server
	go func() {
		if err := server.Start(ctx, serverConfig); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("server error: %v", err)
		}
	}()
	time.Sleep(500 * time.Millisecond)

	t.Logf("Server started with %s load balancer, QUIC port %d, traffic port %d", loadBalancer, quicPort, trafficPort)

	// Create client config file for client 1 (will be started via exec)
	client1ConfigPath := filepath.Join(certDir, "client1.yaml")
	client1Config := map[string]any{
		"client_id": "client-1",
		"server": map[string]any{
			"servers": []map[string]any{
				{"address": fmt.Sprintf("127.0.0.1:%d", quicPort), "server_name": "localhost"},
			},
		},
		"local": map[string]any{
			"host": "127.0.0.1",
			"port": localAddr.Port,
		},
		"tls": map[string]any{
			"ca_cert_file":     filepath.Join(certDir, "ca.crt"),
			"client_cert_file": filepath.Join(certDir, "client.crt"),
			"client_key_file":  filepath.Join(certDir, "client.key"),
		},
		// Use default heartbeat_interval (10s) and health_timeout (30s)
	}
	client1ConfigData, err := yaml.Marshal(client1Config)
	if err != nil {
		t.Fatalf("marshal client1 config: %v", err)
	}
	if err := os.WriteFile(client1ConfigPath, client1ConfigData, 0600); err != nil {
		t.Fatalf("failed to write client1 config: %v", err)
	}

	// Build the binary first to avoid go run's subprocess issues
	binaryPath := filepath.Join(certDir, "qmux-test")
	buildCmd := exec.Command("go", "build", "-o", binaryPath, ".")
	buildCmd.Dir = ".."
	if output, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build binary: %v, output: %s", err, output)
	}

	// Start Client 1 via exec (so we can kill it with SIGKILL)
	client1Cmd := exec.CommandContext(ctx, binaryPath, "run", "client", "-c", client1ConfigPath)
	// Set process group so we can kill all child processes
	client1Cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := client1Cmd.Start(); err != nil {
		t.Fatalf("failed to start client 1: %v", err)
	}
	// Ensure process group is cleaned up
	defer func() {
		if client1Cmd.Process != nil {
			// Kill the entire process group
			_ = syscall.Kill(-client1Cmd.Process.Pid, syscall.SIGKILL)
			_ = client1Cmd.Wait()
		}
	}()
	t.Logf("Client 1 started with PID %d", client1Cmd.Process.Pid)
	time.Sleep(2 * time.Second) // Wait for client to connect

	// Start Client 2 (in-process, will remain running)
	client2Config := &config.Client{
		ClientID: "client-2",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"},
			},
		},
		Local: config.LocalService{
			Host: "127.0.0.1",
			Port: localAddr.Port,
		},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		// Use default HeartbeatInterval (10s)
	}

	c2, err := client.New(client2Config)
	if err != nil {
		t.Fatalf("failed to create client 2: %v", err)
	}

	client2Ctx, client2Cancel := context.WithCancel(ctx)
	defer client2Cancel()
	go func() {
		if err := c2.Start(client2Ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("client 2 error: %v", err)
		}
	}()
	time.Sleep(1 * time.Second)

	// Verify client 2 is connected and healthy
	c2Healthy := c2.HealthyConnectionCount()
	c2Total := c2.TotalConnectionCount()
	t.Logf("Client 2: %d healthy, %d total connections", c2Healthy, c2Total)

	if c2Healthy != 1 || c2Total != 1 {
		t.Fatalf("client 2 should have 1 healthy connection, got %d healthy, %d total", c2Healthy, c2Total)
	}

	t.Log("Both clients connected")

	// Kill Client 1 with SIGKILL (simulates process crash, no graceful shutdown)
	t.Log("Killing client 1 with SIGKILL (simulating process crash)...")
	// Kill the entire process group (negative PID) to ensure all child processes are killed
	if err := syscall.Kill(-client1Cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("failed to kill client 1 process group: %v", err)
	}
	// Wait for process to be killed
	_ = client1Cmd.Wait()
	t.Log("Client 1 killed")

	// Test multiple connections to measure success rate
	const totalAttempts = 50
	successCount := 0

	t.Logf("Testing %d connection attempts after abrupt disconnect...", totalAttempts)

	for range totalAttempts {
		if testConnection(t, trafficPort) {
			successCount++
		}
	}

	successRate := float64(successCount) / float64(totalAttempts) * 100
	t.Logf("Connection success rate: %d/%d (%.1f%%)", successCount, totalAttempts, successRate)

	if successRate < 50 {
		t.Fatalf("success rate %.1f%% is below 50%% threshold", successRate)
	}

	t.Logf("Abrupt disconnect test passed with %s load balancer (%.1f%% success rate)", loadBalancer, successRate)
}

// testConnection attempts a single connection through the traffic port and returns success
func testConnection(t *testing.T, trafficPort int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
	if err != nil {
		t.Logf("connection failed: %v", err)
		return false
	}
	defer func() { _ = conn.Close() }()

	testData := "Test data after abrupt disconnect"
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Logf("set deadline failed: %v", err)
		return false
	}

	if _, err := conn.Write([]byte(testData)); err != nil {
		t.Logf("write failed: %v", err)
		return false
	}

	buf := make([]byte, len(testData))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Logf("read failed: %v", err)
		return false
	}

	if string(buf) != testData {
		t.Logf("data mismatch: got %q, expected %q", string(buf), testData)
		return false
	}

	return true
}

func openVerifiedTCPConnection(t *testing.T, trafficPort int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dial held TCP connection: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		_ = conn.Close()
		t.Fatalf("set held TCP connection deadline: %v", err)
	}

	testData := []byte("held TCP relay verification")
	if _, err := conn.Write(testData); err != nil {
		_ = conn.Close()
		t.Fatalf("write held TCP connection: %v", err)
	}
	buf := make([]byte, len(testData))
	if _, err := io.ReadFull(conn, buf); err != nil {
		_ = conn.Close()
		t.Fatalf("read held TCP connection: %v", err)
	}
	if string(buf) != string(testData) {
		_ = conn.Close()
		t.Fatalf("held TCP connection data mismatch: got %q, want %q", buf, testData)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		t.Fatalf("clear held TCP connection deadline: %v", err)
	}
	return conn
}

func assertConnectionClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set closed connection deadline: %v", err)
	}
	var buf [1]byte
	if _, err := conn.Read(buf[:]); err == nil {
		t.Fatal("held TCP connection remained usable after Server.Start returned")
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatalf("held TCP connection remained open after Server.Start returned: %v", err)
		}
	}
}

func testUDPConnection(t *testing.T, trafficPort int) bool {
	t.Helper()
	conn, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
	if err != nil {
		t.Logf("UDP connection failed: %v", err)
		return false
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Logf("set UDP deadline failed: %v", err)
		return false
	}

	testData := []byte("UDP restart verification")
	if _, err := conn.Write(testData); err != nil {
		t.Logf("UDP write failed: %v", err)
		return false
	}
	buf := make([]byte, len(testData))
	n, err := conn.Read(buf)
	if err != nil {
		t.Logf("UDP read failed: %v", err)
		return false
	}
	if n != len(testData) || string(buf[:n]) != string(testData) {
		t.Logf("UDP data mismatch: got %q, expected %q", buf[:n], testData)
		return false
	}
	return true
}

func startRestartEchoBackend(t *testing.T) int {
	t.Helper()
	loopback := net.ParseIP("127.0.0.1")

	const maxAttempts = 32
	var lastTCPBindErr error
	for range maxAttempts {
		udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: loopback})
		if err != nil {
			t.Fatalf("start local UDP echo server: %v", err)
		}
		port := udpConn.LocalAddr().(*net.UDPAddr).Port

		tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: loopback, Port: port})
		if err != nil {
			lastTCPBindErr = err
			if closeErr := udpConn.Close(); closeErr != nil {
				t.Fatalf("release unusable local UDP echo socket: %v (TCP bind: %v)", closeErr, err)
			}
			continue
		}

		reservedUDPPort := udpConn.LocalAddr().(*net.UDPAddr).Port
		reservedTCPPort := tcpListener.Addr().(*net.TCPAddr).Port
		if reservedUDPPort != port || reservedTCPPort != port {
			closeErr := errors.Join(udpConn.Close(), tcpListener.Close())
			t.Fatalf("invalid live local echo reservations: TCP=%d UDP=%d (close: %v)",
				reservedTCPPort, reservedUDPPort, closeErr)
		}

		t.Cleanup(func() {
			if err := errors.Join(tcpListener.Close(), udpConn.Close()); err != nil {
				t.Errorf("close local TCP/UDP echo sockets: %v", err)
			}
		})
		serveTCPEcho(tcpListener)
		serveUDPEcho(udpConn, false)
		return port
	}

	t.Fatalf("start TCP and UDP echo servers on one port after %d attempts: %v", maxAttempts, lastTCPBindErr)
	return 0
}

// reserveRestartServerPorts keeps all three sockets open until it has a
// distinct QUIC UDP port and one traffic port bindable by both TCP and UDP.
func reserveRestartServerPorts(t *testing.T) (quicPort, trafficPort int, release func() error) {
	t.Helper()
	loopback := net.ParseIP("127.0.0.1")
	quicReservation, err := net.ListenUDP("udp", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Fatalf("reserve QUIC UDP port: %v", err)
	}
	quicPort = quicReservation.LocalAddr().(*net.UDPAddr).Port

	const maxAttempts = 32
	var lastTCPBindErr error
	for range maxAttempts {
		// Pick the traffic number from the UDP namespace while the QUIC UDP
		// reservation is live. The kernel therefore cannot choose quicPort.
		trafficUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: loopback})
		if err != nil {
			closeErr := quicReservation.Close()
			t.Fatalf("reserve traffic UDP port: %v (close QUIC reservation: %v)", err, closeErr)
		}
		trafficPort = trafficUDP.LocalAddr().(*net.UDPAddr).Port
		if trafficPort == quicPort {
			if err := trafficUDP.Close(); err != nil {
				quicCloseErr := quicReservation.Close()
				t.Fatalf("release colliding traffic UDP reservation: %v (close QUIC reservation: %v)", err, quicCloseErr)
			}
			continue
		}

		trafficTCP, err := net.ListenTCP("tcp", &net.TCPAddr{IP: loopback, Port: trafficPort})
		if err != nil {
			lastTCPBindErr = err
			if closeErr := trafficUDP.Close(); closeErr != nil {
				quicCloseErr := quicReservation.Close()
				t.Fatalf("release unusable traffic UDP reservation: %v (TCP bind: %v; close QUIC reservation: %v)",
					closeErr, err, quicCloseErr)
			}
			continue
		}

		reservedTrafficUDPPort := trafficUDP.LocalAddr().(*net.UDPAddr).Port
		reservedTrafficTCPPort := trafficTCP.Addr().(*net.TCPAddr).Port
		if reservedTrafficUDPPort != trafficPort || reservedTrafficTCPPort != trafficPort || trafficPort == quicPort {
			closeErr := errors.Join(trafficUDP.Close(), trafficTCP.Close(), quicReservation.Close())
			t.Fatalf(
				"invalid live port reservations: QUIC=%d traffic TCP=%d traffic UDP=%d (close: %v)",
				quicPort, reservedTrafficTCPPort, reservedTrafficUDPPort, closeErr,
			)
		}

		return quicPort, trafficPort, func() error {
			return errors.Join(trafficUDP.Close(), trafficTCP.Close(), quicReservation.Close())
		}
	}

	if err := quicReservation.Close(); err != nil {
		t.Fatalf("find dual-protocol traffic port after %d attempts (last TCP bind: %v; close QUIC reservation: %v)",
			maxAttempts, lastTCPBindErr, err)
	}
	t.Fatalf("find dual-protocol traffic port after %d attempts: %v", maxAttempts, lastTCPBindErr)
	return 0, 0, nil
}

// TestServerRestartReconnect_MTLS tests that clients automatically reconnect
// after the server is stopped and restarted, and can successfully handle TCP
// and UDP requests through the same traffic address after reconnection.
func TestServerRestartReconnect_MTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Generate mTLS certificates for the test
	certDir := generateTestCertificates(t)

	localPort := startRestartEchoBackend(t)
	t.Logf("Local TCP and UDP echo servers listening on 127.0.0.1:%d", localPort)

	// Reserve distinct ports while simultaneously proving that the traffic
	// numeric port is available to both TCP and UDP, then release before Start.
	quicPort, trafficPort, releaseServerPorts := reserveRestartServerPorts(t)
	t.Logf("Allocated QUIC port %d, traffic port %d", quicPort, trafficPort)

	// Configure QMux server with short heartbeat and health timeout
	serverConfig := &config.Server{
		Listeners: []config.QuicListener{
			{
				QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
				TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
				Protocol:    "both",
			},
		},
		Auth: config.ServerAuth{
			Method:     "mtls",
			CACertFile: filepath.Join(certDir, "ca.crt"),
		},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
			SessionTicketEncryptionKeyRotationInterval: 24 * time.Hour,
			SessionTicketEncryptionKeyRotationOverlap:  2,
		},
		HeartbeatInterval: 1 * time.Second,
		HealthTimeout:     3 * time.Second,
	}

	// Start QMux server with a cancellable context
	serverCtx, serverCancel := context.WithCancel(ctx)
	defer serverCancel()
	firstServerErr := make(chan error, 1)
	if err := releaseServerPorts(); err != nil {
		t.Fatalf("release restart server port reservations: %v", err)
	}
	go func() {
		firstServerErr <- server.Start(serverCtx, serverConfig)
	}()
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-firstServerErr:
		t.Fatalf("server stopped unexpectedly after initial start: %v", err)
	default:
		// Server is running.
	}
	t.Logf("QMux server started on QUIC port %d, traffic port %d", quicPort, trafficPort)

	// Create and start client-1 in-process
	client1Config := &config.Client{
		ClientID: "client-1",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"},
			},
		},
		Local: config.LocalService{
			Host: "127.0.0.1",
			Port: localPort,
		},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		HeartbeatInterval: 1 * time.Second,
		HealthTimeout:     3 * time.Second,
	}

	c1, err := client.New(client1Config)
	if err != nil {
		t.Fatalf("failed to create client-1: %v", err)
	}

	client1Ctx, client1Cancel := context.WithCancel(ctx)
	defer client1Cancel()
	go func() {
		if err := c1.Start(client1Ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("client-1 error: %v", err)
		}
	}()
	time.Sleep(1 * time.Second)

	// Create and start client-2 in-process
	client2Config := &config.Client{
		ClientID: "client-2",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"},
			},
		},
		Local: config.LocalService{
			Host: "127.0.0.1",
			Port: localPort,
		},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		HeartbeatInterval: 1 * time.Second,
		HealthTimeout:     3 * time.Second,
	}

	c2, err := client.New(client2Config)
	if err != nil {
		t.Fatalf("failed to create client-2: %v", err)
	}

	client2Ctx, client2Cancel := context.WithCancel(ctx)
	defer client2Cancel()
	go func() {
		if err := c2.Start(client2Ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("client-2 error: %v", err)
		}
	}()
	time.Sleep(1 * time.Second)

	// Verify both clients have healthy connections
	c1Healthy := c1.HealthyConnectionCount()
	c1Total := c1.TotalConnectionCount()
	t.Logf("Client-1: %d healthy, %d total connections", c1Healthy, c1Total)
	if c1Healthy != 1 {
		t.Fatalf("client-1 should have 1 healthy connection, got %d healthy, %d total", c1Healthy, c1Total)
	}

	c2Healthy := c2.HealthyConnectionCount()
	c2Total := c2.TotalConnectionCount()
	t.Logf("Client-2: %d healthy, %d total connections", c2Healthy, c2Total)
	if c2Healthy != 1 {
		t.Fatalf("client-2 should have 1 healthy connection, got %d healthy, %d total", c2Healthy, c2Total)
	}

	t.Log("Both clients connected and healthy")

	// Keep a verified TCP relay open and idle across cancellation, then verify
	// that shutdown terminates the external endpoint before restart.
	heldTCPConn := openVerifiedTCPConnection(t, trafficPort)
	defer func() { _ = heldTCPConn.Close() }()
	if !testUDPConnection(t, trafficPort) {
		t.Fatalf("initial UDP verification failed: traffic port %d is not working", trafficPort)
	}
	t.Log("Initial TCP and UDP connection verification passed")

	// Stop the QMux server by cancelling its context
	t.Log("Stopping QMux server...")
	serverCancel()
	select {
	case err := <-firstServerErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("server shutdown returned unexpected error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for server shutdown")
	}
	assertConnectionClosed(t, heldTCPConn)

	// Wait for both clients to detect the disconnect (healthy drops to 0)
	t.Log("Waiting for clients to detect server shutdown...")
	disconnectTicker := time.NewTicker(200 * time.Millisecond)
	defer disconnectTicker.Stop()
	disconnectTimeout := time.After(15 * time.Second)
	for {
		select {
		case <-disconnectTimeout:
			t.Fatalf("timed out waiting for clients to detect disconnect: client-1 healthy=%d, client-2 healthy=%d",
				c1.HealthyConnectionCount(), c2.HealthyConnectionCount())
		case <-disconnectTicker.C:
			if c1.HealthyConnectionCount() == 0 && c2.HealthyConnectionCount() == 0 {
				goto disconnected
			}
		}
	}
disconnected:
	t.Log("Both clients detected server shutdown")

	// Restart the QMux server on the same QUIC and traffic addresses.
	serverCtx2, serverCancel2 := context.WithCancel(ctx)
	defer serverCancel2()
	secondServerErr := make(chan error, 1)
	go func() {
		secondServerErr <- server.Start(serverCtx2, serverConfig)
	}()
	// Give the server a moment to start, then verify it didn't fail immediately
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-secondServerErr:
		t.Fatalf("server stopped unexpectedly after restart: %v", err)
	default:
		// Server is running
	}
	t.Logf("QMux server restarted on QUIC port %d, traffic port %d", quicPort, trafficPort)

	// Wait for both clients to reconnect
	t.Log("Waiting for both clients to reconnect...")
	reconnectTicker := time.NewTicker(500 * time.Millisecond)
	defer reconnectTicker.Stop()
	reconnectTimeout := time.After(30 * time.Second)
	for {
		select {
		case <-reconnectTimeout:
			t.Fatalf("timed out waiting for clients to reconnect: client-1 healthy=%d total=%d, client-2 healthy=%d total=%d",
				c1.HealthyConnectionCount(), c1.TotalConnectionCount(),
				c2.HealthyConnectionCount(), c2.TotalConnectionCount())
		case <-reconnectTicker.C:
			if c1.HealthyConnectionCount() >= 1 && c2.HealthyConnectionCount() >= 1 {
				goto reconnected
			}
		}
	}
reconnected:
	t.Logf("Both clients reconnected: client-1 healthy=%d total=%d, client-2 healthy=%d total=%d",
		c1.HealthyConnectionCount(), c1.TotalConnectionCount(),
		c2.HealthyConnectionCount(), c2.TotalConnectionCount())

	// Verify both protocols after reconnection using the same traffic port.
	if !testConnection(t, trafficPort) {
		t.Fatalf("post-reconnection TCP verification failed: traffic port %d is not working; client-1 healthy=%d total=%d, client-2 healthy=%d total=%d",
			trafficPort,
			c1.HealthyConnectionCount(), c1.TotalConnectionCount(),
			c2.HealthyConnectionCount(), c2.TotalConnectionCount())
	}
	if !testUDPConnection(t, trafficPort) {
		t.Fatalf("post-reconnection UDP verification failed: traffic port %d is not working; client-1 healthy=%d total=%d, client-2 healthy=%d total=%d",
			trafficPort,
			c1.HealthyConnectionCount(), c1.TotalConnectionCount(),
			c2.HealthyConnectionCount(), c2.TotalConnectionCount())
	}
	t.Log("Post-reconnection TCP and UDP connection verification passed")

	serverCancel2()
	select {
	case err := <-secondServerErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("restarted server shutdown returned unexpected error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for restarted server shutdown")
	}
}
