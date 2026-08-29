package e2e

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

const (
	resourceSamplePeriod    = 100 * time.Millisecond
	benchmarkLogLimit       = 64 * 1024
	linuxATClockTicks       = 17
	perf003ReadyTimeout     = 15 * time.Second
	calibrationBurnTime     = time.Second
	calibrationQuantization = 30 * time.Millisecond
	calibrationEnvelope     = 100 * time.Millisecond
)

var (
	errPERF003SetupPolluted       = errors.New("PERF-003 cold setup was polluted by a client failure")
	errPERF003TerminalClientPoint = errors.New("PERF-003 terminal iperf client resource point")
)

type boundedLog struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *boundedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := benchmarkLogLimit - len(b.data)
	if remaining > 0 {
		b.data = append(b.data, p[:min(len(p), remaining)]...)
	}
	if len(p) > remaining {
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return string(b.data) + "\n[log truncated]"
	}
	return string(b.data)
}

type benchmarkProcess struct {
	role       string
	cmd        *exec.Cmd
	generation string
	log        boundedLog
	done       chan struct{}
	waitErr    error
	endedAt    time.Time

	stopOnce sync.Once
	stopErr  error
}

func startBenchmarkProcess(role string, cmd *exec.Cmd) (*benchmarkProcess, error) {
	process := &benchmarkProcess{role: role, cmd: cmd, done: make(chan struct{})}
	if cmd.Stdout == nil {
		cmd.Stdout = &process.log
	}
	cmd.Stderr = &process.log
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		process.waitErr = cmd.Wait()
		process.endedAt = time.Now()
		close(process.done)
	}()
	return process, nil
}

func (p *benchmarkProcess) expectAlive() error {
	select {
	case <-p.done:
		return fmt.Errorf("%s exited early: %v\n%s", p.role, p.waitErr, p.log.String())
	default:
		return nil
	}
}

func (p *benchmarkProcess) wait(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.waitErr
	case <-timer.C:
		return fmt.Errorf("timed out after %s waiting for %s", timeout, p.role)
	}
}

func (p *benchmarkProcess) waitDone(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

func (p *benchmarkProcess) stop(timeout time.Duration) error {
	p.stopOnce.Do(func() {
		select {
		case <-p.done:
			if p.waitErr != nil {
				p.stopErr = fmt.Errorf("%s exited before teardown: %w\n%s", p.role, p.waitErr, p.log.String())
			}
			return
		default:
		}
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			if errors.Is(err, os.ErrProcessDone) {
				if !p.waitDone(timeout) {
					p.stopErr = fmt.Errorf("stop %s: process did not finish after reporting done", p.role)
				} else {
					p.stopErr = p.expectedStopError()
				}
				return
			}
			p.stopErr = fmt.Errorf("signal %s: %w", p.role, err)
			return
		}
		if p.waitDone(timeout) {
			p.stopErr = p.expectedStopError()
		} else {
			_ = p.cmd.Process.Kill()
			if !p.waitDone(timeout) {
				p.stopErr = fmt.Errorf("stop %s: process did not exit after SIGTERM and kill", p.role)
			} else {
				p.stopErr = fmt.Errorf("stop %s: process ignored SIGTERM and required kill", p.role)
			}
		}
	})
	return p.stopErr
}

func (p *benchmarkProcess) expectedStopError() error {
	if strings.HasPrefix(p.role, "qmux-") && p.waitErr != nil {
		return fmt.Errorf("%s failed during SIGTERM teardown: %w\n%s", p.role, p.waitErr, p.log.String())
	}
	return nil
}

func (p *benchmarkProcess) cleanup(timeout time.Duration) {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	if p.waitDone(timeout) {
		return
	}
	_ = p.cmd.Process.Kill()
	_ = p.waitDone(timeout)
}

type processResourcePoint struct {
	Role         string `json:"role"`
	PID          int    `json:"pid"`
	Generation   string `json:"generation"`
	UserCPUNs    uint64 `json:"user_cpu_ns"`
	SystemCPUNs  uint64 `json:"system_cpu_ns"`
	ResidentByte uint64 `json:"rss_bytes"`
}

type resourceSampleSet struct {
	OffsetNs  int64                  `json:"offset_ns"`
	Processes []processResourcePoint `json:"processes"`
}

type liveResourceSampler struct {
	kind       string
	clockTicks uint64
}

func newLiveResourceSampler() (*liveResourceSampler, error) {
	switch runtime.GOOS {
	case "linux":
		clockTicks, err := readLinuxClockTicks("/proc/self/auxv")
		if err != nil {
			return nil, err
		}
		return &liveResourceSampler{kind: "linux-procfs", clockTicks: clockTicks}, nil
	case "darwin":
		return &liveResourceSampler{kind: "darwin-ps"}, nil
	default:
		return nil, fmt.Errorf("process resource sampling is unsupported on %s", runtime.GOOS)
	}
}

func (s *liveResourceSampler) sample(processes []*benchmarkProcess) ([]processResourcePoint, error) {
	for _, process := range processes {
		if err := process.expectAlive(); err != nil {
			return nil, err
		}
	}

	var (
		points []processResourcePoint
		err    error
	)
	switch s.kind {
	case "linux-procfs":
		points = make([]processResourcePoint, 0, len(processes))
		for _, process := range processes {
			point, sampleErr := readLinuxProcessPoint(process, s.clockTicks)
			if sampleErr != nil {
				return nil, sampleErr
			}
			points = append(points, point)
		}
	case "darwin-ps":
		points, err = readDarwinProcessPoints(processes)
	default:
		err = fmt.Errorf("unknown resource sampler %q", s.kind)
	}
	if err != nil {
		return nil, err
	}
	for i, point := range points {
		process := processes[i]
		if process.generation == "" {
			process.generation = point.Generation
		} else if process.generation != point.Generation {
			return nil, fmt.Errorf("%s PID %d changed generation from %q to %q", process.role,
				process.cmd.Process.Pid, process.generation, point.Generation)
		}
	}
	return points, nil
}

func readLinuxClockTicks(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read Linux auxv: %w", err)
	}
	wordSize := strconv.IntSize / 8
	for offset := 0; offset+2*wordSize <= len(data); offset += 2 * wordSize {
		var tag, value uint64
		if wordSize == 8 {
			tag = binary.NativeEndian.Uint64(data[offset:])
			value = binary.NativeEndian.Uint64(data[offset+wordSize:])
		} else {
			tag = uint64(binary.NativeEndian.Uint32(data[offset:]))
			value = uint64(binary.NativeEndian.Uint32(data[offset+wordSize:]))
		}
		if tag == 0 {
			break
		}
		if tag == linuxATClockTicks {
			if value == 0 {
				return 0, errors.New("linux AT_CLKTCK is zero")
			}
			return value, nil
		}
	}
	return 0, errors.New("linux AT_CLKTCK is missing from auxv")
}

func readLinuxProcessPoint(process *benchmarkProcess, clockTicks uint64) (processResourcePoint, error) {
	pid := process.cmd.Process.Pid
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processResourcePoint{}, fmt.Errorf("sample %s PID %d: %w", process.role, pid, err)
	}
	fields, err := parseLinuxProcStat(string(data))
	if err != nil {
		return processResourcePoint{}, fmt.Errorf("parse %s PID %d stat: %w", process.role, pid, err)
	}
	userTicks, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return processResourcePoint{}, fmt.Errorf("parse %s user ticks: %w", process.role, err)
	}
	systemTicks, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return processResourcePoint{}, fmt.Errorf("parse %s system ticks: %w", process.role, err)
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return processResourcePoint{}, fmt.Errorf("parse %s start ticks: %w", process.role, err)
	}
	rssPages, err := strconv.ParseInt(fields[21], 10, 64)
	if err != nil {
		return processResourcePoint{}, fmt.Errorf("parse %s RSS pages %q: %w", process.role, fields[21], err)
	}
	if rssPages < 0 {
		return processResourcePoint{}, fmt.Errorf("parse %s RSS pages %q: negative value", process.role, fields[21])
	}
	return processResourcePoint{
		Role:         process.role,
		PID:          pid,
		Generation:   strconv.FormatUint(startTicks, 10),
		UserCPUNs:    ticksToNanoseconds(userTicks, clockTicks),
		SystemCPUNs:  ticksToNanoseconds(systemTicks, clockTicks),
		ResidentByte: uint64(rssPages) * uint64(os.Getpagesize()),
	}, nil
}

