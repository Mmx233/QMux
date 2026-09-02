package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/config"
	"gopkg.in/yaml.v3"
)

// TestClientAbruptDisconnect_MTLS tests server behavior when a client disconnects abruptly.
func TestClientAbruptDisconnect_MTLS(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and SIGKILLs a child process while waiting for the default 30s health timeout")
	}
	binaryPath := filepath.Join(t.TempDir(), "qmux-test")
	// The parent go test -race does not instrument this separately built child binary.
	buildCmd := exec.Command("go", "build", "-o", binaryPath, ".")
	buildCmd.Dir = ".."
	if output, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build child binary: %v, output: %s", err, output)
	}

	testClientAbruptDisconnect(t, binaryPath, config.DefaultLoadBalancer)
}

// testClientAbruptDisconnect is the core test function for abrupt disconnect scenarios
func testClientAbruptDisconnect(t *testing.T, binaryPath, loadBalancer string) {
	certDir := generateTestCertificates(t)
	_, localPort := startTCPEchoListener(t)
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)
	serverConfig := newMTLSServerConfig(certDir, "tcp", quicPort, trafficPort, 0, 0)
	serverConfig.LoadBalancer = loadBalancer
	serverConfig.ApplyDefaults()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	timeline := newFaultTimeline(t, loadBalancer+" abrupt disconnect")
	serverRun := startFaultServer(t, ctx, loadBalancer+" abrupt server", serverConfig, timeline)
	defer func() {
		if err := serverRun.run.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop abrupt server: %v", err)
		}
	}()

	client1ConfigPath := filepath.Join(certDir, "client1.yaml")
	client1Config := newMTLSClientConfig(certDir, "client-1", localPort, 0, 0, quicPort)
	client1ConfigData, err := yaml.Marshal(client1Config)
	if err != nil {
		t.Fatalf("marshal client1 config: %v", err)
	}
	if err := os.WriteFile(client1ConfigPath, client1ConfigData, 0600); err != nil {
		t.Fatalf("failed to write client1 config: %v", err)
	}

	client1Cmd := exec.CommandContext(ctx, binaryPath, "run", "client", "-c", client1ConfigPath)
	client1Cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	client1Process, err := startFaultProcess(loadBalancer+" abrupt child", client1Cmd, timeline)
	if err != nil {
		t.Fatalf("failed to start client 1: %v", err)
	}
	defer func() {
		if err := client1Process.killAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop abrupt child: %v", err)
		}
	}()

	client2Config := newMTLSClientConfig(certDir, "client-2", localPort, 0, 0, quicPort)
	c2, err := client.New(client2Config)
	if err != nil {
		t.Fatalf("failed to create client 2: %v", err)
	}
	c2Run := startFaultClient(ctx, loadBalancer+" abrupt client 2", c2, timeline)
	defer func() {
		if err := c2Run.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop abrupt client 2: %v", err)
		}
	}()

	trafficAddress := fmt.Sprintf("127.0.0.1:%d", trafficPort)
	if err := waitForFault(ctx, 30*time.Second, func() string {
		return fmt.Sprintf("two TCP-eligible clients and echo; snapshot=%+v", serverRun.Snapshot())
	}, func(remaining time.Duration) bool {
		snapshot := serverRun.Snapshot()
		return len(snapshot.Routes) == 1 && snapshot.Routes[0].TCPEligibleClients == 2 && remaining > 0 &&
			probeSequencedTCP(trafficAddress, 1, min(250*time.Millisecond, remaining)) == nil
	}, serverRun.run, c2Run, client1Process.run); err != nil {
		t.Fatalf("abrupt baseline did not become usable: %v", err)
	}
	timeline.add("abrupt baseline: snapshot TCP eligible=2, echo 1 ok")

	collector := startProbeCollector(ctx, loadBalancer+" abrupt probes", trafficAddress, nil, 200*time.Millisecond, 250*time.Millisecond, timeline)
	defer func() {
		if err := collector.run.stopAndJoin(2 * time.Second); err != nil {
			t.Errorf("stop abrupt probe collector: %v", err)
		}
	}()
	if err := waitForFault(ctx, 2*time.Second, func() string {
		return fmt.Sprintf("a healthy pre-SIGKILL TCP sample; successes=%d", collector.tcpSuccesses.Load())
	}, func(time.Duration) bool {
		return collector.tcpSuccesses.Load() > 0
	}, serverRun.run, c2Run, client1Process.run, collector.run); err != nil {
		t.Fatalf("abrupt collector did not establish a healthy baseline: %v", err)
	}
	timeline.add("abrupt fault injection: SIGKILL child after healthy collector sample")
	if err := client1Process.killAndJoin(5 * time.Second); err != nil {
		t.Fatalf("SIGKILL abrupt child: %v", err)
	}
	detectionBound := serverConfig.HealthTimeout + 2*serverConfig.HeartbeatInterval
	if err := waitForFault(ctx, detectionBound, func() string {
		return fmt.Sprintf("eligible clients to converge 2->1; snapshot=%+v", serverRun.Snapshot())
	}, func(time.Duration) bool {
		snapshot := serverRun.Snapshot()
		return len(snapshot.Routes) == 1 && snapshot.Routes[0].TCPEligibleClients == 1
	}, serverRun.run, c2Run, collector.run); err != nil {
		t.Fatalf("abrupt disconnect did not converge within %s: %v", detectionBound, err)
	}
	timeline.add("abrupt detection: snapshot TCP eligible transitioned 2->1 within %s", detectionBound)
	if err := collector.run.stopAndJoin(2 * time.Second); err != nil {
		t.Fatalf("join abrupt probe collector: %v", err)
	}
	for sequence := uint64(10_000); sequence < 10_010; sequence++ {
		if err := probeSequencedTCP(trafficAddress, sequence, time.Second); err != nil {
			t.Fatalf("post-transition TCP probe %d: %v", sequence, err)
		}
	}
	timeline.add("abrupt recovery: 10 consecutive synchronous TCP echoes ok")
}

