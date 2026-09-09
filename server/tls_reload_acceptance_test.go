package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
)

const tlsReloadAcceptanceTimeout = 5 * time.Second

type tlsReloadAcceptanceMaterial struct {
	certificate tls.Certificate
	certPEM     []byte
	keyPEM      []byte
}

type tlsReloadAcceptanceServer struct {
	server   *Server
	quicAddr string
	caFile   string
	certFile string
	keyFile  string
}

type tlsReloadAcceptanceSessionCache struct {
	*registrationSessionCache
	hits atomic.Uint64
}

func (c *tlsReloadAcceptanceSessionCache) Get(key string) (*tls.ClientSessionState, bool) {
	state, ok := c.registrationSessionCache.Get(key)
	if ok {
		c.hits.Add(1)
	}
	return state, ok
}

func TestServerTLSReloadUpdatesNewHandshakesWithoutDroppingConnections(t *testing.T) {
	initialServer := newTLSReloadAcceptanceMaterial(t)
	replacementServer := newTLSReloadAcceptanceMaterial(t)
	client := newTLSReloadAcceptanceMaterial(t)
	harness := newTLSReloadAcceptanceServer(t, initialServer, client, 0, nil)

	roots := x509.NewCertPool()
	roots.AddCert(initialServer.certificate.Leaf)
	roots.AddCert(replacementServer.certificate.Leaf)
	clientTLS := tlsReloadAcceptanceClientConfig(client.certificate, roots, nil)
	oldConn := harness.dial(t, clientTLS)
	control := harness.register(t, oldConn, "tls-reload-existing")
	if got := oldConn.ConnectionState().TLS.PeerCertificates[0].Raw; !bytes.Equal(got, initialServer.certificate.Certificate[0]) {
		t.Fatal("initial handshake did not receive the initial server leaf")
	}

	tlsReloadAcceptanceReplaceFile(t, harness.certFile, replacementServer.certPEM)
	tlsReloadAcceptanceReplaceFile(t, harness.keyFile, replacementServer.keyPEM)
	harness.waitForServerLeaf(t, replacementServer.certificate.Certificate[0])

	registered, ok := harness.server.pools[harness.quicAddr].Get("tls-reload-existing")
	if !ok {
		t.Fatal("existing connection disappeared during TLS reload")
	}
	beforeHeartbeat := registered.LastHeartbeat.Load()
	if err := protocol.WriteHeartbeat(control, time.Now().Unix()); err != nil {
		t.Fatalf("write heartbeat on existing connection after reload: %v", err)
	}
	eventually(t, time.Second, func() bool { return registered.LastHeartbeat.Load() > beforeHeartbeat })

	newConn := harness.dial(t, clientTLS.Clone())
	newState := newConn.ConnectionState().TLS
	if newState.DidResume {
		t.Fatal("new leaf assertion unexpectedly used a resumed TLS session")
	}
	if got := newState.PeerCertificates[0].Raw; !bytes.Equal(got, replacementServer.certificate.Certificate[0]) {
		t.Fatal("new full handshake did not receive the replacement server leaf")
	}
}

func TestServerTLSReloadRevalidatesCachedTLS13Trust(t *testing.T) {
	t.Run("client RootCAs withdrawal", func(t *testing.T) {
		serverMaterial := newTLSReloadAcceptanceMaterial(t)
		clientMaterial := newTLSReloadAcceptanceMaterial(t)
		harness := newTLSReloadAcceptanceServer(t, serverMaterial, clientMaterial, 0, nil)
		cache := newTLSReloadAcceptanceSessionCache()
		clientTLS := tlsReloadAcceptanceClientConfig(
			clientMaterial.certificate,
			tlsReloadAcceptanceRoots(serverMaterial.certificate),
			cache,
		)

		first := harness.dial(t, clientTLS)
		assertTLSReloadAcceptanceFullHandshake(t, first)
		waitForTLSReloadAcceptanceTicket(t, cache)
		_ = first.CloseWithError(0, "trust withdrawal")

		withdrawn := clientTLS.Clone()
		withdrawn.RootCAs = x509.NewCertPool()
		hitsBefore := cache.hits.Load()
		harness.assertHandshakeRejected(t, withdrawn)
		if cache.hits.Load() <= hitsBefore {
			t.Fatal("withdrawn RootCAs handshake did not offer the cached TLS session")
		}
	})

	t.Run("server ClientCAs withdrawal", func(t *testing.T) {
		serverMaterial := newTLSReloadAcceptanceMaterial(t)
		clientMaterial := newTLSReloadAcceptanceMaterial(t)
		withdrawnClientCA := newTLSReloadAcceptanceMaterial(t)
		harness := newTLSReloadAcceptanceServer(t, serverMaterial, clientMaterial, 0, nil)
		cache := newTLSReloadAcceptanceSessionCache()
		clientTLS := tlsReloadAcceptanceClientConfig(
			clientMaterial.certificate,
			tlsReloadAcceptanceRoots(serverMaterial.certificate),
			cache,
		)

		first := harness.dial(t, clientTLS)
		assertTLSReloadAcceptanceFullHandshake(t, first)
		waitForTLSReloadAcceptanceTicket(t, cache)
		_ = first.CloseWithError(0, "trust withdrawal")

		previous := harness.server.tlsState.Load()
		tlsReloadAcceptanceReplaceFile(t, harness.caFile, withdrawnClientCA.certPEM)
		eventually(t, tlsReloadAcceptanceTimeout, func() bool {
			return harness.server.tlsState.Load() != previous
		})
		hitsBefore := cache.hits.Load()
		harness.assertHandshakeRejected(t, clientTLS)
		if cache.hits.Load() <= hitsBefore {
			t.Fatal("withdrawn ClientCAs handshake did not offer the cached TLS session")
		}
	})
}

