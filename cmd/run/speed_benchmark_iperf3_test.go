package run

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server"
)

// ============================================
// iperf3 Result Structures
// ============================================

type iperf3Result struct {
	End struct {
		SumSent struct {
			BitsPerSecond float64 `json:"bits_per_second"`
			Bytes         int64   `json:"bytes"`
		} `json:"sum_sent"`
		SumReceived struct {
			BitsPerSecond float64 `json:"bits_per_second"`
			Bytes         int64   `json:"bytes"`
		} `json:"sum_received"`
		Streams []struct {
			UDP struct {
				JitterMs    float64 `json:"jitter_ms"`
				LostPackets int     `json:"lost_packets"`
				Packets     int     `json:"packets"`
			} `json:"udp"`
		} `json:"streams"`
	} `json:"end"`
}

// ============================================
// iperf3 Availability Check
// ============================================

func iperf3Available() bool {
	_, err := exec.LookPath("iperf3")
	return err == nil
}

// ============================================
// iperf3 Speed Report Test
// ============================================

func TestIperf3SpeedReport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping iperf3 speed report in short mode")
	}

	if !iperf3Available() {
		t.Skip("iperf3 not available, skipping iperf3 benchmarks")
	}

	certDir := generateTestCertificates(t)
	defer os.RemoveAll(certDir)

	t.Log("=== QMux iperf3 Speed Report ===")
	t.Log("")

	// TCP Tests
	t.Run("TCP", func(t *testing.T) {
		// Baseline: Direct iperf3 without QMux
		t.Run("Baseline_Direct_1Thread", func(t *testing.T) {
			runIperf3DirectBaseline(t, "tcp", 1)
		})

		t.Run("Baseline_Direct_2Thread", func(t *testing.T) {
			runIperf3DirectBaseline(t, "tcp", 2)
		})

		// QMux TCP tests
		t.Run("QMux_1Thread", func(t *testing.T) {
			runIperf3ThroughQMux(t, certDir, "tcp", 1)
		})

		t.Run("QMux_2Thread", func(t *testing.T) {
			runIperf3ThroughQMux(t, certDir, "tcp", 2)
		})
	})

	// UDP Tests
	t.Run("UDP", func(t *testing.T) {
		// Baseline: Direct iperf3 without QMux
		t.Run("Baseline_Direct_1Thread", func(t *testing.T) {
			runIperf3DirectBaseline(t, "udp", 1)
		})

		t.Run("Baseline_Direct_2Thread", func(t *testing.T) {
			runIperf3DirectBaseline(t, "udp", 2)
		})

		// QMux UDP tests
		t.Run("QMux_1Thread", func(t *testing.T) {
			runIperf3ThroughQMux(t, certDir, "udp", 1)
		})

		t.Run("QMux_2Thread", func(t *testing.T) {
			runIperf3ThroughQMux(t, certDir, "udp", 2)
		})
	})
}

// ============================================
// Direct iperf3 Baseline (No QMux)
// ============================================

func runIperf3DirectBaseline(t *testing.T, protocol string, threads int) {
	serverPort := getFreePort(t)

	// Start iperf3 server
	serverArgs := []string{"-s", "-p", strconv.Itoa(serverPort), "-1"} // -1 = one-off mode
	serverCmd := exec.Command("iperf3", serverArgs...)
	serverCmd.Stdout = io.Discard
	serverCmd.Stderr = io.Discard

	if err := serverCmd.Start(); err != nil {
		t.Fatalf("failed to start iperf3 server: %v", err)
	}
	defer serverCmd.Process.Kill()

	// Wait for server to be ready
	time.Sleep(500 * time.Millisecond)

	// Run iperf3 client
	clientArgs := []string{
		"-c", "127.0.0.1",
		"-p", strconv.Itoa(serverPort),
		"-t", "5", // 5 second test
		"-P", strconv.Itoa(threads), // parallel streams
		"-J", // JSON output
	}

	if protocol == "udp" {
		clientArgs = append(clientArgs, "-u", "-b", "0") // UDP mode, unlimited bandwidth
	}

	clientCmd := exec.Command("iperf3", clientArgs...)
	output, err := clientCmd.Output()
	if err != nil {
		t.Fatalf("iperf3 client failed: %v", err)
	}

	// Parse and report results
	result := parseIperf3Output(t, output)
	reportIperf3Result(t, fmt.Sprintf("Direct %s %d-thread", strings.ToUpper(protocol), threads), result, protocol)
}

