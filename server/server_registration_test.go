package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"reflect"
	"testing"
	"time"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	serverauth "github.com/Mmx233/QMux/server/auth"
	"github.com/Mmx233/QMux/server/auth/mtls"
	"github.com/Mmx233/QMux/server/auth/tokenauth"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const registrationTestAddress = "registration-test"

type registrationSessionCache struct {
	tls.ClientSessionCache
	put chan struct{}
}

func newRegistrationSessionCache() *registrationSessionCache {
	return &registrationSessionCache{
		ClientSessionCache: tls.NewLRUClientSessionCache(1),
		put:                make(chan struct{}, 1),
	}
}

func (c *registrationSessionCache) Put(key string, state *tls.ClientSessionState) {
	c.ClientSessionCache.Put(key, state)
	if state != nil {
		select {
		case c.put <- struct{}{}:
		default:
		}
	}
}

func TestTokenRegistrationTransaction(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	authenticator, err := tokenauth.New(secret)
	if err != nil {
		t.Fatalf("tokenauth.New() error = %v", err)
	}
	harness := newRegistrationHarness(t, authenticator, 2*time.Second)
	stream := harness.openStream(t)
	registration := serverauth.Registration{
		ClientID:     "client-1",
		Version:      protocol.ProtocolVersion,
		Capabilities: []string{protocol.CapabilityUDPWireV2},
		Scheme:       sharedtoken.Scheme,
	}
	proof, err := sharedtoken.Compute(secret, sharedtoken.Transcript{
		ClientID:     registration.ClientID,
		Version:      registration.Version,
		Capabilities: registration.Capabilities,
	}, harness.client.ConnectionState().TLS)
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	if err := protocol.WriteRegisterWithAuth(
		stream,
		registration.ClientID,
		registration.Version,
		registration.Capabilities,
		&protocol.RegisterAuth{Scheme: registration.Scheme, Proof: proof},
	); err != nil {
		t.Fatalf("WriteRegisterWithAuth() error = %v", err)
	}

	var ack protocol.RegisterAckMsg
	if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeRegisterAck, &ack); err != nil {
		t.Fatalf("read registration Ack: %v", err)
	}
	if err := protocol.ValidateRegisterAckWithAuth(ack, sharedtoken.Scheme); err != nil {
		t.Fatalf("ValidateRegisterAckWithAuth() error = %v", err)
	}
	eventually(t, time.Second, func() bool { return harness.pool.Count() == 1 })
	assertPendingRegistrationAvailable(t, harness.pool)
}

func TestRegistrationReservationIsNotSelectableBeforeAck(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	authenticator, err := tokenauth.New(secret)
	if err != nil {
		t.Fatalf("tokenauth.New() error = %v", err)
	}
	ackStarted := make(chan struct{})
	releaseAck := make(chan struct{})
	writerFactory := func(connectionPool *pool.ConnectionPool) registrationAckWriter {
		return func(
			w io.Writer,
			success bool,
			message, serverVersion string,
			selectedCapabilities []string,
			selectedAuthScheme string,
		) error {
			if success {
				if got := connectionPool.Count(); got != 0 {
					t.Errorf("pool Count() before success Ack = %d, want 0", got)
				}
				if _, err := connectionPool.Select(); !errors.Is(err, pool.ErrNoClientsAvailable) {
					t.Errorf("pool Select() before success Ack error = %v, want %v", err, pool.ErrNoClientsAvailable)
				}
				close(ackStarted)
				<-releaseAck
			}
			return protocol.WriteRegisterAckWithAuth(
				w, success, message, serverVersion, selectedCapabilities, selectedAuthScheme,
			)
		}
	}
	harness := newRegistrationHarness(t, authenticator, 2*time.Second, writerFactory)
	stream := harness.openStream(t)
	capabilities := []string{protocol.CapabilityUDPWireV2}
	proof, err := sharedtoken.Compute(secret, sharedtoken.Transcript{
		ClientID:     "client-1",
		Version:      protocol.ProtocolVersion,
		Capabilities: capabilities,
	}, harness.client.ConnectionState().TLS)
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	if err := protocol.WriteRegisterWithAuth(stream, "client-1", protocol.ProtocolVersion, capabilities, &protocol.RegisterAuth{
		Scheme: sharedtoken.Scheme,
		Proof:  proof,
	}); err != nil {
		t.Fatalf("WriteRegisterWithAuth() error = %v", err)
	}
	select {
	case <-ackStarted:
	case <-time.After(time.Second):
		t.Fatal("server did not reach success Ack")
	}
	if got := harness.pool.Count(); got != 0 {
		t.Fatalf("pool Count() while Ack was blocked = %d, want 0", got)
	}
	close(releaseAck)
	var ack protocol.RegisterAckMsg
	if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeRegisterAck, &ack); err != nil {
		t.Fatalf("read registration Ack: %v", err)
	}
	eventually(t, time.Second, func() bool { return harness.pool.Count() == 1 })
}

