package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/Mmx233/QMux/server/auth"
)

// MTLSAuth implements mTLS authentication
//
//goland:noinspection GoNameStartsWithPackageName
type MTLSAuth struct {
	caCertPool *x509.CertPool
}

// New creates a new mTLS authenticator
func New(caCertPool *x509.CertPool) auth.Auth {
	return &MTLSAuth{caCertPool: caCertPool}
}

// Verify preserves the existing application-level mTLS verification after the
// transport handshake has completed.
func (m *MTLSAuth) Verify(state tls.ConnectionState, _ auth.Registration) error {
	if !state.HandshakeComplete {
		return fmt.Errorf("TLS handshake is incomplete")
	}
	if len(state.PeerCertificates) == 0 {
		return fmt.Errorf("no client certificate provided")
	}
	opts := x509.VerifyOptions{
		Roots:     m.caCertPool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if _, err := state.PeerCertificates[0].Verify(opts); err != nil {
		return fmt.Errorf("certificate verification failed: %w", err)
	}
	return nil
}

func (m *MTLSAuth) SelectedScheme() string {
	return ""
}
