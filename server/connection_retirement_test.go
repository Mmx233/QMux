package server

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/auth/mtls"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/Mmx233/QMux/server/traffic"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

func newServerControlQUICPair(t *testing.T, ctx context.Context) (*quic.Conn, *quic.Conn) {
	t.Helper()
	clientCertificate, clientRoots := registrationTestClientCertificate(
		t,
		"oversized-control-client",
		false,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	)
	serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       10 * time.Second,
	})
	if err != nil {
		t.Fatalf("listen raw control QUIC: %v", err)
	}
	type acceptResult struct {
		conn *quic.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, acceptErr := listener.Accept(ctx)
		accepted <- acceptResult{conn: conn, err: acceptErr}
	}()
	peer, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       10 * time.Second,
	})
	if err != nil {
		_ = listener.Close()
		t.Fatalf("dial raw control QUIC: %v", err)
	}
	var serverConn *quic.Conn
	select {
	case result := <-accepted:
		if result.err != nil {
			_ = peer.CloseWithError(0, "accept failed")
			_ = listener.Close()
			t.Fatalf("accept raw control QUIC: %v", result.err)
		}
		serverConn = result.conn
	case <-ctx.Done():
		_ = peer.CloseWithError(0, "accept timeout")
		_ = listener.Close()
		t.Fatal("accept raw control QUIC timed out")
	}
	t.Cleanup(func() {
		_ = peer.CloseWithError(0, "test complete")
		_ = serverConn.CloseWithError(0, "test complete")
		_ = listener.Close()
	})
	return serverConn, peer
}

func TestTrafficConnectionFatalRetiresRegistrationForSameID(t *testing.T) {
	clientCertificate, clientRoots := registrationTestClientCertificate(
		t,
		"client-retirement",
		false,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	)
	serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
	harness := newRegistrationHarnessWithTLS(t, mtls.New(), time.Second, serverTLS, clientTLS)
	const clientID = "retired-client-id"
	registerTCPClient := func() {
		controlStream := harness.openStream(t)
		if err := protocol.WriteRegisterWithAuth(
			controlStream,
			clientID,
			protocol.ProtocolVersion,
			[]string{"tcp", protocol.CapabilityUDPWireV2},
			nil,
		); err != nil {
			t.Fatalf("WriteRegisterWithAuth() error = %v", err)
		}
		var ack protocol.RegisterAckMsg
		if err := protocol.ReadTypedMessage(controlStream, protocol.MsgTypeRegisterAck, &ack); err != nil {
			t.Fatalf("read registration Ack: %v", err)
		}
		if err := protocol.ValidateRegisterAckWithAuth(ack, ""); err != nil {
			t.Fatalf("ValidateRegisterAckWithAuth() error = %v", err)
		}
		eventually(t, time.Second, func() bool { return harness.pool.Count() == 1 })
	}
	registerTCPClient()
	stale, ok := harness.pool.Get(clientID)
	if !ok {
		t.Fatal("registered generation missing from pool")
	}

	liveDuplicate := &pool.ClientConn{ID: clientID}
	if _, err := harness.pool.Reserve(liveDuplicate); err == nil {
		t.Fatal("live duplicate registration was not rejected")
	}
	if got := harness.pool.Count(); got != 1 {
		t.Fatalf("pool Count() after live duplicate rejection = %d, want 1", got)
	}

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve traffic address: %v", err)
	}
	trafficAddr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("release traffic address: %v", err)
	}
	manager := traffic.NewManager(&config.Server{Listeners: []config.QuicListener{{
		QuicAddr:    registrationTestAddress,
		TrafficAddr: trafficAddr,
		Protocol:    "tcp",
	}}}, map[string]*pool.ConnectionPool{registrationTestAddress: harness.pool}, protocol.NewCopyBufferPool(protocol.DefaultCopyBufferSize), zerolog.Nop())
	t.Cleanup(manager.Stop)
	if err := manager.Start(harness.ctx); err != nil {
		t.Fatalf("start traffic manager: %v", err)
	}
	tcpConn, err := net.DialTimeout("tcp", trafficAddr, time.Second)
	if err != nil {
		t.Fatalf("dial traffic listener: %v", err)
	}
	t.Cleanup(func() { _ = tcpConn.Close() })
	streamCtx, cancelStream := context.WithTimeout(context.Background(), time.Second)
	defer cancelStream()
	trafficStream, err := harness.client.AcceptStream(streamCtx)
	if err != nil {
		t.Fatalf("accept traffic stream: %v", err)
	}
	var newConn protocol.NewConnMsg
	if err := protocol.ReadTypedMessage(trafficStream, protocol.MsgTypeNewConn, &newConn); err != nil {
		t.Fatalf("read traffic NewConn message: %v", err)
	}
	if err := harness.client.CloseWithError(91, "fatal traffic connection failure"); err != nil {
		t.Fatalf("close registered connection: %v", err)
	}
	select {
	case <-harness.serverConn.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("server QUIC connection context did not close after fatal traffic connection failure")
	}
	harness.waitForHandler(t)
	if got := harness.pool.Count(); got != 0 {
		t.Fatalf("pool Count() after exact deferred Remove = %d, want 0", got)
	}

	harness.reconnect(t, clientTLS, &quic.Config{})
	registerTCPClient()
	fresh, ok := harness.pool.Get(clientID)
	if !ok || fresh == stale {
		t.Fatalf("re-registered generation = (%p, %v), stale = %p", fresh, ok, stale)
	}
	if got := harness.pool.EligibleCount("tcp"); got != 1 {
		t.Fatalf("eligible TCP generations after re-registration = %d, want 1", got)
	}
	select {
	case <-fresh.Conn.Context().Done():
		t.Fatalf("fresh generation context closed after registration: %v", context.Cause(fresh.Conn.Context()))
	default:
	}
}

