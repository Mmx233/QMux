package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/cmd/generate/certs"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server"
)

// generateTestCertificates generates test certificates for integration tests
func generateTestCertificates(t testing.TB) string {
	tempDir, err := os.MkdirTemp("", "qmux-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	// Generate certificates using the existing logic
	caKey, caCert, err := certs.GenerateCA(1)
	if err != nil {
		t.Fatalf("failed to generate CA: %v", err)
	}

	serverKey, serverCert, err := certs.GenerateServerCert(caKey, caCert, 1)
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

// TestTCPReverseProxy_MTLS tests TCP reverse proxy functionality with mTLS authentication
func TestTCPReverseProxy_MTLS(t *testing.T) {
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

	// Echo server
	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c) // Echo back
			}(conn)
		}
	}()

	// Start QMux server
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort,
				},
				TrafficPort: trafficPort,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
		HealthCheckInterval: 1 * time.Second,
		HealthCheckTimeout:  3 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start server
	serverErrCh := make(chan error, 1)
	go func() {
		if err := server.Start(ctx, serverConfig); err != nil && !errors.Is(err, context.Canceled) {
			serverErrCh <- err
		}
	}()

	// Give server time to start
	time.Sleep(500 * time.Millisecond)

	// Start QMux client
	clientConfig := &config.Client{
		ClientID: "test-client",
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
	}

	c, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	clientErrCh := make(chan error, 1)
	go func() {
		if err := c.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			clientErrCh <- err
		}
	}()

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
			defer conn.Close()

			// Set deadline
			conn.SetDeadline(time.Now().Add(5 * time.Second))

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
	defer os.RemoveAll(certDir)

	// Start local UDP echo server
	localConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local UDP server: %v", err)
	}
	defer localConn.Close()

	localAddr := localConn.LocalAddr().(*net.UDPAddr)
	t.Logf("Local UDP echo server listening on %s", localAddr)

	// UDP echo server
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := localConn.ReadFrom(buf)
			if err != nil {
				return
			}
			// Echo back
			localConn.WriteTo(buf[:n], addr)
		}
	}()

	// Start QMux server
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort,
				},
				TrafficPort: trafficPort,
				Protocol:    "udp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
		HealthCheckInterval: 1 * time.Second,
		HealthCheckTimeout:  3 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start server
	serverErrCh := make(chan error, 1)
	go func() {
		if err := server.Start(ctx, serverConfig); err != nil && !errors.Is(err, context.Canceled) {
			serverErrCh <- err
		}
	}()

	time.Sleep(500 * time.Millisecond)

	// Start QMux client
	clientConfig := &config.Client{
		ClientID: "test-client-udp",
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
	}

	c, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	clientErrCh := make(chan error, 1)
	go func() {
		if err := c.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			clientErrCh <- err
		}
	}()

	time.Sleep(500 * time.Millisecond)

	// Test UDP through tunnel
	testData := []string{
		"UDP Test 1",
		"UDP Test 2",
		strings.Repeat("U", 512),
	}

	for i, data := range testData {
		t.Run(fmt.Sprintf("UDPMessage_%d", i), func(t *testing.T) {
			conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
			if err != nil {
				t.Fatalf("failed to dial UDP: %v", err)
			}
			defer conn.Close()

			conn.SetDeadline(time.Now().Add(5 * time.Second))

			// Send data
			_, err = conn.Write([]byte(data))
			if err != nil {
				t.Fatalf("failed to write UDP: %v", err)
			}

			// Read echo
			buf := make([]byte, 65535)
			n, err := conn.Read(buf)
			if err != nil {
				t.Fatalf("failed to read UDP: %v", err)
			}

			if string(buf[:n]) != data {
				t.Fatalf("UDP data mismatch: got %q, expected %q", string(buf[:n]), data)
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
	defer os.RemoveAll(certDir)

	// Start local TCP echo server
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local server: %v", err)
	}
	defer localListener.Close()

	localAddr := localListener.Addr().(*net.TCPAddr)

	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go io.Copy(conn, conn)
		}
	}()

	// Start QMux infrastructure
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort,
				},
				TrafficPort: trafficPort,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go server.Start(ctx, serverConfig)
	time.Sleep(500 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "test-client-concurrent",
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
	}

	c, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(500 * time.Millisecond)

	// Test 10 concurrent connections
	var wg sync.WaitGroup
	errors := make(chan error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
			if err != nil {
				errors <- fmt.Errorf("conn %d: dial failed: %w", id, err)
				return
			}
			defer conn.Close()

			data := fmt.Sprintf("Connection %d: %s", id, strings.Repeat("X", 100))
			conn.SetDeadline(time.Now().Add(5 * time.Second))

			if _, err := conn.Write([]byte(data)); err != nil {
				errors <- fmt.Errorf("conn %d: write failed: %w", id, err)
				return
			}

			buf := make([]byte, len(data))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errors <- fmt.Errorf("conn %d: read failed: %w", id, err)
				return
			}

			if string(buf) != data {
				errors <- fmt.Errorf("conn %d: data mismatch", id)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}

