package auth

import (
	"crypto/tls"
)

// Registration is the authentication-relevant registration transcript.
// Callers retain ownership of slice storage; Auth implementations must treat
// Capabilities and Proof as read-only and must not retain them after Verify.
type Registration struct {
	ClientID     string
	Version      string
	Capabilities []string
	Scheme       string
	Proof        []byte
}

// MeshRegistration is the authentication-relevant mesh registration transcript.
type MeshRegistration struct {
	Version        string
	Capabilities   []string
	Role           string
	TargetServerID string
	PeerServerID   string
	InstanceID     string
	GroupID        string
	Scheme         string
	Proof          []byte
}

type Auth interface {
	// Verify authenticates a registration against an already completed TLS
	// handshake. Implementations are selected exclusively by server policy.
	Verify(state tls.ConnectionState, registration Registration) error
	VerifyMesh(state tls.ConnectionState, registration MeshRegistration) error

	// SelectedScheme is echoed only after a successful registration.
	SelectedScheme() string
	SelectedMeshScheme() string
}
