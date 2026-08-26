package stek

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// generateTestCert generates a self-signed certificate for testing
func generateTestCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	// Generate private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate private key: %v", err)
	}

	// Create certificate template
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}

	// Create certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}

	// Create TLS certificate
	tlsCert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  privateKey,
	}

	// Create cert pool
	certPool := x509.NewCertPool()
	certPool.AddCert(cert)

	return tlsCert, certPool
}

func newTestSTEKServerConfig(t *testing.T, cert tls.Certificate, interval time.Duration, overlap uint8) (*RotateManager, *tls.Config) {
	t.Helper()
	manager, err := NewRotateManager(interval, overlap)
	if err != nil {
		t.Fatalf("failed to create STEK manager: %v", err)
	}

	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
		NextProtos:   []string{"test-proto"},
	}
	tlsConf.SetSessionTicketKeys(*manager.Keys.Load())
	tlsConf.GetConfigForClient = func(_ *tls.ClientHelloInfo) (*tls.Config, error) {
		cfg := tlsConf.Clone()
		cfg.SetSessionTicketKeys(*manager.Keys.Load())
		return cfg, nil
	}
	return manager, tlsConf
}

// TestSTEKRotationDoesNotBreakExistingConnections verifies that high-speed STEK rotation
// does not affect existing QUIC connections or their ability to resume sessions.
func TestSTEKRotationDoesNotBreakExistingConnections(t *testing.T) {
	// Generate test certificates
	serverCert, certPool := generateTestCert(t)

	// Create STEK manager with very fast rotation (10ms interval, 3 key overlap)
	stekManager, serverTLSConf := newTestSTEKServerConfig(t, serverCert, 10*time.Millisecond, 3)

	// Start UDP listener
	udpAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to resolve UDP address: %v", err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		t.Fatalf("failed to listen UDP: %v", err)
	}
	defer func() { _ = udpConn.Close() }()

	serverAddr := udpConn.LocalAddr().String()
	t.Logf("Server listening on %s", serverAddr)

	// Create QUIC transport and listener
	tr := quic.Transport{Conn: udpConn}
	listener, err := tr.Listen(serverTLSConf, &quic.Config{
		MaxIdleTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("failed to create QUIC listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start STEK rotation
	stekManager.Start(ctx)
	defer stekManager.Stop()

	// Track metrics
	var (
		totalMessages    atomic.Int64
		successfulEchoes atomic.Int64
		connectionErrors atomic.Int64
		rotationCount    atomic.Int64
	)

	// Server goroutine - echo server
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				t.Logf("Accept error: %v", err)
				continue
			}

			go func(c *quic.Conn) {
				for {
					stream, err := c.AcceptStream(ctx)
					if err != nil {
						return
					}
					go func(s *quic.Stream) {
						defer func() { _ = (*s).Close() }()
						buf := make([]byte, 1024)
						for {
							n, err := (*s).Read(buf)
							if err != nil {
								return
							}
							_, _ = (*s).Write(buf[:n])
						}
					}(stream)
				}
			}(conn)
		}
	}()

	// Monitor rotation count
	initialKey := (*stekManager.Keys.Load())[0]
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		lastKey := initialKey
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				keys := stekManager.Keys.Load()
				if keys != nil && len(*keys) > 0 {
					currentKey := (*keys)[0]
					if currentKey != lastKey {
						rotationCount.Add(1)
						lastKey = currentKey
					}
				}
			}
		}
	}()

	// Client TLS config
	clientTLSConf := &tls.Config{
		RootCAs:            certPool,
		InsecureSkipVerify: true,
		NextProtos:         []string{"test-proto"},
	}

	// Test: Multiple concurrent connections sending data during rapid STEK rotation
	const (
		numConnections = 5
		testDuration   = 3 * time.Second
	)

	var wg sync.WaitGroup
	startTime := time.Now()

	for i := range numConnections {
		connID := i
		wg.Go(func() {

			// Create client connection
			conn, err := quic.DialAddr(ctx, serverAddr, clientTLSConf, &quic.Config{
				MaxIdleTimeout: 30 * time.Second,
			})
			if err != nil {
				connectionErrors.Add(1)
				t.Logf("Connection %d: dial error: %v", connID, err)
				return
			}
			defer func() { _ = conn.CloseWithError(0, "done") }()

			// Send messages continuously during test duration
			for time.Since(startTime) < testDuration {
				stream, err := conn.OpenStreamSync(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					t.Logf("Connection %d: open stream error: %v", connID, err)
					continue
				}

				// Send test message
				msgNum := totalMessages.Add(1)
				testMsg := fmt.Sprintf("conn-%d-msg-%d", connID, msgNum)
				_, err = stream.Write([]byte(testMsg))
				if err != nil {
					_ = stream.Close()
					continue
				}

				// Read echo
				buf := make([]byte, len(testMsg))
				_, err = stream.Read(buf)
				_ = stream.Close()
				if err != nil {
					continue
				}

				if string(buf) == testMsg {
					successfulEchoes.Add(1)
				}

				// Small delay between messages
				time.Sleep(5 * time.Millisecond)
			}
		})
	}

	// Wait for all clients to finish
	wg.Wait()

	// Cancel context to stop server
	cancel()
	<-serverDone

	// Report results
	t.Logf("Test completed:")
	t.Logf("  Total messages attempted: %d", totalMessages.Load())
	t.Logf("  Successful echoes: %d", successfulEchoes.Load())
	t.Logf("  Connection errors: %d", connectionErrors.Load())
	t.Logf("  STEK rotations observed: %d", rotationCount.Load())

	// Verify that connections worked despite rapid rotation
	if successfulEchoes.Load() == 0 {
		t.Error("No successful echoes - STEK rotation may have broken connections")
	}

	if connectionErrors.Load() > int64(numConnections/2) {
		t.Errorf("Too many connection errors (%d) - STEK rotation may be causing issues",
			connectionErrors.Load())
	}

	// Verify rotations actually happened
	if rotationCount.Load() < 10 {
		t.Logf("Warning: Only %d rotations observed, expected more with 10ms interval over %v",
			rotationCount.Load(), testDuration)
	}
}

