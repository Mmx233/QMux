package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/Mmx233/QMux/server/auth"
)

func TestVerifyPreservesMTLSCertificateValidation(t *testing.T) {
	ca, leaf := clientCertificateChain(t)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	authenticator := New(pool)
	registration := auth.Registration{
		Scheme: "untrusted-registration-field",
		Proof:  []byte("must not switch the server policy"),
	}

	if err := authenticator.Verify(tls.ConnectionState{
		HandshakeComplete: true,
		PeerCertificates:  []*x509.Certificate{leaf},
	}, registration); err != nil {
		t.Fatalf("Verify() valid client certificate error = %v", err)
	}
	if got := authenticator.SelectedScheme(); got != "" {
		t.Fatalf("SelectedScheme() = %q, want empty mTLS-compatible value", got)
	}
	if err := authenticator.Verify(tls.ConnectionState{HandshakeComplete: true}, auth.Registration{}); err == nil {
		t.Fatal("Verify() accepted a missing client certificate")
	}
	wrongUsage := *leaf
	wrongUsage.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	if err := authenticator.Verify(tls.ConnectionState{
		HandshakeComplete: true,
		PeerCertificates:  []*x509.Certificate{&wrongUsage},
	}, auth.Registration{}); err == nil {
		t.Fatal("Verify() accepted a certificate without client authentication usage")
	}
}

func clientCertificateChain(t *testing.T) (*x509.Certificate, *x509.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "client"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse client certificate: %v", err)
	}
	return ca, leaf
}