func TestServerTLSReloadCombinedCallbackResumesSessions(t *testing.T) {
	const rotationInterval = 100 * time.Millisecond
	overlap := uint8(7)
	for _, test := range []struct {
		name     string
		interval time.Duration
		overlap  *uint8
		wait     time.Duration
	}{
		{name: "automatic"},
		{name: "custom STEK overlap", interval: rotationInterval, overlap: &overlap, wait: 2 * rotationInterval},
	} {
		t.Run(test.name, func(t *testing.T) {
			serverMaterial := newTLSReloadAcceptanceMaterial(t)
			clientMaterial := newTLSReloadAcceptanceMaterial(t)
			harness := newTLSReloadAcceptanceServer(t, serverMaterial, clientMaterial, test.interval, test.overlap)
			cache := newTLSReloadAcceptanceSessionCache()
			clientTLS := tlsReloadAcceptanceClientConfig(
				clientMaterial.certificate,
				tlsReloadAcceptanceRoots(serverMaterial.certificate),
				cache,
			)

			first := harness.dial(t, clientTLS)
			assertTLSReloadAcceptanceFullHandshake(t, first)
			waitForTLSReloadAcceptanceTicket(t, cache)
			_ = first.CloseWithError(0, "resumption")
			if test.wait != 0 {
				time.Sleep(test.wait)
			}

			second := harness.dial(t, clientTLS)
			if !second.ConnectionState().TLS.DidResume {
				t.Fatal("client did not resume the cached TLS 1.3 session")
			}
			harness.register(t, second, "tls-reload-resumed")
			registered, ok := harness.server.pools[harness.quicAddr].Get("tls-reload-resumed")
			if !ok || !registered.Conn.ConnectionState().TLS.DidResume {
				t.Fatal("server did not resume the cached TLS 1.3 session")
			}
		})
	}
}

func newTLSReloadAcceptanceMaterial(t *testing.T) tlsReloadAcceptanceMaterial {
	t.Helper()
	certificate, _ := registrationTestCertificate(t)
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatalf("marshal TLS private key: %v", err)
	}
	return tlsReloadAcceptanceMaterial{
		certificate: certificate,
		certPEM: pem.EncodeToMemory(&pem.Block{
			Type: "CERTIFICATE", Bytes: certificate.Certificate[0],
		}),
		keyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
}

func newTLSReloadAcceptanceSessionCache() *tlsReloadAcceptanceSessionCache {
	return &tlsReloadAcceptanceSessionCache{registrationSessionCache: newRegistrationSessionCache()}
}

