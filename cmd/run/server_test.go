package run

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const runServerAdminResolverChild = "QMUX_RUN_SERVER_ADMIN_RESOLVER_CHILD"

func TestAdminHandler(t *testing.T) {
	ready := false
	handler := newAdminHandler(func() bool { return ready }, nil)
	tests := []struct {
		name       string
		path       string
		setReady   bool
		wantStatus int
		wantBody   string
	}{
		{name: "healthy", path: "/healthz", wantStatus: http.StatusOK, wantBody: "ok\n"},
		{name: "not ready", path: "/readyz", wantStatus: http.StatusServiceUnavailable, wantBody: "not ready\n"},
		{name: "ready", path: "/readyz", setReady: true, wantStatus: http.StatusOK, wantBody: "ok\n"},
		{name: "unknown", path: "/unknown", wantStatus: http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ready = test.setReady
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.wantBody != "" && response.Body.String() != test.wantBody {
				t.Fatalf("body = %q, want %q", response.Body.String(), test.wantBody)
			}
		})
	}
}

func TestAdminMetrics(t *testing.T) {
	handler := newAdminHandler(func() bool { return false }, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain;") {
		t.Fatalf("content type = %q, want Prometheus text format", contentType)
	}
	for _, metric := range []string{"# TYPE go_goroutines gauge\n", "\ngo_goroutines ", "# TYPE promhttp_metric_handler_requests_total counter\n"} {
		if !strings.Contains(response.Body.String(), metric) {
			t.Errorf("metrics response is missing %q", metric)
		}
	}
}

func TestAdminBindFailureDoesNotStartCore(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy admin address: %v", err)
	}
	defer func() {
		if err := occupied.Close(); err != nil {
			t.Errorf("release occupied admin address: %v", err)
		}
	}()

	var started atomic.Bool
	err = runServerComponents(
		context.Background(),
		func(context.Context) error {
			started.Store(true)
			return nil
		},
		func() bool { return false },
		occupied.Addr().String(),
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "listen admin") {
		t.Fatalf("runServerComponents() error = %v, want admin bind failure", err)
	}
	if started.Load() {
		t.Fatal("core started after admin bind failure")
	}
}

func TestServerAdminHostnameResolveCancellation(t *testing.T) {
	if os.Getenv(runServerAdminResolverChild) != "" {
		runServerAdminResolverChildProcess(t)
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM is not supported on Windows")
	}

	process := startRunTestProcess(t, "TestServerAdminHostnameResolveCancellation", runServerAdminResolverChild+"=1")
	process.waitForLogs(t, "resolver-entered")
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal server admin resolver child: %v", err)
	}
	if err, exited := process.wait(time.Second); !exited {
		t.Fatalf("server admin resolver child did not exit after SIGTERM:\n%s", process.stderr.String())
	} else if err != nil {
		t.Fatalf("server admin resolver child failed: %v\n%s", err, process.stderr.String())
	}
}

func runServerAdminResolverChildProcess(t *testing.T) {
	var entered sync.Once
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			entered.Do(func() { _, _ = fmt.Fprintln(os.Stderr, "resolver-entered") })
			<-ctx.Done()
			return nil, context.Cause(ctx)
		},
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	var started atomic.Bool
	err := runServerComponents(
		ctx,
		func(context.Context) error {
			started.Store(true)
			return nil
		},
		func() bool { return false },
		"lif002-server-admin.qmux.invalid:9",
		nil,
	)
	if started.Load() {
		t.Fatal("server core started while admin bind was canceled")
	}
	cause := context.Cause(ctx)
	if cause == nil || !errors.Is(err, cause) {
		t.Fatalf("server admin error = %v, want owner cause %v", err, cause)
	}
	if !strings.Contains(err.Error(), "listen admin on lif002-server-admin.qmux.invalid:9") {
		t.Fatalf("server admin error lost address wrapper: %v", err)
	}
}

func TestRunServerComponentsWithoutAdmin(t *testing.T) {
	coreErr := errors.New("core failed")
	err := runServerComponents(
		context.Background(),
		func(context.Context) error { return coreErr },
		func() bool { return false },
		"",
		nil,
	)
	if !errors.Is(err, coreErr) {
		t.Fatalf("runServerComponents() error = %v, want %v", err, coreErr)
	}
}

func TestRunServerComponentsCancellationJoinsCoreAndAdmin(t *testing.T) {
	adminAddr := freeAdminAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	coreStarted := make(chan struct{})
	coreCanceled := make(chan struct{})
	releaseCore := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runServerComponents(
			ctx,
			func(ctx context.Context) error {
				close(coreStarted)
				<-ctx.Done()
				close(coreCanceled)
				<-releaseCore
				return context.Cause(ctx)
			},
			func() bool { return false },
			adminAddr,
			nil,
		)
	}()
	<-coreStarted
	cancel()
	<-coreCanceled
	select {
	case err := <-done:
		t.Fatalf("orchestrator returned before core joined: %v", err)
	default:
	}
	close(releaseCore)

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runServerComponents() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("orchestrator did not join canceled components")
	}
	assertAddressReusable(t, adminAddr)
}

func TestRunServerComponentsReportsCoreErrorAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	coreStarted := make(chan struct{})
	coreErr := errors.New("core shutdown failed")
	done := make(chan error, 1)
	go func() {
		done <- runServerComponents(
			ctx,
			func(ctx context.Context) error {
				close(coreStarted)
				<-ctx.Done()
				return coreErr
			},
			func() bool { return false },
			"",
			nil,
		)
	}()
	<-coreStarted
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, coreErr) {
			t.Fatalf("runServerComponents() error = %v, want %v", err, coreErr)
		}
		if errors.Is(err, context.Canceled) {
			t.Fatalf("runServerComponents() error = %v also reports clean cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("orchestrator did not report core shutdown error")
	}
}

func TestRunServerComponentsCoreErrorShutsDownAdmin(t *testing.T) {
	adminAddr := freeAdminAddress(t)
	coreErr := errors.New("core failed")
	err := runServerComponents(
		context.Background(),
		func(context.Context) error { return coreErr },
		func() bool { return false },
		adminAddr,
		nil,
	)
	if !errors.Is(err, coreErr) {
		t.Fatalf("runServerComponents() error = %v, want %v", err, coreErr)
	}
	assertAddressReusable(t, adminAddr)
}

func freeAdminAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate admin address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release admin address: %v", err)
	}
	return addr
}

func assertAddressReusable(t *testing.T, addr string) {
	t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("address %s was not released: %v", addr, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close rebound address: %v", err)
	}
}
