package mesh

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
	certgen "github.com/Mmx233/QMux/cmd/generate/certs"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/internal/outbound"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
)

const meshTestToken = "0123456789abcdef0123456789abcdef"

type meshTestMaterial struct {
	ca         *x509.Certificate
	serverCert *x509.Certificate
	serverKey  *rsa.PrivateKey
	clientCert *x509.Certificate
	clientKey  *rsa.PrivateKey
	err        error
}

type meshTestFiles struct {
	ca, serverCert, serverKey, clientCert, clientKey string
}

var (
	meshTestMaterialOnce sync.Once
	meshTestMaterialData meshTestMaterial
)

func testMeshMaterial(t *testing.T) meshTestFiles {
	t.Helper()
	meshTestMaterialOnce.Do(func() {
		caKey, caCert, err := certgen.GenerateCA(1)
		if err != nil {
			meshTestMaterialData.err = err
			return
		}
		serverKey, serverCert, err := certgen.GenerateServerCert(caKey, caCert, 1, []string{"localhost"})
		if err != nil {
			meshTestMaterialData.err = err
			return
		}
		clientKey, clientCert, err := certgen.GenerateClientCert(caKey, caCert, 1)
		if err != nil {
			meshTestMaterialData.err = err
			return
		}
		meshTestMaterialData = meshTestMaterial{
			ca:         caCert,
			serverCert: serverCert,
			serverKey:  serverKey,
			clientCert: clientCert,
			clientKey:  clientKey,
		}
	})
	if meshTestMaterialData.err != nil {
		t.Fatal(meshTestMaterialData.err)
	}
	directory := t.TempDir()
	write := func(name string, contents []byte, mode os.FileMode) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, contents, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return meshTestFiles{
		ca:         write("ca.crt", certgen.EncodeCertificate(meshTestMaterialData.ca), 0o600),
		serverCert: write("server.crt", certgen.EncodeCertificate(meshTestMaterialData.serverCert), 0o600),
		serverKey:  write("server.key", certgen.EncodePrivateKey(meshTestMaterialData.serverKey), 0o600),
		clientCert: write("client.crt", certgen.EncodeCertificate(meshTestMaterialData.clientCert), 0o600),
		clientKey:  write("client.key", certgen.EncodePrivateKey(meshTestMaterialData.clientKey), 0o600),
	}
}

func reserveMeshUDPAddress(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	address := conn.LocalAddr().String()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func testMeshServerConfig(
	id, address, authMethod string,
	files meshTestFiles,
	peers []config.MeshPeer,
	maxPeers int,
) *config.MeshServer {
	serverAuth := config.ServerAuth{Method: authMethod, Token: meshTestToken}
	peerAuth := config.ClientAuth{Method: authMethod, Token: meshTestToken}
	peerTLS := config.ClientTLS{CACertFile: files.ca}
	if authMethod == config.ClientAuthMethodMTLS {
		serverAuth.CACertFile = files.ca
		serverAuth.Token = ""
		peerAuth.Token = ""
		peerTLS.ClientCertFile = files.clientCert
		peerTLS.ClientKeyFile = files.clientKey
	}
	return &config.MeshServer{
		ServerID: id,
		Tunnel: config.MeshServerTunnel{
			Listen: config.MeshTunnelListen{
				Address: address,
				Auth:    serverAuth,
				TLS: config.ServerTLS{
					ServerCertFile: files.serverCert,
					ServerKeyFile:  files.serverKey,
				},
			},
			Peering:           config.MeshPeering{Peers: peers, Auth: peerAuth, TLS: peerTLS},
			HeartbeatInterval: 20 * time.Millisecond,
			HealthTimeout:     500 * time.Millisecond,
		},
		Limits: config.MeshServerLimits{MaxPeers: maxPeers},
	}
}

func testMeshClientConfig(
	instanceID, groupID, authMethod string,
	files meshTestFiles,
	endpoints []config.MeshServerEndpoint,
) *config.MeshClient {
	auth := config.ClientAuth{Method: authMethod, Token: meshTestToken}
	tlsConfig := config.ClientTLS{CACertFile: files.ca}
	if authMethod == config.ClientAuthMethodMTLS {
		auth.Token = ""
		tlsConfig.ClientCertFile = files.clientCert
		tlsConfig.ClientKeyFile = files.clientKey
	}
	return &config.MeshClient{
		InstanceID: instanceID,
		Tunnel: config.MeshClientTunnel{
			Servers:           endpoints,
			Auth:              auth,
			TLS:               tlsConfig,
			HeartbeatInterval: 20 * time.Millisecond,
			HealthTimeout:     500 * time.Millisecond,
		},
		Local: config.LocalService{Host: "127.0.0.1", Port: 8080},
		Group: config.MeshGroup{GroupID: groupID, RuleVersion: 1},
	}
}

func startMeshServer(t *testing.T, server *Server) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- server.Start(context.Background()) }()
	select {
	case <-server.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("mesh server did not become ready")
	}
	if server.Address() == "" {
		select {
		case err := <-done:
			t.Fatalf("mesh server startup failed: %v", err)
		default:
			t.Fatal("mesh server ready closed without a listener")
		}
	}
	t.Cleanup(func() {
		_ = server.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("mesh server returned: %v", err)
			}
		default:
		}
	})
	return done
}

func startMeshClient(t *testing.T, client *Client) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- client.Start(context.Background()) }()
	select {
	case <-client.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("mesh client did not become ready")
	}
	t.Cleanup(func() {
		_ = client.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("mesh client returned: %v", err)
			}
		default:
		}
	})
	return done
}

func awaitMeshSession(t *testing.T, sessions <-chan *Session, event string) *Session {
	t.Helper()
	select {
	case session, ok := <-sessions:
		if !ok {
			t.Fatalf("session channel closed waiting for %s", event)
		}
		return session
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", event)
		return nil
	}
}

func awaitMeshCondition(t *testing.T, event string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", event)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func awaitMeshRegistryEmpty(t *testing.T, server *Server, event string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snapshot := server.Snapshot().Registry
		if snapshot.Pending == 0 && snapshot.ClientTotal == 0 && snapshot.PeerTotal == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", event, snapshot)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func stopMeshClient(t *testing.T, client *Client, done <-chan error) {
	t.Helper()
	if err := client.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("mesh client Start returned %v", err)
	}
}

func stopMeshServer(t *testing.T, server *Server, done <-chan error) {
	t.Helper()
	if err := server.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("mesh server Start returned %v", err)
	}
}

func TestMeshClientServerRealQUICAuthentication(t *testing.T) {
	for _, authMethod := range []string{config.ClientAuthMethodMTLS, config.ClientAuthMethodToken} {
		t.Run(authMethod, func(t *testing.T) {
			files := testMeshMaterial(t)
			address := reserveMeshUDPAddress(t)
			server, err := NewServer(testMeshServerConfig("edge-a", address, authMethod, files, nil, 1))
			if err != nil {
				t.Fatal(err)
			}
			server.stableGrace = 20 * time.Millisecond
			serverDone := startMeshServer(t, server)

			client, err := NewClient(testMeshClientConfig(
				"instance-a",
				"group-a",
				authMethod,
				files,
				[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: address, ServerName: "localhost"}},
			))
			if err != nil {
				t.Fatal(err)
			}
			client.stableGrace = 20 * time.Millisecond
			clientDone := startMeshClient(t, client)

			clientSession := awaitMeshSession(t, client.Sessions(), "client publication")
			serverSession := awaitMeshSession(t, server.Sessions(), "server publication")
			if clientSession.Role() != RoleClient || clientSession.Direction() != DirectionOutbound || clientSession.ServerID() != "edge-a" {
				t.Fatalf("client session identity = role %q direction %d server %q", clientSession.Role(), clientSession.Direction(), clientSession.ServerID())
			}
			if serverSession.Role() != RoleClient || serverSession.Direction() != DirectionInbound || serverSession.InstanceID() != "instance-a" || serverSession.GroupID() != "group-a" {
				t.Fatalf("server session identity = role %q direction %d instance %q group %q", serverSession.Role(), serverSession.Direction(), serverSession.InstanceID(), serverSession.GroupID())
			}
			awaitMeshCondition(t, "bidirectional mesh heartbeat", func() bool {
				return clientSession.ControlStarted() && serverSession.ControlStarted()
			})
			if snapshot := server.Snapshot(); snapshot.Registry.ClientCurrent != 1 || snapshot.PostAckCommitFailures != 0 {
				t.Fatalf("server snapshot = %+v", snapshot)
			}

			stopMeshClient(t, client, clientDone)
			stopMeshServer(t, server, serverDone)
			if snapshot := server.Snapshot(); snapshot.Registry.ClientTotal != 0 || snapshot.Registry.PeerTotal != 0 || snapshot.EndpointWorkers != 0 {
				t.Fatalf("server leaked state after Stop: %+v", snapshot)
			}
		})
	}
}

