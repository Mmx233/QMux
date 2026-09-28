package protocol

import (
	"encoding/binary"
	"encoding/json/v2"
	"fmt"
	"io"
)

const MaxMeshChunkDataSize = 2 * 1024

type MeshBegin struct {
	Revision   uint64
	Groups     uint32
	Paths      uint32
	GroupBytes uint64
}

type MeshChunk struct {
	Sequence uint64
	Record   uint32
	Total    uint32
	Offset   uint32
	Digest   []byte `json:",omitempty"`
	Data     []byte
}

type MeshPath struct {
	Sequence         uint64
	PathID           string
	GroupID          string
	RuleVersion      uint64
	TerminalServerID string
	Servers          []string
	Generation       uint64
}

type MeshWithdraw struct {
	Sequence uint64
	PathID   string
}

type MeshEnd struct {
	Groups        uint32
	Paths         uint32
	GroupBytes    uint64
	FinalSequence uint64
}

type MeshReady struct {
	State string
}

// MarshalMeshControlFrame enforces the mesh-only 4 KiB payload limit before
// returning a complete frame for a bounded writer queue.
func MarshalMeshControlFrame(message any) ([]byte, error) {
	var kind byte
	switch value := message.(type) {
	case MeshBegin:
		kind = MsgTypeMeshBegin
	case MeshChunk:
		if len(value.Data) == 0 || len(value.Data) > MaxMeshChunkDataSize || value.Total == 0 ||
			value.Offset >= value.Total || uint64(value.Offset)+uint64(len(value.Data)) > uint64(value.Total) ||
			value.Offset == 0 && len(value.Digest) != 32 || value.Offset != 0 && len(value.Digest) != 0 {
			return nil, fmt.Errorf("invalid mesh declaration chunk")
		}
		kind = MsgTypeMeshChunk
	case MeshPath:
		if value.PathID == "" || value.GroupID == "" || value.RuleVersion == 0 || value.TerminalServerID == "" || len(value.Servers) == 0 {
			return nil, fmt.Errorf("invalid mesh path record")
		}
		kind = MsgTypeMeshPath
	case MeshWithdraw:
		if value.PathID == "" {
			return nil, fmt.Errorf("invalid mesh path withdrawal")
		}
		kind = MsgTypeMeshWithdraw
	case MeshEnd:
		kind = MsgTypeMeshEnd
	case MeshReady:
		if value.State != MeshStateStaged && value.State != MeshStateAccepted {
			return nil, fmt.Errorf("invalid mesh ready state %q", value.State)
		}
		kind = MsgTypeMeshReady
	default:
		return nil, fmt.Errorf("unsupported mesh control message %T", message)
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("marshal mesh control: %w", err)
	}
	if len(payload) > MaxControlPayloadSize {
		return nil, fmt.Errorf("mesh control payload too large: %d bytes", len(payload))
	}
	frame := make([]byte, 5+len(payload))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame, nil
}

func WriteMeshControl(w io.Writer, message any) error {
	frame, err := MarshalMeshControlFrame(message)
	if err != nil {
		return err
	}
	n, err := w.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	return nil
}

func ReadMeshControl(r io.Reader) (any, error) {
	kind, payload, err := ReadMessageLimited(r, MaxControlPayloadSize)
	if err != nil {
		return nil, err
	}
	return DecodeMeshControl(kind, payload)
}

func DecodeMeshControl(kind byte, payload []byte) (any, error) {
	var message any
	switch kind {
	case MsgTypeMeshBegin:
		message = new(MeshBegin)
	case MsgTypeMeshChunk:
		message = new(MeshChunk)
	case MsgTypeMeshPath:
		message = new(MeshPath)
	case MsgTypeMeshWithdraw:
		message = new(MeshWithdraw)
	case MsgTypeMeshEnd:
		message = new(MeshEnd)
	case MsgTypeMeshReady:
		message = new(MeshReady)
	default:
		return nil, fmt.Errorf("unexpected mesh control message 0x%02x", kind)
	}
	if err := json.Unmarshal(payload, message, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("decode mesh control: %w", err)
	}
	if _, err := MarshalMeshControlFrame(derefMeshControl(message)); err != nil {
		return nil, err
	}
	return derefMeshControl(message), nil
}

func derefMeshControl(message any) any {
	switch value := message.(type) {
	case *MeshBegin:
		return *value
	case *MeshChunk:
		return *value
	case *MeshPath:
		return *value
	case *MeshWithdraw:
		return *value
	case *MeshEnd:
		return *value
	case *MeshReady:
		return *value
	default:
		return message
	}
}
