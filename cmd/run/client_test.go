package run

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Mmx233/QMux/cmd/generate/certs"
	"github.com/Mmx233/QMux/config"
	"gopkg.in/yaml.v3"
)

const runClientTestConfig = "QMUX_RUN_CLIENT_TEST_CONFIG"

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

func startRunClientTestProcess(t *testing.T, path string) *runClientTestProcess {
	t.Helper()
	process := &runClientTestProcess{done: make(chan struct{})}
	process.command = exec.Command(os.Args[0], "-test.run=^TestRunClientLifecycle$")
	for _, value := range process.command.Environ() {
		if !strings.HasPrefix(value, runClientTestConfig+"=") && !strings.HasPrefix(value, "GORACE=") {
			process.command.Env = append(process.command.Env, value)
		}
	}
	// The race runtime otherwise adds a one-second delay after runClient returns.
	process.command.Env = append(process.command.Env,
		runClientTestConfig+"="+path,
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"),
	)
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
