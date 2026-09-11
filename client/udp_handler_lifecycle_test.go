package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/internal/testutil"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const clientUDPResolverChild = "QMUX_CLIENT_UDP_RESOLVER_CHILD"

func newClientUDPBackend(t *testing.T) *net.UDPConn {
	t.Helper()
	backend, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend
}

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

func awaitClientUDPCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	awaitClientUDPConditionWithin(t, 3*time.Second, description, condition)
}

func awaitClientUDPConditionWithin(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", description)
		case <-ticker.C:
		}
	}
}

func literalClientUDPFragment(sessionID uint32, fragmentID uint64, index, total byte, payload []byte) []byte {
	wire := make([]byte, 0, 15+len(payload))
	wire = append(wire, 0x22)
	wire = binary.BigEndian.AppendUint32(wire, sessionID)
	wire = binary.BigEndian.AppendUint64(wire, fragmentID)
	wire = append(wire, index, total)
	return append(wire, payload...)
}

func clientUDPDatagram(sessionID uint32, payload []byte) []byte {
	wire := binary.BigEndian.AppendUint32([]byte{protocol.UDPDatagramTypeNormal}, sessionID)
	return append(wire, payload...)
}

func sendClientUDPDatagram(t *testing.T, conn *quic.Conn, sessionID uint32, payload []byte) {
	t.Helper()
	if err := conn.SendDatagram(clientUDPDatagram(sessionID, payload)); err != nil {
		t.Fatalf("send client UDP datagram: %v", err)
	}
}

func readClientUDPBackend(t *testing.T, backend *net.UDPConn, timeout time.Duration) ([]byte, *net.UDPAddr) {
	t.Helper()
	if err := backend.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64<<10)
	n, addr, err := backend.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read client UDP backend: %v", err)
	}
	return buf[:n], addr
}

func readClientUDPResponse(t *testing.T, conn *quic.Conn, timeout time.Duration) (uint32, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	wire, err := conn.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatalf("read client UDP response: %v", err)
	}
	parsed, err := protocol.DecodeUDPDatagram(wire)
	if err != nil {
		t.Fatalf("decode client UDP response: %v", err)
	}
	return parsed.SessionID, parsed.Payload
}

type udpSessionTestSnapshot struct {
	phase         udpSessionPhase
	packetCount   int
	retainedBytes int
	candidate     *net.UDPConn
	session       *UDPSession
}

func snapshotUDPState(state *udpSessionState) udpSessionTestSnapshot {
	state.mu.Lock()
	defer state.mu.Unlock()
	return udpSessionTestSnapshot{
		phase:         state.phase,
		packetCount:   len(state.fifo),
		retainedBytes: state.retainedBytes,
		candidate:     state.candidate,
		session:       state.session,
	}
}

func awaitUDPState(t *testing.T, handler *UDPHandler, sessionID uint32, phase udpSessionPhase) (*udpSessionState, udpSessionTestSnapshot) {
	t.Helper()
	var state *udpSessionState
	var snapshot udpSessionTestSnapshot
	awaitClientUDPCondition(t, "UDP session state", func() bool {
		state = handler.loadSessionState(sessionID)
		if state == nil {
			return false
		}
		snapshot = snapshotUDPState(state)
		return snapshot.phase == phase
	})
	return state, snapshot
}

func startUDPHandlerForDispatch(t *testing.T, handler *UDPHandler, quicConn *quic.Conn) {
	t.Helper()
	handler.lifecycleMu.Lock()
	handler.ctx, handler.cancel = context.WithCancel(context.Background())
	handler.started = true
	handler.lifecycleMu.Unlock()
	t.Cleanup(func() {
		_ = quicConn.CloseWithError(0, "test complete")
		handler.stopAndWait()
	})
}

func assertNoUDPSessions(t *testing.T, handler *UDPHandler) {
	t.Helper()
	handler.sessionsMu.Lock()
	count := len(handler.sessions)
	handler.sessionsMu.Unlock()
	if count != 0 {
		t.Fatalf("handler retained %d UDP sessions", count)
	}
}

func TestUDPHandlerHostnameResolveCancellationReleasesResources(t *testing.T) {
	if os.Getenv(clientUDPResolverChild) != "" {
		runUDPHandlerResolverChild(t)
		return
	}
	testutil.RunResolverCancellationProcess(t, clientUDPResolverChild)
}