// ============================================
// iperf3 Through QMux
// ============================================

func runIperf3ThroughQMux(t *testing.T, certDir string, protocol string, threads int) {
	// Start local iperf3 server (this is what QMux client connects to)
	localPort := getFreePort(t)
	serverArgs := []string{"-s", "-p", strconv.Itoa(localPort), "-1"}
	serverCmd := exec.Command("iperf3", serverArgs...)
	serverCmd.Stdout = io.Discard
	serverCmd.Stderr = io.Discard

	if err := serverCmd.Start(); err != nil {
		t.Fatalf("failed to start iperf3 server: %v", err)
	}
	defer serverCmd.Process.Kill()

	time.Sleep(300 * time.Millisecond)

	// Setup QMux infrastructure
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	quicConfig := getOptimizedQuicConfig()

	// Use "both" protocol mode for UDP tests since iperf3 needs TCP control connection
	qmuxProtocol := protocol
	if protocol == "udp" {
		qmuxProtocol = "both"
	}

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			Listen:      config.Listen{IP: "127.0.0.1", Port: quicPort},
			TrafficPort: trafficPort,
			Protocol:    qmuxProtocol,
			Quic:        quicConfig,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	go server.Start(ctx, serverConfig)
	time.Sleep(300 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: fmt.Sprintf("iperf3-%s-client", protocol),
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
		t.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(500 * time.Millisecond)

	// Run iperf3 client through QMux traffic port
	clientArgs := []string{
		"-c", "127.0.0.1",
		"-p", strconv.Itoa(trafficPort),
		"-t", "5",
		"-P", strconv.Itoa(threads),
		"-J",
	}

	if protocol == "udp" {
		clientArgs = append(clientArgs, "-u", "-b", "0")
	}

	clientCmd := exec.Command("iperf3", clientArgs...)
	output, err := clientCmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Logf("iperf3 stderr: %s", string(exitErr.Stderr))
		}
		t.Fatalf("iperf3 client through QMux failed: %v", err)
	}

	result := parseIperf3Output(t, output)
	reportIperf3Result(t, fmt.Sprintf("QMux %s %d-thread", strings.ToUpper(protocol), threads), result, protocol)
}

// ============================================
// iperf3 Output Parsing and Reporting
// ============================================

func parseIperf3Output(t *testing.T, output []byte) *iperf3Result {
	var result iperf3Result
	if err := json.Unmarshal(output, &result); err != nil {
		t.Logf("Raw iperf3 output: %s", string(output))
		t.Fatalf("failed to parse iperf3 JSON output: %v", err)
	}
	return &result
}

func reportIperf3Result(t *testing.T, label string, result *iperf3Result, protocol string) {
	sentMbps := result.End.SumSent.BitsPerSecond / 1e6
	recvMbps := result.End.SumReceived.BitsPerSecond / 1e6
	sentMB := float64(result.End.SumSent.Bytes) / 1024 / 1024
	recvMB := float64(result.End.SumReceived.Bytes) / 1024 / 1024

	if protocol == "udp" && len(result.End.Streams) > 0 {
		stream := result.End.Streams[0]
		lossPercent := float64(0)
		if stream.UDP.Packets > 0 {
			lossPercent = float64(stream.UDP.LostPackets) / float64(stream.UDP.Packets) * 100
		}
		t.Logf("%s: %.2f Mbps sent, %.2f Mbps recv (%.2f MB sent, %.2f MB recv, jitter: %.3fms, loss: %.2f%%)",
			label, sentMbps, recvMbps, sentMB, recvMB, stream.UDP.JitterMs, lossPercent)
	} else {
		t.Logf("%s: %.2f Mbps sent, %.2f Mbps recv (%.2f MB sent, %.2f MB recv)",
			label, sentMbps, recvMbps, sentMB, recvMB)
	}
}

// ============================================
// Comprehensive iperf3 Benchmark
// ============================================

