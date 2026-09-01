package client

import (
	"crypto/tls"
	"testing"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/rs/zerolog"
)

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
