package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/Mmx233/QMux/server/auth"
)

func TestVerifyRequiresCompletedVerifiedTLSHandshake(t *testing.T) {
	certificate := &x509.Certificate{}
	authenticator := New()
	registration := auth.Registration{
		Scheme: "untrusted-registration-field",
		Proof:  []byte("must not switch the server policy"),
	}

	if err := authenticator.Verify(tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{certificate}},
	}, registration); err == nil {
		t.Fatal("Verify() accepted an incomplete TLS handshake")
	}
	if err := authenticator.Verify(tls.ConnectionState{
		HandshakeComplete: true,
		PeerCertificates:  []*x509.Certificate{certificate},
	}, auth.Registration{}); err == nil {
		t.Fatal("Verify() accepted an unverified peer certificate")
	}
	verifiedState := tls.ConnectionState{
		HandshakeComplete: true,
		PeerCertificates:  []*x509.Certificate{certificate},
		VerifiedChains:    [][]*x509.Certificate{{certificate}},
	}
	if err := authenticator.Verify(verifiedState, registration); err != nil {
		t.Fatalf("Verify() verified TLS state error = %v", err)
	}
	if err := authenticator.VerifyMesh(verifiedState, auth.MeshRegistration{}); err != nil {
		t.Fatalf("VerifyMesh() verified TLS state error = %v", err)
	}
	if got := authenticator.SelectedScheme(); got != "" {
		t.Fatalf("SelectedScheme() = %q, want empty mTLS-compatible value", got)
	}
	if got := authenticator.SelectedMeshScheme(); got != "" {
		t.Fatalf("SelectedMeshScheme() = %q, want empty mTLS-compatible value", got)
	}
}
