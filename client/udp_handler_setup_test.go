package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
	"golang.org/x/net/dns/dnsmessage"
)

const clientUDPSetupChild = "QMUX_CLIENT_UDP_SETUP_CHILD"

func runClientUDPSetupSubprocess(t *testing.T) bool {
	t.Helper()
	if os.Getenv(clientUDPSetupChild) == t.Name() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v")
	cmd.Env = append(os.Environ(), clientUDPSetupChild+"="+t.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated UDP setup test failed: %v\n%s", err, out)
	}
	return true
}

type clientUDPResolverGate struct {
	stall       atomic.Bool
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func installClientUDPResolverGate(t *testing.T) *clientUDPResolverGate {
	t.Helper()
	dns, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsDone := make(chan struct{})
	go func() {
		defer close(dnsDone)
		buf := make([]byte, 2048)
		for {
			n, addr, err := dns.ReadFrom(buf)
			if err != nil {
				return
			}
			var request dnsmessage.Message
			if err := request.Unpack(buf[:n]); err != nil {
				continue
			}
			response := dnsmessage.Message{
				ID:                 request.ID,
				Response:           true,
				RecursionAvailable: true,
				Questions:          request.Questions,
			}
			for _, question := range request.Questions {
				if question.Type == dnsmessage.TypeA {
					response.Answers = append(response.Answers, dnsmessage.Resource{
						Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
						Body:   &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}},
					})
				}
			}
			wire, err := response.Pack()
			if err == nil {
				_, _ = dns.WriteTo(wire, addr)
			}
		}
	}()

	gate := &clientUDPResolverGate{entered: make(chan struct{}), release: make(chan struct{})}
	previousResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if gate.stall.Load() {
				gate.enteredOnce.Do(func() { close(gate.entered) })
				select {
				case <-gate.release:
				case <-ctx.Done():
					return nil, context.Cause(ctx)
				}
			}
			return (&net.Dialer{}).DialContext(ctx, "udp4", dns.LocalAddr().String())
		},
	}
	t.Cleanup(func() {
		gate.unblock()
		net.DefaultResolver = previousResolver
		_ = dns.Close()
		<-dnsDone
	})
	return gate
}

func (g *clientUDPResolverGate) unblock() {
	g.releaseOnce.Do(func() { close(g.release) })
}

func newClientUDPSetupHandler(t *testing.T, host string, backend *net.UDPConn, budget *udpSessionBudget) (*UDPHandler, *quic.Conn, *quic.Conn) {
	t.Helper()
	clientConn, serverConn := newUDPHandlerQUICPair(t)
	handler := newUDPHandler(host, backend.LocalAddr().(*net.UDPAddr).Port, true,
		config.DefaultMaxUDPFragmentGroupsPerHandler,
		config.DefaultMaxUDPFragmentBackingBytesPerHandler,
		zerolog.Nop(), budget)
	handler.Start(context.Background(), clientConn)
	t.Cleanup(func() {
		_ = clientConn.CloseWithError(0, "test complete")
		handler.stopAndWait()
	})
	return handler, clientConn, serverConn
}