func TestTokenRegistrationAuthFailuresSendNoAck(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tests := []struct {
		name string
		auth *protocol.RegisterAuth
	}{
		{name: "missing proof"},
		{name: "wrong scheme", auth: &protocol.RegisterAuth{Scheme: "mtls", Proof: make([]byte, sharedtoken.ProofSize)}},
		{name: "wrong proof", auth: &protocol.RegisterAuth{Scheme: sharedtoken.Scheme, Proof: make([]byte, sharedtoken.ProofSize)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator, err := tokenauth.New(secret)
			if err != nil {
				t.Fatalf("tokenauth.New() error = %v", err)
			}
			harness := newRegistrationHarness(t, authenticator, time.Second)
			stream := harness.openStream(t)
			if err := protocol.WriteRegisterWithAuth(
				stream,
				"client-1",
				protocol.ProtocolVersion,
				[]string{protocol.CapabilityUDPWireV2},
				test.auth,
			); err != nil {
				t.Fatalf("WriteRegisterWithAuth() error = %v", err)
			}
			assertGenericRegistrationCloseWithoutAck(t, stream)
			harness.waitForHandler(t)
			if got := harness.pool.Count(); got != 0 {
				t.Fatalf("pool Count() after authentication failure = %d, want 0", got)
			}
		})
	}
}

func TestRegistrationRejectsMalformedAndOversizedMessages(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tests := []struct {
		name string
		wire []byte
	}{
		{name: "malformed JSON", wire: []byte{protocol.MsgTypeRegister, 0, 0, 0, 1, '{'}},
		{name: "oversized payload", wire: oversizedRegistrationHeader()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator, err := tokenauth.New(secret)
			if err != nil {
				t.Fatalf("tokenauth.New() error = %v", err)
			}
			harness := newRegistrationHarness(t, authenticator, time.Second)
			stream := harness.openStream(t)
			if _, err := stream.Write(test.wire); err != nil {
				t.Fatalf("write registration wire data: %v", err)
			}
			assertGenericRegistrationCloseWithoutAck(t, stream)
			harness.waitForHandler(t)
			if got := harness.pool.Count(); got != 0 {
				t.Fatalf("pool Count() after invalid registration = %d, want 0", got)
			}
		})
	}
}

func TestRegistrationStallIsBounded(t *testing.T) {
	authenticator, err := tokenauth.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("tokenauth.New() error = %v", err)
	}
	harness := newRegistrationHarness(t, authenticator, 150*time.Millisecond)
	stream := harness.openStream(t)
	started := time.Now()
	assertGenericRegistrationCloseWithoutAck(t, stream)
	harness.waitForHandler(t)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled registration took %v, want <= 1s", elapsed)
	}
}

func TestRegistrationPartialMessageStallsAreBounded(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
	}{
		{name: "partial header", wire: []byte{protocol.MsgTypeRegister, 0}},
		{name: "partial payload", wire: []byte{protocol.MsgTypeRegister, 0, 0, 0, 20, '{'}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator, err := tokenauth.New([]byte("0123456789abcdef0123456789abcdef"))
			if err != nil {
				t.Fatalf("tokenauth.New() error = %v", err)
			}
			harness := newRegistrationHarness(t, authenticator, 150*time.Millisecond)
			stream := harness.openStream(t)
			if _, err := stream.Write(test.wire); err != nil {
				t.Fatalf("write partial registration: %v", err)
			}
			started := time.Now()
			assertGenericRegistrationCloseWithoutAck(t, stream)
			harness.waitForHandler(t)
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("partial registration stall took %v, want <= 1s", elapsed)
			}
			if got := harness.pool.Count(); got != 0 {
				t.Fatalf("pool Count() after partial stall = %d, want 0", got)
			}
		})
	}
}

