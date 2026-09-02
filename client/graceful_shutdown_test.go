package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

func TestClientShutdownNegotiatesDrainAndJoinsOwnership(t *testing.T) {
	peer := newLifecycleStartPeer(t)
	requestSeen := make(chan struct{})
	serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(stream); err != nil {
			return err
		}
		for {
			msgType, payload, err := protocol.ReadMessage(stream)
			if err != nil {
				<-conn.Context().Done()
				return nil
			}
			if msgType != protocol.MsgTypeDrainRequest {
				continue
			}
			if err := protocol.DecodeDrainRequest(payload); err != nil {
				return err
			}
			close(requestSeen)
			if err := protocol.WriteDrainComplete(stream, -1); err != nil {
				return err
			}
		}
	})
	c := newClientLifecycleClient(t, "graceful-drain", peer.endpoint())
	t.Cleanup(func() { _ = c.Stop() })
	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
	awaitRetirementCondition(t, "client drain runtime", func() bool {
		runtimes := c.runtimeSnapshot()
		return len(runtimes) == 1 && runtimes[0].sc.controlAlive()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	awaitLifecycle(t, requestSeen, "DrainRequest")
	if err := awaitClientLifecycle(t, startDone, "graceful Start join"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := awaitLifecycle(t, serverDone, "graceful server close"); err != nil {
		t.Fatal(err)
	}
	if runtimes := c.runtimeSnapshot(); len(runtimes) != 0 {
		t.Fatalf("owned runtimes after Shutdown = %d, want 0", len(runtimes))
	}
	for _, endpoint := range c.Snapshot().Endpoints {
		if endpoint.AccountingFaults != 0 || endpoint.Registered != 0 || endpoint.Retiring != 0 {
			t.Fatalf("endpoint after Shutdown = %+v", endpoint)
		}
	}
}

func TestClientShutdownAcceptsThroughFenceAndWaitsForHandler(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for drain backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	request := []byte("request before half-close")
	requestRead := make(chan struct{})
	releaseHandler := make(chan struct{})
	defer func() {
		select {
		case <-releaseHandler:
		default:
			close(releaseHandler)
		}
	}()
	backendDone := make(chan error, 1)
	go func() {
		conn, acceptErr := backend.Accept()
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
			backendDone <- fmt.Errorf("backend request = %q, want %q", got, request)
			return
		}
		close(requestRead)
		<-releaseHandler
		backendDone <- nil
	}()

	peer := newLifecycleStartPeer(t)
	completeSent := make(chan struct{})
	publishStream := make(chan struct{})
	defer func() {
		select {
		case <-publishStream:
		default:
			close(publishStream)
		}
	}()
	serverDone := peer.serveRegistration(func(conn *quic.Conn, control *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(control); err != nil {
			return err
		}
		for {
			msgType, payload, err := protocol.ReadMessage(control)
			if err != nil {
				<-conn.Context().Done()
				return nil
			}
			if msgType != protocol.MsgTypeDrainRequest {
				continue
			}
			if err := protocol.DecodeDrainRequest(payload); err != nil {
				return err
			}
			stream, err := conn.OpenStreamSync(context.Background())
			if err != nil {
				return err
			}
			if got := int64(stream.StreamID()); got != 1 {
				return fmt.Errorf("server stream ID = %d, want 1", got)
			}
			if err := protocol.WriteDrainComplete(control, 1); err != nil {
				return err
			}
			close(completeSent)
			<-publishStream
			if err := protocol.WriteNewConn(stream, 1, "tcp", "peer", "local", time.Now().Unix()); err != nil {
				return err
			}
			if _, err := stream.Write(request); err != nil {
				return err
			}
			if err := stream.Close(); err != nil {
				return err
			}
			<-conn.Context().Done()
			return nil
		}
	})

	c := newClientLifecycleClient(t, "positive-drain-fence", peer.endpoint())
	c.config.Local.Port = backend.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { _ = c.Stop() })
	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
	awaitRetirementCondition(t, "positive-fence client runtime", func() bool {
		runtimes := c.runtimeSnapshot()
		return len(runtimes) == 1 && runtimes[0].sc.controlAlive()
	})
	shutdownDone := callClientLifecycle(func() error { return c.Shutdown(context.Background()) })
	awaitLifecycle(t, completeSent, "positive DrainComplete")
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before the fenced stream arrived: %v", err)
	default:
	}
	close(publishStream)
	awaitLifecycle(t, requestRead, "backend request half-close")
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before the stream handler completed: %v", err)
	default:
	}
	close(releaseHandler)
	if err := awaitLifecycle(t, backendDone, "backend handler release"); err != nil {
		t.Fatal(err)
	}
	if err := awaitClientLifecycle(t, shutdownDone, "positive-fence Shutdown"); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := awaitClientLifecycle(t, startDone, "positive-fence Start join"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := awaitLifecycle(t, serverDone, "positive-fence server close"); err != nil {
		t.Fatal(err)
	}
}