func TestControlStreamTerminalRetiresRegisteredConnection(t *testing.T) {
	tests := []struct {
		name      string
		terminate func(*quic.Stream) error
	}{
		{
			name: "fin",
			terminate: func(stream *quic.Stream) error {
				return stream.Close()
			},
		},
		{
			name: "reset",
			terminate: func(stream *quic.Stream) error {
				stream.CancelWrite(91)
				return nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clientCertificate, clientRoots := registrationTestClientCertificate(
				t,
				"control-terminal-"+test.name,
				false,
				[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			)
			serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
			harness := newRegistrationHarnessWithTLS(t, mtls.New(), time.Second, serverTLS, clientTLS)
			controlStream := registerMTLSClient(t, harness, "control-terminal-client")
			registered, ok := harness.pool.Get("control-terminal-client")
			if !ok {
				t.Fatal("registered generation missing from pool")
			}
			select {
			case <-registered.Conn.Context().Done():
				t.Fatalf("registered connection closed before control stream termination: %v", context.Cause(registered.Conn.Context()))
			default:
			}

			if err := test.terminate(controlStream); err != nil {
				t.Fatalf("terminate control stream: %v", err)
			}
			select {
			case <-harness.client.Context().Done():
			case <-time.After(time.Second):
				t.Fatal("client connection did not close after control stream terminated")
			}
			harness.waitForHandler(t)
			if got := harness.pool.Count(); got != 0 {
				t.Fatalf("pool Count() after control stream termination = %d, want 0", got)
			}
		})
	}
}

func TestStaleControlHeartbeatRetiresOnlyItsGeneration(t *testing.T) {
	clientCertificate, clientRoots := registrationTestClientCertificate(
		t,
		"stale-control-heartbeat",
		false,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	)
	serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
	harness := newRegistrationHarnessWithTLS(t, mtls.New(), time.Second, serverTLS, clientTLS)
	const clientID = "stale-control-client"
	controlStream := registerMTLSClient(t, harness, clientID)
	stale, ok := harness.pool.Get(clientID)
	if !ok {
		t.Fatal("registered stale generation missing from pool")
	}
	if !harness.pool.Remove(stale) {
		t.Fatal("remove stale generation before heartbeat failed")
	}
	fresh := &pool.ClientConn{ID: clientID}
	if err := harness.pool.Add(fresh); err != nil {
		t.Fatalf("add fresh same-ID generation: %v", err)
	}

	if err := protocol.WriteHeartbeat(controlStream, time.Now().Unix()); err != nil {
		t.Fatalf("write heartbeat from stale generation: %v", err)
	}
	select {
	case <-harness.client.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("stale QUIC generation did not close after heartbeat")
	}
	harness.waitForHandler(t)
	got, ok := harness.pool.Get(clientID)
	if !ok || got != fresh {
		t.Fatalf("pool generation after stale cleanup = (%p, %v), want fresh %p", got, ok, fresh)
	}
}

func TestOversizedControlPayloadRetiresOnlyItsGeneration(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTest()
	serverQUIC, peerQUIC := newServerControlQUICPair(t, testCtx)
	peerStream, err := peerQUIC.OpenStreamSync(testCtx)
	if err != nil {
		t.Fatalf("open oversized control stream: %v", err)
	}
	var header [5]byte
	header[0] = protocol.MsgTypeHeartbeat
	binary.BigEndian.PutUint32(header[1:], protocol.MaxControlPayloadSize+1)
	if n, err := peerStream.Write(header[:]); err != nil || n != len(header) {
		t.Fatalf("write oversized control header = (%d, %v)", n, err)
	}
	controlStream, err := serverQUIC.AcceptStream(testCtx)
	if err != nil {
		t.Fatalf("accept oversized control stream: %v", err)
	}

	connectionPool := pool.New(registrationTestAddress, pool.NewRoundRobinBalancer(), zerolog.Nop())
	t.Cleanup(connectionPool.Stop)
	const clientID = "oversized-control-generation"
	old := &pool.ClientConn{
		ID:            clientID,
		Conn:          serverQUIC,
		ControlStream: controlStream,
		RegisteredAt:  time.Now(),
		Metadata:      pool.ClientMetadata{Capabilities: []string{"tcp"}},
	}
	if err := connectionPool.Add(old); err != nil {
		t.Fatalf("add old control generation: %v", err)
	}
	s := &Server{
		config: &config.Server{
			HeartbeatInterval: time.Hour,
			HealthTimeout:     2 * time.Hour,
		},
		logger: zerolog.Nop(),
	}
	type controlResult struct {
		closeReason string
		retirement  *pool.Retirement
	}
	controlDone := make(chan controlResult, 1)
	go func() {
		closeReason, retirement := s.handleControlStream(testCtx, connectionPool, old, registrationTestAddress)
		controlDone <- controlResult{closeReason: closeReason, retirement: retirement}
	}()

	var result controlResult
	select {
	case result = <-controlDone:
	case <-time.After(time.Second):
		t.Fatal("oversized control handler waited for payload body or EOF")
	}
	if testErr, connErr := testCtx.Err(), old.Conn.Context().Err(); testErr != nil || connErr != nil {
		t.Fatalf("oversized control handler returned after unrelated shutdown: test context = %v, old connection = %v", testErr, connErr)
	}
	if result.retirement == nil {
		t.Fatalf("oversized control retirement is nil (close reason %q)", result.closeReason)
	}
	if current, ok := connectionPool.Get(clientID); ok || current != nil {
		t.Fatalf("old generation remained current after oversized control payload: (%p, %t)", current, ok)
	}

	replacement := &pool.ClientConn{
		ID:       clientID,
		Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}},
	}
	if err := connectionPool.Add(replacement); err != nil {
		t.Fatalf("add same-ID replacement before old cleanup: %v", err)
	}
	if err := old.Conn.CloseWithError(0, "old oversized control generation"); err != nil {
		t.Fatalf("close old control generation: %v", err)
	}
	select {
	case <-old.Conn.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("old control generation context did not close")
	}
	if !result.retirement.Done() {
		t.Fatal("complete old control generation retirement failed")
	}
	current, ok := connectionPool.Get(clientID)
	if !ok || current != replacement || !connectionPool.IsCurrentEligible(replacement, "tcp") {
		t.Fatalf("replacement after old cleanup = (%p, %t, eligible=%t), want current eligible %p",
			current, ok, connectionPool.IsCurrentEligible(replacement, "tcp"), replacement)
	}
}

