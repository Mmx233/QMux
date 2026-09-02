package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

func newClientRelayQUICPair(t *testing.T, ctx context.Context) (*quic.Conn, *quic.Conn) {
	t.Helper()
	peer := newLifecyclePeer(t)
	accepted := make(chan struct {
		conn *quic.Conn
		err  error
	}, 1)
	go func() {
		conn, err := peer.listener.Accept(ctx)
		accepted <- struct {
			conn *quic.Conn
			err  error
		}{conn, err}
	}()

	clientConn, err := quic.DialAddr(ctx, peer.listener.Addr().String(), peer.clientTLS, &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       10 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial client relay QUIC peer: %v", err)
	}
	result := <-accepted
	if result.err != nil {
		t.Fatalf("accept client relay QUIC connection: %v", result.err)
	}
	t.Cleanup(func() {
		_ = clientConn.CloseWithError(0, "test complete")
		_ = result.conn.CloseWithError(0, "test complete")
	})
	return clientConn, result.conn
}

func TestClientTCPRelayDeliversResponseAfterRequestFIN(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()

	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen for delayed-response backend: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	request := []byte("request completed before response")
	response := []byte("response after request FIN")
	backendDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptTCP()
		if acceptErr != nil {
			backendDone <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		got, readErr := io.ReadAll(conn)
		if readErr != nil {
			backendDone <- readErr
			return
		}
		if !bytes.Equal(got, request) {
			backendDone <- errors.New("backend received an incomplete request")
			return
		}
		_, writeErr := conn.Write(response)
		backendDone <- writeErr
	}()

	clientQUIC, peerQUIC := newClientRelayQUICPair(t, testCtx)
	peerStream, err := peerQUIC.OpenStreamSync(testCtx)
	if err != nil {
		t.Fatalf("open delayed-response peer stream: %v", err)
	}
	if err := protocol.WriteNewConn(peerStream, 1, "tcp", "peer", "local", time.Now().Unix()); err != nil {
		t.Fatalf("write NewConn: %v", err)
	}
	if _, err := peerStream.Write(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := peerStream.Close(); err != nil {
		t.Fatalf("close request send side: %v", err)
	}
	clientStream, err := clientQUIC.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("accept delayed-response client stream: %v", err)
	}

	c := &Client{
		config: &config.Client{Local: config.LocalService{
			Host: "127.0.0.1",
			Port: listener.Addr().(*net.TCPAddr).Port,
		}},
		logger: zerolog.Nop(),
	}
	handlerDone := make(chan struct{})
	go func() {
		c.handleStream(testCtx, clientStream, &ServerConnection{serverAddr: "relay-test"})
		close(handlerDone)
	}()

	if err := peerStream.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set response deadline: %v", err)
	}
	got, err := io.ReadAll(peerStream)
	if err != nil {
		t.Fatalf("read delayed response: %v", err)
	}
	if !bytes.Equal(got, response) {
		t.Fatalf("delayed response = %q, want %q", got, response)
	}
	if err := <-backendDone; err != nil {
		t.Fatalf("delayed-response backend: %v", err)
	}
	select {
	case <-handlerDone:
	case <-testCtx.Done():
		t.Fatal("client relay did not finish after both FINs")
	}
}