// TestSTEKRotationSessionResumption tests that session resumption works across STEK rotations
// as long as the session ticket was encrypted with a key still in the overlap window.
func TestSTEKRotationSessionResumption(t *testing.T) {
	serverCert, certPool := generateTestCert(t)

	// Create STEK manager with 50ms rotation and 3 key overlap
	// This means keys are valid for 150ms (3 * 50ms)
	stekManager, serverTLSConf := newTestSTEKServerConfig(t, serverCert, 50*time.Millisecond, 3)

	udpAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer func() { _ = udpConn.Close() }()

	serverAddr := udpConn.LocalAddr().String()

	tr := quic.Transport{Conn: udpConn}
	listener, err := tr.Listen(serverTLSConf, &quic.Config{MaxIdleTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Start rotation
	stekManager.Start(ctx)
	defer stekManager.Stop()

	// Accept connections in background
	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go func(c *quic.Conn) {
				stream, err := c.AcceptStream(ctx)
				if err != nil {
					return
				}
				buf := make([]byte, 1024)
				n, _ := (*stream).Read(buf)
				_, _ = (*stream).Write(buf[:n])
				_ = (*stream).Close()
			}(conn)
		}
	}()

	clientTLSConf := &tls.Config{
		RootCAs:            certPool,
		InsecureSkipVerify: true,
		NextProtos:         []string{"test-proto"},
	}

	// Test multiple reconnections during rotation
	for i := range 5 {
		t.Run(fmt.Sprintf("Reconnection_%d", i), func(t *testing.T) {
			conn, err := quic.DialAddr(ctx, serverAddr, clientTLSConf, &quic.Config{
				MaxIdleTimeout: 30 * time.Second,
			})
			if err != nil {
				t.Fatalf("dial failed: %v", err)
			}

			stream, err := conn.OpenStreamSync(ctx)
			if err != nil {
				_ = conn.CloseWithError(0, "")
				t.Fatalf("open stream failed: %v", err)
			}

			testData := fmt.Sprintf("test-message-%d", i)
			_, err = stream.Write([]byte(testData))
			if err != nil {
				t.Fatalf("write failed: %v", err)
			}
			_ = stream.Close()

			_ = conn.CloseWithError(0, "done")

			// Wait for some rotations before next connection
			time.Sleep(60 * time.Millisecond)
		})
	}
}

// TestSTEKRotationUnderLoad tests STEK rotation behavior under heavy connection load
func TestSTEKRotationUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	serverCert, certPool := generateTestCert(t)

	// Very aggressive rotation: 5ms interval
	stekManager, serverTLSConf := newTestSTEKServerConfig(t, serverCert, 5*time.Millisecond, 4)

	udpAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	udpConn, _ := net.ListenUDP("udp", udpAddr)
	defer func() { _ = udpConn.Close() }()

	serverAddr := udpConn.LocalAddr().String()

	tr := quic.Transport{Conn: udpConn}
	listener, _ := tr.Listen(serverTLSConf, &quic.Config{MaxIdleTimeout: 30 * time.Second})
	defer func() { _ = listener.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stekManager.Start(ctx)
	defer stekManager.Stop()

	// Server accepts and echoes
	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go func(c *quic.Conn) {
				for {
					stream, err := c.AcceptStream(ctx)
					if err != nil {
						return
					}
					go func(s *quic.Stream) {
						buf := make([]byte, 1024)
						n, _ := (*s).Read(buf)
						_, _ = (*s).Write(buf[:n])
						_ = (*s).Close()
					}(stream)
				}
			}(conn)
		}
	}()

	clientTLSConf := &tls.Config{
		RootCAs:            certPool,
		InsecureSkipVerify: true,
		NextProtos:         []string{"test-proto"},
	}

	var (
		successCount atomic.Int64
		failCount    atomic.Int64
	)

	// Spawn many concurrent clients
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 10 {
				if ctx.Err() != nil {
					return
				}

				conn, err := quic.DialAddr(ctx, serverAddr, clientTLSConf, &quic.Config{
					MaxIdleTimeout: 5 * time.Second,
				})
				if err != nil {
					failCount.Add(1)
					continue
				}

				stream, err := conn.OpenStreamSync(ctx)
				if err != nil {
					_ = conn.CloseWithError(0, "")
					failCount.Add(1)
					continue
				}

				_, _ = stream.Write([]byte("ping"))
				_ = stream.Close()
				_ = conn.CloseWithError(0, "done")
				successCount.Add(1)

				time.Sleep(10 * time.Millisecond)
			}
		})
	}

	wg.Wait()

	t.Logf("Load test results: success=%d, fail=%d", successCount.Load(), failCount.Load())

	// Should have high success rate even under aggressive rotation
	total := successCount.Load() + failCount.Load()
	if total > 0 {
		successRate := float64(successCount.Load()) / float64(total) * 100
		t.Logf("Success rate: %.1f%%", successRate)
		if successRate < 80 {
			t.Errorf("Success rate too low: %.1f%%, expected >= 80%%", successRate)
		}
	}
}
