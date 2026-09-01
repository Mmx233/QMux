package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	certgen "github.com/Mmx233/QMux/cmd/generate/certs"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const lifecycleTestALPN = "qmux-registration-lifecycle-test"

type lifecycleTLSMaterial struct {
	server  tls.Certificate
	client  tls.Certificate
	pool    *x509.CertPool
	caPEM   []byte
	certPEM []byte
	keyPEM  []byte
	err     error
}

type elapsedDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c elapsedDeadlineContext) Deadline() (time.Time, bool) {
	return c.deadline, true
}

var (
	lifecycleTLSOnce sync.Once
	lifecycleTLSData lifecycleTLSMaterial
)

func lifecycleTLSConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()

	lifecycleTLSOnce.Do(func() {
		caKey, caCert, err := certgen.GenerateCA(1)
		if err != nil {
			lifecycleTLSData.err = fmt.Errorf("generate CA: %w", err)
			return
		}
		serverKey, serverCert, err := certgen.GenerateServerCert(caKey, caCert, 1, []string{"lifecycle.test", "localhost"})
		if err != nil {
			lifecycleTLSData.err = fmt.Errorf("generate server certificate: %w", err)
			return
		}
		clientKey, clientCert, err := certgen.GenerateClientCert(caKey, caCert, 1)
		if err != nil {
			lifecycleTLSData.err = fmt.Errorf("generate client certificate: %w", err)
			return
		}

		pool := x509.NewCertPool()
		pool.AddCert(caCert)
		lifecycleTLSData.server = tls.Certificate{
			Certificate: [][]byte{serverCert.Raw},
			PrivateKey:  serverKey,
			Leaf:        serverCert,
		}
		lifecycleTLSData.client = tls.Certificate{
			Certificate: [][]byte{clientCert.Raw},
			PrivateKey:  clientKey,
			Leaf:        clientCert,
		}
		lifecycleTLSData.pool = pool
		lifecycleTLSData.caPEM = certgen.EncodeCertificate(caCert)
		lifecycleTLSData.certPEM = certgen.EncodeCertificate(clientCert)
		lifecycleTLSData.keyPEM = certgen.EncodePrivateKey(clientKey)
	})
	if lifecycleTLSData.err != nil {
		t.Fatal(lifecycleTLSData.err)
	}

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{lifecycleTLSData.server},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    lifecycleTLSData.pool,
		NextProtos:   []string{lifecycleTestALPN},
	}
	clientTLS := &tls.Config{
		Certificates: []tls.Certificate{lifecycleTLSData.client},
		RootCAs:      lifecycleTLSData.pool,
		ServerName:   "lifecycle.test",
		NextProtos:   []string{lifecycleTestALPN},
	}
	return serverTLS, clientTLS
}

func lifecycleClientTLSFiles(t *testing.T) config.ClientTLS {
	t.Helper()
	lifecycleTLSConfigs(t)

	directory := t.TempDir()
	writeFile := func(name string, contents []byte) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatalf("write lifecycle TLS file %s: %v", name, err)
		}
		return path
	}

	return config.ClientTLS{
		CACertFile:     writeFile("ca.crt", lifecycleTLSData.caPEM),
		ClientCertFile: writeFile("client.crt", lifecycleTLSData.certPEM),
		ClientKeyFile:  writeFile("client.key", lifecycleTLSData.keyPEM),
	}
}

type lifecyclePeer struct {
	listener  *quic.Listener
	clientTLS *tls.Config
	ctx       context.Context
	cancel    context.CancelFunc
}

func newLifecyclePeer(t *testing.T) *lifecyclePeer {
	return newLifecyclePeerWithNextProtos(t, []string{lifecycleTestALPN})
}

func newLifecycleStartPeer(t *testing.T) *lifecyclePeer {
	return newLifecyclePeerWithNextProtos(t, nil)
}

func newLifecyclePeerWithNextProtos(t *testing.T, nextProtos []string) *lifecyclePeer {
	t.Helper()

	serverTLS, clientTLS := lifecycleTLSConfigs(t)
	serverTLS.NextProtos = nextProtos
	clientTLS.NextProtos = nextProtos
	return newLifecyclePeerWithTLS(t, serverTLS, clientTLS)
}

