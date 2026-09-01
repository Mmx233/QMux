package protocol

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestAdditiveAuthFieldsRemainDecoderCompatible(t *testing.T) {
	t.Run("register", func(t *testing.T) {
		var wire bytes.Buffer
		auth := &RegisterAuth{Scheme: "scheme-v1", Proof: []byte{1, 2, 3}}
		if err := WriteRegisterWithAuth(&wire, "client-a", "2.0", []string{"tcp"}, auth); err != nil {
			t.Fatalf("WriteRegisterWithAuth: %v", err)
		}

		var old struct {
			ClientID     string
			Version      string
			Capabilities []string
		}
		if err := ReadTypedMessage(&wire, MsgTypeRegister, &old); err != nil {
			t.Fatalf("legacy decode: %v", err)
		}
		if old.ClientID != "client-a" || old.Version != "2.0" || len(old.Capabilities) != 1 || old.Capabilities[0] != "tcp" {
			t.Fatalf("legacy registration = %+v", old)
		}
	})

	t.Run("ack", func(t *testing.T) {
		var wire bytes.Buffer
		if err := WriteRegisterAckWithAuth(&wire, true, "registered", "2.0", []string{CapabilityUDPWireV2}, "scheme-v1"); err != nil {
			t.Fatalf("WriteRegisterAckWithAuth: %v", err)
		}

		var old struct {
			Success              bool
			Message              string
			ServerVersion        string
			SelectedCapabilities []string
		}
		if err := ReadTypedMessage(&wire, MsgTypeRegisterAck, &old); err != nil {
			t.Fatalf("legacy decode: %v", err)
		}
		if !old.Success || old.ServerVersion != "2.0" || len(old.SelectedCapabilities) != 1 {
			t.Fatalf("legacy acknowledgment = %+v", old)
		}
	})
}

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

func TestRegistrationPayloadLimit(t *testing.T) {
	tooLargeHeader := make([]byte, 5)
	tooLargeHeader[0] = MsgTypeRegister
	binary.BigEndian.PutUint32(tooLargeHeader[1:], MaxRegistrationPayloadSize+1)
	if _, _, err := ReadMessageLimited(bytes.NewReader(tooLargeHeader), MaxRegistrationPayloadSize); err == nil || !strings.Contains(err.Error(), "payload too large") {
		t.Fatalf("oversized registration error = %v", err)
	}

	payload := bytes.Repeat([]byte{'x'}, MaxRegistrationPayloadSize)
	msgType, got, err := ReadMessageLimited(bytes.NewReader(frame(MsgTypeRegister, payload)), MaxRegistrationPayloadSize)
	if err != nil {
		t.Fatalf("read maximum registration payload: %v", err)
	}
	if msgType != MsgTypeRegister || !bytes.Equal(got, payload) {
		t.Fatal("maximum registration payload did not round trip")
	}

	genericPayload := bytes.Repeat([]byte{'x'}, MaxRegistrationPayloadSize+1)
	if _, _, err := ReadMessage(bytes.NewReader(frame(MsgTypeRegister, genericPayload))); err != nil {
		t.Fatalf("generic reader unexpectedly inherited registration limit: %v", err)
	}
}

func frame(msgType byte, payload []byte) []byte {
	wire := make([]byte, 5+len(payload))
	wire[0] = msgType
	binary.BigEndian.PutUint32(wire[1:5], uint32(len(payload)))
	copy(wire[5:], payload)
	return wire
}