// TestClientReconnection_MTLS tests client reconnection and failover with mTLS authentication
func TestClientReconnection_MTLS(t *testing.T) {
	certDir := generateTestCertificates(t)
	defer os.RemoveAll(certDir)

	// Start local TCP echo server
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local server: %v", err)
	}
	defer localListener.Close()

	localAddr := localListener.Addr().(*net.TCPAddr)

	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go io.Copy(conn, conn)
		}
	}()

	// Start QMux server
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort,
				},
				TrafficPort: trafficPort,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
		HealthCheckInterval: 500 * time.Millisecond,
		HealthCheckTimeout:  1 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go server.Start(ctx, serverConfig)
	time.Sleep(500 * time.Millisecond)

	// Start first client
	clientConfig := &config.Client{
		ClientID: "test-client-reconnect",
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
		HeartbeatInterval: 500 * time.Millisecond,
	}

	c1, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create first client: %v", err)
	}

	client1Ctx, client1Cancel := context.WithCancel(ctx)
	go c1.Start(client1Ctx)
	time.Sleep(500 * time.Millisecond)

	// Test connection works with first client
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to traffic port: %v", err)
	}
	testData := "First client test"
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte(testData))
	buf := make([]byte, len(testData))
	io.ReadFull(conn, buf)
	if string(buf) != testData {
		t.Fatalf("data mismatch with first client")
	}
	conn.Close()

	t.Log("First client connection successful")

	// Disconnect first client
	client1Cancel()
	time.Sleep(1500 * time.Millisecond) // Wait for health check to mark it unhealthy

	t.Log("First client disconnected")

	// Start second client with same ID
	c2, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create second client: %v", err)
	}

	client2Ctx, client2Cancel := context.WithCancel(ctx)
	defer client2Cancel()
	go c2.Start(client2Ctx)
	time.Sleep(500 * time.Millisecond)

	// Test connection works with second client (failover)
	conn, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect after reconnection: %v", err)
	}
	testData = "Second client test"
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte(testData))
	buf = make([]byte, len(testData))
	io.ReadFull(conn, buf)
	if string(buf) != testData {
		t.Fatalf("data mismatch with second client")
	}
	conn.Close()

	t.Log("Client reconnection and failover successful")
}

// getFreePort gets a free port for testing
func getFreePort(t testing.TB) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

// BenchmarkClientAuth_MTLS benchmarks client authentication with mTLS authentication
func BenchmarkClientAuth_MTLS(b *testing.B) {
	certDir := generateTestCertificates(b)
	defer os.RemoveAll(certDir)

	// Start local TCP echo server
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start local server: %v", err)
	}
	defer localListener.Close()

	localAddr := localListener.Addr().(*net.TCPAddr)

	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go io.Copy(conn, conn)
		}
	}()

	// Start QMux server
	quicPort := getFreePort(b)
	trafficPort := getFreePort(b)

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort,
				},
				TrafficPort: trafficPort,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	go server.Start(ctx, serverConfig)
	time.Sleep(500 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "bench-client-auth",
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
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := client.New(clientConfig)
		if err != nil {
			b.Fatalf("failed to create client: %v", err)
		}

		clientCtx, clientCancel := context.WithTimeout(ctx, 5*time.Second)
		errCh := make(chan error, 1)
		go func() {
			errCh <- c.Start(clientCtx)
		}()

		// Wait for connection to establish
		time.Sleep(200 * time.Millisecond)

		clientCancel()
		<-errCh
	}
}

