package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/server/traffic"
)

const stab007HeldFlows = 100

type stab007Attempt struct {
	conn      net.Conn
	connected bool
}

type stab007ResourcePoint struct {
	Workload         int                          `json:"workload"`
	Connected        int                          `json:"connected"`
	Verified         int                          `json:"verified"`
	DialFailures     int                          `json:"dial_failures"`
	Goroutines       int                          `json:"goroutines"`
	LinuxFDs         *int                         `json:"linux_fds,omitempty"`
	RSSBytes         uint64                       `json:"rss_bytes"`
	TCPAdmission     traffic.TCPAdmissionSnapshot `json:"tcp_admission"`
	TerminalAccepted uint64                       `json:"terminal_accepted"`
}

type stab007Result struct {
	SchemaVersion string                 `json:"schema_version"`
	GOOS          string                 `json:"goos"`
	GOARCH        string                 `json:"goarch"`
	GoVersion     string                 `json:"go_version"`
	Sampler       string                 `json:"sampler"`
	Points        []stab007ResourcePoint `json:"points"`
}

func TestTCPAdmissionCapacityBurstRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping STAB-007 real-socket capacity runner in short mode")
	}
	sampler, err := newLiveResourceSampler()
	if err != nil {
		t.Skip(err)
	}
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	resourceProcess := &benchmarkProcess{
		role: "stab-007-runner",
		cmd:  &exec.Cmd{Process: self},
		done: make(chan struct{}),
	}

	certDir := generateTestCertificates(t)
	_, backendPort := startTCPEchoListener(t)
	quicPort, trafficPort := getFreePort(t), getFreePort(t)
	trafficAddress := fmt.Sprintf("127.0.0.1:%d", trafficPort)
	testCtx, cancelTest := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelTest()
	timeline := newFaultTimeline(t, "STAB-007 TCP capacity")
	serverRun := startFaultServer(t, testCtx, "capacity-server",
		newMTLSServerConfig(certDir, "tcp", quicPort, trafficPort, time.Second, 10*time.Second), timeline)
	clientRun := startFaultClient(testCtx, "capacity-client", newTestClient(t,
		newMTLSClientConfig(certDir, "capacity-client", backendPort, time.Second, 0, quicPort)), timeline)
	t.Cleanup(func() {
		_ = clientRun.stopAndJoin(10 * time.Second)
		_ = serverRun.run.stopAndJoin(10 * time.Second)
	})

	if err := waitForFault(testCtx, 15*time.Second,
		func() string {
			return fmt.Sprintf("one TCP client and backend echo; snapshot=%+v", serverRun.Snapshot())
		},
		func(remaining time.Duration) bool {
			snapshot := serverRun.Snapshot()
			return len(snapshot.Routes) == 1 && snapshot.Routes[0].TCPEligibleClients == 1 &&
				probeSequencedTCP(trafficAddress, 0, min(remaining, time.Second)) == nil
		}, serverRun.run, clientRun); err != nil {
		t.Fatal(err)
	}
	baseline := waitSTAB007Snapshot(t, serverRun, 0, 15*time.Second)
	terminalBaseline := stab007TerminalTotal(baseline)
	result := stab007Result{
		SchemaVersion: "stab-007/v1",
		GOOS:          runtime.GOOS,
		GOARCH:        runtime.GOARCH,
		GoVersion:     runtime.Version(),
		Sampler:       sampler.kind,
	}
	warm := sampleSTAB007Point(t, sampler, resourceProcess, 0, 0, 0, 0, baseline)
	result.Points = append(result.Points, warm)

	held := make([]net.Conn, 0, stab007HeldFlows)
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	for sequence := 1; sequence <= stab007HeldFlows; sequence++ {
		conn, err := openSTAB007VerifiedFlow(trafficAddress, uint64(sequence), 5*time.Second)
		if err != nil {
			if conn != nil {
				_ = conn.Close()
			}
			t.Fatalf("establish held flow %d/%d: %v", sequence, stab007HeldFlows, err)
		}
		held = append(held, conn)
	}
	snapshot := waitSTAB007Snapshot(t, serverRun, stab007HeldFlows, 15*time.Second)
	if got := stab007TerminalTotal(snapshot) - terminalBaseline; got != stab007HeldFlows {
		t.Fatalf("accepted terminals after 100 verified flows = %d, want %d; snapshot=%+v", got, stab007HeldFlows, snapshot)
	}
	if snapshot.ActiveHighWater < stab007HeldFlows {
		t.Fatalf("TCP active high-water = %d, want at least %d", snapshot.ActiveHighWater, stab007HeldFlows)
	}
	result.Points = append(result.Points, sampleSTAB007Point(t, sampler, resourceProcess,
		stab007HeldFlows, stab007HeldFlows, stab007HeldFlows, 0, snapshot))

	for _, workload := range []int{101, 200, 500} {
		extra := workload - stab007HeldFlows
		before := serverRun.Snapshot().Routes[0].TCPAdmission
		attempts := runSTAB007Attempts(trafficAddress, workload, extra)
		var connected, verified int
		for _, attempt := range attempts {
			if attempt.connected {
				connected++
			}
			if attempt.conn != nil {
				verified++
				_ = attempt.conn.Close()
			}
		}
		snapshot = waitSTAB007Settled(t, serverRun, stab007HeldFlows, 15*time.Second)
		accepted := stab007TerminalTotal(snapshot) - stab007TerminalTotal(before)
		peerLimit := snapshot.PeerStreamLimit - before.PeerStreamLimit
		generationCapacity := snapshot.GenerationCapacity - before.GenerationCapacity
		if connected != extra || verified != 0 || accepted != uint64(extra) ||
			peerLimit+generationCapacity != uint64(extra) || workload == 101 && (peerLimit != 1 || generationCapacity != 0) {
			t.Fatalf("workload %d extra connected/verified/terminal/peer-limit/generation-capacity = %d/%d/%d/%d/%d, want %d/0/%d/reconciled; snapshot=%+v",
				workload, connected, verified, accepted, peerLimit, generationCapacity, extra, extra, snapshot)
		}
		result.Points = append(result.Points, sampleSTAB007Point(t, sampler, resourceProcess,
			workload, stab007HeldFlows+connected, stab007HeldFlows+verified, extra-connected, snapshot))

		verifySTAB007HeldFlows(t, held, uint64(workload)*1_000)
		_ = waitSTAB007Snapshot(t, serverRun, stab007HeldFlows, 15*time.Second)
	}

	for _, conn := range held {
		_ = conn.Close()
	}
	held = nil
	snapshot = waitSTAB007Snapshot(t, serverRun, 0, 15*time.Second)
	waitSTAB007Goroutines(t, warm.Goroutines, 15*time.Second)
	teardown := sampleSTAB007Point(t, sampler, resourceProcess, 0, 0, 0, 0, snapshot)
	if teardown.Goroutines > warm.Goroutines {
		t.Fatalf("teardown goroutines = %d, want at most warm baseline %d", teardown.Goroutines, warm.Goroutines)
	}
	result.Points = append(result.Points, teardown)

	cold, err := openSTAB007VerifiedFlow(trafficAddress, 999_999, 5*time.Second)
	if err != nil {
		t.Fatalf("cold flow after overload teardown: %v", err)
	}
	snapshot = waitSTAB007Snapshot(t, serverRun, 1, 15*time.Second)
	result.Points = append(result.Points, sampleSTAB007Point(t, sampler, resourceProcess,
		1, 1, 1, 0, snapshot))
	_ = cold.Close()
	_ = waitSTAB007Snapshot(t, serverRun, 0, 15*time.Second)

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("STAB007_RAW %s", encoded)
}

