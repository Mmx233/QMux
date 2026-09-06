package run

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	adminReadHeaderTimeout = 5 * time.Second
	adminShutdownTimeout   = 5 * time.Second
)

func newAdminServer(address string, ready func() bool) (*http.Server, net.Listener, error) {
	if address == "" {
		return nil, nil, nil
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("listen admin on %s: %w", address, err)
	}
	return &http.Server{
		Handler:           newAdminHandler(ready),
		ReadHeaderTimeout: adminReadHeaderTimeout,
	}, listener, nil
}

func shutdownAdmin(server *http.Server) error {
	if server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminShutdownTimeout)
	defer cancel()
	return server.Shutdown(ctx)
}

func newAdminHandler(ready func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeAdminResponse(w, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready() {
			writeAdminResponse(w, http.StatusOK, "ok\n")
			return
		}
		writeAdminResponse(w, http.StatusServiceUnavailable, "not ready\n")
	})
	return mux
}

func writeAdminResponse(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
