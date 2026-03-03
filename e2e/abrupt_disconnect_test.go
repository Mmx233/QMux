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
	defer os.RemoveAll(certDir)

	// Start local TCP echo server
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local server: %v", err)
	}
	defer localListener.Close()

	localAddr := localListener.Addr().(*net.TCPAddr)
	t.Logf("Local echo server listening on %s", localAddr)

	// Echo server goroutine
	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go io.Copy(conn, conn)
		}
	}()

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
	client1Config := map[string]interface{}{
		"client_id": "client-1",
		"server": map[string]interface{}{
			"servers": []map[string]interface{}{
				{"address": fmt.Sprintf("127.0.0.1:%d", quicPort), "server_name": "localhost"},
			},
		},
		"local": map[string]interface{}{
			"host": "127.0.0.1",
			"port": localAddr.Port,
		},
		"tls": map[string]interface{}{
			"ca_cert_file":     filepath.Join(certDir, "ca.crt"),
			"client_cert_file": filepath.Join(certDir, "client.crt"),
			"client_key_file":  filepath.Join(certDir, "client.key"),
		},
		// Use default heartbeat_interval (10s) and health_timeout (30s)
	}
	client1ConfigData, _ := yaml.Marshal(client1Config)
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
			syscall.Kill(-client1Cmd.Process.Pid, syscall.SIGKILL)
			client1Cmd.Wait()
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
	client1Cmd.Wait()
	t.Log("Client 1 killed")

	// Test multiple connections to measure success rate
	const totalAttempts = 50
	successCount := 0

	t.Logf("Testing %d connection attempts after abrupt disconnect...", totalAttempts)

	for i := 0; i < totalAttempts; i++ {
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
	defer conn.Close()

	testData := "Test data after abrupt disconnect"
	conn.SetDeadline(time.Now().Add(5 * time.Second))

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

// TestServerRestartReconnect_MTLS tests that clients automatically reconnect
// after the server is stopped and restarted, and can successfully handle
// requests through the traffic port after reconnection.
func TestServerRestartReconnect_MTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Generate mTLS certificates for the test
	certDir := generateTestCertificates(t)
	defer os.RemoveAll(certDir)

	// Start local TCP echo server on a random port
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local echo server: %v", err)
	}
	defer localListener.Close()

	localAddr := localListener.Addr().(*net.TCPAddr)
	t.Logf("Local echo server listening on %s", localAddr)

	// Echo server goroutine: echo back all received data
	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go io.Copy(conn, conn)
		}
	}()

	// Get free ports for QMux server
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)
	t.Logf("Allocated QUIC port %d, traffic port %d", quicPort, trafficPort)

	// Configure QMux server with short heartbeat and health timeout
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
		HeartbeatInterval: 1 * time.Second,
		HealthTimeout:     3 * time.Second,
	}

	// Start QMux server with a cancellable context
	serverCtx, serverCancel := context.WithCancel(ctx)
	defer serverCancel()
	go func() {
		if err := server.Start(serverCtx, serverConfig); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("server error: %v", err)
		}
	}()
	time.Sleep(500 * time.Millisecond)
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
			Port: localAddr.Port,
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
			Port: localAddr.Port,
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

	// Verify initial end-to-end connection via traffic port
	if !testConnection(t, trafficPort) {
		t.Fatalf("initial connection verification failed: traffic port %d is not working", trafficPort)
	}
	t.Log("Initial connection verification passed")
	// Stop the QMux server by cancelling its context
	t.Log("Stopping QMux server...")
	serverCancel()

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

	// Use a new traffic port for the restarted server since the old traffic
	// manager's TCP listener is not closed by server.Start on context cancel.
	trafficPort2 := getFreePort(t)
	serverConfig.Listeners[0].TrafficAddr = fmt.Sprintf("127.0.0.1:%d", trafficPort2)
	t.Logf("Allocated new traffic port %d for server restart", trafficPort2)

	// Restart the QMux server with a new cancellable context
	serverCtx2, serverCancel2 := context.WithCancel(ctx)
	defer serverCancel2()
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- server.Start(serverCtx2, serverConfig)
	}()
	// Give the server a moment to start, then verify it didn't fail immediately
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-serverErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("server failed to restart: %v", err)
		}
	default:
		// Server is running
	}
	t.Logf("QMux server restarted on QUIC port %d, traffic port %d", quicPort, trafficPort2)

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

	// Verify end-to-end connection works after reconnection using the new traffic port
	if !testConnection(t, trafficPort2) {
		t.Fatalf("post-reconnection connection verification failed: traffic port %d is not working; client-1 healthy=%d total=%d, client-2 healthy=%d total=%d",
			trafficPort2,
			c1.HealthyConnectionCount(), c1.TotalConnectionCount(),
			c2.HealthyConnectionCount(), c2.TotalConnectionCount())
	}
	t.Log("Post-reconnection connection verification passed")
}
