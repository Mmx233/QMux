package protocol

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/Mmx233/QMux/config"
)

func validMeshClientRegistration() MeshRegister {
	return MeshRegister{
		Version:        MeshProtocolVersion,
		Capabilities:   MeshCapabilities(),
		Role:           MeshRoleClient,
		TargetServerID: "edge-a",
		InstanceID:     "instance-a",
		GroupID:        "group-a",
		Auth:           &RegisterAuth{Scheme: "scheme-v1", Proof: []byte{1, 2, 3}},
	}
}

func TestMeshRegistrationStrictRoundTrip(t *testing.T) {
	registration := validMeshClientRegistration()
	var wire bytes.Buffer
	if err := WriteMeshRegister(&wire, registration); err != nil {
		t.Fatalf("WriteMeshRegister: %v", err)
	}
	got, err := ReadMeshRegister(&wire)
	if err != nil {
		t.Fatalf("ReadMeshRegister: %v", err)
	}
	if got.Version != registration.Version || got.Role != registration.Role ||
		got.TargetServerID != registration.TargetServerID || got.InstanceID != registration.InstanceID ||
		got.GroupID != registration.GroupID || got.PeerServerID != "" ||
		got.Auth == nil || got.Auth.Scheme != registration.Auth.Scheme || !bytes.Equal(got.Auth.Proof, registration.Auth.Proof) {
		t.Fatalf("mesh registration round trip = %+v, want %+v", got, registration)
	}

	ack := MeshRegisterAck{
		Success:              true,
		Message:              "registered",
		ServerID:             registration.TargetServerID,
		Role:                 registration.Role,
		SelectedVersion:      MeshProtocolVersion,
		SelectedCapabilities: MeshCapabilities(),
		SelectedAuthScheme:   registration.Auth.Scheme,
	}
	wire.Reset()
	if err := WriteMeshRegisterAck(&wire, ack); err != nil {
		t.Fatalf("WriteMeshRegisterAck: %v", err)
	}
	gotAck, err := ReadMeshRegisterAck(&wire)
	if err != nil {
		t.Fatalf("ReadMeshRegisterAck: %v", err)
	}
	if err := ValidateMeshRegisterAck(gotAck, "edge-a", MeshRoleClient, "scheme-v1", registration.Capabilities); err != nil {
		t.Fatalf("ValidateMeshRegisterAck: %v", err)
	}
}

