package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server"
)

const faultTimelineLimit = 4096

type faultEvent struct {
	elapsed time.Duration
	message string
}

type faultTimeline struct {
	origin  time.Time
	mu      sync.Mutex
	events  []faultEvent
	dropped int
}

func newFaultTimeline(t testing.TB, label string) *faultTimeline {
	t.Helper()
	timeline := &faultTimeline{origin: time.Now(), events: make([]faultEvent, 0, faultTimelineLimit)}
	t.Cleanup(func() {
		if t.Failed() || testing.Verbose() {
			t.Logf("%s fault timeline:\n%s", label, timeline)
		}
	})
	return timeline
}

func (l *faultTimeline) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	event := faultEvent{elapsed: time.Since(l.origin), message: fmt.Sprintf(format, args...)}
	if len(l.events) == faultTimelineLimit {
		copy(l.events, l.events[1:])
		l.events = l.events[:faultTimelineLimit-1]
		l.dropped++
	}
	l.events = append(l.events, event)
}

func (l *faultTimeline) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out strings.Builder
	if l.dropped > 0 {
		_, _ = fmt.Fprintf(&out, "(%d earlier events dropped)\n", l.dropped)
	}
	for _, event := range l.events {
		_, _ = fmt.Fprintf(&out, "%9s  %s\n", event.elapsed.Round(time.Millisecond), event.message)
	}
	return out.String()
}

type faultRun struct {
	name     string
	cancel   context.CancelFunc
	terminal chan error
	exited   chan struct{}
	exitErr  error

	joinMu    sync.Mutex
	joined    bool
	joinedErr error
}

func startFaultRun(
	parent context.Context,
	name string,
	timeline *faultTimeline,
	start func(context.Context) error,
) *faultRun {
	ctx, cancel := context.WithCancel(parent)
	run := &faultRun{
		name:     name,
		cancel:   cancel,
		terminal: make(chan error, 1),
		exited:   make(chan struct{}),
	}
	timeline.add("%s start", name)
	go func() {
		run.exitErr = start(ctx)
		run.terminal <- run.exitErr
		close(run.exited)
		timeline.add("%s exit: %v", name, run.exitErr)
	}()
	return run
}

func (r *faultRun) join(timeout time.Duration) error {
	r.joinMu.Lock()
	defer r.joinMu.Unlock()
	if r.joined {
		return r.joinedErr
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r.joinedErr = <-r.terminal:
		r.joined = true
		return r.joinedErr
	case <-timer.C:
		return fmt.Errorf("timed out after %s joining %s", timeout, r.name)
	}
}

func (r *faultRun) stopAndJoin(timeout time.Duration) error {
	r.cancel()
	err := r.join(timeout)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

type faultServerRun struct {
	*server.Server
	run *faultRun
}

func startFaultServer(
	t testing.TB,
	parent context.Context,
	name string,
	cfg *config.Server,
	timeline *faultTimeline,
) *faultServerRun {
	t.Helper()
	instance, err := server.New(cfg)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return &faultServerRun{
		Server: instance,
		run:    startFaultRun(parent, name, timeline, instance.Start),
	}
}

func startFaultClient(
	parent context.Context,
	name string,
	instance *client.Client,
	timeline *faultTimeline,
) *faultRun {
	return startFaultRun(parent, name, timeline, instance.Start)
}

type faultProcess struct {
	run      *faultRun
	cmd      *exec.Cmd
	killOnce sync.Once
	killErr  error
}

func startFaultProcess(name string, cmd *exec.Cmd, timeline *faultTimeline) (*faultProcess, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	process := &faultProcess{cmd: cmd}
	process.run = &faultRun{
		name:     name,
		terminal: make(chan error, 1),
		exited:   make(chan struct{}),
	}
	timeline.add("%s start pid=%d", name, cmd.Process.Pid)
	go func() {
		process.run.exitErr = cmd.Wait()
		process.run.terminal <- process.run.exitErr
		close(process.run.exited)
		timeline.add("%s exit: %v", name, process.run.exitErr)
	}()
	return process, nil
}

func (p *faultProcess) kill() error {
	p.killOnce.Do(func() {
		p.killErr = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	})
	return p.killErr
}

func (p *faultProcess) killAndJoin(timeout time.Duration) error {
	if err := p.kill(); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	err := p.run.join(timeout)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus)
		if ok && status.Signaled() && status.Signal() == syscall.SIGKILL {
			return nil
		}
	}
	if err == nil {
		return fmt.Errorf("%s exited cleanly before SIGKILL", p.run.name)
	}
	return fmt.Errorf("%s did not exit from SIGKILL: %w", p.run.name, err)
}

