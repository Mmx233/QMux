package traffic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

const (
	defaultQUICIncomingStreams = 100
	partialNewConnWindow       = 32 * 1024
	partialNewConnPrefix       = 4096
)

func TestNewConnWriteResult(t *testing.T) {
	writeErr := errors.New("write failed")
	tests := []struct {
		name       string
		n          int
		err        error
		wantRetry  bool
		wantErr    error
		wantAnyErr bool
	}{
		{name: "negative count", n: -1, wantAnyErr: true},
		{name: "oversized count", n: 11, wantAnyErr: true},
		{name: "short nil error", n: 4, wantRetry: true, wantErr: io.ErrShortWrite},
		{name: "partial error", n: 4, err: writeErr, wantRetry: true, wantErr: writeErr},
		{name: "full success", n: 10},
		{name: "full error", n: 10, err: writeErr, wantErr: writeErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retry, err := newConnWriteResult(tt.n, 10, tt.err)
			if retry != tt.wantRetry {
				t.Fatalf("retry = %v, want %v", retry, tt.wantRetry)
			}
			if tt.wantAnyErr {
				if err == nil {
					t.Fatal("error = nil, want invalid-count error")
				}
			} else if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func startTCPAdmissionManager(t *testing.T, quicAddr string, connectionPool *pool.ConnectionPool) *Manager {
	return startTCPAdmissionManagerWithCapacity(t, quicAddr, connectionPool, config.ListenerCapacity{})
}

func startTCPAdmissionManagerWithCapacity(
	t *testing.T,
	quicAddr string,
	connectionPool *pool.ConnectionPool,
	capacity config.ListenerCapacity,
) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	manager := NewManager(&config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    quicAddr,
		TrafficAddr: "127.0.0.1:0",
		Protocol:    "tcp",
		Capacity:    capacity,
	}}}, map[string]*pool.ConnectionPool{quicAddr: connectionPool}, zerolog.Nop())
	t.Cleanup(func() {
		cancel()
		manager.Stop()
	})
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start TCP admission manager: %v", err)
	}
	return manager
}

type tcpFallbackSetup struct {
	connectionPool          *pool.ConnectionPool
	primary, backup         *pool.ClientConn
	listener                *Listener
	primaryPeer, backupPeer *quic.Conn
}

func startTCPFallbackSetup(t *testing.T, ctx context.Context, primaryWindow uint64) *tcpFallbackSetup {
	t.Helper()
	primaryListener, primaryServer, primaryPeer := newRelayLifecycleQUICPair(t, ctx, primaryWindow)
	registerRelayLifecycleQUICCleanup(t, primaryListener, primaryServer, primaryPeer)
	backupListener, backupServer, backupPeer := newRelayLifecycleQUICPair(t, ctx, 256*1024)
	registerRelayLifecycleQUICCleanup(t, backupListener, backupServer, backupPeer)
	quicAddr := primaryListener.Addr().String()
	connectionPool := pool.New(quicAddr, pool.NewLeastConnectionsBalancer(), zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	primary := &pool.ClientConn{ID: "primary", Conn: primaryServer, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	backup := &pool.ClientConn{ID: "backup", Conn: backupServer, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	backup.ActiveConns.Store(1)
	if err := connectionPool.Add(primary); err != nil {
		t.Fatalf("Add(primary) error = %v", err)
	}
	if err := connectionPool.Add(backup); err != nil {
		t.Fatalf("Add(backup) error = %v", err)
	}
	listenerCtx, cancelListener := context.WithCancel(context.Background())
	listener := &Listener{
		Addr:          strings.Repeat("destination", 8*1024),
		Pool:          connectionPool,
		ctx:           listenerCtx,
		cancel:        cancelListener,
		logger:        zerolog.Nop(),
		flows:         make(map[*tcpFlow]struct{}),
		tcpFlowLimit:  config.DefaultMaxTCPConnections,
		tcpSetupLimit: config.DefaultMaxPendingTCPSetups,
		tcpSetupSlots: make(chan struct{}, config.DefaultMaxPendingTCPSetups),
	}
	t.Cleanup(listener.close)
	return &tcpFallbackSetup{
		connectionPool: connectionPool,
		primary:        primary,
		backup:         backup,
		listener:       listener,
		primaryPeer:    primaryPeer,
		backupPeer:     backupPeer,
	}
}

func startTCPFallbackFlow(t *testing.T, listener *Listener) net.Conn {
	t.Helper()
	release, ok := listener.acquireTCPSetup()
	if !ok {
		t.Fatal("acquireTCPSetup() = false")
	}
	local, remote := net.Pipe()
	registerTCPAdmissionClose(t, "fallback TCP peer", remote)
	flow, ok := listener.addTCPFlow(local)
	if !ok {
		t.Fatal("addTCPFlow() = false")
	}
	go listener.handleTCPConnection(flow, time.Now().Add(tcpSetupTimeout), release)
	return remote
}

func openPartialTCPFallback(
	t *testing.T,
	ctx context.Context,
	setup *tcpFallbackSetup,
	failPrimary func(*quic.Stream),
) (net.Conn, *quic.Stream, *quic.Stream) {
	t.Helper()
	tcpConn := startTCPFallbackFlow(t, setup.listener)
	primaryStream, err := setup.primaryPeer.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("primary AcceptStream() error = %v", err)
	}
	if err := primaryStream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set primary prefix read deadline: %v", err)
	}
	prefix := make([]byte, partialNewConnPrefix)
	if _, err := io.ReadFull(primaryStream, prefix); err != nil {
		t.Fatalf("read primary NewConn prefix: %v", err)
	}
	failPrimary(primaryStream)

	backupStream, err := setup.backupPeer.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("backup AcceptStream() error = %v", err)
	}
	if err := backupStream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set backup NewConn read deadline: %v", err)
	}
	var backupFrame bytes.Buffer
	var newConn protocol.NewConnMsg
	if err := protocol.ReadTypedMessage(io.TeeReader(backupStream, &backupFrame), protocol.MsgTypeNewConn, &newConn); err != nil {
		t.Fatalf("read backup NewConn message: %v", err)
	}
	if backupFrame.Len() <= partialNewConnWindow+partialNewConnPrefix {
		t.Fatalf(
			"NewConn frame length = %d, want > primary ceiling %d + prefix %d",
			backupFrame.Len(), partialNewConnWindow, partialNewConnPrefix,
		)
	}
	if !bytes.Equal(prefix, backupFrame.Bytes()[:partialNewConnPrefix]) {
		t.Fatal("backup NewConn frame did not reuse the primary frame prefix")
	}
	if newConn.ConnID == 0 {
		t.Fatal("backup NewConn connID = 0")
	}
	return tcpConn, primaryStream, backupStream
}