// BenchmarkTCPConnection_MTLS benchmarks TCP connection throughput with mTLS authentication
func BenchmarkTCPConnection_MTLS(b *testing.B) {
	certDir := generateTestCertificates(b)
	defer os.RemoveAll(certDir)

	// Start local TCP echo server
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start local server: %v", err)
	}
	defer localListener.Close()

	localAddr := localListener.Addr().(*net.TCPAddr)

	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go io.Copy(conn, conn)
		}
	}()

	// Start QMux server
	quicPort := getFreePort(b)
	trafficPort := getFreePort(b)

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort,
				},
				TrafficPort: trafficPort,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	go server.Start(ctx, serverConfig)
	time.Sleep(500 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "bench-client-tcp",
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
	}

	c, err := client.New(clientConfig)
	if err != nil {
		b.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(500 * time.Millisecond)

	testData := []byte(strings.Repeat("X", 1024)) // 1KB

	b.ResetTimer()
	b.SetBytes(int64(len(testData)))

	for i := 0; i < b.N; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
		if err != nil {
			b.Fatalf("failed to connect: %v", err)
		}

		conn.SetDeadline(time.Now().Add(5 * time.Second))
		conn.Write(testData)

		buf := make([]byte, len(testData))
		io.ReadFull(conn, buf)
		conn.Close()
	}
}

// BenchmarkUDPConnection_MTLS benchmarks UDP connection throughput with mTLS authentication
func BenchmarkUDPConnection_MTLS(b *testing.B) {
	certDir := generateTestCertificates(b)
	defer os.RemoveAll(certDir)

	// Start local UDP echo server
	localConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start local UDP server: %v", err)
	}
	defer localConn.Close()

	localAddr := localConn.LocalAddr().(*net.UDPAddr)

	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := localConn.ReadFrom(buf)
			if err != nil {
				return
			}
			localConn.WriteTo(buf[:n], addr)
		}
	}()

	// Start QMux server
	quicPort := getFreePort(b)
	trafficPort := getFreePort(b)

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort,
				},
				TrafficPort: trafficPort,
				Protocol:    "udp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	go server.Start(ctx, serverConfig)
	time.Sleep(500 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "bench-client-udp",
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
	}

	c, err := client.New(clientConfig)
	if err != nil {
		b.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(500 * time.Millisecond)

	testData := []byte(strings.Repeat("U", 512)) // 512 bytes

	b.ResetTimer()
	b.SetBytes(int64(len(testData)))

	for i := 0; i < b.N; i++ {
		conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
		if err != nil {
			b.Fatalf("failed to dial UDP: %v", err)
		}

		conn.SetDeadline(time.Now().Add(5 * time.Second))
		conn.Write(testData)

		buf := make([]byte, 65535)
		conn.Read(buf)
		conn.Close()
	}
}