func registerDrainClient(t *testing.T, harness *registrationHarness, clientID string) *quic.Stream {
	t.Helper()
	stream := harness.openStream(t)
	if err := protocol.WriteRegisterWithAuth(stream, clientID, protocol.ProtocolVersion,
		[]string{"tcp", protocol.CapabilityUDPWireV2, protocol.CapabilityTCPDrainV1}, nil); err != nil {
		t.Fatalf("WriteRegisterWithAuth() error = %v", err)
	}
	var ack protocol.RegisterAckMsg
	if err := protocol.ReadTypedMessage(stream, protocol.MsgTypeRegisterAck, &ack); err != nil {
		t.Fatalf("read registration Ack: %v", err)
	}
	if !protocol.HasCapability(ack.SelectedCapabilities, protocol.CapabilityTCPDrainV1) {
		t.Fatalf("selected capabilities = %v, missing drain", ack.SelectedCapabilities)
	}
	eventually(t, time.Second, func() bool { return harness.pool.Count() == 1 })
	return stream
}

func TestDrainRequestRetiresGenerationAndKeepsRetiredHeartbeatAlive(t *testing.T) {
	clientCertificate, clientRoots := registrationTestClientCertificate(
		t, "drain-retirement", false, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
	harness := newRegistrationHarnessWithTLS(t, mtls.New(), time.Second, serverTLS, clientTLS)
	const clientID = "draining-client"
	controlStream := registerDrainClient(t, harness, clientID)
	old, _ := harness.pool.Get(clientID)
	if err := old.ControlStream.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("set stale DrainComplete deadline: %v", err)
	}

	if err := protocol.WriteDrainRequest(controlStream); err != nil {
		t.Fatalf("WriteDrainRequest() error = %v", err)
	}
	msgType, payload, err := protocol.ReadMessage(controlStream)
	if err != nil {
		t.Fatalf("read DrainComplete: %v", err)
	}
	if msgType != protocol.MsgTypeDrainComplete {
		t.Fatalf("message type = 0x%x, want DrainComplete", msgType)
	}
	complete, err := protocol.DecodeDrainComplete(payload)
	if err != nil || complete.AcceptFence != -1 {
		t.Fatalf("DrainComplete = (%+v, %v), want fence -1", complete, err)
	}
	if harness.pool.IsCurrentEligible(old, "tcp") {
		t.Fatal("old generation remained eligible after DrainRequest")
	}
	fresh := &pool.ClientConn{ID: clientID, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	if err := harness.pool.Add(fresh); err != nil {
		t.Fatalf("add same-ID fresh generation: %v", err)
	}
	if err := protocol.WriteDrainRequest(controlStream); err != nil {
		t.Fatalf("write duplicate DrainRequest: %v", err)
	}
	for range 3 {
		if err := protocol.WriteHeartbeat(controlStream, time.Now().Unix()); err != nil {
			t.Fatalf("write retired heartbeat: %v", err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if current, ok := harness.pool.Get(clientID); !ok || current != fresh {
		t.Fatalf("current generation = (%p, %v), want fresh %p", current, ok, fresh)
	}
	select {
	case <-harness.client.Context().Done():
		t.Fatalf("retired generation closed while heartbeating: %v", context.Cause(harness.client.Context()))
	default:
	}
}

func TestDrainCompleteWriteDeadlineRejectsExpiredHealthBound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	deadline, fresh := drainCompleteWriteDeadline(now, time.Second, now.Add(-time.Nanosecond))
	if fresh || !deadline.Before(now) {
		t.Fatalf("drain deadline = (%v, %t), want expired", deadline, fresh)
	}
}

func TestDrainCompleteWriteFailureClosesOnlyRetiredGeneration(t *testing.T) {
	clientCertificate, clientRoots := registrationTestClientCertificate(
		t, "drain-write-failure", false, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
	harness := newRegistrationHarnessWithTLS(t, mtls.New(), time.Second, serverTLS, clientTLS)
	const clientID = "drain-write-failure-client"
	controlStream := registerDrainClient(t, harness, clientID)
	old, _ := harness.pool.Get(clientID)
	controlStream.CancelRead(99)
	if err := protocol.WriteDrainRequest(controlStream); err != nil {
		t.Fatalf("WriteDrainRequest() error = %v", err)
	}
	eventually(t, time.Second, func() bool { return !harness.pool.IsCurrentEligible(old, "tcp") })
	fresh := &pool.ClientConn{ID: clientID, Metadata: pool.ClientMetadata{Capabilities: []string{"tcp"}}}
	if err := harness.pool.Add(fresh); err != nil {
		t.Fatalf("add fresh generation: %v", err)
	}
	select {
	case <-harness.client.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("old generation did not close after DrainComplete write failure")
	}
	harness.waitForHandler(t)
	if current, ok := harness.pool.Get(clientID); !ok || current != fresh {
		t.Fatalf("current generation = (%p, %v), want fresh %p", current, ok, fresh)
	}
}
