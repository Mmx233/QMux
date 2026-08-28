package client

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const heartbeatTestStreamWindow = 64

func TestHeartbeatWriteDeadlineBoundsFlowControlStall(t *testing.T) {
	t.Run("ordinary write parks", func(t *testing.T) {
		calibrateParkedHeartbeats(t)
	})

	t.Run("production helper times out", func(t *testing.T) {
		parkedHeartbeats := calibrateParkedHeartbeats(t)
		sender, _ := newHeartbeatFlowControlPair(t)
		stream := openHeartbeatTestStream(t, sender)
		if err := stream.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set prefill deadline: %v", err)
		}
		for range max(0, parkedHeartbeats-2) {
			if err := protocol.WriteHeartbeat(stream, 1_700_000_000); err != nil {
				t.Fatalf("prefill flow-controlled stream: %v", err)
			}
		}

		const writeTimeout = 250 * time.Millisecond
		sc := NewServerConnection("heartbeat.test:8443", "heartbeat.test", tls.NewLRUClientSessionCache(1), zerolog.Nop())
		sc.SetHealthConfig(writeTimeout)
		sc.controlStream.Store(stream)
		sc.MarkHealthy()
		var reconnects atomic.Int32
		sc.SetReconnectCallback(func(string) { reconnects.Add(1) })

		var err error
		var elapsed time.Duration
		for range parkedHeartbeats {
			started := time.Now()
			result := make(chan error, 1)
			go func() { result <- sc.SendHeartbeat() }()
			select {
			case err = <-result:
				elapsed = time.Since(started)
			case <-time.After(3 * time.Second):
				_ = sender.CloseWithError(0, "unblock missed heartbeat deadline")
				t.Fatal("heartbeat write did not honor its deadline")
			}
			if err != nil {
				break
			}
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("sendHeartbeat() error = %T %v, want os.ErrDeadlineExceeded", err, err)
		}
		if elapsed < writeTimeout*9/10 {
			t.Fatalf("sendHeartbeat() returned after %v, want at least %v", elapsed, writeTimeout*9/10)
		}
		if sc.IsHealthy() || reconnects.Load() != 1 {
			t.Fatalf("deadline failure left healthy=%t reconnects=%d, want false/1", sc.IsHealthy(), reconnects.Load())
		}
	})

	t.Run("heartbeat loop exits stalled write", func(t *testing.T) {
		parkedHeartbeats := calibrateParkedHeartbeats(t)
		sender, receiver := newHeartbeatFlowControlPair(t)
		stream := openHeartbeatTestStream(t, sender)
		peerDone := make(chan error, 1)
		go func() {
			peerStream, err := receiver.AcceptStream(context.Background())
			if err != nil {
				peerDone <- err
				return
			}
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-receiver.Context().Done():
					peerDone <- nil
					return
				case <-ticker.C:
					if err := protocol.WriteHeartbeat(peerStream, time.Now().Unix()); err != nil {
						select {
						case <-receiver.Context().Done():
							peerDone <- nil
						default:
							peerDone <- err
						}
						return
					}
				}
			}
		}()
		if err := stream.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set loop prefill deadline: %v", err)
		}
		for range max(0, parkedHeartbeats-2) {
			if err := protocol.WriteHeartbeat(stream, 1_700_000_000); err != nil {
				t.Fatalf("prefill heartbeat loop stream: %v", err)
			}
		}

		sc := NewServerConnection("heartbeat.test:8443", "heartbeat.test", tls.NewLRUClientSessionCache(1), zerolog.Nop())
		sc.SetHealthConfig(5 * time.Second)
		sc.MarkHealthy()
		var reconnects atomic.Int32
		sc.SetReconnectCallback(func(string) {
			reconnects.Add(1)
			sc.cancel()
		})
		loopDone := make(chan struct{})
		go func() {
			sc.heartbeatLoop(100*time.Millisecond, stream)
			close(loopDone)
		}()
		select {
		case <-loopDone:
		case <-time.After(2 * time.Second):
			_ = sender.CloseWithError(0, "unblock stalled heartbeat loop")
			t.Fatal("heartbeat loop did not exit its stalled write")
		}
		if sc.IsHealthy() || reconnects.Load() != 1 {
			t.Fatalf("stalled heartbeat loop left healthy=%t reconnects=%d, want false/1", sc.IsHealthy(), reconnects.Load())
		}
		if err := sender.CloseWithError(0, "heartbeat loop complete"); err != nil {
			t.Fatalf("close heartbeat loop connection: %v", err)
		}
		select {
		case err := <-peerDone:
			if err != nil {
				t.Fatalf("heartbeat-only peer: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("heartbeat-only peer did not stop after close")
		}
	})

	t.Run("bidirectional heartbeat stays healthy", func(t *testing.T) {
		sender, receiver := newHeartbeatFlowControlPair(t)
		stream := openHeartbeatTestStream(t, sender)
		peerDone := make(chan error, 1)
		go func() {
			peerStream, err := receiver.AcceptStream(context.Background())
			if err != nil {
				peerDone <- err
				return
			}
			var heartbeat protocol.HeartbeatMsg
			if err := protocol.ReadTypedMessage(peerStream, protocol.MsgTypeHeartbeat, &heartbeat); err != nil {
				peerDone <- err
				return
			}
			peerDone <- protocol.WriteHeartbeat(peerStream, time.Now().Unix())
		}()

		sc := NewServerConnection("heartbeat.test:8443", "heartbeat.test", tls.NewLRUClientSessionCache(1), zerolog.Nop())
		var reconnects atomic.Int32
		sc.SetReconnectCallback(func(string) { reconnects.Add(1) })
		if err := sc.sendHeartbeat(stream, time.Now().Add(time.Second)); err != nil {
			t.Fatalf("send heartbeat: %v", err)
		}
		if err := stream.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("set heartbeat read deadline: %v", err)
		}
		var heartbeat protocol.HeartbeatMsg
		if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeHeartbeat, &heartbeat); err != nil {
			t.Fatalf("read peer heartbeat: %v", err)
		}
		if err := <-peerDone; err != nil {
			t.Fatalf("peer heartbeat exchange: %v", err)
		}
		if !sc.IsHealthy() || reconnects.Load() != 0 {
			t.Fatalf("normal heartbeat left healthy=%t reconnects=%d, want true/0", sc.IsHealthy(), reconnects.Load())
		}
	})
}

