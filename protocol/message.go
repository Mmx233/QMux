package protocol

import (
	"fmt"
	"slices"
)

// Message types
const (
	MsgTypeRegister    = 0x01 // Client registration
	MsgTypeRegisterAck = 0x02 // Server acknowledgment
	MsgTypeHeartbeat   = 0x03 // Keepalive
	MsgTypeNewConn     = 0x04 // New connection metadata
	MsgTypeConnClose   = 0x06 // Connection closed
	MsgTypeError       = 0xFF // Error message
)

// Note: MsgTypeConnData (0x05) is not used in current design.
// Data flows directly on QUIC streams for better performance.
// Each connection gets its own stream, no need for message framing.

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

// HeartbeatMsg is sent periodically to keep connection alive
type HeartbeatMsg struct {
	Timestamp int64 // Unix timestamp
}

// NewConnMsg is sent by server to client when new connection arrives
type NewConnMsg struct {
	ConnID     uint64 // Unique connection ID
	Protocol   string // "tcp" or "udp"
	SourceAddr string // Original client address (IP:port)
	DestAddr   string // Target address on traffic listener (IP:port)
	Timestamp  int64  // Connection timestamp
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
//
//goland:noinspection GoNameStartsWithPackageName
const ProtocolVersion = "2.0"

const CapabilityUDPWireV2 = "udp-wire-v2"

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