func TestMeshTokenModeOmitsConfiguredClientIdentity(t *testing.T) {
	files := testMeshMaterial(t)
	missing := t.TempDir()
	certFile := filepath.Join(missing, "missing-client.crt")
	keyFile := filepath.Join(missing, "missing-client.key")

	t.Run("client", func(t *testing.T) {
		cfg := testMeshClientConfig("instance-a", "group-a", config.ClientAuthMethodToken, files,
			[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: reserveMeshUDPAddress(t), ServerName: "localhost"}})
		cfg.Tunnel.TLS.ClientCertFile = certFile
		cfg.Tunnel.TLS.ClientKeyFile = keyFile
		cfg.Tunnel.TLS.AutoReload = true
		client, err := NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Stop() })
		if state := client.tlsState.Load(); state == nil || state.baseTLSConfig.RootCAs == nil || len(state.baseTLSConfig.Certificates) != 0 {
			t.Fatalf("token client TLS state = %+v", state)
		}
	})

	t.Run("outbound peer", func(t *testing.T) {
		cfg := testMeshServerConfig("edge-a", reserveMeshUDPAddress(t), config.ClientAuthMethodToken, files,
			[]config.MeshPeer{{ServerID: "edge-b", Address: reserveMeshUDPAddress(t), ServerName: "localhost"}}, 1)
		cfg.Tunnel.Peering.TLS.ClientCertFile = certFile
		cfg.Tunnel.Peering.TLS.ClientKeyFile = keyFile
		cfg.Tunnel.Peering.TLS.AutoReload = true
		server, err := NewServer(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Stop() })
		if state := server.outboundTLS.Load(); state == nil || state.baseTLSConfig.RootCAs == nil || len(state.baseTLSConfig.Certificates) != 0 {
			t.Fatalf("token peer TLS state = %+v", state)
		}
	})
}

func TestMeshPublishedSessionOutlivesAttemptDeadline(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	server, err := NewServer(testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1))
	if err != nil {
		t.Fatal(err)
	}
	serverDone := startMeshServer(t, server)
	client, err := NewClient(testMeshClientConfig(
		"instance-a", "group-a", config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: address, ServerName: "localhost"}},
	))
	if err != nil {
		t.Fatal(err)
	}
	client.attemptTimeout = 500 * time.Millisecond
	clientDone := startMeshClient(t, client)
	clientSession := awaitMeshSession(t, client.Sessions(), "client publication")
	awaitMeshSession(t, server.Sessions(), "server publication")

	timer := time.NewTimer(client.attemptTimeout + 200*time.Millisecond)
	defer timer.Stop()
	select {
	case <-clientSession.Done():
		t.Fatalf("published session retained attempt deadline: %v", clientSession.Err())
	case <-timer.C:
	}
	if snapshot := client.Snapshot(); !snapshot.Endpoints[0].Current {
		t.Fatalf("published session disappeared after attempt deadline: %+v", snapshot)
	}

	stopMeshClient(t, client, clientDone)
	stopMeshServer(t, server, serverDone)
}

func TestMeshClientStopUnblocksQUICHeartbeatWrite(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	serverConfig := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1)
	serverConfig.Tunnel.Quic.InitialStreamReceiveWindow = 1
	serverConfig.Tunnel.Quic.MaxStreamReceiveWindow = 1
	server, err := NewServer(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	server.sessions = make(chan *Session) // Hold delivery so the server does not read the control stream.
	serverDone := startMeshServer(t, server)
	client, err := NewClient(testMeshClientConfig(
		"instance-a", "group-a", config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: address, ServerName: "localhost"}},
	))
	if err != nil {
		t.Fatal(err)
	}
	client.config.Tunnel.HeartbeatInterval = 2 * time.Second
	client.config.Tunnel.HealthTimeout = 20 * time.Second
	clientDone := startMeshClient(t, client)
	session := awaitMeshSession(t, client.Sessions(), "client with unread server control")
	entered := make(chan struct{})
	returned := make(chan struct{})
	var frames atomic.Uint64
	write := func(stream io.Writer, timestamp int64) error {
		close(entered)
		defer close(returned)
		for range 100_000 {
			if err := protocol.WriteHeartbeat(stream, timestamp); err != nil {
				return err
			}
			frames.Add(1)
		}
		return errors.New("QUIC heartbeat writes did not block")
	}
	session.heartbeatWrite.Store(&write)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("client never entered its heartbeat write")
	}
	deadline := time.Now().Add(time.Second)
	for {
		before := frames.Load()
		select {
		case <-returned:
			t.Fatalf("heartbeat write returned before Stop after %d frames: %v", frames.Load(), session.Err())
		case <-time.After(100 * time.Millisecond):
		}
		if frames.Load() == before {
			select {
			case <-returned:
				t.Fatal("heartbeat write returned at the flow-control barrier")
			default:
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("QUIC heartbeat write never blocked after %d frames", frames.Load())
		}
	}
	stopped := make(chan error, 1)
	go func() { stopped <- client.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("client Stop waited for the heartbeat write deadline")
	}
	if err := <-clientDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		t.Fatal("client Stop left the control owner running")
	}
	if snapshot := client.Snapshot(); !snapshot.Closed || snapshot.EndpointWorkers != 0 || snapshot.Endpoints[0].Current {
		t.Fatalf("client Stop snapshot = %+v", snapshot)
	}
	stopMeshServer(t, server, serverDone)
}

