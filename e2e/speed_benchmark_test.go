package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/config"
)

const (
	testDataSize100MB = 100 * 1024 * 1024
	testChunkSize     = 1024 * 1024
)

// ============================================
// TCP Benchmarks - Single Connection
// ============================================

func BenchmarkTCP_SingleConn_100MB(b *testing.B) {
	runTCPThroughputBenchmark(b, 1, testDataSize100MB)
}

// ============================================
// TCP Benchmarks - Dual Connections
// ============================================

func BenchmarkTCP_DualConn_100MB(b *testing.B) {
	runTCPThroughputBenchmark(b, 2, testDataSize100MB)
}

// ============================================
// UDP Benchmarks - Single Connection
// ============================================

func BenchmarkUDP_SingleConn_Throughput(b *testing.B) {
	runUDPThroughputBenchmark(b, 1)
}

// ============================================
// UDP Benchmarks - Dual Connections
// ============================================

func BenchmarkUDP_DualConn_Throughput(b *testing.B) {
	runUDPThroughputBenchmark(b, 2)
}

// ============================================
// Core Benchmark Functions
// ============================================

func runTCPThroughputBenchmark(b *testing.B, connCount int, totalSize int64) {
	certDir := generateTestCertificates(b)

	localListener, trafficPort := setupTCPEchoServer(b, certDir)
	closeOnCleanup(b, localListener)

	b.SetBytes(totalSize * 2 * int64(connCount)) // send + receive per connection
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		runTCPTransfer(b, trafficPort, connCount, totalSize)
	}
}

func runTCPTransfer(b *testing.B, trafficPort int, connCount int, totalSize int64) {
	var wg sync.WaitGroup
	errCh := make(chan error, connCount*2)

	for c := range connCount {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 10*time.Second)
		if err != nil {
			b.Fatalf("[conn %d] dial failed: %v", c, err)
		}

		wg.Add(2)

		// Sender
		go func(conn net.Conn, connID int) {
			defer wg.Done()
			data := make([]byte, testChunkSize)
			remaining := totalSize
			for remaining > 0 {
				toSend := testChunkSize
				if int64(toSend) > remaining {
					toSend = int(remaining)
				}
				n, err := conn.Write(data[:toSend])
				if err != nil {
					errCh <- fmt.Errorf("[conn %d] send error: %w", connID, err)
					return
				}
				remaining -= int64(n)
			}
		}(conn, c)

		// Receiver
		go func(conn net.Conn, connID int) {
			defer wg.Done()
			defer func() { _ = conn.Close() }()
			buf := make([]byte, testChunkSize)
			remaining := totalSize
			for remaining > 0 {
				n, err := conn.Read(buf)
				if err != nil {
					errCh <- fmt.Errorf("[conn %d] recv error: %w", connID, err)
					return
				}
				remaining -= int64(n)
			}
		}(conn, c)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		b.Fatal(err)
	}
}

func runUDPThroughputBenchmark(b *testing.B, connCount int) {
	certDir := generateTestCertificates(b)

	localConn, trafficPort := setupUDPEchoServer(b, certDir)
	closeOnCleanup(b, localConn)

	const packetSize = 512
	const packetsPerConn = 5000

	b.SetBytes(int64(packetSize * 2 * packetsPerConn * connCount))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		runUDPTransferPipelined(b, trafficPort, connCount, packetSize, packetsPerConn)
	}
}

