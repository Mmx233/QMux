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
	runtime := &connectionRuntime{}
	handlerDone := make(chan struct{})
	go func() {
		c.handleStream(testCtx, clientStream, &ServerConnection{serverAddr: "relay-test"}, runtime)
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
	runtime := &connectionRuntime{}
	handlerDone := make(chan struct{})
	if err := backend.Close(); err != nil {
		t.Fatalf("release unavailable backend port: %v", err)
	}
	go func() {
		c.handleStream(testCtx, clientStream, &ServerConnection{serverAddr: "relay-test"}, runtime)
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
	runtime := &connectionRuntime{}

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
			c.handleStream(testCtx, stream, sc, runtime)
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
		c.handleStream(testCtx, legalClient, sc, runtime)
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

func TestClientTCPRuntimeOwnershipIsolatedAcrossServers(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelTest()
	testDeadline, ok := testCtx.Deadline()
	if !ok {
		t.Fatal("runtime ownership test context has no deadline")
	}

	backend, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen for runtime ownership backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	peers := []*lifecyclePeer{newLifecycleStartPeer(t), newLifecycleStartPeer(t)}
	type peerFlow struct {
		endpoint string
		marker   byte
		stream   *quic.Stream
	}
	flowReady := make(chan peerFlow, len(peers))
	peerDone := make([]<-chan error, len(peers))
	for i, peer := range peers {
		endpoint := peer.endpoint().Address
		marker := byte('A' + i)
		peerDone[i] = peer.serveRegistration(func(conn *quic.Conn, control *quic.Stream, _ protocol.RegisterMsg) error {
			if err := writeSuccessfulLifecycleAck(control); err != nil {
				return err
			}
			stream, err := conn.OpenStreamSync(testCtx)
			if err != nil {
				return fmt.Errorf("open %s TCP stream: %w", endpoint, err)
			}
			if err := stream.SetDeadline(testDeadline); err != nil {
				return fmt.Errorf("set %s TCP stream deadline: %w", endpoint, err)
			}
			if err := protocol.WriteNewConn(stream, 1, "tcp", endpoint, "local", time.Now().Unix()); err != nil {
				return fmt.Errorf("write %s NewConn: %w", endpoint, err)
			}
			if err := readClientNewConnAck(stream, 1); err != nil {
				return fmt.Errorf("%s: %w", endpoint, err)
			}
			if _, err := stream.Write([]byte{marker}); err != nil {
				return fmt.Errorf("write %s marker: %w", endpoint, err)
			}
			flowReady <- peerFlow{endpoint: endpoint, marker: marker, stream: stream}
			<-conn.Context().Done()
			return nil
		})
	}

	c := newClientLifecycleClient(t, "same-conn-id-runtime-ownership", peers[0].endpoint(), peers[1].endpoint())
	c.config.Local.Port = backend.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { _ = c.Stop() })
	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })

	flows := make(map[string]peerFlow, len(peers))
	for range peers {
		flow := awaitLifecycle(t, flowReady, "same-ID peer flow")
		flows[flow.endpoint] = flow
	}
	if err := backend.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set runtime ownership backend deadline: %v", err)
	}
	backendByMarker := make(map[byte]*net.TCPConn, len(peers))
	for range peers {
		conn, err := backend.AcceptTCP()
		if err != nil {
			t.Fatalf("accept runtime ownership backend: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.SetDeadline(testDeadline); err != nil {
			t.Fatalf("set runtime ownership connection deadline: %v", err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatalf("set runtime ownership marker deadline: %v", err)
		}
		var marker [1]byte
		if _, err := io.ReadFull(conn, marker[:]); err != nil {
			t.Fatalf("read runtime ownership marker: %v", err)
		}
		if _, exists := backendByMarker[marker[0]]; exists {
			t.Fatalf("duplicate runtime ownership marker %q", marker[0])
		}
		backendByMarker[marker[0]] = conn
		if err := conn.SetReadDeadline(time.Time{}); err != nil {
			t.Fatalf("clear runtime ownership marker deadline: %v", err)
		}
	}

	runtimeByEndpoint := make(map[string]*connectionRuntime, len(peers))
	awaitRetirementCondition(t, "same-ID active runtimes", func() bool {
		clear(runtimeByEndpoint)
		for _, runtime := range c.runtimeSnapshot() {
			runtimeByEndpoint[runtime.sc.ServerAddr()] = runtime
		}
		return len(runtimeByEndpoint) == len(peers) && c.tcpActive.Load() == int64(len(peers))
	})
	firstEndpoint := peers[0].endpoint().Address
	secondEndpoint := peers[1].endpoint().Address
	firstRuntime := runtimeByEndpoint[firstEndpoint]
	secondRuntime := runtimeByEndpoint[secondEndpoint]
	firstOwner, firstActive := firstRuntime.localConns.Load(uint64(1))
	secondOwner, secondActive := secondRuntime.localConns.Load(uint64(1))
	if !firstActive || !secondActive || firstOwner == secondOwner {
		t.Fatalf("same-ID runtime owners = first(%t, %p) second(%t, %p), want distinct live sockets", firstActive, firstOwner, secondActive, secondOwner)
	}

	firstFlow := flows[firstEndpoint]
	firstBackend := backendByMarker[firstFlow.marker]
	if err := firstFlow.stream.Close(); err != nil {
		t.Fatalf("close first peer send side: %v", err)
	}
	if err := firstBackend.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set first backend FIN deadline: %v", err)
	}
	var probe [1]byte
	if n, err := firstBackend.Read(probe[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("first backend after peer FIN = (%d, %v), want EOF", n, err)
	}
	if err := firstBackend.Close(); err != nil {
		t.Fatalf("close first backend: %v", err)
	}
	awaitRetirementCondition(t, "first same-ID handler completion", func() bool {
		_, active := firstRuntime.localConns.Load(uint64(1))
		return !active && c.tcpActive.Load() == 1
	})
	if current, active := secondRuntime.localConns.Load(uint64(1)); !active || current != secondOwner {
		t.Fatalf("second same-ID owner after first flow = (%t, %p), want original %p", active, current, secondOwner)
	}

	if err := firstRuntime.sc.Close(); err != nil {
		t.Fatalf("retire first runtime connection: %v", err)
	}
	awaitLifecycle(t, firstRuntime.cleanupDone, "first same-ID runtime cleanup")
	if current := c.connMgr.GetConnection(firstEndpoint); current != nil {
		t.Fatalf("retired endpoint retained connection %p", current)
	}
	if current := c.connMgr.GetConnection(secondEndpoint); current != secondRuntime.sc {
		t.Fatalf("surviving endpoint connection = %p, want %p", current, secondRuntime.sc)
	}
	if runtimes := c.runtimeSnapshot(); len(runtimes) != 1 || runtimes[0] != secondRuntime {
		t.Fatalf("runtimes after first retirement = %v, want only second runtime", runtimes)
	}

	secondFlow := flows[secondEndpoint]
	secondBackend := backendByMarker[secondFlow.marker]
	peerPayload := []byte("peer after other runtime retired")
	if _, err := secondFlow.stream.Write(peerPayload); err != nil {
		t.Fatalf("write surviving peer payload: %v", err)
	}
	if err := secondBackend.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set surviving backend read deadline: %v", err)
	}
	gotPeer := make([]byte, len(peerPayload))
	if _, err := io.ReadFull(secondBackend, gotPeer); err != nil || !bytes.Equal(gotPeer, peerPayload) {
		t.Fatalf("surviving backend payload = %q, %v; want %q", gotPeer, err, peerPayload)
	}
	backendPayload := []byte("backend after other runtime retired")
	if _, err := secondBackend.Write(backendPayload); err != nil {
		t.Fatalf("write surviving backend payload: %v", err)
	}
	if err := secondFlow.stream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set surviving peer read deadline: %v", err)
	}
	gotBackend := make([]byte, len(backendPayload))
	if _, err := io.ReadFull(secondFlow.stream, gotBackend); err != nil || !bytes.Equal(gotBackend, backendPayload) {
		t.Fatalf("surviving peer payload = %q, %v; want %q", gotBackend, err, backendPayload)
	}
	if err := secondBackend.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear surviving backend read deadline: %v", err)
	}

	stopDone := callClientLifecycle(c.Stop)
	if err := secondBackend.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set surviving backend Stop deadline: %v", err)
	}
	if n, err := secondBackend.Read(probe[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("surviving backend after Stop = (%d, %v), want EOF", n, err)
	}
	if err := awaitClientLifecycle(t, stopDone, "same-ID Client.Stop"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := awaitClientLifecycle(t, startDone, "same-ID Client.Start join"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	for i, done := range peerDone {
		if err := awaitLifecycle(t, done, fmt.Sprintf("same-ID peer %d close", i)); err != nil {
			t.Fatal(err)
		}
	}
	if runtimes := c.runtimeSnapshot(); len(runtimes) != 0 {
		t.Fatalf("same-ID runtimes after Stop = %d, want 0", len(runtimes))
	}
	for _, runtime := range []*connectionRuntime{firstRuntime, secondRuntime} {
		if _, active := runtime.localConns.Load(uint64(1)); active {
			t.Fatal("same-ID runtime retained local connection after Stop")
		}
	}
	if pending, active := c.tcpPending.Load(), c.tcpActive.Load(); pending != 0 || active != 0 {
		t.Fatalf("same-ID TCP accounting after Stop = pending %d, active %d; want 0, 0", pending, active)
	}
}

func TestClientStopClosesTCPBeforeRuntimePublication(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelTest()
	testDeadline, ok := testCtx.Deadline()
	if !ok {
		t.Fatal("pre-publication test context has no deadline")
	}

	backend, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen for pre-publication backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	peer := newLifecycleStartPeer(t)
	ackSeen := make(chan struct{})
	serverDone := peer.serveRegistration(func(conn *quic.Conn, control *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(control); err != nil {
			return err
		}
		stream, err := conn.OpenStreamSync(testCtx)
		if err != nil {
			return err
		}
		if err := stream.SetDeadline(testDeadline); err != nil {
			return err
		}
		if err := protocol.WriteNewConn(stream, 7, "tcp", "peer", "local", time.Now().Unix()); err != nil {
			return err
		}
		if err := readClientNewConnAck(stream, 7); err != nil {
			return err
		}
		close(ackSeen)
		if _, err := stream.Write([]byte("ready")); err != nil {
			return err
		}
		<-conn.Context().Done()
		return nil
	})

	c := newClientLifecycleClient(t, "stop-before-runtime-publication", peer.endpoint())
	c.config.Local.Port = backend.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { _ = c.Stop() })
	c.runtimesMu.Lock()
	runtimesLocked := true
	releaseRuntimes := func() {
		if runtimesLocked {
			runtimesLocked = false
			c.runtimesMu.Unlock()
		}
	}
	defer releaseRuntimes()

	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
	awaitLifecycle(t, ackSeen, "pre-publication NewConn acknowledgment")
	if err := backend.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set pre-publication backend deadline: %v", err)
	}
	backendConn, err := backend.AcceptTCP()
	if err != nil {
		t.Fatalf("accept pre-publication backend: %v", err)
	}
	t.Cleanup(func() { _ = backendConn.Close() })
	if err := backendConn.SetDeadline(testDeadline); err != nil {
		t.Fatalf("set pre-publication connection deadline: %v", err)
	}
	if err := backendConn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set pre-publication marker deadline: %v", err)
	}
	marker := make([]byte, len("ready"))
	if _, err := io.ReadFull(backendConn, marker); err != nil || string(marker) != "ready" {
		t.Fatalf("pre-publication backend marker = %q, %v; want ready", marker, err)
	}
	if len(c.runtimes) != 0 {
		t.Fatalf("runtime published while runtimesMu held: %d", len(c.runtimes))
	}

	stopDone := callClientLifecycle(c.Stop)
	awaitLifecycle(t, c.forceCtx.Done(), "pre-publication force cancellation")
	if err := backendConn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set pre-publication Stop deadline: %v", err)
	}
	var probe [1]byte
	if n, err := backendConn.Read(probe[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("pre-publication backend after Stop = (%d, %v), want EOF", n, err)
	}
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before runtime publication lock released: %v", err)
	default:
	}

	releaseRuntimes()
	if err := awaitClientLifecycle(t, stopDone, "pre-publication Client.Stop"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := awaitClientLifecycle(t, startDone, "pre-publication Client.Start join"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := awaitLifecycle(t, serverDone, "pre-publication peer close"); err != nil {
		t.Fatal(err)
	}
	if runtimes := c.runtimeSnapshot(); len(runtimes) != 0 {
		t.Fatalf("pre-publication runtimes after Stop = %d, want 0", len(runtimes))
	}
	if pending, active := c.tcpPending.Load(), c.tcpActive.Load(); pending != 0 || active != 0 {
		t.Fatalf("pre-publication TCP accounting after Stop = pending %d, active %d; want 0, 0", pending, active)
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
	runtime := &connectionRuntime{}
	handlerDone := make(chan struct{})
	go func() {
		c.handleStream(flowCtx, clientStream, &ServerConnection{serverAddr: "relay-test"}, runtime)
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
		if _, active := runtime.localConns.Load(connID); active {
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
