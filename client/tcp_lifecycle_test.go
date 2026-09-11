package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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

func readClientNewConnAck(stream *quic.Stream, connID uint64) error {
	var ack protocol.NewConnAckMsg
	if err := protocol.ReadTypedMessageLimited(stream, protocol.MsgTypeNewConnAck, &ack, protocol.MaxNewConnAckPayloadSize); err != nil {
		return fmt.Errorf("read NewConn acknowledgment: %w", err)
	}
	if ack.ConnID != connID {
		return fmt.Errorf("NewConn acknowledgment ID = %d, want %d", ack.ConnID, connID)
	}
	return nil
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
		copyBufferPool: protocol.NewCopyBufferPool(protocol.DefaultCopyBufferSize),
		logger:         zerolog.Nop(),
	}
	handlerDone := make(chan struct{})
	go func() {
		c.handleStream(testCtx, clientStream, &ServerConnection{serverAddr: "relay-test"})
		close(handlerDone)
	}()
	if err := readClientNewConnAck(peerStream, 1); err != nil {
		t.Fatal(err)
	}

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
		config:         &config.Client{Local: config.LocalService{Host: "127.0.0.1", Port: backendPort}},
		copyBufferPool: protocol.NewCopyBufferPool(protocol.DefaultCopyBufferSize),
		logger:         zerolog.Nop(),
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