func openVerifiedTCPConnection(t *testing.T, trafficPort int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dial held TCP connection: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		_ = conn.Close()
		t.Fatalf("set held TCP connection deadline: %v", err)
	}

	testData := []byte("held TCP relay verification")
	if _, err := conn.Write(testData); err != nil {
		_ = conn.Close()
		t.Fatalf("write held TCP connection: %v", err)
	}
	buf := make([]byte, len(testData))
	if _, err := io.ReadFull(conn, buf); err != nil {
		_ = conn.Close()
		t.Fatalf("read held TCP connection: %v", err)
	}
	if string(buf) != string(testData) {
		_ = conn.Close()
		t.Fatalf("held TCP connection data mismatch: got %q, want %q", buf, testData)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		t.Fatalf("clear held TCP connection deadline: %v", err)
	}
	return conn
}

func assertConnectionClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set closed connection deadline: %v", err)
	}
	var buf [1]byte
	if _, err := conn.Read(buf[:]); err == nil {
		t.Fatal("held TCP connection remained usable after Server.Start returned")
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatalf("held TCP connection remained open after Server.Start returned: %v", err)
		}
	}
}

func startRestartEchoBackend(t *testing.T) int {
	t.Helper()
	loopback := net.ParseIP("127.0.0.1")

	const maxAttempts = 32
	var lastTCPBindErr error
	for range maxAttempts {
		udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: loopback})
		if err != nil {
			t.Fatalf("start local UDP echo server: %v", err)
		}
		port := udpConn.LocalAddr().(*net.UDPAddr).Port

		tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: loopback, Port: port})
		if err != nil {
			lastTCPBindErr = err
			if closeErr := udpConn.Close(); closeErr != nil {
				t.Fatalf("release unusable local UDP echo socket: %v (TCP bind: %v)", closeErr, err)
			}
			continue
		}

		reservedUDPPort := udpConn.LocalAddr().(*net.UDPAddr).Port
		reservedTCPPort := tcpListener.Addr().(*net.TCPAddr).Port
		if reservedUDPPort != port || reservedTCPPort != port {
			closeErr := errors.Join(udpConn.Close(), tcpListener.Close())
			t.Fatalf("invalid live local echo reservations: TCP=%d UDP=%d (close: %v)",
				reservedTCPPort, reservedUDPPort, closeErr)
		}

		t.Cleanup(func() {
			if err := errors.Join(tcpListener.Close(), udpConn.Close()); err != nil {
				t.Errorf("close local TCP/UDP echo sockets: %v", err)
			}
		})
		serveTCPEcho(tcpListener)
		serveUDPEcho(udpConn, false)
		return port
	}

	t.Fatalf("start TCP and UDP echo servers on one port after %d attempts: %v", maxAttempts, lastTCPBindErr)
	return 0
}

