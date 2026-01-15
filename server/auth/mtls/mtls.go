package mtls

import (
	"context"
	"crypto/x509"
	"fmt"

	"github.com/Mmx233/QMux/server/auth"
	"github.com/quic-go/quic-go"
)

// MTLSAuth implements mTLS authentication
type MTLSAuth struct {
	caCertPool *x509.CertPool
}

// New creates a new mTLS authenticator
func New(caCertPool *x509.CertPool) auth.Auth {
	return &MTLSAuth{
		caCertPool: caCertPool,
	}
}

// VerifyConn verifies a QUIC connection using mTLS
func (m *MTLSAuth) VerifyConn(ctx context.Context, conn *quic.Conn) (bool, error) {
	// Get TLS connection state
	tlsState := conn.ConnectionState().TLS

	// Check if client certificate was provided
	if len(tlsState.PeerCertificates) == 0 {
		return false, fmt.Errorf("no client certificate provided")
	}

	// Verify certificate chain
	opts := x509.VerifyOptions{
		Roots:     m.caCertPool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	_, err := tlsState.PeerCertificates[0].Verify(opts)
	if err != nil {
		return false, fmt.Errorf("certificate verification failed: %w", err)
	}

	return true, nil
}