func TestStopPreemptsBlockedShutdown(t *testing.T) {
	peer := newLifecycleStartPeer(t)
	requestSeen, serverDone := serveBlockedDrainPeer(peer)
	c := newClientLifecycleClient(t, "stop-preempts-shutdown", peer.endpoint())
	t.Cleanup(func() { _ = c.Stop() })
	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
	awaitRetirementCondition(t, "Stop-preemption client runtime", func() bool {
		return len(c.runtimeSnapshot()) == 1
	})
	shutdownDone := callClientLifecycle(func() error { return c.Shutdown(context.Background()) })
	awaitLifecycle(t, requestSeen, "blocked DrainRequest")
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := awaitClientLifecycle(t, shutdownDone, "preempted Shutdown"); !errors.Is(err, ErrClientStopped) {
		t.Fatalf("Shutdown() error = %v, want ErrClientStopped", err)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("repeated Stop() error = %v", err)
	}
	if err := c.Shutdown(context.Background()); !errors.Is(err, ErrClientStopped) {
		t.Fatalf("post-terminal Shutdown() error = %v, want ErrClientStopped", err)
	}
	if err := awaitClientLifecycle(t, startDone, "Stop-preempted Start join"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := awaitLifecycle(t, serverDone, "Stop-preempted server close"); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownCallerCauseForcesBlockedDrain(t *testing.T) {
	peer := newLifecycleStartPeer(t)
	requestSeen, serverDone := serveBlockedDrainPeer(peer)
	c := newClientLifecycleClient(t, "shutdown-caller-cause", peer.endpoint())
	t.Cleanup(func() { _ = c.Stop() })
	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
	awaitRetirementCondition(t, "caller-cause client runtime", func() bool {
		return len(c.runtimeSnapshot()) == 1
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	want := errors.New("caller drain deadline")
	shutdownDone := callClientLifecycle(func() error { return c.Shutdown(ctx) })
	awaitLifecycle(t, requestSeen, "caller-cause DrainRequest")
	cancel(want)
	if err := awaitClientLifecycle(t, shutdownDone, "caller-cause Shutdown"); !errors.Is(err, want) {
		t.Fatalf("Shutdown() error = %v, want caller cause", err)
	}
	if err := awaitClientLifecycle(t, startDone, "caller-cause Start join"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := awaitLifecycle(t, serverDone, "caller-cause server close"); err != nil {
		t.Fatal(err)
	}
}

func serveBlockedDrainPeer(peer *lifecyclePeer) (<-chan struct{}, <-chan error) {
	requestSeen := make(chan struct{})
	serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(stream); err != nil {
			return err
		}
		for {
			msgType, payload, err := protocol.ReadMessage(stream)
			if err != nil {
				<-conn.Context().Done()
				return nil
			}
			if msgType == protocol.MsgTypeDrainRequest {
				if err := protocol.DecodeDrainRequest(payload); err != nil {
					return err
				}
				close(requestSeen)
				<-conn.Context().Done()
				return nil
			}
		}
	})
	return requestSeen, serverDone
}

func TestShutdownIsolatesUnsupportedPeerWhileSupportedPeerDrains(t *testing.T) {
	unsupportedPeer := newLifecycleStartPeer(t)
	supportedPeer := newLifecycleStartPeer(t)
	unsupportedDrain := make(chan struct{}, 1)
	unsupportedClosed := make(chan struct{})
	unsupportedDone := unsupportedPeer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := protocol.WriteRegisterAckWithAuth(stream, true, "registered", protocol.ProtocolVersion,
			[]string{"tcp", "udp", protocol.CapabilityUDPWireV2}, ""); err != nil {
			return err
		}
		for {
			msgType, _, err := protocol.ReadMessage(stream)
			if err != nil {
				<-conn.Context().Done()
				close(unsupportedClosed)
				return nil
			}
			if msgType == protocol.MsgTypeDrainRequest {
				unsupportedDrain <- struct{}{}
			}
		}
	})
	supportedRequest := make(chan struct{})
	releaseSupported := make(chan struct{})
	defer func() {
		select {
		case <-releaseSupported:
		default:
			close(releaseSupported)
		}
	}()
	supportedClosed := make(chan struct{})
	supportedDone := supportedPeer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(stream); err != nil {
			return err
		}
		for {
			msgType, payload, err := protocol.ReadMessage(stream)
			if err != nil {
				<-conn.Context().Done()
				close(supportedClosed)
				return nil
			}
			if msgType != protocol.MsgTypeDrainRequest {
				continue
			}
			if err := protocol.DecodeDrainRequest(payload); err != nil {
				return err
			}
			close(supportedRequest)
			<-releaseSupported
			if err := protocol.WriteDrainComplete(stream, -1); err != nil {
				return err
			}
		}
	})

	c := newClientLifecycleClient(t, "mixed-drain-peers", unsupportedPeer.endpoint(), supportedPeer.endpoint())
	t.Cleanup(func() { _ = c.Stop() })
	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
	awaitRetirementCondition(t, "mixed-peer runtimes", func() bool {
		runtimes := c.runtimeSnapshot()
		return len(runtimes) == 2 && runtimes[0].sc.controlAlive() && runtimes[1].sc.controlAlive()
	})
	shutdownDone := callClientLifecycle(func() error { return c.Shutdown(context.Background()) })
	awaitLifecycle(t, unsupportedClosed, "unsupported peer local cleanup")
	awaitLifecycle(t, supportedRequest, "supported peer DrainRequest")
	select {
	case <-supportedClosed:
		t.Fatal("supported peer was closed while its drain was pending")
	default:
	}
	select {
	case <-unsupportedDrain:
		t.Fatal("unsupported peer received DrainRequest")
	default:
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before supported peer completed: %v", err)
	default:
	}
	close(releaseSupported)
	if err := awaitClientLifecycle(t, shutdownDone, "mixed-peer Shutdown"); !errors.Is(err, ErrPeerGracefulShutdownUnsupported) {
		t.Fatalf("Shutdown() error = %v, want ErrPeerGracefulShutdownUnsupported", err)
	}
	if err := awaitClientLifecycle(t, startDone, "mixed-peer Start join"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := awaitLifecycle(t, unsupportedDone, "unsupported peer server close"); err != nil {
		t.Fatal(err)
	}
	if err := awaitLifecycle(t, supportedDone, "supported peer server close"); err != nil {
		t.Fatal(err)
	}
	if runtimes := c.runtimeSnapshot(); len(runtimes) != 0 {
		t.Fatalf("owned runtimes after mixed-peer Shutdown = %d, want 0", len(runtimes))
	}
}