func holdDefaultQUICIncomingStreams(t *testing.T, conn *quic.Conn) []*quic.Stream {
	t.Helper()
	streams := make([]*quic.Stream, 0, defaultQUICIncomingStreams)
	for i := range defaultQUICIncomingStreams {
		stream, err := conn.OpenStream()
		if err != nil {
			t.Fatalf("OpenStream(%d) before default limit error = %v", i, err)
		}
		streams = append(streams, stream)
	}
	if stream, err := conn.OpenStream(); err == nil {
		stream.CancelRead(trafficStreamCancelCode)
		stream.CancelWrite(trafficStreamCancelCode)
		t.Fatal("OpenStream() above default peer limit succeeded")
	} else {
		if _, ok := errors.AsType[*quic.StreamLimitReachedError](err); !ok {
			t.Fatalf("OpenStream() above default peer limit error = %T %v, want StreamLimitReachedError", err, err)
		}
	}
	return streams
}

func dialRejectedTCP(t *testing.T, addr string) time.Duration {
	t.Helper()
	started := time.Now()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial saturated traffic listener: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close rejected TCP connection: %v", err)
		}
	}()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set saturated TCP read deadline: %v", err)
	}
	var buf [1]byte
	if _, err := conn.Read(buf[:]); err == nil {
		t.Fatal("saturated traffic connection remained open")
	} else if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		t.Fatalf("saturated traffic connection waited for the read deadline: %v", err)
	}
	return time.Since(started)
}

func registerTCPAdmissionClose(t *testing.T, name string, closer io.Closer) {
	t.Helper()
	t.Cleanup(func() {
		if err := closer.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("close %s: %v", name, err)
		}
	})
}