func TestRegistrationParentCancellationUnblocksRead(t *testing.T) {
	authenticator, err := tokenauth.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("tokenauth.New() error = %v", err)
	}
	harness := newRegistrationHarness(t, authenticator, 5*time.Second)
	stream := harness.openStream(t)
	if _, err := stream.Write([]byte{protocol.MsgTypeRegister, 0, 0}); err != nil {
		t.Fatalf("write partial registration header: %v", err)
	}
	started := time.Now()
	harness.cancel()
	_ = stream.SetReadDeadline(time.Now().Add(time.Second))
	_, _, _ = protocol.ReadMessage(stream)
	harness.waitForHandler(t)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("registration cancellation took %v, want <= 1s", elapsed)
	}
}

func TestRegistrationMissingControlStreamIsBounded(t *testing.T) {
	authenticator, err := tokenauth.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("tokenauth.New() error = %v", err)
	}
	harness := newRegistrationHarness(t, authenticator, 150*time.Millisecond)
	started := time.Now()
	harness.waitForHandler(t)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("missing control stream took %v, want <= 1s", elapsed)
	}
	assertGenericConnectionClose(t, harness.client)
	assertRegistrationFailureReleasedResources(t, harness)
}

func TestRegistrationMissingControlStreamParentCancellation(t *testing.T) {
	authenticator, err := tokenauth.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("tokenauth.New() error = %v", err)
	}
	harness := newRegistrationHarness(t, authenticator, 5*time.Second)
	started := time.Now()
	harness.cancel()
	harness.waitForHandler(t)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("missing control stream cancellation took %v, want <= 1s", elapsed)
	}
	assertGenericConnectionClose(t, harness.client)
	assertRegistrationFailureReleasedResources(t, harness)
}

func TestAuthenticatedIncompatibleRegistrationGetsNegativeAck(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	authenticator, err := tokenauth.New(secret)
	if err != nil {
		t.Fatalf("tokenauth.New() error = %v", err)
	}
	type ackWrite struct {
		success              bool
		serverVersion        string
		selectedCapabilities []string
		err                  error
	}
	var ackWrites []ackWrite
	writerFactory := func(_ *pool.ConnectionPool) registrationAckWriter {
		return func(
			w io.Writer,
			success bool,
			message, serverVersion string,
			selectedCapabilities []string,
			selectedAuthScheme string,
		) error {
			err := protocol.WriteRegisterAckWithAuth(
				w, success, message, serverVersion, selectedCapabilities, selectedAuthScheme,
			)
			ackWrites = append(ackWrites, ackWrite{
				success:              success,
				serverVersion:        serverVersion,
				selectedCapabilities: selectedCapabilities,
				err:                  err,
			})
			return err
		}
	}
	harness := newRegistrationHarness(t, authenticator, time.Second, writerFactory)
	stream := harness.openStream(t)
	version := "1.0"
	capabilities := []string{protocol.CapabilityUDPWireV2}
	proof, err := sharedtoken.Compute(secret, sharedtoken.Transcript{
		ClientID:     "client-1",
		Version:      version,
		Capabilities: capabilities,
	}, harness.client.ConnectionState().TLS)
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	if err := protocol.WriteRegisterWithAuth(stream, "client-1", version, capabilities, &protocol.RegisterAuth{
		Scheme: sharedtoken.Scheme,
		Proof:  proof,
	}); err != nil {
		t.Fatalf("WriteRegisterWithAuth() error = %v", err)
	}
	var ack protocol.RegisterAckMsg
	if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeRegisterAck, &ack); err == nil {
		if ack.Success {
			t.Fatal("incompatible registration received a success Ack")
		}
	} else {
		// QUIC application close can overtake the best-effort negative Ack. The
		// protocol permits either outcome for an incompatible authenticated peer.
		var applicationError *quic.ApplicationError
		if !errors.As(err, &applicationError) ||
			applicationError.ErrorCode != registrationErrorCode ||
			applicationError.ErrorMessage != registrationFailureReason {
			t.Fatalf("incompatible registration error = %v, want negative Ack or generic close", err)
		}
	}
	harness.waitForHandler(t)
	if len(ackWrites) != 1 {
		t.Fatalf("registration Ack writes = %d, want 1", len(ackWrites))
	}
	write := ackWrites[0]
	if write.success {
		t.Fatal("incompatible registration writer received a success Ack")
	}
	if write.serverVersion != protocol.ProtocolVersion {
		t.Fatalf("negative Ack server version = %q, want %q", write.serverVersion, protocol.ProtocolVersion)
	}
	if write.selectedCapabilities != nil {
		t.Fatalf("negative Ack selected capabilities = %v, want nil", write.selectedCapabilities)
	}
	if write.err != nil {
		t.Fatalf("write negative Ack: %v", write.err)
	}
	if got := harness.pool.Count(); got != 0 {
		t.Fatalf("pool Count() after incompatible registration = %d, want 0", got)
	}
}

