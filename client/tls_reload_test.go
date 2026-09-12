package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	certgen "github.com/Mmx233/QMux/cmd/generate/certs"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

func TestConnectionManagerTLSReloadReplacesConfigAndCacheTogether(t *testing.T) {
	var logs bytes.Buffer
	cfg := tlsReloadClientConfig(t, lifecycleClientTLSFiles(t))
	cm, err := NewConnectionManager(cfg, zerolog.New(&logs))
	if err != nil {
		t.Fatalf("NewConnectionManager: %v", err)
	}
	t.Cleanup(func() { _ = cm.Stop() })

	initial := cm.tlsState.Load()
	if initial.certificateNotAfter.IsZero() || initial.caNotAfter.IsZero() {
		t.Fatalf("initial TLS expiry metadata = identity %v, CA %v", initial.certificateNotAfter, initial.caNotAfter)
	}
	initial.sessionCaches.GetOrCreate(cfg.Server.Servers[0].Address)
	if err := cm.tlsReloader.LoadInitial(); err != nil {
		t.Fatalf("unchanged load: %v", err)
	}
	if cm.tlsState.Load() != initial {
		t.Fatal("unchanged load replaced the combined TLS state")
	}

	caPEM, certPEM, keyPEM := generateClientTLSMaterial(t)
	writeTLSMaterial(t, cm.tlsConfig, caPEM, certPEM, keyPEM)
	if err := cm.tlsReloader.LoadInitial(); err != nil {
		t.Fatalf("changed load: %v", err)
	}
	changed := cm.tlsState.Load()
	if changed == initial || changed.baseTLSConfig == initial.baseTLSConfig || changed.sessionCaches == initial.sessionCaches {
		t.Fatal("changed load did not replace TLS config and session caches in one state")
	}
	if changed.sessionCaches.Count() != 0 || initial.sessionCaches.Count() != 1 {
		t.Fatal("cache-manager replacement changed old cache ownership")
	}
	wantCertificateNotAfter := parsePEMCertificate(t, certPEM).NotAfter
	wantCANotAfter := parsePEMCertificate(t, caPEM).NotAfter
	if !changed.certificateNotAfter.Equal(wantCertificateNotAfter) || !changed.caNotAfter.Equal(wantCANotAfter) {
		t.Fatalf("reloaded TLS expiry metadata = identity %v, CA %v; want identity %v, CA %v", changed.certificateNotAfter, changed.caNotAfter, wantCertificateNotAfter, wantCANotAfter)
	}

	const privateMarker = "distinctive-private-key-material"
	if err := os.WriteFile(cm.tlsConfig.ClientKeyFile, []byte(privateMarker), 0o600); err != nil {
		t.Fatalf("write invalid key: %v", err)
	}
	if err := cm.tlsReloader.LoadInitial(); err == nil {
		t.Fatal("invalid key reload succeeded")
	}
	if cm.tlsState.Load() != changed {
		t.Fatal("invalid reload replaced last-known-good state")
	}
	if bytes.Contains(logs.Bytes(), []byte(privateMarker)) {
		t.Fatal("TLS reload log exposed private key material")
	}
}

