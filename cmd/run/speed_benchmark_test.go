package run

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server"
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
	defer os.RemoveAll(certDir)

	localListener, trafficPort := setupTCPEchoServer(b, certDir)
	defer localListener.Close()

	b.SetBytes(totalSize * 2 * int64(connCount)) // send + receive per connection
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		runTCPTransfer(b, trafficPort, connCount, totalSize)
	}
}

func runTCPTransfer(b *testing.B, trafficPort int, connCount int, totalSize int64) {
	var wg sync.WaitGroup
	errCh := make(chan error, connCount*2)

	for c := 0; c < connCount; c++ {
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
			defer conn.Close()
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
	defer os.RemoveAll(certDir)

	localConn, trafficPort := setupUDPEchoServer(b, certDir)
	defer localConn.Close()

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

	for c := 0; c < connCount; c++ {
		conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
		if err != nil {
			b.Fatalf("[conn %d] dial failed: %v", c, err)
		}

		wg.Add(2)

		// Sender goroutine - sends all packets with pacing
		go func(conn net.Conn, connID int) {
			defer wg.Done()
			data := make([]byte, packetSize)
			for i := 0; i < packetsPerConn; i++ {
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
			defer conn.Close()
			buf := make([]byte, 65535)
			received := 0
			// Set a longer deadline for the entire receive operation
			conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			for received < packetsPerConn {
				_, err := conn.Read(buf)
				if err != nil {
					// Allow partial receives - UDP can drop packets
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
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
		defer os.RemoveAll(certDir)

		localListener, trafficPort := setupTCPDiscardServerForTest(t, certDir)
		defer localListener.Close()

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
		defer os.RemoveAll(certDir)

		localConn, trafficPort := setupUDPDiscardServerForTest(t, certDir)
		defer localConn.Close()

		t.Run("QMux_Discard", func(t *testing.T) {
			result := runQMuxUDPDiscardTest(t, trafficPort, "QMux UDP")
			t.Log(result.String())
		})
	})
}

// runRawTCPDiscardTest measures raw TCP throughput (no QMux)
func runRawTCPDiscardTest(t *testing.T, label string) *ThroughputResult {
	const testDuration = 5 * time.Second
	const warmupDuration = 500 * time.Millisecond
	const bufferSize = 128 * 1024 // 128KB like iperf3

	// Create discard server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	// Discard server - just read and discard
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
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

	// Connect
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Set large buffers
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetWriteBuffer(4 * 1024 * 1024)
		tcpConn.SetNoDelay(false)
	}

	data := make([]byte, bufferSize)
	var totalBytes int64

	// Warmup
	warmupEnd := time.Now().Add(warmupDuration)
	for time.Now().Before(warmupEnd) {
		conn.Write(data)
	}

	// Actual test
	start := time.Now()
	deadline := start.Add(testDuration)
	for time.Now().Before(deadline) {
		n, err := conn.Write(data)
		if err != nil {
			break
		}
		totalBytes += int64(n)
	}
	elapsed := time.Since(start)

	mbps := float64(totalBytes) * 8 / elapsed.Seconds() / 1000000

	return &ThroughputResult{
		Label:     label,
		Duration:  elapsed,
		BytesSent: totalBytes,
		SendMbps:  mbps,
		RecvMbps:  mbps,
	}
}

// runQMuxTCPDiscardTest measures TCP throughput through QMux
func runQMuxTCPDiscardTest(t *testing.T, trafficPort int, label string) *ThroughputResult {
	const testDuration = 5 * time.Second
	const warmupDuration = 500 * time.Millisecond
	const bufferSize = 128 * 1024

	// Connect through QMux
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetWriteBuffer(4 * 1024 * 1024)
		tcpConn.SetNoDelay(false)
	}

	data := make([]byte, bufferSize)
	var totalBytes int64

	// Warmup
	warmupEnd := time.Now().Add(warmupDuration)
	for time.Now().Before(warmupEnd) {
		conn.Write(data)
	}

	// Actual test
	start := time.Now()
	deadline := start.Add(testDuration)
	for time.Now().Before(deadline) {
		n, err := conn.Write(data)
		if err != nil {
			break
		}
		totalBytes += int64(n)
	}
	elapsed := time.Since(start)

	mbps := float64(totalBytes) * 8 / elapsed.Seconds() / 1000000

	return &ThroughputResult{
		Label:     label,
		Duration:  elapsed,
		BytesSent: totalBytes,
		SendMbps:  mbps,
		RecvMbps:  mbps,
	}
}

// runRawUDPDiscardTest measures raw UDP throughput (no QMux)
func runRawUDPDiscardTest(t *testing.T, label string) *ThroughputResult {
	const testDuration = 5 * time.Second
	const warmupDuration = 500 * time.Millisecond
	const packetSize = 1400

	// Create discard server
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer serverConn.Close()

	if udpConn, ok := serverConn.(*net.UDPConn); ok {
		udpConn.SetReadBuffer(16 * 1024 * 1024)
	}

	var receivedBytes int64
	done := make(chan struct{})

	// Discard server
	go func() {
		buf := make([]byte, 65535)
		for {
			select {
			case <-done:
				return
			default:
				serverConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
				n, _, err := serverConn.ReadFrom(buf)
				if err != nil {
					continue
				}
				atomic.AddInt64(&receivedBytes, int64(n))
			}
		}
	}()

	// Connect
	conn, err := net.Dial("udp", serverConn.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if udpConn, ok := conn.(*net.UDPConn); ok {
		udpConn.SetWriteBuffer(16 * 1024 * 1024)
	}

	data := make([]byte, packetSize)
	var sentBytes int64

	// Warmup
	warmupEnd := time.Now().Add(warmupDuration)
	for time.Now().Before(warmupEnd) {
		conn.Write(data)
	}

	// Reset counter after warmup
	atomic.StoreInt64(&receivedBytes, 0)

	// Actual test
	start := time.Now()
	deadline := start.Add(testDuration)
	for time.Now().Before(deadline) {
		n, err := conn.Write(data)
		if err != nil {
			break
		}
		sentBytes += int64(n)
	}
	elapsed := time.Since(start)

	// Wait for receiver
	time.Sleep(200 * time.Millisecond)
	close(done)

	recvBytes := atomic.LoadInt64(&receivedBytes)
	sendMbps := float64(sentBytes) * 8 / elapsed.Seconds() / 1000000
	recvMbps := float64(recvBytes) * 8 / elapsed.Seconds() / 1000000
	lossPercent := float64(0)
	if sentBytes > 0 {
		lossPercent = float64(sentBytes-recvBytes) / float64(sentBytes) * 100
	}

	return &ThroughputResult{
		Label:         label,
		Duration:      elapsed,
		BytesSent:     sentBytes,
		BytesReceived: recvBytes,
		SendMbps:      sendMbps,
		RecvMbps:      recvMbps,
		LossPercent:   lossPercent,
	}
}

// runQMuxUDPDiscardTest measures UDP throughput through QMux
func runQMuxUDPDiscardTest(t *testing.T, trafficPort int, label string) *ThroughputResult {
	const testDuration = 5 * time.Second
	const warmupDuration = 500 * time.Millisecond
	const packetSize = 1400

	// Connect through QMux
	conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if udpConn, ok := conn.(*net.UDPConn); ok {
		udpConn.SetWriteBuffer(16 * 1024 * 1024)
	}

	data := make([]byte, packetSize)
	var sentBytes int64

	// Warmup
	warmupEnd := time.Now().Add(warmupDuration)
	for time.Now().Before(warmupEnd) {
		conn.Write(data)
	}

	// Actual test
	start := time.Now()
	deadline := start.Add(testDuration)
	for time.Now().Before(deadline) {
		n, err := conn.Write(data)
		if err != nil {
			break
		}
		sentBytes += int64(n)
	}
	elapsed := time.Since(start)

	sendMbps := float64(sentBytes) * 8 / elapsed.Seconds() / 1000000

	return &ThroughputResult{
		Label:     label,
		Duration:  elapsed,
		BytesSent: sentBytes,
		SendMbps:  sendMbps,
		RecvMbps:  sendMbps,
	}
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
				defer c.Close()
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

	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	quicConfig := getOptimizedQuicConfig()

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
			TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
			Protocol:    "tcp",
			Quic:        quicConfig,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	go server.Start(ctx, serverConfig)
	time.Sleep(300 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "tcp-discard-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"}},
		},
		Local: config.LocalService{Host: "127.0.0.1", Port: localAddr.Port},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		Quic: quicConfig,
	}

	c, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(300 * time.Millisecond)

	return localListener, trafficPort
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
		udpConn.SetReadBuffer(16 * 1024 * 1024)
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

	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	quicConfig := getOptimizedQuicConfig()

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
			TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
			Protocol:    "udp",
			Quic:        quicConfig,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	go server.Start(ctx, serverConfig)
	time.Sleep(300 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "udp-discard-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"}},
		},
		Local: config.LocalService{Host: "127.0.0.1", Port: localAddr.Port},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		Quic: quicConfig,
	}

	c, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(300 * time.Millisecond)

	return localConn, trafficPort
}