// runUDPTransferPipelined uses separate goroutines for send/receive to measure true throughput
func runUDPTransferPipelined(b *testing.B, trafficPort int, connCount int, packetSize int, packetsPerConn int) {
	const batchSize = 100
	var wg sync.WaitGroup
	errCh := make(chan error, connCount*2)

	for c := range connCount {
		conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
		if err != nil {
			b.Fatalf("[conn %d] dial failed: %v", c, err)
		}

		wg.Add(2)

		// Sender goroutine - sends all packets with pacing
		go func(conn net.Conn, connID int) {
			defer wg.Done()
			data := make([]byte, packetSize)
			for i := range packetsPerConn {
				if _, err := conn.Write(data); err != nil {
					errCh <- fmt.Errorf("[conn %d] write error: %w", connID, err)
					return
				}
				if (i+1)%batchSize == 0 {
					time.Sleep(100 * time.Microsecond)
				}
			}
		}(conn, c)

		// Receiver goroutine - receives all responses
		go func(conn net.Conn, connID int) {
			defer wg.Done()
			defer func() { _ = conn.Close() }()
			buf := make([]byte, 65535)
			received := 0
			// Set a longer deadline for the entire receive operation
			if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
				errCh <- fmt.Errorf("[conn %d] set read deadline: %w", connID, err)
				return
			}
			for received < packetsPerConn {
				_, err := conn.Read(buf)
				if err != nil {
					// Allow partial receives - UDP can drop packets
					var netErr net.Error
					if errors.As(err, &netErr) && netErr.Timeout() {
						if received > packetsPerConn/2 {
							// Got most packets, acceptable for benchmark
							return
						}
					}
					errCh <- fmt.Errorf("[conn %d] read error after %d packets: %w", connID, received, err)
					return
				}
				received++
			}
		}(conn, c)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		b.Fatal(err)
	}
}

// ============================================
// Comprehensive Speed Report Test
// ============================================

// ThroughputResult holds the result of a throughput test
type ThroughputResult struct {
	Label         string
	Duration      time.Duration
	BytesSent     int64
	BytesReceived int64
	SendMbps      float64
	RecvMbps      float64
	LossPercent   float64
}

func (r *ThroughputResult) String() string {
	if r.LossPercent > 0 {
		return fmt.Sprintf("%s: TX %.2f Mbps, RX %.2f Mbps (loss %.1f%%)",
			r.Label, r.SendMbps, r.RecvMbps, r.LossPercent)
	}
	return fmt.Sprintf("%s: %.2f Mbps", r.Label, r.RecvMbps)
}

func TestSpeedReport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping speed report in short mode")
	}

	t.Log("=== QMux Speed Report (iperf3-style) ===")
	t.Log("")

	// TCP Tests - Raw baseline (no QMux)
	t.Run("TCP", func(t *testing.T) {
		t.Run("Raw_Discard", func(t *testing.T) {
			result := runRawTCPDiscardTest(t, "Raw TCP")
			t.Log(result.String())
		})

		// QMux TCP
		certDir := generateTestCertificates(t)

		localListener, trafficPort := setupTCPDiscardServerForTest(t, certDir)
		closeOnCleanup(t, localListener)

		t.Run("QMux_Discard", func(t *testing.T) {
			result := runQMuxTCPDiscardTest(t, trafficPort, "QMux TCP")
			t.Log(result.String())
		})
	})

	// UDP Tests
	t.Run("UDP", func(t *testing.T) {
		t.Run("Raw_Discard", func(t *testing.T) {
			result := runRawUDPDiscardTest(t, "Raw UDP")
			t.Log(result.String())
		})

		// QMux UDP
		certDir := generateTestCertificates(t)

		localConn, trafficPort := setupUDPDiscardServerForTest(t, certDir)
		closeOnCleanup(t, localConn)

		t.Run("QMux_Discard", func(t *testing.T) {
			result := runQMuxUDPDiscardTest(t, trafficPort, "QMux UDP")
			t.Log(result.String())
		})
	})
}

func dialBenchmarkConn(t *testing.T, network, address string) net.Conn {
	t.Helper()
	var (
		conn net.Conn
		err  error
	)
	if network == "tcp" {
		conn, err = net.DialTimeout(network, address, 5*time.Second)
	} else {
		conn, err = net.Dial(network, address)
	}
	if err != nil {
		t.Fatalf("dial %s benchmark endpoint: %v", network, err)
	}
	closeOnCleanup(t, conn)

	switch typedConn := conn.(type) {
	case *net.TCPConn:
		_ = typedConn.SetWriteBuffer(4 * 1024 * 1024)
		_ = typedConn.SetNoDelay(false)
	case *net.UDPConn:
		_ = typedConn.SetWriteBuffer(16 * 1024 * 1024)
	}
	return conn
}