func TestMeshStalledDeliveryDisconnectReleasesGeneration(t *testing.T) {
	t.Run("server inbound client", func(t *testing.T) {
		files := testMeshMaterial(t)
		address := reserveMeshUDPAddress(t)
		serverConfig := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1)
		serverConfig.Limits.MaxClientGenerations = 1
		server, err := NewServer(serverConfig)
		if err != nil {
			t.Fatal(err)
		}
		server.sessions = make(chan *Session)
		beforeDelivery := make(chan *Session, 2)
		server.beforeInboundDelivery = func(session *Session) { beforeDelivery <- session }
		serverDone := startMeshServer(t, server)
		registration := protocol.MeshRegister{
			Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
			TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "group-a",
		}
		conn, _, ack, err := rawMeshRegisterOpen(t, address, files, registration, false, nil)
		if err != nil || !ack.Success {
			t.Fatalf("initial registration ACK = %+v, %v", ack, err)
		}
		defer func() { _ = conn.CloseWithError(0, "test complete") }()
		first := awaitMeshSession(t, beforeDelivery, "first inbound session before blocked delivery")
		if first.ControlStarted() {
			t.Fatal("inbound control started before session delivery")
		}
		server.registry.mu.Lock()
		var firstToken *Generation
		if entry := server.registry.clients["instance-a"]; entry != nil {
			firstToken = entry.current.Load()
		}
		server.registry.mu.Unlock()
		if snapshot := server.Snapshot(); firstToken == nil || snapshot.Registry.ClientCurrent != 1 || snapshot.Registry.ClientTotal != 1 || snapshot.Registry.ClientBindings != 1 || snapshot.Registry.ClientHighWater != 1 {
			t.Fatalf("stalled inbound snapshot = %+v, token = %v", snapshot, firstToken)
		}
		_ = conn.CloseWithError(0, "disconnect stalled delivery")
		awaitMeshRegistryEmpty(t, server, "stalled inbound exact retirement")
		if snapshot := server.Snapshot(); firstToken.Phase() != PhaseDone || snapshot.Registry.ClientBindings != 0 || snapshot.Registry.ClientCurrent != 0 || snapshot.PostAckCommitFailures != 0 {
			t.Fatalf("inbound disconnect cleanup = %+v, first phase = %d", snapshot, firstToken.Phase())
		}

		registration.InstanceID = "instance-b"
		conn2, _, ack, err := rawMeshRegisterOpen(t, address, files, registration, false, nil)
		if err != nil || !ack.Success {
			t.Fatalf("recovered-capacity registration ACK = %+v, %v", ack, err)
		}
		defer func() { _ = conn2.CloseWithError(0, "test complete") }()
		second := awaitMeshSession(t, beforeDelivery, "replacement inbound session before blocked delivery")
		if second.ControlStarted() {
			t.Fatal("replacement inbound control started before session delivery")
		}
		server.registry.mu.Lock()
		var secondToken *Generation
		if entry := server.registry.clients["instance-b"]; entry != nil {
			secondToken = entry.current.Load()
		}
		server.registry.mu.Unlock()
		if snapshot := server.Snapshot(); secondToken == nil || secondToken == firstToken || snapshot.Registry.ClientCurrent != 1 || snapshot.Registry.ClientTotal != 1 || snapshot.Registry.ClientBindings != 1 || snapshot.Registry.ClientHighWater != 1 {
			t.Fatalf("recovered inbound capacity = %+v, second token = %v", snapshot, secondToken)
		}
		_ = conn2.CloseWithError(0, "test complete")
		awaitMeshRegistryEmpty(t, server, "replacement inbound cleanup")
		stopMeshServer(t, server, serverDone)
	})

	t.Run("client outbound", func(t *testing.T) {
		files := testMeshMaterial(t)
		address := reserveMeshUDPAddress(t)
		serverConfig := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1)
		serverConfig.Limits.MaxClientGenerations = 1
		serverConfig.Tunnel.HealthTimeout = 30 * time.Second
		server, err := NewServer(serverConfig)
		if err != nil {
			t.Fatal(err)
		}
		serverDone := startMeshServer(t, server)
		clientConfig := testMeshClientConfig("instance-a", "group-a", config.ClientAuthMethodToken, files,
			[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: address, ServerName: "localhost"}})
		clientConfig.Tunnel.HealthTimeout = 30 * time.Second
		client, err := NewClient(clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		client.sessions = make(chan *Session)
		reconnectEntered := make(chan struct{}, 1)
		releaseReconnect := make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseReconnect) })
		client.reconnectDelay = func(int) time.Duration {
			select {
			case reconnectEntered <- struct{}{}:
			default:
			}
			<-releaseReconnect
			return 0
		}
		clientDone := startMeshClient(t, client)
		t.Cleanup(release)
		awaitMeshCondition(t, "stalled client outbound publication", func() bool {
			return client.Snapshot().Endpoints[0].Current && server.Snapshot().Registry.ClientCurrent == 1
		})
		client.publishMu.Lock()
		first := client.endpoints[0].lifecycle.Load()
		client.publishMu.Unlock()
		if first == nil || first.ControlStarted() {
			t.Fatalf("client outbound control started before delivery: %+v", first)
		}
		serverSession := awaitMeshSession(t, server.Sessions(), "server for stalled client outbound")
		if snapshot := server.Snapshot(); snapshot.Registry.ClientTotal != 1 || snapshot.Registry.ClientBindings != 1 || snapshot.Registry.ClientHighWater != 1 {
			t.Fatalf("stalled client capacity = %+v", snapshot)
		}
		serverSession.Close()
		select {
		case <-reconnectEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("client remained blocked delivering a disconnected session")
		}
		awaitMeshRegistryEmpty(t, server, "stalled client server-side retirement")
		if snapshot := client.Snapshot(); snapshot.Endpoints[0].Current || first.ControlStarted() {
			t.Fatalf("stalled client exact retirement = %+v, control started = %v", snapshot, first.ControlStarted())
		}
		release()
		awaitMeshCondition(t, "client outbound capacity recovered", func() bool {
			return client.Snapshot().Endpoints[0].Current && server.Snapshot().Registry.ClientCurrent == 1
		})
		client.publishMu.Lock()
		second := client.endpoints[0].lifecycle.Load()
		client.publishMu.Unlock()
		if snapshot := server.Snapshot(); second == nil || second == first || second.ControlStarted() || snapshot.Registry.ClientTotal != 1 || snapshot.Registry.ClientBindings != 1 || snapshot.Registry.ClientHighWater != 1 {
			t.Fatalf("recovered client capacity = %+v, second session = %+v", snapshot, second)
		}
		stopMeshClient(t, client, clientDone)
		stopMeshServer(t, server, serverDone)
	})

	t.Run("server outbound peer", func(t *testing.T) {
		files := testMeshMaterial(t)
		acceptorAddress := reserveMeshUDPAddress(t)
		acceptorConfig := testMeshServerConfig("edge-b", acceptorAddress, config.ClientAuthMethodToken, files,
			[]config.MeshPeer{{ServerID: "edge-a"}}, 1)
		acceptorConfig.Tunnel.HealthTimeout = 30 * time.Second
		acceptor, err := NewServer(acceptorConfig)
		if err != nil {
			t.Fatal(err)
		}
		acceptorDone := startMeshServer(t, acceptor)
		dialerConfig := testMeshServerConfig("edge-a", reserveMeshUDPAddress(t), config.ClientAuthMethodToken, files,
			[]config.MeshPeer{{ServerID: "edge-b", Address: acceptorAddress, ServerName: "localhost"}}, 1)
		dialerConfig.Tunnel.HealthTimeout = 30 * time.Second
		dialer, err := NewServer(dialerConfig)
		if err != nil {
			t.Fatal(err)
		}
		dialer.sessions = make(chan *Session)
		reconnectEntered := make(chan struct{}, 1)
		releaseReconnect := make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseReconnect) })
		dialer.reconnectDelay = func(int) time.Duration {
			select {
			case reconnectEntered <- struct{}{}:
			default:
			}
			<-releaseReconnect
			return 0
		}
		dialerDone := startMeshServer(t, dialer)
		t.Cleanup(release)
		awaitMeshCondition(t, "stalled peer outbound publication", func() bool {
			return dialer.Snapshot().Registry.PeerCurrent == 1 && acceptor.Snapshot().Registry.PeerCurrent == 1
		})
		acceptorSession := awaitMeshSession(t, acceptor.Sessions(), "acceptor for stalled peer outbound")
		dialer.registry.mu.Lock()
		var firstToken *Generation
		if entry := dialer.registry.peers["edge-b"]; entry != nil {
			firstToken = entry.current.Load()
		}
		dialer.registry.mu.Unlock()
		if snapshot := dialer.Snapshot(); firstToken == nil || snapshot.Registry.PeerTotal != 1 || snapshot.Registry.PeerHighWater != 1 {
			t.Fatalf("stalled outbound peer capacity = %+v, token = %v", snapshot, firstToken)
		}
		acceptorSession.Close()
		select {
		case <-reconnectEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("outbound peer remained blocked delivering a disconnected session")
		}
		awaitMeshCondition(t, "stalled outbound peer exact retirement", func() bool {
			return dialer.Snapshot().Registry.PeerTotal == 0 && acceptor.Snapshot().Registry.PeerTotal == 0
		})
		if snapshot := dialer.Snapshot(); firstToken.Phase() != PhaseDone || snapshot.Registry.PeerCurrent != 0 || snapshot.Registry.PeerRetiring != 0 {
			t.Fatalf("outbound peer disconnect cleanup = %+v, first phase = %d", snapshot, firstToken.Phase())
		}
		release()
		awaitMeshCondition(t, "outbound peer capacity recovered", func() bool {
			return dialer.Snapshot().Registry.PeerCurrent == 1 && acceptor.Snapshot().Registry.PeerCurrent == 1
		})
		dialer.registry.mu.Lock()
		var secondToken *Generation
		if entry := dialer.registry.peers["edge-b"]; entry != nil {
			secondToken = entry.current.Load()
		}
		dialer.registry.mu.Unlock()
		if snapshot := dialer.Snapshot(); secondToken == nil || secondToken == firstToken || snapshot.Registry.PeerTotal != 1 || snapshot.Registry.PeerHighWater != 1 {
			t.Fatalf("recovered outbound peer capacity = %+v, token = %v", snapshot, secondToken)
		}
		stopMeshServer(t, dialer, dialerDone)
		stopMeshServer(t, acceptor, acceptorDone)
	})
}

