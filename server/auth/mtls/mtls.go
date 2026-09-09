package mtls

import (
	"crypto/tls"
	"errors"

	"github.com/Mmx233/QMux/server/auth"
)

// MTLSAuth accepts client certificates already verified by the TLS handshake.
type MTLSAuth struct{}

// New creates a new mTLS authenticator. The caller must install client trust in
// tls.Config.ClientCAs; the completed TLS handshake is the sole verification authority.
func New() auth.Auth {
	return &MTLSAuth{}
}

func (*MTLSAuth) Verify(state tls.ConnectionState, _ auth.Registration) error {
	if !state.HandshakeComplete {
		return errors.New("TLS handshake is incomplete")
	}
	if len(state.VerifiedChains) == 0 {
		return errors.New("client certificate was not verified by TLS")
	}
	return nil
}

func (m *MTLSAuth) SelectedScheme() string {
	return ""
}