func startBlockedNewConn(
	t *testing.T,
	serverConn *quic.Conn,
	clientID string,
	setupTimeout time.Duration,
) (*pool.ConnectionPool, *pool.ClientConn, *Listener, <-chan struct{}) {
	t.Helper()
	connectionPool := pool.New(clientID, pool.NewRoundRobinBalancer(), zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	client := &pool.ClientConn{ID: clientID, Conn: serverConn, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	if err := connectionPool.Add(client); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	listenerCtx, cancelListener := context.WithCancel(context.Background())
	t.Cleanup(cancelListener)
	listener := &Listener{
		Addr:          strings.Repeat("destination", 8*1024),
		Pool:          connectionPool,
		ctx:           listenerCtx,
		cancel:        cancelListener,
		logger:        zerolog.Nop(),
		flows:         make(map[*tcpFlow]struct{}),
		tcpFlowLimit:  config.DefaultMaxTCPConnections,
		tcpSetupLimit: config.DefaultMaxPendingTCPSetups,
		tcpSetupSlots: make(chan struct{}, config.DefaultMaxPendingTCPSetups),
	}
	release, ok := listener.acquireTCPSetup()
	if !ok {
		t.Fatal("acquireTCPSetup() = false")
	}
	local, remote := net.Pipe()
	registerTCPAdmissionClose(t, "blocked NewConn peer", remote)
	done := make(chan struct{})
	go func() {
		flow, ok := listener.addTCPFlow(local)
		if !ok {
			t.Error("addTCPFlow() = false")
			close(done)
			return
		}
		listener.handleTCPConnection(flow, time.Now().Add(setupTimeout), release)
		close(done)
	}()
	return connectionPool, client, listener, done
}

func tcpTerminalTotal(snapshot TCPAdmissionSnapshot) uint64 {
	return snapshot.Committed + snapshot.FlowCapacity + snapshot.ListenerCapacity + snapshot.Unavailable +
		snapshot.GenerationConnectionCapacity + snapshot.GenerationSetupCapacity + snapshot.PeerStreamLimit +
		snapshot.Deadline + snapshot.SetupFailure + snapshot.Canceled
}

func TestTCPAdmissionSnapshotGenerationCapacityCompatibility(t *testing.T) {
	var stats tcpAdmissionStats
	stats.finish(tcpTerminalGenerationConnectionCapacity)
	stats.finish(tcpTerminalGenerationConnectionCapacity)
	stats.finish(tcpTerminalGenerationSetupCapacity)

	snapshot := stats.snapshot()
	if snapshot.GenerationConnectionCapacity != 2 || snapshot.GenerationSetupCapacity != 1 ||
		snapshot.GenerationCapacity != 3 {
		t.Fatalf("generation capacity snapshot = %+v, want split 2/1 and aggregate 3", snapshot)
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal TCP admission snapshot: %v", err)
	}
	var fields map[string]uint64
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("unmarshal TCP admission snapshot: %v", err)
	}
	if value, ok := fields["GenerationCapacity"]; !ok || value != 3 {
		t.Fatalf("GenerationCapacity JSON field = (%d, %v), want (3, true): %s", value, ok, data)
	}
}

func TestTCPFlowSnapshotTracksOwnedSockets(t *testing.T) {
	listener := &Listener{flows: make(map[*tcpFlow]struct{}), tcpFlowLimit: 2, tcpSetupLimit: 3}
	first, firstPeer := net.Pipe()
	second, secondPeer := net.Pipe()
	rejected, rejectedPeer := net.Pipe()
	replacement, replacementPeer := net.Pipe()
	t.Cleanup(func() {
		_ = first.Close()
		_ = firstPeer.Close()
		_ = second.Close()
		_ = secondPeer.Close()
		_ = rejected.Close()
		_ = rejectedPeer.Close()
		_ = replacement.Close()
		_ = replacementPeer.Close()
	})

	firstFlow, ok := listener.addTCPFlow(first)
	if !ok {
		t.Fatal("add first TCP flow = false")
	}
	secondFlow, ok := listener.addTCPFlow(second)
	if !ok {
		t.Fatal("add second TCP flow = false")
	}
	if flow, ok := listener.addTCPFlow(rejected); ok || flow != nil {
		t.Fatal("add flow above configured limit succeeded")
	}
	if firstFlow.isAborted() || secondFlow.isAborted() {
		t.Fatal("rejecting cap+1 aborted an existing flow")
	}
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.FlowLimit != 2 || snapshot.SetupLimit != 3 ||
		snapshot.FlowCurrent != 2 || snapshot.FlowHighWater != 2 || snapshot.FlowCapacity != 1 ||
		tcpTerminalTotal(snapshot) != 1 {
		t.Fatalf("saturated flow snapshot = %+v, want limits 2/3, owned 2/2, and one flow-capacity terminal", snapshot)
	}

	listener.removeTCPFlow(firstFlow)
	listener.removeTCPFlow(firstFlow)
	replacementFlow, ok := listener.addTCPFlow(replacement)
	if !ok {
		t.Fatal("add replacement flow after release = false")
	}
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.FlowCurrent != 2 || snapshot.FlowHighWater != 2 ||
		snapshot.FlowCapacity != 1 {
		t.Fatalf("flow snapshot after recovery = %+v, want current/high-water 2/2 and one rejection", snapshot)
	}
	listener.removeTCPFlow(replacementFlow)
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.FlowCurrent != 1 || snapshot.FlowHighWater != 2 {
		t.Fatalf("flow snapshot after idempotent removal = current:%d high-water:%d, want 1/2", snapshot.FlowCurrent, snapshot.FlowHighWater)
	}

	listener.removeTCPFlow(secondFlow)
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.FlowCurrent != 0 || snapshot.FlowHighWater != 2 {
		t.Fatalf("final flow snapshot = current:%d high-water:%d, want 0/2", snapshot.FlowCurrent, snapshot.FlowHighWater)
	}
}

func TestTCPFlowCounterMatchesOwnedMapDuringTransitions(t *testing.T) {
	listener := &Listener{flows: make(map[*tcpFlow]struct{}), tcpFlowLimit: 1}
	local, peer := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = peer.Close()
	})

	done := make(chan struct{})
	errCh := make(chan string, 1)
	go func() {
		defer close(done)
		for range 10_000 {
			flow, ok := listener.addTCPFlow(local)
			if !ok {
				errCh <- "addTCPFlow() rejected below limit"
				return
			}
			listener.removeTCPFlow(flow)
		}
	}()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case message := <-errCh:
			t.Fatal(message)
		case <-done:
			if snapshot := listener.tcpAdmissionSnapshot(); snapshot.FlowCurrent != 0 || snapshot.FlowHighWater != 1 {
				t.Fatalf("final flow snapshot = %+v, want current/high-water 0/1", snapshot)
			}
			return
		case <-deadline.C:
			t.Fatal("flow transition check timed out")
		default:
			listener.tcpAdmission.mu.Lock()
			listener.flowsMu.Lock()
			current := listener.tcpAdmission.flowCurrent.Load()
			owned := int64(len(listener.flows))
			listener.flowsMu.Unlock()
			listener.tcpAdmission.mu.Unlock()
			if current != owned || current < 0 || current > 1 {
				t.Fatalf("flow current/map = %d/%d, want equal within 0..1", current, owned)
			}
		}
	}
}

func waitForTCPAdmissionState(
	t *testing.T,
	client *pool.ClientConn,
	wantActive int64,
	wantTotal uint64,
	setupSlots chan struct{},
) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for client.ActiveConns.Load() != wantActive || client.TotalConns.Load() != wantTotal || len(setupSlots) != 0 {
		select {
		case <-deadline.C:
			t.Fatalf(
				"TCP admission state = active:%d total:%d setup:%d, want %d/%d/0",
				client.ActiveConns.Load(), client.TotalConns.Load(), len(setupSlots), wantActive, wantTotal,
			)
		case <-ticker.C:
		}
	}
}

