package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"

	"github.com/quic-go/quic-go/quicvarint"
)

const (
	// MaxPayloadSize is the generic protocol message payload limit.
	MaxPayloadSize = 10 * 1024 * 1024
	// MaxRegistrationPayloadSize is the stricter limit for registration traffic.
	MaxRegistrationPayloadSize = 4 * 1024
)

// Wire format: [1 byte type][4 bytes length][payload]

// WriteMessage writes a message to the writer using buffer pooling to reduce allocations.
func WriteMessage(w io.Writer, msgType byte, payload any) error {
	// Get buffer from pool
	buf := GetBuffer()
	defer PutBuffer(buf)

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	// Write header (type + length) directly without binary.Write
	header := [5]byte{msgType}
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))

	// Write header and payload to pooled buffer first, then flush to writer
	buf.Write(header[:])
	buf.Write(data)

	// Single write to the underlying writer
	n, err := w.Write(buf.Bytes())
	if err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	if n != buf.Len() {
		return fmt.Errorf("write message: %w", io.ErrShortWrite)
	}

	return nil
}

// ReadMessage reads a message from the reader with optimized allocations.
// Uses a fixed header buffer to avoid allocations for header reading.
func ReadMessage(r io.Reader) (msgType byte, payload []byte, err error) {
	return ReadMessageLimited(r, MaxPayloadSize)
}

// ReadMessageLimited reads a message while enforcing maxPayloadSize before
// allocating the payload buffer.
func ReadMessageLimited(r io.Reader, maxPayloadSize uint32) (msgType byte, payload []byte, err error) {
	// Use a fixed-size header buffer to avoid allocations
	var header [5]byte

	// Read header (type + length) in one call
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, fmt.Errorf("read header: %w", err)
	}

	// Extract message type and length
	msgType = header[0]
	length := binary.BigEndian.Uint32(header[1:])

	// Validate length (prevent excessive memory allocation)
	if length > maxPayloadSize {
		return 0, nil, fmt.Errorf("payload too large: %d bytes (maximum %d)", length, maxPayloadSize)
	}

	// Read payload - allocation is necessary here as we return the slice
	payload = make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("read payload: %w", err)
	}

	return msgType, payload, nil
}

// DecodeMessage decodes a payload into a message structure
func DecodeMessage(payload []byte, msg any) error {
	if err := json.Unmarshal(payload, msg); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}
	return nil
}

// DecodeDrainRequest strictly decodes an empty JSON object.
func DecodeDrainRequest(payload []byte) error {
	decoder := jsontext.NewDecoder(bytes.NewReader(payload))
	if err := drainObjectStart(decoder); err != nil {
		return fmt.Errorf("decode drain request: %w", err)
	}
	if err := drainObjectEnd(decoder); err != nil {
		return fmt.Errorf("decode drain request: %w", err)
	}
	return nil
}

// DecodeDrainComplete strictly decodes exactly one integral AcceptFence member.
func DecodeDrainComplete(payload []byte) (DrainCompleteMsg, error) {
	decoder := jsontext.NewDecoder(bytes.NewReader(payload))
	if err := drainObjectStart(decoder); err != nil {
		return DrainCompleteMsg{}, fmt.Errorf("decode drain complete: %w", err)
	}
	name, err := decoder.ReadToken()
	if err != nil {
		return DrainCompleteMsg{}, fmt.Errorf("decode drain complete: read member: %w", err)
	}
	if name.Kind() == jsontext.KindEndObject {
		return DrainCompleteMsg{}, fmt.Errorf("decode drain complete: missing AcceptFence")
	}
	if name.Kind() != jsontext.KindString || name.String() != "AcceptFence" {
		return DrainCompleteMsg{}, fmt.Errorf("decode drain complete: unknown member %q", name.String())
	}
	value, err := decoder.ReadToken()
	if err != nil {
		return DrainCompleteMsg{}, fmt.Errorf("decode drain complete: read AcceptFence: %w", err)
	}
	if value.Kind() != jsontext.KindNumber {
		return DrainCompleteMsg{}, fmt.Errorf("decode drain complete: AcceptFence must be an integer")
	}
	acceptFence, err := value.Int()
	if err != nil {
		return DrainCompleteMsg{}, fmt.Errorf("decode drain complete: invalid AcceptFence: %w", err)
	}
	if err := drainObjectEnd(decoder); err != nil {
		return DrainCompleteMsg{}, fmt.Errorf("decode drain complete: %w", err)
	}
	if err := ValidateDrainFence(acceptFence); err != nil {
		return DrainCompleteMsg{}, err
	}
	return DrainCompleteMsg{AcceptFence: acceptFence}, nil
}