func parseLinuxProcStat(line string) ([]string, error) {
	closing := strings.LastIndex(line, ") ")
	if closing < 0 {
		return nil, errors.New("missing process command terminator")
	}
	fields := strings.Fields(line[closing+2:])
	if len(fields) < 22 {
		return nil, fmt.Errorf("got %d fields after command, want at least 22", len(fields))
	}
	return fields, nil
}

func ticksToNanoseconds(ticks, clockTicks uint64) uint64 {
	return ticks/clockTicks*uint64(time.Second) + ticks%clockTicks*uint64(time.Second)/clockTicks
}

func cpuDelta(start, end processResourcePoint) (user, system uint64, err error) {
	if !sameProcessIdentity(start, end) {
		return 0, 0, fmt.Errorf("process identity changed from %s PID %d generation %q to %s PID %d generation %q",
			start.Role, start.PID, start.Generation, end.Role, end.PID, end.Generation)
	}
	if end.UserCPUNs < start.UserCPUNs || end.SystemCPUNs < start.SystemCPUNs {
		return 0, 0, fmt.Errorf("CPU counters regressed for %s PID %d: user %d -> %d, system %d -> %d",
			start.Role, start.PID, start.UserCPUNs, end.UserCPUNs, start.SystemCPUNs, end.SystemCPUNs)
	}
	return end.UserCPUNs - start.UserCPUNs, end.SystemCPUNs - start.SystemCPUNs, nil
}

func sameProcessIdentity(first, second processResourcePoint) bool {
	return first.Role == second.Role && first.PID == second.PID && first.Generation == second.Generation
}

func validateLiveSample(previous, current []processResourcePoint) error {
	if len(previous) != len(current) {
		return fmt.Errorf("resource sample has %d processes, want %d", len(current), len(previous))
	}
	var terminalClientErr error
	for i := range current {
		if !sameProcessIdentity(previous[i], current[i]) {
			return fmt.Errorf("resource sample index %d changed identity from %s PID %d generation %q to %s PID %d generation %q",
				i, previous[i].Role, previous[i].PID, previous[i].Generation, current[i].Role, current[i].PID, current[i].Generation)
		}
		if _, _, err := cpuDelta(previous[i], current[i]); err != nil {
			if current[i].Role != "iperf-client" {
				return err
			}
			terminalClientErr = err
		}
		if current[i].ResidentByte == 0 {
			err := fmt.Errorf("resource sample for %s PID %d has zero RSS", current[i].Role, current[i].PID)
			if current[i].Role != "iperf-client" {
				return err
			}
			terminalClientErr = err
		}
	}
	if terminalClientErr != nil {
		return fmt.Errorf("%w: %v", errPERF003TerminalClientPoint, terminalClientErr)
	}
	return nil
}

func readDarwinProcessPoints(processes []*benchmarkProcess) ([]processResourcePoint, error) {
	pids := make([]string, len(processes))
	byPID := make(map[int]*benchmarkProcess, len(processes))
	for i, process := range processes {
		pid := process.cmd.Process.Pid
		pids[i] = strconv.Itoa(pid)
		byPID[pid] = process
	}
	output, err := exec.Command("/bin/ps", "-o", "pid=,lstart=,utime=,stime=,rss=", "-p", strings.Join(pids, ",")).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sample Darwin processes: %w: %s", err, output)
	}
	pointsByPID := make(map[int]processResourcePoint, len(processes))
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 9 {
			return nil, fmt.Errorf("parse Darwin ps row %q: got %d fields, want 9", line, len(fields))
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("parse Darwin PID %q: %w", fields[0], err)
		}
		process := byPID[pid]
		if process == nil {
			return nil, fmt.Errorf("darwin ps returned unexpected PID %d", pid)
		}
		userCPU, err := parsePSCPUTime(fields[6])
		if err != nil {
			return nil, fmt.Errorf("parse %s user CPU: %w", process.role, err)
		}
		systemCPU, err := parsePSCPUTime(fields[7])
		if err != nil {
			return nil, fmt.Errorf("parse %s system CPU: %w", process.role, err)
		}
		rssKB, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse %s RSS: %w", process.role, err)
		}
		pointsByPID[pid] = processResourcePoint{
			Role:         process.role,
			PID:          pid,
			Generation:   strings.Join(fields[1:6], " "),
			UserCPUNs:    uint64(userCPU),
			SystemCPUNs:  uint64(systemCPU),
			ResidentByte: rssKB * 1024,
		}
	}
	points := make([]processResourcePoint, len(processes))
	for i, process := range processes {
		point, ok := pointsByPID[process.cmd.Process.Pid]
		if !ok {
			return nil, fmt.Errorf("darwin ps omitted %s PID %d", process.role, process.cmd.Process.Pid)
		}
		points[i] = point
	}
	return points, nil
}

