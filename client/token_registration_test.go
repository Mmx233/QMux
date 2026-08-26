package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"testing"
	"time"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const tokenRegistrationTestSecret = "0123456789abcdef0123456789abcdef"

type observedSessionCache struct {
	tls.ClientSessionCache
	put chan struct{}
}

func newObservedSessionCache() *observedSessionCache {
	return &observedSessionCache{
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
		put:                make(chan struct{}, 4),
	}
}

func (c *observedSessionCache) Put(key string, state *tls.ClientSessionState) {
	c.ClientSessionCache.Put(key, state)
	if state != nil {
		select {
		case c.put <- struct{}{}:
		default:
		}
	}
}

func newTokenRegistrationPeer(t *testing.T) *lifecyclePeer {
	t.Helper()
	serverTLS, clientTLS := lifecycleTLSConfigs(t)
	serverTLS.ClientAuth = tls.NoClientCert
	serverTLS.ClientCAs = nil
	serverTLS.NextProtos = []string{lifecycleTestALPN}
	clientTLS.Certificates = nil
	clientTLS.NextProtos = []string{lifecycleTestALPN}
	return newLifecyclePeerWithTLS(t, serverTLS, clientTLS)
}

func tokenTestAuth() config.ClientAuth {
	return config.ClientAuth{
		Method: config.ClientAuthMethodToken,
		Token:  tokenRegistrationTestSecret,
	}
}

func verifyTokenRegistration(conn *quic.Conn, registration protocol.RegisterMsg) error {
	if registration.Auth == nil {
		return errors.New("registration omitted token authentication")
	}
	if registration.Auth.Scheme != sharedtoken.Scheme {
		return errors.New("registration used an unexpected token scheme")
	}
	return sharedtoken.Verify(
		[]byte(tokenRegistrationTestSecret),
		sharedtoken.Transcript{
			ClientID:     registration.ClientID,
			Version:      registration.Version,
			Capabilities: registration.Capabilities,
		},
		registration.Auth.Proof,
		conn.ConnectionState().TLS,
	)
}

func TestTokenRegistrationRequiresExactSchemeEcho(t *testing.T) {
	peer := newTokenRegistrationPeer(t)
	cache := tls.NewLRUClientSessionCache(1)
	sc := NewServerConnection(peer.endpoint().Address, "lifecycle.test", cache, zerolog.Nop())

	serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, registration protocol.RegisterMsg) error {
		if len(conn.ConnectionState().TLS.PeerCertificates) != 0 {
			return errors.New("token client unexpectedly sent a client certificate")
		}
		if err := verifyTokenRegistration(conn, registration); err != nil {
			return err
		}
		return protocol.WriteRegisterAck(
			stream,
			true,
			"registered without a scheme echo",
			protocol.ProtocolVersion,
			config.DefaultCapabilities,
		)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sc.Connect(ctx, peer.clientTLS, config.Quic{}.GetConfig()); err != nil {
		t.Fatalf("connect token client: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	err := sc.RegisterWithAuth(ctx, "token-downgrade-client", tokenTestAuth())
	if err == nil {
		t.Fatal("token registration accepted an acknowledgment without a scheme echo")
	}
	if sc.controlStream != nil {
		t.Fatal("failed token registration committed the control stream")
	}
	if err := awaitLifecycle(t, serverDone, "downgrade peer result"); err != nil {
		t.Fatal(err)
	}
}

func TestTokenProofIsConnectionBoundAcrossResumption(t *testing.T) {
	peer := newTokenRegistrationPeer(t)
	cache := newObservedSessionCache()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var firstRegistration protocol.RegisterMsg
	firstServerDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, registration protocol.RegisterMsg) error {
		firstRegistration = registration
		if err := verifyTokenRegistration(conn, registration); err != nil {
			return err
		}
		return protocol.WriteRegisterAckWithAuth(
			stream,
			true,
			"registered",
			protocol.ProtocolVersion,
			config.DefaultCapabilities,
			sharedtoken.Scheme,
		)
	})
	first := NewServerConnection(peer.endpoint().Address, "lifecycle.test", cache, zerolog.Nop())
	if err := first.Connect(ctx, peer.clientTLS, config.Quic{}.GetConfig()); err != nil {
		t.Fatalf("connect first token client: %v", err)
	}
	if first.Connection().ConnectionState().TLS.DidResume {
		t.Fatal("first token connection unexpectedly resumed a TLS session")
	}
	if err := first.RegisterWithAuth(ctx, "resumed-token-client", tokenTestAuth()); err != nil {
		t.Fatalf("register first token connection: %v", err)
	}
	if err := awaitLifecycle(t, firstServerDone, "first token registration"); err != nil {
		t.Fatal(err)
	}
	if firstRegistration.Auth == nil {
		t.Fatal("first registration omitted token authentication")
	}
	firstProof := bytes.Clone(firstRegistration.Auth.Proof)

	select {
	case <-cache.put:
	case <-ctx.Done():
		t.Fatalf("wait for TLS session ticket: %v", ctx.Err())
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first token connection: %v", err)
	}

	var secondRegistration protocol.RegisterMsg
	var replayErr error
	secondServerDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, registration protocol.RegisterMsg) error {
		secondRegistration = registration
		if err := verifyTokenRegistration(conn, registration); err != nil {
			return err
		}
		replayErr = sharedtoken.Verify(
			[]byte(tokenRegistrationTestSecret),
			sharedtoken.Transcript{
				ClientID:     registration.ClientID,
				Version:      registration.Version,
				Capabilities: registration.Capabilities,
			},
			firstProof,
			conn.ConnectionState().TLS,
		)
		return protocol.WriteRegisterAckWithAuth(
			stream,
			true,
			"registered",
			protocol.ProtocolVersion,
			config.DefaultCapabilities,
			sharedtoken.Scheme,
		)
	})
	second := NewServerConnection(peer.endpoint().Address, "lifecycle.test", cache, zerolog.Nop())
	t.Cleanup(func() { _ = second.Close() })
	if err := second.Connect(ctx, peer.clientTLS, config.Quic{}.GetConfig()); err != nil {
		t.Fatalf("connect resumed token client: %v", err)
	}
	if !second.Connection().ConnectionState().TLS.DidResume {
		t.Fatal("second token connection did not resume the TLS session")
	}
	if err := second.RegisterWithAuth(ctx, "resumed-token-client", tokenTestAuth()); err != nil {
		t.Fatalf("register resumed token connection: %v", err)
	}
	if err := awaitLifecycle(t, secondServerDone, "resumed token registration"); err != nil {
		t.Fatal(err)
	}
	if secondRegistration.Auth == nil {
		t.Fatal("resumed registration omitted token authentication")
	}
	if bytes.Equal(firstProof, secondRegistration.Auth.Proof) {
		t.Fatal("resumed connection reused the first connection's token proof")
	}
	if replayErr == nil {
		t.Fatal("first connection's token proof verified on the resumed connection")
	}
}
