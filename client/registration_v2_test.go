package client

import (
	"crypto/tls"
	"testing"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/rs/zerolog"
)

func TestAcceptRegisterAckOnlyValidatesNegotiation(t *testing.T) {
	newConnection := func() *ServerConnection {
		return NewServerConnection(
			"server.example.com:8443",
			"server.example.com",
			tls.NewLRUClientSessionCache(1),
			zerolog.Nop(),
		)
	}

	t.Run("legacy ack remains unhealthy", func(t *testing.T) {
		connection := newConnection()
		err := connection.acceptRegisterAck(protocol.RegisterAckMsg{
			Success:              true,
			ServerVersion:        "1.0",
			SelectedCapabilities: []string{protocol.CapabilityUDPWireV2},
		})
		if err == nil {
			t.Fatal("legacy acknowledgment was accepted")
		}
		if connection.IsHealthy() {
			t.Fatal("connection became healthy after incompatible acknowledgment")
		}
	})

	t.Run("missing capability remains unhealthy", func(t *testing.T) {
		connection := newConnection()
		err := connection.acceptRegisterAck(protocol.RegisterAckMsg{
			Success:              true,
			ServerVersion:        protocol.ProtocolVersion,
			SelectedCapabilities: []string{"tcp", "udp"},
		})
		if err == nil {
			t.Fatal("acknowledgment without UDP wire v2 was accepted")
		}
		if connection.IsHealthy() {
			t.Fatal("connection became healthy without negotiated UDP wire v2")
		}
	})

	t.Run("v2 acknowledgment remains provisional", func(t *testing.T) {
		connection := newConnection()
		err := connection.acceptRegisterAck(protocol.RegisterAckMsg{
			Success:              true,
			ServerVersion:        protocol.ProtocolVersion,
			SelectedCapabilities: []string{"tcp", "udp", protocol.CapabilityUDPWireV2},
		})
		if err != nil {
			t.Fatal(err)
		}
		if connection.IsHealthy() {
			t.Fatal("acknowledgment validation published provisional connection")
		}
	})
}

func TestAcceptRegisterAckRequiresExactTokenScheme(t *testing.T) {
	newConnection := func() *ServerConnection {
		return NewServerConnection(
			"server.example.com:8443",
			"server.example.com",
			tls.NewLRUClientSessionCache(1),
			zerolog.Nop(),
		)
	}
	compatibleAck := func(selectedAuthScheme string) protocol.RegisterAckMsg {
		return protocol.RegisterAckMsg{
			Success:              true,
			ServerVersion:        protocol.ProtocolVersion,
			SelectedCapabilities: config.DefaultCapabilities,
			SelectedAuthScheme:   selectedAuthScheme,
		}
	}

	for _, test := range []struct {
		name   string
		scheme string
		valid  bool
	}{
		{name: "missing scheme", scheme: ""},
		{name: "different scheme", scheme: sharedtoken.Scheme + "-other"},
		{name: "exact scheme", scheme: sharedtoken.Scheme, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := newConnection()
			err := connection.acceptRegisterAckWithAuth(compatibleAck(test.scheme), sharedtoken.Scheme)
			if test.valid && err != nil {
				t.Fatalf("exact token scheme was rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatalf("token acknowledgment scheme %q was accepted", test.scheme)
			}
			if connection.IsHealthy() {
				t.Fatal("acknowledgment validation published provisional connection")
			}
		})
	}
}