func TestUDPHandlerDNSSetupDoesNotBlockExistingSession(t *testing.T) {
	if runClientUDPSetupSubprocess(t) {
		return
	}
	gate := installClientUDPResolverGate(t)
	backend := newClientUDPBackend(t)
	budget := newUDPSessionBudget(2)
	handler, clientConn, serverConn := newClientUDPSetupHandler(t, "udp-isolation.qmux.invalid", backend, budget)

	sendClientUDPDatagram(t, serverConn, 1, []byte("baseline"))
	if payload, _ := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(payload, []byte("baseline")) {
		t.Fatalf("baseline payload = %q", payload)
	}
	awaitUDPState(t, handler, 1, udpSessionPhaseReady)
	awaitClientUDPCondition(t, "baseline response worker", func() bool {
		return handler.dsendStats.load().Workers == 1
	})

	gate.stall.Store(true)
	sendClientUDPDatagram(t, serverConn, 2, []byte("new-session"))
	awaitUDPHandler(t, gate.entered, "new session DNS gate")
	_, pending := awaitUDPState(t, handler, 2, udpSessionPhaseCollecting)
	if pending.packetCount != 1 || pending.retainedBytes != protocol.UDPHeaderSize+len("new-session") || pending.candidate != nil || pending.session != nil {
		t.Fatalf("blocked session state = %+v", pending)
	}
	if snapshot := budget.snapshot(); snapshot.Current != 1 || snapshot.Permits != 2 || handler.dsendStats.load().Workers != 1 {
		t.Fatalf("blocked session accounting = budget %+v/workers %d", snapshot, handler.dsendStats.load().Workers)
	}

	sendClientUDPDatagram(t, serverConn, 1, []byte("existing-during-dns"))
	if payload, _ := readClientUDPBackend(t, backend, 500*time.Millisecond); !bytes.Equal(payload, []byte("existing-during-dns")) {
		t.Fatalf("existing session payload while DNS blocked = %q", payload)
	}
	gate.unblock()
	if payload, _ := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(payload, []byte("new-session")) {
		t.Fatalf("new session payload after DNS release = %q", payload)
	}
	awaitUDPState(t, handler, 2, udpSessionPhaseReady)
	awaitClientUDPCondition(t, "both response workers", func() bool {
		return handler.dsendStats.load().Workers == 2
	})

	_ = clientConn.CloseWithError(0, "verify cleanup")
	handler.stopAndWait()
	assertNoUDPSessions(t, handler)
	if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 0 || snapshot.AccountingFaults != 0 || handler.dsendStats.load().Workers != 0 {
		t.Fatalf("A/B cleanup = budget %+v/workers %d", snapshot, handler.dsendStats.load().Workers)
	}
}

func TestUDPHandlerPendingFIFOIsBoundedOrderedAndDeduplicated(t *testing.T) {
	if runClientUDPSetupSubprocess(t) {
		return
	}
	t.Run("collecting", func(t *testing.T) {
		gate := installClientUDPResolverGate(t)
		backend := newClientUDPBackend(t)
		budget := newUDPSessionBudget(1)
		handler, _, serverConn := newClientUDPSetupHandler(t, "udp-fifo.qmux.invalid", backend, budget)
		gate.stall.Store(true)

		const sessionID = uint32(10)
		payloads := make([][]byte, udpPendingPacketLimit)
		expectedBacking := 0
		for i := range payloads {
			payloads[i] = []byte(fmt.Sprintf("fifo-%02d", i))
			expectedBacking += protocol.UDPHeaderSize + len(payloads[i])
			sendClientUDPDatagram(t, serverConn, sessionID, payloads[i])
		}
		awaitUDPHandler(t, gate.entered, "FIFO DNS gate")
		state, pending := awaitUDPState(t, handler, sessionID, udpSessionPhaseCollecting)
		awaitClientUDPCondition(t, "eight pending FIFO packets", func() bool {
			pending = snapshotUDPState(state)
			return pending.packetCount == udpPendingPacketLimit
		})
		if pending.retainedBytes != expectedBacking || pending.candidate != nil || pending.session != nil {
			t.Fatalf("collecting FIFO = %+v, want %d retained bytes", pending, expectedBacking)
		}
		sendClientUDPDatagram(t, serverConn, sessionID, []byte("ninth"))
		awaitClientUDPCondition(t, "ninth packet drop", func() bool {
			return budget.snapshot().PendingDrops == 1
		})
		if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 1 || handler.epochAllocator.Load() != 0 || handler.dsendStats.load().Workers != 0 {
			t.Fatalf("collecting FIFO accounting = budget %+v/epoch %d/workers %d", snapshot, handler.epochAllocator.Load(), handler.dsendStats.load().Workers)
		}

		gate.unblock()
		var source string
		for i, want := range payloads {
			got, addr := readClientUDPBackend(t, backend, 2*time.Second)
			if !bytes.Equal(got, want) {
				t.Fatalf("FIFO payload %d = %q, want %q", i, got, want)
			}
			if i == 0 {
				source = addr.String()
			} else if addr.String() != source {
				t.Fatalf("FIFO packet %d came from %s, want sole socket %s", i, addr, source)
			}
		}
		_, ready := awaitUDPState(t, handler, sessionID, udpSessionPhaseReady)
		awaitClientUDPCondition(t, "FIFO response worker", func() bool {
			return handler.dsendStats.load().Workers == 1
		})
		if ready.session == nil || ready.session.epoch != 1 || handler.epochAllocator.Load() != 1 {
			t.Fatalf("FIFO ready state = %+v/allocator %d", ready, handler.epochAllocator.Load())
		}
	})

	t.Run("draining", func(t *testing.T) {
		backend := newClientUDPBackend(t)
		budget := newUDPSessionBudget(1)
		clientConn, serverConn := newUDPHandlerQUICPair(t)
		handler := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true,
			config.DefaultMaxUDPFragmentGroupsPerHandler,
			config.DefaultMaxUDPFragmentBackingBytesPerHandler,
			zerolog.Nop(), budget)
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		handler.beforeSessionPublish = func() {
			handler.lifecycleMu.Lock()
			handler.lifecycleMu.Unlock()
			handler.sessionsMu.Lock()
			state := handler.sessions[11]
			handler.sessionsMu.Unlock()
			state.mu.Lock()
			phase := state.phase
			state.mu.Unlock()
			if phase != udpSessionPhaseDraining {
				panic(fmt.Sprintf("publish hook phase = %d", phase))
			}
			close(entered)
			<-release
		}
		handler.Start(context.Background(), clientConn)
		t.Cleanup(func() {
			_ = clientConn.CloseWithError(0, "test complete")
			handler.stopAndWait()
		})
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

		sendClientUDPDatagram(t, serverConn, 11, []byte("flushed"))
		awaitUDPHandler(t, entered, "draining publish gate")
		if payload, _ := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(payload, []byte("flushed")) {
			t.Fatalf("flushed payload = %q", payload)
		}
		_, draining := awaitUDPState(t, handler, 11, udpSessionPhaseDraining)
		if draining.packetCount != 0 || draining.candidate == nil || draining.session != nil {
			t.Fatalf("draining state = %+v", draining)
		}
		sendClientUDPDatagram(t, serverConn, 11, []byte("drop-during-drain"))
		awaitClientUDPCondition(t, "draining packet drop", func() bool {
			return budget.snapshot().PendingDrops == 1
		})
		releaseOnce.Do(func() { close(release) })
		awaitUDPState(t, handler, 11, udpSessionPhaseReady)
		sendClientUDPDatagram(t, serverConn, 11, []byte("ready"))
		if payload, _ := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(payload, []byte("ready")) {
			t.Fatalf("ready payload after finite flush = %q", payload)
		}
	})
}

