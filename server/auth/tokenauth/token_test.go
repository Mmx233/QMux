package tokenauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
	"github.com/Mmx233/QMux/server/auth"
)

func TestVerifyRequiresConfiguredScheme(t *testing.T) {
	authenticator, err := New([]byte("0123456789abcdef"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, scheme := range []string{"", "mtls", sharedtoken.Scheme + "-other"} {
		err := authenticator.Verify(tls.ConnectionState{}, auth.Registration{Scheme: scheme, Proof: make([]byte, sharedtoken.ProofSize)})
		if err == nil {
			t.Fatalf("Verify() accepted scheme %q", scheme)
		}
	}
}

func TestVerifyExporterBoundProof(t *testing.T) {
	clientState, serverState := connectedTLSStates(t)
	secret := []byte("0123456789abcdef0123456789abcdef")
	registration := auth.Registration{
		ClientID:     "client-1",
		Version:      "2.0",
		Capabilities: []string{"udp-wire-v2"},
		Scheme:       sharedtoken.Scheme,
	}
	proof, err := sharedtoken.Compute(secret, sharedtoken.Transcript{
		ClientID:     registration.ClientID,
		Version:      registration.Version,
		Capabilities: registration.Capabilities,
	}, clientState)
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	registration.Proof = proof

	authenticator, err := New(secret)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := authenticator.Verify(serverState, registration); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	registration.Proof = append([]byte(nil), proof...)
	registration.Proof[0] ^= 0xff
	if err := authenticator.Verify(serverState, registration); err == nil {
		t.Fatal("Verify() accepted a modified proof")
	}
}

func connectedTLSStates(t *testing.T) (tls.ConnectionState, tls.ConnectionState) {
	t.Helper()
	certificate := selfSignedServerCertificate(t)
	serverRaw, clientRaw := net.Pipe()
	server := tls.Server(serverRaw, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
	})
	client := tls.Client(clientRaw, &tls.Config{
		InsecureSkipVerify: true, // Test-only certificate; exporter binding is under test.
		MinVersion:         tls.VersionTLS13,
	})
	t.Cleanup(func() {
		_ = serverRaw.Close()
		_ = clientRaw.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- server.HandshakeContext(ctx) }()
	go func() { errs <- client.HandshakeContext(ctx) }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("TLS handshake error = %v", err)
		}
	}
	return client.ConnectionState(), server.ConnectionState()
}

func selfSignedServerCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