func runRawTCPBaseline(t *testing.T, addr string, connCount int) {
	const totalSize = 100 * 1024 * 1024

	var totalBytes int64
	var wg sync.WaitGroup
	errCh := make(chan error, connCount*2)

	start := time.Now()

	for c := 0; c < connCount; c++ {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatalf("[conn %d] dial failed: %v", c, err)
		}

		wg.Add(2)

		go func(conn net.Conn, connID int) {
			defer wg.Done()
			data := make([]byte, testChunkSize)
			remaining := int64(totalSize)
			for remaining > 0 {
				n, err := conn.Write(data)
				if err != nil {
					errCh <- fmt.Errorf("[conn %d] send error: %w", connID, err)
					return
				}
				atomic.AddInt64(&totalBytes, int64(n))
				remaining -= int64(n)
			}
		}(conn, c)

		go func(conn net.Conn, connID int) {
			defer wg.Done()
			defer conn.Close()
			buf := make([]byte, testChunkSize)
			remaining := int64(totalSize)
			for remaining > 0 {
				n, err := conn.Read(buf)
				if err != nil {
					errCh <- fmt.Errorf("[conn %d] recv error: %w", connID, err)
					return
				}
				atomic.AddInt64(&totalBytes, int64(n))
				remaining -= int64(n)
			}
		}(conn, c)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	elapsed := time.Since(start)
	throughput := float64(totalBytes) / elapsed.Seconds() / 1024 / 1024
	t.Logf("Raw TCP (%d conn): %.2f MB/s (%v)", connCount, throughput, elapsed.Round(time.Millisecond))
}

