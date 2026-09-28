package protocol

import (
	"fmt"
	"slices"
)

// Message types
const (
	MsgTypeRegister        = 0x01 // Client registration
	MsgTypeRegisterAck     = 0x02 // Server acknowledgment
	MsgTypeHeartbeat       = 0x03 // Keepalive
	MsgTypeNewConn         = 0x04 // New connection metadata
	MsgTypeDrainRequest    = 0x05 // Client requests retirement from TCP selection
	MsgTypeConnClose       = 0x06 // Connection closed
	MsgTypeDrainComplete   = 0x07 // Server reports the final accepted TCP stream
	MsgTypeNewConnAck      = 0x08 // Client confirms its backend connection is ready
	MsgTypeMeshRegister    = 0x09 // Mesh client or peer registration
	MsgTypeMeshRegisterAck = 0x0A // Mesh registration acknowledgment
	MsgTypeMeshBegin       = 0x0B // Initial state snapshot boundary
	MsgTypeMeshChunk       = 0x0C // Public group declaration chunk
	MsgTypeMeshPath        = 0x0D // Candidate path record
	MsgTypeMeshWithdraw    = 0x0E // Candidate path withdrawal
	MsgTypeMeshEnd         = 0x0F // Initial state completion boundary
	MsgTypeMeshReady       = 0x10 // Dialer's staged/accepted result
	MsgTypeError           = 0xFF // Error message
)

// RegisterMsg is sent by client to register with server
type RegisterMsg struct {
	ClientID     string        // Unique client identifier
	Version      string        // Protocol version
	Capabilities []string      // Supported features (e.g., "tcp", "udp")
	Auth         *RegisterAuth `json:",omitempty"` // Optional application-layer authentication proof
}

// RegisterAuth carries the authentication scheme and connection-bound proof.
type RegisterAuth struct {
	Scheme string
	Proof  []byte
}

// RegisterAckMsg is sent by server to acknowledge registration
type RegisterAckMsg struct {
	Success              bool     // Registration success
	Message              string   // Optional message
	ServerVersion        string   // Protocol version selected by the server
	SelectedCapabilities []string // Capabilities selected for this connection
	SelectedAuthScheme   string   `json:",omitempty"` // Authentication scheme selected by the server
}

const (
	MeshProtocolVersion     = "1.0"
	CapabilityMeshSessionV1 = "mesh-session-v1"
	MeshRoleClient          = "client"
	MeshRolePeer            = "peer"
	MeshStateStaged         = "staged"
	MeshStateAccepted       = "accepted"
)

// MeshRegister is the mesh-only registration header. Initial group and route
// state is intentionally carried by later mesh tasks on the same transaction.
type MeshRegister struct {
	Version        string
	Capabilities   []string
	Role           string
	TargetServerID string
	PeerServerID   string        `json:",omitempty"`
	InstanceID     string        `json:",omitempty"`
	GroupID        string        `json:",omitempty"`
	Auth           *RegisterAuth `json:",omitempty"`
}

// MeshRegisterAck is the mesh-only registration acknowledgment.
type MeshRegisterAck struct {
	Success              bool
	Message              string   `json:",omitempty"`
	State                string   `json:",omitempty"`
	ServerID             string   `json:",omitempty"`
	Role                 string   `json:",omitempty"`
	SelectedVersion      string   `json:",omitempty"`
	SelectedCapabilities []string `json:",omitempty"`
	SelectedAuthScheme   string   `json:",omitempty"`
}

// HeartbeatMsg is sent periodically to keep connection alive
type HeartbeatMsg struct {
	Timestamp int64 // Unix timestamp
}

// DrainRequestMsg asks the server to retire this generation from TCP selection.
type DrainRequestMsg struct{}

// DrainCompleteMsg reports the last server-initiated bidirectional stream ID.
type DrainCompleteMsg struct {
	AcceptFence int64
}