func TestMTLSRegistrationAcceptsTLSVerifiedChains(t *testing.T) {
	tests := []struct {
		name             string
		withIntermediate bool
		wantPeerCerts    int
		wantVerified     int
	}{
		{name: "root-direct", wantPeerCerts: 1, wantVerified: 2},
		{name: "root-intermediate-leaf", withIntermediate: true, wantPeerCerts: 2, wantVerified: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clientCertificate, clientRoots := registrationTestClientCertificate(
				t, test.name, test.withIntermediate, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			)
			serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
			harness := newRegistrationHarnessWithTLS(t, mtls.New(clientRoots), time.Second, serverTLS, clientTLS)

			registerMTLSClient(t, harness, "mtls-"+test.name)
			serverState := harness.serverConn.ConnectionState().TLS
			if got := len(serverState.PeerCertificates); got != test.wantPeerCerts {
				t.Fatalf("server PeerCertificates length = %d, want %d", got, test.wantPeerCerts)
			}
			if got := len(serverState.VerifiedChains[0]); got != test.wantVerified {
				t.Fatalf("server verified chain length = %d, want %d", got, test.wantVerified)
			}
		})
	}
}

func TestMTLSRegistrationAcceptsResumedTLS13Session(t *testing.T) {
	clientCertificate, clientRoots := registrationTestClientCertificate(
		t, "resumed", true, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	)
	serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
	before := serverTLS.Clone()
	manager, err := configureSessionTicketKeyRotation(serverTLS, 0, 0)
	if err != nil {
		t.Fatalf("configure automatic session tickets: %v", err)
	}
	if manager != nil || serverTLS.GetConfigForClient != nil {
		t.Fatal("zero interval installed a custom session ticket manager")
	}
	if !reflect.DeepEqual(serverTLS, before) {
		t.Fatal("zero interval mutated the TLS config")
	}
	cache := newRegistrationSessionCache()
	clientTLS.ClientSessionCache = cache

	first := newRegistrationHarnessWithTLS(t, mtls.New(clientRoots), time.Second, serverTLS, clientTLS)
	registerMTLSClient(t, first, "mtls-first")
	if first.client.ConnectionState().TLS.DidResume || first.serverConn.ConnectionState().TLS.DidResume {
		t.Fatal("first mTLS connection unexpectedly resumed a TLS session")
	}
	select {
	case <-cache.put:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS session ticket")
	}
	if err := first.client.CloseWithError(0, "resume test"); err != nil {
		t.Fatalf("close first mTLS connection: %v", err)
	}
	first.waitForHandler(t)

	second := newRegistrationHarnessWithTLS(t, mtls.New(clientRoots), time.Second, serverTLS, clientTLS)
	if !second.client.ConnectionState().TLS.DidResume || !second.serverConn.ConnectionState().TLS.DidResume {
		t.Fatal("second mTLS connection did not resume the TLS session on both peers")
	}
	verifiedChains := second.serverConn.ConnectionState().TLS.VerifiedChains
	if len(verifiedChains) != 1 {
		t.Fatalf("resumed server verified chain count = %d, want 1", len(verifiedChains))
	}
	if got := len(verifiedChains[0]); got != 3 {
		t.Fatalf("resumed server verified chain length = %d, want 3", got)
	}
	registerMTLSClient(t, second, "mtls-second")
}

type sessionTicketLogCapture struct {
	events chan []byte
}

func (c *sessionTicketLogCapture) Write(p []byte) (int, error) {
	c.events <- append([]byte(nil), p...)
	return len(p), nil
}

