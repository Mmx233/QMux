package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestGracefulShutdownDrainsActiveTCPAndCleansUDP(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelTest()
	certDir := generateTestCertificates(t)

	backendTCP, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("start graceful TCP backend: %v", err)
	}
	backendPort := backendTCP.Addr().(*net.TCPAddr).Port
	backendUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: backendPort})
	if err != nil {
		_ = backendTCP.Close()
		t.Fatalf("start graceful UDP backend: %v", err)
	}
	t.Cleanup(func() { _ = errors.Join(backendTCP.Close(), backendUDP.Close()) })
	serveUDPEcho(backendUDP, false)

	request := bytes.Repeat([]byte("graceful-request-"), 4096)
	tail := bytes.Repeat([]byte("graceful-tail-"), 32768)
	requestRead := make(chan struct{})
	releaseTail := make(chan struct{})
	defer func() {
		select {
		case <-releaseTail:
		default:
			close(releaseTail)
		}
	}()
	backendDone := make(chan error, 1)
	go func() {
		conn, acceptErr := backendTCP.AcceptTCP()
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
			backendDone <- fmt.Errorf("backend request length = %d, want %d", len(got), len(request))
			return
		}
		close(requestRead)
		<-releaseTail
		if _, writeErr := conn.Write(tail); writeErr != nil {
			backendDone <- writeErr
			return
		}
		backendDone <- conn.CloseWrite()
	}()

	quicPort, trafficPort, releaseServerPorts := reserveRestartServerPorts(t)
	if err := releaseServerPorts(); err != nil {
		t.Fatalf("release graceful server ports: %v", err)
	}
	serverConfig := newMTLSServerConfig(certDir, "both", quicPort, trafficPort, 20*time.Millisecond, 2*time.Second)
	serverConfig.Listeners[0].InitialStreamReceiveWindow = 16 * 1024
	serverConfig.Listeners[0].MaxStreamReceiveWindow = 16 * 1024
	serverConfig.Listeners[0].InitialConnectionReceiveWindow = 32 * 1024
	serverConfig.Listeners[0].MaxConnectionReceiveWindow = 32 * 1024
	timeline := newFaultTimeline(t, "graceful shutdown")
	serverRun := startFaultServer(t, testCtx, "graceful server", serverConfig, timeline)
	defer func() {
		if err := serverRun.run.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop graceful server: %v", err)
		}
	}()

	clientConfig := newMTLSClientConfig(certDir, "graceful-e2e-client", backendPort,
		20*time.Millisecond, 2*time.Second, quicPort)
	clientConfig.Quic.InitialStreamReceiveWindow = 16 * 1024
	clientConfig.Quic.MaxStreamReceiveWindow = 16 * 1024
	clientConfig.Quic.InitialConnectionReceiveWindow = 32 * 1024
	clientConfig.Quic.MaxConnectionReceiveWindow = 32 * 1024
	waitForQUICListener(t, testCtx, clientConfig, make(chan error))
	clientInstance := newTestClient(t, clientConfig)
	clientRun := startFaultClient(testCtx, "graceful client", clientInstance, timeline)
	defer func() {
		if err := clientRun.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop graceful client: %v", err)
		}
	}()
	trafficAddress := fmt.Sprintf("127.0.0.1:%d", trafficPort)
	if err := waitForFault(testCtx, 10*time.Second, func() string {
		return fmt.Sprintf("both listener ready; snapshot=%+v", serverRun.Snapshot())
	}, func(time.Duration) bool {
		snapshot := serverRun.Snapshot()
		return len(snapshot.Routes) == 1 && snapshot.Routes[0].Ready
	}, serverRun.run, clientRun); err != nil {
		t.Fatal(err)
	}

	udpConn, err := net.Dial("udp", trafficAddress)
	if err != nil {
		t.Fatalf("dial graceful UDP flow: %v", err)
	}
	defer func() { _ = udpConn.Close() }()
	if err := udpConn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set graceful UDP deadline: %v", err)
	}
	udpPayload := []byte("live UDP session during graceful TCP drain")
	if _, err := udpConn.Write(udpPayload); err != nil {
		t.Fatalf("write graceful UDP flow: %v", err)
	}
	udpEcho := make([]byte, len(udpPayload))
	if _, err := io.ReadFull(udpConn, udpEcho); err != nil {
		t.Fatalf("read graceful UDP echo: %v", err)
	}
	if !bytes.Equal(udpEcho, udpPayload) {
		t.Fatalf("graceful UDP echo = %q, want %q", udpEcho, udpPayload)
	}
	if err := waitForFault(testCtx, 3*time.Second, func() string {
		return fmt.Sprintf("live UDP ownership; client=%+v server=%+v", clientInstance.Snapshot(), serverRun.Snapshot())
	}, func(time.Duration) bool {
		clientSnapshot := clientInstance.Snapshot()
		serverSnapshot := serverRun.Snapshot()
		return clientSnapshot.UDPSessions.Current == 1 && clientSnapshot.UDPSessions.Permits == 1 &&
			serverSnapshot.Routes[0].UDPAdmission.SessionsCurrent == 1
	}, serverRun.run, clientRun); err != nil {
		t.Fatal(err)
	}

	publicConn, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: trafficPort})
	if err != nil {
		t.Fatalf("dial graceful TCP flow: %v", err)
	}
	defer func() { _ = publicConn.Close() }()
	if err := publicConn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("set graceful TCP deadline: %v", err)
	}
	if _, err := publicConn.Write(request); err != nil {
		t.Fatalf("write graceful TCP request: %v", err)
	}
	if err := publicConn.CloseWrite(); err != nil {
		t.Fatalf("half-close graceful TCP request: %v", err)
	}
	select {
	case <-requestRead:
	case err := <-backendDone:
		t.Fatalf("backend failed before tail release: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("backend did not receive the half-closed request")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- clientInstance.Shutdown(context.Background()) }()
	if err := waitForFault(testCtx, 3*time.Second, func() string {
		return fmt.Sprintf("retiring active TCP flow; snapshot=%+v", serverRun.Snapshot())
	}, func(time.Duration) bool {
		route := serverRun.Snapshot().Routes[0]
		return route.TCPEligibleClients == 0 && route.PoolCapacity.ServerRetiring == 1 &&
			route.TCPAdmission.ActiveCurrent == 1
	}, serverRun.run, clientRun); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before the active TCP tail: %v", err)
	default:
	}
	close(releaseTail)
	gotTail, err := io.ReadAll(publicConn)
	if err != nil {
		t.Fatalf("read graceful TCP tail: %v", err)
	}
	if !bytes.Equal(gotTail, tail) {
		t.Fatalf("graceful TCP tail length = %d, want %d", len(gotTail), len(tail))
	}
	if err := <-backendDone; err != nil {
		t.Fatalf("graceful backend completion: %v", err)
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the active TCP tail")
	}
	if err := clientRun.join(5 * time.Second); err != nil {
		t.Fatalf("join graceful client Start: %v", err)
	}

	clientSnapshot := clientInstance.Snapshot()
	if clientSnapshot.UDPSessions.Current != 0 || clientSnapshot.UDPSessions.Permits != 0 ||
		clientSnapshot.UDPSessions.AccountingFaults != 0 || clientSnapshot.DSend.OwnedItems != 0 ||
		clientSnapshot.DSend.OwnedBacking != 0 || clientSnapshot.DSend.Workers != 0 ||
		clientSnapshot.LiveAssemblers != 0 || clientSnapshot.Fragments.RetainedGroups != 0 ||
		clientSnapshot.Fragments.RetainedBackingBytes != 0 {
		t.Fatalf("client ownership after Shutdown = %+v", clientSnapshot)
	}
	for _, endpoint := range clientSnapshot.Endpoints {
		if endpoint.Handshaking != 0 || endpoint.Pending != 0 || endpoint.Registered != 0 ||
			endpoint.Retiring != 0 || endpoint.AccountingFaults != 0 {
			t.Fatalf("client endpoint after Shutdown = %+v", endpoint)
		}
	}
	if err := waitForFault(testCtx, 3*time.Second, func() string {
		return fmt.Sprintf("server retirement cleanup; snapshot=%+v", serverRun.Snapshot())
	}, func(time.Duration) bool {
		route := serverRun.Snapshot().Routes[0]
		return route.Listening && route.PoolCapacity.ServerRetiring == 0 &&
			route.PoolCapacity.ClientGenerations.Current == 0 && route.PoolCapacity.TCPActive == 0 &&
			route.PoolCapacity.UDPSessions == 0 && route.UDPAdmission.SessionsCurrent == 0 &&
			route.UDPAdmission.SessionPermits == 0 && route.UDPAdmission.DSendWorkers == 0
	}, serverRun.run); err != nil {
		t.Fatal(err)
	}
}