func TestIperf3ComprehensiveBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping comprehensive iperf3 benchmark in short mode")
	}

	if !iperf3Available() {
		t.Skip("iperf3 not available")
	}

	certDir := generateTestCertificates(t)
	defer os.RemoveAll(certDir)

	t.Log("=== Comprehensive iperf3 Benchmark ===")
	t.Log("")
	t.Log("Format: [Test Name]: Sent Mbps, Recv Mbps (Sent MB, Recv MB)")
	t.Log("")

	// Collect all results for summary
	type benchResult struct {
		name     string
		sentMbps float64
		recvMbps float64
	}
	var results []benchResult

	// TCP Baseline
	t.Run("TCP_Baseline", func(t *testing.T) {
		for _, threads := range []int{1, 2, 4} {
			t.Run(fmt.Sprintf("%dThread", threads), func(t *testing.T) {
				result := runIperf3BaselineWithResult(t, "tcp", threads)
				if result != nil {
					results = append(results, benchResult{
						name:     fmt.Sprintf("TCP Baseline %d-thread", threads),
						sentMbps: result.End.SumSent.BitsPerSecond / 1e6,
						recvMbps: result.End.SumReceived.BitsPerSecond / 1e6,
					})
				}
			})
		}
	})

	// TCP QMux
	t.Run("TCP_QMux", func(t *testing.T) {
		for _, threads := range []int{1, 2, 4} {
			t.Run(fmt.Sprintf("%dThread", threads), func(t *testing.T) {
				result := runIperf3QMuxWithResult(t, certDir, "tcp", threads)
				if result != nil {
					results = append(results, benchResult{
						name:     fmt.Sprintf("TCP QMux %d-thread", threads),
						sentMbps: result.End.SumSent.BitsPerSecond / 1e6,
						recvMbps: result.End.SumReceived.BitsPerSecond / 1e6,
					})
				}
			})
		}
	})

	// UDP Baseline
	t.Run("UDP_Baseline", func(t *testing.T) {
		for _, threads := range []int{1, 2} {
			t.Run(fmt.Sprintf("%dThread", threads), func(t *testing.T) {
				result := runIperf3BaselineWithResult(t, "udp", threads)
				if result != nil {
					results = append(results, benchResult{
						name:     fmt.Sprintf("UDP Baseline %d-thread", threads),
						sentMbps: result.End.SumSent.BitsPerSecond / 1e6,
						recvMbps: result.End.SumReceived.BitsPerSecond / 1e6,
					})
				}
			})
		}
	})

	// UDP QMux
	t.Run("UDP_QMux", func(t *testing.T) {
		for _, threads := range []int{1, 2} {
			t.Run(fmt.Sprintf("%dThread", threads), func(t *testing.T) {
				result := runIperf3QMuxWithResult(t, certDir, "udp", threads)
				if result != nil {
					results = append(results, benchResult{
						name:     fmt.Sprintf("UDP QMux %d-thread", threads),
						sentMbps: result.End.SumSent.BitsPerSecond / 1e6,
						recvMbps: result.End.SumReceived.BitsPerSecond / 1e6,
					})
				}
			})
		}
	})

	// Print summary
	t.Log("")
	t.Log("=== Summary ===")
	for _, r := range results {
		t.Logf("%-25s: %.2f Mbps (recv)", r.name, r.recvMbps)
	}
}

func runIperf3BaselineWithResult(t *testing.T, protocol string, threads int) *iperf3Result {
	serverPort := getFreePort(t)

	serverCmd := exec.Command("iperf3", "-s", "-p", strconv.Itoa(serverPort), "-1")
	serverCmd.Stdout = io.Discard
	serverCmd.Stderr = io.Discard

	if err := serverCmd.Start(); err != nil {
		t.Errorf("failed to start iperf3 server: %v", err)
		return nil
	}
	defer serverCmd.Process.Kill()

	time.Sleep(500 * time.Millisecond)

	clientArgs := []string{
		"-c", "127.0.0.1",
		"-p", strconv.Itoa(serverPort),
		"-t", "5",
		"-P", strconv.Itoa(threads),
		"-J",
	}

	if protocol == "udp" {
		clientArgs = append(clientArgs, "-u", "-b", "0")
	}

	clientCmd := exec.Command("iperf3", clientArgs...)
	output, err := clientCmd.Output()
	if err != nil {
		t.Errorf("iperf3 client failed: %v", err)
		return nil
	}

	result := parseIperf3Output(t, output)
	reportIperf3Result(t, fmt.Sprintf("Baseline %s %d-thread", strings.ToUpper(protocol), threads), result, protocol)
	return result
}