// runRawUDPBaseline measures raw UDP echo throughput (same as QMux test for fair comparison)
func runRawUDPBaseline(t *testing.T, serverAddr string, connCount int, packetSize int) {
	const testDuration = 3 * time.Second
	const targetPPS = 50000 // Same rate limit as QMux test

	// Create UDP echo server
	echoConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create echo server: %v", err)
	}
	defer echoConn.Close()
	echoAddr := echoConn.LocalAddr().String()

	// Echo server goroutine
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 65535)
		for {
			select {
			case <-done:
				return
			default:
				echoConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
				n, addr, err := echoConn.ReadFrom(buf)
				if err != nil {
					continue
				}
				echoConn.WriteTo(buf[:n], addr)
			}
		}
	}()

	var sentPackets int64
	var receivedBytes int64
	var receivedPackets int64
	var wg sync.WaitGroup

	sendInterval := time.Second / time.Duration(targetPPS)

	for c := 0; c < connCount; c++ {
		conn, err := net.Dial("udp", echoAddr)
		if err != nil {
			t.Fatalf("[conn %d] dial failed: %v", c, err)
		}

		wg.Add(2)

		// Sender goroutine - rate limited
		go func(conn net.Conn) {
			defer wg.Done()
			data := make([]byte, packetSize)
			ticker := time.NewTicker(sendInterval)
			defer ticker.Stop()
			deadline := time.Now().Add(testDuration)

			for time.Now().Before(deadline) {
				select {
				case <-done:
					return
				case <-ticker.C:
					if _, err := conn.Write(data); err != nil {
						return
					}
					atomic.AddInt64(&sentPackets, 1)
				}
			}
		}(conn)

		// Receiver goroutine
		go func(conn net.Conn) {
			defer wg.Done()
			defer conn.Close()
			buf := make([]byte, 65535)

			for {
				conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				n, err := conn.Read(buf)
				if err != nil {
					select {
					case <-done:
						return
					default:
						if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
							continue
						}
						return
					}
				}
				atomic.AddInt64(&receivedBytes, int64(n))
				atomic.AddInt64(&receivedPackets, 1)
			}
		}(conn)
	}

	// Wait for test duration plus drain time
	time.Sleep(testDuration + 500*time.Millisecond)
	close(done)
	wg.Wait()

	elapsed := testDuration
	sent := atomic.LoadInt64(&sentPackets)
	recvBytes := atomic.LoadInt64(&receivedBytes)
	recvPkts := atomic.LoadInt64(&receivedPackets)

	throughput := float64(recvBytes) / elapsed.Seconds() / 1024 / 1024
	pps := float64(recvPkts) / elapsed.Seconds()
	lossRate := float64(0)
	if sent > 0 {
		lossRate = float64(sent-recvPkts) / float64(sent) * 100
	}

	t.Logf("Raw UDP %d conn %dB: %.2f MB/s, %.0f pps (sent %d, recv %d, loss %.1f%%)",
		connCount, packetSize, throughput, pps, sent, recvPkts, lossRate)
}