// reserveRestartServerPorts keeps all three sockets open until it has a
// distinct QUIC UDP port and one traffic port bindable by both TCP and UDP.
func reserveRestartServerPorts(t *testing.T) (quicPort, trafficPort int, release func() error) {
	t.Helper()
	loopback := net.ParseIP("127.0.0.1")
	quicReservation, err := net.ListenUDP("udp", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Fatalf("reserve QUIC UDP port: %v", err)
	}
	quicPort = quicReservation.LocalAddr().(*net.UDPAddr).Port

	const maxAttempts = 32
	var lastTCPBindErr error
	for range maxAttempts {
		// Pick the traffic number from the UDP namespace while the QUIC UDP
		// reservation is live. The kernel therefore cannot choose quicPort.
		trafficUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: loopback})
		if err != nil {
			closeErr := quicReservation.Close()
			t.Fatalf("reserve traffic UDP port: %v (close QUIC reservation: %v)", err, closeErr)
		}
		trafficPort = trafficUDP.LocalAddr().(*net.UDPAddr).Port
		if trafficPort == quicPort {
			if err := trafficUDP.Close(); err != nil {
				quicCloseErr := quicReservation.Close()
				t.Fatalf("release colliding traffic UDP reservation: %v (close QUIC reservation: %v)", err, quicCloseErr)
			}
			continue
		}

		trafficTCP, err := net.ListenTCP("tcp", &net.TCPAddr{IP: loopback, Port: trafficPort})
		if err != nil {
			lastTCPBindErr = err
			if closeErr := trafficUDP.Close(); closeErr != nil {
				quicCloseErr := quicReservation.Close()
				t.Fatalf("release unusable traffic UDP reservation: %v (TCP bind: %v; close QUIC reservation: %v)",
					closeErr, err, quicCloseErr)
			}
			continue
		}

		reservedTrafficUDPPort := trafficUDP.LocalAddr().(*net.UDPAddr).Port
		reservedTrafficTCPPort := trafficTCP.Addr().(*net.TCPAddr).Port
		if reservedTrafficUDPPort != trafficPort || reservedTrafficTCPPort != trafficPort || trafficPort == quicPort {
			closeErr := errors.Join(trafficUDP.Close(), trafficTCP.Close(), quicReservation.Close())
			t.Fatalf(
				"invalid live port reservations: QUIC=%d traffic TCP=%d traffic UDP=%d (close: %v)",
				quicPort, reservedTrafficTCPPort, reservedTrafficUDPPort, closeErr,
			)
		}

		return quicPort, trafficPort, func() error {
			return errors.Join(trafficUDP.Close(), trafficTCP.Close(), quicReservation.Close())
		}
	}

	if err := quicReservation.Close(); err != nil {
		t.Fatalf("find dual-protocol traffic port after %d attempts (last TCP bind: %v; close QUIC reservation: %v)",
			maxAttempts, lastTCPBindErr, err)
	}
	t.Fatalf("find dual-protocol traffic port after %d attempts: %v", maxAttempts, lastTCPBindErr)
	return 0, 0, nil
}