func TestUDPHandlerPendingPayloadOwnershipAndZeroLength(t *testing.T) {
	if runClientUDPSetupSubprocess(t) {
		return
	}
	t.Run("normal backing", func(t *testing.T) {
		gate := installClientUDPResolverGate(t)
		backend := newClientUDPBackend(t)
		handler, _, serverConn := newClientUDPSetupHandler(t, "udp-normal-owner.qmux.invalid", backend, newUDPSessionBudget(1))
		gate.stall.Store(true)

		want := bytes.Repeat([]byte{0x7b}, protocol.MaxUDPPayload)
		wire := clientUDPDatagram(20, want)
		if err := serverConn.SendDatagram(wire); err != nil {
			t.Fatal(err)
		}
		awaitUDPHandler(t, gate.entered, "normal ownership DNS gate")
		state, _ := awaitUDPState(t, handler, 20, udpSessionPhaseCollecting)
		clear(wire)
		for i := 1; i < udpPendingPacketLimit; i++ {
			sendClientUDPDatagram(t, serverConn, 20, []byte{byte(i)})
		}
		awaitClientUDPCondition(t, "normal ownership receive iterations", func() bool {
			return snapshotUDPState(state).packetCount == udpPendingPacketLimit
		})
		gate.unblock()
		if got, _ := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(got, want) {
			t.Fatalf("retained normal payload changed: got %d bytes", len(got))
		}
		for i := 1; i < udpPendingPacketLimit; i++ {
			if got, _ := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(got, []byte{byte(i)}) {
				t.Fatalf("normal payload %d = %v", i, got)
			}
		}
	})

	t.Run("zero length", func(t *testing.T) {
		gate := installClientUDPResolverGate(t)
		backend := newClientUDPBackend(t)
		handler, _, serverConn := newClientUDPSetupHandler(t, "udp-zero.qmux.invalid", backend, newUDPSessionBudget(1))
		gate.stall.Store(true)

		sendClientUDPDatagram(t, serverConn, 21, nil)
		awaitUDPHandler(t, gate.entered, "zero-length DNS gate")
		state, _ := awaitUDPState(t, handler, 21, udpSessionPhaseCollecting)
		sendClientUDPDatagram(t, serverConn, 21, []byte("after-zero"))
		awaitClientUDPCondition(t, "zero-length pending pair", func() bool {
			return snapshotUDPState(state).packetCount == 2
		})
		pending := snapshotUDPState(state)
		if pending.retainedBytes != 2*protocol.UDPHeaderSize+len("after-zero") {
			t.Fatalf("zero-length retained bytes = %d", pending.retainedBytes)
		}
		gate.unblock()
		zero, source := readClientUDPBackend(t, backend, 2*time.Second)
		if len(zero) != 0 {
			t.Fatalf("zero-length datagram = %q", zero)
		}
		if got, addr := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(got, []byte("after-zero")) || addr.String() != source.String() {
			t.Fatalf("payload after zero = %q from %v, want same socket %v", got, addr, source)
		}
		awaitUDPState(t, handler, 21, udpSessionPhaseReady)
	})

	t.Run("fragment join", func(t *testing.T) {
		gate := installClientUDPResolverGate(t)
		backend := newClientUDPBackend(t)
		budget := newUDPSessionBudget(1)
		handler, _, serverConn := newClientUDPSetupHandler(t, "udp-fragment-owner.qmux.invalid", backend, budget)
		gate.stall.Store(true)

		want := bytes.Repeat([]byte{0xa6}, protocol.MaxUDPPayload+200)
		var sequence uint32
		fragments, err := protocol.FragmentUDP(22, 7, want, &sequence, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := serverConn.SendDatagram(fragments[0]); err != nil {
			t.Fatal(err)
		}
		awaitClientUDPCondition(t, "incomplete fragment retention", func() bool {
			return handler.fragmentAssembler.Snapshot().RetainedGroups == 1
		})
		if handler.loadSessionState(22) != nil || budget.snapshot().Permits != 0 {
			t.Fatal("incomplete fragment created a UDP session")
		}
		for _, fragment := range fragments[1:] {
			if err := serverConn.SendDatagram(fragment); err != nil {
				t.Fatal(err)
			}
		}
		awaitUDPHandler(t, gate.entered, "fragment ownership DNS gate")
		state, pending := awaitUDPState(t, handler, 22, udpSessionPhaseCollecting)
		if pending.packetCount != 1 || pending.retainedBytes != len(want) {
			t.Fatalf("joined fragment pending state = %+v", pending)
		}
		if snapshot := handler.fragmentAssembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
			t.Fatalf("completed fragment retained assembler ownership: %+v", snapshot)
		}
		for _, fragment := range fragments {
			clear(fragment)
		}
		sendClientUDPDatagram(t, serverConn, 22, []byte("after-fragment"))
		awaitClientUDPCondition(t, "fragment and successor pending", func() bool {
			return snapshotUDPState(state).packetCount == 2
		})
		gate.unblock()
		first, source := readClientUDPBackend(t, backend, 2*time.Second)
		if !bytes.Equal(first, want) {
			t.Fatalf("joined fragment payload changed: got %d bytes", len(first))
		}
		if got, addr := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(got, []byte("after-fragment")) || addr.String() != source.String() {
			t.Fatalf("payload after fragment = %q from %v, want same socket %v", got, addr, source)
		}
	})
}

