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
	deadline := time.NewTimer(3 * time.Second)
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

	budget := newUDPSessionBudget(1)
	handler := newUDPHandler("lif002-client.qmux.invalid", 9, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop(), budget)
	handler.ctx, handler.cancel = context.WithCancel(context.Background())
	handler.started = true
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		handler.Stop()
	}()

	_, err := handler.getOrCreateSession(1, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("session creation error = %v, want context.Canceled", err)
	}
	handler.wait()
	assertNoUDPSessions(t, handler)
	if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 0 {
		t.Fatalf("canceled session budget = %+v, want no current session or permit", snapshot)
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
		if _, err := handler.getOrCreateSession(uint32(id+1), nil); !errors.Is(err, errClientUDPSessionLimit) {
			t.Fatalf("handler %d error = %v, want errClientUDPSessionLimit", id, err)
		}
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
	budget := newUDPSessionBudget(1)
	release, ok := budget.acquire()
	if !ok {
		t.Fatal("UDP session budget acquisition failed")
	}
	currentConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen current UDP session socket: %v", err)
	}
	staleConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		_ = currentConn.Close()
		t.Fatalf("listen stale UDP session socket: %v", err)
	}
	t.Cleanup(func() {
		_ = currentConn.Close()
		_ = staleConn.Close()
	})

	handler := newUDPHandler("127.0.0.1", 1, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop(), budget)
	t.Cleanup(handler.Stop)
	stale := &UDPSession{id: 7, localConn: staleConn}
	current := &UDPSession{id: 7, localConn: currentConn}
	handler.sessions.Store(current.id, current)
	budget.publish()

	handler.closeSession(stale)
	if got, ok := handler.sessions.Load(current.id); !ok || got != current {
		t.Fatalf("stale close changed replacement = (%p, %v), want (%p, true)", got, ok, current)
	}
	if snapshot := budget.snapshot(); snapshot.Current != 1 {
		t.Fatalf("published sessions after stale close = %d, want 1", snapshot.Current)
	}
	if err := currentConn.SetReadDeadline(time.Now()); err != nil {
		t.Fatalf("set current session socket deadline after stale close: %v", err)
	}
	if _, err := currentConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("current session socket read unexpectedly succeeded")
	} else {
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("current session socket after stale close error = %v, want timeout", err)
		}
	}

	handler.closeSession(current)
	release() // Direct test sessions have no reader goroutine to own this release.
	if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 0 || snapshot.AccountingFaults != 0 {
		t.Fatalf("exact close budget = %d active/%d held/%d faults, want zero",
			snapshot.Current, snapshot.Permits, snapshot.AccountingFaults)
	}
}

func TestUDPHandlerDuplicatePublicationReleasesLoser(t *testing.T) {
	backend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen duplicate-publication UDP backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })

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
	handler.ctx = context.Background()
	handler.started = true
	ready := sync.WaitGroup{}
	ready.Add(2)
	publish := make(chan struct{})
	handler.beforeSessionPublish = func() {
		ready.Done()
		<-publish
	}

	results := make(chan *UDPSession, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			session, createErr := handler.getOrCreateSession(9, nil)
			results <- session
			errs <- createErr
		}()
	}
	ready.Wait()
	close(publish)
	first, second := <-results, <-results
	if err := <-errs; err != nil {
		t.Fatalf("first duplicate creator: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("second duplicate creator: %v", err)
	}
	if first == nil || second != first {
		t.Fatalf("duplicate creators returned (%p, %p), want one exact session", first, second)
	}
	if snapshot := budget.snapshot(); snapshot.Current != 1 || snapshot.Permits != 1 {
		t.Fatalf("duplicate publication budget = %d active/%d held, want 1/1", snapshot.Current, snapshot.Permits)
	}
	if got := handler.epochAllocator.Load(); got != 2 || first.epoch == 0 {
		t.Fatalf("duplicate publication epochs = allocated %d/winner %d, want 2/nonzero", got, first.epoch)
	}

	handler.Stop()
	handler.wait()
	if snapshot := budget.snapshot(); snapshot.Current != 0 || snapshot.Permits != 0 || snapshot.AccountingFaults != 0 {
		t.Fatalf("duplicate publication cleanup = %d active/%d held/%d faults, want zero",
			snapshot.Current, snapshot.Permits, snapshot.AccountingFaults)
	}
}

func TestUDPHandlerRecreatesSameSessionIDWithIsolatedEpoch(t *testing.T) {
	backend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	handler := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop(), newUDPSessionBudget(1))
	handler.ctx = context.Background()
	handler.started = true
	defer handler.stopAndWait()

	const sessionID = uint32(77)
	oldSession, err := handler.getOrCreateSession(sessionID, nil)
	if err != nil {
		t.Fatal(err)
	}
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

	handler.closeSession(oldSession)
	awaitClientUDPCondition(t, "old session permit release", func() bool {
		snapshot := handler.sessionBudget.snapshot()
		return snapshot.Current == 0 && snapshot.Permits == 0 && handler.dsendStats.load().Workers == 0
	})
	newSession, err := handler.getOrCreateSession(sessionID, nil)
	if err != nil {
		t.Fatal(err)
	}
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
	backend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	budget := newUDPSessionBudget(2)
	handler := newUDPHandler("127.0.0.1", backend.LocalAddr().(*net.UDPAddr).Port, true, config.DefaultMaxUDPFragmentGroupsPerHandler, config.DefaultMaxUDPFragmentBackingBytesPerHandler, zerolog.Nop(), budget)
	handler.ctx = context.Background()
	handler.started = true
	handler.epochAllocator.Store(math.MaxUint32 - 1)
	defer handler.stopAndWait()

	existing, err := handler.getOrCreateSession(1, nil)
	if err != nil || existing == nil {
		t.Fatalf("last epoch session = (%p, %v), want non-nil", existing, err)
	}
	if existing.epoch != math.MaxUint32 {
		t.Fatalf("last session epoch = %d, want %d", existing.epoch, uint32(math.MaxUint32))
	}
	if failed, err := handler.getOrCreateSession(2, nil); failed != nil || !errors.Is(err, errClientUDPEpochExhausted) {
		t.Fatalf("exhausted session creation = (%p, %v)", failed, err)
	}
	if got, err := handler.getOrCreateSession(1, nil); err != nil || got != existing {
		t.Fatalf("existing session after exhaustion = (%p, %v), want %p", got, err, existing)
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
	backend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
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
	backend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen UDP backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	clientConn, _ := newUDPHandlerQUICPair(t)
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

	session, err := handler.getOrCreateSession(7, clientConn)
	if err != nil {
		t.Fatalf("create active UDP session: %v", err)
	}
	if got := handler.dsendStats.load().Workers; got != 1 {
		t.Fatalf("Dsend workers = %d, want 1", got)
	}
	if _, err := handler.getOrCreateSession(8, clientConn); !errors.Is(err, errClientUDPSessionLimit) {
		t.Fatalf("create UDP session over cap error = %v, want errClientUDPSessionLimit", err)
	}
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