// BenchmarkTCPThroughput_MTLS benchmarks sustained TCP throughput with mTLS authentication
func BenchmarkTCPThroughput_MTLS(b *testing.B) {
	certDir := generateTestCertificates(b)
	defer os.RemoveAll(certDir)

	// Start local TCP echo server
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start local server: %v", err)
	}
	defer localListener.Close()

	localAddr := localListener.Addr().(*net.TCPAddr)

	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go io.Copy(conn, conn)
		}
	}()

	// Start QMux server
	quicPort := getFreePort(b)
	trafficPort := getFreePort(b)

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort,
				},
				TrafficPort: trafficPort,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	go server.Start(ctx, serverConfig)
	time.Sleep(500 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "bench-client-throughput",
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
	}

	c, err := client.New(clientConfig)
	if err != nil {
		b.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(500 * time.Millisecond)

	// Open persistent connection
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
	if err != nil {
		b.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	testData := []byte(strings.Repeat("X", 10240)) // 10KB chunks
	buf := make([]byte, len(testData))

	b.ResetTimer()
	b.SetBytes(int64(len(testData)))

	for i := 0; i < b.N; i++ {
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		conn.Write(testData)
		io.ReadFull(conn, buf)
	}
}

// TestMultiServerConnection_MTLS tests client connecting to multiple servers simultaneously
func TestMultiServerConnection_MTLS(t *testing.T) {
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

	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go io.Copy(conn, conn)
		}
	}()

	// Start two QMux servers
	quicPort1 := getFreePort(t)
	trafficPort1 := getFreePort(t)
	quicPort2 := getFreePort(t)
	trafficPort2 := getFreePort(t)

	serverConfig1 := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort1,
				},
				TrafficPort: trafficPort1,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
		HealthCheckInterval: 1 * time.Second,
		HealthCheckTimeout:  3 * time.Second,
	}

	serverConfig2 := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort2,
				},
				TrafficPort: trafficPort2,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
		HealthCheckInterval: 1 * time.Second,
		HealthCheckTimeout:  3 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start both servers
	go server.Start(ctx, serverConfig1)
	go server.Start(ctx, serverConfig2)
	time.Sleep(500 * time.Millisecond)

	t.Logf("Server 1 listening on QUIC port %d, traffic port %d", quicPort1, trafficPort1)
	t.Logf("Server 2 listening on QUIC port %d, traffic port %d", quicPort2, trafficPort2)

	// Start client with multi-server configuration
	clientConfig := &config.Client{
		ClientID: "test-client-multiserver",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: fmt.Sprintf("127.0.0.1:%d", quicPort1), ServerName: "localhost"},
				{Address: fmt.Sprintf("127.0.0.1:%d", quicPort2), ServerName: "localhost"},
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
	}

	c, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	clientErrCh := make(chan error, 1)
	go func() {
		if err := c.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			clientErrCh <- err
		}
	}()

	time.Sleep(1 * time.Second)

	// Verify client connected to both servers
	healthyCount := c.HealthyConnectionCount()
	totalCount := c.TotalConnectionCount()
	t.Logf("Client connections: %d healthy, %d total", healthyCount, totalCount)

	if healthyCount != 2 {
		t.Errorf("expected 2 healthy connections, got %d", healthyCount)
	}
	if totalCount != 2 {
		t.Errorf("expected 2 total connections, got %d", totalCount)
	}

	// Test traffic through both servers
	for i, trafficPort := range []int{trafficPort1, trafficPort2} {
		t.Run(fmt.Sprintf("Server_%d", i+1), func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
			if err != nil {
				t.Fatalf("failed to connect to traffic port %d: %v", trafficPort, err)
			}
			defer conn.Close()

			testData := fmt.Sprintf("Hello from server %d", i+1)
			conn.SetDeadline(time.Now().Add(5 * time.Second))

			if _, err := conn.Write([]byte(testData)); err != nil {
				t.Fatalf("failed to write: %v", err)
			}

			buf := make([]byte, len(testData))
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("failed to read: %v", err)
			}

			if string(buf) != testData {
				t.Fatalf("data mismatch: got %q, expected %q", string(buf), testData)
			}
		})
	}

	// Check for errors
	select {
	case err := <-clientErrCh:
		t.Fatalf("client error: %v", err)
	default:
	}

	t.Log("Multi-server connection test successful")
}