func TestUDPHandlerPendingSetupDeadlineReleasesResources(t *testing.T) {
	if runClientUDPSetupSubprocess(t) {
		return
	}
	entered := make(chan struct{})
	var enteredOnce sync.Once
	previousResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			enteredOnce.Do(func() { close(entered) })
			<-ctx.Done()
			return nil, context.Cause(ctx)
		},
	}
	t.Cleanup(func() { net.DefaultResolver = previousResolver })
	backend := newClientUDPBackend(t)
	budget := newUDPSessionBudget(1)
	handler, _, serverConn := newClientUDPSetupHandler(t, "udp-deadline.qmux.invalid", backend, budget)

	sendClientUDPDatagram(t, serverConn, 30, []byte("deadline"))
	awaitUDPHandler(t, entered, "setup deadline DNS")
	_, pending := awaitUDPState(t, handler, 30, udpSessionPhaseCollecting)
	if pending.packetCount != 1 || budget.snapshot().Permits != 1 || handler.dsendStats.load().Workers != 0 {
		t.Fatalf("deadline pending state = %+v/budget %+v/workers %d", pending, budget.snapshot(), handler.dsendStats.load().Workers)
	}
	awaitClientUDPConditionWithin(t, udpSessionSetupTimeout+2*time.Second, "setup deadline cleanup", func() bool {
		snapshot := budget.snapshot()
		return handler.loadSessionState(30) == nil && snapshot.Permits == 0 && snapshot.CreateErrors == 1
	})
	handler.lifecycleMu.Lock()
	live := handler.started && !handler.closed && handler.ctx.Err() == nil
	handler.lifecycleMu.Unlock()
	if !live {
		t.Fatal("setup timeout stopped the live handler")
	}
	if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.WriteErrors != 0 || snapshot.AccountingFaults != 0 {
		t.Fatalf("setup deadline counters = %+v", snapshot)
	}
}