func newTLSReloadAcceptanceServer(
	t *testing.T,
	serverMaterial, clientMaterial tlsReloadAcceptanceMaterial,
	rotationInterval time.Duration,
	overlap *uint8,
) *tlsReloadAcceptanceServer {
	t.Helper()
	dir := t.TempDir()
	harness := &tlsReloadAcceptanceServer{
		quicAddr: tlsReloadAcceptanceUDPAddress(t),
		caFile:   filepath.Join(dir, "client-ca.crt"),
		certFile: filepath.Join(dir, "server.crt"),
		keyFile:  filepath.Join(dir, "server.key"),
	}
	tlsReloadAcceptanceWriteFile(t, harness.caFile, clientMaterial.certPEM)
	tlsReloadAcceptanceWriteFile(t, harness.certFile, serverMaterial.certPEM)
	tlsReloadAcceptanceWriteFile(t, harness.keyFile, serverMaterial.keyPEM)

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr: harness.quicAddr, TrafficAddr: freeSnapshotTCPAddress(t), Protocol: "tcp",
			HandshakeIdleTimeout: time.Second, MaxIdleTimeout: 10 * time.Second,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: harness.caFile},
		TLS: config.ServerTLS{
			ServerCertFile: harness.certFile,
			ServerKeyFile:  harness.keyFile,
			AutoReload:     true,
			SessionTicketEncryptionKeyRotationInterval: rotationInterval,
			SessionTicketEncryptionKeyRotationOverlap:  overlap,
		},
		HeartbeatInterval: time.Hour,
		HealthTimeout:     2 * time.Hour,
	}
	server, err := New(serverConfig)
	if err != nil {
		t.Fatalf("create TLS reload acceptance server: %v", err)
	}
	harness.server = server
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("TLS reload acceptance server stopped with %v", err)
			}
		case <-time.After(tlsReloadAcceptanceTimeout):
			t.Error("timed out stopping TLS reload acceptance server")
		}
	})
	return harness
}

func (h *tlsReloadAcceptanceServer) dial(t *testing.T, clientTLS *tls.Config) *quic.Conn {
	t.Helper()
	deadline := time.Now().Add(tlsReloadAcceptanceTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		conn, err := quic.DialAddr(ctx, h.quicAddr, clientTLS, &quic.Config{HandshakeIdleTimeout: time.Second})
		cancel()
		if err == nil {
			t.Cleanup(func() { _ = conn.CloseWithError(0, "test complete") })
			return conn
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dial TLS reload acceptance server: %v", lastErr)
	return nil
}

func (h *tlsReloadAcceptanceServer) register(t *testing.T, conn *quic.Conn, clientID string) *quic.Stream {
	t.Helper()
	return registerMTLSClient(t, &registrationHarness{
		client: conn,
		pool:   h.server.pools[h.quicAddr],
	}, clientID)
}

func (h *tlsReloadAcceptanceServer) waitForServerLeaf(t *testing.T, leaf []byte) {
	t.Helper()
	eventually(t, tlsReloadAcceptanceTimeout, func() bool {
		state := h.server.tlsState.Load()
		return state != nil && bytes.Equal(state.certificate.Certificate[0], leaf)
	})
}

func (h *tlsReloadAcceptanceServer) assertHandshakeRejected(t *testing.T, clientTLS *tls.Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, h.quicAddr, clientTLS, &quic.Config{HandshakeIdleTimeout: time.Second})
	if err == nil {
		_, err = conn.AcceptStream(ctx)
		_ = conn.CloseWithError(0, "rejected handshake test")
	}
	var transportError *quic.TransportError
	if !errors.As(err, &transportError) || !transportError.ErrorCode.IsCryptoError() {
		t.Fatalf("withdrawn trust handshake error = %T %v, want QUIC crypto close", err, err)
	}
}

func tlsReloadAcceptanceClientConfig(
	clientCertificate tls.Certificate,
	roots *x509.CertPool,
	cache tls.ClientSessionCache,
) *tls.Config {
	return &tls.Config{
		RootCAs:            roots,
		Certificates:       []tls.Certificate{clientCertificate},
		ServerName:         "localhost",
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		ClientSessionCache: cache,
	}
}

func tlsReloadAcceptanceRoots(certificate tls.Certificate) *x509.CertPool {
	roots := x509.NewCertPool()
	roots.AddCert(certificate.Leaf)
	return roots
}

func assertTLSReloadAcceptanceFullHandshake(t *testing.T, conn *quic.Conn) {
	t.Helper()
	state := conn.ConnectionState().TLS
	if state.Version != tls.VersionTLS13 || state.DidResume {
		t.Fatalf("initial TLS state = version %#x, resumed %t; want TLS 1.3 full handshake", state.Version, state.DidResume)
	}
}

func waitForTLSReloadAcceptanceTicket(t *testing.T, cache *tlsReloadAcceptanceSessionCache) {
	t.Helper()
	select {
	case <-cache.put:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS 1.3 session ticket")
	}
}

func tlsReloadAcceptanceUDPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("allocate TLS reload UDP address: %v", err)
	}
	address := listener.LocalAddr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release TLS reload UDP address: %v", err)
	}
	return address
}

func tlsReloadAcceptanceWriteFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write TLS reload material: %v", err)
	}
}

func tlsReloadAcceptanceReplaceFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	temporary := path + ".next"
	tlsReloadAcceptanceWriteFile(t, temporary, contents)
	if err := os.Rename(temporary, path); err != nil {
		t.Fatalf("replace TLS reload material: %v", err)
	}
}