func TestMeshInboundAuthenticatedRejectionsAndSilentAuthFailure(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	serverConfig := testMeshServerConfig(
		"edge-a",
		address,
		config.ClientAuthMethodToken,
		files,
		[]config.MeshPeer{{ServerID: "edge-b"}},
		1,
	)
	server, err := NewServer(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	serverDone := startMeshServer(t, server)

	tests := []struct {
		name         string
		registration protocol.MeshRegister
		silent       bool
		badProof     bool
	}{
		{
			name: "wrong target",
			registration: protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
				TargetServerID: "edge-z", InstanceID: "instance-a", GroupID: "group-a",
			},
		},
		{
			name: "missing capability",
			registration: protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Role: protocol.MeshRoleClient,
				TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "group-a",
			},
		},
		{
			name: "unconfigured peer",
			registration: protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRolePeer,
				TargetServerID: "edge-a", PeerServerID: "edge-c",
			},
		},
		{
			name: "self peer",
			registration: protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRolePeer,
				TargetServerID: "edge-a", PeerServerID: "edge-a",
			},
		},
		{
			name: "authentication failure is silent",
			registration: protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
				TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "group-a",
			},
			silent:   true,
			badProof: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ack, err := rawMeshRegister(t, address, files, test.registration, test.badProof)
			if test.silent {
				if err == nil {
					t.Fatalf("authentication failure returned diagnostic ACK %+v", ack)
				}
				return
			}
			if err != nil {
				t.Fatalf("read negative ACK: %v", err)
			}
			if ack.Success {
				t.Fatalf("invalid registration succeeded: %+v", ack)
			}
		})
	}
	awaitMeshRegistryEmpty(t, server, "rejected registrations cleanup")
	stopMeshServer(t, server, serverDone)
}