func TestForceClosesAllControlsBeforeJoin(t *testing.T) {
	forceCtx, forceCancel := context.WithCancel(context.Background())
	c := &Client{forceCtx: forceCtx, forceCancel: forceCancel, runtimes: make(map[*ServerConnection]*connectionRuntime)}
	release := make(chan struct{})
	connections := []*ServerConnection{
		NewServerConnection("first", "", nil, zerolog.Nop()),
		NewServerConnection("second", "", nil, zerolog.Nop()),
	}
	for _, sc := range connections {
		sc.controlStarted.Store(true)
		sc.controlWG.Go(func() { <-release })
		child, cancel := context.WithCancel(forceCtx)
		acceptCtx, cancelAccept := context.WithCancel(child)
		c.runtimes[sc] = &connectionRuntime{
			sc: sc, forceCtx: child, cancelForce: cancel, acceptCtx: acceptCtx,
			cancelAccept: cancelAccept, acceptDone: make(chan struct{}), cleanupDone: make(chan struct{}),
		}
	}
	c.forceOwned()
	for _, sc := range connections {
		if sc.ctx.Err() == nil {
			t.Fatalf("%s was not canceled before control join", sc.ServerAddr())
		}
	}
	close(release)
	for _, sc := range connections {
		sc.waitControl()
	}
}

