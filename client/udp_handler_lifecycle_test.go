package client

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

func newUDPHandlerQUICPair(t *testing.T) (*quic.Conn, *quic.Conn) {
	t.Helper()
	peer := newLifecyclePeer(t)
	accepted := make(chan *quic.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := peer.listener.Accept(peer.ctx)
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientConn, err := quic.DialAddr(ctx, peer.listener.Addr().String(), peer.clientTLS, &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       10 * time.Second,
		EnableDatagrams:      true,
	})
	if err != nil {
		t.Fatalf("dial UDP handler QUIC peer: %v", err)
	}
	var serverConn *quic.Conn
	select {
	case serverConn = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("accept UDP handler QUIC peer: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out accepting UDP handler QUIC peer")
	}
	t.Cleanup(func() {
		_ = clientConn.CloseWithError(0, "test complete")
		_ = serverConn.CloseWithError(0, "test complete")
	})
	return clientConn, serverConn
}

func awaitUDPHandler(t *testing.T, done <-chan struct{}, event string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", event)
	}
}

func assertNoUDPSessions(t *testing.T, handler *UDPHandler) {
	t.Helper()
	count := 0
	handler.sessions.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("handler retained %d UDP sessions", count)
	}
}

func TestUDPHandlerStopBeforeStart(t *testing.T) {
	handler := NewUDPHandler("127.0.0.1", 1, true, zerolog.Nop())
	handler.Stop()
	handler.Stop()
	handler.Start(context.Background(), nil)
	handler.stopAndWait()

	if handler.started {
		t.Fatal("Start after Stop started the handler")
	}
	if !handler.closed {
		t.Fatal("Stop did not close the handler")
	}
	if _, err := handler.fragmentAssembler.AddFragment(1, 1, 0, 2, []byte("pending")); !errors.Is(err, protocol.ErrFragmentAssemblerClosed) {
		t.Fatalf("assembler after Stop error = %v, want ErrFragmentAssemblerClosed", err)
	}
	assertNoUDPSessions(t, handler)
}

func TestUDPHandlerStopJoinsBlockedReceiveSessionAndAssembler(t *testing.T) {
	backend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen UDP backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	clientConn, _ := newUDPHandlerQUICPair(t)
	handler := NewUDPHandler(
		"127.0.0.1",
		backend.LocalAddr().(*net.UDPAddr).Port,
		true,
		zerolog.Nop(),
	)
	handler.Start(context.Background(), clientConn)

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	handler.Start(canceledCtx, clientConn)
	if err := handler.ctx.Err(); err != nil {
		t.Fatalf("double Start replaced the live handler context: %v", err)
	}

	session, err := handler.getOrCreateSession(7, clientConn)
	if err != nil {
		t.Fatalf("create active UDP session: %v", err)
	}
	if _, err := handler.fragmentAssembler.AddFragment(7, 3, 0, 2, []byte("pending")); err != nil {
		t.Fatalf("create pending fragment group: %v", err)
	}

	start := make(chan struct{})
	var callers sync.WaitGroup
	for range 16 {
		callers.Go(func() {
			<-start
			handler.Stop()
		})
	}
	close(start)
	callers.Wait()

	waitDone := make(chan struct{})
	go func() {
		handler.wait()
		close(waitDone)
	}()
	awaitUDPHandler(t, waitDone, "UDP handler fixed loops and session reader")

	assertNoUDPSessions(t, handler)
	if _, err := handler.fragmentAssembler.AddFragment(7, 4, 0, 2, []byte("closed")); !errors.Is(err, protocol.ErrFragmentAssemblerClosed) {
		t.Fatalf("assembler after Stop error = %v, want ErrFragmentAssemblerClosed", err)
	}
	_ = session.localConn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := session.localConn.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("session socket read after Stop error = %v, want net.ErrClosed", err)
	}
}

func TestUDPHandlerReceiveTerminalErrorStopsWithoutWaitingForItself(t *testing.T) {
	clientConn, serverConn := newUDPHandlerQUICPair(t)
	handler := NewUDPHandler("127.0.0.1", 1, true, zerolog.Nop())
	handler.Start(context.Background(), clientConn)
	if err := serverConn.CloseWithError(1, "terminal receive error"); err != nil {
		t.Fatal(err)
	}

	waitDone := make(chan struct{})
	go func() {
		handler.wait()
		close(waitDone)
	}()
	awaitUDPHandler(t, waitDone, "terminal receive self-Stop")
	if !handler.closed {
		t.Fatal("terminal ReceiveDatagram error did not close the handler")
	}
	if _, err := handler.fragmentAssembler.AddFragment(1, 1, 0, 2, []byte("closed")); !errors.Is(err, protocol.ErrFragmentAssemblerClosed) {
		t.Fatalf("assembler after terminal receive error = %v", err)
	}
}