func TestConnectionManagerStartRereadsFrozenPathsWhenReloadDisabled(t *testing.T) {
	cfg := tlsReloadClientConfig(t, lifecycleClientTLSFiles(t))
	cm, err := NewConnectionManager(cfg, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewConnectionManager: %v", err)
	}
	t.Cleanup(func() { _ = cm.Stop() })
	initial := cm.tlsState.Load()

	caPEM, certPEM, keyPEM := generateClientTLSMaterial(t)
	writeTLSMaterial(t, cm.tlsConfig, caPEM, certPEM, keyPEM)
	cfg.Auth.Method = config.ClientAuthMethodToken
	cfg.TLS = config.ClientTLS{CACertFile: filepath.Join(t.TempDir(), "caller-mutated-missing.pem")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cm.Start(ctx); err != nil {
		t.Fatalf("Start with changed frozen material: %v", err)
	}
	changed := cm.tlsState.Load()
	if changed == initial || changed.sessionCaches == initial.sessionCaches || len(changed.baseTLSConfig.Certificates) != 1 {
		t.Fatal("disabled-mode Start did not publish changed material from frozen mTLS paths")
	}
}

func TestConnectionManagerTLSReloadChangesClientIdentityForNewHandshakes(t *testing.T) {
	tlsFiles := lifecycleClientTLSFiles(t)
	newCAPEM, newCertPEM, newKeyPEM := generateClientTLSMaterial(t)
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(lifecycleTLSData.caPEM) || !clientCAs.AppendCertsFromPEM(newCAPEM) {
		t.Fatal("build server client CA pool")
	}
	serverTLS, _ := lifecycleTLSConfigs(t)
	serverTLS.ClientCAs = clientCAs
	serverTLS.NextProtos = nil
	peer := newLifecyclePeerWithTLS(t, serverTLS, nil)
	tlsFiles.AutoReload = true
	cfg := tlsReloadClientConfig(t, tlsFiles)
	cfg.Server.Servers[0] = peer.endpoint()
	cm, err := NewConnectionManager(cfg, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewConnectionManager: %v", err)
	}
	t.Cleanup(func() { _ = cm.Stop() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := cm.tlsReloader.PrepareStart(ctx, true); err != nil {
		t.Fatalf("start TLS watcher: %v", err)
	}

	serve := func(afterAck func(*quic.Conn) error) (<-chan []byte, <-chan error) {
		identity := make(chan []byte, 1)
		done := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
			state := conn.ConnectionState().TLS
			if len(state.PeerCertificates) == 0 {
				return errors.New("client certificate is missing")
			}
			identity <- bytes.Clone(state.PeerCertificates[0].Raw)
			if err := writeSuccessfulLifecycleAck(stream); err != nil {
				return err
			}
			return afterAck(conn)
		})
		return identity, done
	}
	waitForClose := func(conn *quic.Conn) error {
		<-conn.Context().Done()
		return nil
	}

	firstIdentity, firstDone := serve(func(conn *quic.Conn) error {
		stream, err := conn.AcceptStream(peer.ctx)
		if err != nil {
			return err
		}
		var request [1]byte
		if _, err := io.ReadFull(stream, request[:]); err != nil {
			return err
		}
		if request[0] != 0x41 {
			return errors.New("unexpected request on existing connection")
		}
		if _, err := stream.Write([]byte{0x42}); err != nil {
			return err
		}
		if err := stream.Close(); err != nil {
			return err
		}
		return waitForClose(conn)
	})
	first, err := cm.connectAndRegister(ctx, peer.endpoint())
	if err != nil {
		t.Fatalf("connect with initial client identity: %v", err)
	}
	if got := awaitLifecycle(t, firstIdentity, "initial client identity"); !bytes.Equal(got, lifecycleTLSData.client.Certificate[0]) {
		t.Fatal("initial handshake presented an unexpected client identity")
	}

	initial := cm.tlsState.Load()
	replaceTLSReloadFile(t, tlsFiles.ClientCertFile, newCertPEM)
	replaceTLSReloadFile(t, tlsFiles.ClientKeyFile, newKeyPEM)
	changed := awaitClientTLSStateChange(t, cm, initial)
	if changed.baseTLSConfig == initial.baseTLSConfig || changed.sessionCaches == initial.sessionCaches {
		t.Fatal("client identity reload did not replace TLS config and session caches together")
	}

	oldStream, err := first.Connection().OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream on existing connection after reload: %v", err)
	}
	if _, err := oldStream.Write([]byte{0x41}); err != nil {
		t.Fatalf("write on existing connection after reload: %v", err)
	}
	var response [1]byte
	if _, err := io.ReadFull(oldStream, response[:]); err != nil {
		t.Fatalf("read on existing connection after reload: %v", err)
	}
	if response[0] != 0x42 {
		t.Fatalf("existing connection response = %#x, want 0x42", response[0])
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close initial connection: %v", err)
	}
	if err := awaitLifecycle(t, firstDone, "initial connection cleanup"); err != nil {
		t.Fatal(err)
	}

	newCertificate, err := tls.X509KeyPair(newCertPEM, newKeyPEM)
	if err != nil {
		t.Fatalf("parse generated client identity: %v", err)
	}
	secondIdentity, secondDone := serve(waitForClose)
	second, err := cm.connectAndRegister(ctx, peer.endpoint())
	if err != nil {
		t.Fatalf("connect with reloaded client identity: %v", err)
	}
	if got := awaitLifecycle(t, secondIdentity, "reloaded client identity"); !bytes.Equal(got, newCertificate.Certificate[0]) {
		t.Fatal("new handshake did not present the reloaded client identity")
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close reloaded connection: %v", err)
	}
	if err := awaitLifecycle(t, secondDone, "reloaded connection cleanup"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(tlsFiles.ClientKeyFile, []byte("invalid private key"), 0o600); err != nil {
		t.Fatalf("write invalid client key: %v", err)
	}
	if err := cm.tlsReloader.LoadInitial(); err == nil {
		t.Fatal("invalid client identity reload succeeded")
	}
	if cm.tlsState.Load() != changed {
		t.Fatal("invalid client identity reload replaced last-known-good state")
	}
	thirdIdentity, thirdDone := serve(waitForClose)
	third, err := cm.connectAndRegister(ctx, peer.endpoint())
	if err != nil {
		t.Fatalf("connect with last-known-good client identity: %v", err)
	}
	if got := awaitLifecycle(t, thirdIdentity, "last-known-good client identity"); !bytes.Equal(got, newCertificate.Certificate[0]) {
		t.Fatal("invalid reload changed the operative client identity")
	}
	if err := third.Close(); err != nil {
		t.Fatalf("close last-known-good connection: %v", err)
	}
	if err := awaitLifecycle(t, thirdDone, "last-known-good connection cleanup"); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionManagerTokenModeOmitsConfiguredClientIdentity(t *testing.T) {
	serverTLS, _ := lifecycleTLSConfigs(t)
	serverTLS.ClientAuth = tls.NoClientCert
	serverTLS.ClientCAs = nil
	serverTLS.NextProtos = nil
	peer := newLifecyclePeerWithTLS(t, serverTLS, nil)
	cfg := tlsReloadClientConfig(t, lifecycleClientTLSFiles(t))
	cfg.Auth = tokenTestAuth()
	missingDir := t.TempDir()
	cfg.TLS.ClientCertFile = filepath.Join(missingDir, "missing-client.crt")
	cfg.TLS.ClientKeyFile = filepath.Join(missingDir, "missing-client.key")
	cfg.Server.Servers[0] = peer.endpoint()
	cm, err := NewConnectionManager(cfg, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewConnectionManager: %v", err)
	}
	t.Cleanup(func() { _ = cm.Stop() })
	state := cm.tlsState.Load()
	if got := len(state.baseTLSConfig.Certificates); got != 0 {
		t.Fatalf("token-mode TLS state contains %d client certificates, want 0", got)
	}
	if !state.certificateNotAfter.IsZero() || state.caNotAfter.IsZero() {
		t.Fatalf("token-mode TLS expiry metadata = identity %v, CA %v", state.certificateNotAfter, state.caNotAfter)
	}

	serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, registration protocol.RegisterMsg) error {
		if len(conn.ConnectionState().TLS.PeerCertificates) != 0 {
			return errors.New("token client unexpectedly sent a client certificate")
		}
		if err := verifyTokenRegistration(conn, registration); err != nil {
			return err
		}
		if err := protocol.WriteRegisterAckWithAuth(stream, true, "registered", protocol.ProtocolVersion, config.DefaultCapabilities, registration.Auth.Scheme); err != nil {
			return err
		}
		<-conn.Context().Done()
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sc, err := cm.connectAndRegister(ctx, peer.endpoint())
	if err != nil {
		t.Fatalf("connect token client: %v", err)
	}
	if err := sc.Close(); err != nil {
		t.Fatalf("close token client: %v", err)
	}
	if err := awaitLifecycle(t, serverDone, "token client cleanup"); err != nil {
		t.Fatal(err)
	}
}

func tlsReloadClientConfig(t *testing.T, tlsFiles config.ClientTLS) *config.Client {
	t.Helper()
	return &config.Client{
		ClientID: "tls-reload-client",
		Server: config.ClientServer{Servers: []config.ServerEndpoint{{
			Address: "127.0.0.1:1", ServerName: "localhost",
		}}},
		Local:             config.LocalService{Host: "127.0.0.1", Port: 1},
		TLS:               tlsFiles,
		HeartbeatInterval: time.Hour,
		HealthTimeout:     2 * time.Hour,
	}
}

func generateClientTLSMaterial(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	caKey, caCert, err := certgen.GenerateCA(1)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	clientKey, clientCert, err := certgen.GenerateClientCert(caKey, caCert, 1)
	if err != nil {
		t.Fatalf("generate client certificate: %v", err)
	}
	return certgen.EncodeCertificate(caCert), certgen.EncodeCertificate(clientCert), certgen.EncodePrivateKey(clientKey)
}

func writeTLSMaterial(t *testing.T, paths config.ClientTLS, caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	for path, contents := range map[string][]byte{
		paths.CACertFile: caPEM, paths.ClientCertFile: certPEM, paths.ClientKeyFile: keyPEM,
	} {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatalf("write TLS material: %v", err)
		}
	}
}

func parsePEMCertificate(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("decode certificate PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return certificate
}

func replaceTLSReloadFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	temporary := path + ".next"
	if err := os.WriteFile(temporary, contents, 0o600); err != nil {
		t.Fatalf("write replacement TLS file: %v", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatalf("replace TLS file: %v", err)
	}
}

func awaitClientTLSStateChange(t *testing.T, cm *ConnectionManager, previous *clientTLSState) *clientTLSState {
	t.Helper()
	deadline := time.NewTimer(clientLifecycleTimeout)
	defer deadline.Stop()
	for {
		if current := cm.tlsState.Load(); current != previous {
			return current
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for client TLS state change")
		case <-time.After(time.Millisecond):
		}
	}
}