// TestMultiServerFailover_MTLS tests client failover when one server goes down
func TestMultiServerFailover_MTLS(t *testing.T) {
	certDir := generateTestCertificates(t)
	defer os.RemoveAll(certDir)

	// Start local TCP echo server
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local server: %v", err)
	}
	defer localListener.Close()

	localAddr := localListener.Addr().(*net.TCPAddr)

	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go io.Copy(conn, conn)
		}
	}()

	// Start two QMux servers
	quicPort1 := getFreePort(t)
	trafficPort1 := getFreePort(t)
	quicPort2 := getFreePort(t)
	trafficPort2 := getFreePort(t)

	serverConfig1 := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort1,
				},
				TrafficPort: trafficPort1,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
		HealthCheckInterval: 500 * time.Millisecond,
		HealthCheckTimeout:  1 * time.Second,
	}

	serverConfig2 := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort2,
				},
				TrafficPort: trafficPort2,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
		HealthCheckInterval: 500 * time.Millisecond,
		HealthCheckTimeout:  1 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Start server 1 with its own context so we can cancel it
	server1Ctx, server1Cancel := context.WithCancel(ctx)
	go server.Start(server1Ctx, serverConfig1)

	// Start server 2
	go server.Start(ctx, serverConfig2)
	time.Sleep(500 * time.Millisecond)

	t.Logf("Server 1 listening on QUIC port %d, traffic port %d", quicPort1, trafficPort1)
	t.Logf("Server 2 listening on QUIC port %d, traffic port %d", quicPort2, trafficPort2)

	// Start client with multi-server configuration
	clientConfig := &config.Client{
		ClientID: "test-client-failover",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: fmt.Sprintf("127.0.0.1:%d", quicPort1), ServerName: "localhost"},
				{Address: fmt.Sprintf("127.0.0.1:%d", quicPort2), ServerName: "localhost"},
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
		HeartbeatInterval: 500 * time.Millisecond,
	}

	c, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(1 * time.Second)

	// Verify both connections are healthy
	if c.HealthyConnectionCount() != 2 {
		t.Fatalf("expected 2 healthy connections initially, got %d", c.HealthyConnectionCount())
	}
	t.Log("Both servers connected and healthy")

	// Test traffic through server 1 before shutdown
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort1), 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to server 1: %v", err)
	}
	testData := "Before failover"
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte(testData))
	buf := make([]byte, len(testData))
	io.ReadFull(conn, buf)
	if string(buf) != testData {
		t.Fatalf("data mismatch before failover")
	}
	conn.Close()
	t.Log("Traffic through server 1 successful before failover")

	// Shutdown server 1
	t.Log("Shutting down server 1...")
	server1Cancel()
	time.Sleep(2 * time.Second) // Wait for health check to detect failure

	// Verify server 2 is still healthy (client should have detected server 1 failure)
	healthyCount := c.HealthyConnectionCount()
	t.Logf("Healthy connections after server 1 shutdown: %d", healthyCount)

	// Traffic through server 2 should still work
	conn, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort2), 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to server 2 after failover: %v", err)
	}
	testData = "After failover"
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte(testData))
	buf = make([]byte, len(testData))
	io.ReadFull(conn, buf)
	if string(buf) != testData {
		t.Fatalf("data mismatch after failover")
	}
	conn.Close()

	t.Log("Failover test successful - traffic continues through remaining server")
}

// TestMultiServerLoadBalancing_MTLS tests round-robin load balancing across servers
func TestMultiServerLoadBalancing_MTLS(t *testing.T) {
	certDir := generateTestCertificates(t)
	defer os.RemoveAll(certDir)

	// Start local TCP echo server
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local server: %v", err)
	}
	defer localListener.Close()

	localAddr := localListener.Addr().(*net.TCPAddr)

	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go io.Copy(conn, conn)
		}
	}()

	// Start two QMux servers
	quicPort1 := getFreePort(t)
	trafficPort1 := getFreePort(t)
	quicPort2 := getFreePort(t)
	trafficPort2 := getFreePort(t)

	serverConfig1 := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort1,
				},
				TrafficPort: trafficPort1,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	serverConfig2 := &config.Server{
		Listeners: []config.QuicListener{
			{
				Listen: config.Listen{
					IP:   "127.0.0.1",
					Port: quicPort2,
				},
				TrafficPort: trafficPort2,
				Protocol:    "tcp",
			},
		},
		Auth: config.ServerAuth{
			Method: "mtls",
		},
		TLS: config.ServerTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go server.Start(ctx, serverConfig1)
	go server.Start(ctx, serverConfig2)
	time.Sleep(500 * time.Millisecond)

	// Start client with multi-server configuration
	clientConfig := &config.Client{
		ClientID: "test-client-loadbalance",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: fmt.Sprintf("127.0.0.1:%d", quicPort1), ServerName: "localhost"},
				{Address: fmt.Sprintf("127.0.0.1:%d", quicPort2), ServerName: "localhost"},
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
	}

	c, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(1 * time.Second)

	// Verify both connections are healthy
	if c.HealthyConnectionCount() != 2 {
		t.Fatalf("expected 2 healthy connections, got %d", c.HealthyConnectionCount())
	}

	// Test load balancer selection
	connMgr := c.ConnectionManager()
	balancer := connMgr.Balancer()

	// Make multiple selections and verify round-robin distribution
	selections := make(map[string]int)
	numSelections := 10

	for i := 0; i < numSelections; i++ {
		sc, err := balancer.Select()
		if err != nil {
			t.Fatalf("selection %d failed: %v", i, err)
		}
		selections[sc.ServerAddr()]++
	}

	t.Logf("Load balancer selections: %v", selections)

	// With 2 servers and 10 selections, each should get 5
	for addr, count := range selections {
		if count != numSelections/2 {
			t.Errorf("server %s got %d selections, expected %d", addr, count, numSelections/2)
		}
	}

	t.Log("Load balancing test successful")
}