func calibrateParkedHeartbeats(t *testing.T) int {
	t.Helper()
	sender, _ := newHeartbeatFlowControlPair(t)
	stream := openHeartbeatTestStream(t, sender)

	var completed atomic.Int64
	writeDone := make(chan error, 1)
	go func() {
		for {
			if err := protocol.WriteHeartbeat(stream, 1_700_000_000); err != nil {
				writeDone <- err
				return
			}
			completed.Add(1)
		}
	}()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	var parkedHeartbeats int64
	for parkedHeartbeats == 0 {
		before := completed.Load()
		select {
		case err := <-writeDone:
			t.Fatalf("ordinary heartbeat write stopped before flow control filled: %v", err)
		case <-deadline.C:
			t.Fatal("ordinary heartbeat writes did not park")
		case <-time.After(100 * time.Millisecond):
			after := completed.Load()
			if before > 0 && after == before {
				parkedHeartbeats = after
			}
		}
	}

	if err := sender.CloseWithError(0, "release parked heartbeat write"); err != nil {
		t.Fatalf("close parked heartbeat connection: %v", err)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("parked heartbeat write returned nil after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked heartbeat write did not unblock after close")
	}
	return int(parkedHeartbeats)
}

func newHeartbeatFlowControlPair(t *testing.T) (*quic.Conn, *quic.Conn) {
	t.Helper()
	serverTLS, clientTLS := lifecycleTLSConfigs(t)
	serverTLS.NextProtos = []string{"qmux-heartbeat-deadline-test"}
	clientTLS.NextProtos = serverTLS.NextProtos
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, &quic.Config{
		HandshakeIdleTimeout:           5 * time.Second,
		MaxIdleTimeout:                 10 * time.Second,
		InitialStreamReceiveWindow:     heartbeatTestStreamWindow,
		MaxStreamReceiveWindow:         heartbeatTestStreamWindow,
		InitialConnectionReceiveWindow: 1 << 20,
		MaxConnectionReceiveWindow:     1 << 20,
	})
	if err != nil {
		t.Fatalf("listen QUIC: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	sender, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, &quic.Config{MaxIdleTimeout: 10 * time.Second})
	if err != nil {
		cancel()
		_ = listener.Close()
		t.Fatalf("dial QUIC: %v", err)
	}
	var receiver *quic.Conn
	select {
	case receiver = <-accepted:
	case err := <-acceptErr:
		_ = sender.CloseWithError(0, "accept failed")
		cancel()
		_ = listener.Close()
		t.Fatalf("accept QUIC: %v", err)
	case <-ctx.Done():
		_ = sender.CloseWithError(0, "accept timeout")
		cancel()
		_ = listener.Close()
		t.Fatal("accept QUIC timed out")
	}
	t.Cleanup(func() {
		_ = sender.CloseWithError(0, "test complete")
		_ = receiver.CloseWithError(0, "test complete")
		cancel()
		_ = listener.Close()
	})
	return sender, receiver
}

func openHeartbeatTestStream(t *testing.T, conn *quic.Conn) *quic.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open heartbeat stream: %v", err)
	}
	return stream
}
