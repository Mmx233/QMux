package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/cmd/generate/certs"
	"github.com/Mmx233/QMux/config"
	"gopkg.in/yaml.v3"
)

func TestCoordinateClientSignals(t *testing.T) {
	t.Run("prequeued first signal filters stopped Start", func(t *testing.T) {
		signals := make(chan os.Signal, 1)
		signals <- syscall.SIGTERM
		startRelease := make(chan struct{})
		err := coordinateClientSignals(
			func(context.Context) error { <-startRelease; return client.ErrClientStopped },
			func(context.Context) error { close(startRelease); return nil },
			func() error { return errors.New("unexpected force") }, signals, func() {},
		)
		if err != nil {
			t.Fatalf("coordinate error = %v, want nil", err)
		}
	})

	t.Run("first signal drains and joins Start", func(t *testing.T) {
		signals := make(chan os.Signal, 2)
		startRelease := make(chan struct{})
		shutdownCalled := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- coordinateClientSignals(
				func(context.Context) error { <-startRelease; return nil },
				func(ctx context.Context) error {
					if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 29*time.Second {
						return errors.New("shutdown deadline is not 30 seconds")
					}
					close(shutdownCalled)
					close(startRelease)
					return nil
				},
				func() error { return errors.New("unexpected force") }, signals, func() {},
			)
		}()
		signals <- syscall.SIGTERM
		<-shutdownCalled
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("signal path preserves joined startup error", func(t *testing.T) {
		signals := make(chan os.Signal, 1)
		signals <- syscall.SIGTERM
		startRelease := make(chan struct{})
		want := errors.New("client startup failed")
		err := coordinateClientSignals(
			func(context.Context) error {
				<-startRelease
				return errors.Join(client.ErrClientStopped, want)
			},
			func(context.Context) error { close(startRelease); return nil },
			func() error { return errors.New("unexpected force") }, signals, func() {},
		)
		if !errors.Is(err, want) {
			t.Fatalf("coordinate error = %v, want joined startup error", err)
		}
	})

	t.Run("second signal restores defaults before force", func(t *testing.T) {
		signals := make(chan os.Signal, 2)
		startRelease := make(chan struct{})
		shutdownRelease := make(chan struct{})
		shutdownCalled := make(chan struct{})
		defaultsRestored := false
		done := make(chan error, 1)
		go func() {
			done <- coordinateClientSignals(
				func(context.Context) error { <-startRelease; return client.ErrClientStopped },
				func(context.Context) error { close(shutdownCalled); <-shutdownRelease; return errors.New("preempted") },
				func() error {
					if !defaultsRestored {
						return errors.New("force ran before signal defaults were restored")
					}
					close(startRelease)
					close(shutdownRelease)
					return nil
				}, signals, func() { defaultsRestored = true },
			)
		}()
		signals <- syscall.SIGTERM
		<-shutdownCalled
		signals <- syscall.SIGTERM
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("shutdown error is preserved after Start joins", func(t *testing.T) {
		signals := make(chan os.Signal, 1)
		startRelease := make(chan struct{})
		want := errors.New("graceful shutdown failed")
		done := make(chan error, 1)
		go func() {
			done <- coordinateClientSignals(
				func(context.Context) error { <-startRelease; return nil },
				func(context.Context) error { close(startRelease); return want },
				func() error { return errors.New("unexpected force") }, signals, func() {},
			)
		}()
		signals <- syscall.SIGTERM
		if err := <-done; !errors.Is(err, want) {
			t.Fatalf("coordinate error = %v, want shutdown error", err)
		}
	})

	t.Run("startup error is returned", func(t *testing.T) {
		want := errors.New("client startup failed")
		err := coordinateClientSignals(
			func(context.Context) error { return want },
			func(context.Context) error { return nil },
			func() error { return nil }, make(chan os.Signal), func() {},
		)
		if !errors.Is(err, want) {
			t.Fatalf("coordinate error = %v, want startup error", err)
		}
	})
}

func TestFinishClientSignalResultQueuedForce(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	startRelease := make(chan struct{})
	startDone := make(chan error)
	startJoined := make(chan struct{})
	go func() {
		<-startRelease
		close(startJoined)
		startDone <- client.ErrClientStopped
	}()

	defaultsRestored := false
	stopCalls := 0
	err := finishClientSignalResult(
		errors.New("first shutdown failed"), signals, startDone, nil,
		func() error {
			stopCalls++
			if !defaultsRestored {
				return errors.New("stop ran before signal defaults were restored")
			}
			close(startRelease)
			return nil
		},
		func() { defaultsRestored = true },
	)
	if err != nil {
		t.Fatalf("queued force result = %v, want nil", err)
	}
	if stopCalls != 1 {
		t.Fatalf("Stop calls = %d, want 1", stopCalls)
	}
	select {
	case <-startJoined:
	default:
		t.Fatal("queued force returned before Start joined")
	}
}

const (
	runClientTestConfig = "QMUX_RUN_CLIENT_TEST_CONFIG"
	runClientSignalTest = "QMUX_RUN_CLIENT_SIGNAL_TEST"
)

type runClientTestBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *runClientTestBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *runClientTestBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type runClientTestProcess struct {
	command *exec.Cmd
	stderr  runClientTestBuffer
	done    chan struct{}
	err     error
}

func TestRunClientLifecycle(t *testing.T) {
	if os.Getenv(runClientSignalTest) != "" {
		runClientSignalTestChild()
		return
	}
	if path := os.Getenv(runClientTestConfig); path != "" {
		configFile = path
		if err := runClient(nil, nil); err != nil {
			t.Fatal(err)
		}
		return
	}

	_, ca, err := certs.GenerateCA(1)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, certs.EncodeCertificate(ca), 0600); err != nil {
		t.Fatalf("write CA: %v", err)
	}

	t.Run("SIGTERM joins the single client run", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("SIGTERM is not supported on Windows")
		}
		process := startRunClientTestProcess(t, writeRunClientTestConfig(t, caPath))
		process.waitForLogs(t, "starting QMux client", "starting client")

		if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("signal client: %v", err)
		}
		if err, exited := process.wait(time.Second); !exited {
			t.Fatalf("client did not exit within 1s of SIGTERM:\n%s", process.stderr.String())
		} else if err != nil {
			t.Fatalf("client exited after SIGTERM: %v\n%s", err, process.stderr.String())
		}

		logs := process.stderr.String()
		shutdown := strings.Index(logs, "client shutdown complete")
		stopped := strings.LastIndex(logs, "client stopped")
		if shutdown < 0 || stopped < 0 || shutdown >= stopped {
			t.Fatalf("shutdown log order is invalid:\n%s", logs)
		}
	})

	t.Run("third signal uses default termination", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("SIGTERM is not supported on Windows")
		}
		process := startRunClientTestProcess(t, "", runClientSignalTest+"=1")
		process.waitForLogs(t, "signal child ready")
		if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("send first signal: %v", err)
		}
		process.waitForLogs(t, "shutdown blocked")
		if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("send second signal: %v", err)
		}
		process.waitForLogs(t, "stop blocked")
		if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("send third signal: %v", err)
		}
		err, exited := process.wait(time.Second)
		if !exited {
			t.Fatalf("signal child survived the third SIGTERM:\n%s", process.stderr.String())
		}
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("signal child wait error = %T %v, want signal exit", err, err)
		}
		status, ok := exitError.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
			t.Fatalf("signal child exit status = %v, want SIGTERM", exitError.ProcessState)
		}
	})

	t.Run("missing credentials fail fast", func(t *testing.T) {
		missingCA := filepath.Join(t.TempDir(), "missing-ca.crt")
		process := startRunClientTestProcess(t, writeRunClientTestConfig(t, missingCA))
		err, exited := process.wait(4 * time.Second)
		if !exited {
			t.Fatal("client did not exit before the former CLI 5s retry interval")
		}
		if err == nil {
			t.Fatal("client exited successfully with a missing CA")
		}
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("client wait error = %T %v, want exit error", err, err)
		}
		if status, ok := exitError.Sys().(syscall.WaitStatus); !ok || status.Signaled() {
			t.Fatalf("client exit status = %v, want unsignaled credential failure", exitError.ProcessState)
		}
		if logs := process.stderr.String(); strings.Contains(logs, "starting QMux client") {
			t.Fatalf("client started after credential failure:\n%s", logs)
		}
	})
}

