package protocol

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Mmx233/QMux/config"
)

func TestValidateRegistrationCompatibilityMatrix(t *testing.T) {
	tests := []struct {
		name         string
		version      string
		capabilities []string
		wantErr      bool
	}{
		{name: "v2 with capability", version: "2.0", capabilities: []string{"tcp", "udp", CapabilityUDPWireV2}},
		{name: "legacy version", version: "1.0", capabilities: []string{"tcp", "udp", CapabilityUDPWireV2}, wantErr: true},
		{name: "future version", version: "3.0", capabilities: []string{CapabilityUDPWireV2}, wantErr: true},
		{name: "missing version", capabilities: []string{CapabilityUDPWireV2}, wantErr: true},
		{name: "missing capability", version: "2.0", capabilities: []string{"tcp", "udp"}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRegistration(test.version, test.capabilities)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestValidateRegisterAckCompatibilityMatrix(t *testing.T) {
	valid := RegisterAckMsg{
		Success:              true,
		ServerVersion:        ProtocolVersion,
		SelectedCapabilities: []string{"tcp", "udp", CapabilityUDPWireV2},
	}
	if err := ValidateRegisterAck(valid); err != nil {
		t.Fatalf("valid ack rejected: %v", err)
	}

	tests := []RegisterAckMsg{
		{Success: false, Message: "rejected", ServerVersion: ProtocolVersion},
		{Success: true, ServerVersion: "1.0", SelectedCapabilities: []string{CapabilityUDPWireV2}},
		{Success: true, ServerVersion: ProtocolVersion, SelectedCapabilities: []string{"tcp", "udp"}},
		{Success: true}, // The shape returned by a legacy server.
	}
	for i, ack := range tests {
		if err := ValidateRegisterAck(ack); err == nil {
			t.Fatalf("incompatible ack %d was accepted: %+v", i, ack)
		}
	}
}

func TestWriteRegisterAckIncludesNegotiation(t *testing.T) {
	var wire bytes.Buffer
	wantCaps := []string{"tcp", CapabilityUDPWireV2}
	if err := WriteRegisterAckWithAuth(&wire, true, "registered", ProtocolVersion, wantCaps, ""); err != nil {
		t.Fatal(err)
	}

	var ack RegisterAckMsg
	if err := ReadTypedMessage(&wire, MsgTypeRegisterAck, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.ServerVersion != ProtocolVersion || strings.Join(ack.SelectedCapabilities, ",") != strings.Join(wantCaps, ",") {
		t.Fatalf("negotiation fields did not round trip: %+v", ack)
	}
}

func TestSelectCapabilities(t *testing.T) {
	selected := SelectCapabilities(
		[]string{"udp", "unsupported", CapabilityUDPWireV2, "udp"},
		[]string{"tcp", "udp", CapabilityUDPWireV2},
	)
	want := []string{"udp", CapabilityUDPWireV2}
	if strings.Join(selected, ",") != strings.Join(want, ",") {
		t.Fatalf("selected=%v, want %v", selected, want)
	}
}

func TestDefaultCapabilitiesAdvertiseUDPWireV2(t *testing.T) {
	if !HasCapability(config.DefaultCapabilities, CapabilityUDPWireV2) {
		t.Fatalf("default capabilities %v do not advertise %q", config.DefaultCapabilities, CapabilityUDPWireV2)
	}
}
