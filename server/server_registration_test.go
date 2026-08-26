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
	"errors"
	"io"
	"math/big"
	"net"
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
	permit, ok := acquirePendingRegistration(harness.slots)
	if !ok {
		t.Fatal("registered connection still occupied pending registration capacity")
	}
	permit.Release()
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
	harness := newRegistrationHarness(t, authenticator, time.Second)
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
	if got := harness.pool.Count(); got != 0 {
		t.Fatalf("pool Count() after incompatible registration = %d, want 0", got)
	}
}

func TestMTLSRegistrationUsesClientOpenedControlStream(t *testing.T) {
	certificate, roots := registrationTestCertificate(t)
	authenticator := mtls.New(roots)
	harness := newRegistrationHarnessWithTLS(
		t,
		authenticator,
		time.Second,
		&tls.Config{
			Certificates: []tls.Certificate{certificate},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    roots,
			MinVersion:   tls.VersionTLS13,
			NextProtos:   []string{"qmux-registration-test"},
		},
		&tls.Config{
			RootCAs:      roots,
			Certificates: []tls.Certificate{certificate},
			ServerName:   "localhost",
			MinVersion:   tls.VersionTLS13,
			NextProtos:   []string{"qmux-registration-test"},
		},
	)
	stream := harness.openStream(t)
	if err := protocol.WriteRegister(
		stream,
		"mtls-client",
		protocol.ProtocolVersion,
		[]string{protocol.CapabilityUDPWireV2},
	); err != nil {
		t.Fatalf("WriteRegister() error = %v", err)
	}
	var ack protocol.RegisterAckMsg
	if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeRegisterAck, &ack); err != nil {
		t.Fatalf("read mTLS registration Ack: %v", err)
	}
	if err := protocol.ValidateRegisterAckWithAuth(ack, ""); err != nil {
		t.Fatalf("ValidateRegisterAckWithAuth() error = %v", err)
	}
	eventually(t, time.Second, func() bool { return harness.pool.Count() == 1 })
}

type registrationHarness struct {
	client      *quic.Conn
	pool        *pool.ConnectionPool
	slots       chan struct{}
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
	t.Helper()
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	transport := &quic.Transport{Conn: udpConn}
	listener, err := transport.Listen(serverTLS, &quic.Config{})
	if err != nil {
		_ = udpConn.Close()
		t.Fatalf("listen QUIC: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	accepted := make(chan *quic.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept(ctx)
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, &quic.Config{})
	if err != nil {
		cancel()
		_ = listener.Close()
		_ = transport.Close()
		_ = udpConn.Close()
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
		config: &config.Server{
			HeartbeatInterval: time.Hour,
			HealthTimeout:     2 * time.Hour,
		},
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
	slots := make(chan struct{}, 1)
	permit, ok := acquirePendingRegistration(slots)
	if !ok {
		t.Fatal("acquire registration permit failed")
	}
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		server.handleConnection(ctx, serverConn, registrationTestAddress, permit)
	}()

	harness := &registrationHarness{
		client:      client,
		pool:        connectionPool,
		slots:       slots,
		cancel:      cancel,
		handlerDone: handlerDone,
	}
	t.Cleanup(func() {
		_ = client.CloseWithError(0, "test complete")
		cancel()
		select {
		case <-handlerDone:
		case <-time.After(2 * time.Second):
			t.Error("server connection handler did not stop")
		}
		connectionPool.Stop()
		_ = listener.Close()
		_ = transport.Close()
		_ = udpConn.Close()
	})
	return harness
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
	if got := harness.pool.Count(); got != 0 {
		t.Fatalf("pool Count() after failed registration = %d, want 0", got)
	}
	permit, ok := acquirePendingRegistration(harness.slots)
	if !ok {
		t.Fatal("failed registration did not release pending capacity")
	}
	permit.Release()
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