func parsePSCPUTime(value string) (time.Duration, error) {
	daySplit := strings.Split(value, "-")
	if len(daySplit) > 2 {
		return 0, fmt.Errorf("invalid CPU time %q", value)
	}
	var days float64
	clock := daySplit[0]
	if len(daySplit) == 2 {
		parsedDays, err := strconv.ParseFloat(daySplit[0], 64)
		if err != nil {
			return 0, err
		}
		days = parsedDays
		clock = daySplit[1]
	}
	parts := strings.Split(clock, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("invalid CPU time %q", value)
	}
	values := make([]float64, len(parts))
	for i, part := range parts {
		parsed, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, err
		}
		values[i] = parsed
	}
	seconds := days * 24 * 60 * 60
	if len(values) == 3 {
		seconds += values[0] * 60 * 60
		values = values[1:]
	}
	seconds += values[0]*60 + values[1]
	if seconds < 0 {
		return 0, fmt.Errorf("negative CPU time %q", value)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

type rssSummary struct {
	AverageBytes float64
	MaximumBytes uint64
}

func summarizeRSS(samples []resourceSampleSet, duration time.Duration, roles ...string) (rssSummary, error) {
	if len(samples) < 2 || duration <= 0 {
		return rssSummary{}, errors.New("resource measurement requires at least two samples and positive duration")
	}
	selected := make(map[string]bool, len(roles))
	for _, role := range roles {
		selected[role] = true
	}
	var area float64
	var maximum uint64
	for i, sample := range samples {
		var rss uint64
		for _, process := range sample.Processes {
			if len(selected) == 0 || selected[process.Role] {
				rss += process.ResidentByte
			}
		}
		if rss > maximum {
			maximum = rss
		}
		start := time.Duration(sample.OffsetNs)
		if i == 0 {
			start = 0
		}
		end := duration
		if i+1 < len(samples) {
			end = time.Duration(samples[i+1].OffsetNs)
		}
		if end < start || end > duration {
			return rssSummary{}, errors.New("resource sample offsets are not monotonic within duration")
		}
		area += float64(rss) * float64(end-start)
	}
	return rssSummary{AverageBytes: area / float64(duration), MaximumBytes: maximum}, nil
}

type valueDistribution struct {
	Median float64
	MAD    float64
	Min    float64
	Max    float64
}

func distribution(values []float64) valueDistribution {
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	median := ordered[len(ordered)/2]
	deviations := make([]float64, len(ordered))
	for i, value := range ordered {
		if value >= median {
			deviations[i] = value - median
		} else {
			deviations[i] = median - value
		}
	}
	slices.Sort(deviations)
	return valueDistribution{Median: median, MAD: deviations[len(deviations)/2], Min: ordered[0], Max: ordered[len(ordered)-1]}
}

type perf003Environment struct {
	binaryPath string
	certDir    string
	sampler    *liveResourceSampler
	provenance perf003Provenance
}

type perf003Provenance struct {
	Revision      string            `json:"revision"`
	SourceDirty   bool              `json:"source_dirty"`
	CPUModel      string            `json:"cpu_model"`
	GoVersion     string            `json:"go_version"`
	GOOS          string            `json:"goos"`
	GOARCH        string            `json:"goarch"`
	IperfVersion  string            `json:"iperf_version"`
	GOMAXPROCS    int               `json:"gomaxprocs"`
	KernelRelease string            `json:"kernel_release"`
	Dependencies  map[string]string `json:"dependencies"`
}

type perf003RoleUsage struct {
	Role             string  `json:"role"`
	PID              int     `json:"pid"`
	Generation       string  `json:"generation"`
	UserCPUSeconds   float64 `json:"user_cpu_seconds"`
	SystemCPUSeconds float64 `json:"system_cpu_seconds"`
	CPUSeconds       float64 `json:"cpu_seconds"`
	AverageRSSBytes  float64 `json:"average_rss_bytes"`
	MaximumRSSBytes  uint64  `json:"maximum_rss_bytes"`
	CPUSecondsPerGiB float64 `json:"cpu_seconds_per_received_gib"`
}

type perf003Aggregate struct {
	Roles            []string `json:"roles"`
	CPUSeconds       float64  `json:"cpu_seconds"`
	AverageRSSBytes  float64  `json:"average_rss_bytes"`
	MaximumRSSBytes  uint64   `json:"maximum_rss_bytes"`
	CPUSecondsPerGiB float64  `json:"cpu_seconds_per_received_gib"`
}

type perf003RawResult struct {
	SchemaVersion      string              `json:"schema_version"`
	Protocol           string              `json:"protocol"`
	Threads            int                 `json:"threads"`
	Path               string              `json:"path"`
	Repeat             int                 `json:"repeat"`
	Sampler            string              `json:"sampler"`
	SamplePeriodNs     int64               `json:"sample_period_ns"`
	WindowDurationNs   int64               `json:"window_duration_ns"`
	SentBytes          int64               `json:"sent_bytes"`
	ReceivedBytes      int64               `json:"received_bytes"`
	ReceivedMbps       float64             `json:"received_mbps"`
	UDPJitterMs        *float64            `json:"udp_jitter_ms,omitempty"`
	UDPLostPackets     *int                `json:"udp_lost_packets,omitempty"`
	UDPPackets         *int                `json:"udp_packets,omitempty"`
	Processes          []perf003RoleUsage  `json:"processes"`
	QMuxCombined       *perf003Aggregate   `json:"qmux_combined,omitempty"`
	GeneratorsCombined perf003Aggregate    `json:"generators_combined"`
	CaseTotal          perf003Aggregate    `json:"case_total"`
	Normalization      map[string]float64  `json:"normalization"`
	Samples            []resourceSampleSet `json:"samples"`
	// Both offsets use the transfer start as origin and enclose the transfer window.
	PersistentCPUStartBound resourceSampleSet `json:"persistent_cpu_start_bound"`
	PersistentCPUEndBound   resourceSampleSet `json:"persistent_cpu_end_bound"`
	Provenance              perf003Provenance `json:"provenance"`
	OptimizedQUIC           config.Quic       `json:"optimized_quic"`
}

type perf003CaseProcesses struct {
	all        []*benchmarkProcess
	persistent []*benchmarkProcess
	targetPort int
	readyURL   string
	qmuxClient *benchmarkProcess
}

func runPERF003Matrix(t *testing.T) {
	t.Helper()
	sampler, err := newLiveResourceSampler()
	if err != nil {
		t.Skip(err)
	}
	binaryPath := filepath.Join(t.TempDir(), "qmux-perf003")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build QMux benchmark binary: %v\n%s", err, output)
	}
	env := &perf003Environment{
		binaryPath: binaryPath,
		certDir:    generateTestCertificates(t),
		sampler:    sampler,
		provenance: perf003SourceProvenance(t),
	}
	var results []perf003RawResult
	cases := []struct {
		protocol string
		threads  int
	}{{"tcp", 1}, {"tcp", 2}, {"tcp", 4}, {"udp", 1}, {"udp", 2}}
	for repeat := range 5 {
		paths := []string{"direct", "qmux"}
		if repeat%2 == 1 {
			paths[0], paths[1] = paths[1], paths[0]
		}
		for _, testCase := range cases {
			for _, path := range paths {
				name := fmt.Sprintf("%s/%d-thread/%s/repeat-%d", testCase.protocol, testCase.threads, path, repeat+1)
				t.Run(name, func(t *testing.T) {
					result := runPERF003Case(t, env, testCase.protocol, testCase.threads, path, repeat+1)
					encoded, err := json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("PERF003_RAW %s", encoded)
					results = append(results, result)
				})
			}
		}
	}
	printPERF003Summary(t, results)
}

func perf003SourceProvenance(t *testing.T) perf003Provenance {
	t.Helper()
	versionOutput, err := exec.Command("iperf3", "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("read iperf3 version: %v: %s", err, versionOutput)
	}
	provenance := perf003Provenance{
		CPUModel: perf003CPUModel(), GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		GOMAXPROCS: runtime.GOMAXPROCS(0), Dependencies: make(map[string]string),
	}
	kernelOutput, err := exec.Command("uname", "-r").Output()
	if err != nil {
		t.Fatalf("read kernel release: %v", err)
	}
	provenance.KernelRelease = strings.TrimSpace(string(kernelOutput))
	if lines := strings.Split(strings.TrimSpace(string(versionOutput)), "\n"); len(lines) > 0 {
		provenance.IperfVersion = lines[0]
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range info.Deps {
			provenance.Dependencies[dependency.Path] = dependency.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				provenance.Revision = setting.Value
			case "vcs.modified":
				provenance.SourceDirty = setting.Value == "true"
			}
		}
	}
	if provenance.Revision == "" {
		command := exec.Command("git", "rev-parse", "HEAD")
		command.Dir = ".."
		if output, err := command.Output(); err == nil {
			provenance.Revision = strings.TrimSpace(string(output))
		}
	}
	status := exec.Command("git", "status", "--porcelain")
	status.Dir = ".."
	if output, err := status.Output(); err == nil {
		provenance.SourceDirty = len(bytes.TrimSpace(output)) > 0
	}
	return provenance
}

func perf003CPUModel() string {
	if runtime.GOOS == "darwin" {
		if output, err := exec.Command("/usr/sbin/sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			return strings.TrimSpace(string(output))
		}
	}
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for line := range strings.SplitSeq(string(data), "\n") {
				if key, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "model name" {
					return strings.TrimSpace(value)
				}
			}
		}
	}
	return "unknown"
}

