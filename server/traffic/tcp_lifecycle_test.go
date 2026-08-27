package traffic

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const relayLifecycleTestALPN = "qmux-traffic-relay-lifecycle-test"

type netErrClosedListener struct {
	calls int
}

func (l *netErrClosedListener) Accept() (net.Conn, error) {
	l.calls++
	return nil, net.ErrClosed
}

func (*netErrClosedListener) Close() error   { return nil }
func (*netErrClosedListener) Addr() net.Addr { return testNetAddr("closed-listener") }

type testNetAddr string

func (a testNetAddr) Network() string { return "test" }
func (a testNetAddr) String() string  { return string(a) }

func TestAcceptTCPReturnsOnNetErrClosed(t *testing.T) {
	stub := &netErrClosedListener{}
	listener := &Listener{
		TCPListener: stub,
		ctx:         context.Background(),
		logger:      zerolog.Nop(),
	}
	listener.fixedWG.Add(1)
	done := make(chan struct{})
	go func() {
		listener.acceptTCP()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("acceptTCP did not return after net.ErrClosed")
	}
	if stub.calls != 1 {
		t.Fatalf("Accept calls = %d, want 1", stub.calls)
	}
}

func relayLifecycleTLSConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate relay test key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "QMux relay lifecycle test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create relay test certificate: %v", err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{relayLifecycleTestALPN},
	}, &tls.Config{
		// This certificate is generated solely for the in-process test listener.
		InsecureSkipVerify: true, //nolint:gosec
		NextProtos:         []string{relayLifecycleTestALPN},
	}
}

type acceptedQUICConn struct {
	conn *quic.Conn
	err  error
}

func newRelayLifecycleQUICPair(t *testing.T, ctx context.Context, streamReceiveWindow uint64) (*quic.Listener, *quic.Conn, *quic.Conn) {
	t.Helper()
	serverTLS, peerTLS := relayLifecycleTLSConfigs(t)
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       10 * time.Second,
	})
	if err != nil {
		t.Fatalf("listen for relay lifecycle QUIC pair: %v", err)
	}

	accepted := make(chan acceptedQUICConn, 1)
	go func() {
		conn, acceptErr := listener.Accept(ctx)
		accepted <- acceptedQUICConn{conn: conn, err: acceptErr}
	}()
	peerConn, err := quic.DialAddr(ctx, listener.Addr().String(), peerTLS, &quic.Config{
		HandshakeIdleTimeout:           5 * time.Second,
		MaxIdleTimeout:                 10 * time.Second,
		InitialStreamReceiveWindow:     streamReceiveWindow,
		MaxStreamReceiveWindow:         streamReceiveWindow,
		InitialConnectionReceiveWindow: 2 * streamReceiveWindow,
		MaxConnectionReceiveWindow:     2 * streamReceiveWindow,
	})
	if err != nil {
		if closeErr := listener.Close(); closeErr != nil {
			t.Fatalf("dial relay lifecycle QUIC pair: %v; close listener: %v", err, closeErr)
		}
		t.Fatalf("dial relay lifecycle QUIC pair: %v", err)
	}

	select {
	case result := <-accepted:
		if result.err != nil {
			peerCloseErr := peerConn.CloseWithError(0, "accept failed")
			listenerCloseErr := listener.Close()
			t.Fatalf(
				"accept relay lifecycle QUIC pair: %v; close peer: %v; close listener: %v",
				result.err,
				peerCloseErr,
				listenerCloseErr,
			)
		}
		return listener, result.conn, peerConn
	case <-ctx.Done():
		peerCloseErr := peerConn.CloseWithError(0, "accept timeout")
		listenerCloseErr := listener.Close()
		t.Fatalf(
			"accept relay lifecycle QUIC pair: %v; close peer: %v; close listener: %v",
			context.Cause(ctx),
			peerCloseErr,
			listenerCloseErr,
		)
	}
	return nil, nil, nil
}