func TestTCPSetupPermitSaturationRejectsAndRecovers(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 256*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)

	quicAddr := quicListener.Addr().String()
	connectionPool := pool.New(quicAddr, pool.NewRoundRobinBalancer(), zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	client := &pool.ClientConn{ID: "permit-client", Conn: serverConn, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	if err := connectionPool.Add(client); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	manager := startTCPAdmissionManagerWithCapacity(t, quicAddr, connectionPool, config.ListenerCapacity{
		MaxTCPConnections:   3,
		MaxPendingTCPSetups: 2,
	})
	listener := manager.listeners[0]

	permits := make([]*tcpSetupPermit, 0, listener.tcpSetupLimit)
	for range listener.tcpSetupLimit {
		permit, ok := listener.acquireTCPSetup()
		if !ok {
			t.Fatalf("acquireTCPSetup() rejected before %d permits", listener.tcpSetupLimit)
		}
		permits = append(permits, permit)
	}
	if permit, ok := listener.acquireTCPSetup(); ok {
		permit.release()
		t.Fatal("acquireTCPSetup() exceeded listener bound")
	}
	if elapsed := dialRejectedTCP(t, listener.TCPListener.Addr().String()); elapsed >= 2*time.Second {
		t.Fatalf("listener-capacity rejection took %v, want immediate close", elapsed)
	}
	snapshot := manager.TCPAdmissionSnapshots()[0]
	if snapshot.FlowLimit != 3 || snapshot.SetupLimit != 2 || snapshot.SetupCurrent != 2 || snapshot.SetupHighWater != 2 ||
		snapshot.ListenerCapacity != 1 || snapshot.FlowCapacity != 0 || tcpTerminalTotal(snapshot) != 1 {
		t.Fatalf("saturated TCP admission snapshot = %+v, want limits 3/2, setup 2/2, and one setup-capacity terminal", snapshot)
	}
	for _, permit := range permits {
		permit.release()
		permit.release()
	}
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.SetupCurrent != 0 {
		t.Fatalf("setup current after permit release = %d, want 0", snapshot.SetupCurrent)
	}

	tcpConn, peerStream := openRelayLifecycleFlow(t, testCtx, manager, peerConn)
	waitForTCPAdmissionState(t, client, 1, 1, listener.tcpSetupSlots)
	snapshot = listener.tcpAdmissionSnapshot()
	if snapshot.SetupCurrent != 0 || snapshot.ActiveCurrent != 1 || snapshot.ActiveHighWater != 1 ||
		snapshot.Attempts != 1 || snapshot.Retries != 0 || snapshot.Committed != 1 ||
		tcpTerminalTotal(snapshot) != 2 {
		t.Fatalf("recovered TCP admission snapshot = %+v", snapshot)
	}
	peerStream.CancelRead(trafficStreamCancelCode)
	peerStream.CancelWrite(trafficStreamCancelCode)
	if err := tcpConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close recovered TCP flow: %v", err)
	}
	waitForActiveConnections(t, client, 0)
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.SetupCurrent != 0 || snapshot.ActiveCurrent != 0 {
		t.Fatalf("TCP admission current after recovered flow teardown = %+v, want zero", snapshot)
	}
}

func TestTCPSetupPermitDefaultLimitCornersAndRecovery(t *testing.T) {
	const limit = config.DefaultMaxPendingTCPSetups
	listener := &Listener{
		tcpSetupLimit: limit,
		tcpSetupSlots: make(chan struct{}, limit),
	}

	permits := make([]*tcpSetupPermit, 0, limit)
	for i := range limit {
		permit, ok := listener.acquireTCPSetup()
		if !ok {
			t.Fatalf("acquireTCPSetup(%d) rejected below default limit", i)
		}
		permits = append(permits, permit)
	}
	for i := range limit {
		if permit, ok := listener.acquireTCPSetup(); ok {
			permit.release()
			t.Fatalf("2x attempt %d exceeded default setup limit", limit+i)
		}
	}
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.SetupLimit != limit || snapshot.SetupCurrent != limit || snapshot.SetupHighWater != limit ||
		snapshot.ActiveCurrent != 0 || snapshot.ActiveHighWater != 0 {
		t.Fatalf("saturated default setup snapshot = %+v", snapshot)
	}

	permits[0].release()
	recovered, ok := listener.acquireTCPSetup()
	if !ok {
		t.Fatal("acquireTCPSetup() did not recover after release")
	}
	recovered.release()
	for _, permit := range permits[1:] {
		permit.release()
	}
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.SetupLimit != limit || snapshot.SetupCurrent != 0 || snapshot.SetupHighWater != limit ||
		snapshot.ActiveCurrent != 0 || snapshot.ActiveHighWater != 0 || len(listener.tcpSetupSlots) != 0 {
		t.Fatalf("recovered default setup snapshot = %+v, slots=%d", snapshot, len(listener.tcpSetupSlots))
	}
}

func TestTCPSetupPermitTransitionsAtomicallyToActive(t *testing.T) {
	listener := &Listener{
		tcpSetupLimit: 1,
		tcpSetupSlots: make(chan struct{}, 1),
	}
	permit, ok := listener.acquireTCPSetup()
	if !ok {
		t.Fatal("acquireTCPSetup() = false")
	}
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.SetupCurrent != 1 || snapshot.ActiveCurrent != 0 {
		t.Fatalf("setup snapshot = %+v, want setup/active 1/0", snapshot)
	}

	permit.activate()
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.SetupCurrent != 0 || snapshot.ActiveCurrent != 1 ||
		snapshot.SetupHighWater != 1 || snapshot.ActiveHighWater != 1 {
		t.Fatalf("active snapshot = %+v, want setup/active 0/1 with high-water 1/1", snapshot)
	}
	if held := len(listener.tcpSetupSlots); held != 0 {
		t.Fatalf("setup slots after activation = %d, want 0", held)
	}

	permit.release()
	listener.tcpAdmission.finishActive()
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.SetupCurrent != 0 || snapshot.ActiveCurrent != 0 {
		t.Fatalf("final snapshot = %+v, want setup/active 0/0", snapshot)
	}
}

func TestTCPSetupPermitHasNoSnapshotOwnershipGap(t *testing.T) {
	listener := &Listener{
		tcpSetupLimit: 1,
		tcpSetupSlots: make(chan struct{}, 1),
	}
	for range 1_000 {
		permit, ok := listener.acquireTCPSetup()
		if !ok {
			t.Fatal("acquireTCPSetup() = false")
		}
		done := make(chan struct{})
		go func() {
			permit.activate()
			close(done)
		}()
		for {
			snapshot := listener.tcpAdmissionSnapshot()
			if snapshot.SetupCurrent+snapshot.ActiveCurrent != 1 {
				t.Fatalf("setup/active snapshot = %d/%d, want exactly one owner", snapshot.SetupCurrent, snapshot.ActiveCurrent)
			}
			select {
			case <-done:
				listener.tcpAdmission.finishActive()
				goto next
			default:
			}
		}
	next:
	}
	if snapshot := listener.tcpAdmissionSnapshot(); snapshot.SetupCurrent != 0 || snapshot.ActiveCurrent != 0 {
		t.Fatalf("final snapshot = %+v, want no owner", snapshot)
	}
}