func TestSessionTicketRotationModeLogs(t *testing.T) {
	for _, test := range []struct {
		name     string
		interval time.Duration
		overlap  *uint8
		message  string
	}{
		{name: "automatic", message: "using Go automatic session ticket key rotation"},
		{name: "custom", interval: time.Hour, overlap: new(uint8(7)), message: "session ticket key rotation enabled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			certificate, _ := registrationTestCertificate(t)
			capture := &sessionTicketLogCapture{events: make(chan []byte, 16)}
			srv := &Server{
				config: &config.Server{
					Auth: config.ServerAuth{Method: "token"},
					TLS: config.ServerTLS{
						ServerCert: certificate,
						SessionTicketEncryptionKeyRotationInterval: test.interval,
						SessionTicketEncryptionKeyRotationOverlap:  test.overlap,
					},
				},
				handshakes: make(map[string]*handshakeStats),
				logger:     zerolog.New(capture),
			}
			listener := config.QuicListener{
				QuicAddr: "127.0.0.1:0", TrafficAddr: "127.0.0.1:0", Protocol: "tcp",
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- srv.startListener(ctx, listener) }()

			var event map[string]any
			for event["message"] != test.message {
				select {
				case raw := <-capture.events:
					if err := json.Unmarshal(raw, &event); err != nil {
						t.Fatalf("decode log event: %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatalf("timed out waiting for %q", test.message)
				}
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("startListener: %v", err)
			}

			if test.interval == 0 {
				if _, ok := event["old_key_limit"]; ok {
					t.Fatalf("automatic mode logged custom key limit: %v", event)
				}
				return
			}
			if got := int(event["old_key_limit"].(float64)); got != 7 {
				t.Fatalf("old_key_limit = %d, want 7", got)
			}
			if got := int(event["max_total_keys"].(float64)); got != 8 {
				t.Fatalf("max_total_keys = %d, want 8", got)
			}
		})
	}
}

func TestMTLSHandshakeRejectsUnverifiedClientCertificates(t *testing.T) {
	for _, test := range []struct {
		name        string
		noCert      bool
		serverOnly  bool
		foreignRoot bool
	}{
		{name: "no-certificate", noCert: true},
		{name: "server-auth-only", serverOnly: true},
		{name: "foreign-root", foreignRoot: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
			if test.serverOnly {
				usage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			}
			clientCertificate, clientRoots := registrationTestClientCertificate(t, test.name, false, usage)
			if test.foreignRoot {
				_, clientRoots = registrationTestClientCertificate(
					t, "trusted-root", false, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
				)
			}
			serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
			if test.noCert {
				clientTLS.Certificates = nil
			} else {
				// Force the certificate onto the wire so EKU/root failures cannot
				// silently degrade into the no-certificate case.
				clientTLS.Certificates = nil
				clientTLS.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
					return &clientCertificate, nil
				}
			}
			assertMTLSHandshakeRejected(t, serverTLS, clientTLS, !test.noCert)
		})
	}
}

type registrationHarness struct {
	client      *quic.Conn
	serverConn  *quic.Conn
	server      *Server
	listener    *quic.Listener
	pool        *pool.ConnectionPool
	ctx         context.Context
	cancel      context.CancelFunc
	handlerDone <-chan struct{}
}

func newRegistrationHarness(
	t *testing.T,
	authenticator serverauth.Auth,
	timeout time.Duration,
	ackWriterFactories ...func(*pool.ConnectionPool) registrationAckWriter,
) *registrationHarness {
	t.Helper()
	certificate, _ := registrationTestCertificate(t)
	return newRegistrationHarnessWithTLS(
		t,
		authenticator,
		timeout,
		&tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS13,
			NextProtos:   []string{"qmux-registration-test"},
		},
		&tls.Config{
			InsecureSkipVerify: true, // Test-only self-signed server certificate.
			MinVersion:         tls.VersionTLS13,
			NextProtos:         []string{"qmux-registration-test"},
		},
		ackWriterFactories...,
	)
}

func newRegistrationHarnessWithTLS(
	t *testing.T,
	authenticator serverauth.Auth,
	timeout time.Duration,
	serverTLS, clientTLS *tls.Config,
	ackWriterFactories ...func(*pool.ConnectionPool) registrationAckWriter,
) *registrationHarness {
	return newRegistrationHarnessWithTLSAndQUIC(
		t,
		authenticator,
		timeout,
		serverTLS,
		clientTLS,
		&config.Server{
			HeartbeatInterval: time.Hour,
			HealthTimeout:     2 * time.Hour,
		},
		&quic.Config{},
		&quic.Config{},
		ackWriterFactories...,
	)
}