func runUDPHandlerResolverChild(t *testing.T) {
	var entered sync.Once
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			entered.Do(func() { _, _ = fmt.Fprintln(os.Stdout, "resolver-entered") })
			<-ctx.Done()
			return nil, context.Cause(ctx)
		},
	}

	backend := newClientUDPBackend(t)
	clientConn, serverConn := newUDPHandlerQUICPair(t)
	budget := newUDPSessionBudget(1)
	handler := newUDPHandler("lif002-client.qmux.invalid", backend.LocalAddr().(*net.UDPAddr).Port, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop(), budget)
	handler.Start(context.Background(), clientConn)
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		handler.Stop()
	}()

	sendClientUDPDatagram(t, serverConn, 1, []byte("blocked"))
	awaitUDPHandler(t, handler.done, "resolver cancellation cleanup")
	assertNoUDPSessions(t, handler)
	if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 0 || snapshot.CreateErrors != 0 || snapshot.WriteErrors != 0 {
		t.Fatalf("canceled session budget = %+v, want no current session, permit, or error", snapshot)
	}
	if workers := handler.dsendStats.load().Workers; workers != 0 {
		t.Fatalf("canceled session workers = %d, want 0", workers)
	}
}

func TestUDPDecodeErrorsIgnoreClosedAssembler(t *testing.T) {
	handler := NewUDPHandler("127.0.0.1", 1, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop())
	t.Cleanup(handler.Stop)
	var fragmentSequence uint32
	datagrams, err := protocol.FragmentUDP(1, 1, make([]byte, protocol.MaxUDPPayload+1), &fragmentSequence, true)
	if err != nil {
		t.Fatal(err)
	}

	// A queued valid fragment can still be decoded after Stop closes the assembler.
	handler.Stop()
	_, _, _, err = protocol.DecodeAndAssembleUDPDatagram(datagrams[0], handler.fragmentAssembler)
	if !errors.Is(err, protocol.ErrFragmentAssemblerClosed) {
		t.Fatalf("decode after Stop = %v, want closed assembler", err)
	}
	for _, ignored := range []error{err, errors.Join(errors.New("wrapped"), err), protocol.ErrFragmentAssemblerFull} {
		handler.recordDecodeError(ignored)
	}
	if got := handler.sessionBudget.snapshot().DecodeDrops; got != 0 {
		t.Fatalf("resource errors counted as decode drops: %d", got)
	}

	_, _, _, err = protocol.DecodeAndAssembleUDPDatagram(nil, handler.fragmentAssembler)
	if !errors.Is(err, protocol.ErrDatagramTooShort) {
		t.Fatalf("malformed datagram error = %v", err)
	}
	handler.recordDecodeError(err)
	if got := handler.sessionBudget.snapshot().DecodeDrops; got != 1 {
		t.Fatalf("malformed decode drops = %d, want 1", got)
	}
}

func TestUDPSessionBudgetBoundsSharedHandlersBeforeDial(t *testing.T) {
	if got := cap(newUDPSessionBudget(0).slots); got != config.DefaultMaxLocalUDPSessions {
		t.Fatalf("default UDP session limit = %d, want %d", got, config.DefaultMaxLocalUDPSessions)
	}

	budget := newUDPSessionBudget(1)
	release, ok := budget.acquire()
	if !ok {
		t.Fatal("initial UDP session budget acquisition failed")
	}
	first := newUDPHandler("invalid.invalid", 1, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop(), budget)
	second := newUDPHandler("invalid.invalid", 1, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop(), budget)
	t.Cleanup(first.Stop)
	t.Cleanup(second.Stop)

	for id, handler := range []*UDPHandler{first, second} {
		handler.dispatchPayload(uint32(id+1), udpPendingPacket{payload: []byte("blocked"), retainedBytes: len("blocked")}, nil)
		assertNoUDPSessions(t, handler)
	}
	snapshot := budget.snapshot()
	if snapshot.Permits != 1 || snapshot.CapacityDrops != 2 {
		t.Fatalf("shared UDP budget = %d held/%d drops, want 1/2", snapshot.Permits, snapshot.CapacityDrops)
	}
	if snapshot.HighWater != 1 {
		t.Fatalf("shared UDP budget high-water = %d, want 1", snapshot.HighWater)
	}

	release()
	release()
	snapshot = budget.snapshot()
	if snapshot.Permits != 0 || snapshot.AccountingFaults != 0 {
		t.Fatalf("released UDP budget = %d held/%d faults, want zero", snapshot.Permits, snapshot.AccountingFaults)
	}
}