func TestQueuedControlErrorCannotQuiesceAsSuccess(t *testing.T) {
	peer := newLifecycleStartPeer(t)
	framesSent := make(chan struct{})
	serverDone := peer.serveRegistration(func(conn *quic.Conn, stream *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(stream); err != nil {
			return err
		}
		for {
			msgType, payload, err := protocol.ReadMessage(stream)
			if err != nil {
				<-conn.Context().Done()
				return nil
			}
			if msgType != protocol.MsgTypeDrainRequest {
				continue
			}
			if err := protocol.DecodeDrainRequest(payload); err != nil {
				return err
			}
			if err := protocol.WriteMessage(stream, 0x20, struct{}{}); err != nil {
				return err
			}
			if err := protocol.WriteDrainComplete(stream, -1); err != nil {
				return err
			}
			if err := protocol.WriteDrainComplete(stream, 1); err != nil {
				return err
			}
			close(framesSent)
			<-conn.Context().Done()
			return nil
		}
	})

	c := newClientLifecycleClient(t, "queued-control-error", peer.endpoint())
	t.Cleanup(func() { _ = c.Stop() })
	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
	var runtime *connectionRuntime
	awaitRetirementCondition(t, "queued-error client runtime", func() bool {
		runtimes := c.runtimeSnapshot()
		if len(runtimes) != 1 || !runtimes[0].sc.controlAlive() {
			return false
		}
		runtime = runtimes[0]
		return true
	})
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	defer func() {
		select {
		case <-releaseHandler:
		default:
			close(releaseHandler)
		}
	}()
	runtime.sc.SetNonHeartbeatHandler(func(byte, []byte) error {
		close(handlerEntered)
		<-releaseHandler
		return nil
	})

	shutdownDone := callClientLifecycle(func() error { return c.Shutdown(context.Background()) })
	awaitLifecycle(t, framesSent, "queued control frames")
	awaitLifecycle(t, handlerEntered, "blocked control handler")
	awaitRetirementCondition(t, "queued conflicting drain complete", func() bool {
		return runtime.sc.controlPending.Load() == 3
	})
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before queued control error was processed: %v", err)
	default:
	}
	close(releaseHandler)

	if err := awaitClientLifecycle(t, shutdownDone, "queued control error shutdown"); err == nil {
		t.Fatal("Shutdown succeeded with a queued conflicting DrainComplete")
	}
	if err := awaitClientLifecycle(t, startDone, "queued control error Start join"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := awaitLifecycle(t, serverDone, "queued control error server close"); err != nil {
		t.Fatal(err)
	}
}
