package e2e

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
// Resource Monitor
// ============================================

type resourceStats struct {
	avgCPUPercent float64
	maxCPUPercent float64
	avgMemoryMB   float64
	maxMemoryMB   float64
	samples       int
}

type resourceMonitor struct {
	stopCh    chan struct{}
	wg        sync.WaitGroup
	stats     resourceStats
	mu        sync.Mutex
	startTime time.Time

	// For CPU calculation
	lastCPUTime   time.Duration
	lastCheckTime time.Time
	cpuSamples    []float64
	memSamples    []float64
}

func newResourceMonitor() *resourceMonitor {
	return &resourceMonitor{
		stopCh:     make(chan struct{}),
		cpuSamples: make([]float64, 0, 100),
		memSamples: make([]float64, 0, 100),
	}
}

func (rm *resourceMonitor) start() {
	rm.startTime = time.Now()
	rm.lastCheckTime = time.Now()

	// Get initial CPU time
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	rm.wg.Go(func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-rm.stopCh:
				return
			case <-ticker.C:
				rm.sample()
			}
		}
	})
}

func (rm *resourceMonitor) sample() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	// Memory in MB
	memMB := float64(m.Alloc) / 1024 / 1024

	// CPU: estimate based on GC CPU fraction and goroutine count
	// This is an approximation since Go doesn't expose per-process CPU directly
	// Use NumGoroutine as a proxy for activity level
	numGoroutines := runtime.NumGoroutine()
	numCPU := runtime.NumCPU()

	// Estimate CPU usage based on goroutines and GC
	// This is a rough estimate - actual CPU would need OS-specific calls
	cpuEstimate := float64(numGoroutines) / float64(numCPU) * 10 // rough scaling
	if cpuEstimate > 100 {
		cpuEstimate = 100
	}

	// Also factor in GC CPU fraction
	gcCPU := m.GCCPUFraction * 100
	cpuEstimate = cpuEstimate + gcCPU

	rm.mu.Lock()
	rm.cpuSamples = append(rm.cpuSamples, cpuEstimate)
	rm.memSamples = append(rm.memSamples, memMB)
	rm.mu.Unlock()
}

func (rm *resourceMonitor) stop() resourceStats {
	close(rm.stopCh)
	rm.wg.Wait()

	rm.mu.Lock()
	defer rm.mu.Unlock()

	if len(rm.cpuSamples) == 0 {
		return resourceStats{}
	}

	var totalCPU, totalMem float64
	var maxCPU, maxMem float64

	for _, cpu := range rm.cpuSamples {
		totalCPU += cpu
		if cpu > maxCPU {
			maxCPU = cpu
		}
	}

	for _, mem := range rm.memSamples {
		totalMem += mem
		if mem > maxMem {
			maxMem = mem
		}
	}

	return resourceStats{
		avgCPUPercent: totalCPU / float64(len(rm.cpuSamples)),
		maxCPUPercent: maxCPU,
		avgMemoryMB:   totalMem / float64(len(rm.memSamples)),
		maxMemoryMB:   maxMem,
		samples:       len(rm.cpuSamples),
	}
}

// ============================================
// Benchmark Result with Resources
// ============================================

type benchResultWithResources struct {
	name       string
	sentMbps   float64
	recvMbps   float64
	avgCPU     float64
	maxCPU     float64
	avgMemMB   float64
	maxMemMB   float64
	goRoutines int
}