func TestUDPSessionBudgetAccountingFaultFailsClosedAndDrainsExisting(t *testing.T) {
	budget := newUDPSessionBudget(2)
	release, ok := budget.acquire()
	if !ok {
		t.Fatal("initial UDP session budget acquisition failed")
	}
	budget.publish()
	budget.mu.Lock()
	budget.accountingFaults++
	budget.mu.Unlock()

	before := budget.snapshot()
	if rejectedRelease, ok := budget.acquire(); ok || rejectedRelease != nil {
		t.Fatal("UDP session budget acquisition succeeded after accounting fault")
	}
	after := budget.snapshot()
	if after.CapacityDrops != before.CapacityDrops || after.Current != 1 || after.Permits != 1 {
		t.Fatalf("fault rejection snapshot = %+v, want one existing session and no capacity drop", after)
	}

	budget.unpublish()
	release()
	final := budget.snapshot()
	if final.Current != 0 || final.Permits != 0 || final.AccountingFaults != 1 || final.CapacityDrops != 0 {
		t.Fatalf("drained fault snapshot = %+v, want zero current/permits, one fault, and no capacity drops", final)
	}

	t.Run("token present held underflow", func(t *testing.T) {
		budget := newUDPSessionBudget(1)
		release, ok := budget.acquire()
		if !ok {
			t.Fatal("initial UDP session budget acquisition failed")
		}
		budget.mu.Lock()
		budget.permitsHeld = 0
		budget.mu.Unlock()
		release()
		snapshot := budget.snapshot()
		if snapshot.Permits != 0 || len(budget.slots) != 0 || snapshot.AccountingFaults != 1 {
			t.Fatalf("held underflow snapshot = %+v, slots=%d, want restored zero held, empty channel, and one fault",
				snapshot, len(budget.slots))
		}
	})

	t.Run("missing token", func(t *testing.T) {
		budget := newUDPSessionBudget(1)
		release, ok := budget.acquire()
		if !ok {
			t.Fatal("initial UDP session budget acquisition failed")
		}
		budget.mu.Lock()
		<-budget.slots
		budget.mu.Unlock()
		release()
		snapshot := budget.snapshot()
		if snapshot.Permits != 1 || len(budget.slots) != 0 || snapshot.AccountingFaults != 1 {
			t.Fatalf("missing-token snapshot = %+v, slots=%d, want held unchanged, empty channel, and one fault",
				snapshot, len(budget.slots))
		}
	})

	t.Run("published active underflow", func(t *testing.T) {
		budget := newUDPSessionBudget(1)
		budget.unpublish()
		snapshot := budget.snapshot()
		if snapshot.Current != 0 || snapshot.AccountingFaults != 1 {
			t.Fatalf("published underflow snapshot = %+v, want restored zero current and one fault", snapshot)
		}
	})
}