func TestTCPGenerationSetupCapacityRejectsAndRecovers(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 32*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)

	connectionPool := pool.New(quicListener.Addr().String(), pool.NewLeastConnectionsBalancer(), zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	client := &pool.ClientConn{ID: "capacity-client", Conn: serverConn, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	if err := connectionPool.Add(client); err != nil {
		t.Fatalf("Add(client) error = %v", err)
	}
	manager := startTCPAdmissionManager(t, quicListener.Addr().String(), connectionPool)
	listener := manager.listeners[0]

	leases := make([]*pool.TCPLease, 0, 16)
	for i := range 16 {
		admission, err := connectionPool.BeginTCPAdmission()
		if err != nil {
			t.Fatalf("BeginTCPAdmission(%d) error = %v", i, err)
		}
		lease, err := admission.Next()
		if err != nil || lease == nil {
			t.Fatalf("Next(%d) = (%v, %v), want lease", i, lease, err)
		}
		leases = append(leases, lease)
	}
	dialRejectedTCP(t, listener.TCPListener.Addr().String())
	deadline := time.Now().Add(time.Second)
	snapshot := listener.tcpAdmission.snapshot()
	for (snapshot.SetupCurrent != 0 || tcpTerminalTotal(snapshot) != 1) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		snapshot = listener.tcpAdmission.snapshot()
	}
	if snapshot.SetupCurrent != 0 || snapshot.Attempts != 0 ||
		snapshot.GenerationConnectionCapacity != 0 || snapshot.GenerationSetupCapacity != 1 ||
		snapshot.PeerStreamLimit != 0 || snapshot.Unavailable != 0 || tcpTerminalTotal(snapshot) != 1 {
		t.Fatalf("generation-capacity TCP admission snapshot = %+v", snapshot)
	}
	for _, lease := range leases {
		if !lease.Release() {
			t.Fatal("Release(capacity lease) = false")
		}
	}

	tcpConn, peerStream := openRelayLifecycleFlow(t, testCtx, manager, peerConn)
	peerStream.CancelRead(trafficStreamCancelCode)
	peerStream.CancelWrite(trafficStreamCancelCode)
	_ = tcpConn.Close()
	waitForActiveConnections(t, client, 0)
	if snapshot := listener.tcpAdmission.snapshot(); snapshot.Committed != 1 || snapshot.ActiveCurrent != 0 ||
		snapshot.GenerationConnectionCapacity != 0 ||
		snapshot.GenerationSetupCapacity != 1 || tcpTerminalTotal(snapshot) != 2 {
		t.Fatalf("generation-capacity recovery TCP admission snapshot = %+v", snapshot)
	}
}

func TestTCPGenerationConnectionCapacityRejectsAndRecovers(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 32*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)

	connectionPool := pool.NewWithLimits(
		quicListener.Addr().String(),
		pool.NewLeastConnectionsBalancer(),
		zerolog.Nop(),
		pool.Limits{
			MaxClientGenerations:             1,
			MaxPendingRegistrations:          1,
			MaxTCPConnectionsPerGeneration:   1,
			MaxPendingTCPSetupsPerGeneration: 2,
			MaxUDPSessionsPerGeneration:      1,
		},
	)
	t.Cleanup(connectionPool.Stop)
	client := &pool.ClientConn{ID: "connection-capacity-client", Conn: serverConn, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	if err := connectionPool.Add(client); err != nil {
		t.Fatalf("Add(client) error = %v", err)
	}
	admission, err := connectionPool.BeginTCPAdmission()
	if err != nil {
		t.Fatalf("BeginTCPAdmission() error = %v", err)
	}
	held, err := admission.Next()
	if err != nil || held == nil || !held.Commit() {
		t.Fatalf("commit held connection lease = (%v, %v)", held, err)
	}

	manager := startTCPAdmissionManager(t, quicListener.Addr().String(), connectionPool)
	listener := manager.listeners[0]
	dialRejectedTCP(t, listener.TCPListener.Addr().String())
	deadline := time.Now().Add(time.Second)
	snapshot := listener.tcpAdmission.snapshot()
	for tcpTerminalTotal(snapshot) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		snapshot = listener.tcpAdmission.snapshot()
	}
	if snapshot.GenerationConnectionCapacity != 1 || snapshot.GenerationSetupCapacity != 0 ||
		snapshot.Attempts != 0 || tcpTerminalTotal(snapshot) != 1 {
		t.Fatalf("generation connection-capacity snapshot = %+v", snapshot)
	}
	if !held.Release() {
		t.Fatal("release held connection lease = false")
	}

	tcpConn, peerStream := openRelayLifecycleFlow(t, testCtx, manager, peerConn)
	peerStream.CancelRead(trafficStreamCancelCode)
	peerStream.CancelWrite(trafficStreamCancelCode)
	_ = tcpConn.Close()
	waitForActiveConnections(t, client, 0)
	if snapshot := listener.tcpAdmission.snapshot(); snapshot.Committed != 1 ||
		snapshot.GenerationConnectionCapacity != 1 || snapshot.GenerationSetupCapacity != 0 ||
		tcpTerminalTotal(snapshot) != 2 {
		t.Fatalf("generation connection-capacity recovery snapshot = %+v", snapshot)
	}
}