func newLifecyclePeerWithTLS(t *testing.T, serverTLS, clientTLS *tls.Config) *lifecyclePeer {
	t.Helper()

	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, &quic.Config{
		HandshakeIdleTimeout: 10 * time.Second,
		MaxIdleTimeout:       30 * time.Second,
		EnableDatagrams:      true,
	})
	if err != nil {
		t.Fatalf("listen for lifecycle peer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	peer := &lifecyclePeer{
		listener:  listener,
		clientTLS: clientTLS,
		ctx:       ctx,
		cancel:    cancel,
	}
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
	})
	return peer
}

func (p *lifecyclePeer) endpoint() config.ServerEndpoint {
	return config.ServerEndpoint{
		Address:    p.listener.Addr().String(),
		ServerName: "lifecycle.test",
	}
}

func (p *lifecyclePeer) serveRegistration(
	handler func(*quic.Conn, *quic.Stream, protocol.RegisterMsg) error,
) <-chan error {
	done := make(chan error, 1)
	go func() {
		conn, err := p.listener.Accept(p.ctx)
		if err != nil {
			done <- fmt.Errorf("accept QUIC connection: %w", err)
			return
		}
		stream, err := conn.AcceptStream(p.ctx)
		if err != nil {
			done <- fmt.Errorf("accept registration stream: %w", err)
			return
		}
		defer func() { _ = stream.Close() }()

		var registration protocol.RegisterMsg
		if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeRegister, &registration); err != nil {
			done <- fmt.Errorf("read registration: %w", err)
			return
		}
		done <- handler(conn, stream, registration)
	}()
	return done
}

func newLifecycleManager(t *testing.T, peer *lifecyclePeer) *ConnectionManager {
	t.Helper()

	endpoint := peer.endpoint()
	cm, err := NewConnectionManager(&config.Client{
		ClientID: "lifecycle-client",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{endpoint},
		},
		HeartbeatInterval: 20 * time.Millisecond,
		HealthTimeout:     2 * time.Second,
	}, zerolog.Nop())
	if err != nil {
		t.Fatalf("create connection manager: %v", err)
	}
	cm.baseTLSConfig = peer.clientTLS.Clone()
	cm.quicConfig = &quic.Config{
		HandshakeIdleTimeout: 10 * time.Second,
		MaxIdleTimeout:       30 * time.Second,
		EnableDatagrams:      true,
	}
	cm.attemptTimeout = 20 * time.Second
	return cm
}

func awaitLifecycle[T any](t *testing.T, ch <-chan T, event string) T {
	t.Helper()
	var value T
	select {
	case value = <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", event)
	}
	return value
}

func assertLifecycleUnpublished(t *testing.T, cm *ConnectionManager) {
	t.Helper()
	if got := cm.TotalCount(); got != 0 {
		t.Fatalf("provisional connection entered manager map: count=%d", got)
	}
	select {
	case sc, ok := <-cm.NewConns:
		if ok {
			t.Fatalf("provisional connection was published on NewConns: %v", sc.ServerAddr())
		}
	default:
	}
}

func writeSuccessfulLifecycleAck(stream *quic.Stream) error {
	return protocol.WriteRegisterAckWithAuth(
		stream,
		true,
		"registered",
		protocol.ProtocolVersion,
		config.DefaultCapabilities,
		"",
	)
}

func TestRegistrationIOErrorPreservesContextAndTransportCauses(t *testing.T) {
	transportErr := errors.New("stream read failed")

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := registrationIOError(ctx, "read registration ack", transportErr)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error does not preserve cancellation: %v", err)
		}
		if !errors.Is(err, transportErr) {
			t.Fatalf("error does not preserve transport cause: %v", err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		err := registrationIOError(ctx, "read registration ack", transportErr)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error does not preserve deadline: %v", err)
		}
		if !errors.Is(err, transportErr) {
			t.Fatalf("error does not preserve transport cause: %v", err)
		}
	})

	t.Run("elapsed deadline before context notification", func(t *testing.T) {
		ctx := elapsedDeadlineContext{
			Context:  context.Background(),
			deadline: time.Now().Add(-time.Second),
		}
		err := registrationIOError(ctx, "read registration ack", transportErr)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error does not infer elapsed deadline: %v", err)
		}
		if !errors.Is(err, transportErr) {
			t.Fatalf("error does not preserve transport cause: %v", err)
		}
	})
}