func TestUDPHandlerFailureCountersAreOperationClassified(t *testing.T) {
	if runClientUDPSetupSubprocess(t) {
		return
	}
	t.Run("initial write error", func(t *testing.T) {
		gate := installClientUDPResolverGate(t)
		backend := newClientUDPBackend(t)
		budget := newUDPSessionBudget(1)
		handler, _, serverConn := newClientUDPSetupHandler(t, "udp-write-error.qmux.invalid", backend, budget)
		gate.stall.Store(true)

		payload := bytes.Repeat([]byte{0xe1}, udpPendingBackingLimit)
		var sequence uint32
		fragments, err := protocol.FragmentUDP(31, 1, payload, &sequence, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(fragments) != 255 {
			t.Fatalf("maximum payload fragments = %d, want 255", len(fragments))
		}
		for i, fragment := range fragments {
			if err := serverConn.SendDatagram(fragment); err != nil {
				t.Fatal(err)
			}
			if i+1 < len(fragments) {
				wantRetained := int64(i+1) * int64(protocol.FragmentBufferSize)
				awaitClientUDPCondition(t, "maximum payload fragment consumption", func() bool {
					return handler.fragmentAssembler.Snapshot().RetainedBackingBytes == wantRetained
				})
			}
		}
		awaitUDPHandler(t, gate.entered, "oversized payload DNS gate")
		_, pending := awaitUDPState(t, handler, 31, udpSessionPhaseCollecting)
		if pending.packetCount != 1 || pending.retainedBytes != udpPendingBackingLimit {
			t.Fatalf("maximum pending payload = %+v", pending)
		}
		if snapshot := handler.fragmentAssembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
			t.Fatalf("maximum payload retained fragments: %+v", snapshot)
		}
		sendClientUDPDatagram(t, serverConn, 31, nil)
		awaitClientUDPCondition(t, "pending byte-cap drop", func() bool {
			return budget.snapshot().PendingDrops == 1
		})
		gate.unblock()
		awaitClientUDPCondition(t, "initial write failure cleanup", func() bool {
			snapshot := budget.snapshot()
			return handler.loadSessionState(31) == nil && snapshot.Permits == 0 && snapshot.WriteErrors == 1
		})
		if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.CreateErrors != 0 || snapshot.WriteErrors != 1 || snapshot.PendingDrops != 1 || snapshot.AccountingFaults != 0 || handler.dsendStats.load().Workers != 0 {
			t.Fatalf("initial write failure counters = budget %+v/workers %d", snapshot, handler.dsendStats.load().Workers)
		}
	})

	t.Run("Stop closed before cancel", func(t *testing.T) {
		backend := newClientUDPBackend(t)
		clientConn, _ := newUDPHandlerQUICPair(t)
		budget := newUDPSessionBudget(1)
		handler := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true,
			config.DefaultMaxUDPFragmentGroupsPerHandler,
			config.DefaultMaxUDPFragmentBackingBytesPerHandler,
			zerolog.Nop(), budget)
		startUDPHandlerForDispatch(t, handler, clientConn)
		publishEntered := make(chan struct{})
		publishRelease := make(chan struct{})
		cancelEntered := make(chan struct{})
		cancelRelease := make(chan struct{})
		var publishOnce, cancelOnce sync.Once
		t.Cleanup(func() {
			publishOnce.Do(func() { close(publishRelease) })
			cancelOnce.Do(func() { close(cancelRelease) })
		})
		handler.beforeSessionPublish = func() {
			close(publishEntered)
			<-publishRelease
		}
		handler.lifecycleMu.Lock()
		originalCancel := handler.cancel
		handler.cancel = func() {
			close(cancelEntered)
			<-cancelRelease
			originalCancel()
		}
		handler.lifecycleMu.Unlock()

		handler.dispatchPayload(32, udpPendingPacket{payload: []byte("stop"), retainedBytes: len("stop")}, clientConn)
		awaitUDPHandler(t, publishEntered, "Stop counter publish gate")
		stopDone := make(chan struct{})
		go func() {
			handler.Stop()
			close(stopDone)
		}()
		awaitUDPHandler(t, cancelEntered, "Stop cancel gate")
		handler.lifecycleMu.Lock()
		closedBeforeCancel := handler.closed && handler.ctx.Err() == nil
		handler.lifecycleMu.Unlock()
		if !closedBeforeCancel {
			t.Fatal("Stop did not expose the closed-before-cancel window")
		}
		publishOnce.Do(func() { close(publishRelease) })
		awaitClientUDPCondition(t, "closed-before-cancel worker cleanup", func() bool {
			return handler.loadSessionState(32) == nil && budget.snapshot().Permits == 0
		})
		if snapshot := budget.snapshot(); snapshot.CreateErrors != 0 || snapshot.WriteErrors != 0 || snapshot.Current != 0 || snapshot.AccountingFaults != 0 {
			t.Fatalf("closed-before-cancel counters = %+v", snapshot)
		}
		cancelOnce.Do(func() { close(cancelRelease) })
		awaitUDPHandler(t, stopDone, "Stop after cancel release")
	})

	t.Run("owning QUIC canceled", func(t *testing.T) {
		backend := newClientUDPBackend(t)
		clientConn, serverConn := newUDPHandlerQUICPair(t)
		budget := newUDPSessionBudget(1)
		handler := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true,
			config.DefaultMaxUDPFragmentGroupsPerHandler,
			config.DefaultMaxUDPFragmentBackingBytesPerHandler,
			zerolog.Nop(), budget)
		startUDPHandlerForDispatch(t, handler, clientConn)
		publishEntered := make(chan struct{})
		publishRelease := make(chan struct{})
		var publishOnce sync.Once
		t.Cleanup(func() { publishOnce.Do(func() { close(publishRelease) }) })
		handler.beforeSessionPublish = func() {
			close(publishEntered)
			<-publishRelease
		}

		handler.dispatchPayload(33, udpPendingPacket{payload: []byte("quic-close"), retainedBytes: len("quic-close")}, clientConn)
		awaitUDPHandler(t, publishEntered, "QUIC cancellation publish gate")
		if err := serverConn.CloseWithError(1, "cancel owner"); err != nil {
			t.Fatal(err)
		}
		awaitUDPHandler(t, clientConn.Context().Done(), "owning QUIC cancellation")
		handler.lifecycleMu.Lock()
		handlerStillLive := !handler.closed && handler.ctx.Err() == nil
		handler.lifecycleMu.Unlock()
		if !handlerStillLive {
			t.Fatal("manual handler lifecycle changed before QUIC-only cancellation check")
		}
		publishOnce.Do(func() { close(publishRelease) })
		awaitClientUDPCondition(t, "QUIC-canceled worker cleanup", func() bool {
			return handler.loadSessionState(33) == nil && budget.snapshot().Permits == 0
		})
		if snapshot := budget.snapshot(); snapshot.CreateErrors != 0 || snapshot.WriteErrors != 0 || snapshot.Current != 0 || snapshot.AccountingFaults != 0 {
			t.Fatalf("QUIC cancellation counters = %+v", snapshot)
		}
	})
}