func TestTCPDefaultQUICStreamLimitRejectsWithoutPoisoningGeneration(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 256*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)
	_ = holdDefaultQUICIncomingStreams(t, serverConn)

	quicAddr := quicListener.Addr().String()
	connectionPool := pool.New(quicAddr, pool.NewLeastConnectionsBalancer(), zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	client := &pool.ClientConn{ID: "saturated-client", Conn: serverConn, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	if err := connectionPool.Add(client); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	manager := startTCPAdmissionManager(t, quicAddr, connectionPool)
	if elapsed := dialRejectedTCP(t, manager.listeners[0].TCPListener.Addr().String()); elapsed >= 2*time.Second {
		t.Fatalf("stream-limit rejection took %v, want nonblocking rejection", elapsed)
	}
	if got := connectionPool.EligibleCount("tcp"); got != 1 {
		t.Fatalf("eligible clients after stream-limit rejection = %d, want 1", got)
	}
	if active, total := client.ActiveConns.Load(), client.TotalConns.Load(); active != 0 || total != 0 {
		t.Fatalf("saturated client active/total = %d/%d, want 0/0", active, total)
	}
	snapshot := manager.listeners[0].tcpAdmission.snapshot()
	deadline := time.Now().Add(time.Second)
	for (snapshot.SetupCurrent != 0 || tcpTerminalTotal(snapshot) != 1) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		snapshot = manager.listeners[0].tcpAdmission.snapshot()
	}
	if snapshot.Attempts != 1 || snapshot.StreamLimitAttempts != 1 || snapshot.Retries != 0 ||
		snapshot.PeerStreamLimit != 1 ||
		snapshot.GenerationConnectionCapacity != 0 || snapshot.GenerationSetupCapacity != 0 ||
		snapshot.SetupFailure != 0 || tcpTerminalTotal(snapshot) != 1 {
		t.Fatalf("stream-limit TCP admission snapshot = %+v", snapshot)
	}
}

func TestTCPSaturatedPreferredUsesBackupGeneration(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelTest()
	preferredListener, preferredServer, preferredPeer := newRelayLifecycleQUICPair(t, testCtx, 256*1024)
	registerRelayLifecycleQUICCleanup(t, preferredListener, preferredServer, preferredPeer)
	backupListener, backupServer, backupPeer := newRelayLifecycleQUICPair(t, testCtx, 256*1024)
	registerRelayLifecycleQUICCleanup(t, backupListener, backupServer, backupPeer)
	_ = holdDefaultQUICIncomingStreams(t, preferredServer)

	quicAddr := preferredListener.Addr().String()
	connectionPool := pool.New(quicAddr, pool.NewLeastConnectionsBalancer(), zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	preferred := &pool.ClientConn{ID: "preferred", Conn: preferredServer, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	backup := &pool.ClientConn{ID: "backup", Conn: backupServer, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	backup.ActiveConns.Store(1) // Make the saturated zero-load generation the LC first choice.
	if err := connectionPool.Add(preferred); err != nil {
		t.Fatalf("Add(preferred) error = %v", err)
	}
	if err := connectionPool.Add(backup); err != nil {
		t.Fatalf("Add(backup) error = %v", err)
	}
	manager := startTCPAdmissionManager(t, quicAddr, connectionPool)

	rawTCPConn, err := net.DialTimeout("tcp", manager.listeners[0].TCPListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial traffic listener: %v", err)
	}
	tcpConn := rawTCPConn.(*net.TCPConn)
	registerTCPAdmissionClose(t, "backup TCP connection", tcpConn)
	peerStream, err := backupPeer.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("backup AcceptStream() error = %v", err)
	}
	var newConn protocol.NewConnMsg
	if err := protocol.ReadTypedMessage(peerStream, protocol.MsgTypeNewConn, &newConn); err != nil {
		t.Fatalf("read backup NewConn message: %v", err)
	}
	waitForTCPAdmissionState(t, backup, 2, 1, manager.listeners[0].tcpSetupSlots)
	snapshot := manager.listeners[0].tcpAdmission.snapshot()
	if snapshot.Attempts != 2 || snapshot.Retries != 1 || snapshot.StreamLimitAttempts != 1 ||
		snapshot.Committed != 1 || snapshot.PeerStreamLimit != 0 ||
		snapshot.GenerationConnectionCapacity != 0 || snapshot.GenerationSetupCapacity != 0 ||
		snapshot.SetupFailure != 0 ||
		tcpTerminalTotal(snapshot) != 1 {
		t.Fatalf("backup-success TCP admission snapshot = %+v", snapshot)
	}
	if preferred.TotalConns.Load() != 0 || backup.TotalConns.Load() != 1 {
		t.Fatalf("preferred/backup totals = %d/%d, want 0/1", preferred.TotalConns.Load(), backup.TotalConns.Load())
	}
	if got := connectionPool.EligibleCount("tcp"); got != 2 {
		t.Fatalf("eligible clients after preferred stream limit = %d, want 2", got)
	}
	peerStream.CancelRead(trafficStreamCancelCode)
	peerStream.CancelWrite(trafficStreamCancelCode)
	if err := tcpConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close backup TCP flow: %v", err)
	}
	waitForActiveConnections(t, backup, 1)
	if snapshot := manager.listeners[0].tcpAdmission.snapshot(); snapshot.ActiveCurrent != 0 {
		t.Fatalf("backup active current after teardown = %d, want 0", snapshot.ActiveCurrent)
	}
}

func TestTCPProvisionalStreamAttachmentLosesToLocalShutdown(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 256*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)
	stream, err := serverConn.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	duplicate, err := serverConn.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream(duplicate) error = %v", err)
	}

	local, remote := net.Pipe()
	registerTCPAdmissionClose(t, "provisional local peer", remote)
	listenerCtx, cancelListener := context.WithCancel(context.Background())
	listener := &Listener{
		ctx:          listenerCtx,
		cancel:       cancelListener,
		flows:        make(map[*tcpFlow]struct{}),
		tcpFlowLimit: config.DefaultMaxTCPConnections,
	}
	flow, ok := listener.addTCPFlow(local)
	if !ok {
		t.Fatal("addTCPFlow() rejected before shutdown")
	}
	if !flow.setStream(stream) {
		t.Fatal("setStream() rejected first provisional stream")
	}
	if flow.setStream(duplicate) {
		t.Fatal("setStream() overwrote an attached provisional stream")
	}
	if !flow.commitStream(stream) {
		t.Fatal("duplicate attachment replaced the first provisional stream")
	}
	listener.close()
	if flow.detachStream(stream) {
		t.Fatal("detachStream() succeeded after listener shutdown won the race")
	}
	localRead := make(chan error, 1)
	go func() {
		_, err := remote.Read(make([]byte, 1))
		localRead <- err
	}()
	select {
	case err := <-localRead:
		if err == nil {
			t.Fatal("local TCP peer remained open after shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("local TCP peer remained blocked after shutdown")
	}

	for i := range 2 {
		peerStream, err := peerConn.AcceptStream(testCtx)
		if err != nil {
			t.Fatalf("AcceptStream(%d) after provisional reset error = %v", i, err)
		}
		if err := peerStream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set provisional stream %d read deadline: %v", i, err)
		}
		if _, err := io.ReadAll(peerStream); err == nil {
			t.Fatalf("provisional QUIC stream %d ended without reset", i)
		} else {
			var streamErr *quic.StreamError
			if !errors.As(err, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != trafficStreamCancelCode {
				t.Fatalf("provisional stream %d read error = %T %v, want remote reset code %d", i, err, err, trafficStreamCancelCode)
			}
		}
		if err := peerStream.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set provisional stream %d write deadline: %v", i, err)
		}
		writeBuffer := make([]byte, 32*1024)
		var writeErr error
		for writeErr == nil {
			_, writeErr = peerStream.Write(writeBuffer)
		}
		var streamErr *quic.StreamError
		if !errors.As(writeErr, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != trafficStreamCancelCode || streamErr.StreamID != peerStream.StreamID() {
			t.Fatalf(
				"provisional stream %d write error = %T %v, want remote STOP_SENDING for stream %d code %d",
				i, writeErr, writeErr, peerStream.StreamID(), trafficStreamCancelCode,
			)
		}
	}
}

func TestTCPNewConnDeadlineDoesNotPoisonGeneration(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 32*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)

	connectionPool, client, listener, done := startBlockedNewConn(t, serverConn, "deadline-client", 200*time.Millisecond)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("NewConn write did not obey its setup deadline")
	}
	if got := connectionPool.EligibleCount("tcp"); got != 1 {
		t.Fatalf("eligible clients after NewConn deadline = %d, want 1", got)
	}
	if active, total := client.ActiveConns.Load(), client.TotalConns.Load(); active != 0 || total != 0 {
		t.Fatalf("deadline client active/total = %d/%d, want 0/0", active, total)
	}
	if held := len(listener.tcpSetupSlots); held != 0 {
		t.Fatalf("setup permits after NewConn deadline = %d, want 0", held)
	}
	snapshot := listener.tcpAdmission.snapshot()
	if snapshot.SetupCurrent != 0 || snapshot.ActiveCurrent != 0 || snapshot.Attempts != 1 ||
		snapshot.Deadline != 1 || tcpTerminalTotal(snapshot) != 1 {
		t.Fatalf("deadline TCP admission snapshot = %+v", snapshot)
	}

	peerStream, err := peerConn.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("AcceptStream() after NewConn deadline error = %v", err)
	}
	if err := peerStream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline-reset read deadline: %v", err)
	}
	if _, err := io.ReadAll(peerStream); err == nil {
		t.Fatal("deadline-aborted NewConn stream ended without reset")
	}
}