// NewConnMsg is sent by server to client when new connection arrives
type NewConnMsg struct {
	ConnID     uint64 // Unique connection ID
	Protocol   string // "tcp" or "udp"
	SourceAddr string // Original client address (IP:port)
	DestAddr   string // Target address on traffic listener (IP:port)
	Timestamp  int64  // Connection timestamp
}

// NewConnAckMsg confirms that the client connected to the backend.
type NewConnAckMsg struct {
	ConnID uint64
}

// ConnCloseMsg indicates connection closure
type ConnCloseMsg struct {
	ConnID uint64 // Connection ID
	Reason string // Close reason
}

// ErrorMsg carries error information
type ErrorMsg struct {
	Code    uint32 // Error code
	Message string // Error message
}

// Message wraps a typed message with its type
type Message struct {
	Type    byte
	Payload any
}

// ProtocolVersion is retained as the exported wire-version API.
const ProtocolVersion = "2.0"

const (
	CapabilityUDPWireV2  = "udp-wire-v2"
	CapabilityTCPDrainV1 = "tcp-drain-v1"
)

// HasCapability reports whether capabilities contains capability.
func HasCapability(capabilities []string, capability string) bool {
	return slices.Contains(capabilities, capability)
}

// ValidateRegistration rejects peers that cannot safely exchange UDP wire v2 datagrams.
func ValidateRegistration(version string, capabilities []string) error {
	if version != ProtocolVersion {
		return fmt.Errorf("incompatible protocol version: got %q, require %q", version, ProtocolVersion)
	}
	if !HasCapability(capabilities, CapabilityUDPWireV2) {
		return fmt.Errorf("required capability %q is missing", CapabilityUDPWireV2)
	}
	return nil
}

// SelectCapabilities returns the requested capabilities supported by this peer.
// It preserves request order while removing duplicates.
func SelectCapabilities(requested, supported []string) []string {
	selected := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, capability := range requested {
		if _, duplicate := seen[capability]; duplicate {
			continue
		}
		if HasCapability(supported, capability) {
			selected = append(selected, capability)
			seen[capability] = struct{}{}
		}
	}
	return selected
}

// ValidateRegisterAck verifies the server side of protocol negotiation.
func ValidateRegisterAck(ack RegisterAckMsg) error {
	if !ack.Success {
		return fmt.Errorf("registration failed: %s", ack.Message)
	}
	if err := ValidateRegistration(ack.ServerVersion, ack.SelectedCapabilities); err != nil {
		return fmt.Errorf("invalid registration acknowledgment: %w", err)
	}
	return nil
}

// ValidateRegisterAckWithAuth verifies protocol negotiation and requires the
// server to echo the exact expected authentication scheme.
func ValidateRegisterAckWithAuth(ack RegisterAckMsg, expectedAuthScheme string) error {
	if err := ValidateRegisterAck(ack); err != nil {
		return err
	}
	if ack.SelectedAuthScheme != expectedAuthScheme {
		return fmt.Errorf("invalid registration acknowledgment: selected auth scheme got %q, require %q", ack.SelectedAuthScheme, expectedAuthScheme)
	}
	return nil
}

// MeshCapabilities returns the capabilities supported by the mesh registration
// transaction without adding them to the ordinary L4 capability list.
func MeshCapabilities() []string {
	return []string{CapabilityMeshSessionV1}
}

// ValidateMeshRegistration verifies the mesh wire version and required
// capability after authentication has bound both fields to the TLS session.
func ValidateMeshRegistration(version string, capabilities []string) error {
	if version != MeshProtocolVersion {
		return fmt.Errorf("incompatible mesh protocol version: got %q, require %q", version, MeshProtocolVersion)
	}
	if err := validateCapabilities(capabilities); err != nil {
		return err
	}
	if !HasCapability(capabilities, CapabilityMeshSessionV1) {
		return fmt.Errorf("required capability %q is missing", CapabilityMeshSessionV1)
	}
	return nil
}

