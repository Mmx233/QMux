package tokenauth

import (
	"crypto/tls"
	"fmt"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
	"github.com/Mmx233/QMux/server/auth"
)

// TokenAuth verifies exporter-bound proofs in the registration transaction.
type TokenAuth struct {
	secret []byte
}

var _ auth.Auth = (*TokenAuth)(nil)

func New(secret []byte) (auth.Auth, error) {
	if len(secret) < sharedtoken.MinSecretSize {
		return nil, fmt.Errorf("token must be at least %d bytes", sharedtoken.MinSecretSize)
	}
	return &TokenAuth{secret: append([]byte(nil), secret...)}, nil
}

func (a *TokenAuth) Verify(state tls.ConnectionState, registration auth.Registration) error {
	if registration.Scheme != sharedtoken.Scheme {
		return fmt.Errorf("required token authentication proof is missing")
	}
	if err := sharedtoken.Verify(
		a.secret,
		sharedtoken.Transcript{
			ClientID:     registration.ClientID,
			Version:      registration.Version,
			Capabilities: registration.Capabilities,
		},
		registration.Proof,
		state,
	); err != nil {
		return fmt.Errorf("verify token proof: %w", err)
	}
	return nil
}

func (a *TokenAuth) SelectedScheme() string {
	return sharedtoken.Scheme
}