func TestMeshPeerNonPreferredDirectionRejected(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	unreachable := reserveMeshUDPAddress(t)
	server, err := NewServer(testMeshServerConfig(
		"edge-a",
		address,
		config.ClientAuthMethodToken,
		files,
		[]config.MeshPeer{{ServerID: "edge-z", Address: unreachable, ServerName: "localhost"}},
		1,
	))
	if err != nil {
		t.Fatal(err)
	}
	serverDone := startMeshServer(t, server)
	ack, err := rawMeshRegister(t, address, files, protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRolePeer,
		TargetServerID: "edge-a", PeerServerID: "edge-z",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Success || !strings.Contains(ack.Message, "non-preferred dial direction") {
		t.Fatalf("non-preferred peer acknowledgment = %+v", ack)
	}

	stopMeshServer(t, server, serverDone)
	if snapshot := server.Snapshot(); snapshot.Registry.Pending != 0 || snapshot.Registry.PeerTotal != 0 || snapshot.Registry.Arbitrations != 0 || snapshot.EndpointWorkers != 0 {
		t.Fatalf("non-preferred peer rejection leaked state: %+v", snapshot)
	}
}

func rawMeshRegister(
	t *testing.T,
	address string,
	files meshTestFiles,
	registration protocol.MeshRegister,
	badProof bool,
) (protocol.MeshRegisterAck, error) {
	t.Helper()
	conn, _, ack, err := rawMeshRegisterOpen(t, address, files, registration, badProof, nil)
	if conn != nil {
		_ = conn.CloseWithError(0, "test complete")
	}
	return ack, err
}

func rawMeshRegisterOpen(
	t *testing.T,
	address string,
	files meshTestFiles,
	registration protocol.MeshRegister,
	badProof bool,
	onDial func(*quic.Conn),
	beforeInitial ...func(*quic.Stream),
) (*quic.Conn, *quic.Stream, protocol.MeshRegisterAck, error) {
	return rawMeshRegisterOpenWithInitial(t, address, files, registration, badProof, onDial, nil, nil, beforeInitial...)
}

func rawMeshRegisterOpenWithInitial(
	t *testing.T,
	address string,
	files meshTestFiles,
	registration protocol.MeshRegister,
	badProof bool,
	onDial func(*quic.Conn),
	quicConfig *quic.Config,
	initial func(*quic.Stream) error,
	beforeInitial ...func(*quic.Stream),
) (*quic.Conn, *quic.Stream, protocol.MeshRegisterAck, error) {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(mustReadMeshFile(t, files.ca)) {
		t.Fatal("append mesh test CA")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if quicConfig == nil {
		quicConfig = config.Quic{}.GetConfig()
	}
	conn, err := quic.DialAddr(ctx, address, &tls.Config{
		RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		NextProtos: []string{meshALPN},
	}, quicConfig)
	if err != nil {
		return nil, nil, protocol.MeshRegisterAck{}, err
	}
	if onDial != nil {
		onDial(conn)
	}
	proof, err := sharedtoken.ComputeMesh([]byte(meshTestToken), sharedtoken.MeshTranscript{
		Version:        registration.Version,
		Capabilities:   registration.Capabilities,
		Role:           registration.Role,
		TargetServerID: registration.TargetServerID,
		PeerServerID:   registration.PeerServerID,
		InstanceID:     registration.InstanceID,
		GroupID:        registration.GroupID,
	}, conn.ConnectionState().TLS)
	if err != nil {
		return conn, nil, protocol.MeshRegisterAck{}, err
	}
	if badProof {
		proof[0] ^= 0xff
	}
	registration.Auth = &protocol.RegisterAuth{Scheme: sharedtoken.MeshScheme, Proof: proof}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return conn, nil, protocol.MeshRegisterAck{}, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	if err := protocol.WriteMeshRegister(stream, registration); err != nil {
		return conn, stream, protocol.MeshRegisterAck{}, err
	}
	if len(beforeInitial) > 0 {
		beforeInitial[0](stream)
	}
	if !badProof {
		if initial != nil {
			if err := initial(stream); err != nil {
				return conn, stream, protocol.MeshRegisterAck{}, err
			}
		} else if registration.Role == protocol.MeshRoleClient {
			clientConfig := testMeshClientConfig(registration.InstanceID, registration.GroupID, config.ClientAuthMethodToken, files,
				[]config.MeshServerEndpoint{{ServerID: registration.TargetServerID, Address: address, ServerName: "localhost"}})
			if err := config.FinalizeMeshClientConfig(clientConfig); err != nil {
				return conn, stream, protocol.MeshRegisterAck{}, err
			}
			if err := sendClientInitial(stream, clientConfig.Group.CanonicalBytes()); err != nil {
				return conn, stream, protocol.MeshRegisterAck{}, err
			}
		} else if registration.Role == protocol.MeshRolePeer {
			if err := sendPeerInitial(stream); err != nil {
				return conn, stream, protocol.MeshRegisterAck{}, err
			}
		}
		if registration.Role == protocol.MeshRolePeer {
			kind, payload, err := protocol.ReadMessageLimited(stream, protocol.MaxControlPayloadSize)
			if err != nil {
				return conn, stream, protocol.MeshRegisterAck{}, err
			}
			if kind == protocol.MsgTypeMeshRegisterAck {
				var ack protocol.MeshRegisterAck
				err := protocol.DecodeMessage(payload, &ack)
				return conn, stream, ack, err
			}
			begin, err := protocol.DecodeMeshControl(kind, payload)
			if err != nil {
				return conn, stream, protocol.MeshRegisterAck{}, err
			}
			if _, ok := begin.(protocol.MeshBegin); !ok {
				return conn, stream, protocol.MeshRegisterAck{}, fmt.Errorf("peer initial message = %T", begin)
			}
		peerInitial:
			for {
				message, err := protocol.ReadMeshControl(stream)
				if err != nil {
					return conn, stream, protocol.MeshRegisterAck{}, err
				}
				switch message.(type) {
				case protocol.MeshChunk, protocol.MeshPath:
				case protocol.MeshEnd:
					break peerInitial
				default:
					return conn, stream, protocol.MeshRegisterAck{}, fmt.Errorf("peer initial message = %T", message)
				}
			}
			if err := protocol.WriteMeshControl(stream, protocol.MeshReady{State: protocol.MeshStateStaged}); err != nil {
				return conn, stream, protocol.MeshRegisterAck{}, err
			}
		}
	}
	ack, err := protocol.ReadMeshRegisterAck(stream)
	return conn, stream, ack, err
}

func mustReadMeshFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestMeshPeerSingleSidedDialEitherIDOrder(t *testing.T) {
	for _, test := range []struct {
		name, dialerID, acceptorID string
	}{
		{name: "smaller initiates", dialerID: "edge-a", acceptorID: "edge-z"},
		{name: "larger initiates", dialerID: "edge-z", acceptorID: "edge-a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := testMeshMaterial(t)
			dialerAddress := reserveMeshUDPAddress(t)
			acceptorAddress := reserveMeshUDPAddress(t)
			acceptor, err := NewServer(testMeshServerConfig(
				test.acceptorID, acceptorAddress, config.ClientAuthMethodToken, files,
				[]config.MeshPeer{{ServerID: test.dialerID}}, 1,
			))
			if err != nil {
				t.Fatal(err)
			}
			acceptorDone := startMeshServer(t, acceptor)
			dialer, err := NewServer(testMeshServerConfig(
				test.dialerID, dialerAddress, config.ClientAuthMethodToken, files,
				[]config.MeshPeer{{ServerID: test.acceptorID, Address: acceptorAddress, ServerName: "localhost"}}, 1,
			))
			if err != nil {
				t.Fatal(err)
			}
			dialer.reconnectDelay = func(int) time.Duration { return 5 * time.Millisecond }
			dialerDone := startMeshServer(t, dialer)

			outboundSession := awaitMeshSession(t, dialer.Sessions(), "single-sided outbound peer")
			inboundSession := awaitMeshSession(t, acceptor.Sessions(), "single-sided inbound peer")
			if outboundSession.Direction() != DirectionOutbound || inboundSession.Direction() != DirectionInbound {
				t.Fatalf("peer directions = %d/%d", outboundSession.Direction(), inboundSession.Direction())
			}
			if dialer.Snapshot().Registry.PeerCurrent != 1 || acceptor.Snapshot().Registry.PeerCurrent != 1 {
				t.Fatalf("peer current snapshots = %+v / %+v", dialer.Snapshot(), acceptor.Snapshot())
			}

			stopMeshServer(t, dialer, dialerDone)
			stopMeshServer(t, acceptor, acceptorDone)
		})
	}
}

func TestMeshPeerDoubleDialMaxOneConvergesByStableID(t *testing.T) {
	for _, first := range []string{"edge-a", "edge-b"} {
		t.Run(first+" starts first", func(t *testing.T) {
			files := testMeshMaterial(t)
			addressA := reserveMeshUDPAddress(t)
			addressB := reserveMeshUDPAddress(t)
			serverA, err := NewServer(testMeshServerConfig(
				"edge-a", addressA, config.ClientAuthMethodToken, files,
				[]config.MeshPeer{{ServerID: "edge-b", Address: addressB, ServerName: "localhost"}}, 1,
			))
			if err != nil {
				t.Fatal(err)
			}
			serverB, err := NewServer(testMeshServerConfig(
				"edge-b", addressB, config.ClientAuthMethodToken, files,
				[]config.MeshPeer{{ServerID: "edge-a", Address: addressA, ServerName: "localhost"}}, 1,
			))
			if err != nil {
				t.Fatal(err)
			}
			serverA.reconnectDelay = func(int) time.Duration { return 5 * time.Millisecond }
			serverB.reconnectDelay = func(int) time.Duration { return 5 * time.Millisecond }
			prepared := make(chan string, 2)
			releaseA := make(chan struct{})
			releaseB := make(chan struct{})
			releaseReject := make(chan struct{})
			releaseAFn := sync.OnceFunc(func() { close(releaseA) })
			releaseBFn := sync.OnceFunc(func() { close(releaseB) })
			releaseRejectFn := sync.OnceFunc(func() { close(releaseReject) })
			t.Cleanup(func() {
				releaseAFn()
				releaseBFn()
				releaseRejectFn()
			})
			serverA.beforePeerDial = func(string) {
				prepared <- "edge-a"
				<-releaseA
			}
			serverB.beforePeerDial = func(string) {
				prepared <- "edge-b"
				<-releaseB
			}
			rejectReached := make(chan struct{}, 1)
			serverA.beforeReject = func(registration protocol.MeshRegister, err error) {
				if registration.Role == protocol.MeshRolePeer && strings.Contains(err.Error(), "non-preferred dial direction") {
					rejectReached <- struct{}{}
					<-releaseReject
				}
			}
			var doneA, doneB <-chan error
			if first == "edge-a" {
				doneA = startMeshServer(t, serverA)
				doneB = startMeshServer(t, serverB)
			} else {
				doneB = startMeshServer(t, serverB)
				doneA = startMeshServer(t, serverA)
			}
			seenPrepared := make(map[string]bool, 2)
			for range 2 {
				select {
				case serverID := <-prepared:
					seenPrepared[serverID] = true
				case <-time.After(10 * time.Second):
					t.Fatal("timed out waiting for both outbound prepared generations")
				}
			}
			if !seenPrepared["edge-a"] || !seenPrepared["edge-b"] {
				t.Fatalf("outbound prepared barrier reached by %v", seenPrepared)
			}
			for name, snapshot := range map[string]ServerSnapshot{"edge-a": serverA.Snapshot(), "edge-b": serverB.Snapshot()} {
				if snapshot.Registry.PeerPrepared != 1 || snapshot.Registry.PeerHighWater != 1 {
					t.Fatalf("%s pre-dial barrier snapshot = %+v", name, snapshot)
				}
			}

			// Let the non-preferred larger-ID dial reach edge-a first and hold its
			// rejection until the preferred edge-a -> edge-b direction commits.
			releaseBFn()
			select {
			case <-rejectReached:
			case <-time.After(10 * time.Second):
				t.Fatal("non-preferred dial did not reach the rejection barrier")
			}
			releaseAFn()
			awaitMeshCondition(t, "preferred double-dial direction commit", func() bool {
				return serverA.Snapshot().Registry.PeerCurrent == 1 && serverB.Snapshot().Registry.PeerCurrent == 1
			})
			releaseRejectFn()
			awaitMeshCondition(t, "double-dial loser cleanup", func() bool {
				return serverA.Snapshot().Registry.Pending == 0 && serverB.Snapshot().Registry.Arbitrations == 0
			})

			sessionA := awaitMeshSession(t, serverA.Sessions(), "double-dial winner at edge-a")
			sessionB := awaitMeshSession(t, serverB.Sessions(), "double-dial winner at edge-b")
			if sessionA.Direction() != DirectionOutbound || sessionB.Direction() != DirectionInbound {
				t.Fatalf("stable-ID winner directions = edge-a:%d edge-b:%d", sessionA.Direction(), sessionB.Direction())
			}
			awaitMeshCondition(t, "one peer current on each side", func() bool {
				return serverA.Snapshot().Registry.PeerCurrent == 1 && serverB.Snapshot().Registry.PeerCurrent == 1
			})
			for name, snapshot := range map[string]ServerSnapshot{"edge-a": serverA.Snapshot(), "edge-b": serverB.Snapshot()} {
				if snapshot.Registry.PeerHighWater > 1 || snapshot.Registry.PeerTotal != 1 || snapshot.Registry.Arbitrations != 0 {
					t.Fatalf("%s max_peers=1 snapshot = %+v", name, snapshot)
				}
			}

			stopDone := make(chan struct{}, 2)
			go func() { _ = serverA.Stop(); stopDone <- struct{}{} }()
			go func() { _ = serverB.Stop(); stopDone <- struct{}{} }()
			<-stopDone
			<-stopDone
			if err := <-doneA; err != nil {
				t.Fatal(err)
			}
			if err := <-doneB; err != nil {
				t.Fatal(err)
			}
			for name, snapshot := range map[string]ServerSnapshot{"edge-a": serverA.Snapshot(), "edge-b": serverB.Snapshot()} {
				if snapshot.Registry.Pending != 0 || snapshot.Registry.PeerTotal != 0 || snapshot.Registry.Arbitrations != 0 || snapshot.EndpointWorkers != 0 {
					t.Fatalf("%s leaked after concurrent Stop: %+v", name, snapshot)
				}
			}
		})
	}
}

func TestMeshPeerArbitrationTimeoutReturnsToBackoff(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	server, err := NewServer(testMeshServerConfig(
		"edge-b",
		address,
		config.ClientAuthMethodToken,
		files,
		[]config.MeshPeer{{ServerID: "edge-a", Address: reserveMeshUDPAddress(t), ServerName: "localhost"}},
		1,
	))
	if err != nil {
		t.Fatal(err)
	}
	server.registrationTimeout = 50 * time.Millisecond
	server.initialTimeout = 50 * time.Millisecond
	loserPrepared := make(chan struct{})
	releaseLoser := make(chan struct{})
	releaseLoserFn := sync.OnceFunc(func() { close(releaseLoser) })
	t.Cleanup(releaseLoserFn)
	var dialCalls atomic.Int32
	server.beforePeerDial = func(string) {
		if dialCalls.Add(1) == 1 {
			close(loserPrepared)
			<-releaseLoser
		}
	}
	backoff := make(chan int, 1)
	server.reconnectDelay = func(stage int) time.Duration {
		backoff <- stage
		return time.Hour
	}
	serverDone := startMeshServer(t, server)
	select {
	case <-loserPrepared:
	case <-time.After(10 * time.Second):
		t.Fatal("outbound loser did not reserve its prepared generation")
	}

	time.AfterFunc(3*server.registrationTimeout, releaseLoserFn)
	ack, registerErr := rawMeshRegister(t, address, files, protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRolePeer,
		TargetServerID: "edge-b", PeerServerID: "edge-a",
	}, false)
	if registerErr == nil && ack.Success {
		t.Fatalf("timed-out arbitration published a session: %+v", ack)
	}
	select {
	case stage := <-backoff:
		if stage != 0 {
			t.Fatalf("post-timeout reconnect stage = %d, want 0", stage)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("arbitration timeout did not return the endpoint worker to backoff")
	}
	awaitMeshCondition(t, "arbitration timeout cleanup", func() bool {
		snapshot := server.Snapshot().Registry
		return snapshot.Pending == 0 && snapshot.PeerTotal == 0 && snapshot.Arbitrations == 0
	})

	stopMeshServer(t, server, serverDone)
	if snapshot := server.Snapshot(); snapshot.Registry.Pending != 0 || snapshot.Registry.PeerTotal != 0 || snapshot.Registry.Arbitrations != 0 || snapshot.EndpointWorkers != 0 {
		t.Fatalf("arbitration timeout leaked state: %+v", snapshot)
	}
}

func TestMeshPeerArbitrationTransfersFirstInboundAttempt(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	server, err := NewServer(testMeshServerConfig(
		"edge-b", address, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-a", Address: reserveMeshUDPAddress(t), ServerName: "localhost"}}, 1,
	))
	if err != nil {
		t.Fatal(err)
	}
	loserPrepared := make(chan struct{})
	releaseLoser := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseLoser) })
	var dials atomic.Int32
	server.beforePeerDial = func(string) {
		if dials.Add(1) == 1 {
			close(loserPrepared)
			<-releaseLoser
		}
	}
	serverDone := startMeshServer(t, server)
	t.Cleanup(release)
	select {
	case <-loserPrepared:
	case <-time.After(10 * time.Second):
		t.Fatal("outbound loser did not reserve its peer slot")
	}

	type result struct {
		ack protocol.MeshRegisterAck
		err error
	}
	registered := make(chan result, 1)
	go func() {
		ack, err := rawMeshRegister(t, address, files, protocol.MeshRegister{
			Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRolePeer,
			TargetServerID: "edge-b", PeerServerID: "edge-a",
		}, false)
		registered <- result{ack, err}
	}()
	awaitMeshCondition(t, "preferred inbound arbitration", func() bool {
		snapshot := server.Snapshot().Registry
		return snapshot.Arbitrations == 1 && snapshot.Pending == 1 && snapshot.PeerTotal == 1
	})
	release()
	select {
	case result := <-registered:
		if result.err != nil || !result.ack.Success {
			t.Fatalf("first preferred inbound attempt = %+v, %v", result.ack, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("preferred inbound did not complete after loser cleanup")
	}
	if dials.Load() != 1 {
		t.Fatalf("outbound loser attempted %d dials, want one", dials.Load())
	}
	if snapshot := server.Snapshot(); snapshot.Registry.PeerHighWater != 1 || snapshot.PostAckCommitFailures != 0 {
		t.Fatalf("first inbound transfer snapshot = %+v", snapshot)
	}
	stopMeshServer(t, server, serverDone)
}

func TestMeshClientMultiEndpointPartialFailureRecovers(t *testing.T) {
	files := testMeshMaterial(t)
	addressA := reserveMeshUDPAddress(t)
	addressB := reserveMeshUDPAddress(t)
	serverA, err := NewServer(testMeshServerConfig("edge-a", addressA, config.ClientAuthMethodToken, files, nil, 1))
	if err != nil {
		t.Fatal(err)
	}
	doneA := startMeshServer(t, serverA)
	client, err := NewClient(testMeshClientConfig(
		"instance-a", "group-a", config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{
			{ServerID: "edge-a", Address: addressA, ServerName: "localhost"},
			{ServerID: "edge-b", Address: addressB, ServerName: "localhost"},
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	client.reconnectDelay = func(int) time.Duration { return 10 * time.Millisecond }
	client.attemptTimeout = 200 * time.Millisecond
	clientDone := startMeshClient(t, client)
	first := awaitMeshSession(t, client.Sessions(), "healthy endpoint while peer is absent")
	if first.ServerID() != "edge-a" {
		t.Fatalf("first endpoint = %q, want edge-a", first.ServerID())
	}
	awaitMeshCondition(t, "failed endpoint retry", func() bool {
		return client.Snapshot().Endpoints[1].ReconnectAttempts > 0
	})

	serverB, err := NewServer(testMeshServerConfig("edge-b", addressB, config.ClientAuthMethodToken, files, nil, 1))
	if err != nil {
		t.Fatal(err)
	}
	doneB := startMeshServer(t, serverB)
	second := awaitMeshSession(t, client.Sessions(), "recovered endpoint")
	if second.ServerID() != "edge-b" {
		t.Fatalf("recovered endpoint = %q, want edge-b", second.ServerID())
	}
	awaitMeshSession(t, serverB.Sessions(), "server B client generation")
	awaitMeshCondition(t, "both client endpoints current", func() bool {
		snapshot := client.Snapshot()
		return snapshot.Endpoints[0].Current && snapshot.Endpoints[1].Current
	})

	stopMeshClient(t, client, clientDone)
	stopMeshServer(t, serverB, doneB)
	stopMeshServer(t, serverA, doneA)
}

func TestMeshClientStableExactCurrentResetsRetryStage(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	server, err := NewServer(testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1))
	if err != nil {
		t.Fatal(err)
	}
	serverDone := startMeshServer(t, server)
	client, err := NewClient(testMeshClientConfig(
		"instance-a", "group-a", config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: address, ServerName: "localhost"}},
	))
	if err != nil {
		t.Fatal(err)
	}
	client.stableGrace = 20 * time.Millisecond
	client.endpoints[0].lifecycle.SetRetryStage(outbound.MaxReconnectStage)
	reconnectStages := make(chan int, 1)
	client.reconnectDelay = func(stage int) time.Duration {
		reconnectStages <- stage
		return time.Hour
	}
	clientDone := startMeshClient(t, client)
	clientSession := awaitMeshSession(t, client.Sessions(), "stable client generation")
	serverSession := awaitMeshSession(t, server.Sessions(), "stable server generation")
	awaitMeshCondition(t, "server heartbeat past stable grace", clientSession.ReconnectStable)
	serverSession.Close()

	select {
	case stage := <-reconnectStages:
		if stage != 0 {
			t.Fatalf("stable exact-current retry stage = %d, want 0", stage)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stable exact-current failure did not enter reconnect wait")
	}
	if snapshot := client.Snapshot(); snapshot.Endpoints[0].Current || snapshot.Endpoints[0].ReconnectStage != 0 {
		t.Fatalf("stable exact-current retirement snapshot = %+v", snapshot)
	}

	stopMeshClient(t, client, clientDone)
	stopMeshServer(t, server, serverDone)
}

func TestMeshServerConcurrentDuplicateClientKeepsOneCurrent(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	serverConfig := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1)
	serverConfig.Limits.MaxClientGenerations = 8
	server, err := NewServer(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	ackBlocked := make(chan struct{})
	releaseAck := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseAck) })
	server.beforeSuccessAck = func(registration protocol.MeshRegister, _ *quic.Conn) {
		if registration.Role == protocol.MeshRoleClient {
			close(ackBlocked)
			<-releaseAck
		}
	}
	serverDone := startMeshServer(t, server)
	t.Cleanup(release)

	const count = 8
	registration := protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
		TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "group-a",
	}
	type result struct {
		conn   *quic.Conn
		stream *quic.Stream
		ack    protocol.MeshRegisterAck
		err    error
	}
	results := make(chan result, count)
	for range count {
		go func() {
			conn, stream, ack, err := rawMeshRegisterOpen(t, address, files, registration, false, nil)
			if err != nil || !ack.Success {
				if conn != nil {
					_ = conn.CloseWithError(0, "rejected contender")
				}
				conn, stream = nil, nil
			}
			results <- result{conn, stream, ack, err}
		}()
	}
	select {
	case <-ackBlocked:
	case <-time.After(10 * time.Second):
		t.Fatal("no contender reached the prepared ACK barrier")
	}
	for range count - 1 {
		select {
		case contender := <-results:
			if contender.err != nil || contender.ack.Success || !strings.Contains(contender.ack.Message, "prepared") {
				t.Fatalf("prepared contender terminal ACK = %+v, %v", contender.ack, contender.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("contender did not return a terminal rejection")
		}
	}
	if snapshot := server.Snapshot(); snapshot.Registry.ClientPrepared != 1 || snapshot.Registry.ClientCurrent != 0 || snapshot.Registry.ClientBindings != 1 || snapshot.PostAckCommitFailures != 0 {
		t.Fatalf("held prepared claim snapshot = %+v", snapshot)
	}
	release()
	var winner result
	select {
	case winner = <-results:
		if winner.err != nil || !winner.ack.Success || winner.conn == nil {
			t.Fatalf("unique contender terminal ACK = %+v, %v", winner.ack, winner.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("winner did not return a success ACK")
	}
	defer winner.conn.CloseWithError(0, "test complete")
	if err := winner.stream.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			msgType, _, err := protocol.ReadMessageLimited(winner.stream, protocol.MaxControlPayloadSize)
			if err != nil {
				return
			}
			if msgType == protocol.MsgTypeHeartbeat {
				if protocol.WriteHeartbeat(winner.stream, time.Now().Unix()) != nil {
					return
				}
			}
		}
	}()
	first := awaitMeshSession(t, server.Sessions(), "one accepted duplicate client")
	ack, err := rawMeshRegister(t, address, files, registration, false)
	if err != nil || ack.Success || !strings.Contains(ack.Message, "current") {
		t.Fatalf("healthy-current sequential duplicate ACK = %+v, %v", ack, err)
	}
	awaitMeshCondition(t, "sequential duplicate pending cleanup", func() bool {
		return server.Snapshot().Registry.Pending == 0
	})
	select {
	case duplicate := <-server.Sessions():
		t.Fatalf("duplicate client received a success ACK: %+v", duplicate)
	default:
	}
	select {
	case <-first.Done():
		t.Fatal("duplicate registration evicted the healthy current")
	default:
	}
	if snapshot := server.Snapshot(); snapshot.Registry.Pending != 0 || snapshot.Registry.ClientCurrent != 1 || snapshot.Registry.ClientPrepared != 0 || snapshot.Registry.ClientTotal != 1 || snapshot.Registry.ClientBindings != 1 || snapshot.PostAckCommitFailures != 0 {
		t.Fatalf("duplicate client snapshot = %+v", snapshot)
	}

	_ = winner.conn.CloseWithError(0, "test complete")
	awaitMeshRegistryEmpty(t, server, "duplicate client cleanup")
	stopMeshServer(t, server, serverDone)
	if snapshot := server.Snapshot(); snapshot.Registry.ClientTotal != 0 || snapshot.Registry.ClientBindings != 0 {
		t.Fatalf("duplicate client cleanup snapshot = %+v", snapshot)
	}
}

func TestMeshReceiverSuccessAckCancelAndStopBarriers(t *testing.T) {
	for _, test := range []struct {
		name  string
		after bool
		stop  bool
	}{
		{name: "cancel before ACK"},
		{name: "Stop before ACK", stop: true},
		{name: "cancel after ACK", after: true},
		{name: "Stop after ACK", after: true, stop: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := testMeshMaterial(t)
			address := reserveMeshUDPAddress(t)
			server, err := NewServer(testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1))
			if err != nil {
				t.Fatal(err)
			}
			declaration := testDeclaration(t, "group-a", 1, 0)
			checkGroup := func(event string, wantRevision uint64, wantPublished bool, wantGroups int) {
				t.Helper()
				server.controlState.mu.Lock()
				current := server.controlState.groups["group-a"]
				revision := server.controlState.revision
				server.controlState.mu.Unlock()
				groups, size := server.declarations.snapshot()
				wantBytes := int64(0)
				if wantGroups != 0 {
					wantBytes = int64(len(declaration))
				}
				if revision != wantRevision || (current != nil) != wantPublished || groups != wantGroups || size != wantBytes {
					t.Fatalf("%s: revision %d, published %t, ledger %d/%d", event, revision, current != nil, groups, size)
				}
				if current != nil && !bytes.Equal(current.bytes, declaration) {
					t.Fatalf("%s: published declaration changed", event)
				}
			}
			reached := make(chan *quic.Conn, 1)
			releaseAck := make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseAck) })
			barrier := func(_ protocol.MeshRegister, receivingConn *quic.Conn) {
				reached <- receivingConn
				<-releaseAck
			}
			if test.after {
				server.afterSuccessAck = barrier
			} else {
				server.beforeSuccessAck = barrier
			}
			serverDone := startMeshServer(t, server)
			t.Cleanup(release)
			dialed := make(chan *quic.Conn, 1)
			type result struct {
				conn *quic.Conn
				ack  protocol.MeshRegisterAck
				err  error
			}
			registered := make(chan result, 1)
			go func() {
				conn, _, ack, err := rawMeshRegisterOpen(t, address, files, protocol.MeshRegister{
					Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
					TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "group-a",
				}, false, func(conn *quic.Conn) { dialed <- conn })
				registered <- result{conn, ack, err}
			}()
			var conn *quic.Conn
			select {
			case conn = <-dialed:
			case <-time.After(10 * time.Second):
				t.Fatal("registration did not dial")
			}
			var receivingConn *quic.Conn
			select {
			case receivingConn = <-reached:
			case <-time.After(10 * time.Second):
				t.Fatal("registration did not reach the ACK barrier")
			}
			if snapshot := server.Snapshot(); snapshot.Registry.ClientPrepared != 1 || snapshot.Registry.ClientCurrent != 0 || snapshot.Registry.ClientTotal != 1 || snapshot.Registry.ClientBindings != 1 || snapshot.PostAckCommitFailures != 0 {
				t.Fatalf("held ACK boundary snapshot = %+v", snapshot)
			}
			checkGroup("held ACK boundary", 0, false, 1)
			select {
			case session := <-server.Sessions():
				t.Fatalf("prepared session was published before commit: %+v", session)
			default:
			}
			if test.after {
				select {
				case response := <-registered:
					if response.err != nil || !response.ack.Success {
						t.Fatalf("post-ACK response = %+v, %v", response.ack, response.err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("success ACK did not reach the caller")
				}
			}
			var stopped chan error
			if test.stop {
				stopped = make(chan error, 1)
				go func() { stopped <- server.Stop() }()
				awaitMeshCondition(t, "registry Stop gate", func() bool { return server.Snapshot().Registry.Closed })
			} else {
				_ = conn.CloseWithError(0, "cancel registration")
				select {
				case <-receivingConn.Context().Done():
				case <-time.After(10 * time.Second):
					t.Fatal("receiver did not observe canceled registration")
				}
			}
			checkGroup("canceled ACK boundary", 0, false, 1)
			release()
			if !test.after {
				select {
				case response := <-registered:
					if response.err == nil && response.ack.Success {
						t.Fatalf("pre-ACK canceled registration succeeded: %+v", response.ack)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("registration did not return after ACK barrier")
				}
			}
			_ = conn.CloseWithError(0, "test complete")
			if test.stop {
				select {
				case err := <-stopped:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("Stop remained blocked at ACK boundary")
				}
				if err := <-serverDone; err != nil {
					t.Fatal(err)
				}
			} else {
				awaitMeshRegistryEmpty(t, server, "canceled registration cleanup")
				wantGroups := 0
				if test.after {
					wantGroups = 1
				}
				awaitMeshCondition(t, "ACK group ownership settled", func() bool {
					groups, _ := server.declarations.snapshot()
					return groups == wantGroups
				})
				checkGroup("canceled registration settled", uint64(wantGroups), test.after, wantGroups)
				stopMeshServer(t, server, serverDone)
			}
			wantRevision := uint64(0)
			if test.after {
				wantRevision = 1
			}
			checkGroup("after Stop", wantRevision, false, 0)
			if snapshot := server.Snapshot(); snapshot.Registry.Pending != 0 || snapshot.Registry.ClientTotal != 0 || snapshot.Registry.ClientBindings != 0 || snapshot.Registry.ClientPrepared != 0 || snapshot.PostAckCommitFailures != 0 {
				t.Fatalf("ACK boundary cleanup snapshot = %+v", snapshot)
			}
		})
	}
}