func TestMeshRegistrationRejectsOtherWireType(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteMessage(&wire, MsgTypeRegister, RegisterMsg{ClientID: "legacy", Version: "1.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMeshRegister(&wire); err == nil || !strings.Contains(err.Error(), "unexpected message type: got 0x01, expected 0x09") {
		t.Fatalf("mesh reader accepted L4 registration type: %v", err)
	}

	if err := WriteMeshRegister(&wire, validMeshClientRegistration()); err != nil {
		t.Fatal(err)
	}
	var legacy RegisterMsg
	if err := ReadTypedMessage(&wire, MsgTypeRegister, &legacy); err == nil || !strings.Contains(err.Error(), "unexpected message type: got 0x09, expected 0x01") {
		t.Fatalf("L4 reader accepted mesh registration type: %v", err)
	}
}

func TestMeshRegistrationStrictDecode(t *testing.T) {
	valid := `{"Version":"1.0","Capabilities":["mesh-session-v1"],"Role":"client","TargetServerID":"edge-a","InstanceID":"instance-a","GroupID":"group-a"}`
	tests := []struct {
		name    string
		payload string
	}{
		{name: "unknown member", payload: strings.TrimSuffix(valid, "}") + `,"Unknown":true}`},
		{name: "duplicate member", payload: strings.TrimSuffix(valid, "}") + `,"Role":"client"}`},
		{name: "duplicate capability", payload: strings.Replace(valid, `["mesh-session-v1"]`, `["mesh-session-v1","mesh-session-v1"]`, 1)},
		{name: "trailing JSON", payload: valid + `{}`},
		{name: "trailing data", payload: valid + `x`},
		{name: "client with peer identity", payload: strings.TrimSuffix(valid, "}") + `,"PeerServerID":"edge-b"}`},
		{name: "peer with client identity", payload: `{"Version":"1.0","Capabilities":["mesh-session-v1"],"Role":"peer","TargetServerID":"edge-a","PeerServerID":"edge-b","InstanceID":"instance-a"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ReadMeshRegister(bytes.NewReader(frame(MsgTypeMeshRegister, []byte(test.payload)))); err == nil {
				t.Fatal("invalid mesh registration was accepted")
			}
		})
	}
}

func TestMeshRegistrationAckStrictDecode(t *testing.T) {
	valid := `{"Success":true,"ServerID":"edge-a","Role":"client","SelectedVersion":"1.0","SelectedCapabilities":["mesh-session-v1"]}`
	tests := []struct {
		name    string
		payload string
	}{
		{name: "unknown member", payload: strings.TrimSuffix(valid, "}") + `,"Unknown":true}`},
		{name: "duplicate member", payload: strings.TrimSuffix(valid, "}") + `,"Role":"client"}`},
		{name: "duplicate capability", payload: strings.Replace(valid, `["mesh-session-v1"]`, `["mesh-session-v1","mesh-session-v1"]`, 1)},
		{name: "trailing JSON", payload: valid + `{}`},
		{name: "trailing data", payload: valid + `x`},
		{name: "missing server identity", payload: strings.Replace(valid, `"ServerID":"edge-a",`, "", 1)},
		{name: "invalid role", payload: strings.Replace(valid, `"Role":"client"`, `"Role":"unknown"`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ReadMeshRegisterAck(bytes.NewReader(frame(MsgTypeMeshRegisterAck, []byte(test.payload)))); err == nil {
				t.Fatal("invalid mesh registration acknowledgment was accepted")
			}
		})
	}
}

func TestMeshRegistrationFixedPayloadLimit(t *testing.T) {
	header := make([]byte, 5)
	header[0] = MsgTypeMeshRegister
	binary.BigEndian.PutUint32(header[1:], MaxRegistrationPayloadSize+1)
	if _, err := ReadMeshRegister(bytes.NewReader(header)); err == nil || !strings.Contains(err.Error(), "payload too large") {
		t.Fatalf("ReadMeshRegister oversized error = %v", err)
	}

	registration := validMeshClientRegistration()
	registration.GroupID = strings.Repeat("x", MaxRegistrationPayloadSize)
	if err := WriteMeshRegister(new(bytes.Buffer), registration); err == nil || !strings.Contains(err.Error(), "payload too large") {
		t.Fatalf("WriteMeshRegister oversized error = %v", err)
	}

	ackHeader := make([]byte, 5)
	ackHeader[0] = MsgTypeMeshRegisterAck
	binary.BigEndian.PutUint32(ackHeader[1:], MaxRegistrationPayloadSize+1)
	if _, err := ReadMeshRegisterAck(bytes.NewReader(ackHeader)); err == nil || !strings.Contains(err.Error(), "payload too large") {
		t.Fatalf("ReadMeshRegisterAck oversized error = %v", err)
	}
	if err := WriteMeshRegisterAck(new(bytes.Buffer), MeshRegisterAck{
		Success: false,
		Message: strings.Repeat("x", MaxRegistrationPayloadSize),
	}); err == nil || !strings.Contains(err.Error(), "payload too large") {
		t.Fatalf("WriteMeshRegisterAck oversized error = %v", err)
	}
}

func TestValidateMeshRegisterAckExactNegotiation(t *testing.T) {
	valid := MeshRegisterAck{
		Success:              true,
		ServerID:             "edge-a",
		Role:                 MeshRolePeer,
		SelectedVersion:      MeshProtocolVersion,
		SelectedCapabilities: MeshCapabilities(),
		SelectedAuthScheme:   "scheme-v1",
	}
	requested := append(MeshCapabilities(), "future-capability")
	if err := ValidateMeshRegisterAck(valid, "edge-a", MeshRolePeer, "scheme-v1", requested); err != nil {
		t.Fatalf("valid acknowledgment rejected: %v", err)
	}

	mutations := map[string]MeshRegisterAck{
		"failure":            {Success: false, Message: "rejected"},
		"server identity":    func() MeshRegisterAck { v := valid; v.ServerID = "edge-b"; return v }(),
		"role":               func() MeshRegisterAck { v := valid; v.Role = MeshRoleClient; return v }(),
		"version":            func() MeshRegisterAck { v := valid; v.SelectedVersion = "2.0"; return v }(),
		"missing capability": func() MeshRegisterAck { v := valid; v.SelectedCapabilities = nil; return v }(),
		"duplicate capability": func() MeshRegisterAck {
			v := valid
			v.SelectedCapabilities = []string{CapabilityMeshSessionV1, CapabilityMeshSessionV1}
			return v
		}(),
		"unrequested capability": func() MeshRegisterAck {
			v := valid
			v.SelectedCapabilities = append(v.SelectedCapabilities, "other")
			return v
		}(),
		"scheme": func() MeshRegisterAck { v := valid; v.SelectedAuthScheme = "scheme-v2"; return v }(),
	}
	for name, mutated := range mutations {
		t.Run(name, func(t *testing.T) {
			if err := ValidateMeshRegisterAck(mutated, "edge-a", MeshRolePeer, "scheme-v1", requested); err == nil {
				t.Fatal("mutated acknowledgment was accepted")
			}
		})
	}
}

func TestMeshCapabilityDoesNotEnterL4Defaults(t *testing.T) {
	if HasCapability(config.DefaultCapabilities, CapabilityMeshSessionV1) {
		t.Fatalf("ordinary L4 default capabilities %v contain mesh capability %q", config.DefaultCapabilities, CapabilityMeshSessionV1)
	}
}