func TestClientRejectsConcurrentOversizedNewConnPayloads(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()

	backend, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen for oversized NewConn backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	clientQUIC, peerQUIC := newClientRelayQUICPair(t, testCtx)
	c := &Client{
		config: &config.Client{Local: config.LocalService{
			Host: "127.0.0.1",
			Port: backend.Addr().(*net.TCPAddr).Port,
		}},
		copyBufferPool: protocol.NewCopyBufferPool(protocol.DefaultCopyBufferSize),
		logger:         zerolog.Nop(),
	}
	sc := &ServerConnection{serverAddr: "relay-test"}

	const streamCount = 4
	peerStreams := make([]*quic.Stream, 0, streamCount)
	clientStreams := make([]*quic.Stream, 0, streamCount)
	var header [5]byte
	header[0] = protocol.MsgTypeNewConn
	binary.BigEndian.PutUint32(header[1:], protocol.MaxNewConnPayloadSize+1)
	for i := range streamCount {
		peerStream, err := peerQUIC.OpenStreamSync(testCtx)
		if err != nil {
			t.Fatalf("open oversized peer stream %d: %v", i, err)
		}
		if n, err := peerStream.Write(header[:]); err != nil || n != len(header) {
			t.Fatalf("write oversized NewConn header %d = (%d, %v)", i, n, err)
		}
		peerStreams = append(peerStreams, peerStream)
	}
	for i := range streamCount {
		clientStream, err := clientQUIC.AcceptStream(testCtx)
		if err != nil {
			t.Fatalf("accept oversized client stream %d: %v", i, err)
		}
		clientStreams = append(clientStreams, clientStream)
	}

	start := make(chan struct{})
	handlerDone := make([]chan struct{}, streamCount)
	for i, stream := range clientStreams {
		handlerDone[i] = make(chan struct{})
		go func() {
			<-start
			c.handleStream(testCtx, stream, sc)
			close(handlerDone[i])
		}()
	}
	if err := backend.SetDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatalf("set oversized backend accept deadline: %v", err)
	}
	oversizedAccept := make(chan error, 1)
	go func() {
		conn, acceptErr := backend.AcceptTCP()
		if acceptErr == nil {
			_ = conn.Close()
			acceptErr = errors.New("oversized NewConn dialed backend")
		}
		oversizedAccept <- acceptErr
	}()
	close(start)

	for i, peerStream := range peerStreams {
		if err := peerStream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set oversized stream %d read deadline: %v", i, err)
		}
		var response [1]byte
		n, readErr := peerStream.Read(response[:])
		var streamErr *quic.StreamError
		if n != 0 || !errors.As(readErr, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != 0 || streamErr.StreamID != peerStream.StreamID() {
			t.Fatalf("oversized stream %d read = (%d, %T %v), want no Ack and remote reset for stream %d code 0", i, n, readErr, readErr, peerStream.StreamID())
		}

		select {
		case <-peerStream.Context().Done():
		case <-time.After(2 * time.Second):
			t.Fatalf("oversized stream %d did not receive STOP_SENDING", i)
		}
		streamErr = nil
		if cause := context.Cause(peerStream.Context()); !errors.As(cause, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != 0 || streamErr.StreamID != peerStream.StreamID() {
			t.Fatalf("oversized stream %d send context cause = %T %v, want remote STOP_SENDING for stream %d code 0", i, cause, cause, peerStream.StreamID())
		}
		n, writeErr := peerStream.Write([]byte("must fail"))
		streamErr = nil
		if n != 0 || !errors.As(writeErr, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != 0 || streamErr.StreamID != peerStream.StreamID() {
			t.Fatalf("oversized stream %d future write = (%d, %T %v), want remote STOP_SENDING for stream %d code 0", i, n, writeErr, writeErr, peerStream.StreamID())
		}
	}
	for i, done := range handlerDone {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("oversized NewConn handler %d did not join", i)
		}
	}
	if pending, active := c.tcpPending.Load(), c.tcpActive.Load(); pending != 0 || active != 0 {
		t.Fatalf("TCP accounting after oversized NewConn = pending %d, active %d; want 0, 0", pending, active)
	}
	select {
	case acceptErr := <-oversizedAccept:
		var netErr net.Error
		if !errors.As(acceptErr, &netErr) || !netErr.Timeout() {
			t.Fatalf("oversized NewConn backend accept error = %v, want timeout", acceptErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("oversized NewConn backend accept did not finish")
	}

	if err := backend.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("clear backend accept deadline: %v", err)
	}
	type acceptResult struct {
		conn *net.TCPConn
		err  error
	}
	legalAccept := make(chan acceptResult, 1)
	go func() {
		conn, acceptErr := backend.AcceptTCP()
		legalAccept <- acceptResult{conn: conn, err: acceptErr}
	}()
	legalPeer, err := peerQUIC.OpenStreamSync(testCtx)
	if err != nil {
		t.Fatalf("open legal peer stream after oversized NewConn: %v", err)
	}
	if err := protocol.WriteNewConn(legalPeer, 99, "tcp", "peer", "local", time.Now().Unix()); err != nil {
		t.Fatalf("write legal NewConn after oversized NewConn: %v", err)
	}
	legalClient, err := clientQUIC.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("accept legal client stream after oversized NewConn: %v", err)
	}
	legalDone := make(chan struct{})
	go func() {
		c.handleStream(testCtx, legalClient, sc)
		close(legalDone)
	}()
	if err := readClientNewConnAck(legalPeer, 99); err != nil {
		t.Fatal(err)
	}
	var backendConn *net.TCPConn
	select {
	case result := <-legalAccept:
		if result.err != nil {
			t.Fatalf("accept legal backend connection: %v", result.err)
		}
		backendConn = result.conn
	case <-time.After(2 * time.Second):
		t.Fatal("legal NewConn did not dial backend")
	}
	if err := legalPeer.Close(); err != nil {
		t.Fatalf("close legal peer send side: %v", err)
	}
	if err := backendConn.Close(); err != nil {
		t.Fatalf("close legal backend connection: %v", err)
	}
	select {
	case <-legalDone:
	case <-time.After(2 * time.Second):
		t.Fatal("legal NewConn handler did not join")
	}
	if pending, active := c.tcpPending.Load(), c.tcpActive.Load(); pending != 0 || active != 0 {
		t.Fatalf("final TCP accounting = pending %d, active %d; want 0, 0", pending, active)
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
		copyBufferPool: protocol.NewCopyBufferPool(protocol.DefaultCopyBufferSize),
		logger:         zerolog.Nop(),
	}
	handlerDone := make(chan struct{})
	go func() {
		c.handleStream(flowCtx, clientStream, &ServerConnection{serverAddr: "relay-test"})
		close(handlerDone)
	}()
	if err := readClientNewConnAck(peerStream, connID); err != nil {
		t.Fatal(err)
	}

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