func TestUDPHandlerReadySnapshotSurvivesConcurrentClose(t *testing.T) {
	backend := newClientUDPBackend(t)
	clientConn, _ := newUDPHandlerQUICPair(t)
	budget := newUDPSessionBudget(2)
	handler := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true,
		config.DefaultMaxUDPFragmentGroupsPerHandler,
		config.DefaultMaxUDPFragmentBackingBytesPerHandler,
		zerolog.Nop(), budget)
	startUDPHandlerForDispatch(t, handler, clientConn)

	const sessionID = uint32(40)
	handler.dispatchPayload(sessionID, udpPendingPacket{payload: []byte("old"), retainedBytes: len("old")}, clientConn)
	readClientUDPBackend(t, backend, 2*time.Second)
	oldState, oldSnapshot := awaitUDPState(t, handler, sessionID, udpSessionPhaseReady)
	decision, exactSession := oldState.acceptAndSnapshot(udpPendingPacket{payload: []byte("raced"), retainedBytes: len("raced")})
	if decision != udpDispatchReadyWrite || exactSession == nil || exactSession != oldSnapshot.session {
		t.Fatalf("ready snapshot = decision %d/session %p, want exact %p", decision, exactSession, oldSnapshot.session)
	}
	handler.closeSessionExact(oldState, exactSession)
	if _, err := exactSession.localConn.Write([]byte("raced")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("snapshotted session write after close = %v, want net.ErrClosed", err)
	}

	handler.dispatchPayload(sessionID, udpPendingPacket{payload: []byte("successor"), retainedBytes: len("successor")}, clientConn)
	if payload, _ := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(payload, []byte("successor")) {
		t.Fatalf("successor payload = %q", payload)
	}
	newState, newSnapshot := awaitUDPState(t, handler, sessionID, udpSessionPhaseReady)
	handler.closeSessionExact(oldState, exactSession)
	if handler.loadSessionState(sessionID) != newState || newSnapshot.session == nil || newSnapshot.session == exactSession {
		t.Fatalf("stale close changed same-ID successor: old=%p new=%p mapped=%p", exactSession, newSnapshot.session, handler.loadSessionState(sessionID))
	}
}

