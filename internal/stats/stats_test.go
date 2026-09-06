package stats

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func TestTransportRetainsClosedConnections(t *testing.T) {
	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	defer certServer.Close()
	listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{
		Certificates: certServer.TLS.Certificates, NextProtos: []string{"stats-test"},
	}, &quic.Config{EnableDatagrams: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var tracker Transport
	var expected TransportSnapshot
	for range 2 {
		conn, err := quic.DialAddr(ctx, listener.Addr().String(), &tls.Config{
			InsecureSkipVerify: true, // Local test certificate.
			NextProtos:         []string{"stats-test"},
		}, &quic.Config{EnableDatagrams: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.CloseWithError(0, "test cleanup") })
		peer, err := listener.Accept(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = peer.CloseWithError(0, "test cleanup") })
		tracker.Add(conn)
		tracker.Add(conn)
		if err := conn.SendDatagram([]byte("traffic")); err != nil {
			t.Fatal(err)
		}
		if _, err := peer.ReceiveDatagram(ctx); err != nil {
			t.Fatal(err)
		}
		live := tracker.Snapshot()
		if live.Connections != 1 || live.SentBytes <= expected.SentBytes {
			t.Fatalf("live snapshot: %+v", live)
		}
		if err := conn.CloseWithError(0, "test finished"); err != nil {
			t.Fatal(err)
		}
		expected.addTraffic(conn.ConnectionStats())
		tracker.Remove(conn)
		tracker.Remove(conn)
		if got := tracker.Snapshot(); got != expected {
			t.Fatalf("retired snapshot = %+v, want %+v", got, expected)
		}
		if err := peer.CloseWithError(0, "test finished"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOperationConcurrentAccounting(t *testing.T) {
	var operation Operation
	var writers sync.WaitGroup
	for range 8 {
		writers.Go(func() {
			for range 100 {
				start := operation.Start()
				operation.Finish(start, Result(context.DeadlineExceeded, "error"))
			}
		})
	}
	writers.Wait()
	snapshot := operation.Snapshot()
	if snapshot.Attempts != 800 || snapshot.Results["timeout"] != 800 || snapshot.Duration.Count != 800 || snapshot.Duration.Sum < 0 {
		t.Fatalf("operation accounting = %+v", snapshot)
	}
	snapshot.Results["timeout"] = 0
	if operation.Snapshot().Results["timeout"] != 800 {
		t.Fatal("snapshot aliases live counters")
	}
}