func runClientSignalTestChild() {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	_, _ = fmt.Fprintln(os.Stderr, "signal child ready")
	_ = coordinateClientSignals(
		func(context.Context) error { select {} },
		func(context.Context) error {
			_, _ = fmt.Fprintln(os.Stderr, "shutdown blocked")
			select {}
		},
		func() error {
			_, _ = fmt.Fprintln(os.Stderr, "stop blocked")
			select {}
		},
		signals,
		func() { signal.Stop(signals) },
	)
}

func writeRunClientTestConfig(t *testing.T, caPath string) string {
	t.Helper()
	data, err := yaml.Marshal(config.Client{
		ClientID: "run-client-lifecycle",
		Server: config.ClientServer{Servers: []config.ServerEndpoint{{
			Address:    "127.0.0.1:1",
			ServerName: "localhost",
		}}},
		Local: config.LocalService{Host: "127.0.0.1", Port: 1},
		Auth:  config.ClientAuth{Method: config.ClientAuthMethodToken, Token: strings.Repeat("t", 32)},
		TLS:   config.ClientTLS{CACertFile: caPath},
	})
	if err != nil {
		t.Fatalf("marshal client config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "client.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write client config: %v", err)
	}
	return path
}

func startRunClientTestProcess(t *testing.T, path string, extraEnv ...string) *runClientTestProcess {
	t.Helper()
	process := &runClientTestProcess{done: make(chan struct{})}
	process.command = exec.Command(os.Args[0], "-test.run=^TestRunClientLifecycle$")
	for _, value := range process.command.Environ() {
		if !strings.HasPrefix(value, runClientTestConfig+"=") &&
			!strings.HasPrefix(value, runClientSignalTest+"=") &&
			!strings.HasPrefix(value, "GORACE=") {
			process.command.Env = append(process.command.Env, value)
		}
	}
	// The race runtime otherwise adds a one-second delay after runClient returns.
	process.command.Env = append(process.command.Env, extraEnv...)
	if path != "" {
		process.command.Env = append(process.command.Env, runClientTestConfig+"="+path)
	}
	process.command.Env = append(process.command.Env,
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	process.command.Stderr = &process.stderr
	if err := process.command.Start(); err != nil {
		t.Fatalf("start client subprocess: %v", err)
	}
	go func() {
		process.err = process.command.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		select {
		case <-process.done:
			return
		default:
		}
		_ = process.command.Process.Kill()
		select {
		case <-process.done:
		case <-time.After(time.Second):
			t.Error("client subprocess did not stop during cleanup")
		}
	})
	return process
}

func (p *runClientTestProcess) wait(timeout time.Duration) (error, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.err, true
	case <-timer.C:
		return nil, false
	}
}

func (p *runClientTestProcess) waitForLogs(t *testing.T, markers ...string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		logs := p.stderr.String()
		found := true
		for _, marker := range markers {
			found = found && strings.Contains(logs, marker)
		}
		if found {
			return
		}
		select {
		case <-p.done:
			t.Fatalf("client exited before startup markers: %v\n%s", p.err, logs)
		case <-deadline.C:
			t.Fatalf("timed out waiting for startup markers:\n%s", logs)
		case <-ticker.C:
		}
	}
}