// ValidateMeshRegisterAck verifies exact mesh negotiation and remote identity.
func ValidateMeshRegisterAck(
	ack MeshRegisterAck,
	expectedServerID, expectedRole, expectedAuthScheme string,
	requestedCapabilities []string,
) error {
	if !ack.Success {
		return fmt.Errorf("mesh registration failed: %s", ack.Message)
	}
	if ack.ServerID != expectedServerID {
		return fmt.Errorf("invalid mesh registration acknowledgment: server_id got %q, require %q", ack.ServerID, expectedServerID)
	}
	if ack.Role != expectedRole {
		return fmt.Errorf("invalid mesh registration acknowledgment: role got %q, require %q", ack.Role, expectedRole)
	}
	if err := ValidateMeshRegistration(ack.SelectedVersion, ack.SelectedCapabilities); err != nil {
		return fmt.Errorf("invalid mesh registration acknowledgment: %w", err)
	}
	if ack.SelectedAuthScheme != expectedAuthScheme {
		return fmt.Errorf("invalid mesh registration acknowledgment: selected auth scheme got %q, require %q", ack.SelectedAuthScheme, expectedAuthScheme)
	}
	if ack.State != MeshStateStaged && ack.State != MeshStateAccepted {
		return fmt.Errorf("invalid mesh registration acknowledgment: state %q", ack.State)
	}
	if err := validateCapabilities(requestedCapabilities); err != nil {
		return fmt.Errorf("invalid requested capabilities: %w", err)
	}
	for _, selected := range ack.SelectedCapabilities {
		if !HasCapability(requestedCapabilities, selected) {
			return fmt.Errorf("invalid mesh registration acknowledgment: unrequested capability %q", selected)
		}
	}
	return nil
}

func validateMeshRegisterShape(registration MeshRegister) error {
	if registration.Version == "" {
		return fmt.Errorf("mesh registration version is required")
	}
	if registration.TargetServerID == "" {
		return fmt.Errorf("mesh registration target server_id is required")
	}
	if err := validateCapabilities(registration.Capabilities); err != nil {
		return err
	}
	switch registration.Role {
	case MeshRoleClient:
		if registration.InstanceID == "" || registration.GroupID == "" {
			return fmt.Errorf("mesh client registration requires instance_id and group_id")
		}
		if registration.PeerServerID != "" {
			return fmt.Errorf("mesh client registration must not carry peer server_id")
		}
	case MeshRolePeer:
		if registration.PeerServerID == "" {
			return fmt.Errorf("mesh peer registration requires peer server_id")
		}
		if registration.InstanceID != "" || registration.GroupID != "" {
			return fmt.Errorf("mesh peer registration must not carry client identity")
		}
	default:
		return fmt.Errorf("invalid mesh registration role %q", registration.Role)
	}
	return nil
}

func validateMeshRegisterAckShape(ack MeshRegisterAck) error {
	if err := validateCapabilities(ack.SelectedCapabilities); err != nil {
		return err
	}
	if !ack.Success {
		return nil
	}
	if ack.ServerID == "" || ack.SelectedVersion == "" {
		return fmt.Errorf("successful mesh registration acknowledgment requires server_id and selected version")
	}
	if ack.State != MeshStateStaged && ack.State != MeshStateAccepted {
		return fmt.Errorf("invalid mesh registration acknowledgment state %q", ack.State)
	}
	if ack.Role != MeshRoleClient && ack.Role != MeshRolePeer {
		return fmt.Errorf("invalid mesh registration acknowledgment role %q", ack.Role)
	}
	return nil
}

func validateCapabilities(capabilities []string) error {
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if capability == "" {
			return fmt.Errorf("capability must not be empty")
		}
		if _, exists := seen[capability]; exists {
			return fmt.Errorf("duplicate capability %q", capability)
		}
		seen[capability] = struct{}{}
	}
	return nil
}
