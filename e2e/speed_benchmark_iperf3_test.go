package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
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
		Sum struct {
			JitterMs    float64 `json:"jitter_ms"`
			LostPackets int     `json:"lost_packets"`
			Packets     int     `json:"packets"`
		} `json:"sum"`
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

func newIperf3ServerCommand(port int) *exec.Cmd {
	serverCmd := exec.Command("iperf3", "-s", "-p", strconv.Itoa(port), "-1")
	serverCmd.Stdout = io.Discard
	serverCmd.Stderr = io.Discard
	return serverCmd
}

func startIperf3Server(t testing.TB, port int) {
	t.Helper()
	serverCmd := newIperf3ServerCommand(port)
	if err := serverCmd.Start(); err != nil {
		t.Fatalf("start iperf3 server: %v", err)
	}
	t.Cleanup(func() {
		_ = serverCmd.Process.Kill()
	})
}

func iperf3ClientArgs(port, durationSeconds, threads int, protocol string, jsonOutput bool) []string {
	args := []string{
		"-c", "127.0.0.1",
		"-p", strconv.Itoa(port),
		"-t", strconv.Itoa(durationSeconds),
		"-P", strconv.Itoa(threads),
	}
	if jsonOutput {
		args = append(args, "-J")
	}
	if protocol == "udp" {
		args = append(args, "-u", "-b", "0")
	}
	return args
}

func logIperf3ExitError(t testing.TB, err error) {
	t.Helper()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		t.Logf("iperf3 stderr: %s", string(exitErr.Stderr))
	}
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

	t.Log("=== QMux iperf3 Speed Report ===")
	t.Log("")

	// TCP Tests
	t.Run("TCP", func(t *testing.T) {
		t.Run("Baseline_Direct_1Thread", func(t *testing.T) {
			runIperf3DirectBaseline(t, "tcp", 1)
		})

		t.Run("Baseline_Direct_2Thread", func(t *testing.T) {
			runIperf3DirectBaseline(t, "tcp", 2)
		})

		t.Run("QMux_1Thread", func(t *testing.T) {
			runIperf3ThroughQMux(t, certDir, "tcp", 1)
		})

		t.Run("QMux_2Thread", func(t *testing.T) {
			runIperf3ThroughQMux(t, certDir, "tcp", 2)
		})
	})

	// UDP Tests
	t.Run("UDP", func(t *testing.T) {
		t.Run("Baseline_Direct_1Thread", func(t *testing.T) {
			runIperf3DirectBaseline(t, "udp", 1)
		})

		t.Run("Baseline_Direct_2Thread", func(t *testing.T) {
			runIperf3DirectBaseline(t, "udp", 2)
		})

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
	startIperf3Server(t, serverPort)

	time.Sleep(500 * time.Millisecond)

	clientCmd := exec.Command("iperf3", iperf3ClientArgs(serverPort, 5, threads, protocol, true)...)
	output, err := clientCmd.Output()
	if err != nil {
		t.Fatalf("iperf3 client failed: %v", err)
	}

	result := parseIperf3Output(t, output)
	reportIperf3Result(t, fmt.Sprintf("Direct %s %d-thread", strings.ToUpper(protocol), threads), result, protocol)
}

// ============================================
// iperf3 Through QMux
// ============================================

func runIperf3ThroughQMux(t *testing.T, certDir string, protocol string, threads int) {
	localPort := getFreePort(t)
	startIperf3Server(t, localPort)
	time.Sleep(300 * time.Millisecond)

	qmuxProtocol := protocol
	if protocol == "udp" {
		qmuxProtocol = "both"
	}
	trafficPort := setupQMuxEndpoint(t, certDir, qmuxProtocol, fmt.Sprintf("iperf3-%s-client", protocol), localPort,
		2*time.Minute, 300*time.Millisecond, 500*time.Millisecond, true)

	clientCmd := exec.Command("iperf3", iperf3ClientArgs(trafficPort, 5, threads, protocol, true)...)
	output, err := clientCmd.Output()
	if err != nil {
		logIperf3ExitError(t, err)
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
// Comprehensive iperf3 Benchmark with Resources
// ============================================

func TestIperf3ComprehensiveBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping comprehensive iperf3 benchmark in short mode")
	}

	if !iperf3Available() {
		t.Skip("iperf3 not available")
	}

	runPERF003Matrix(t)
}