func runIperf3QMuxWithResult(t *testing.T, certDir string, protocol string, threads int) *iperf3Result {
	localPort := getFreePort(t)
	serverCmd := exec.Command("iperf3", "-s", "-p", strconv.Itoa(localPort), "-1")
	serverCmd.Stdout = io.Discard
	serverCmd.Stderr = io.Discard

	if err := serverCmd.Start(); err != nil {
		t.Errorf("failed to start iperf3 server: %v", err)
		return nil
	}
	defer serverCmd.Process.Kill()

	time.Sleep(300 * time.Millisecond)

	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)
	quicConfig := getOptimizedQuicConfig()

	// Use "both" protocol mode for UDP tests since iperf3 needs TCP control connection
	qmuxProtocol := protocol
	if protocol == "udp" {
		qmuxProtocol = "both"
	}

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			Listen:      config.Listen{IP: "127.0.0.1", Port: quicPort},
			TrafficPort: trafficPort,
			Protocol:    qmuxProtocol,
			Quic:        quicConfig,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	go server.Start(ctx, serverConfig)
	time.Sleep(300 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: fmt.Sprintf("iperf3-%s-%d", protocol, threads),
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
		t.Errorf("failed to create client: %v", err)
		return nil
	}

	go c.Start(ctx)
	time.Sleep(500 * time.Millisecond)

	clientArgs := []string{
		"-c", "127.0.0.1",
		"-p", strconv.Itoa(trafficPort),
		"-t", "5",
		"-P", strconv.Itoa(threads),
		"-J",
	}

	if protocol == "udp" {
		clientArgs = append(clientArgs, "-u", "-b", "0")
	}

	clientCmd := exec.Command("iperf3", clientArgs...)
	output, err := clientCmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Logf("iperf3 stderr: %s", string(exitErr.Stderr))
		}
		t.Errorf("iperf3 client through QMux failed: %v", err)
		return nil
	}

	result := parseIperf3Output(t, output)
	reportIperf3Result(t, fmt.Sprintf("QMux %s %d-thread", strings.ToUpper(protocol), threads), result, protocol)
	return result
}

// ============================================
// Live iperf3 Output Test (for debugging)
// ============================================

func TestIperf3LiveOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live output test in short mode")
	}

	if !iperf3Available() {
		t.Skip("iperf3 not available")
	}

	certDir := generateTestCertificates(t)
	defer os.RemoveAll(certDir)

	// Start local iperf3 server
	localPort := getFreePort(t)
	serverCmd := exec.Command("iperf3", "-s", "-p", strconv.Itoa(localPort), "-1")
	serverCmd.Stdout = io.Discard
	serverCmd.Stderr = io.Discard

	if err := serverCmd.Start(); err != nil {
		t.Fatalf("failed to start iperf3 server: %v", err)
	}
	defer serverCmd.Process.Kill()

	time.Sleep(300 * time.Millisecond)

	// Setup QMux
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)
	quicConfig := getOptimizedQuicConfig()

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			Listen:      config.Listen{IP: "127.0.0.1", Port: quicPort},
			TrafficPort: trafficPort,
			Protocol:    "tcp",
			Quic:        quicConfig,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	go server.Start(ctx, serverConfig)
	time.Sleep(300 * time.Millisecond)

	clientConfig := &config.Client{
		ClientID: "iperf3-live-client",
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
		t.Fatalf("failed to create client: %v", err)
	}

	go c.Start(ctx)
	time.Sleep(500 * time.Millisecond)

	// Run iperf3 with live output (no -J flag)
	clientCmd := exec.Command("iperf3",
		"-c", "127.0.0.1",
		"-p", strconv.Itoa(trafficPort),
		"-t", "10",
		"-P", "2",
	)

	stdout, err := clientCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("failed to get stdout pipe: %v", err)
	}

	if err := clientCmd.Start(); err != nil {
		t.Fatalf("failed to start iperf3: %v", err)
	}

	// Stream output
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		t.Log(scanner.Text())
	}

	if err := clientCmd.Wait(); err != nil {
		t.Logf("iperf3 finished with: %v", err)
	}
}