func runSTAB007Attempts(address string, workload, count int) []stab007Attempt {
	start := make(chan struct{})
	results := make(chan stab007Attempt, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			<-start
			conn, err := openSTAB007VerifiedFlow(address, uint64(workload*1_000_000+i), 8*time.Second)
			if err != nil {
				results <- stab007Attempt{connected: conn != nil}
				if conn != nil {
					_ = conn.Close()
				}
				return
			}
			results <- stab007Attempt{conn: conn, connected: true}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	attempts := make([]stab007Attempt, 0, count)
	for result := range results {
		attempts = append(attempts, result)
	}
	return attempts
}

func openSTAB007VerifiedFlow(address string, sequence uint64, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return conn, err
	}
	payload := sequencePayload(sequence)
	if n, err := conn.Write(payload); err != nil {
		return conn, err
	} else if n != len(payload) {
		return conn, io.ErrShortWrite
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echo); err != nil {
		return conn, err
	}
	if !bytes.Equal(echo, payload) {
		return conn, fmt.Errorf("echo mismatch: got %x, want %x", echo, payload)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return conn, err
	}
	return conn, nil
}

func verifySTAB007HeldFlows(t *testing.T, held []net.Conn, base uint64) {
	t.Helper()
	for i, conn := range held {
		if err := verifySTAB007Flow(conn, base+uint64(i), 5*time.Second); err != nil {
			t.Fatalf("verify held flow %d/%d: %v", i+1, len(held), err)
		}
	}
}