func TestMeshServerClientCapacityRejectsOnlyNewTransaction(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	serverConfig := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1)
	serverConfig.Limits.MaxClientGenerations = 1
	server, err := NewServer(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	serverDone := startMeshServer(t, server)
	client, err := NewClient(testMeshClientConfig(
		"instance-a", "group-a", config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: address, ServerName: "localhost"}},
	))
	if err != nil {
		t.Fatal(err)
	}
	clientDone := startMeshClient(t, client)
	clientSession := awaitMeshSession(t, client.Sessions(), "capacity incumbent client")
	awaitMeshSession(t, server.Sessions(), "capacity incumbent server")

	ack, err := rawMeshRegister(t, address, files, protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
		TargetServerID: "edge-a", InstanceID: "instance-b", GroupID: "group-b",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Success || !strings.Contains(ack.Message, "capacity") {
		t.Fatalf("client capacity acknowledgment = %+v", ack)
	}
	select {
	case <-clientSession.Done():
		t.Fatal("capacity rejection evicted the incumbent client")
	default:
	}
	if snapshot := server.Snapshot(); snapshot.Registry.ClientCurrent != 1 || snapshot.Registry.ClientTotal != 1 || snapshot.Registry.ClientHighWater != 1 || snapshot.PostAckCommitFailures != 0 {
		t.Fatalf("client capacity rejection snapshot = %+v", snapshot)
	}

	stopMeshClient(t, client, clientDone)
	stopMeshServer(t, server, serverDone)
	if snapshot := server.Snapshot(); snapshot.Registry.ClientTotal != 0 || snapshot.Registry.ClientBindings != 0 || snapshot.EndpointWorkers != 0 {
		t.Fatalf("client capacity cleanup snapshot = %+v", snapshot)
	}
}