func TestPreferredServerIP(t *testing.T) {
	tests := []struct {
		name      string
		addresses []net.IPAddr
		wantIP    string
		wantZone  string
	}{
		{
			name: "IPv4 preferred after IPv6",
			addresses: []net.IPAddr{
				{IP: net.ParseIP("2001:db8::1")},
				{IP: net.ParseIP("192.0.2.10")},
			},
			wantIP: "192.0.2.10",
		},
		{
			name:      "IPv6 only",
			addresses: []net.IPAddr{{IP: net.ParseIP("2001:db8::2")}},
			wantIP:    "2001:db8::2",
		},
		{
			name:      "zoned IPv6",
			addresses: []net.IPAddr{{IP: net.ParseIP("fe80::1"), Zone: "en0"}},
			wantIP:    "fe80::1",
			wantZone:  "en0",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := preferredServerIP(test.addresses)
			if got.IP.String() != test.wantIP || got.Zone != test.wantZone {
				t.Fatalf("preferred address = %s zone %q, want %s zone %q", got.IP, got.Zone, test.wantIP, test.wantZone)
			}
		})
	}
}

func TestResolveServerAddressPreservesIPLiteral(t *testing.T) {
	tests := []string{
		"[2001:db8::1]:8443",
		"[fe80::1%en0]:8443",
	}

	for _, address := range tests {
		t.Run(address, func(t *testing.T) {
			gotAddress, _, err := resolveServerAddress(context.Background(), net.DefaultResolver, address)
			if err != nil {
				t.Fatalf("resolve literal address: %v", err)
			}
			if gotAddress != address {
				t.Fatalf("resolved address = %q, want unchanged %q", gotAddress, address)
			}
		})
	}
}