// TestServerRestartReconnect_MTLS tests that clients automatically reconnect
// after the server is stopped and restarted, and can successfully handle TCP
// and UDP requests through the same traffic address after reconnection.
func TestServerRestartReconnect_MTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	certDir := generateTestCertificates(t)
	localPort := startRestartEchoBackend(t)
	quicPort, trafficPort, releaseServerPorts := reserveRestartServerPorts(t)
	serverConfig := &config.Server{
		Listeners: []config.QuicListener{
			{
				QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
				TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
				Protocol:    "both",
			},
		},
		Auth: config.ServerAuth{
			Method:     "mtls",
			CACertFile: filepath.Join(certDir, "ca.crt"),
		},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
			SessionTicketEncryptionKeyRotationInterval: 24 * time.Hour,
			SessionTicketEncryptionKeyRotationOverlap:  new(uint8(2)),
		},
		HeartbeatInterval: 1 * time.Second,
		HealthTimeout:     3 * time.Second,
	}
	if err := releaseServerPorts(); err != nil {
		t.Fatalf("release restart server port reservations: %v", err)
	}

	timeline := newFaultTimeline(t, "server restart reconnect")
	currentServer := startFaultServer(t, ctx, "restart server generation 1", serverConfig, timeline)
	defer func() {
		if err := currentServer.run.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop current restart server: %v", err)
		}
	}()

	client1Config := &config.Client{
		ClientID: "client-1",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"},
			},
		},
		Local: config.LocalService{
			Host: "127.0.0.1",
			Port: localPort,
		},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		HeartbeatInterval: 1 * time.Second,
		HealthTimeout:     3 * time.Second,
	}
	c1, err := client.New(client1Config)
	if err != nil {
		t.Fatalf("failed to create client-1: %v", err)
	}
	c1Run := startFaultClient(ctx, "restart client 1", c1, timeline)
	defer func() {
		if err := c1Run.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop restart client 1: %v", err)
		}
	}()

	client2Config := &config.Client{
		ClientID: "client-2",
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{
				{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"},
			},
		},
		Local: config.LocalService{
			Host: "127.0.0.1",
			Port: localPort,
		},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		HeartbeatInterval: 1 * time.Second,
		HealthTimeout:     3 * time.Second,
	}
	c2, err := client.New(client2Config)
	if err != nil {
		t.Fatalf("failed to create client-2: %v", err)
	}
	c2Run := startFaultClient(ctx, "restart client 2", c2, timeline)
	defer func() {
		if err := c2Run.stopAndJoin(5 * time.Second); err != nil {
			t.Errorf("stop restart client 2: %v", err)
		}
	}()

	trafficAddress := fmt.Sprintf("127.0.0.1:%d", trafficPort)
	udpProbe, err := newSequencedUDPProbe(trafficAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udpProbe.Close() }()
	waitForTwoEligible := func(serverRun *faultServerRun, runs ...*faultRun) {
		t.Helper()
		if err := waitForFault(ctx, 20*time.Second, func() string {
			return fmt.Sprintf("two TCP/UDP eligible clients; snapshot=%+v", serverRun.Snapshot())
		}, func(time.Duration) bool {
			snapshot := serverRun.Snapshot()
			return len(snapshot.Routes) == 1 && snapshot.Routes[0].TCPEligibleClients == 2 &&
				snapshot.Routes[0].UDPEligibleClients == 2
		}, runs...); err != nil {
			t.Fatalf("restart generation did not become ready: %v", err)
		}
	}
	waitForTwoEligible(currentServer, currentServer.run, c1Run, c2Run)
	if err := probeSequencedTCP(trafficAddress, 1, time.Second); err != nil {
		t.Fatalf("initial sequenced TCP probe: %v", err)
	}
	if err := probeSequencedUDPEventually(ctx, timeline, udpProbe, 2*time.Second, 250*time.Millisecond); err != nil {
		t.Fatalf("initial sequenced UDP probe: %v", err)
	}
	timeline.add("restart baseline: snapshot TCP/UDP eligible=2/2 and exact blackbox ok")

	heldTCPConn := openVerifiedTCPConnection(t, trafficPort)
	defer func() { _ = heldTCPConn.Close() }()
	collector := startProbeCollector(ctx, "restart continuous probes", trafficAddress, udpProbe, 40*time.Millisecond, 150*time.Millisecond, timeline)
	defer func() {
		if err := collector.run.stopAndJoin(2 * time.Second); err != nil {
			t.Errorf("stop restart probe collector: %v", err)
		}
	}()
	if err := waitForFault(ctx, 2*time.Second, func() string {
		return fmt.Sprintf("healthy continuous TCP/UDP samples; TCP=%d UDP=%d", collector.tcpSuccesses.Load(), collector.udpSuccesses.Load())
	}, func(time.Duration) bool {
		return collector.tcpSuccesses.Load() > 0 && collector.udpSuccesses.Load() > 0
	}, currentServer.run, c1Run, c2Run, collector.run); err != nil {
		t.Fatalf("continuous probes did not establish a healthy baseline: %v", err)
	}
	timeline.add("restart baseline: continuous TCP/UDP collector samples ok")

	failuresBeforeStop := collector.tcpFailures.Load()
	timeline.add("restart fault injection: cancel server generation 1")
	currentServer.run.cancel()
	if err := currentServer.run.join(5 * time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("join restart server generation 1: %v", err)
	}
	assertConnectionClosed(t, heldTCPConn)
	if err := waitForFault(ctx, 2*time.Second, func() string {
		return fmt.Sprintf("a TCP outage sample; failures=%d", collector.tcpFailures.Load()-failuresBeforeStop)
	}, func(time.Duration) bool {
		return collector.tcpFailures.Load() > failuresBeforeStop
	}, c1Run, c2Run, collector.run); err != nil {
		t.Fatalf("continuous probe missed the server outage: %v", err)
	}
	timeline.add("restart detection: held TCP closed and continuous TCP outage sample observed")

	currentServer = startFaultServer(t, ctx, "restart server generation 2", serverConfig, timeline)
	waitForTwoEligible(currentServer, currentServer.run, c1Run, c2Run, collector.run)
	timeline.add("restart recovery candidate: fresh snapshot TCP/UDP eligible=2/2")
	if err := collector.run.stopAndJoin(2 * time.Second); err != nil {
		t.Fatalf("join restart probe collector: %v", err)
	}
	if collector.tcpFailures.Load() <= failuresBeforeStop {
		t.Fatal("continuous probe recorded no TCP failure during outage")
	}
	for sequence := uint64(10_000); sequence < 10_010; sequence++ {
		if err := probeSequencedTCP(trafficAddress, sequence, time.Second); err != nil {
			t.Fatalf("post-restart TCP probe %d: %v", sequence, err)
		}
	}
	if err := probeSequencedUDPEventually(ctx, timeline, udpProbe, 2*time.Second, 250*time.Millisecond); err != nil {
		t.Fatalf("post-restart UDP probe: %v", err)
	}
	timeline.add("restart recovery: 10 consecutive TCP echoes and exact UDP echo ok")
}
