package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
)

func acceptanceRuntime(t *testing.T, c *Client, address string) *connectionRuntime {
	t.Helper()
	var found *connectionRuntime
	awaitRetirementCondition(t, "acceptance runtime "+address, func() bool {
		for _, runtime := range c.runtimeSnapshot() {
			if runtime.sc.ServerAddr() == address && runtime.sc.controlAlive() {
				found = runtime
				return true
			}
		}
		return false
	})
	return found
}

func assertAcceptanceOwnershipReleased(t *testing.T, c *Client) {
	t.Helper()
	if runtimes := c.runtimeSnapshot(); len(runtimes) != 0 {
		t.Fatalf("owned runtimes = %d, want 0", len(runtimes))
	}
	for _, endpoint := range c.Snapshot().Endpoints {
		if endpoint.Handshaking != 0 || endpoint.Pending != 0 || endpoint.Registered != 0 ||
			endpoint.Retiring != 0 || endpoint.AccountingFaults != 0 {
			t.Fatalf("endpoint retained ownership: %+v", endpoint)
		}
	}
}

func startBlockedAcceptanceBackend(t *testing.T, payload []byte) (int, <-chan struct{}, <-chan error) {
	t.Helper()
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for acceptance backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	backendActive := make(chan struct{})
	backendDone := make(chan error, 1)
	go func() {
		conn, acceptErr := backend.Accept()
		if acceptErr != nil {
			backendDone <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		got := make([]byte, len(payload))
		if _, readErr := io.ReadFull(conn, got); readErr != nil {
			backendDone <- readErr
			return
		}
		close(backendActive)
		_, readErr := conn.Read(make([]byte, 1))
		backendDone <- readErr
	}()
	return backend.Addr().(*net.TCPAddr).Port, backendActive, backendDone
}

func TestAbruptStopResetsActiveTCPAndJoins(t *testing.T) {
	payload := []byte("active relay interrupted by Stop")
	backendPort, backendActive, backendDone := startBlockedAcceptanceBackend(t, payload)

	peer := newLifecycleStartPeer(t)
	peerReset := make(chan error, 1)
	serverDone := peer.serveRegistration(func(conn *quic.Conn, control *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(control); err != nil {
			return err
		}
		stream, err := conn.OpenStreamSync(context.Background())
		if err != nil {
			return err
		}
		if err := protocol.WriteNewConn(stream, 1, "tcp", "peer", "local", time.Now().Unix()); err != nil {
			return err
		}
		if _, err := stream.Write(payload); err != nil {
			return err
		}
		_, err = stream.Read(make([]byte, 1))
		peerReset <- err
		<-conn.Context().Done()
		return nil
	})

	c := newClientLifecycleClient(t, "abrupt-active-tcp", peer.endpoint())
	c.config.Local.Port = backendPort
	t.Cleanup(func() { _ = c.Stop() })
	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
	awaitLifecycle(t, backendActive, "active abrupt TCP relay")

	if err := awaitClientLifecycle(t, callClientLifecycle(c.Stop), "Stop with active TCP"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := awaitLifecycle(t, peerReset, "peer stream reset"); err == nil {
		t.Fatal("peer stream ended without an abrupt error")
	}
	if err := awaitLifecycle(t, backendDone, "backend relay interruption"); err == nil {
		t.Fatal("backend relay ended without an abrupt error")
	}
	if err := awaitClientLifecycle(t, startDone, "abrupt Start join"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("repeated Stop() error = %v", err)
	}
	if err := awaitLifecycle(t, serverDone, "abrupt peer close"); err != nil {
		t.Fatal(err)
	}
	assertAcceptanceOwnershipReleased(t, c)
}

func TestShutdownReportsControlExitBeforeAndAfterComplete(t *testing.T) {
	t.Run("before Complete", func(t *testing.T) {
		peer := newLifecycleStartPeer(t)
		requestSeen := make(chan struct{})
		serverDone := peer.serveRegistration(func(conn *quic.Conn, control *quic.Stream, _ protocol.RegisterMsg) error {
			if err := writeSuccessfulLifecycleAck(control); err != nil {
				return err
			}
			for {
				msgType, payload, err := protocol.ReadMessage(control)
				if err != nil {
					return nil
				}
				if msgType != protocol.MsgTypeDrainRequest {
					continue
				}
				if err := protocol.DecodeDrainRequest(payload); err != nil {
					return err
				}
				close(requestSeen)
				return conn.CloseWithError(0x41, "control exit before Complete")
			}
		})

		c := newClientLifecycleClient(t, "control-exit-before-complete", peer.endpoint())
		t.Cleanup(func() { _ = c.Stop() })
		startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
		acceptanceRuntime(t, c, peer.endpoint().Address)
		shutdownDone := callClientLifecycle(func() error { return c.Shutdown(context.Background()) })
		awaitLifecycle(t, requestSeen, "DrainRequest before control exit")
		if err := awaitClientLifecycle(t, shutdownDone, "Shutdown after pre-Complete exit"); err == nil ||
			!strings.Contains(err.Error(), peer.endpoint().Address) {
			t.Fatalf("Shutdown error = %v, want exact peer target error", err)
		}
		if err := awaitClientLifecycle(t, startDone, "pre-Complete Start join"); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		if err := awaitLifecycle(t, serverDone, "pre-Complete peer close"); err != nil {
			t.Fatal(err)
		}
		assertAcceptanceOwnershipReleased(t, c)
	})

	t.Run("after Complete and fence with active handler", func(t *testing.T) {
		payload := []byte("handler remains active after Complete")
		backendPort, backendActive, backendDone := startBlockedAcceptanceBackend(t, payload)

		peer := newLifecycleStartPeer(t)
		completeSent := make(chan struct{})
		dropPeer := make(chan struct{})
		serverDone := peer.serveRegistration(func(conn *quic.Conn, control *quic.Stream, _ protocol.RegisterMsg) error {
			if err := writeSuccessfulLifecycleAck(control); err != nil {
				return err
			}
			for {
				msgType, body, err := protocol.ReadMessage(control)
				if err != nil {
					return nil
				}
				if msgType != protocol.MsgTypeDrainRequest {
					continue
				}
				if err := protocol.DecodeDrainRequest(body); err != nil {
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
				if err := protocol.WriteNewConn(stream, 1, "tcp", "peer", "local", time.Now().Unix()); err != nil {
					return err
				}
				if _, err := stream.Write(payload); err != nil {
					return err
				}
				<-dropPeer
				return conn.CloseWithError(0x42, "control exit after Complete")
			}
		})

		c := newClientLifecycleClient(t, "control-exit-after-complete", peer.endpoint())
		c.config.Local.Port = backendPort
		t.Cleanup(func() { _ = c.Stop() })
		startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
		acceptanceRuntime(t, c, peer.endpoint().Address)
		shutdownDone := callClientLifecycle(func() error { return c.Shutdown(context.Background()) })
		awaitLifecycle(t, completeSent, "post-Complete fence")
		awaitLifecycle(t, backendActive, "post-Complete active handler")
		select {
		case err := <-shutdownDone:
			t.Fatalf("Shutdown returned while the fenced handler was active: %v", err)
		default:
		}
		close(dropPeer)
		if err := awaitClientLifecycle(t, shutdownDone, "Shutdown after post-Complete exit"); err == nil ||
			!strings.Contains(err.Error(), peer.endpoint().Address) {
			t.Fatalf("Shutdown error = %v, want exact peer target error", err)
		}
		if err := awaitLifecycle(t, backendDone, "post-Complete handler interruption"); err == nil {
			t.Fatal("backend handler ended without the peer-loss interruption")
		}
		if err := awaitClientLifecycle(t, startDone, "post-Complete Start join"); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		if err := awaitLifecycle(t, serverDone, "post-Complete peer close"); err != nil {
			t.Fatal(err)
		}
		assertAcceptanceOwnershipReleased(t, c)
	})
}

func TestShutdownBlockedDrainWriteCallerArbitration(t *testing.T) {
	peer := newLifecycleStartPeer(t)
	serverDone := peer.serveRegistration(func(conn *quic.Conn, control *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(control); err != nil {
			return err
		}
		<-conn.Context().Done()
		return nil
	})
	c := newClientLifecycleClient(t, "blocked-write-callers", peer.endpoint())
	t.Cleanup(func() { _ = c.Stop() })
	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
	runtime := acceptanceRuntime(t, c, peer.endpoint().Address)
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	runtime.sc.writeDrain = func(*quic.Stream) error {
		close(writeStarted)
		<-runtime.sc.ctx.Done()
		<-releaseWrite
		return errors.New("DrainRequest write released after force")
	}

	backgroundDone := callClientLifecycle(func() error { return c.Shutdown(context.Background()) })
	awaitLifecycle(t, writeStarted, "blocked DrainRequest write")
	wantA := errors.New("caller A deadline")
	ctxA, cancelA := context.WithCancelCause(context.Background())
	cancelA(wantA)
	callerADone := callClientLifecycle(func() error { return c.Shutdown(ctxA) })
	awaitRetirementCondition(t, "caller A shared terminal selection", func() bool {
		c.lifecycleMu.Lock()
		defer c.lifecycleMu.Unlock()
		return c.terminalSelected && errors.Is(c.terminalSemantic, wantA)
	})

	wantB := errors.New("caller B deadline")
	ctxB, cancelB := context.WithCancelCause(context.Background())
	cancelB(wantB)
	callerBDone := callClientLifecycle(func() error { return c.Shutdown(ctxB) })
	close(releaseWrite)

	if err := awaitClientLifecycle(t, backgroundDone, "background Shutdown arbitration"); !errors.Is(err, wantA) {
		t.Fatalf("background Shutdown error = %v, want caller A cause", err)
	}
	if err := awaitClientLifecycle(t, callerADone, "caller A Shutdown"); !errors.Is(err, wantA) {
		t.Fatalf("caller A Shutdown error = %v, want caller A cause", err)
	}
	if err := awaitClientLifecycle(t, callerBDone, "caller B losing Shutdown"); !errors.Is(err, wantA) || !errors.Is(err, wantB) {
		t.Fatalf("caller B Shutdown error = %v, want own and shared causes", err)
	}
	if err := awaitClientLifecycle(t, startDone, "blocked-write Start join"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	wantC := errors.New("caller C after terminal")
	ctxC, cancelC := context.WithCancelCause(context.Background())
	cancelC(wantC)
	if err := c.Shutdown(ctxC); !errors.Is(err, wantA) || errors.Is(err, wantC) {
		t.Fatalf("post-terminal Shutdown error = %v, want shared A without caller C", err)
	}
	if err := awaitLifecycle(t, serverDone, "blocked-write peer close"); err != nil {
		t.Fatal(err)
	}
	assertAcceptanceOwnershipReleased(t, c)
}

func TestShutdownIsolatesInvalidTargetFromHealthyDrain(t *testing.T) {
	for _, test := range []struct {
		name       string
		invalidate func(*ServerConnection)
		preempt    bool
	}{
		{
			name: "current unhealthy",
			invalidate: func(sc *ServerConnection) {
				sc.MarkUnhealthy()
			},
		},
		{
			name: "live control unavailable",
			invalidate: func(sc *ServerConnection) {
				sc.controlStarted.Store(false)
				sc.controlStream.Store(nil)
			},
			preempt: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalidPeer := newLifecycleStartPeer(t)
			healthyPeer := newLifecycleStartPeer(t)
			invalidDone := invalidPeer.serveRegistration(func(conn *quic.Conn, control *quic.Stream, _ protocol.RegisterMsg) error {
				if err := writeSuccessfulLifecycleAck(control); err != nil {
					return err
				}
				<-conn.Context().Done()
				return nil
			})
			healthyRequest := make(chan struct{})
			releaseHealthy := make(chan struct{})
			healthyClosed := make(chan struct{})
			healthyDone := healthyPeer.serveRegistration(func(conn *quic.Conn, control *quic.Stream, _ protocol.RegisterMsg) error {
				if err := writeSuccessfulLifecycleAck(control); err != nil {
					return err
				}
				for {
					msgType, payload, err := protocol.ReadMessage(control)
					if err != nil {
						<-conn.Context().Done()
						close(healthyClosed)
						return nil
					}
					if msgType != protocol.MsgTypeDrainRequest {
						continue
					}
					if err := protocol.DecodeDrainRequest(payload); err != nil {
						return err
					}
					close(healthyRequest)
					select {
					case <-releaseHealthy:
						if err := protocol.WriteDrainComplete(control, -1); err != nil {
							return err
						}
					case <-conn.Context().Done():
						close(healthyClosed)
						return nil
					}
				}
			})

			c := newClientLifecycleClient(t, "invalid-target-isolation", invalidPeer.endpoint(), healthyPeer.endpoint())
			t.Cleanup(func() { _ = c.Stop() })
			startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
			invalidRuntime := acceptanceRuntime(t, c, invalidPeer.endpoint().Address)
			acceptanceRuntime(t, c, healthyPeer.endpoint().Address)
			test.invalidate(invalidRuntime.sc)
			shutdownDone := callClientLifecycle(func() error { return c.Shutdown(context.Background()) })
			if err := awaitLifecycle(t, invalidDone, "invalid target exact cleanup"); err != nil {
				t.Fatal(err)
			}
			awaitLifecycle(t, invalidRuntime.cleanupDone, "invalid target ownership join")
			awaitLifecycle(t, healthyRequest, "healthy target DrainRequest")
			select {
			case <-healthyClosed:
				t.Fatal("healthy target closed while its drain was pending")
			default:
			}
			select {
			case err := <-shutdownDone:
				t.Fatalf("Shutdown returned before healthy target completed: %v", err)
			default:
			}

			if test.preempt {
				want := errors.New("global caller preempts target error")
				ctx, cancel := context.WithCancelCause(context.Background())
				cancel(want)
				callerDone := callClientLifecycle(func() error { return c.Shutdown(ctx) })
				if err := awaitClientLifecycle(t, callerDone, "preempting caller Shutdown"); !errors.Is(err, want) {
					t.Fatalf("caller Shutdown error = %v, want global cause", err)
				}
				if err := awaitClientLifecycle(t, shutdownDone, "preempted background Shutdown"); !errors.Is(err, want) {
					t.Fatalf("background Shutdown error = %v, want global cause", err)
				}
			} else {
				close(releaseHealthy)
				if err := awaitClientLifecycle(t, shutdownDone, "isolated unhealthy Shutdown"); err == nil || !strings.Contains(err.Error(), "connection is unhealthy") {
					t.Fatalf("Shutdown error = %v, want unhealthy target error", err)
				}
			}
			if err := awaitClientLifecycle(t, startDone, "invalid-isolation Start join"); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			if err := awaitLifecycle(t, healthyDone, "healthy target close"); err != nil {
				t.Fatal(err)
			}
			assertAcceptanceOwnershipReleased(t, c)
		})
	}
}

func TestShutdownJoinsReaderBlockedOnFullControlQueue(t *testing.T) {
	peer := newLifecycleStartPeer(t)
	sendFrames := make(chan struct{})
	framesSent := make(chan struct{})
	serverDone := peer.serveRegistration(func(conn *quic.Conn, control *quic.Stream, _ protocol.RegisterMsg) error {
		if err := writeSuccessfulLifecycleAck(control); err != nil {
			return err
		}
		<-sendFrames
		if err := protocol.WriteHeartbeat(control, time.Now().Unix()); err != nil {
			return err
		}
		if err := protocol.WriteHeartbeat(control, time.Now().Unix()); err != nil {
			return err
		}
		close(framesSent)
		<-conn.Context().Done()
		return nil
	})

	c := newClientLifecycleClient(t, "full-control-queue", peer.endpoint())
	t.Cleanup(func() { _ = c.Stop() })
	startDone := callClientLifecycle(func() error { return c.Start(context.Background()) })
	runtime := acceptanceRuntime(t, c, peer.endpoint().Address)
	want := errors.New("DrainRequest writer failed with full read queue")
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	runtime.sc.writeDrain = func(*quic.Stream) error {
		close(writeStarted)
		<-releaseWrite
		return want
	}
	shutdownDone := callClientLifecycle(func() error { return c.Shutdown(context.Background()) })
	awaitLifecycle(t, writeStarted, "DrainRequest write before full control queue")
	close(sendFrames)
	awaitLifecycle(t, framesSent, "two queued control frames")
	awaitRetirementCondition(t, "reader blocked on full control queue", func() bool {
		return runtime.sc.controlPending.Load() == 2
	})
	close(releaseWrite)

	if err := awaitClientLifecycle(t, shutdownDone, "Shutdown after full-queue writer exit"); !errors.Is(err, want) {
		t.Fatalf("Shutdown error = %v, want write failure", err)
	}
	if err := awaitClientLifecycle(t, startDone, "full-queue Start join"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-runtime.sc.controlDone:
	default:
		t.Fatal("controlDone was not published after reader join")
	}
	select {
	case <-runtime.cleanupDone:
	default:
		t.Fatal("runtime cleanup completed without publishing cleanupDone")
	}
	if err := awaitLifecycle(t, serverDone, "full-queue peer close"); err != nil {
		t.Fatal(err)
	}
	assertAcceptanceOwnershipReleased(t, c)
}
