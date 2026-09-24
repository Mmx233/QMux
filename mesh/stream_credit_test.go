package mesh

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/quic-go/quic-go"
)

func TestMeshRealQUICStreamCreditAcrossSources(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	serverConfig := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 2)
	capacity := config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 1, MaxTCPConnectionsPerGeneration: 1, MaxPendingTCPSetupsPerGeneration: 1}
	serverConfig.Ingress.Listeners = []config.MeshIngressListener{
		{Address: "127.0.0.1:18080", Protocol: config.MeshIngressProtocolHTTP, Capacity: capacity},
		{Address: "127.0.0.1:18081", Protocol: config.MeshIngressProtocolHTTP, Capacity: capacity},
	}
	server, err := NewServer(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	startMeshServer(t, server)

	sessions := make(map[string]*Session)
	for _, instance := range []string{"a", "b"} {
		clientConfig := testMeshClientConfig(instance, "api", config.ClientAuthMethodToken, files,
			[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: address, ServerName: "localhost"}})
		clientConfig.Tunnel.Quic.MaxIncomingStreams = 1
		client, err := NewClient(clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		startMeshClient(t, client)
		sessions[instance] = awaitMeshSession(t, client.Sessions(), instance+" client session")
	}
	serverSessions := make(map[string]*Session)
	for range 2 {
		session := awaitMeshSession(t, server.Sessions(), "server client session")
		serverSessions[session.InstanceID()] = session
	}
	server.registry.mu.Lock()
	a := server.registry.clients["a"].current.Load()
	b := server.registry.clients["b"].current.Load()
	server.registry.mu.Unlock()
	if !server.registry.PublishForwarding(a, readyForwarding) {
		t.Fatal("publish A")
	}

	firstSource, secondSource := server.ingressSources[0], server.ingressSources[1]
	firstFlow, err := firstSource.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	defer firstFlow.release()
	firstSetup, err := firstFlow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	defer firstSetup.release()
	firstLease, err := server.registry.beginClientSelection("api", firstSetup, "least-connections").next()
	if err != nil || firstLease.Generation() != a {
		t.Fatalf("first source selected A = (%v, %v)", firstLease, err)
	}
	defer firstLease.release()
	serverSessionA := serverSessions["a"]
	serverSessionB := serverSessions["b"]
	if serverSessionA == nil || serverSessionB == nil {
		t.Fatalf("server sessions = %v", serverSessions)
	}
	firstStream, err := serverSessionA.Connection().OpenStream()
	if err != nil {
		t.Fatalf("A first physical stream: %v", err)
	}
	defer firstStream.CancelRead(meshStreamError)
	defer firstStream.CancelWrite(meshStreamError)
	secondFlow, err := secondSource.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	defer secondFlow.release()
	secondSetup, err := secondFlow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	defer secondSetup.release()
	_, err = server.registry.beginClientSelection("api", secondSetup, "least-connections").open(context.Background(), func(g *Generation) (func(), error) {
		if g != a {
			t.Fatalf("only A should be ready, got %v", g)
		}
		_, openErr := serverSessionA.Connection().OpenStream()
		return nil, openErr
	})
	if !errors.Is(err, ErrLocalTransportBudget) || !server.registry.GroupAvailability("api") {
		t.Fatalf("single saturated transport = %v, group available %t", err, server.registry.GroupAvailability("api"))
	}
	if got := secondSource.snapshot(); got.Flows != 1 || got.Setups != 1 {
		t.Fatalf("local transport rejection lost source lease = %+v", got)
	}
	if !server.registry.PublishForwarding(b, readyForwarding) {
		t.Fatal("publish B")
	}
	selection := server.registry.beginClientSelection("api", secondSetup, "round-robin")
	streamLimited := false
	var secondStream *quic.Stream
	secondLease, err := selection.open(context.Background(), func(g *Generation) (func(), error) {
		var conn *quic.Conn
		if g == a {
			conn = serverSessionA.Connection()
		} else if g == b {
			conn = serverSessionB.Connection()
		} else {
			t.Fatalf("unexpected candidate %v", g)
		}
		stream, openErr := conn.OpenStream()
		if openErr != nil {
			var limitErr *quic.StreamLimitReachedError
			streamLimited = streamLimited || errors.As(openErr, &limitErr)
			return nil, openErr
		}
		secondStream = stream
		return func() {
			stream.CancelRead(meshStreamError)
			stream.CancelWrite(meshStreamError)
		}, nil
	})
	if err != nil || !streamLimited || secondLease.Generation() != b {
		t.Fatalf("physical credit retry = lease %v, err %v, limited %t", secondLease, err, streamLimited)
	}
	defer secondLease.release()
	defer secondStream.CancelRead(meshStreamError)
	defer secondStream.CancelWrite(meshStreamError)
	if got := firstSource.snapshot(); got.Flows != 1 || got.Setups != 1 {
		t.Fatalf("first source budget changed = %+v", got)
	}
	if got := secondSource.snapshot(); got.Flows != 1 || got.Setups != 1 {
		t.Fatalf("retry borrowed another source budget = %+v", got)
	}
	if got := server.registry.trafficSnapshot(a, secondSource); got.Pending != 0 || got.Active != 0 {
		t.Fatalf("failed A reservation retained = %+v", got)
	}

	if _, err := firstStream.Write([]byte("x")); err != nil {
		t.Fatalf("send first stream byte: %v", err)
	}
	if err := firstStream.Close(); err != nil {
		t.Fatalf("close first stream write: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted, err := sessions["a"].Connection().AcceptStream(ctx)
	if err != nil {
		t.Fatalf("accept A physical stream: %v", err)
	}
	if data, err := io.ReadAll(accepted); err != nil || string(data) != "x" {
		t.Fatalf("read A physical stream = %q, %v", data, err)
	}
	if err := accepted.Close(); err != nil {
		t.Fatalf("close accepted stream write: %v", err)
	}
	if _, err := io.Copy(io.Discard, firstStream); err != nil {
		t.Fatalf("drain first stream reverse direction: %v", err)
	}
	reused, err := serverSessionA.Connection().OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("physical A stream credit was not returned: %v", err)
	}
	reused.CancelRead(meshStreamError)
	reused.CancelWrite(meshStreamError)
}

func TestMeshRealQUICDefaultStreamCreditAcrossSources(t *testing.T) {
	const defaultStreamCredit = 100
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	serverConfig := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 2)
	capacity := config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 1, MaxPendingTCPSetupsPerGeneration: 1}
	serverConfig.Ingress.Listeners = []config.MeshIngressListener{
		{Address: "127.0.0.1:18080", Protocol: config.MeshIngressProtocolHTTP, Capacity: capacity},
		{Address: "127.0.0.1:18081", Protocol: config.MeshIngressProtocolHTTP, Capacity: capacity},
	}
	server, err := NewServer(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	startMeshServer(t, server)

	clientSessions := make(map[string]*Session)
	for _, instance := range []string{"a", "b"} {
		clientConfig := testMeshClientConfig(instance, "api", config.ClientAuthMethodToken, files,
			[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: address, ServerName: "localhost"}})
		if clientConfig.Tunnel.Quic.MaxIncomingStreams != 0 {
			t.Fatal("test must use the quic-go default incoming stream limit")
		}
		client, err := NewClient(clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		startMeshClient(t, client)
		clientSessions[instance] = awaitMeshSession(t, client.Sessions(), instance+" client session")
	}
	serverSessions := make(map[string]*Session)
	for range 2 {
		session := awaitMeshSession(t, server.Sessions(), "server client session")
		serverSessions[session.InstanceID()] = session
	}
	server.registry.mu.Lock()
	a := server.registry.clients["a"].current.Load()
	b := server.registry.clients["b"].current.Load()
	server.registry.mu.Unlock()
	if !server.registry.PublishForwarding(a, readyForwarding) {
		t.Fatal("publish A")
	}
	serverSessionA, serverSessionB := serverSessions["a"], serverSessions["b"]
	if serverSessionA == nil || serverSessionB == nil {
		t.Fatalf("server sessions = %v", serverSessions)
	}

	sources := server.ingressSources
	flows := make([]*flowLease, len(sources))
	for i, source := range sources {
		flows[i], err = source.beginFlow()
		if err != nil {
			t.Fatal(err)
		}
		defer flows[i].release()
	}
	var firstStream *quic.Stream
	for i := range defaultStreamCredit {
		owner := i % len(sources)
		request, err := flows[owner].beginRequest()
		if err != nil {
			t.Fatalf("source %d request %d: %v", owner, i, err)
		}
		defer request.release()
		setup, err := flows[owner].beginSetup()
		if err != nil {
			t.Fatalf("source %d setup %d: %v", owner, i, err)
		}
		defer setup.release()
		lease, err := server.registry.beginClientSelection("api", setup, "round-robin").next()
		if err != nil || lease.Generation() != a {
			t.Fatalf("source %d generation %d = (%v, %v)", owner, i, lease, err)
		}
		defer lease.release()
		stream, err := serverSessionA.Connection().OpenStream()
		if err != nil {
			t.Fatalf("physical stream %d of %d: %v", i+1, defaultStreamCredit, err)
		}
		defer stream.CancelRead(meshStreamError)
		defer stream.CancelWrite(meshStreamError)
		if !lease.acknowledge() {
			t.Fatalf("source %d ACK %d failed", owner, i)
		}
		if i == 0 {
			firstStream = stream
		}
	}
	for i, source := range sources {
		if got := source.snapshot(); got.Flows != 1 || got.Setups != 0 || got.Requests != defaultStreamCredit/2 || got.FlowHigh > 1 || got.SetupHigh > 1 {
			t.Fatalf("source %d exceeded its own capacity: %+v", i, got)
		}
		if got := server.registry.trafficSnapshot(a, source); got.Pending != 0 || got.Active != defaultStreamCredit/2 {
			t.Fatalf("source %d exact generation = %+v", i, got)
		}
	}

	request, err := flows[1].beginRequest()
	if err != nil {
		t.Fatal(err)
	}
	defer request.release()
	setup, err := flows[1].beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	defer setup.release()
	physicalLimit := false
	var unexpectedStream *quic.Stream
	unexpected, err := server.registry.beginClientSelection("api", setup, "round-robin").open(context.Background(), func(g *Generation) (func(), error) {
		if g != a {
			t.Fatalf("only A is ready, got %v", g)
		}
		stream, openErr := serverSessionA.Connection().OpenStream()
		var limitErr *quic.StreamLimitReachedError
		physicalLimit = errors.As(openErr, &limitErr)
		if stream != nil {
			unexpectedStream = stream
			return func() { stream.CancelRead(meshStreamError); stream.CancelWrite(meshStreamError) }, openErr
		}
		return nil, openErr
	})
	if unexpectedStream != nil {
		unexpectedStream.CancelRead(meshStreamError)
		unexpectedStream.CancelWrite(meshStreamError)
	}
	if unexpected != nil {
		unexpected.release()
	}
	if !physicalLimit || !errors.Is(err, ErrLocalTransportBudget) || !server.registry.GroupAvailability("api") {
		t.Fatalf("101st default-credit stream = (%v, physical limit %t, group available %t)", err, physicalLimit, server.registry.GroupAvailability("api"))
	}
	if got := sources[1].snapshot(); got.Flows != 1 || got.Setups != 1 || got.Requests != defaultStreamCredit/2+1 {
		t.Fatalf("101st request borrowed or lost source capacity: %+v", got)
	}
	if got := server.registry.trafficSnapshot(a, sources[1]); got.Pending != 0 || got.Active != defaultStreamCredit/2 {
		t.Fatalf("failed 101st A reservation retained: %+v", got)
	}

	if !server.registry.PublishForwarding(b, readyForwarding) {
		t.Fatal("publish B")
	}
	selection := server.registry.beginClientSelection("api", setup, "round-robin")
	selection.start = 0 // Exercise saturated A before the eligible B fallback.
	attempts, retriedLimit := 0, false
	var retryStream *quic.Stream
	retryLease, err := selection.open(context.Background(), func(g *Generation) (func(), error) {
		attempts++
		conn := serverSessionA.Connection()
		if g == b {
			conn = serverSessionB.Connection()
		}
		stream, openErr := conn.OpenStream()
		var limitErr *quic.StreamLimitReachedError
		retriedLimit = retriedLimit || errors.As(openErr, &limitErr)
		if stream == nil {
			return nil, openErr
		}
		retryStream = stream
		return func() { stream.CancelRead(meshStreamError); stream.CancelWrite(meshStreamError) }, openErr
	})
	if retryLease != nil {
		defer retryLease.release()
	}
	if retryStream != nil {
		defer retryStream.CancelRead(meshStreamError)
		defer retryStream.CancelWrite(meshStreamError)
	}
	if err != nil || retryLease.Generation() != b || attempts != 2 || !retriedLimit || !retryLease.acknowledge() {
		t.Fatalf("default-credit bounded retry = (%v, %v, %d attempts, limit %t)", retryLease, err, attempts, retriedLimit)
	}
	if got := sources[1].snapshot(); got.Flows != 1 || got.Setups != 0 || got.Requests != defaultStreamCredit/2+1 {
		t.Fatalf("retry changed second source budget: %+v", got)
	}

	if firstStream == nil {
		t.Fatal("first A stream was not opened")
	}
	if _, err := firstStream.Write([]byte("x")); err != nil {
		t.Fatalf("send first A stream byte: %v", err)
	}
	if err := firstStream.Close(); err != nil {
		t.Fatalf("close first A stream write: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted, err := clientSessions["a"].Connection().AcceptStream(ctx)
	if err != nil {
		t.Fatalf("accept first A stream: %v", err)
	}
	if data, err := io.ReadAll(accepted); err != nil || string(data) != "x" {
		t.Fatalf("read first A stream = %q, %v", data, err)
	}
	if err := accepted.Close(); err != nil {
		t.Fatalf("close accepted A stream write: %v", err)
	}
	if _, err := io.Copy(io.Discard, firstStream); err != nil {
		t.Fatalf("drain first A stream reverse direction: %v", err)
	}
	reused, err := serverSessionA.Connection().OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("default A stream credit was not returned: %v", err)
	}
	reused.CancelRead(meshStreamError)
	reused.CancelWrite(meshStreamError)
}