func TestUDPHandlerCloseUsesExactSessionPointer(t *testing.T) {
	backend := newClientUDPBackend(t)
	clientConn, serverConn := newUDPHandlerQUICPair(t)
	budget := newUDPSessionBudget(1)
	handler := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop(), budget)
	startUDPHandlerForDispatch(t, handler, clientConn)

	const sessionID = uint32(7)
	handler.dispatchPayload(sessionID, udpPendingPacket{payload: []byte("old"), retainedBytes: len("old")}, clientConn)
	readClientUDPBackend(t, backend, 3*time.Second)
	oldState, oldSnapshot := awaitUDPState(t, handler, sessionID, udpSessionPhaseReady)
	oldSession := oldSnapshot.session
	handler.closeSessionExact(oldState, oldSession)
	awaitClientUDPCondition(t, "old UDP worker exit", func() bool {
		snapshot := budget.snapshot()
		return snapshot.Current == 0 && snapshot.Permits == 0 && handler.dsendStats.load().Workers == 0
	})

	handler.dispatchPayload(sessionID, udpPendingPacket{payload: []byte("new"), retainedBytes: len("new")}, clientConn)
	_, successorAddr := readClientUDPBackend(t, backend, 3*time.Second)
	newState, newSnapshot := awaitUDPState(t, handler, sessionID, udpSessionPhaseReady)
	awaitClientUDPCondition(t, "replacement UDP worker", func() bool {
		return handler.dsendStats.load().Workers == 1
	})
	handler.closeSessionExact(oldState, oldSession)
	if got := handler.loadSessionState(sessionID); got != newState {
		t.Fatalf("stale close changed replacement = %p, want %p", got, newState)
	}
	if snapshot := budget.snapshot(); snapshot.Current != 1 || snapshot.Permits != 1 {
		t.Fatalf("published sessions after stale close = %+v, want one current and permit", snapshot)
	}
	handler.dispatchPayload(sessionID, udpPendingPacket{payload: []byte("successor-forward"), retainedBytes: len("successor-forward")}, clientConn)
	if payload, _ := readClientUDPBackend(t, backend, 3*time.Second); !bytes.Equal(payload, []byte("successor-forward")) {
		t.Fatalf("replacement forward payload = %q", payload)
	}
	if _, err := backend.WriteToUDP([]byte("successor-reply"), successorAddr); err != nil {
		t.Fatal(err)
	}
	if gotID, payload := readClientUDPResponse(t, serverConn, 3*time.Second); gotID != sessionID || !bytes.Equal(payload, []byte("successor-reply")) {
		t.Fatalf("replacement reply = session %d payload %q", gotID, payload)
	}

	handler.closeSessionExact(newState, newSnapshot.session)
	awaitClientUDPCondition(t, "replacement UDP worker exit", func() bool {
		snapshot := budget.snapshot()
		return snapshot.Current == 0 && snapshot.Permits == 0 && handler.dsendStats.load().Workers == 0
	})
	if snapshot := budget.snapshot(); snapshot.AccountingFaults != 0 {
		t.Fatalf("exact close budget = %d active/%d held/%d faults, want zero",
			snapshot.Current, snapshot.Permits, snapshot.AccountingFaults)
	}
}

func TestUDPHandlerPendingDispatchHasSingleWinner(t *testing.T) {
	backend := newClientUDPBackend(t)
	clientConn, _ := newUDPHandlerQUICPair(t)
	budget := newUDPSessionBudget(2)
	handler := newUDPHandler(
		"127.0.0.1",
		backend.LocalAddr().(*net.UDPAddr).Port,
		true,
		config.DefaultMaxUDPFragmentGroupsPerHandler,
		config.DefaultMaxUDPFragmentBackingBytesPerHandler,
		zerolog.Nop(),
		budget,
	)
	startUDPHandlerForDispatch(t, handler, clientConn)
	ready := make(chan struct{})
	publish := make(chan struct{})
	handler.beforeSessionPublish = func() {
		close(ready)
		<-publish
	}
	var publishOnce sync.Once
	t.Cleanup(func() { publishOnce.Do(func() { close(publish) }) })

	start := make(chan struct{})
	var callers sync.WaitGroup
	for range 2 {
		callers.Go(func() {
			<-start
			handler.dispatchPayload(9, udpPendingPacket{payload: []byte("winner"), retainedBytes: len("winner")}, clientConn)
		})
	}
	close(start)
	callers.Wait()
	awaitUDPHandler(t, ready, "single pending worker publish gate")
	_, state := awaitUDPState(t, handler, 9, udpSessionPhaseDraining)
	if state.candidate == nil || state.session != nil {
		t.Fatalf("pending winner state = %+v", state)
	}
	if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 1 {
		t.Fatalf("pending winner budget = %+v, want zero current and one permit", snapshot)
	}
	if got := handler.epochAllocator.Load(); got != 1 || handler.dsendStats.load().Workers != 0 {
		t.Fatalf("pending winner = %d epochs/%d ready workers, want 1/0", got, handler.dsendStats.load().Workers)
	}
	publishOnce.Do(func() { close(publish) })
	_, state = awaitUDPState(t, handler, 9, udpSessionPhaseReady)
	if state.session == nil || state.session.epoch != 1 {
		t.Fatalf("published winner state = %+v", state)
	}
	awaitClientUDPCondition(t, "published winner worker", func() bool {
		return handler.dsendStats.load().Workers == 1
	})
	if snapshot := budget.snapshot(); snapshot.Current != 1 || snapshot.Permits != 1 || handler.dsendStats.load().Workers != 1 {
		t.Fatalf("published winner = budget %+v/workers %d, want 1/1", snapshot, handler.dsendStats.load().Workers)
	}
}