func appendBenchmarkResult(
	results *[]benchResultWithResources,
	name string,
	result *iperf3Result,
	stats resourceStats,
) {
	if result == nil {
		return
	}
	*results = append(*results, benchResultWithResources{
		name:       name,
		sentMbps:   result.End.SumSent.BitsPerSecond / 1e6,
		recvMbps:   result.End.SumReceived.BitsPerSecond / 1e6,
		avgCPU:     stats.avgCPUPercent,
		maxCPU:     stats.maxCPUPercent,
		avgMemMB:   stats.avgMemoryMB,
		maxMemMB:   stats.maxMemoryMB,
		goRoutines: runtime.NumGoroutine(),
	})
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

func reportIperf3ResultWithResources(t *testing.T, label string, result *iperf3Result, protocol string, stats resourceStats) {
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
		t.Logf("%s: %.2f Mbps sent, %.2f Mbps recv (%.2f MB sent, %.2f MB recv, jitter: %.3fms, loss: %.2f%%) | CPU: avg=%.1f%% max=%.1f%% | Mem: avg=%.1fMB max=%.1fMB",
			label, sentMbps, recvMbps, sentMB, recvMB, stream.UDP.JitterMs, lossPercent,
			stats.avgCPUPercent, stats.maxCPUPercent, stats.avgMemoryMB, stats.maxMemoryMB)
	} else {
		t.Logf("%s: %.2f Mbps sent, %.2f Mbps recv (%.2f MB sent, %.2f MB recv) | CPU: avg=%.1f%% max=%.1f%% | Mem: avg=%.1fMB max=%.1fMB",
			label, sentMbps, recvMbps, sentMB, recvMB,
			stats.avgCPUPercent, stats.maxCPUPercent, stats.avgMemoryMB, stats.maxMemoryMB)
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

	certDir := generateTestCertificates(t)

	t.Log("=== Comprehensive iperf3 Benchmark with Resource Monitoring ===")
	t.Log("")

	var results []benchResultWithResources

	// TCP Baseline
	t.Run("TCP_Baseline", func(t *testing.T) {
		for _, threads := range []int{1, 2, 4} {
			t.Run(fmt.Sprintf("%dThread", threads), func(t *testing.T) {
				result, stats := runIperf3BaselineWithResources(t, "tcp", threads)
				appendBenchmarkResult(&results, fmt.Sprintf("TCP Baseline %d-thread", threads), result, stats)
			})
		}
	})

	// TCP QMux
	t.Run("TCP_QMux", func(t *testing.T) {
		for _, threads := range []int{1, 2, 4} {
			t.Run(fmt.Sprintf("%dThread", threads), func(t *testing.T) {
				result, stats := runIperf3QMuxWithResources(t, certDir, "tcp", threads)
				appendBenchmarkResult(&results, fmt.Sprintf("TCP QMux %d-thread", threads), result, stats)
			})
		}
	})

	// UDP Baseline
	t.Run("UDP_Baseline", func(t *testing.T) {
		for _, threads := range []int{1, 2} {
			t.Run(fmt.Sprintf("%dThread", threads), func(t *testing.T) {
				result, stats := runIperf3BaselineWithResources(t, "udp", threads)
				appendBenchmarkResult(&results, fmt.Sprintf("UDP Baseline %d-thread", threads), result, stats)
			})
		}
	})

	// UDP QMux
	t.Run("UDP_QMux", func(t *testing.T) {
		for _, threads := range []int{1, 2} {
			t.Run(fmt.Sprintf("%dThread", threads), func(t *testing.T) {
				result, stats := runIperf3QMuxWithResources(t, certDir, "udp", threads)
				appendBenchmarkResult(&results, fmt.Sprintf("UDP QMux %d-thread", threads), result, stats)
			})
		}
	})

	// Print summary table
	printBenchmarkSummary(t, results)
}

func printBenchmarkSummary(t *testing.T, results []benchResultWithResources) {
	t.Log("")
	t.Log("╔════════════════════════════════════════════════════════════════════════════════════════════════╗")
	t.Log("║                              Benchmark Results with Resource Usage                             ║")
	t.Log("╠═════════════════════════╦═══════════════╦═══════════════╦═══════════════╦══════════════════════╣")
	t.Log("║ Test                    ║ Recv (Mbps)   ║ Avg CPU (%)   ║ Max CPU (%)   ║ Avg/Max Mem (MB)     ║")
	t.Log("╠═════════════════════════╬═══════════════╬═══════════════╬═══════════════╬══════════════════════╣")
	for _, r := range results {
		t.Logf("║ %-23s ║ %13.2f ║ %13.1f ║ %13.1f ║ %8.1f / %-8.1f ║",
			r.name, r.recvMbps, r.avgCPU, r.maxCPU, r.avgMemMB, r.maxMemMB)
	}
	t.Log("╚═════════════════════════╩═══════════════╩═══════════════╩═══════════════╩══════════════════════╝")
}

func runIperf3BaselineWithResources(t *testing.T, protocol string, threads int) (*iperf3Result, resourceStats) {
	serverPort := getFreePort(t)

	// Start resource monitor
	monitor := newResourceMonitor()
	monitor.start()

	serverCmd := newIperf3ServerCommand(serverPort)
	if err := serverCmd.Start(); err != nil {
		monitor.stop()
		t.Errorf("failed to start iperf3 server: %v", err)
		return nil, resourceStats{}
	}
	t.Cleanup(func() {
		_ = serverCmd.Process.Kill()
	})

	time.Sleep(500 * time.Millisecond)

	clientCmd := exec.Command("iperf3", iperf3ClientArgs(serverPort, 5, threads, protocol, true)...)
	output, err := clientCmd.Output()

	stats := monitor.stop()

	if err != nil {
		t.Errorf("iperf3 client failed: %v", err)
		return nil, stats
	}

	result := parseIperf3Output(t, output)
	reportIperf3ResultWithResources(t, fmt.Sprintf("Baseline %s %d-thread", strings.ToUpper(protocol), threads), result, protocol, stats)
	return result, stats
}

func runIperf3QMuxWithResources(t *testing.T, certDir string, protocol string, threads int) (*iperf3Result, resourceStats) {
	localPort := getFreePort(t)
	startIperf3Server(t, localPort)
	time.Sleep(300 * time.Millisecond)

	qmuxProtocol := protocol
	if protocol == "udp" {
		qmuxProtocol = "both"
	}
	trafficPort := setupQMuxEndpoint(t, certDir, qmuxProtocol, fmt.Sprintf("iperf3-%s-%d", protocol, threads), localPort,
		2*time.Minute, 300*time.Millisecond, 500*time.Millisecond, false)
	if trafficPort == 0 {
		return nil, resourceStats{}
	}

	// Start resource monitor AFTER QMux is set up, to measure only during transfer
	monitor := newResourceMonitor()
	monitor.start()

	clientCmd := exec.Command("iperf3", iperf3ClientArgs(trafficPort, 5, threads, protocol, true)...)
	output, err := clientCmd.Output()

	stats := monitor.stop()

	if err != nil {
		logIperf3ExitError(t, err)
		t.Errorf("iperf3 client through QMux failed: %v", err)
		return nil, stats
	}

	result := parseIperf3Output(t, output)
	reportIperf3ResultWithResources(t, fmt.Sprintf("QMux %s %d-thread", strings.ToUpper(protocol), threads), result, protocol, stats)
	return result, stats
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

	localPort := getFreePort(t)
	startIperf3Server(t, localPort)
	time.Sleep(300 * time.Millisecond)

	trafficPort := setupQMuxEndpoint(t, certDir, "tcp", "iperf3-live-client", localPort,
		2*time.Minute, 300*time.Millisecond, 500*time.Millisecond, true)

	clientCmd := exec.Command("iperf3", iperf3ClientArgs(trafficPort, 10, 2, "tcp", false)...)

	stdout, err := clientCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("failed to get stdout pipe: %v", err)
	}

	if err := clientCmd.Start(); err != nil {
		t.Fatalf("failed to start iperf3: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		t.Log(scanner.Text())
	}

	if err := clientCmd.Wait(); err != nil {
		t.Logf("iperf3 finished with: %v", err)
	}
}