func verifySTAB007Flow(conn net.Conn, sequence uint64, timeout time.Duration) error {
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	payload := sequencePayload(sequence)
	if n, err := conn.Write(payload); err != nil {
		return err
	} else if n != len(payload) {
		return io.ErrShortWrite
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echo); err != nil {
		return err
	}
	if !bytes.Equal(echo, payload) {
		return fmt.Errorf("echo mismatch: got %x, want %x", echo, payload)
	}
	return conn.SetDeadline(time.Time{})
}

func waitSTAB007Snapshot(t *testing.T, run *faultServerRun, active int64, timeout time.Duration) traffic.TCPAdmissionSnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snapshot := run.Snapshot().Routes[0].TCPAdmission
		if snapshot.SetupCurrent == 0 && snapshot.ActiveCurrent == active {
			return snapshot
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot := run.Snapshot().Routes[0].TCPAdmission
	t.Fatalf("TCP admission did not settle at setup=0 active=%d within %s: %+v", active, timeout, snapshot)
	return traffic.TCPAdmissionSnapshot{}
}

func waitSTAB007Settled(t *testing.T, run *faultServerRun, active int64, timeout time.Duration) traffic.TCPAdmissionSnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var stableSince time.Time
	var previous uint64
	for time.Now().Before(deadline) {
		snapshot := run.Snapshot().Routes[0].TCPAdmission
		terminals := stab007TerminalTotal(snapshot)
		if snapshot.SetupCurrent == 0 && snapshot.ActiveCurrent == active {
			if stableSince.IsZero() || terminals != previous {
				stableSince = time.Now()
				previous = terminals
			} else if time.Since(stableSince) >= 200*time.Millisecond {
				return snapshot
			}
		} else {
			stableSince = time.Time{}
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot := run.Snapshot().Routes[0].TCPAdmission
	t.Fatalf("TCP admission did not settle at setup=0 active=%d within %s: %+v", active, timeout, snapshot)
	return traffic.TCPAdmissionSnapshot{}
}

func stab007TerminalTotal(snapshot traffic.TCPAdmissionSnapshot) uint64 {
	return snapshot.Committed + snapshot.ListenerCapacity + snapshot.Unavailable + snapshot.GenerationCapacity +
		snapshot.PeerStreamLimit + snapshot.Deadline + snapshot.SetupFailure + snapshot.Canceled
}

func waitSTAB007Goroutines(t *testing.T, baseline int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines = %d, want at most warm baseline %d within %s", runtime.NumGoroutine(), baseline, timeout)
}

func sampleSTAB007Point(
	t *testing.T,
	sampler *liveResourceSampler,
	process *benchmarkProcess,
	workload, connected, verified, dialFailures int,
	snapshot traffic.TCPAdmissionSnapshot,
) stab007ResourcePoint {
	t.Helper()
	points, err := sampler.sample([]*benchmarkProcess{process})
	if err != nil {
		t.Fatal(err)
	}
	point := stab007ResourcePoint{
		Workload: workload, Connected: connected, Verified: verified, DialFailures: dialFailures,
		Goroutines: runtime.NumGoroutine(), RSSBytes: points[0].ResidentByte,
		TCPAdmission: snapshot, TerminalAccepted: stab007TerminalTotal(snapshot),
	}
	if runtime.GOOS == "linux" {
		entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", os.Getpid()))
		if err != nil {
			t.Fatal(err)
		}
		count := len(entries)
		point.LinuxFDs = &count
	}
	return point
}