func TestResolveServerAddressHonorsContext(t *testing.T) {
	tests := []struct {
		name            string
		newContext      func() (context.Context, context.CancelFunc)
		want            error
		cancelAfterDial bool
	}{
		{
			name: "cancellation",
			newContext: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			want:            context.Canceled,
			cancelAfterDial: true,
		},
		{
			name: "deadline",
			newContext: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 100*time.Millisecond)
			},
			want: context.DeadlineExceeded,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookupStarted := make(chan struct{})
			var signalStarted sync.Once
			resolver := &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
					signalStarted.Do(func() { close(lookupStarted) })
					<-ctx.Done()
					return nil, context.Cause(ctx)
				},
			}
			ctx, cancel := test.newContext()
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, _, err := resolveServerAddress(ctx, resolver, "resolver-stall.test:8443")
				result <- err
			}()
			awaitLifecycle(t, lookupStarted, "resolver lookup start")
			if test.cancelAfterDial {
				cancel()
			}
			if err := awaitLifecycle(t, result, "resolver context error"); !errors.Is(err, test.want) {
				t.Fatalf("resolver error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestConnectPreservesOriginalHostnameForTLS(t *testing.T) {
	peer := newLifecyclePeer(t)
	_, port, err := net.SplitHostPort(peer.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	serverAddr := net.JoinHostPort("localhost", port)
	sc := NewServerConnection(
		serverAddr,
		"",
		tls.NewLRUClientSessionCache(1),
		zerolog.Nop(),
	)
	type acceptResult struct {
		conn *quic.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, acceptErr := peer.listener.Accept(peer.ctx)
		accepted <- acceptResult{conn: conn, err: acceptErr}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sc.Connect(ctx, peer.clientTLS, &quic.Config{HandshakeIdleTimeout: 10 * time.Second}); err != nil {
		t.Fatalf("connect with inferred localhost TLS name: %v", err)
	}
	if got := sc.Connection().ConnectionState().TLS.ServerName; got != "localhost" {
		t.Fatalf("negotiated TLS server name = %q, want localhost", got)
	}
	serverConnection := awaitLifecycle(t, accepted, "hostname-preserving handshake")
	if serverConnection.err != nil {
		t.Fatal(serverConnection.err)
	}
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := serverConnection.conn.CloseWithError(0, "test complete"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationLifecycleCallerCancellationInterruptsStalledAck(t *testing.T) {
	peer := newLifecyclePeer(t)
	cm := newLifecycleManager(t, peer)
	ready := make(chan struct{})
	serverDone := peer.serveRegistration(func(conn *quic.Conn, _ *quic.Stream, _ protocol.RegisterMsg) error {
		close(ready)
		<-conn.Context().Done()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := cm.connectAndRegister(ctx, peer.endpoint())
		result <- err
	}()
	awaitLifecycle(t, ready, "server to receive registration")
	cancel()

	err := awaitLifecycle(t, result, "registration cancellation")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("registration error = %v, want context cancellation", err)
	}
	if err := awaitLifecycle(t, serverDone, "provisional QUIC connection close"); err != nil {
		t.Fatal(err)
	}
	assertLifecycleUnpublished(t, cm)
}

func TestRegistrationLifecycleAttemptDeadlineInterruptsStalledAck(t *testing.T) {
	peer := newLifecyclePeer(t)
	cm := newLifecycleManager(t, peer)
	cm.attemptTimeout = 250 * time.Millisecond
	ready := make(chan struct{})
	serverDone := peer.serveRegistration(func(conn *quic.Conn, _ *quic.Stream, _ protocol.RegisterMsg) error {
		close(ready)
		<-conn.Context().Done()
		return nil
	})

	result := make(chan error, 1)
	go func() {
		_, err := cm.connectAndRegister(context.Background(), peer.endpoint())
		result <- err
	}()
	awaitLifecycle(t, ready, "server to stall until attempt deadline")

	err := awaitLifecycle(t, result, "registration attempt deadline")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("registration error = %v, want attempt deadline exceeded", err)
	}
	if err := awaitLifecycle(t, serverDone, "deadline-expired provisional QUIC connection close"); err != nil {
		t.Fatal(err)
	}
	assertLifecycleUnpublished(t, cm)
}

func TestRegistrationLifecycleStartAttemptDeadlineSchedulesFreshReconnect(t *testing.T) {
	peer := newLifecycleStartPeer(t)
	endpoint := peer.endpoint()
	cm, err := NewConnectionManager(&config.Client{
		ClientID: "start-deadline-reconnect",
		Server:   config.ClientServer{Servers: []config.ServerEndpoint{endpoint}},
		Local:    config.LocalService{Host: "127.0.0.1", Port: 1},
		Quic: config.Quic{
			HandshakeIdleTimeout: 10 * time.Second,
			MaxIdleTimeout:       30 * time.Second,
		},
		TLS:               lifecycleClientTLSFiles(t),
		HeartbeatInterval: time.Hour,
		HealthTimeout:     2 * time.Hour,
	}, zerolog.Nop())
	if err != nil {
		t.Fatalf("create connection manager: %v", err)
	}
	cm.attemptTimeout = 250 * time.Millisecond

	accepted := make(chan *quic.Conn, 2)
	serverDone := make(chan error, 1)
	go func() {
		for range 2 {
			conn, err := peer.listener.Accept(peer.ctx)
			if err != nil {
				serverDone <- fmt.Errorf("accept QUIC connection: %w", err)
				return
			}
			stream, err := conn.AcceptStream(peer.ctx)
			if err != nil {
				serverDone <- fmt.Errorf("accept registration stream: %w", err)
				return
			}
			var registration protocol.RegisterMsg
			if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeRegister, &registration); err != nil {
				serverDone <- fmt.Errorf("read registration: %w", err)
				return
			}
			accepted <- conn
			<-conn.Context().Done()
		}
		serverDone <- nil
	}()

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	startDone := make(chan error, 1)
	go func() {
		startDone <- cm.Start(runCtx)
	}()

	firstConnection := awaitLifecycle(t, accepted, "initial registration connection")
	if err := awaitLifecycle(t, startDone, "bounded Start attempt"); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if err := runCtx.Err(); err != nil {
		t.Fatalf("run context ended before reconnect scheduling: %v", err)
	}
	awaitLifecycle(t, firstConnection.Context().Done(), "attempt-deadline connection close")

	cm.reconnectMu.Lock()
	reconnectScheduled := cm.reconnecting[endpoint.Address]
	cm.reconnectMu.Unlock()
	if !reconnectScheduled {
		t.Fatal("internal attempt deadline did not schedule reconnect")
	}

	secondConnection := awaitLifecycle(t, accepted, "fresh reconnect registration connection")
	if secondConnection == firstConnection {
		t.Fatal("reconnect reused the initial QUIC connection")
	}

	cancelRun()
	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := awaitLifecycle(t, serverDone, "reconnect cleanup"); err != nil {
		t.Fatal(err)
	}
	assertLifecycleUnpublished(t, cm)
}

func TestConnectionManagerStartPublishesSeventeenEndpointsBeforeDrain(t *testing.T) {
	const endpointCount = 17
	peer := newLifecycleStartPeer(t)
	_, port, err := net.SplitHostPort(peer.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	endpoints := make([]config.ServerEndpoint, endpointCount)
	serverDone := make([]<-chan error, endpointCount)
	for i := range endpointCount {
		host := []byte("localhost")
		for bit := range len(host) {
			if i&(1<<bit) != 0 {
				host[bit] -= 'a' - 'A'
			}
		}
		endpoints[i] = config.ServerEndpoint{
			Address:    net.JoinHostPort(string(host), port),
			ServerName: "lifecycle.test",
		}
		serverDone[i] = peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
			if err := writeSuccessfulLifecycleAck(stream); err != nil {
				return err
			}
			<-conn.Context().Done()
			return nil
		})
	}

	cm, err := NewConnectionManager(&config.Client{
		ClientID: "seventeen-endpoints",
		Server:   config.ClientServer{Servers: endpoints},
		Local:    config.LocalService{Host: "127.0.0.1", Port: 1},
		Quic: config.Quic{
			HandshakeIdleTimeout: 10 * time.Second,
			MaxIdleTimeout:       30 * time.Second,
		},
		TLS:               lifecycleClientTLSFiles(t),
		HeartbeatInterval: time.Hour,
		HealthTimeout:     2 * time.Hour,
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cm.Stop() })
	cm.attemptTimeout = 20 * time.Second

	startDone := make(chan error, 1)
	go func() { startDone <- cm.Start(context.Background()) }()
	if err := awaitLifecycle(t, startDone, "17-endpoint ConnectionManager.Start before draining NewConns"); err != nil {
		t.Fatal(err)
	}
	if got := len(cm.NewConns); got != endpointCount {
		t.Fatalf("queued connections = %d, want %d", got, endpointCount)
	}
	seen := make(map[string]bool, endpointCount)
	for range endpointCount {
		sc := awaitLifecycle(t, cm.NewConns, "queued endpoint connection")
		seen[sc.ServerAddr()] = true
	}
	if len(seen) != endpointCount {
		t.Fatalf("distinct published endpoints = %d, want %d", len(seen), endpointCount)
	}
	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
	for i := range endpointCount {
		if err := awaitLifecycle(t, serverDone[i], "17-endpoint connection cleanup"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRegistrationLifecycleStopInterruptsStalledAck(t *testing.T) {
	peer := newLifecyclePeer(t)
	cm := newLifecycleManager(t, peer)
	ready := make(chan struct{})
	releaseAck := make(chan struct{})
	ackAttempted := make(chan struct{})
	serverDone := peer.serveRegistration(func(_ *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		close(ready)
		<-releaseAck
		_ = protocol.WriteRegisterAckWithAuth(
			stream,
			true,
			"late acknowledgment",
			protocol.ProtocolVersion,
			config.DefaultCapabilities,
			"",
		)
		close(ackAttempted)
		return nil
	})

	attemptDone := make(chan error, 1)
	cm.wg.Go(func() {
		sc, err := cm.connectAndRegister(context.Background(), peer.endpoint())
		if err == nil {
			if !cm.publishServerConnection(context.Background(), sc) {
				_ = sc.Close()
			}
		}
		attemptDone <- err
	})
	awaitLifecycle(t, ready, "server to stall registration acknowledgment")

	stopDone := make(chan error, 1)
	go func() { stopDone <- cm.Stop() }()
	if err := awaitLifecycle(t, stopDone, "ConnectionManager.Stop"); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if err := awaitLifecycle(t, attemptDone, "stalled attempt exit"); !errors.Is(err, context.Canceled) {
		t.Fatalf("stalled registration error = %v, want manager cancellation", err)
	}
	assertLifecycleUnpublished(t, cm)

	close(releaseAck)
	awaitLifecycle(t, ackAttempted, "late acknowledgment write")
	if err := awaitLifecycle(t, serverDone, "late acknowledgment handler"); err != nil {
		t.Fatal(err)
	}
	assertLifecycleUnpublished(t, cm)
}

func TestRegistrationLifecycleTruncatedAckClosesAttemptAndRedials(t *testing.T) {
	tests := []struct {
		name     string
		truncate func([]byte) []byte
	}{
		{
			name: "three byte header",
			truncate: func(wire []byte) []byte {
				return wire[:3]
			},
		},
		{
			name: "half payload",
			truncate: func(wire []byte) []byte {
				return wire[:5+(len(wire)-5)/2]
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			peer := newLifecyclePeer(t)
			cm := newLifecycleManager(t, peer)
			cm.attemptTimeout = 250 * time.Millisecond
			accepted := make(chan *quic.Conn, 2)

			firstDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
				accepted <- conn
				var wire bytes.Buffer
				if err := protocol.WriteRegisterAckWithAuth(
					&wire,
					true,
					"registered",
					protocol.ProtocolVersion,
					config.DefaultCapabilities,
					"",
				); err != nil {
					return err
				}
				if _, err := stream.Write(test.truncate(wire.Bytes())); err != nil {
					return fmt.Errorf("write truncated acknowledgment: %w", err)
				}
				<-conn.Context().Done()
				return nil
			})

			first, err := cm.connectAndRegister(context.Background(), peer.endpoint())
			if err == nil {
				_ = first.Close()
				t.Fatal("truncated acknowledgment unexpectedly registered")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("truncated acknowledgment error = %v, want attempt deadline", err)
			}
			if first != nil {
				t.Fatal("failed attempt returned a provisional ServerConnection")
			}
			firstQUIC := awaitLifecycle(t, accepted, "first QUIC connection")
			if err := awaitLifecycle(t, firstDone, "first provisional connection close"); err != nil {
				t.Fatal(err)
			}
			assertLifecycleUnpublished(t, cm)

			secondDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
				accepted <- conn
				return writeSuccessfulLifecycleAck(stream)
			})
			second, err := cm.connectAndRegister(context.Background(), peer.endpoint())
			if err != nil {
				t.Fatalf("registration after truncated acknowledgment failed: %v", err)
			}
			secondQUIC := awaitLifecycle(t, accepted, "fresh QUIC connection")
			if firstQUIC == secondQUIC {
				_ = second.Close()
				t.Fatal("subsequent registration reused the failed QUIC connection")
			}
			if err := second.Close(); err != nil {
				t.Fatalf("close successful second connection: %v", err)
			}
			if err := awaitLifecycle(t, secondDone, "second registration handler"); err != nil {
				t.Fatal(err)
			}
			assertLifecycleUnpublished(t, cm)
		})
	}
}

func TestRegistrationLifecycleSuccessfulAckClearsAttemptDeadline(t *testing.T) {
	peer := newLifecyclePeer(t)
	cm := newLifecycleManager(t, peer)
	cm.attemptTimeout = 300 * time.Millisecond
	const heartbeatCount = 4
	heartbeats := make(chan protocol.HeartbeatMsg, heartbeatCount)
	serverDone := peer.serveRegistration(func(_ *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(stream); err != nil {
			return err
		}
		for range heartbeatCount {
			var heartbeat protocol.HeartbeatMsg
			if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeHeartbeat, &heartbeat); err != nil {
				return fmt.Errorf("read post-registration heartbeat: %w", err)
			}
			heartbeats <- heartbeat
		}
		return nil
	})

	attemptStarted := time.Now()
	sc, err := cm.connectAndRegister(context.Background(), peer.endpoint())
	if err != nil {
		t.Fatalf("register before attempt deadline: %v", err)
	}
	defer func() { _ = sc.Close() }()

	waitPastDeadline := cm.attemptTimeout - time.Since(attemptStarted) + 50*time.Millisecond
	if waitPastDeadline > 0 {
		<-time.After(waitPastDeadline)
	}
	for i := range heartbeatCount {
		if err := sc.SendHeartbeat(); err != nil {
			t.Fatalf("heartbeat %d after attempt deadline: %v", i+1, err)
		}
	}
	for i := range heartbeatCount {
		awaitLifecycle(t, heartbeats, fmt.Sprintf("heartbeat %d after attempt deadline", i+1))
	}
	if err := awaitLifecycle(t, serverDone, "post-deadline heartbeats"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationLifecycleSuccessfulPublicationStartsHeartbeat(t *testing.T) {
	peer := newLifecyclePeer(t)
	cm := newLifecycleManager(t, peer)
	heartbeatSeen := make(chan struct{})
	serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(stream); err != nil {
			return err
		}
		var heartbeat protocol.HeartbeatMsg
		if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeHeartbeat, &heartbeat); err != nil {
			return fmt.Errorf("read published heartbeat: %w", err)
		}
		close(heartbeatSeen)
		<-conn.Context().Done()
		return nil
	})

	sc, err := cm.connectAndRegister(context.Background(), peer.endpoint())
	if err != nil {
		t.Fatalf("connect and register: %v", err)
	}
	published := make(chan bool, 1)
	cm.publishMu.Lock()
	cm.wg.Go(func() {
		committed := cm.publishServerConnection(context.Background(), sc)
		if !committed {
			_ = sc.Close()
		}
		published <- committed
	})
	cm.publishMu.Unlock()
	if !awaitLifecycle(t, published, "successful publication") {
		t.Fatal("registered connection was not published")
	}
	if got := awaitLifecycle(t, cm.NewConns, "published connection delivery"); got != sc {
		t.Fatalf("delivered connection = %p, want %p", got, sc)
	}
	awaitLifecycle(t, heartbeatSeen, "heartbeat after successful publication")
	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := awaitLifecycle(t, serverDone, "published connection close"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationLifecycleCancellationAfterDeliveryRemainsCommitted(t *testing.T) {
	peer := newLifecyclePeer(t)
	cm := newLifecycleManager(t, peer)
	cm.NewConns = make(chan *ServerConnection)
	serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(stream); err != nil {
			return err
		}
		<-conn.Context().Done()
		return nil
	})

	sc, err := cm.connectAndRegister(context.Background(), peer.endpoint())
	if err != nil {
		t.Fatalf("connect and register: %v", err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	published := make(chan bool, 1)
	cm.publishMu.Lock()
	cm.wg.Go(func() {
		committed := cm.publishServerConnection(runCtx, sc)
		if !committed {
			_ = sc.Close()
		}
		published <- committed
	})
	cm.publishMu.Unlock()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for cm.GetConnection(sc.ServerAddr()) != sc {
		select {
		case <-deadline.C:
			t.Fatal("connection did not reach NewConns delivery")
		case <-ticker.C:
		}
	}

	got := func() *ServerConnection {
		cm.publishMu.Lock()
		defer cm.publishMu.Unlock()
		delivered := awaitLifecycle(t, cm.NewConns, "published connection delivery")
		cancelRun()
		return delivered
	}()
	if got != sc {
		t.Fatalf("delivered connection = %p, want %p", got, sc)
	}

	if !awaitLifecycle(t, published, "committed publication") {
		t.Fatal("delivered connection was rolled back after run cancellation")
	}
	if got := cm.GetConnection(sc.ServerAddr()); got != sc {
		t.Fatalf("committed connection = %p, want %p", got, sc)
	}
	if !sc.IsHealthy() || sc.State() == StateDisconnected {
		t.Fatalf("delivered connection was closed before Stop: healthy=%t state=%s", sc.IsHealthy(), sc.State())
	}
	select {
	case err := <-serverDone:
		t.Fatalf("delivered connection closed before Stop: %v", err)
	default:
	}

	if err := cm.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := awaitLifecycle(t, serverDone, "manager-owned connection close"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationLifecycleAckCancelRace(t *testing.T) {
	const iterations = 200
	const batchSize = 8

	for batchStart := 0; batchStart < iterations; batchStart += batchSize {
		t.Run(fmt.Sprintf("batch-%d", batchStart/batchSize), func(t *testing.T) {
			peer := newLifecyclePeer(t)
			cm := newLifecycleManager(t, peer)
			for iteration := batchStart; iteration < min(batchStart+batchSize, iterations); iteration++ {
				ready := make(chan struct{})
				releaseAck := make(chan struct{})
				serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
					close(ready)
					<-releaseAck
					_ = writeSuccessfulLifecycleAck(stream)
					<-conn.Context().Done()
					return nil
				})

				ctx, cancel := context.WithCancel(context.Background())
				type registrationResult struct {
					connection *ServerConnection
					err        error
				}
				result := make(chan registrationResult, 1)
				go func() {
					sc, err := cm.connectAndRegister(ctx, peer.endpoint())
					result <- registrationResult{connection: sc, err: err}
				}()
				select {
				case <-ready:
				case outcome := <-result:
					t.Fatalf("race registration %d ended before server readiness: %v", iteration, outcome.err)
				case <-time.After(30 * time.Second):
					t.Fatalf("timed out waiting for race registration %d", iteration)
				}

				start := make(chan struct{})
				var racers sync.WaitGroup
				racers.Add(2)
				go func() {
					defer racers.Done()
					<-start
					cancel()
				}()
				go func() {
					defer racers.Done()
					<-start
					close(releaseAck)
				}()
				close(start)
				racers.Wait()

				outcome := awaitLifecycle(t, result, fmt.Sprintf("Ack/cancel race %d", iteration))
				if outcome.err != nil {
					if !errors.Is(outcome.err, context.Canceled) {
						t.Fatalf("race %d returned non-cancellation error: %v", iteration, outcome.err)
					}
					if outcome.connection != nil {
						t.Fatalf("race %d returned a provisional connection with error", iteration)
					}
				} else {
					if outcome.connection == nil {
						t.Fatalf("race %d succeeded without a connection", iteration)
					}
					if err := outcome.connection.Close(); err != nil {
						t.Fatalf("race %d close winning connection: %v", iteration, err)
					}
				}
				if err := awaitLifecycle(t, serverDone, fmt.Sprintf("race connection %d close", iteration)); err != nil {
					t.Fatal(err)
				}
				assertLifecycleUnpublished(t, cm)
			}
		})
	}
}

func TestRegistrationLifecycleRepeatedFailuresCloseProvisionalConnections(t *testing.T) {
	peer := newLifecyclePeer(t)
	cm := newLifecycleManager(t, peer)
	const attempts = 100
	seen := make(map[*quic.Conn]struct{}, attempts)

	for attempt := range attempts {
		accepted := make(chan *quic.Conn, 1)
		serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
			accepted <- conn
			if err := protocol.WriteRegisterAckWithAuth(
				stream,
				false,
				"test rejection",
				protocol.ProtocolVersion,
				nil,
				"",
			); err != nil {
				return err
			}
			<-conn.Context().Done()
			return nil
		})

		sc, err := cm.connectAndRegister(context.Background(), peer.endpoint())
		if err == nil {
			_ = sc.Close()
			t.Fatalf("attempt %d unexpectedly accepted rejection", attempt+1)
		}
		if sc != nil {
			t.Fatalf("attempt %d returned rejected provisional connection", attempt+1)
		}
		conn := awaitLifecycle(t, accepted, fmt.Sprintf("rejected QUIC connection %d", attempt+1))
		if _, duplicate := seen[conn]; duplicate {
			t.Fatalf("attempt %d reused a prior QUIC connection", attempt+1)
		}
		seen[conn] = struct{}{}
		if err := awaitLifecycle(t, serverDone, fmt.Sprintf("rejected QUIC connection %d close", attempt+1)); err != nil {
			t.Fatal(err)
		}
		assertLifecycleUnpublished(t, cm)
	}
}
