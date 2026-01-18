package protocol

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
	ClientID     string   // Unique client identifier
	Version      string   // Protocol version
	Capabilities []string // Supported features (e.g., "tcp", "udp")
}

// RegisterAckMsg is sent by server to acknowledge registration
type RegisterAckMsg struct {
	Success bool   // Registration success
	Message string // Optional message
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
	Payload interface{}
}

const ProtocolVersion = "1.0"
