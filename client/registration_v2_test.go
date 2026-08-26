package client

import (
	"crypto/tls"
	"testing"

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