func registerRelayLifecycleQUICCleanup(
	t *testing.T,
	listener *quic.Listener,
	serverConn *quic.Conn,
	peerConn *quic.Conn,
) {
	t.Helper()
	t.Cleanup(func() {
		if err := peerConn.CloseWithError(0, "test complete"); err != nil {
			t.Errorf("close relay QUIC peer: %v", err)
		}
		if err := serverConn.CloseWithError(0, "test complete"); err != nil {
			t.Errorf("close relay QUIC server connection: %v", err)
		}
		if err := listener.Close(); err != nil {
			t.Errorf("close relay QUIC listener: %v", err)
		}
	})
}

func waitForActiveConnections(t *testing.T, client *pool.ClientConn, want int64) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for client.ActiveConns.Load() != want {
		select {
		case <-deadline.C:
			t.Fatalf("active connections = %d, want %d", client.ActiveConns.Load(), want)
		case <-ticker.C:
		}
	}
}

func startRelayLifecycleManager(
	t *testing.T,
	quicListener *quic.Listener,
	serverConn *quic.Conn,
	clientID string,
) (*Manager, *pool.ClientConn) {
	t.Helper()
	quicAddr := quicListener.Addr().String()
	connectionPool := pool.New(quicAddr, pool.NewRoundRobinBalancer(), zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	pooledClient := &pool.ClientConn{
		ID:   clientID,
		Conn: serverConn,
		Metadata: pool.ClientMetadata{
			Capabilities: []string{"tcp"},
		},
	}
	if err := connectionPool.Add(pooledClient); err != nil {
		t.Fatalf("add relay peer to pool: %v", err)
	}

	managerCtx, cancelManager := context.WithCancel(context.Background())
	manager := NewManager(&config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    quicAddr,
		TrafficAddr: "127.0.0.1:0",
		Protocol:    "tcp",
	}}}, map[string]*pool.ConnectionPool{quicAddr: connectionPool}, zerolog.Nop())
	t.Cleanup(func() {
		cancelManager()
		manager.Stop()
	})
	if err := manager.Start(managerCtx); err != nil {
		t.Fatalf("start traffic manager: %v", err)
	}
	return manager, pooledClient
}

func openRelayLifecycleFlow(
	t *testing.T,
	ctx context.Context,
	manager *Manager,
	peerConn *quic.Conn,
) (*net.TCPConn, *quic.Stream) {
	t.Helper()
	rawTCPConn, err := net.DialTimeout("tcp", manager.listeners[0].TCPListener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial traffic listener: %v", err)
	}
	tcpConn := rawTCPConn.(*net.TCPConn)
	t.Cleanup(func() {
		if err := tcpConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close relay TCP client: %v", err)
		}
	})
	if err := tcpConn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatalf("set traffic TCP deadline: %v", err)
	}

	peerStream, err := peerConn.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("accept traffic QUIC stream: %v", err)
	}
	if err := peerStream.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatalf("set peer stream deadline: %v", err)
	}
	var newConn protocol.NewConnMsg
	if err := protocol.ReadTypedMessage(peerStream, protocol.MsgTypeNewConn, &newConn); err != nil {
		t.Fatalf("read NewConn message: %v", err)
	}
	return tcpConn, peerStream
}