func runTCPSpeedTest(t *testing.T, trafficPort int, connCount int, totalSize int64) {
	var totalBytes int64
	var wg sync.WaitGroup
	errCh := make(chan error, connCount*2)

	start := time.Now()

	for c := 0; c < connCount; c++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 10*time.Second)
		if err != nil {
			t.Fatalf("[conn %d] dial failed: %v", c, err)
		}

		wg.Add(2)

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
				atomic.AddInt64(&totalBytes, int64(n))
				remaining -= int64(n)
			}
		}(conn, c)

		go func(conn net.Conn, connID int) {
			defer wg.Done()
			defer conn.Close()
			buf := make([]byte, testChunkSize)
			remaining := totalSize
			for remaining > 0 {
				n, err := conn.Read(buf)
				if err != nil {
					errCh <- fmt.Errorf("[conn %d] recv error: %w", connID, err)
					return
				}
				atomic.AddInt64(&totalBytes, int64(n))
				remaining -= int64(n)
			}
		}(conn, c)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	elapsed := time.Since(start)
	throughput := float64(totalBytes) / elapsed.Seconds() / 1024 / 1024
	t.Logf("TCP %d conn: %.2f MB/s (100MB in %v)", connCount, throughput, elapsed.Round(time.Millisecond))
}

func runUDPSpeedTest(t *testing.T, trafficPort int, connCount int) {
	runUDPSpeedTestWithSize(t, trafficPort, connCount, 512)
}

// runUDPSpeedTestWithSize measures UDP throughput through QMux using echo mode
// with rate limiting to avoid overwhelming the QUIC connection
func runUDPSpeedTestWithSize(t *testing.T, trafficPort int, connCount int, packetSize int) {
	const testDuration = 3 * time.Second
	const targetPPS = 50000 // Target packets per second per connection

	var sentPackets int64
	var receivedBytes int64
	var receivedPackets int64
	var wg sync.WaitGroup
	done := make(chan struct{})

	// Calculate send interval for rate limiting
	sendInterval := time.Second / time.Duration(targetPPS)

	for c := 0; c < connCount; c++ {
		conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
		if err != nil {
			t.Fatalf("[conn %d] dial failed: %v", c, err)
		}

		wg.Add(2)

		// Sender goroutine - rate limited sending
		go func(conn net.Conn, connID int) {
			defer wg.Done()
			data := make([]byte, packetSize)
			ticker := time.NewTicker(sendInterval)
			defer ticker.Stop()
			deadline := time.Now().Add(testDuration)

			for time.Now().Before(deadline) {
				select {
				case <-done:
					return
				case <-ticker.C:
					if _, err := conn.Write(data); err != nil {
						return
					}
					atomic.AddInt64(&sentPackets, 1)
				}
			}
		}(conn, c)

		// Receiver goroutine - count received bytes
		go func(conn net.Conn, connID int) {
			defer wg.Done()
			defer conn.Close()
			buf := make([]byte, 65535)

			for {
				conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				n, err := conn.Read(buf)
				if err != nil {
					select {
					case <-done:
						return
					default:
						if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
							continue
						}
						return
					}
				}
				atomic.AddInt64(&receivedBytes, int64(n))
				atomic.AddInt64(&receivedPackets, 1)
			}
		}(conn, c)
	}

	start := time.Now()

	// Wait for test duration plus drain time
	time.Sleep(testDuration + 500*time.Millisecond)
	close(done)
	wg.Wait()

	elapsed := testDuration // Use fixed duration for calculation
	sent := atomic.LoadInt64(&sentPackets)
	recvBytes := atomic.LoadInt64(&receivedBytes)
	recvPkts := atomic.LoadInt64(&receivedPackets)

	// One-way throughput (what we received back from echo)
	throughput := float64(recvBytes) / elapsed.Seconds() / 1024 / 1024
	pps := float64(recvPkts) / elapsed.Seconds()
	lossRate := float64(0)
	if sent > 0 {
		lossRate = float64(sent-recvPkts) / float64(sent) * 100
	}

	_ = start // suppress unused warning
	t.Logf("UDP %d conn %dB: %.2f MB/s, %.0f pps (sent %d, recv %d, loss %.1f%%)",
		connCount, packetSize, throughput, pps, sent, recvPkts, lossRate)
}