func waitForFault(
	ctx context.Context,
	timeout time.Duration,
	describe func() string,
	ready func(time.Duration) bool,
	runs ...*faultRun,
) error {
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		for _, run := range runs {
			select {
			case <-run.exited:
				return fmt.Errorf("%s exited while waiting for %s: %v", run.name, describe(), run.exitErr)
			default:
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, describe())
		}
		if ready(remaining) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("context ended waiting for %s: %w", describe(), context.Cause(ctx))
		case <-timer.C:
			return fmt.Errorf("timed out after %s waiting for %s", timeout, describe())
		case <-ticker.C:
		}
	}
}

func sequencePayload(sequence uint64) []byte {
	payload := make([]byte, len("qmux-fault:")+8)
	copy(payload, "qmux-fault:")
	binary.BigEndian.PutUint64(payload[len("qmux-fault:"):], sequence)
	return payload
}

func probeSequencedTCP(address string, sequence uint64, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}
	payload := sequencePayload(sequence)
	if n, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write: %w", err)
	} else if n != len(payload) {
		return io.ErrShortWrite
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echo); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if !bytes.Equal(echo, payload) {
		return fmt.Errorf("echo mismatch: got %x, want %x", echo, payload)
	}
	return nil
}

type sequencedUDPProbe struct {
	conn *net.UDPConn
	buf  []byte
	next atomic.Uint64
}

func newSequencedUDPProbe(address string) (*sequencedUDPProbe, error) {
	remote, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve UDP probe address: %w", err)
	}
	conn, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		return nil, fmt.Errorf("dial UDP probe: %w", err)
	}
	return &sequencedUDPProbe{conn: conn, buf: make([]byte, 64)}, nil
}

func (p *sequencedUDPProbe) probe(sequence uint64, timeout time.Duration) error {
	if err := p.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set UDP deadline: %w", err)
	}
	payload := sequencePayload(sequence)
	if n, err := p.conn.Write(payload); err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("UDP refused: %w", err)
		}
		return fmt.Errorf("write UDP: %w", err)
	} else if n != len(payload) {
		return io.ErrShortWrite
	}
	for {
		n, err := p.conn.Read(p.buf)
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				return fmt.Errorf("UDP refused: %w", err)
			}
			return fmt.Errorf("read UDP: %w", err)
		}
		if n != len(payload) || !bytes.Equal(p.buf[:len("qmux-fault:")], []byte("qmux-fault:")) {
			return fmt.Errorf("invalid UDP echo: %x", p.buf[:n])
		}
		got := binary.BigEndian.Uint64(p.buf[len("qmux-fault:"):n])
		if got != sequence {
			continue
		}
		if !bytes.Equal(p.buf[:n], payload) {
			return fmt.Errorf("UDP echo mismatch for sequence %d", sequence)
		}
		return nil
	}
}

func (p *sequencedUDPProbe) probeNext(timeout time.Duration) (uint64, error) {
	sequence := p.next.Add(1)
	return sequence, p.probe(sequence, timeout)
}

func (p *sequencedUDPProbe) Close() error {
	return p.conn.Close()
}

func probeSequencedUDPEventually(
	ctx context.Context,
	timeline *faultTimeline,
	probe *sequencedUDPProbe,
	timeout time.Duration,
	perCall time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("UDP probe did not recover within %s", timeout)
		}
		sequence, err := probe.probeNext(min(perCall, remaining))
		if err == nil {
			timeline.add("synchronous UDP probe %d ok", sequence)
			return nil
		}
		timeline.add("synchronous UDP probe %d fail: %v", sequence, err)
		timer := time.NewTimer(min(20*time.Millisecond, time.Until(deadline)))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}

type probeCollector struct {
	run          *faultRun
	tcpFailures  atomic.Int64
	tcpSuccesses atomic.Int64
	udpSuccesses atomic.Int64
}

func startProbeCollector(
	parent context.Context,
	name string,
	address string,
	udp *sequencedUDPProbe,
	interval time.Duration,
	probeTimeout time.Duration,
	timeline *faultTimeline,
) *probeCollector {
	collector := &probeCollector{}
	collector.run = startFaultRun(parent, name, timeline, func(ctx context.Context) error {
		var sequence uint64
		for {
			sequence++
			if err := probeSequencedTCP(address, sequence, probeTimeout); err != nil {
				collector.tcpFailures.Add(1)
				timeline.add("probe %d TCP fail: %v", sequence, err)
			} else {
				collector.tcpSuccesses.Add(1)
				timeline.add("probe %d TCP ok", sequence)
			}
			if udp != nil {
				udpSequence, err := udp.probeNext(probeTimeout)
				if err != nil {
					timeline.add("UDP probe %d fail: %v", udpSequence, err)
				} else {
					collector.udpSuccesses.Add(1)
					timeline.add("UDP probe %d ok", udpSequence)
				}
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return nil
			case <-timer.C:
			}
		}
	})
	return collector
}