func runPERF003Case(t *testing.T, env *perf003Environment, protocol string, threads int, path string, repeat int) perf003RawResult {
	t.Helper()
	processes := startPERF003CaseProcesses(t, env, protocol, threads, path)
	defer func() {
		for _, process := range slices.Backward(processes.all) {
			process.cleanup(time.Second)
		}
	}()

	persistentStartBefore := time.Now()
	persistentStart, err := env.sampler.sample(processes.persistent)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	clientCommand := exec.Command("iperf3", iperf3ClientArgs(processes.targetPort, 5, threads, protocol, true)...)
	clientCommand.Stdout = &output
	windowStart := time.Now()
	client, err := startBenchmarkProcess("iperf-client", clientCommand)
	if err != nil {
		t.Fatal(err)
	}
	processes.all = append(processes.all, client)
	measured := append(slices.Clone(processes.persistent), client)

	firstLive, err := env.sampler.sample(measured)
	if err != nil {
		t.Fatal(err)
	}
	firstPrevious := append(slices.Clone(persistentStart), processResourcePoint{
		Role: client.role, PID: client.cmd.Process.Pid, Generation: client.generation,
	})
	if err := validateLiveSample(firstPrevious, firstLive); err != nil {
		t.Fatal(err)
	}
	if err := client.expectAlive(); err != nil {
		t.Fatal(err)
	}
	samples := []resourceSampleSet{{OffsetNs: time.Since(windowStart).Nanoseconds(), Processes: firstLive}}
	ticker := time.NewTicker(resourceSamplePeriod)
	defer ticker.Stop()
sampleLoop:
	for {
		select {
		case <-client.done:
			break sampleLoop
		case <-ticker.C:
			points, err := env.sampler.sample(measured)
			sampleReturnedAt := time.Now()
			if err != nil {
				if !client.waitDone(resourceSamplePeriod) {
					t.Fatal(err)
				}
				persistentPoints, persistentErr := env.sampler.sample(processes.persistent)
				if persistentErr == nil {
					previous := samples[len(samples)-1].Processes[:len(processes.persistent)]
					persistentErr = validateLiveSample(previous, persistentPoints)
				}
				if persistentErr != nil {
					t.Fatalf("terminal iperf client sample failed (%v), and persistent resample failed: %v", err, persistentErr)
				}
				break sampleLoop
			}
			if err := validateLiveSample(samples[len(samples)-1].Processes, points); err != nil {
				if errors.Is(err, errPERF003TerminalClientPoint) && client.waitDone(resourceSamplePeriod) {
					break sampleLoop
				}
				t.Fatal(err)
			}
			samples = append(samples, resourceSampleSet{OffsetNs: sampleReturnedAt.Sub(windowStart).Nanoseconds(), Processes: points})
			select {
			case <-client.done:
				endedOffset := client.endedAt.Sub(windowStart).Nanoseconds()
				if samples[len(samples)-1].OffsetNs > endedOffset {
					samples[len(samples)-1].OffsetNs = endedOffset
				}
				break sampleLoop
			default:
			}
		}
	}
	if err := client.wait(15 * time.Second); err != nil {
		t.Fatalf("iperf client failed: %v\n%s", err, client.log.String())
	}
	windowEnd := client.endedAt
	windowDuration := windowEnd.Sub(windowStart)
	for i := range samples {
		if samples[i].OffsetNs > windowDuration.Nanoseconds() {
			if i != len(samples)-1 {
				t.Fatalf("resource sample %d offset %dns exceeds transfer duration %dns", i, samples[i].OffsetNs, windowDuration.Nanoseconds())
			}
			samples[i].OffsetNs = windowDuration.Nanoseconds()
		}
	}
	persistentFinal, err := env.sampler.sample(processes.persistent)
	if err != nil {
		t.Fatal(err)
	}
	persistentEndAfter := time.Now()
	previousPersistent := samples[len(samples)-1].Processes[:len(processes.persistent)]
	if err := validateLiveSample(previousPersistent, persistentFinal); err != nil {
		t.Fatalf("invalid final persistent resource sample %+v: %v", persistentFinal, err)
	}
	lastLive := slices.Clone(samples[len(samples)-1].Processes)
	samples = append(samples, resourceSampleSet{OffsetNs: windowDuration.Nanoseconds(), Processes: lastLive})
	initial := make([]processResourcePoint, 0, len(measured))
	initial = append(initial, persistentStart...)
	clientInitial := firstLive[len(firstLive)-1]
	clientInitial.UserCPUNs, clientInitial.SystemCPUNs = 0, 0
	initial = append(initial, clientInitial)
	final := make([]processResourcePoint, 0, len(measured))
	final = append(final, persistentFinal...)
	clientFinal := clientInitial
	clientFinal.UserCPUNs = uint64(client.cmd.ProcessState.UserTime())
	clientFinal.SystemCPUNs = uint64(client.cmd.ProcessState.SystemTime())
	final = append(final, clientFinal)
	for _, process := range processes.persistent {
		if err := process.expectAlive(); err != nil {
			t.Fatal(err)
		}
	}
	if path == "qmux" {
		checkPERF003Ready(t, processes)
	}
	result := parseIperf3Output(t, output.Bytes())
	if err := validatePERF003Iperf(protocol, result); err != nil {
		t.Fatal(err)
	}

	cpuStart := resourceSampleSet{OffsetNs: persistentStartBefore.Sub(windowStart).Nanoseconds(), Processes: persistentStart}
	cpuEnd := resourceSampleSet{OffsetNs: persistentEndAfter.Sub(windowStart).Nanoseconds(), Processes: persistentFinal}
	if cpuStart.OffsetNs > 0 || cpuEnd.OffsetNs < windowDuration.Nanoseconds() {
		t.Fatalf("persistent CPU bounds [%dns, %dns] do not enclose transfer window [0ns, %dns]", cpuStart.OffsetNs, cpuEnd.OffsetNs, windowDuration.Nanoseconds())
	}
	raw := buildPERF003Result(t, env, protocol, threads, path, repeat, windowDuration, result, samples, cpuStart, cpuEnd, initial, final)
	for _, process := range slices.Backward(processes.persistent) {
		if err := process.expectAlive(); err != nil {
			t.Fatal(err)
		}
		if err := process.stop(3 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if path == "qmux" {
		checkPERF003ClientLog(t, processes.qmuxClient)
	}
	return raw
}

func startPERF003CaseProcesses(t *testing.T, env *perf003Environment, protocol string, threads int, path string) *perf003CaseProcesses {
	t.Helper()
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		processes, err := startPERF003CaseAttempt(t, env, protocol, threads, path, attempt)
		if err == nil {
			return processes
		}
		lastErr = err
		if processes != nil {
			for _, process := range slices.Backward(processes.all) {
				process.cleanup(time.Second)
			}
		}
		if (!isPERF003BindCollision(err, processes) && !errors.Is(err, errPERF003SetupPolluted)) || attempt == 3 {
			break
		}
		t.Logf("pre-transfer setup was not cold; retrying setup (%d/3): %v", attempt, err)
	}
	t.Fatalf("start %s %s %d-thread case: %v", path, protocol, threads, lastErr)
	return nil
}

func startPERF003CaseAttempt(t *testing.T, env *perf003Environment, protocol string, threads int, path string, attempt int) (*perf003CaseProcesses, error) {
	t.Helper()
	result := &perf003CaseProcesses{}
	serverPort := getFreePort(t)
	server, err := startBenchmarkProcess("iperf-server", exec.Command("iperf3", "-s", "-p", strconv.Itoa(serverPort)))
	if err != nil {
		return result, err
	}
	result.all = append(result.all, server)
	result.persistent = append(result.persistent, server)
	if err := pollPERF003TCP(server, serverPort, 3*time.Second); err != nil {
		return result, err
	}
	if path == "direct" {
		result.targetPort = serverPort
		return result, nil
	}

	quicPort, trafficPort, adminPort := getFreePort(t), getFreePort(t), getFreePort(t)
	serverPath, clientPath, err := writePERF003Configs(t, env.certDir, protocol, threads, attempt, serverPort, quicPort, trafficPort)
	if err != nil {
		return result, err
	}
	serverCommand := exec.Command(env.binaryPath, "run", "server", "-c", serverPath, "--admin-address", fmt.Sprintf("127.0.0.1:%d", adminPort))
	qmuxServer, err := startBenchmarkProcess("qmux-server", serverCommand)
	if err != nil {
		return result, err
	}
	result.all = append(result.all, qmuxServer)
	result.persistent = append(result.persistent, qmuxServer)
	result.targetPort = trafficPort
	result.readyURL = fmt.Sprintf("http://127.0.0.1:%d/readyz", adminPort)
	if err := pollPERF003Endpoint(result, strings.Replace(result.readyURL, "/readyz", "/healthyz", 1), 5*time.Second); err != nil {
		return result, err
	}
	if err := pollPERF003Log(qmuxServer, "QUIC listener started", 5*time.Second); err != nil {
		return result, err
	}
	clientCommand := exec.Command(env.binaryPath, "run", "client", "-c", clientPath)
	qmuxClient, err := startBenchmarkProcess("qmux-client", clientCommand)
	if err != nil {
		return result, err
	}
	result.all = append(result.all, qmuxClient)
	result.persistent = append(result.persistent, qmuxClient)
	result.qmuxClient = qmuxClient
	if err := pollPERF003Ready(result, perf003ReadyTimeout); err != nil {
		return result, err
	}
	for _, process := range result.persistent {
		if err := process.expectAlive(); err != nil {
			return result, err
		}
	}
	if err := perf003ClientLogError(qmuxClient); err != nil {
		return result, err
	}
	return result, nil
}

func pollPERF003Log(process *benchmarkProcess, marker string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := process.expectAlive(); err != nil {
			return err
		}
		if strings.Contains(process.log.String(), marker) {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("%s did not log %q within %s", process.role, marker, timeout)
}

func pollPERF003TCP(process *benchmarkProcess, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := process.expectAlive(); err != nil {
			return err
		}
		connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("TCP port %d did not become ready within %s", port, timeout)
}

func writePERF003Configs(t *testing.T, certDir, protocol string, threads, attempt, localPort, quicPort, trafficPort int) (string, string, error) {
	t.Helper()
	qmuxProtocol := protocol
	if protocol == "udp" {
		qmuxProtocol = "both"
	}
	quic := getOptimizedQuicConfig()
	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{QuicAddr: fmt.Sprintf("127.0.0.1:%d", quicPort), TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort), Protocol: qmuxProtocol, Quic: quic}},
		Auth:      config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS:       config.ServerTLS{ServerCertFile: filepath.Join(certDir, "server.crt"), ServerKeyFile: filepath.Join(certDir, "server.key")},
	}
	clientConfig := &config.Client{
		ClientID: fmt.Sprintf("perf003-%s-%d-%d", protocol, threads, attempt),
		Server:   config.ClientServer{Servers: []config.ServerEndpoint{{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"}}},
		Local:    config.LocalService{Host: "127.0.0.1", Port: localPort},
		TLS:      config.ClientTLS{CACertFile: filepath.Join(certDir, "ca.crt"), ClientCertFile: filepath.Join(certDir, "client.crt"), ClientKeyFile: filepath.Join(certDir, "client.key")},
		Quic:     quic,
	}
	directory := t.TempDir()
	serverPath, clientPath := filepath.Join(directory, "server.yaml"), filepath.Join(directory, "client.yaml")
	for path, value := range map[string]any{serverPath: serverConfig, clientPath: clientConfig} {
		data, err := yaml.Marshal(value)
		if err != nil {
			return "", "", err
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			return "", "", err
		}
	}
	loadedServer, err := config.LoadServerConfig(serverPath)
	if err != nil {
		return "", "", err
	}
	loadedClient, err := config.LoadClientConfig(clientPath)
	if err != nil {
		return "", "", err
	}
	if len(loadedServer.Listeners) != 1 || loadedServer.Listeners[0].Quic != quic || loadedClient.Quic != quic ||
		loadedServer.Listeners[0].QuicAddr != serverConfig.Listeners[0].QuicAddr ||
		loadedServer.Listeners[0].TrafficAddr != serverConfig.Listeners[0].TrafficAddr ||
		loadedServer.Listeners[0].Protocol != serverConfig.Listeners[0].Protocol || loadedClient.Local != clientConfig.Local {
		return "", "", errors.New("typed YAML round trip changed PERF-003 addresses or optimized QUIC configuration")
	}
	return serverPath, clientPath, nil
}

func pollPERF003Ready(processes *perf003CaseProcesses, timeout time.Duration) error {
	return pollPERF003Endpoint(processes, processes.readyURL, timeout)
}

func pollPERF003Endpoint(processes *perf003CaseProcesses, url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, process := range processes.persistent {
			if err := process.expectAlive(); err != nil {
				return err
			}
		}
		if processes.qmuxClient != nil {
			if err := perf003ClientLogError(processes.qmuxClient); err != nil {
				return err
			}
		}
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s did not become ready within %s", url, timeout)
}

func checkPERF003Ready(t *testing.T, processes *perf003CaseProcesses) {
	t.Helper()
	client := &http.Client{Timeout: 250 * time.Millisecond}
	response, err := client.Get(processes.readyURL)
	if err != nil {
		t.Fatalf("post-transfer readiness: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("post-transfer readiness status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

func perf003ClientLogError(process *benchmarkProcess) error {
	logText := strings.ToLower(process.log.String())
	for _, marker := range []string{"failed to create client", "client stopped with error, reconnecting"} {
		if strings.Contains(logText, marker) {
			return fmt.Errorf("%w: qmux client logged %q:\n%s", errPERF003SetupPolluted, marker, process.log.String())
		}
	}
	return nil
}

func checkPERF003ClientLog(t *testing.T, process *benchmarkProcess) {
	t.Helper()
	if err := perf003ClientLogError(process); err != nil {
		t.Fatal(err)
	}
}

func isPERF003BindCollision(err error, processes *perf003CaseProcesses) bool {
	text := strings.ToLower(err.Error())
	if processes != nil {
		for _, process := range processes.all {
			text += "\n" + strings.ToLower(process.log.String())
		}
	}
	return strings.Contains(text, "address already in use") || strings.Contains(text, "bind failed")
}

func buildPERF003Result(t *testing.T, env *perf003Environment, protocol string, threads int, path string, repeat int, duration time.Duration, iperf *iperf3Result, samples []resourceSampleSet, cpuStart, cpuEnd resourceSampleSet, initial, final []processResourcePoint) perf003RawResult {
	t.Helper()
	receivedBytes := iperfReceivedBytes(iperf)
	receivedGiB := float64(receivedBytes) / (1 << 30)
	roles := make([]perf003RoleUsage, len(initial))
	for i := range initial {
		user, system, err := cpuDelta(initial[i], final[i])
		if err != nil {
			t.Fatal(err)
		}
		rss, err := summarizeRSS(samples, duration, initial[i].Role)
		if err != nil {
			t.Fatal(err)
		}
		cpuSeconds := float64(user+system) / float64(time.Second)
		roles[i] = perf003RoleUsage{
			Role: initial[i].Role, PID: initial[i].PID, Generation: initial[i].Generation,
			UserCPUSeconds: float64(user) / float64(time.Second), SystemCPUSeconds: float64(system) / float64(time.Second), CPUSeconds: cpuSeconds,
			AverageRSSBytes: rss.AverageBytes, MaximumRSSBytes: rss.MaximumBytes, CPUSecondsPerGiB: cpuSeconds / receivedGiB,
		}
	}
	generators := aggregatePERF003(t, roles, samples, duration, receivedGiB, "iperf-server", "iperf-client")
	caseTotal := aggregatePERF003(t, roles, samples, duration, receivedGiB)
	raw := perf003RawResult{
		SchemaVersion: "perf-003/v1", Protocol: protocol, Threads: threads, Path: path, Repeat: repeat,
		Sampler: env.sampler.kind, SamplePeriodNs: resourceSamplePeriod.Nanoseconds(), WindowDurationNs: duration.Nanoseconds(),
		SentBytes: iperf.End.SumSent.Bytes, ReceivedBytes: receivedBytes, ReceivedMbps: iperf.End.SumReceived.BitsPerSecond / 1e6,
		Processes: roles, Samples: samples, PersistentCPUStartBound: cpuStart, PersistentCPUEndBound: cpuEnd, Provenance: env.provenance, OptimizedQUIC: getOptimizedQuicConfig(),
		Normalization: map[string]float64{"received_bytes": float64(receivedBytes), "received_gib": receivedGiB}, GeneratorsCombined: generators, CaseTotal: caseTotal,
	}
	if path == "qmux" {
		qmux := aggregatePERF003(t, roles, samples, duration, receivedGiB, "qmux-server", "qmux-client")
		raw.QMuxCombined = &qmux
	}
	if protocol == "udp" {
		raw.UDPJitterMs = &iperf.End.Sum.JitterMs
		raw.UDPLostPackets = &iperf.End.Sum.LostPackets
		raw.UDPPackets = &iperf.End.Sum.Packets
	}
	return raw
}

func iperfReceivedBytes(result *iperf3Result) int64 {
	return result.End.SumReceived.Bytes
}

func validatePERF003Iperf(protocol string, result *iperf3Result) error {
	sent, received := result.End.SumSent.Bytes, iperfReceivedBytes(result)
	if sent <= 0 || received <= 0 || received > sent {
		return fmt.Errorf("invalid iperf3 delivery: sent=%d received=%d", sent, received)
	}
	if protocol == "udp" {
		packets, lost := result.End.Sum.Packets, result.End.Sum.LostPackets
		if packets <= 0 || lost < 0 || lost >= packets {
			return fmt.Errorf("invalid iperf3 UDP delivery: packets=%d lost=%d", packets, lost)
		}
	}
	return nil
}

func aggregatePERF003(t *testing.T, usages []perf003RoleUsage, samples []resourceSampleSet, duration time.Duration, receivedGiB float64, roles ...string) perf003Aggregate {
	t.Helper()
	selected := make(map[string]bool, len(roles))
	for _, role := range roles {
		selected[role] = true
	}
	result := perf003Aggregate{Roles: slices.Clone(roles)}
	if len(roles) == 0 {
		result.Roles = make([]string, len(usages))
	}
	for i, usage := range usages {
		if len(selected) == 0 || selected[usage.Role] {
			result.CPUSeconds += usage.CPUSeconds
			if len(roles) == 0 {
				result.Roles[i] = usage.Role
			}
		}
	}
	rss, err := summarizeRSS(samples, duration, roles...)
	if err != nil {
		t.Fatal(err)
	}
	result.AverageRSSBytes = rss.AverageBytes
	result.MaximumRSSBytes = rss.MaximumBytes
	result.CPUSecondsPerGiB = result.CPUSeconds / receivedGiB
	return result
}

func printPERF003Summary(t *testing.T, results []perf003RawResult) {
	t.Helper()
	type metrics struct{ throughput, caseCPU, qmuxCPU, averageRSS, maximumRSS []float64 }
	type roleMetrics struct{ cpu, averageRSS, maximumRSS []float64 }
	groups := make(map[string]*metrics)
	roleGroups := make(map[string]*roleMetrics)
	for _, result := range results {
		key := fmt.Sprintf("%s/%d-thread/%s", result.Protocol, result.Threads, result.Path)
		group := groups[key]
		if group == nil {
			group = &metrics{}
			groups[key] = group
		}
		group.throughput = append(group.throughput, result.ReceivedMbps)
		group.caseCPU = append(group.caseCPU, result.CaseTotal.CPUSecondsPerGiB)
		group.averageRSS = append(group.averageRSS, result.CaseTotal.AverageRSSBytes)
		group.maximumRSS = append(group.maximumRSS, float64(result.CaseTotal.MaximumRSSBytes))
		if result.QMuxCombined != nil {
			group.qmuxCPU = append(group.qmuxCPU, result.QMuxCombined.CPUSecondsPerGiB)
		}
		for _, usage := range result.Processes {
			roleKey := key + "/" + usage.Role
			roleGroup := roleGroups[roleKey]
			if roleGroup == nil {
				roleGroup = &roleMetrics{}
				roleGroups[roleKey] = roleGroup
			}
			roleGroup.cpu = append(roleGroup.cpu, usage.CPUSecondsPerGiB)
			roleGroup.averageRSS = append(roleGroup.averageRSS, usage.AverageRSSBytes)
			roleGroup.maximumRSS = append(roleGroup.maximumRSS, float64(usage.MaximumRSSBytes))
		}
	}
	if len(groups) != 10 {
		t.Fatalf("PERF-003 summary has %d groups, want 10", len(groups))
	}
	for key, group := range groups {
		if len(group.throughput) != 5 {
			t.Fatalf("PERF-003 summary group %s has %d repeats, want 5", key, len(group.throughput))
		}
	}
	for key, group := range roleGroups {
		if len(group.cpu) != 5 {
			t.Fatalf("PERF-003 role summary group %s has %d repeats, want 5", key, len(group.cpu))
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		group := groups[key]
		t.Logf("PERF003_SUMMARY %s throughput_mbps=%+v case_cpu_seconds_per_gib=%+v avg_rss_bytes=%+v max_rss_bytes=%+v",
			key, distribution(group.throughput), distribution(group.caseCPU), distribution(group.averageRSS), distribution(group.maximumRSS))
		if len(group.qmuxCPU) > 0 {
			t.Logf("PERF003_SUMMARY %s qmux_cpu_seconds_per_gib=%+v", key, distribution(group.qmuxCPU))
		}
	}
	roleKeys := make([]string, 0, len(roleGroups))
	for key := range roleGroups {
		roleKeys = append(roleKeys, key)
	}
	slices.Sort(roleKeys)
	for _, key := range roleKeys {
		group := roleGroups[key]
		t.Logf("PERF003_ROLE_SUMMARY %s cpu_seconds_per_gib=%+v avg_rss_bytes=%+v max_rss_bytes=%+v",
			key, distribution(group.cpu), distribution(group.averageRSS), distribution(group.maximumRSS))
	}
}

func TestProcessResourceSamplerMath(t *testing.T) {
	fields, err := parseLinuxProcStat("42 (name with ) spaces) R 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22")
	if err != nil || fields[11] != "11" || fields[19] != "19" || fields[21] != "21" {
		t.Fatalf("parse Linux stat = %v, %v", fields, err)
	}
	for value, want := range map[string]time.Duration{"8:03.27": 8*time.Minute + 3270*time.Millisecond, "1:02:03.50": time.Hour + 2*time.Minute + 3500*time.Millisecond, "2-01:00:00": 49 * time.Hour} {
		got, err := parsePSCPUTime(value)
		if err != nil || got != want {
			t.Fatalf("parsePSCPUTime(%q) = %s, %v; want %s", value, got, err, want)
		}
	}
	samples := []resourceSampleSet{
		{Processes: []processResourcePoint{{Role: "a", ResidentByte: 10}, {Role: "b", ResidentByte: 20}}},
		{OffsetNs: int64(time.Second), Processes: []processResourcePoint{{Role: "a", ResidentByte: 30}, {Role: "b", ResidentByte: 10}}},
	}
	all, err := summarizeRSS(samples, 2*time.Second)
	if err != nil || all.AverageBytes != 35 || all.MaximumBytes != 40 {
		t.Fatalf("all RSS = %+v, %v", all, err)
	}
	aOnly, err := summarizeRSS(samples, 2*time.Second, "a")
	if err != nil || aOnly.AverageBytes != 20 || aOnly.MaximumBytes != 30 {
		t.Fatalf("a RSS = %+v, %v", aOnly, err)
	}
	d := distribution([]float64{9, 1, 5, 7, 3})
	if d != (valueDistribution{Median: 5, MAD: 2, Min: 1, Max: 9}) {
		t.Fatalf("distribution = %+v", d)
	}
	base := processResourcePoint{Role: "worker", PID: 42, Generation: "g1", UserCPUNs: 2, SystemCPUNs: 3, ResidentByte: 10}
	clientBase := processResourcePoint{Role: "iperf-client", PID: 43, Generation: "g2", UserCPUNs: 5, SystemCPUNs: 6, ResidentByte: 10}
	for _, testCase := range []struct {
		name         string
		previous     []processResourcePoint
		current      []processResourcePoint
		wantErr      bool
		wantTerminal bool
		wantCPUErr   bool
	}{
		{name: "valid", previous: []processResourcePoint{base}, current: []processResourcePoint{{Role: "worker", PID: 42, Generation: "g1", UserCPUNs: 3, SystemCPUNs: 5, ResidentByte: 20}}},
		{name: "length", previous: []processResourcePoint{base}, wantErr: true},
		{name: "role mismatch", previous: []processResourcePoint{base}, current: []processResourcePoint{{Role: "other", PID: 42, Generation: "g1", ResidentByte: 10}}, wantErr: true, wantCPUErr: true},
		{name: "PID mismatch", previous: []processResourcePoint{base}, current: []processResourcePoint{{Role: "worker", PID: 99, Generation: "g1", ResidentByte: 10}}, wantErr: true, wantCPUErr: true},
		{name: "generation mismatch", previous: []processResourcePoint{base}, current: []processResourcePoint{{Role: "worker", PID: 42, Generation: "g9", ResidentByte: 10}}, wantErr: true, wantCPUErr: true},
		{name: "user regression", previous: []processResourcePoint{base}, current: []processResourcePoint{{Role: "worker", PID: 42, Generation: "g1", UserCPUNs: 1, SystemCPUNs: 3, ResidentByte: 10}}, wantErr: true, wantCPUErr: true},
		{name: "system regression", previous: []processResourcePoint{base}, current: []processResourcePoint{{Role: "worker", PID: 42, Generation: "g1", UserCPUNs: 2, SystemCPUNs: 2, ResidentByte: 10}}, wantErr: true, wantCPUErr: true},
		{name: "persistent zero RSS", previous: []processResourcePoint{base}, current: []processResourcePoint{{Role: "worker", PID: 42, Generation: "g1", UserCPUNs: 2, SystemCPUNs: 3}}, wantErr: true},
		{name: "terminal client CPU", previous: []processResourcePoint{clientBase}, current: []processResourcePoint{{Role: "iperf-client", PID: 43, Generation: "g2", UserCPUNs: 0, SystemCPUNs: 0, ResidentByte: 10}}, wantErr: true, wantTerminal: true, wantCPUErr: true},
		{name: "terminal client RSS", previous: []processResourcePoint{clientBase}, current: []processResourcePoint{{Role: "iperf-client", PID: 43, Generation: "g2", UserCPUNs: 5, SystemCPUNs: 6}}, wantErr: true, wantTerminal: true},
		{name: "persistent fault before terminal client", previous: []processResourcePoint{base, clientBase}, current: []processResourcePoint{{Role: "worker", PID: 42, Generation: "g1", UserCPUNs: 2, SystemCPUNs: 3}, {Role: "iperf-client", PID: 43, Generation: "g2"}}, wantErr: true},
	} {
		t.Run("live sample/"+testCase.name, func(t *testing.T) {
			err := validateLiveSample(testCase.previous, testCase.current)
			if (err != nil) != testCase.wantErr || errors.Is(err, errPERF003TerminalClientPoint) != testCase.wantTerminal {
				t.Fatalf("validateLiveSample error = %v, wantErr %t, wantTerminal %t", err, testCase.wantErr, testCase.wantTerminal)
			}
			if len(testCase.previous) == 1 && len(testCase.current) == 1 {
				_, _, cpuErr := cpuDelta(testCase.previous[0], testCase.current[0])
				if (cpuErr != nil) != testCase.wantCPUErr {
					t.Fatalf("cpuDelta error = %v, wantErr %t", cpuErr, testCase.wantCPUErr)
				}
			}
		})
	}

	wordSize := strconv.IntSize / 8
	auxv := make([]byte, wordSize*4)
	putNativeWord(auxv, 0, linuxATClockTicks)
	putNativeWord(auxv, wordSize, 250)
	auxvPath := filepath.Join(t.TempDir(), "auxv")
	if err := os.WriteFile(auxvPath, auxv, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readLinuxClockTicks(auxvPath); err != nil || got != 250 {
		t.Fatalf("clock ticks = %d, %v; want 250", got, err)
	}
	putNativeWord(auxv, 0, 0)
	if err := os.WriteFile(auxvPath, auxv, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLinuxClockTicks(auxvPath); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing AT_CLKTCK error = %v", err)
	}

	for name, fixture := range map[string]string{
		"tcp":         `{"end":{"sum_sent":{"bytes":101},"sum_received":{"bytes":101}}}`,
		"udp":         `{"end":{"sum_sent":{"bytes":303},"sum_received":{"bytes":202},"sum":{"lost_packets":1,"packets":7}}}`,
		"multistream": `{"end":{"sum_sent":{"bytes":400},"sum_received":{"bytes":303},"streams":[{"receiver":{"bytes":1}},{"receiver":{"bytes":2}}]}}`,
	} {
		var result iperf3Result
		if err := json.Unmarshal([]byte(fixture), &result); err != nil {
			t.Fatalf("%s fixture: %v", name, err)
		}
		want := map[string]int64{"tcp": 101, "udp": 202, "multistream": 303}[name]
		if got := iperfReceivedBytes(&result); got != want {
			t.Fatalf("%s received bytes = %d, want %d", name, got, want)
		}
		protocol := "tcp"
		if name == "udp" {
			protocol = "udp"
		}
		if err := validatePERF003Iperf(protocol, &result); err != nil {
			t.Fatalf("%s validation: %v", name, err)
		}
	}
	for name, fixture := range map[string]string{
		"received exceeds sent": `{"end":{"sum_sent":{"bytes":10},"sum_received":{"bytes":11}}}`,
		"negative UDP loss":     `{"end":{"sum_sent":{"bytes":10},"sum_received":{"bytes":5},"sum":{"lost_packets":-1,"packets":2}}}`,
		"all UDP packets lost":  `{"end":{"sum_sent":{"bytes":10},"sum_received":{"bytes":5},"sum":{"lost_packets":2,"packets":2}}}`,
	} {
		var result iperf3Result
		if err := json.Unmarshal([]byte(fixture), &result); err != nil {
			t.Fatalf("%s fixture: %v", name, err)
		}
		protocol := "tcp"
		if strings.Contains(name, "UDP") {
			protocol = "udp"
		}
		if err := validatePERF003Iperf(protocol, &result); err == nil {
			t.Fatalf("%s fixture passed validation", name)
		}
	}
	polluted := &benchmarkProcess{}
	_, _ = polluted.log.Write([]byte("client stopped with error, reconnecting"))
	if err := perf003ClientLogError(polluted); !errors.Is(err, errPERF003SetupPolluted) {
		t.Fatalf("client failure error = %v, want setup pollution sentinel", err)
	}
}

func putNativeWord(data []byte, offset int, value uint64) {
	if strconv.IntSize == 64 {
		binary.NativeEndian.PutUint64(data[offset:], value)
	} else {
		binary.NativeEndian.PutUint32(data[offset:], uint32(value))
	}
}

func TestProcessResourceSamplerCalibration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping process resource sampler calibration in short mode")
	}
	sampler, err := newLiveResourceSampler()
	if err != nil {
		t.Skip(err)
	}
	idle, idleInput, idleOutput := startResourceHelper(t, "calibration-idle", 0, false)
	loaded, loadedInput, loadedOutput := startResourceHelper(t, "calibration-loaded", 64<<20, true)
	defer idle.cleanup(time.Second)
	defer loaded.cleanup(time.Second)
	waitResourceHelperLine(t, idleOutput, "ready")
	waitResourceHelperLine(t, loadedOutput, "ready")

	start, err := sampler.sample([]*benchmarkProcess{idle, loaded})
	if err != nil {
		t.Fatal(err)
	}
	if got := int64(start[1].ResidentByte) - int64(start[0].ResidentByte); got < 48<<20 || got > 80<<20 {
		t.Fatalf("fixed RSS delta = %.1f MiB, want 64 MiB within +/-16 MiB", float64(got)/(1<<20))
	}
	measurementStart := time.Now()
	samples := []resourceSampleSet{{Processes: start}}
	if _, err := idleInput.Write([]byte{'b'}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadedInput.Write([]byte{'b'}); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		time.Sleep(resourceSamplePeriod)
		points, err := sampler.sample([]*benchmarkProcess{idle, loaded})
		if err != nil {
			t.Fatal(err)
		}
		samples = append(samples, resourceSampleSet{OffsetNs: time.Since(measurementStart).Nanoseconds(), Processes: points})
	}
	_, _ = waitResourceHelperDone(t, idleOutput)
	truthUser, truthSystem := waitResourceHelperDone(t, loadedOutput)
	end, err := sampler.sample([]*benchmarkProcess{idle, loaded})
	if err != nil {
		t.Fatal(err)
	}
	duration := time.Since(measurementStart)
	samples = append(samples, resourceSampleSet{OffsetNs: duration.Nanoseconds(), Processes: end})
	idleUser, idleSystem, err := cpuDelta(start[0], end[0])
	if err != nil {
		t.Fatal(err)
	}
	loadedUser, loadedSystem, err := cpuDelta(start[1], end[1])
	if err != nil {
		t.Fatal(err)
	}
	idleCPU := time.Duration(idleUser+idleSystem) * time.Nanosecond
	if idleCPU > 250*time.Millisecond {
		t.Fatalf("idle CPU calibration = %s, want at most 250ms", idleCPU)
	}
	truthTotal := truthUser + truthSystem
	if truthTotal < uint64(4*calibrationBurnTime/5) || truthUser < uint64(200*time.Millisecond) || truthSystem < uint64(100*time.Millisecond) {
		t.Fatalf("native CPU truth total=%s user=%s system=%s, want at least 800ms/200ms/100ms",
			time.Duration(truthTotal), time.Duration(truthUser), time.Duration(truthSystem))
	}
	assertCPUCalibration(t, "user", loadedUser, truthUser)
	assertCPUCalibration(t, "system", loadedSystem, truthSystem)
	t.Logf("CPU calibration adapter user=%s system=%s; native user=%s system=%s; quantization=%s envelope=%s",
		time.Duration(loadedUser), time.Duration(loadedSystem), time.Duration(truthUser), time.Duration(truthSystem),
		calibrationQuantization, calibrationEnvelope)
	aggregateRSS, err := summarizeRSS(samples, duration)
	if err != nil || aggregateRSS.AverageBytes == 0 || aggregateRSS.MaximumBytes == 0 {
		t.Fatalf("live aggregate RSS = %+v, %v", aggregateRSS, err)
	}
	loadedRSS, err := summarizeRSS(samples, duration, loaded.role)
	if err != nil || aggregateRSS.AverageBytes <= loadedRSS.AverageBytes || aggregateRSS.MaximumBytes < loadedRSS.MaximumBytes {
		t.Fatalf("aggregate RSS %+v does not contain loaded RSS %+v: %v", aggregateRSS, loadedRSS, err)
	}

	originalGeneration := loaded.generation
	loaded.generation = "wrong-generation"
	if _, err := sampler.sample([]*benchmarkProcess{idle, loaded}); err == nil || !strings.Contains(err.Error(), "changed generation") {
		t.Fatalf("generation mismatch error = %v", err)
	}
	loaded.generation = originalGeneration

	if _, err := idleInput.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadedInput.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	if err := idle.wait(2 * time.Second); err != nil {
		t.Fatalf("idle helper: %v", err)
	}
	if err := loaded.wait(2 * time.Second); err != nil {
		t.Fatalf("loaded helper: %v", err)
	}
}

func assertCPUCalibration(t *testing.T, name string, sampled, truth uint64) {
	t.Helper()
	if sampled+uint64(calibrationQuantization) < truth || sampled > truth+uint64(calibrationEnvelope) {
		t.Fatalf("%s CPU adapter=%s native=%s, want native-%s <= adapter <= native+%s",
			name, time.Duration(sampled), time.Duration(truth), calibrationQuantization, calibrationEnvelope)
	}
}

func startResourceHelper(t *testing.T, role string, allocation int, busy bool) (*benchmarkProcess, io.WriteCloser, *bufio.Reader) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestProcessResourceHelper$")
	cmd.Env = append(os.Environ(),
		"QMUX_RESOURCE_HELPER=hold",
		"QMUX_RESOURCE_HELPER_BYTES="+strconv.Itoa(allocation),
		"QMUX_RESOURCE_HELPER_BUSY="+strconv.FormatBool(busy),
	)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	process, err := startBenchmarkProcess(role, cmd)
	if err != nil {
		t.Fatal(err)
	}
	return process, input, bufio.NewReader(output)
}

func waitResourceHelperLine(t *testing.T, reader *bufio.Reader, want string) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != want {
		t.Fatalf("resource helper line = %q, %v; want %q", line, err, want)
	}
}

func waitResourceHelperDone(t *testing.T, reader *bufio.Reader) (user, system uint64) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read resource helper result: %v", err)
	}
	if count, err := fmt.Sscanf(strings.TrimSpace(line), "done %d %d", &user, &system); err != nil || count != 2 {
		t.Fatalf("resource helper result = %q, %v", line, err)
	}
	return user, system
}

func TestBenchmarkProcessEarlyExit(t *testing.T) {
	sampler, samplerErr := newLiveResourceSampler()
	if samplerErr != nil {
		t.Skip(samplerErr)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestProcessResourceHelper$")
	cmd.Env = append(os.Environ(), "QMUX_RESOURCE_HELPER=exit")
	process, err := startBenchmarkProcess("early-exit", cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.wait(2 * time.Second); err == nil {
		t.Fatal("early-exit helper returned success")
	}
	if err := process.expectAlive(); err == nil || !strings.Contains(err.Error(), "exited early") {
		t.Fatalf("expectAlive error = %v", err)
	}
	if _, err := sampler.sample([]*benchmarkProcess{process}); err == nil || !strings.Contains(err.Error(), "exited early") {
		t.Fatalf("sampler early-exit error = %v", err)
	}
	if err := process.stop(time.Second); err == nil || !strings.Contains(err.Error(), "exited before teardown") {
		t.Fatalf("stop early-exit error = %v", err)
	}
}

func TestBenchmarkProcessStopStatus(t *testing.T) {
	for _, testCase := range []struct {
		role    string
		wantErr bool
	}{{role: "qmux-server", wantErr: true}, {role: "iperf-server"}} {
		process, err := startBenchmarkProcess(testCase.role, exec.Command("sleep", "10"))
		if err != nil {
			t.Fatal(err)
		}
		err = process.stop(time.Second)
		if (err != nil) != testCase.wantErr {
			t.Fatalf("stop %s error = %v, wantErr %t", testCase.role, err, testCase.wantErr)
		}
	}
}

func TestProcessResourceHelper(t *testing.T) {
	mode := os.Getenv("QMUX_RESOURCE_HELPER")
	if mode == "" {
		return
	}
	if mode == "exit" {
		os.Exit(7)
	}
	allocation, err := strconv.Atoi(os.Getenv("QMUX_RESOURCE_HELPER_BYTES"))
	if err != nil {
		t.Fatal(err)
	}
	var buffer []byte
	if allocation > 0 {
		buffer, err = unix.Mmap(-1, 0, allocation, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = unix.Munmap(buffer) }()
	}
	for offset := 0; offset < len(buffer); offset += os.Getpagesize() {
		buffer[offset] = 1
	}
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	command := []byte{0}
	if _, err := io.ReadFull(os.Stdin, command); err != nil || command[0] != 'b' {
		t.Fatalf("read begin command: %v", err)
	}
	var before, after unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &before); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("QMUX_RESOURCE_HELPER_BUSY") == "true" {
		started := time.Now()
		arithmeticDeadline := started.Add(calibrationBurnTime / 2)
		var value uint64
		for time.Now().Before(arithmeticDeadline) {
			value = value*1664525 + 1013904223
		}
		buffer[0] ^= byte(value)
		devNull, err := unix.Open(os.DevNull, unix.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = unix.Close(devNull) }()
		oneByte := []byte{0}
		for time.Now().Before(started.Add(calibrationBurnTime)) {
			if _, err := unix.Write(devNull, oneByte); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		time.Sleep(calibrationBurnTime)
	}
	if err := unix.Getrusage(unix.RUSAGE_SELF, &after); err != nil {
		t.Fatal(err)
	}
	userCPU := after.Utime.Nano() - before.Utime.Nano()
	systemCPU := after.Stime.Nano() - before.Stime.Nano()
	_, _ = fmt.Fprintf(os.Stdout, "done %d %d\n", userCPU, systemCPU)
	if _, err := io.ReadFull(os.Stdin, command); err != nil || command[0] != 'x' {
		t.Fatalf("read exit command: %v", err)
	}
	runtime.KeepAlive(buffer)
}
