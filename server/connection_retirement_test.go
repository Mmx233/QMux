package server

import (
	"context"
	"crypto/x509"
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

func TestTrafficConnectionFatalRetiresRegistrationForSameID(t *testing.T) {
	clientCertificate, clientRoots := registrationTestClientCertificate(
		t,
		"client-retirement",
		false,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	)
	serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
	harness := newRegistrationHarnessWithTLS(t, mtls.New(clientRoots), time.Second, serverTLS, clientTLS)
	const clientID = "retired-client-id"
	registerTCPClient := func() {
		controlStream := harness.openStream(t)
		if err := protocol.WriteRegister(
			controlStream,
			clientID,
			protocol.ProtocolVersion,
			[]string{"tcp", protocol.CapabilityUDPWireV2},
		); err != nil {
			t.Fatalf("WriteRegister() error = %v", err)
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
	}}}, map[string]*pool.ConnectionPool{registrationTestAddress: harness.pool}, zerolog.Nop())
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