func TestMeshStopBeforeStartIsIdempotent(t *testing.T) {
	files := testMeshMaterial(t)
	server, err := NewServer(testMeshServerConfig("edge-a", reserveMeshUDPAddress(t), config.ClientAuthMethodToken, files, nil, 1))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(testMeshClientConfig(
		"instance-a", "group-a", config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: reserveMeshUDPAddress(t), ServerName: "localhost"}},
	))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := server.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := client.Stop(); err != nil {
			t.Fatal(err)
		}
	}
	if !server.Snapshot().Registry.Closed || !client.Snapshot().Closed {
		t.Fatal("Stop before Start did not close publication gates")
	}
}

func TestMeshServerStartFailureClosesOwnedResources(t *testing.T) {
	files := testMeshMaterial(t)
	peerCA := filepath.Join(t.TempDir(), "peer-ca.crt")
	if err := os.WriteFile(peerCA, mustReadMeshFile(t, files.ca), 0o600); err != nil {
		t.Fatal(err)
	}
	serverConfig := testMeshServerConfig(
		"edge-a",
		"127.0.0.1:2",
		config.ClientAuthMethodToken,
		files,
		[]config.MeshPeer{{ServerID: "edge-b", Address: "127.0.0.1:1", ServerName: "localhost"}},
		1,
	)
	serverConfig.Tunnel.Peering.TLS.CACertFile = peerCA
	server, err := NewServer(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(peerCA); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("mesh server Start succeeded after outbound TLS material was removed")
	}
	if snapshot := server.Snapshot(); !snapshot.Registry.Closed || snapshot.Registry.Pending != 0 || snapshot.Registry.PeerTotal != 0 || snapshot.EndpointWorkers != 0 {
		t.Fatalf("mesh server leaked after startup failure: %+v", snapshot)
	}

	waits := make(chan error, 2)
	go func() { waits <- server.inboundReloader.Wait() }()
	go func() { waits <- server.outboundReloader.Wait() }()
	for range 2 {
		select {
		case err := <-waits:
			if err != nil {
				t.Fatalf("TLS reloader returned after startup failure: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("TLS reloader remained active after startup failure")
		}
	}
	if err := server.Stop(); err != nil {
		t.Fatal(err)
	}
}
