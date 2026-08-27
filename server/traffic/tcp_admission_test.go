package traffic

import (
	"context"
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

const defaultQUICIncomingStreams = 100

func startTCPAdmissionManager(t *testing.T, quicAddr string, connectionPool *pool.ConnectionPool) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	manager := NewManager(&config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    quicAddr,
		TrafficAddr: "127.0.0.1:0",
		Protocol:    "tcp",
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
		tcpSetupSlots: make(chan struct{}, maxPendingTCPSetups),
	}
	release, ok := acquireTCPSetup(listener.tcpSetupSlots)
	if !ok {
		t.Fatal("acquireTCPSetup() = false")
	}
	local, remote := net.Pipe()
	registerTCPAdmissionClose(t, "blocked NewConn peer", remote)
	done := make(chan struct{})
	go func() {
		listener.handleTCPConnection(local, time.Now().Add(setupTimeout), release)
		close(done)
	}()
	return connectionPool, client, listener, done
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
	manager := startTCPAdmissionManager(t, quicAddr, connectionPool)
	listener := manager.listeners[0]

	releases := make([]func(), 0, maxPendingTCPSetups)
	for range maxPendingTCPSetups {
		release, ok := acquireTCPSetup(listener.tcpSetupSlots)
		if !ok {
			t.Fatalf("acquireTCPSetup() rejected before %d permits", maxPendingTCPSetups)
		}
		releases = append(releases, release)
	}
	if release, ok := acquireTCPSetup(listener.tcpSetupSlots); ok {
		release()
		t.Fatal("acquireTCPSetup() exceeded listener bound")
	}
	if elapsed := dialRejectedTCP(t, listener.TCPListener.Addr().String()); elapsed >= 2*time.Second {
		t.Fatalf("listener-capacity rejection took %v, want immediate close", elapsed)
	}
	for _, release := range releases {
		release()
		release()
	}

	tcpConn, peerStream := openRelayLifecycleFlow(t, testCtx, manager, peerConn)
	waitForTCPAdmissionState(t, client, 1, 1, listener.tcpSetupSlots)
	peerStream.CancelRead(trafficStreamCancelCode)
	peerStream.CancelWrite(trafficStreamCancelCode)
	if err := tcpConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close recovered TCP flow: %v", err)
	}
	waitForActiveConnections(t, client, 0)
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

	local, remote := net.Pipe()
	registerTCPAdmissionClose(t, "provisional local peer", remote)
	listenerCtx, cancelListener := context.WithCancel(context.Background())
	listener := &Listener{
		ctx:    listenerCtx,
		cancel: cancelListener,
		flows:  make(map[*tcpFlow]struct{}),
	}
	flow, ok := listener.addTCPFlow(local)
	if !ok {
		t.Fatal("addTCPFlow() rejected before shutdown")
	}
	listener.close()
	if flow.setStream(stream) {
		t.Fatal("setStream() committed after listener shutdown")
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

	peerStream, err := peerConn.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("AcceptStream() after provisional reset error = %v", err)
	}
	if err := peerStream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set provisional stream read deadline: %v", err)
	}
	if _, err := io.ReadAll(peerStream); err == nil {
		t.Fatal("provisional QUIC stream ended without reset")
	} else {
		var streamErr *quic.StreamError
		if !errors.As(err, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != trafficStreamCancelCode {
			t.Fatalf("provisional stream read error = %T %v, want remote reset code %d", err, err, trafficStreamCancelCode)
		}
	}
	if err := peerStream.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set provisional stream write deadline: %v", err)
	}
	writeBuffer := make([]byte, 32*1024)
	var writeErr error
	for writeErr == nil {
		_, writeErr = peerStream.Write(writeBuffer)
	}
	var streamErr *quic.StreamError
	if !errors.As(writeErr, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != trafficStreamCancelCode || streamErr.StreamID != peerStream.StreamID() {
		t.Fatalf(
			"provisional stream write error = %T %v, want remote STOP_SENDING for stream %d code %d",
			writeErr, writeErr, peerStream.StreamID(), trafficStreamCancelCode,
		)
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
}

func TestTCPNewConnConnectionCloseMarksGenerationUnhealthy(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 32*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)

	connectionPool, client, listener, done := startBlockedNewConn(t, serverConn, "connection-close-client", 3*time.Second)
	if _, err := peerConn.AcceptStream(testCtx); err != nil {
		t.Fatalf("AcceptStream() before connection close error = %v", err)
	}
	if err := peerConn.CloseWithError(91, "close during NewConn write"); err != nil {
		t.Fatalf("close peer during NewConn write: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("NewConn handler did not stop after peer connection close")
	}
	if got := connectionPool.EligibleCount("tcp"); got != 0 {
		t.Fatalf("eligible clients after NewConn connection close = %d, want 0", got)
	}
	if current, ok := connectionPool.Get(client.ID); !ok || current != client {
		t.Fatalf("current generation after NewConn connection close = (%p, %v), want (%p, true)", current, ok, client)
	}
	if active, total := client.ActiveConns.Load(), client.TotalConns.Load(); active != 0 || total != 0 {
		t.Fatalf("connection-close client active/total = %d/%d, want 0/0", active, total)
	}
	if held := len(listener.tcpSetupSlots); held != 0 {
		t.Fatalf("setup permits after NewConn connection close = %d, want 0", held)
	}
}

func TestTCPConnectionScopedOpenFailureMarksExactGenerationUnhealthy(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTest()
	quicListener, serverConn, peerConn := newRelayLifecycleQUICPair(t, testCtx, 256*1024)
	registerRelayLifecycleQUICCleanup(t, quicListener, serverConn, peerConn)
	if err := serverConn.CloseWithError(1, "connection failure"); err != nil {
		t.Fatalf("close selected QUIC generation: %v", err)
	}

	quicAddr := quicListener.Addr().String()
	connectionPool := pool.New(quicAddr, pool.NewRoundRobinBalancer(), zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	client := &pool.ClientConn{ID: "failed-generation", Conn: serverConn, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	if err := connectionPool.Add(client); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	manager := startTCPAdmissionManager(t, quicAddr, connectionPool)
	_ = dialRejectedTCP(t, manager.listeners[0].TCPListener.Addr().String())
	if got := connectionPool.EligibleCount("tcp"); got != 0 {
		t.Fatalf("eligible clients after connection-scoped OpenStream failure = %d, want 0", got)
	}
	if active, total := client.ActiveConns.Load(), client.TotalConns.Load(); active != 0 || total != 0 {
		t.Fatalf("failed generation active/total = %d/%d, want 0/0", active, total)
	}
}