func TestUDPHandlerRecreatesSameSessionIDWithIsolatedEpoch(t *testing.T) {
	backend := newClientUDPBackend(t)
	clientConn, _ := newUDPHandlerQUICPair(t)
	handler := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop(), newUDPSessionBudget(1))
	startUDPHandlerForDispatch(t, handler, clientConn)

	const sessionID = uint32(77)
	handler.dispatchPayload(sessionID, udpPendingPacket{payload: []byte("old-ready"), retainedBytes: len("old-ready")}, clientConn)
	readClientUDPBackend(t, backend, 3*time.Second)
	oldState, oldStateSnapshot := awaitUDPState(t, handler, sessionID, udpSessionPhaseReady)
	oldSession := oldStateSnapshot.session
	oldPayload := bytes.Repeat([]byte{0xa1}, protocol.MaxUDPPayload+1)
	oldFragments, err := handler.fragmentDatagrams(sessionID, oldSession.epoch, oldPayload, &oldSession.fragmentSequence)
	if err != nil {
		t.Fatal(err)
	}
	delayedOld := append([]byte(nil), oldFragments[1].Data...)
	parsedOld, err := protocol.DecodeUDPDatagram(oldFragments[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	_, _, complete, err := protocol.DecodeAndAssembleUDPDatagram(oldFragments[0].Data, handler.fragmentAssembler)
	handler.dsendStats.releaseDatagrams(oldFragments, int64(len(oldFragments)))
	if err != nil || complete || uint32(parsedOld.FragmentID) != 1 {
		t.Fatalf("old first fragment = identity %#x complete %v error %v", parsedOld.FragmentID, complete, err)
	}

	handler.closeSessionExact(oldState, oldSession)
	awaitClientUDPCondition(t, "old session permit release", func() bool {
		snapshot := handler.sessionBudget.snapshot()
		return snapshot.Current == 0 && snapshot.Permits == 0 && handler.dsendStats.load().Workers == 0
	})
	handler.dispatchPayload(sessionID, udpPendingPacket{payload: []byte("new-ready"), retainedBytes: len("new-ready")}, clientConn)
	readClientUDPBackend(t, backend, 3*time.Second)
	_, newStateSnapshot := awaitUDPState(t, handler, sessionID, udpSessionPhaseReady)
	newSession := newStateSnapshot.session
	if newSession == oldSession || newSession.epoch == oldSession.epoch {
		t.Fatalf("recreated session = %p epoch %d, old = %p epoch %d", newSession, newSession.epoch, oldSession, oldSession.epoch)
	}

	newPayload := bytes.Repeat([]byte{0xb2}, protocol.MaxUDPPayload+1)
	newFragments, err := handler.fragmentDatagrams(sessionID, newSession.epoch, newPayload, &newSession.fragmentSequence)
	if err != nil {
		t.Fatal(err)
	}
	parsedNew, err := protocol.DecodeUDPDatagram(newFragments[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for _, fragment := range newFragments {
		_, got, complete, err = protocol.DecodeAndAssembleUDPDatagram(fragment.Data, handler.fragmentAssembler)
		if err != nil {
			break
		}
	}
	handler.dsendStats.releaseDatagrams(newFragments, int64(len(newFragments)))
	if err != nil || !complete || !bytes.Equal(got, newPayload) || uint32(parsedNew.FragmentID) != 1 {
		t.Fatalf("new first packet = identity %#x bytes %d complete %v error %v", parsedNew.FragmentID, len(got), complete, err)
	}
	if parsedOld.FragmentID == parsedNew.FragmentID || handler.fragmentAssembler.Snapshot().RetainedGroups != 1 {
		t.Fatal("recreated session identity mixed with retained old partial")
	}
	_, got, complete, err = protocol.DecodeAndAssembleUDPDatagram(delayedOld, handler.fragmentAssembler)
	if err != nil || !complete || !bytes.Equal(got, oldPayload) {
		t.Fatalf("delayed old completion = bytes %d complete %v error %v", len(got), complete, err)
	}
}

func TestUDPHandlerEpochExhaustionCleansCandidateAndKeepsExistingSession(t *testing.T) {
	backend := newClientUDPBackend(t)
	clientConn, _ := newUDPHandlerQUICPair(t)
	budget := newUDPSessionBudget(2)
	handler := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop(), budget)
	startUDPHandlerForDispatch(t, handler, clientConn)
	handler.epochAllocator.Store(math.MaxUint32 - 1)

	handler.dispatchPayload(1, udpPendingPacket{payload: []byte("last"), retainedBytes: len("last")}, clientConn)
	readClientUDPBackend(t, backend, 3*time.Second)
	existingState, existingSnapshot := awaitUDPState(t, handler, 1, udpSessionPhaseReady)
	existing := existingSnapshot.session
	awaitClientUDPCondition(t, "last-epoch response worker", func() bool {
		return handler.dsendStats.load().Workers == 1
	})
	if existing.epoch != math.MaxUint32 {
		t.Fatalf("last session epoch = %d, want %d", existing.epoch, uint32(math.MaxUint32))
	}
	handler.dispatchPayload(2, udpPendingPacket{payload: []byte("exhausted"), retainedBytes: len("exhausted")}, clientConn)
	awaitClientUDPCondition(t, "exhausted candidate cleanup", func() bool {
		snapshot := budget.snapshot()
		return handler.loadSessionState(2) == nil && snapshot.CreateErrors == 1 && snapshot.Permits == 1
	})
	handler.dispatchPayload(1, udpPendingPacket{payload: []byte("existing"), retainedBytes: len("existing")}, clientConn)
	if payload, _ := readClientUDPBackend(t, backend, 3*time.Second); !bytes.Equal(payload, []byte("existing")) {
		t.Fatalf("existing session payload after exhaustion = %q", payload)
	}
	if got := snapshotUDPState(existingState).session; got != existing {
		t.Fatalf("existing session after exhaustion = %p, want %p", got, existing)
	}
	snapshot := budget.snapshot()
	if snapshot.Current != 1 || snapshot.Permits != 1 || snapshot.AccountingFaults != 0 || handler.dsendStats.load().Workers != 1 {
		t.Fatalf("exhaustion cleanup = budget %+v workers %d", snapshot, handler.dsendStats.load().Workers)
	}
	datagrams, err := handler.fragmentDatagrams(existing.id, existing.epoch, make([]byte, protocol.MaxUDPPayload+1), &existing.fragmentSequence)
	if err != nil {
		t.Fatalf("existing session fragmentation after exhaustion: %v", err)
	}
	parsed, err := protocol.DecodeUDPDatagram(datagrams[0].Data)
	handler.dsendStats.releaseDatagrams(datagrams, int64(len(datagrams)))
	if err != nil {
		t.Fatalf("decode existing identity after exhaustion: %v", err)
	}
	if parsed.FragmentID != uint64(math.MaxUint32)<<32|1 {
		t.Fatalf("existing identity after exhaustion = %#x", parsed.FragmentID)
	}
}

func TestUDPHandlerReceivesLiteralWidenedFragmentsAndRejectsLegacy(t *testing.T) {
	backend := newClientUDPBackend(t)
	clientConn, serverConn := newUDPHandlerQUICPair(t)
	handler := NewUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop())
	handler.Start(context.Background(), clientConn)
	defer handler.stopAndWait()

	legacy := literalClientUDPFragment(9, 0x1122334455667788, 0, 2, []byte("legacy"))
	legacy[0] = 0x21
	if _, err := protocol.DecodeUDPDatagram(legacy); !errors.Is(err, protocol.ErrUnknownDatagramType) {
		t.Fatalf("legacy decode error = %v", err)
	}
	if err := serverConn.SendDatagram(legacy); err != nil {
		t.Fatal(err)
	}
	awaitClientUDPCondition(t, "legacy fragment rejection", func() bool {
		return handler.sessionBudget.snapshot().DecodeDrops == 1
	})
	if snapshot := handler.fragmentAssembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
		t.Fatalf("legacy fragment retained state: %+v", snapshot)
	}

	const low = uint64(0x55667788)
	oldID := uint64(0x11223344)<<32 | low
	newID := uint64(0x88776655)<<32 | low
	if err := serverConn.SendDatagram(literalClientUDPFragment(9, oldID, 0, 2, []byte("old-"))); err != nil {
		t.Fatal(err)
	}
	awaitClientUDPCondition(t, "old widened partial retention", func() bool {
		return handler.fragmentAssembler.Snapshot().RetainedGroups == 1
	})
	for _, wire := range [][]byte{
		literalClientUDPFragment(9, newID, 0, 2, []byte("new-")),
		literalClientUDPFragment(9, newID, 1, 2, []byte("payload")),
	} {
		if err := serverConn.SendDatagram(wire); err != nil {
			t.Fatal(err)
		}
	}
	buf := make([]byte, 64)
	if err := backend.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, _, err := backend.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "new-payload" {
		t.Fatalf("new widened payload = %q", got)
	}
	if snapshot := handler.fragmentAssembler.Snapshot(); snapshot.RetainedGroups != 1 {
		t.Fatalf("new identity changed old partial: %+v", snapshot)
	}
	if err := serverConn.SendDatagram(literalClientUDPFragment(9, oldID, 1, 2, []byte("payload"))); err != nil {
		t.Fatal(err)
	}
	n, _, err = backend.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "old-payload" {
		t.Fatalf("old widened payload = %q", got)
	}
}

