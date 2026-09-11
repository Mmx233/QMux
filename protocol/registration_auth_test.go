package protocol

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestAuthRegistrationRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	want := &RegisterAuth{Scheme: "scheme-v1", Proof: []byte{0, 1, 2, 3}}
	if err := WriteRegisterWithAuth(&wire, "client-a", "2.0", []string{"tcp"}, want); err != nil {
		t.Fatalf("WriteRegisterWithAuth: %v", err)
	}

	var got RegisterMsg
	if err := ReadTypedMessageLimited(&wire, MsgTypeRegister, &got, MaxRegistrationPayloadSize); err != nil {
		t.Fatalf("ReadTypedMessageLimited: %v", err)
	}
	if got.Auth == nil || got.Auth.Scheme != want.Scheme || !bytes.Equal(got.Auth.Proof, want.Proof) {
		t.Fatalf("decoded auth = %+v, want %+v", got.Auth, want)
	}
}

func TestValidateRegisterAckWithAuthRequiresExactEcho(t *testing.T) {
	valid := RegisterAckMsg{
		Success:              true,
		ServerVersion:        ProtocolVersion,
		SelectedCapabilities: []string{CapabilityUDPWireV2},
		SelectedAuthScheme:   "scheme-v1",
	}
	if err := ValidateRegisterAckWithAuth(valid, "scheme-v1"); err != nil {
		t.Fatalf("ValidateRegisterAckWithAuth: %v", err)
	}

	for _, selected := range []string{"", "SCHEME-V1", "scheme-v2"} {
		ack := valid
		ack.SelectedAuthScheme = selected
		if err := ValidateRegisterAckWithAuth(ack, "scheme-v1"); err == nil {
			t.Fatalf("accepted selected auth scheme %q", selected)
		}
	}
	if err := ValidateRegisterAck(valid); err != nil {
		t.Fatalf("legacy ValidateRegisterAck rejected additive auth field: %v", err)
	}
}

func TestProtocolPayloadLimits(t *testing.T) {
	tests := []struct {
		name    string
		msgType byte
		limit   uint32
	}{
		{"registration", MsgTypeRegister, MaxRegistrationPayloadSize},
		{"NewConnAck", MsgTypeNewConnAck, MaxNewConnAckPayloadSize},
		{"NewConn", MsgTypeNewConn, MaxNewConnPayloadSize},
		{"control", MsgTypeHeartbeat, MaxControlPayloadSize},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{'x'}, int(test.limit))
			msgType, got, err := ReadMessageLimited(bytes.NewReader(frame(test.msgType, payload)), test.limit)
			if err != nil {
				t.Fatalf("read maximum payload: %v", err)
			}
			if msgType != test.msgType || !bytes.Equal(got, payload) {
				t.Fatal("maximum payload did not round trip")
			}

			tooLargeHeader := make([]byte, 5)
			tooLargeHeader[0] = test.msgType
			binary.BigEndian.PutUint32(tooLargeHeader[1:], test.limit+1)
			if _, _, err := ReadMessageLimited(bytes.NewReader(tooLargeHeader), test.limit); err == nil || !strings.Contains(err.Error(), "payload too large") {
				t.Fatalf("oversized payload error = %v", err)
			}

			truncated := frame(test.msgType, payload)
			truncated = truncated[:len(truncated)-1]
			if _, _, err := ReadMessageLimited(bytes.NewReader(truncated), test.limit); err == nil || !strings.Contains(err.Error(), "read payload") {
				t.Fatalf("truncated payload error = %v", err)
			}
		})
	}

	genericPayload := bytes.Repeat([]byte{'x'}, MaxRegistrationPayloadSize+1)
	if _, _, err := ReadMessage(bytes.NewReader(frame(MsgTypeRegister, genericPayload))); err != nil {
		t.Fatalf("generic reader unexpectedly inherited semantic limit: %v", err)
	}
}

func frame(msgType byte, payload []byte) []byte {
	wire := make([]byte, 5+len(payload))
	wire[0] = msgType
	binary.BigEndian.PutUint32(wire[1:5], uint32(len(payload)))
	copy(wire[5:], payload)
	return wire
}