// runUDPLatencyTest measures round-trip latency (synchronous mode)
func runUDPLatencyTest(t *testing.T, trafficPort int, packetSize int) {
	const iterations = 1000

	conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	data := make([]byte, packetSize)
	buf := make([]byte, 65535)

	var totalLatency time.Duration
	successful := 0

	for i := 0; i < iterations; i++ {
		start := time.Now()
		conn.SetDeadline(time.Now().Add(1 * time.Second))
		if _, err := conn.Write(data); err != nil {
			continue
		}
		if _, err := conn.Read(buf); err != nil {
			continue
		}
		totalLatency += time.Since(start)
		successful++
	}

	if successful > 0 {
		avgLatency := totalLatency / time.Duration(successful)
		t.Logf("UDP Latency %dB: avg %.2fms (%d/%d successful)",
			packetSize, float64(avgLatency.Microseconds())/1000, successful, iterations)
	}
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

	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	quicPort := getFreePort(b)
	trafficPort := getFreePort(b)

	quicConfig := getOptimizedQuicConfig()

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
			TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
			Protocol:    "tcp",
			Quic:        quicConfig,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	b.Cleanup(cancel)

	go server.Start(ctx, serverConfig)
	time.Sleep(300 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "tcp-bench-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"}},
		},
		Local: config.LocalService{Host: "127.0.0.1", Port: localAddr.Port},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		Quic: quicConfig,
	}

	c, err := client.New(clientConfig)
	if err != nil {
		b.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(300 * time.Millisecond)

	return localListener, trafficPort
}

func setupTCPEchoServerForTest(t *testing.T, certDir string) (net.Listener, int) {
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local server: %v", err)
	}
	localAddr := localListener.Addr().(*net.TCPAddr)

	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	quicConfig := getOptimizedQuicConfig()

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
			TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
			Protocol:    "tcp",
			Quic:        quicConfig,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	go server.Start(ctx, serverConfig)
	time.Sleep(300 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "tcp-test-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"}},
		},
		Local: config.LocalService{Host: "127.0.0.1", Port: localAddr.Port},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		Quic: quicConfig,
	}

	c, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(300 * time.Millisecond)

	return localListener, trafficPort
}

func setupUDPEchoServer(b *testing.B, certDir string) (net.PacketConn, int) {
	localConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start local UDP server: %v", err)
	}
	localAddr := localConn.LocalAddr().(*net.UDPAddr)

	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := localConn.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := localConn.WriteTo(buf[:n], addr); err != nil {
				return
			}
		}
	}()

	quicPort := getFreePort(b)
	trafficPort := getFreePort(b)

	quicConfig := getOptimizedQuicConfig()

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
			TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
			Protocol:    "udp",
			Quic:        quicConfig,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	b.Cleanup(cancel)

	go server.Start(ctx, serverConfig)
	time.Sleep(300 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "udp-bench-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"}},
		},
		Local: config.LocalService{Host: "127.0.0.1", Port: localAddr.Port},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		Quic: quicConfig,
	}

	c, err := client.New(clientConfig)
	if err != nil {
		b.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(300 * time.Millisecond)

	return localConn, trafficPort
}

func setupUDPEchoServerForTest(t *testing.T, certDir string) (net.PacketConn, int) {
	localConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local UDP server: %v", err)
	}
	localAddr := localConn.LocalAddr().(*net.UDPAddr)

	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := localConn.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := localConn.WriteTo(buf[:n], addr); err != nil {
				return
			}
		}
	}()

	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	quicConfig := getOptimizedQuicConfig()

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
			TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
			Protocol:    "udp",
			Quic:        quicConfig,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	go server.Start(ctx, serverConfig)
	time.Sleep(300 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "udp-test-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"}},
		},
		Local: config.LocalService{Host: "127.0.0.1", Port: localAddr.Port},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		Quic: quicConfig,
	}

	c, err := client.New(clientConfig)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(300 * time.Millisecond)

	return localConn, trafficPort
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