func TestTCPPartialNewConnResetUsesBackupAndReleasesPrimaryLease(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelTest()
	setup := startTCPFallbackSetup(t, testCtx, partialNewConnWindow)
	connectionPool, primary, backup := setup.connectionPool, setup.primary, setup.backup
	listener := setup.listener
	tcpConn, primaryStream, backupStream := openPartialTCPFallback(t, testCtx, setup, func(stream *quic.Stream) {
		stream.CancelRead(73)
	})
	waitForTCPAdmissionState(t, backup, 2, 1, listener.tcpSetupSlots)
	if active, total := primary.ActiveConns.Load(), primary.TotalConns.Load(); active != 0 || total != 0 {
		t.Fatalf("partial primary active/total = %d/%d, want 0/0", active, total)
	}
	if got := connectionPool.EligibleCount("tcp"); got != 2 {
		t.Fatalf("eligible clients after partial stream reset = %d, want 2", got)
	}

	if err := primaryStream.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set primary reset write deadline: %v", err)
	}
	writeBuffer := make([]byte, 32*1024)
	var writeErr error
	for writeErr == nil {
		_, writeErr = primaryStream.Write(writeBuffer)
	}
	var streamErr *quic.StreamError
	if !errors.As(writeErr, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != trafficStreamCancelCode {
		t.Fatalf("primary write error = %T %v, want remote STOP_SENDING code %d", writeErr, writeErr, trafficStreamCancelCode)
	}

	backupStream.CancelRead(trafficStreamCancelCode)
	backupStream.CancelWrite(trafficStreamCancelCode)
	if err := tcpConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close backup TCP flow: %v", err)
	}
	waitForActiveConnections(t, backup, 1)
	if !connectionPool.MarkUnhealthy(backup) {
		t.Fatal("MarkUnhealthy(backup) = false")
	}

	leases := make([]*pool.TCPLease, 0, 16)
	for i := range 16 {
		admission, err := connectionPool.BeginTCPAdmission()
		if err != nil {
			t.Fatalf("BeginTCPAdmission(%d) error = %v", i, err)
		}
		lease, err := admission.Next()
		if err != nil || lease == nil {
			t.Fatalf("primary lease probe %d = (%v, %v), want lease", i, lease, err)
		}
		leases = append(leases, lease)
	}
	admission, err := connectionPool.BeginTCPAdmission()
	if err != nil {
		t.Fatalf("BeginTCPAdmission(17) error = %v", err)
	}
	if lease, err := admission.Next(); lease != nil || !errors.Is(err, pool.ErrTCPGenerationSetupCapacity) {
		t.Fatalf("17th primary lease probe = (%v, %v), want generation setup capacity", lease, err)
	}
	for i, lease := range leases {
		if !lease.Release() {
			t.Fatalf("release primary lease probe %d = false", i)
		}
	}
	waitForTCPAdmissionState(t, primary, 0, 0, listener.tcpSetupSlots)
}