func measureWrites(conn net.Conn, payloadSize int, warmupDuration, testDuration time.Duration, afterWarmup func()) (int64, time.Duration) {
	data := make([]byte, payloadSize)
	warmupEnd := time.Now().Add(warmupDuration)
	for time.Now().Before(warmupEnd) {
		_, _ = conn.Write(data)
	}
	if afterWarmup != nil {
		afterWarmup()
	}

	var totalBytes int64
	start := time.Now()
	deadline := start.Add(testDuration)
	for time.Now().Before(deadline) {
		n, err := conn.Write(data)
		if err != nil {
			break
		}
		totalBytes += int64(n)
	}
	return totalBytes, time.Since(start)
}

func newThroughputResult(label string, sentBytes, receivedBytes int64, elapsed time.Duration, reportReceived bool) *ThroughputResult {
	sendMbps := float64(sentBytes) * 8 / elapsed.Seconds() / 1000000
	recvMbps := float64(receivedBytes) * 8 / elapsed.Seconds() / 1000000
	result := &ThroughputResult{
		Label:     label,
		Duration:  elapsed,
		BytesSent: sentBytes,
		SendMbps:  sendMbps,
		RecvMbps:  recvMbps,
	}
	if reportReceived {
		result.BytesReceived = receivedBytes
		if sentBytes > 0 {
			result.LossPercent = float64(sentBytes-receivedBytes) / float64(sentBytes) * 100
		}
	}
	return result
}

// runRawTCPDiscardTest measures raw TCP throughput (no QMux)
func runRawTCPDiscardTest(t *testing.T, label string) *ThroughputResult {
	const bufferSize = 128 * 1024 // 128KB like iperf3

	// Create discard server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	closeOnCleanup(t, listener)

	// Discard server - just read and discard
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, bufferSize)
				for {
					_, err := c.Read(buf)
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	conn := dialBenchmarkConn(t, "tcp", listener.Addr().String())
	totalBytes, elapsed := measureWrites(conn, bufferSize, 500*time.Millisecond, 5*time.Second, nil)
	return newThroughputResult(label, totalBytes, totalBytes, elapsed, false)
}

// runQMuxTCPDiscardTest measures TCP throughput through QMux
func runQMuxTCPDiscardTest(t *testing.T, trafficPort int, label string) *ThroughputResult {
	const bufferSize = 128 * 1024

	conn := dialBenchmarkConn(t, "tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
	totalBytes, elapsed := measureWrites(conn, bufferSize, 500*time.Millisecond, 5*time.Second, nil)
	return newThroughputResult(label, totalBytes, totalBytes, elapsed, false)
}

// runRawUDPDiscardTest measures raw UDP throughput (no QMux)
func runRawUDPDiscardTest(t *testing.T, label string) *ThroughputResult {
	const packetSize = 1400

	// Create discard server
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	closeOnCleanup(t, serverConn)

	if udpConn, ok := serverConn.(*net.UDPConn); ok {
		_ = udpConn.SetReadBuffer(16 * 1024 * 1024)
	}

	var receivedBytes atomic.Int64
	done := make(chan struct{})

	// Discard server
	go func() {
		buf := make([]byte, 65535)
		for {
			select {
			case <-done:
				return
			default:
				_ = serverConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
				n, _, err := serverConn.ReadFrom(buf)
				if err != nil {
					continue
				}
				receivedBytes.Add(int64(n))
			}
		}
	}()

	conn := dialBenchmarkConn(t, "udp", serverConn.LocalAddr().String())
	sentBytes, elapsed := measureWrites(conn, packetSize, 500*time.Millisecond, 5*time.Second, func() {
		receivedBytes.Store(0)
	})

	// Wait for receiver
	time.Sleep(200 * time.Millisecond)
	close(done)

	return newThroughputResult(label, sentBytes, receivedBytes.Load(), elapsed, true)
}

// runQMuxUDPDiscardTest measures UDP throughput through QMux
func runQMuxUDPDiscardTest(t *testing.T, trafficPort int, label string) *ThroughputResult {
	const packetSize = 1400

	conn := dialBenchmarkConn(t, "udp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
	sentBytes, elapsed := measureWrites(conn, packetSize, 500*time.Millisecond, 5*time.Second, nil)
	return newThroughputResult(label, sentBytes, sentBytes, elapsed, false)
}

func setupQMuxEndpoint(
	t testing.TB,
	certDir, protocol, clientID string,
	localPort int,
	timeout, serverStartupDelay, clientStartupDelay time.Duration,
	fatalOnClientError bool,
) int {
	t.Helper()
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)
	quicConfig := getOptimizedQuicConfig()

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
			TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
			Protocol:    protocol,
			Quic:        quicConfig,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	startTestServer(ctx, serverConfig)
	time.Sleep(serverStartupDelay)

	clientConfig := &config.Client{
		ClientID: clientID,
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"}},
		},
		Local: config.LocalService{Host: "127.0.0.1", Port: localPort},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		Quic: quicConfig,
	}

	c, err := client.New(clientConfig)
	if err != nil {
		if fatalOnClientError {
			t.Fatalf("create %s client: %v", protocol, err)
			return 0
		}
		t.Errorf("create %s client: %v", protocol, err)
		return 0
	}
	startTestClient(ctx, c)
	time.Sleep(clientStartupDelay)
	return trafficPort
}