func newRegistrationHarnessWithTLSAndQUIC(
	t *testing.T,
	authenticator serverauth.Auth,
	timeout time.Duration,
	serverTLS, clientTLS *tls.Config,
	serverConfig *config.Server,
	serverQUIC, clientQUIC *quic.Config,
	ackWriterFactories ...func(*pool.ConnectionPool) registrationAckWriter,
) *registrationHarness {
	t.Helper()
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	transport := &quic.Transport{Conn: udpConn}
	listener, err := transport.Listen(serverTLS, serverQUIC)
	if err != nil {
		_ = udpConn.Close()
		t.Fatalf("listen QUIC: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	accepted := make(chan *quic.Conn, 1)
	acceptErr := make(chan error, 1)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		conn, err := listener.Accept(ctx)
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		select {
		case <-acceptDone:
		case <-time.After(2 * time.Second):
			t.Error("QUIC accept loop did not stop")
		}
		_ = transport.Close()
		_ = udpConn.Close()
	})
	dialCtx, cancelDial := context.WithTimeout(ctx, 2*time.Second)
	client, err := quic.DialAddr(dialCtx, listener.Addr().String(), clientTLS, clientQUIC)
	cancelDial()
	if err != nil {
		t.Fatalf("dial QUIC: %v", err)
	}
	var serverConn *quic.Conn
	select {
	case serverConn = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("accept QUIC: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("accept QUIC timed out")
	}

	connectionPool := pool.New(registrationTestAddress, pool.NewRoundRobinBalancer(), zerolog.Nop())
	server := &Server{
		config:              serverConfig,
		pools:               map[string]*pool.ConnectionPool{registrationTestAddress: connectionPool},
		authenticator:       authenticator,
		registrationTimeout: timeout,
		logger:              zerolog.Nop(),
	}
	if len(ackWriterFactories) > 1 {
		t.Fatal("newRegistrationHarness accepts at most one Ack writer")
	}
	if len(ackWriterFactories) == 1 {
		server.writeRegistrationAck = ackWriterFactories[0](connectionPool)
	}
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		server.handleConnection(ctx, serverConn, registrationTestAddress)
	}()

	harness := &registrationHarness{
		client:      client,
		serverConn:  serverConn,
		server:      server,
		listener:    listener,
		pool:        connectionPool,
		ctx:         ctx,
		cancel:      cancel,
		handlerDone: handlerDone,
	}
	t.Cleanup(func() {
		_ = harness.client.CloseWithError(0, "test complete")
		cancel()
		select {
		case <-harness.handlerDone:
		case <-time.After(2 * time.Second):
			t.Error("server connection handler did not stop")
		}
		connectionPool.Stop()
	})
	return harness
}

func (h *registrationHarness) reconnect(t *testing.T, clientTLS *tls.Config, clientQUIC *quic.Config) {
	t.Helper()
	accepted := make(chan *quic.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := h.listener.Accept(h.ctx)
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	dialCtx, cancelDial := context.WithTimeout(h.ctx, 2*time.Second)
	client, err := quic.DialAddr(dialCtx, h.listener.Addr().String(), clientTLS, clientQUIC)
	cancelDial()
	if err != nil {
		t.Fatalf("reconnect QUIC: %v", err)
	}
	var serverConn *quic.Conn
	select {
	case serverConn = <-accepted:
	case err := <-acceptErr:
		_ = client.CloseWithError(0, "reconnect accept failed")
		t.Fatalf("accept reconnected QUIC: %v", err)
	case <-time.After(2 * time.Second):
		_ = client.CloseWithError(0, "reconnect accept timeout")
		t.Fatal("accept reconnected QUIC timed out")
	}
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		h.server.handleConnection(h.ctx, serverConn, registrationTestAddress)
	}()
	h.client = client
	h.serverConn = serverConn
	h.handlerDone = handlerDone
}

func (h *registrationHarness) openStream(t *testing.T) *quic.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := h.client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync() error = %v", err)
	}
	if err := stream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	return stream
}