func TestTCPNewConnStreamResetDoesNotPoisonGeneration(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 32*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)

	connectionPool, client, listener, done := startBlockedNewConn(t, serverConn, "stream-reset-client", 3*time.Second)

	peerStream, err := peerConn.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("AcceptStream() before reset error = %v", err)
	}
	peerStream.CancelRead(73)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("NewConn write did not stop after peer stream reset")
	}
	if got := connectionPool.EligibleCount("tcp"); got != 1 {
		t.Fatalf("eligible clients after NewConn stream reset = %d, want 1", got)
	}
	if active, total := client.ActiveConns.Load(), client.TotalConns.Load(); active != 0 || total != 0 {
		t.Fatalf("stream-reset client active/total = %d/%d, want 0/0", active, total)
	}
	if held := len(listener.tcpSetupSlots); held != 0 {
		t.Fatalf("setup permits after NewConn stream reset = %d, want 0", held)
	}
	if snapshot := listener.tcpAdmission.snapshot(); snapshot.SetupFailure != 1 || tcpTerminalTotal(snapshot) != 1 {
		t.Fatalf("stream-reset TCP admission snapshot = %+v, want one setup failure", snapshot)
	}

	tcpConn := startTCPFallbackFlow(t, listener)
	stream, err := peerConn.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("AcceptStream() for next request error = %v", err)
	}
	if err := stream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set next request read deadline: %v", err)
	}
	var newConn protocol.NewConnMsg
	if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeNewConn, &newConn); err != nil {
		t.Fatalf("read next request NewConn message: %v", err)
	}
	if current, ok := connectionPool.Get(client.ID); !ok || current != client {
		t.Fatalf("current generation after stream reset = (%p, %v), want (%p, true)", current, ok, client)
	}
	waitForTCPAdmissionState(t, client, 1, 1, listener.tcpSetupSlots)
	stream.CancelRead(trafficStreamCancelCode)
	stream.CancelWrite(trafficStreamCancelCode)
	if err := tcpConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close next TCP flow: %v", err)
	}
	waitForActiveConnections(t, client, 0)
	snapshot := listener.tcpAdmission.snapshot()
	if snapshot.SetupCurrent != 0 || snapshot.ActiveCurrent != 0 || snapshot.Attempts != 2 ||
		snapshot.Committed != 1 || snapshot.SetupFailure != 1 || tcpTerminalTotal(snapshot) != 2 {
		t.Fatalf("stream-reset recovery TCP admission snapshot = %+v", snapshot)
	}
}

func TestTCPSetupShutdownCountsCanceledOnce(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 32*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)

	_, _, listener, done := startBlockedNewConn(t, serverConn, "shutdown-client", 3*time.Second)
	if _, err := peerConn.AcceptStream(testCtx); err != nil {
		t.Fatalf("AcceptStream() before setup shutdown error = %v", err)
	}
	listener.close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TCP setup did not stop after listener shutdown")
	}
	snapshot := listener.tcpAdmission.snapshot()
	if snapshot.SetupCurrent != 0 || snapshot.ActiveCurrent != 0 || snapshot.Attempts != 1 ||
		snapshot.Canceled != 1 || tcpTerminalTotal(snapshot) != 1 {
		t.Fatalf("canceled TCP admission snapshot = %+v", snapshot)
	}
}

func TestTCPNewConnConnectionCloseUsesBackup(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelTest()
	setup := startTCPFallbackSetup(t, testCtx, partialNewConnWindow)
	primary, backup := setup.primary, setup.backup
	listener := setup.listener
	tcpConn, _, backupStream := openPartialTCPFallback(t, testCtx, setup, func(*quic.Stream) {
		if err := setup.primaryPeer.CloseWithError(91, "close during NewConn write"); err != nil {
			t.Fatalf("close peer during NewConn write: %v", err)
		}
	})
	if active, total := primary.ActiveConns.Load(), primary.TotalConns.Load(); active != 0 || total != 0 {
		t.Fatalf("connection-close client active/total = %d/%d, want 0/0", active, total)
	}
	waitForTCPAdmissionState(t, backup, 2, 1, listener.tcpSetupSlots)
	backupStream.CancelRead(trafficStreamCancelCode)
	backupStream.CancelWrite(trafficStreamCancelCode)
	if err := tcpConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close backup TCP flow: %v", err)
	}
	waitForActiveConnections(t, backup, 1)
}

func TestTCPConnectionScopedOpenFailureUsesBackup(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()
	setup := startTCPFallbackSetup(t, testCtx, 256*1024)
	if err := setup.primary.Conn.CloseWithError(1, "connection failure"); err != nil {
		t.Fatalf("close selected QUIC generation: %v", err)
	}
	tcpConn := startTCPFallbackFlow(t, setup.listener)
	backupStream, err := setup.backupPeer.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("backup AcceptStream() error = %v", err)
	}
	var newConn protocol.NewConnMsg
	if err := protocol.ReadTypedMessage(backupStream, protocol.MsgTypeNewConn, &newConn); err != nil {
		t.Fatalf("read backup NewConn message: %v", err)
	}
	if active, total := setup.primary.ActiveConns.Load(), setup.primary.TotalConns.Load(); active != 0 || total != 0 {
		t.Fatalf("failed generation active/total = %d/%d, want 0/0", active, total)
	}
	waitForTCPAdmissionState(t, setup.backup, 2, 1, setup.listener.tcpSetupSlots)
	backupStream.CancelRead(trafficStreamCancelCode)
	backupStream.CancelWrite(trafficStreamCancelCode)
	if err := tcpConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close backup TCP flow: %v", err)
	}
	waitForActiveConnections(t, setup.backup, 1)
}