func TestClientTCPSetupFailureResetsBothStreamDirections(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()

	backend, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("reserve unavailable backend port: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	backendPort := backend.Addr().(*net.TCPAddr).Port

	clientQUIC, peerQUIC := newClientRelayQUICPair(t, testCtx)
	peerStream, err := peerQUIC.OpenStreamSync(testCtx)
	if err != nil {
		t.Fatalf("open setup-failure peer stream: %v", err)
	}
	if err := protocol.WriteNewConn(peerStream, 1, "tcp", "peer", "local", time.Now().Unix()); err != nil {
		t.Fatalf("write setup-failure NewConn: %v", err)
	}
	clientStream, err := clientQUIC.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("accept setup-failure client stream: %v", err)
	}

	c := &Client{
		config: &config.Client{Local: config.LocalService{Host: "127.0.0.1", Port: backendPort}},
		logger: zerolog.Nop(),
	}
	handlerDone := make(chan struct{})
	if err := backend.Close(); err != nil {
		t.Fatalf("release unavailable backend port: %v", err)
	}
	go func() {
		c.handleStream(testCtx, clientStream, &ServerConnection{serverAddr: "relay-test"})
		close(handlerDone)
	}()

	if err := peerStream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set setup-failure read deadline: %v", err)
	}
	var response [1]byte
	n, readErr := peerStream.Read(response[:])
	var streamErr *quic.StreamError
	if n != 0 || !errors.As(readErr, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != 0 || streamErr.StreamID != peerStream.StreamID() {
		t.Fatalf("setup-failure read = (%d, %T %v), want no bytes and remote reset for stream %d code 0", n, readErr, readErr, peerStream.StreamID())
	}

	select {
	case <-peerStream.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("setup-failure peer did not receive STOP_SENDING")
	}
	streamErr = nil
	if cause := context.Cause(peerStream.Context()); !errors.As(cause, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != 0 || streamErr.StreamID != peerStream.StreamID() {
		t.Fatalf("setup-failure send context cause = %T %v, want remote STOP_SENDING for stream %d code 0", cause, cause, peerStream.StreamID())
	}
	n, writeErr := peerStream.Write([]byte("must fail"))
	streamErr = nil
	if n != 0 || !errors.As(writeErr, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != 0 || streamErr.StreamID != peerStream.StreamID() {
		t.Fatalf("setup-failure future write = (%d, %T %v), want remote STOP_SENDING for stream %d code 0", n, writeErr, writeErr, peerStream.StreamID())
	}

	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("setup-failure handler did not join")
	}
	select {
	case <-clientQUIC.Context().Done():
		t.Fatalf("setup failure closed client QUIC connection: %v", context.Cause(clientQUIC.Context()))
	case <-peerQUIC.Context().Done():
		t.Fatalf("setup failure closed peer QUIC connection: %v", context.Cause(peerQUIC.Context()))
	default:
	}
	probeStream, err := peerQUIC.OpenStreamSync(testCtx)
	if err != nil {
		t.Fatalf("open probe stream after setup failure: %v", err)
	}
	if _, err := probeStream.Write([]byte{1}); err != nil {
		t.Fatalf("write probe stream after setup failure: %v", err)
	}
	clientProbe, err := clientQUIC.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("accept probe stream after setup failure: %v", err)
	}
	var probe [1]byte
	if _, err := io.ReadFull(clientProbe, probe[:]); err != nil {
		t.Fatalf("read probe stream after setup failure: %v", err)
	}
}

type blockedClientRelay struct {
	peerStream  *quic.Stream
	handlerDone <-chan struct{}
	backendDone <-chan error
}

func newBlockedClientRelay(
	t *testing.T,
	testCtx context.Context,
	flowCtx context.Context,
	connID uint64,
) blockedClientRelay {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen for blocked backend: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	backendDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptTCP()
		if acceptErr != nil {
			backendDone <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		_, readErr := io.Copy(io.Discard, conn)
		backendDone <- readErr
	}()

	clientQUIC, peerQUIC := newClientRelayQUICPair(t, testCtx)
	peerStream, err := peerQUIC.OpenStreamSync(testCtx)
	if err != nil {
		t.Fatalf("open blocked peer stream: %v", err)
	}
	if err := protocol.WriteNewConn(peerStream, connID, "tcp", "peer", "local", time.Now().Unix()); err != nil {
		t.Fatalf("write blocked NewConn: %v", err)
	}
	clientStream, err := clientQUIC.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("accept blocked client stream: %v", err)
	}

	c := &Client{
		config: &config.Client{Local: config.LocalService{
			Host: "127.0.0.1",
			Port: listener.Addr().(*net.TCPAddr).Port,
		}},
		logger: zerolog.Nop(),
	}
	handlerDone := make(chan struct{})
	go func() {
		c.handleStream(flowCtx, clientStream, &ServerConnection{serverAddr: "relay-test"})
		close(handlerDone)
	}()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, active := c.localConns.Load(connID); active {
			break
		}
		select {
		case <-handlerDone:
			t.Fatal("client handler stopped before blocked relay became active")
		case <-deadline.C:
			t.Fatal("client relay did not connect to blocked backend")
		case <-ticker.C:
		}
	}
	return blockedClientRelay{peerStream, handlerDone, backendDone}
}

func (relay blockedClientRelay) wait(t *testing.T) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	select {
	case <-relay.handlerDone:
	case <-deadline.C:
		t.Fatal("client relay did not join blocked copies")
	}
	select {
	case err := <-relay.backendDone:
		if err != nil {
			t.Fatalf("blocked backend read: %v", err)
		}
	case <-deadline.C:
		t.Fatal("blocked backend connection remained open")
	}
}

func TestClientTCPRelayContextCancelJoinsBlockedCopies(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()
	flowCtx, cancelFlow := context.WithCancel(testCtx)
	relay := newBlockedClientRelay(t, testCtx, flowCtx, 2)

	cancelFlow()
	relay.wait(t)
}

func TestClientTCPRelayPeerResetAbortsBlockedLocalRead(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()
	relay := newBlockedClientRelay(t, testCtx, testCtx, 3)

	relay.peerStream.CancelWrite(quic.StreamErrorCode(42))
	relay.wait(t)
}