func registerMTLSClient(t *testing.T, harness *registrationHarness, clientID string) *quic.Stream {
	t.Helper()
	stream := harness.openStream(t)
	if err := protocol.WriteRegisterWithAuth(
		stream,
		clientID,
		protocol.ProtocolVersion,
		[]string{protocol.CapabilityUDPWireV2},
		nil,
	); err != nil {
		t.Fatalf("WriteRegisterWithAuth() error = %v", err)
	}
	var ack protocol.RegisterAckMsg
	if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeRegisterAck, &ack); err != nil {
		t.Fatalf("read mTLS registration Ack: %v", err)
	}
	if err := protocol.ValidateRegisterAckWithAuth(ack, ""); err != nil {
		t.Fatalf("ValidateRegisterAckWithAuth() error = %v", err)
	}
	eventually(t, time.Second, func() bool { return harness.pool.Count() == 1 })
	return stream
}

func (h *registrationHarness) waitForHandler(t *testing.T) {
	t.Helper()
	select {
	case <-h.handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("server connection handler did not stop")
	}
}

func assertGenericRegistrationCloseWithoutAck(t *testing.T, stream *quic.Stream) {
	t.Helper()
	var ack protocol.RegisterAckMsg
	err := protocol.ReadTypedMessage(stream, protocol.MsgTypeRegisterAck, &ack)
	if err == nil {
		t.Fatalf("received diagnostic registration Ack: %+v", ack)
	}
	var applicationError *quic.ApplicationError
	if !errors.As(err, &applicationError) {
		t.Fatalf("registration failure error = %T %v, want application close", err, err)
	}
	if applicationError.ErrorCode != registrationErrorCode || applicationError.ErrorMessage != registrationFailureReason {
		t.Fatalf("application close = (%d, %q), want (%d, %q)",
			applicationError.ErrorCode, applicationError.ErrorMessage, registrationErrorCode, registrationFailureReason)
	}
}

func assertGenericConnectionClose(t *testing.T, conn *quic.Conn) {
	t.Helper()
	select {
	case <-conn.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("client connection did not receive registration close")
	}
	var applicationError *quic.ApplicationError
	err := context.Cause(conn.Context())
	if !errors.As(err, &applicationError) {
		t.Fatalf("connection close error = %T %v, want application close", err, err)
	}
	if applicationError.ErrorCode != registrationErrorCode || applicationError.ErrorMessage != registrationFailureReason {
		t.Fatalf("application close = (%d, %q), want (%d, %q)",
			applicationError.ErrorCode, applicationError.ErrorMessage, registrationErrorCode, registrationFailureReason)
	}
}

func assertRegistrationFailureReleasedResources(t *testing.T, harness *registrationHarness) {
	t.Helper()
	snapshot := harness.pool.Snapshot()
	if snapshot.ServerPending != 0 || snapshot.Reservations != 0 || snapshot.Registered != 0 ||
		snapshot.ServerRetiring != 0 || snapshot.PendingRegistrations.Current != 0 ||
		snapshot.ClientGenerations.Current != 0 {
		t.Fatalf("pool snapshot after failed registration = %+v, want no owned generations", snapshot)
	}
}

func assertPendingRegistrationAvailable(t *testing.T, connectionPool *pool.ConnectionPool) {
	t.Helper()
	pending := connectionPool.BeginPending()
	if pending == nil {
		t.Fatal("pending registration capacity unavailable")
	}
	if !connectionPool.Abort(pending) {
		t.Fatal("abort pending registration failed")
	}
}

func oversizedRegistrationHeader() []byte {
	header := make([]byte, 5)
	header[0] = protocol.MsgTypeRegister
	binary.BigEndian.PutUint32(header[1:], protocol.MaxRegistrationPayloadSize+1)
	return header
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not satisfied before timeout")
		}
		time.Sleep(time.Millisecond)
	}
}

func registrationMTLSTLSConfigs(
	t *testing.T,
	clientRoots *x509.CertPool,
	clientCertificate tls.Certificate,
) (*tls.Config, *tls.Config) {
	t.Helper()
	serverCertificate, serverRoots := registrationTestCertificate(t)
	return &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientRoots,
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		NextProtos:   []string{"qmux-registration-test"},
	}, &tls.Config{
		RootCAs:      serverRoots,
		Certificates: []tls.Certificate{clientCertificate},
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		NextProtos:   []string{"qmux-registration-test"},
	}
}

