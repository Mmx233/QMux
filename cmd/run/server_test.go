package run

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mmx233/QMux/server"
)

func TestAdminHandler(t *testing.T) {
	ready := false
	handler := newAdminHandler(func() server.Snapshot { return server.Snapshot{Ready: ready} })
	tests := []struct {
		name       string
		path       string
		setReady   bool
		wantStatus int
		wantBody   string
	}{
		{name: "healthy", path: "/healthyz", wantStatus: http.StatusOK, wantBody: "ok\n"},
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
		func() server.Snapshot { return server.Snapshot{} },
		occupied.Addr().String(),
	)
	if err == nil || !strings.Contains(err.Error(), "listen admin") {
		t.Fatalf("runServerComponents() error = %v, want admin bind failure", err)
	}
	if started.Load() {
		t.Fatal("core started after admin bind failure")
	}
}

func TestRunServerComponentsWithoutAdmin(t *testing.T) {
	coreErr := errors.New("core failed")
	err := runServerComponents(
		context.Background(),
		func(context.Context) error { return coreErr },
		func() server.Snapshot { return server.Snapshot{} },
		"",
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
			func() server.Snapshot { return server.Snapshot{} },
			adminAddr,
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
			func() server.Snapshot { return server.Snapshot{} },
			"",
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
		func() server.Snapshot { return server.Snapshot{} },
		adminAddr,
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
