package stek

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func generateTestCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"Test"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: privateKey}, pool
}

func newTestSTEKServerConfig(t *testing.T, cert tls.Certificate, overlap uint8) (*RotateManager, *tls.Config) {
	t.Helper()
	manager, err := NewRotateManager(time.Hour, overlap)
	if err != nil {
		t.Fatalf("NewRotateManager: %v", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"test-proto"},
		MinVersion:   tls.VersionTLS13,
	}
	tlsConfig.SetSessionTicketKeys(*manager.Keys.Load())
	tlsConfig.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		config := tlsConfig.Clone()
		config.SetSessionTicketKeys(*manager.Keys.Load())
		return config, nil
	}
	return manager, tlsConfig
}

type stekQUICHarness struct {
	address  string
	listener *quic.Listener
	cancel   context.CancelFunc
	done     chan struct{}
}

func newSTEKQUICHarness(t *testing.T, tlsConfig *tls.Config) *stekQUICHarness {
	t.Helper()
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	transport := &quic.Transport{Conn: udpConn}
	listener, err := transport.Listen(tlsConfig, &quic.Config{MaxIdleTimeout: time.Second})
	if err != nil {
		_ = udpConn.Close()
		t.Fatalf("listen QUIC: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	harness := &stekQUICHarness{
		address:  udpConn.LocalAddr().String(),
		listener: listener,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	go func() {
		defer close(harness.done)
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go func() {
				for {
					stream, err := conn.AcceptStream(ctx)
					if err != nil {
						return
					}
					go func() {
						defer func() { _ = stream.Close() }()
						buffer := make([]byte, 64)
						n, err := stream.Read(buffer)
						if err == nil {
							_, _ = stream.Write(buffer[:n])
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		_ = transport.Close()
		<-harness.done
	})
	return harness
}

func (h *stekQUICHarness) dial(t *testing.T, tlsConfig *tls.Config) *quic.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	conn, err := quic.DialAddr(ctx, h.address, tlsConfig, &quic.Config{MaxIdleTimeout: time.Second})
	if err != nil {
		t.Fatalf("dial QUIC: %v", err)
	}
	return conn
}

func assertSTEKEcho(t *testing.T, conn *quic.Conn, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Write([]byte(payload)); err != nil {
		t.Fatalf("write echo payload: %v", err)
	}
	response := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, response); err != nil {
		t.Fatalf("read echo payload: %v", err)
	}
	if string(response) != payload {
		t.Fatalf("echo = %q, want %q", response, payload)
	}
}

func TestSTEKRotationDoesNotBreakExistingConnections(t *testing.T) {
	cert, roots := generateTestCert(t)
	manager, serverTLS := newTestSTEKServerConfig(t, cert, 2)
	harness := newSTEKQUICHarness(t, serverTLS)
	clientTLS := &tls.Config{RootCAs: roots, ServerName: "localhost", NextProtos: []string{"test-proto"}, MinVersion: tls.VersionTLS13}

	conn := harness.dial(t, clientTLS)
	defer func() { _ = conn.CloseWithError(0, "done") }()
	assertSTEKEcho(t, conn, "before rotation")
	for range 3 {
		if err := manager.rotate(); err != nil {
			t.Fatalf("rotate: %v", err)
		}
	}
	assertSTEKEcho(t, conn, "after rotation")
}

type pinnedSessionCache struct {
	mu     sync.Mutex
	key    string
	state  *tls.ClientSessionState
	stored chan struct{}
}

func newPinnedSessionCache() *pinnedSessionCache {
	return &pinnedSessionCache{stored: make(chan struct{})}
}

func (c *pinnedSessionCache) Put(key string, state *tls.ClientSessionState) {
	if state == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == nil {
		c.key = key
		c.state = state
		close(c.stored)
	}
}

func (c *pinnedSessionCache) Get(key string) (*tls.ClientSessionState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.state != nil && c.key == key
}

func newSTEKResumptionFixture(t *testing.T, oldKeyLimit uint8) (*RotateManager, *stekQUICHarness, *tls.Config) {
	t.Helper()
	cert, roots := generateTestCert(t)
	manager, serverTLS := newTestSTEKServerConfig(t, cert, oldKeyLimit)
	harness := newSTEKQUICHarness(t, serverTLS)
	cache := newPinnedSessionCache()
	clientTLS := &tls.Config{
		RootCAs:            roots,
		ServerName:         "localhost",
		NextProtos:         []string{"test-proto"},
		MinVersion:         tls.VersionTLS13,
		ClientSessionCache: cache,
	}

	first := harness.dial(t, clientTLS)
	if first.ConnectionState().TLS.DidResume {
		t.Fatal("first connection unexpectedly resumed")
	}
	assertSTEKEcho(t, first, "first")
	select {
	case <-cache.stored:
	case <-time.After(time.Second):
		t.Fatal("client did not receive a session ticket")
	}
	_ = first.CloseWithError(0, "done")
	return manager, harness, clientTLS
}

func TestSTEKRotationSessionResumption(t *testing.T) {
	manager, harness, clientTLS := newSTEKResumptionFixture(t, 1)
	if err := manager.rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	second := harness.dial(t, clientTLS)
	defer func() { _ = second.CloseWithError(0, "done") }()
	if !second.ConnectionState().TLS.DidResume {
		t.Fatal("connection did not resume with the retained ticket key")
	}
	assertSTEKEcho(t, second, "resumed")
}

func TestSTEKSevenOldKeySessionResumptionBoundary(t *testing.T) {
	manager, harness, clientTLS := newSTEKResumptionFixture(t, 7)
	for range 7 {
		if err := manager.rotate(); err != nil {
			t.Fatalf("rotate: %v", err)
		}
	}
	second := harness.dial(t, clientTLS)
	if !second.ConnectionState().TLS.DidResume {
		t.Fatal("original ticket did not resume after seven rotations")
	}
	_ = second.CloseWithError(0, "done")

	if err := manager.rotate(); err != nil {
		t.Fatalf("eighth rotate: %v", err)
	}
	third := harness.dial(t, clientTLS)
	defer func() { _ = third.CloseWithError(0, "done") }()
	if third.ConnectionState().TLS.DidResume {
		t.Fatal("original ticket resumed after its key was dropped")
	}
}