func TestUDPHandlerReadySessionOutlivesSetupDeadline(t *testing.T) {
	backend := newClientUDPBackend(t)
	handler, _, serverConn := newClientUDPSetupHandler(t, "127.0.0.1", backend, newUDPSessionBudget(1))

	const sessionID = uint32(41)
	sendClientUDPDatagram(t, serverConn, sessionID, []byte("baseline"))
	if payload, source := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(payload, []byte("baseline")) {
		t.Fatalf("ready baseline = %q", payload)
	} else {
		awaitUDPState(t, handler, sessionID, udpSessionPhaseReady)
		timer := time.NewTimer(udpSessionSetupTimeout + 200*time.Millisecond)
		<-timer.C
		sendClientUDPDatagram(t, serverConn, sessionID, []byte("after-deadline"))
		if got, _ := readClientUDPBackend(t, backend, 2*time.Second); !bytes.Equal(got, []byte("after-deadline")) {
			t.Fatalf("forward payload after setup deadline = %q", got)
		}
		if _, err := backend.WriteToUDP([]byte("reply-after-deadline"), source); err != nil {
			t.Fatal(err)
		}
		if gotID, got := readClientUDPResponse(t, serverConn, 2*time.Second); gotID != sessionID || !bytes.Equal(got, []byte("reply-after-deadline")) {
			t.Fatalf("reply after setup deadline = session %d payload %q", gotID, got)
		}
	}
}