// setupTCPDiscardServerForTest creates a TCP discard server behind QMux
func setupTCPDiscardServerForTest(t *testing.T, certDir string) (net.Listener, int) {
	const bufferSize = 128 * 1024

	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local server: %v", err)
	}
	localAddr := localListener.Addr().(*net.TCPAddr)

	// Discard server
	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, bufferSize)
				for {
					_, err := c.Read(buf)
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return localListener, setupQMuxEndpoint(t, certDir, "tcp", "tcp-discard-client", localAddr.Port,
		10*time.Minute, 300*time.Millisecond, 300*time.Millisecond, true)
}

// setupUDPDiscardServerForTest creates a UDP discard server behind QMux
func setupUDPDiscardServerForTest(t *testing.T, certDir string) (net.PacketConn, int) {
	localConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local UDP server: %v", err)
	}
	localAddr := localConn.LocalAddr().(*net.UDPAddr)

	// Set large buffers
	if udpConn, ok := localConn.(*net.UDPConn); ok {
		_ = udpConn.SetReadBuffer(16 * 1024 * 1024)
	}

	// Discard server - just read and discard
	go func() {
		buf := make([]byte, 65535)
		for {
			_, _, err := localConn.ReadFrom(buf)
			if err != nil {
				return
			}
		}
	}()

	return localConn, setupQMuxEndpoint(t, certDir, "udp", "udp-discard-client", localAddr.Port,
		10*time.Minute, 300*time.Millisecond, 300*time.Millisecond, true)
}

// ============================================
// Setup Functions
// ============================================

func setupTCPEchoServer(b *testing.B, certDir string) (net.Listener, int) {
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start local server: %v", err)
	}
	localAddr := localListener.Addr().(*net.TCPAddr)

	serveTCPEcho(localListener)
	return localListener, setupQMuxEndpoint(b, certDir, "tcp", "tcp-bench-client", localAddr.Port,
		10*time.Minute, 300*time.Millisecond, 300*time.Millisecond, true)
}

func setupUDPEchoServer(b *testing.B, certDir string) (net.PacketConn, int) {
	localConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start local UDP server: %v", err)
	}
	localAddr := localConn.LocalAddr().(*net.UDPAddr)

	serveUDPEcho(localConn, true)
	return localConn, setupQMuxEndpoint(b, certDir, "udp", "udp-bench-client", localAddr.Port,
		10*time.Minute, 300*time.Millisecond, 300*time.Millisecond, true)
}

func getOptimizedQuicConfig() config.Quic {
	return config.Quic{
		InitialStreamReceiveWindow:     16 * 1024 * 1024,
		MaxStreamReceiveWindow:         64 * 1024 * 1024,
		InitialConnectionReceiveWindow: 32 * 1024 * 1024,
		MaxConnectionReceiveWindow:     128 * 1024 * 1024,
		MaxIncomingStreams:             1000,
		Allow0RTT:                      true,
	}
}