func TestTCPRelayPublicFirstHalfCloseDeliversDelayedResponse(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 256*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)

	manager, pooledClient := startRelayLifecycleManager(t, quicListener, serverConn, "relay-peer")
	tcpConn, peerStream := openRelayLifecycleFlow(t, testCtx, manager, peerConn)
	waitForActiveConnections(t, pooledClient, 1)

	payload := bytes.Repeat([]byte("graceful-relay-payload-"), 4096)
	written, err := io.Copy(tcpConn, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("write request payload: %v", err)
	}
	if written != int64(len(payload)) {
		t.Fatalf("request bytes written = %d, want %d", written, len(payload))
	}
	if err := tcpConn.CloseWrite(); err != nil {
		t.Fatalf("half-close request TCP write side: %v", err)
	}

	received, err := io.ReadAll(peerStream)
	if err != nil {
		t.Fatalf("read request payload: %v", err)
	}
	if !bytes.Equal(received, payload) {
		gotHash := sha256.Sum256(received)
		wantHash := sha256.Sum256(payload)
		t.Fatalf(
			"received payload mismatch: got len=%d sha256=%x, want len=%d sha256=%x; first mismatch at byte %d",
			len(received),
			gotHash,
			len(payload),
			wantHash,
			firstMismatchOffset(received, payload),
		)
	}
	// Cover COR-001's delayed-response case: the handler must stay active while
	// the peer waits after receiving the request FIN.
	time.Sleep(100 * time.Millisecond)
	if active := pooledClient.ActiveConns.Load(); active != 1 {
		t.Fatalf("active connections before response FIN = %d, want 1", active)
	}

	response := bytes.Repeat([]byte("delayed-response-payload-"), 4096)
	written, err = io.Copy(peerStream, bytes.NewReader(response))
	if err != nil {
		t.Fatalf("write delayed response: %v", err)
	}
	if written != int64(len(response)) {
		t.Fatalf("response bytes written = %d, want %d", written, len(response))
	}
	if err := peerStream.Close(); err != nil {
		t.Fatalf("half-close response QUIC write side: %v", err)
	}
	received, err = io.ReadAll(tcpConn)
	if err != nil {
		t.Fatalf("read delayed response: %v", err)
	}
	if !bytes.Equal(received, response) {
		t.Fatalf(
			"response payload mismatch: got len=%d, want len=%d; first mismatch at byte %d",
			len(received),
			len(response),
			firstMismatchOffset(received, response),
		)
	}
	waitForActiveConnections(t, pooledClient, 0)

	manager.Close()
	waitManager(t, manager)
}

func TestTCPRelayPeerFirstHalfCloseKeepsRequestDirectionOpen(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 256*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)

	manager, pooledClient := startRelayLifecycleManager(t, quicListener, serverConn, "peer-first-relay")
	tcpConn, peerStream := openRelayLifecycleFlow(t, testCtx, manager, peerConn)
	waitForActiveConnections(t, pooledClient, 1)

	response := []byte("peer-first-response")
	if _, err := peerStream.Write(response); err != nil {
		t.Fatalf("write peer-first response: %v", err)
	}
	if err := peerStream.Close(); err != nil {
		t.Fatalf("half-close peer QUIC write side: %v", err)
	}
	received, err := io.ReadAll(tcpConn)
	if err != nil {
		t.Fatalf("read peer-first response: %v", err)
	}
	if !bytes.Equal(received, response) {
		t.Fatalf("response payload mismatch: got %q, want %q", received, response)
	}
	if active := pooledClient.ActiveConns.Load(); active != 1 {
		t.Fatalf("active connections before request FIN = %d, want 1", active)
	}

	request := []byte("request-after-peer-fin")
	if _, err := tcpConn.Write(request); err != nil {
		t.Fatalf("write request after peer FIN: %v", err)
	}
	if err := tcpConn.CloseWrite(); err != nil {
		t.Fatalf("half-close request TCP write side: %v", err)
	}
	received, err = io.ReadAll(peerStream)
	if err != nil {
		t.Fatalf("read request after peer FIN: %v", err)
	}
	if !bytes.Equal(received, request) {
		t.Fatalf("request payload mismatch: got %q, want %q", received, request)
	}
	waitForActiveConnections(t, pooledClient, 0)

	manager.Close()
	waitManager(t, manager)
}

func TestTCPRelayPeerResetAbortsBlockedSibling(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 256*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)

	manager, pooledClient := startRelayLifecycleManager(t, quicListener, serverConn, "reset-relay-peer")
	_, peerStream := openRelayLifecycleFlow(t, testCtx, manager, peerConn)
	waitForActiveConnections(t, pooledClient, 1)

	const peerResetCode quic.StreamErrorCode = 42
	peerStream.CancelWrite(peerResetCode)
	waitForActiveConnections(t, pooledClient, 0)

	manager.Close()
	waitManager(t, manager)
}

func firstMismatchOffset(got, want []byte) int {
	limit := min(len(got), len(want))
	for i := range limit {
		if got[i] != want[i] {
			return i
		}
	}
	return limit
}

type relayWriteResult struct {
	written int64
	err     error
}