func TestUDPHandlerStopJoinsPendingAndReadyWorkers(t *testing.T) {
	if runClientUDPSetupSubprocess(t) {
		return
	}
	t.Run("collecting", func(t *testing.T) {
		entered := make(chan struct{})
		canceled := make(chan struct{})
		release := make(chan struct{})
		var enteredOnce, canceledOnce, releaseOnce sync.Once
		previousResolver := net.DefaultResolver
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				enteredOnce.Do(func() { close(entered) })
				<-ctx.Done()
				canceledOnce.Do(func() { close(canceled) })
				<-release
				return nil, context.Cause(ctx)
			},
		}
		t.Cleanup(func() {
			releaseOnce.Do(func() { close(release) })
			net.DefaultResolver = previousResolver
		})
		backend := newClientUDPBackend(t)
		budget := newUDPSessionBudget(1)
		handler, _, serverConn := newClientUDPSetupHandler(t, "udp-stop-collecting.qmux.invalid", backend, budget)

		sendClientUDPDatagram(t, serverConn, 50, []byte("collecting"))
		awaitUDPHandler(t, entered, "collecting DNS")
		state, pending := awaitUDPState(t, handler, 50, udpSessionPhaseCollecting)
		if pending.packetCount != 1 || budget.snapshot().Permits != 1 || handler.dsendStats.load().Workers != 0 {
			t.Fatalf("collecting before Stop = state %+v/budget %+v/workers %d", pending, budget.snapshot(), handler.dsendStats.load().Workers)
		}
		stopDone := make(chan struct{})
		go func() {
			handler.Stop()
			close(stopDone)
		}()
		awaitUDPHandler(t, canceled, "collecting resolver cancellation")
		awaitUDPHandler(t, stopDone, "collecting Stop")
		if handler.loadSessionState(50) != nil || snapshotUDPState(state).phase != udpSessionPhaseClosed {
			t.Fatal("collecting state survived Stop")
		}
		waitDone := make(chan struct{})
		go func() {
			handler.wait()
			close(waitDone)
		}()
		awaitUDPHandler(t, waitDone, "collecting worker cancellation")
		if snapshot := budget.snapshot(); snapshot.Permits != 0 || snapshot.Current != 0 || snapshot.CreateErrors != 0 || snapshot.WriteErrors != 0 || snapshot.AccountingFaults != 0 {
			t.Fatalf("collecting worker cleanup = %+v", snapshot)
		}
		releaseOnce.Do(func() { close(release) })
	})

	t.Run("draining", func(t *testing.T) {
		backend := newClientUDPBackend(t)
		clientConn, _ := newUDPHandlerQUICPair(t)
		budget := newUDPSessionBudget(1)
		handler := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true,
			config.DefaultMaxUDPFragmentGroupsPerHandler,
			config.DefaultMaxUDPFragmentBackingBytesPerHandler,
			zerolog.Nop(), budget)
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		handler.beforeSessionPublish = func() {
			close(entered)
			<-release
		}
		startUDPHandlerForDispatch(t, handler, clientConn)
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

		handler.dispatchPayload(51, udpPendingPacket{payload: []byte("draining"), retainedBytes: len("draining")}, clientConn)
		awaitUDPHandler(t, entered, "draining Stop gate")
		readClientUDPBackend(t, backend, 2*time.Second)
		state, draining := awaitUDPState(t, handler, 51, udpSessionPhaseDraining)
		if draining.candidate == nil || draining.session != nil || handler.dsendStats.load().Workers != 0 {
			t.Fatalf("draining before Stop = state %+v/workers %d", draining, handler.dsendStats.load().Workers)
		}
		handler.Stop()
		if handler.loadSessionState(51) != nil || snapshotUDPState(state).phase != udpSessionPhaseClosed {
			t.Fatal("draining state survived Stop")
		}
		if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 1 || snapshot.CreateErrors != 0 || snapshot.WriteErrors != 0 {
			t.Fatalf("draining closed accounting = %+v, want permit held by worker", snapshot)
		}
		waitDone := make(chan struct{})
		go func() {
			handler.wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
			t.Fatal("draining wait returned before worker exit")
		case <-time.After(50 * time.Millisecond):
		}
		releaseOnce.Do(func() { close(release) })
		awaitUDPHandler(t, waitDone, "draining worker exit")
		if snapshot := budget.snapshot(); snapshot.Permits != 0 || snapshot.Current != 0 || snapshot.AccountingFaults != 0 {
			t.Fatalf("draining worker cleanup = %+v", snapshot)
		}
	})

	t.Run("ready", func(t *testing.T) {
		backend := newClientUDPBackend(t)
		budget := newUDPSessionBudget(1)
		handler, clientConn, serverConn := newClientUDPSetupHandler(t, "127.0.0.1", backend, budget)
		sendClientUDPDatagram(t, serverConn, 52, []byte("ready"))
		readClientUDPBackend(t, backend, 2*time.Second)
		_, ready := awaitUDPState(t, handler, 52, udpSessionPhaseReady)
		awaitClientUDPCondition(t, "ready response worker", func() bool {
			return handler.dsendStats.load().Workers == 1
		})
		_ = clientConn.CloseWithError(0, "ready retirement")
		handler.Stop()
		handler.wait()
		if handler.loadSessionState(52) != nil {
			t.Fatal("ready state survived Stop")
		}
		if _, err := ready.session.localConn.Write([]byte("closed")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("ready socket after Stop = %v, want net.ErrClosed", err)
		}
		if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 0 || snapshot.CreateErrors != 0 || snapshot.WriteErrors != 0 || snapshot.AccountingFaults != 0 || handler.dsendStats.load().Workers != 0 {
			t.Fatalf("ready worker cleanup = budget %+v/workers %d", snapshot, handler.dsendStats.load().Workers)
		}
	})
}