func assertMTLSHandshakeRejected(
	t *testing.T,
	serverTLS, clientTLS *tls.Config,
	expectCertificate bool,
) {
	t.Helper()
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	transport := &quic.Transport{Conn: udpConn}
	listener, err := transport.Listen(serverTLS, &quic.Config{})
	if err != nil {
		_ = transport.Close()
		_ = udpConn.Close()
		t.Fatalf("listen QUIC: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	accepted := make(chan *quic.Conn, 1)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		conn, acceptErr := listener.Accept(ctx)
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	var client *quic.Conn
	t.Cleanup(func() {
		if client != nil {
			_ = client.CloseWithError(0, "test complete")
		}
		cancel()
		_ = listener.Close()
		select {
		case <-acceptDone:
		case <-time.After(2 * time.Second):
			t.Error("mTLS rejection accept loop did not stop")
		}
		_ = transport.Close()
		_ = udpConn.Close()
	})

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDial()
	client, err = quic.DialAddr(dialCtx, listener.Addr().String(), clientTLS, &quic.Config{})
	if err == nil {
		_, err = client.AcceptStream(dialCtx)
	}
	var transportError *quic.TransportError
	if !errors.As(err, &transportError) || !transportError.ErrorCode.IsCryptoError() {
		t.Fatalf("mTLS rejection error = %T %v, want QUIC crypto close", err, err)
	}
	const certificateRequired = quic.TransportErrorCode(0x174)
	if expectCertificate && transportError.ErrorCode == certificateRequired {
		t.Fatal("mTLS rejection reported certificate_required after the client sent a certificate")
	}
	if !expectCertificate && transportError.ErrorCode != certificateRequired {
		t.Fatalf("mTLS rejection code = %#x, want certificate_required %#x",
			transportError.ErrorCode, certificateRequired)
	}
	cancel()
	_ = listener.Close()
	select {
	case <-acceptDone:
	case <-time.After(2 * time.Second):
		t.Fatal("mTLS rejection accept loop did not stop")
	}
	// Listener.Accept only publishes connections with completed TLS handshakes.
	// Without one, the production handler, registration, and pool are unreachable.
	select {
	case conn := <-accepted:
		_ = conn.CloseWithError(0, "unexpected accepted connection")
		t.Fatal("rejected mTLS handshake produced a server connection")
	default:
	}
}

func registrationTestClientCertificate(
	t *testing.T,
	name string,
	withIntermediate bool,
	usages []x509.ExtKeyUsage,
) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client root key: %v", err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name + " root"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER, err := x509.CreateCertificate(
		rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey,
	)
	if err != nil {
		t.Fatalf("create client root certificate: %v", err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatalf("parse client root certificate: %v", err)
	}

	issuer, issuerKey := root, rootKey
	var intermediateDER []byte
	if withIntermediate {
		intermediateKey, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if keyErr != nil {
			t.Fatalf("generate client intermediate key: %v", keyErr)
		}
		intermediateTemplate := &x509.Certificate{
			SerialNumber:          big.NewInt(2),
			Subject:               pkix.Name{CommonName: name + " intermediate"},
			NotBefore:             time.Now().Add(-time.Minute),
			NotAfter:              time.Now().Add(time.Hour),
			KeyUsage:              x509.KeyUsageCertSign,
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		intermediateDER, err = x509.CreateCertificate(
			rand.Reader, intermediateTemplate, root, &intermediateKey.PublicKey, rootKey,
		)
		if err != nil {
			t.Fatalf("create client intermediate certificate: %v", err)
		}
		issuer, err = x509.ParseCertificate(intermediateDER)
		if err != nil {
			t.Fatalf("parse client intermediate certificate: %v", err)
		}
		issuerKey = intermediateKey
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client leaf key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: name + " client"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usages,
	}
	leafDER, err := x509.CreateCertificate(
		rand.Reader, leafTemplate, issuer, &leafKey.PublicKey, issuerKey,
	)
	if err != nil {
		t.Fatalf("create client leaf certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse client leaf certificate: %v", err)
	}
	chain := [][]byte{leafDER}
	if withIntermediate {
		chain = append(chain, intermediateDER)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return tls.Certificate{Certificate: chain, PrivateKey: leafKey, Leaf: leaf}, roots
}

func registrationTestCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse test certificate: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, roots
}