func drainObjectStart(decoder *jsontext.Decoder) error {
	token, err := decoder.ReadToken()
	if err != nil {
		return err
	}
	if token.Kind() != jsontext.KindBeginObject {
		return fmt.Errorf("payload must be an object")
	}
	return nil
}

func drainObjectEnd(decoder *jsontext.Decoder) error {
	token, err := decoder.ReadToken()
	if err != nil {
		return err
	}
	if token.Kind() != jsontext.KindEndObject {
		return fmt.Errorf("object is not closed")
	}
	if _, err := decoder.ReadToken(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON token")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

// ValidateDrainFence validates a server-initiated bidirectional QUIC stream ID.
func ValidateDrainFence(fence int64) error {
	if fence == -1 || fence >= 1 && fence <= int64(quicvarint.Max) && fence%4 == 1 {
		return nil
	}
	return fmt.Errorf("invalid drain accept fence %d", fence)
}

// ReadTypedMessage reads and decodes a message in one call
func ReadTypedMessage(r io.Reader, expectedType byte, msg any) error {
	return ReadTypedMessageLimited(r, expectedType, msg, MaxPayloadSize)
}

// ReadTypedMessageLimited reads and decodes a message while enforcing a
// caller-provided payload limit.
func ReadTypedMessageLimited(r io.Reader, expectedType byte, msg any, maxPayloadSize uint32) error {
	msgType, payload, err := ReadMessageLimited(r, maxPayloadSize)
	if err != nil {
		return err
	}

	if msgType != expectedType {
		return fmt.Errorf("unexpected message type: got 0x%02x, expected 0x%02x", msgType, expectedType)
	}

	return DecodeMessage(payload, msg)
}

// WriteRegister writes a registration message
func WriteRegister(w io.Writer, clientID, version string, capabilities []string) error {
	return WriteRegisterWithAuth(w, clientID, version, capabilities, nil)
}

// WriteRegisterWithAuth writes a registration message with an optional
// authentication proof.
func WriteRegisterWithAuth(w io.Writer, clientID, version string, capabilities []string, auth *RegisterAuth) error {
	msg := RegisterMsg{
		ClientID:     clientID,
		Version:      version,
		Capabilities: capabilities,
		Auth:         auth,
	}
	return WriteMessage(w, MsgTypeRegister, msg)
}

// WriteRegisterAck writes a registration acknowledgment
func WriteRegisterAck(w io.Writer, success bool, message, serverVersion string, selectedCapabilities []string) error {
	return WriteRegisterAckWithAuth(w, success, message, serverVersion, selectedCapabilities, "")
}

// WriteRegisterAckWithAuth writes a registration acknowledgment that echoes
// the server-selected authentication scheme.
func WriteRegisterAckWithAuth(w io.Writer, success bool, message, serverVersion string, selectedCapabilities []string, selectedAuthScheme string) error {
	msg := RegisterAckMsg{
		Success:              success,
		Message:              message,
		ServerVersion:        serverVersion,
		SelectedCapabilities: selectedCapabilities,
		SelectedAuthScheme:   selectedAuthScheme,
	}
	return WriteMessage(w, MsgTypeRegisterAck, msg)
}

// WriteHeartbeat writes a heartbeat message
func WriteHeartbeat(w io.Writer, timestamp int64) error {
	msg := HeartbeatMsg{
		Timestamp: timestamp,
	}
	return WriteMessage(w, MsgTypeHeartbeat, msg)
}

// WriteDrainRequest writes an empty drain request.
func WriteDrainRequest(w io.Writer) error {
	return WriteMessage(w, MsgTypeDrainRequest, DrainRequestMsg{})
}

// WriteDrainComplete writes the final server stream fence.
func WriteDrainComplete(w io.Writer, acceptFence int64) error {
	if err := ValidateDrainFence(acceptFence); err != nil {
		return err
	}
	return WriteMessage(w, MsgTypeDrainComplete, DrainCompleteMsg{AcceptFence: acceptFence})
}

// WriteNewConn writes a new connection message
func WriteNewConn(w io.Writer, connID uint64, protocol, sourceAddr, destAddr string, timestamp int64) error {
	msg := NewConnMsg{
		ConnID:     connID,
		Protocol:   protocol,
		SourceAddr: sourceAddr,
		DestAddr:   destAddr,
		Timestamp:  timestamp,
	}
	return WriteMessage(w, MsgTypeNewConn, msg)
}

// WriteConnClose writes a connection close message
func WriteConnClose(w io.Writer, connID uint64, reason string) error {
	msg := ConnCloseMsg{
		ConnID: connID,
		Reason: reason,
	}
	return WriteMessage(w, MsgTypeConnClose, msg)
}

// WriteError writes an error message
func WriteError(w io.Writer, code uint32, message string) error {
	msg := ErrorMsg{
		Code:    code,
		Message: message,
	}
	return WriteMessage(w, MsgTypeError, msg)
}