func TestUDPHandlerStopBeforeStart(t *testing.T) {
	handler := NewUDPHandler("127.0.0.1", 1, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop())
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
	backend := newClientUDPBackend(t)
	clientConn, serverConn := newUDPHandlerQUICPair(t)
	budget := newUDPSessionBudget(1)
	handler := newUDPHandler(
		"127.0.0.1",
		backend.LocalAddr().(*net.UDPAddr).Port,
		true,
		config.DefaultMaxUDPFragmentGroupsPerHandler,
		config.DefaultMaxUDPFragmentBackingBytesPerHandler,
		zerolog.Nop(),
		budget,
	)
	handler.Start(context.Background(), clientConn)

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	handler.Start(canceledCtx, clientConn)
	if err := handler.ctx.Err(); err != nil {
		t.Fatalf("double Start replaced the live handler context: %v", err)
	}

	sendClientUDPDatagram(t, serverConn, 7, []byte("active"))
	readClientUDPBackend(t, backend, 3*time.Second)
	_, activeState := awaitUDPState(t, handler, 7, udpSessionPhaseReady)
	session := activeState.session
	awaitClientUDPCondition(t, "active response worker", func() bool {
		return handler.dsendStats.load().Workers == 1
	})
	sendClientUDPDatagram(t, serverConn, 8, []byte("over-cap"))
	awaitClientUDPCondition(t, "UDP capacity drop", func() bool {
		return budget.snapshot().CapacityDrops == 1
	})
	if snapshot := budget.snapshot(); snapshot.Current != 1 || snapshot.Permits != 1 {
		t.Fatalf("live UDP budget = %d active/%d held, want 1/1", snapshot.Current, snapshot.Permits)
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
	if got := handler.dsendStats.load().Workers; got != 0 {
		t.Fatalf("stopped Dsend workers = %d, want 0", got)
	}
	if _, err := handler.fragmentAssembler.AddFragment(7, 4, 0, 2, []byte("closed")); !errors.Is(err, protocol.ErrFragmentAssemblerClosed) {
		t.Fatalf("assembler after Stop error = %v, want ErrFragmentAssemblerClosed", err)
	}
	_ = session.localConn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := session.localConn.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("session socket read after Stop error = %v, want net.ErrClosed", err)
	}
	snapshot := budget.snapshot()
	if snapshot.Current != 0 || snapshot.Permits != 0 || snapshot.AccountingFaults != 0 {
		t.Fatalf("stopped UDP budget = %d active/%d held/%d faults, want zero",
			snapshot.Current, snapshot.Permits, snapshot.AccountingFaults)
	}
	if snapshot.CapacityDrops != 1 || snapshot.HighWater != 1 {
		t.Fatalf("UDP budget counters = %d drops/%d high-water, want 1/1", snapshot.CapacityDrops, snapshot.HighWater)
	}
}

func TestUDPHandlerReceiveTerminalErrorStopsWithoutWaitingForItself(t *testing.T) {
	clientConn, serverConn := newUDPHandlerQUICPair(t)
	handler := NewUDPHandler("127.0.0.1", 1, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop())
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