func TestTCPRelayManagerShutdownAbortsFlowControlBlockedSend(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelTest()
	const streamReceiveWindow = 32 * 1024
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, streamReceiveWindow)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)

	manager, pooledClient := startRelayLifecycleManager(t, quicListener, serverConn, "blocked-relay-peer")

	trafficAddr := manager.listeners[0].TCPListener.Addr().String()
	rawTCPConn, err := net.DialTimeout("tcp", trafficAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial traffic listener: %v", err)
	}
	tcpConn := rawTCPConn.(*net.TCPConn)
	t.Cleanup(func() {
		if err := tcpConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close blocked relay TCP client: %v", err)
		}
	})
	if err := tcpConn.SetWriteBuffer(32 * 1024); err != nil {
		t.Fatalf("set traffic TCP write buffer: %v", err)
	}
	if err := tcpConn.SetWriteDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatalf("set traffic TCP write deadline: %v", err)
	}

	peerStream, err := peerConn.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("accept blocked traffic QUIC stream: %v", err)
	}
	var newConn protocol.NewConnMsg
	if err := protocol.ReadTypedMessage(peerStream, protocol.MsgTypeNewConn, &newConn); err != nil {
		t.Fatalf("read blocked relay NewConn message: %v", err)
	}
	waitForActiveConnections(t, pooledClient, 1)

	payload := bytes.Repeat([]byte("flow-control-blocked-relay-"), (8*1024*1024)/len("flow-control-blocked-relay-")+1)
	payload = payload[:8*1024*1024]
	writerStarted := make(chan struct{})
	writerDone := make(chan relayWriteResult, 1)
	go func() {
		close(writerStarted)
		written, writeErr := io.Copy(tcpConn, bytes.NewReader(payload))
		writerDone <- relayWriteResult{written: written, err: writeErr}
	}()
	<-writerStarted

	select {
	case result := <-writerDone:
		t.Fatalf(
			"TCP writer completed before shutdown despite fixed %d-byte QUIC window: wrote %d/%d bytes: %v",
			streamReceiveWindow,
			result.written,
			len(payload),
			result.err,
		)
	case <-time.After(200 * time.Millisecond):
	}
	if active := pooledClient.ActiveConns.Load(); active != 1 {
		t.Fatalf("active connections after blocked-write observation = %d, want 1", active)
	}

	shutdownDone := make(chan struct{})
	go func() {
		manager.Close()
		manager.Wait()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(3 * time.Second):
		closeErr := peerConn.CloseWithError(1, "unblock timed out traffic manager shutdown")
		select {
		case <-shutdownDone:
			t.Fatal("traffic manager Wait remained blocked by flow-controlled QUIC send")
		case <-time.After(3 * time.Second):
			t.Fatalf(
				"traffic manager Wait remained blocked by flow-controlled QUIC send and did not finish after closing the peer connection: %v",
				closeErr,
			)
		}
	}

	select {
	case <-writerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("TCP writer did not terminate after traffic manager shutdown")
	}
	if active := pooledClient.ActiveConns.Load(); active != 0 {
		t.Fatalf("active connections after Manager.Wait = %d, want 0", active)
	}

	if err := peerStream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set blocked peer stream read deadline: %v", err)
	}
	received, readErr := io.ReadAll(peerStream)
	var streamErr *quic.StreamError
	if !errors.As(readErr, &streamErr) {
		t.Fatalf("blocked peer read error = %v after %d payload bytes, want QUIC stream reset", readErr, len(received))
	}
	if !streamErr.Remote || streamErr.ErrorCode != trafficStreamCancelCode || streamErr.StreamID != peerStream.StreamID() {
		t.Fatalf(
			"blocked peer stream reset = {stream=%d code=%d remote=%t}, want {stream=%d code=%d remote=true}",
			streamErr.StreamID,
			streamErr.ErrorCode,
			streamErr.Remote,
			peerStream.StreamID(),
			trafficStreamCancelCode,
		)
	}
	if len(received) >= len(payload) {
		t.Fatalf("peer received %d bytes before reset, want fewer than full %d-byte payload", len(received), len(payload))
	}
}
