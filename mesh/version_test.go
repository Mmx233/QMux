package mesh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
)

func versionTestDeclaration(t *testing.T, files meshTestFiles, version uint64, policy string, metric int64) []byte {
	t.Helper()
	cfg := testMeshClientConfig("instance-a", "api", config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: "localhost:8443"}})
	cfg.Group.RuleVersion = version
	cfg.Group.Metric = metric
	cfg.Group.OutdatedClientPolicy = policy
	if version == 2 {
		cfg.Group.Routes.HTTP = []config.MeshHTTPRoute{{Hostnames: []string{"old.example.com"}}}
	} else {
		cfg.Group.Routes.TLSPassthrough = &config.MeshTLSRoute{Hostnames: []string{"new.example.com"}}
		cfg.Group.OriginTLS = config.MeshOriginTLS{Enabled: true, Verify: true, ExtraCAFiles: []string{files.ca}}
	}
	if err := config.FinalizeMeshClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Group.CanonicalBytes()
}

func registerVersionTestClient(t *testing.T, server *Server, files meshTestFiles, instance string, declaration []byte) *quic.Conn {
	t.Helper()
	registration := protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
		TargetServerID: "edge-a", InstanceID: instance, GroupID: "api",
	}
	conn, _, ack, err := rawMeshRegisterOpenWithInitial(t, server.Address(), files, registration, false, nil, nil,
		func(stream *quic.Stream) error { return sendClientInitial(stream, declaration) })
	if err != nil || !ack.Success || ack.State != protocol.MeshStateAccepted {
		if conn != nil {
			_ = conn.CloseWithError(0, "failed registration")
		}
		t.Fatalf("client %s ACK = %+v, %v", instance, ack, err)
	}
	t.Cleanup(func() { _ = conn.CloseWithError(0, "test complete") })
	awaitMeshSession(t, server.Sessions(), "versioned client "+instance)
	return conn
}

func TestMeshClientRejectsValidStagedACKRealQUIC(t *testing.T) {
	files := testMeshMaterial(t)
	certificate, err := tls.LoadX509KeyPair(files.serverCert, files.serverKey)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13, NextProtos: []string{meshALPN},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	releaseResponder := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		served <- func() error {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = conn.CloseWithError(0, "test complete") }()
			stream, err := conn.AcceptStream(ctx)
			if err != nil {
				return err
			}
			registration, err := protocol.ReadMeshRegister(stream)
			if err != nil {
				return err
			}
			if registration.Role != protocol.MeshRoleClient || registration.Auth == nil {
				return errors.New("client registration role or authentication missing")
			}
			for {
				message, err := protocol.ReadMeshControl(stream)
				if err != nil {
					return err
				}
				if _, complete := message.(protocol.MeshEnd); complete {
					break
				}
			}
			ack := protocol.MeshRegisterAck{
				Success: true, State: protocol.MeshStateStaged, ServerID: "edge-a", Role: protocol.MeshRoleClient,
				SelectedVersion: protocol.MeshProtocolVersion, SelectedCapabilities: protocol.MeshCapabilities(),
				SelectedAuthScheme: registration.Auth.Scheme,
			}
			if err := protocol.ValidateMeshRegisterAck(ack, "edge-a", protocol.MeshRoleClient, registration.Auth.Scheme, registration.Capabilities); err != nil {
				return err
			}
			if err := protocol.WriteMeshRegisterAck(stream, ack); err != nil {
				return err
			}
			select {
			case <-releaseResponder:
			case <-ctx.Done():
			}
			return nil
		}()
	}()
	client, err := NewClient(testMeshClientConfig("instance-a", "api", config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: listener.Addr().String(), ServerName: "localhost"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Stop() }()
	attemptErr := client.runEndpointAttempt(ctx, &client.endpoints[0])
	close(releaseResponder)
	if attemptErr == nil || !strings.Contains(attemptErr.Error(), `unexpected mesh initial state result "staged"`) {
		t.Fatalf("valid staged client ACK was not rejected: %v", attemptErr)
	}
	if err := <-served; err != nil {
		t.Fatalf("staged ACK responder: %v", err)
	}
	select {
	case session := <-client.Sessions():
		t.Fatalf("staged client ACK delivered a session: %+v", session)
	default:
	}
	if client.Snapshot().Endpoints[0].Current {
		t.Fatal("staged client ACK published an exact session")
	}
}

func TestMeshClientGroupVersionRolloutRealQUIC(t *testing.T) {
	for _, test := range []struct {
		name         string
		firstVersion uint64
		policy       string
	}{
		{"v2 then v3 pause", 2, config.MeshOutdatedClientPolicyPauseNewTraffic},
		{"v3 then v2 pause", 3, config.MeshOutdatedClientPolicyPauseNewTraffic},
		{"v2 then v3 apply", 2, config.MeshOutdatedClientPolicyApplyLatestRules},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := testMeshMaterial(t)
			v2 := versionTestDeclaration(t, files, 2, config.MeshOutdatedClientPolicyApplyLatestRules, 22)
			v3 := versionTestDeclaration(t, files, 3, test.policy, 33)
			if len(v2) == len(v3) {
				t.Fatal("rollout declarations must exercise different retained sizes")
			}
			cfg := testMeshServerConfig("edge-a", reserveMeshUDPAddress(t), config.ClientAuthMethodToken, files, nil, 1)
			cfg.Tunnel.HealthTimeout = 15 * time.Second
			cfg.Limits.MaxGroups = 2
			cfg.Limits.MaxGroupDeclarationBytes = int64(max(len(v2), len(v3)))
			cfg.Limits.MaxTotalGroupDeclarationBytes = int64(len(v2) + len(v3))
			server, err := NewServer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			startMeshServer(t, server)
			firstData, secondData := v2, v3
			if test.firstVersion == 3 {
				firstData, secondData = v3, v2
			}
			first := registerVersionTestClient(t, server, files, "instance-a", firstData)
			awaitMeshCondition(t, "first version healthy", func() bool { return server.registry.GroupAvailability("api") })
			var stale *clientSelection
			if test.firstVersion == 2 {
				source := newTCPSource(config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 1, MaxTCPConnectionsPerGeneration: 4, MaxPendingTCPSetupsPerGeneration: 4}, 0)
				flow, err := source.beginFlow()
				if err != nil {
					t.Fatal(err)
				}
				defer flow.release()
				setup, err := flow.beginSetup()
				if err != nil {
					t.Fatal(err)
				}
				defer setup.release()
				stale = server.registry.beginClientSelection("api", setup, "round-robin")
				lease, err := server.registry.beginClientSelection("api", setup, "round-robin").next()
				if err != nil {
					t.Fatal(err)
				}
				defer lease.release()
			}
			second := registerVersionTestClient(t, server, files, "instance-b", secondData)
			awaitMeshCondition(t, "second version healthy", func() bool {
				server.registry.mu.Lock()
				defer server.registry.mu.Unlock()
				entry := server.registry.clients["instance-b"]
				return entry != nil && !entry.current.Empty() && entry.current.Load().forwarding.L4Healthy
			})
			server.controlState.mu.Lock()
			latest := server.controlState.groups["api"]
			revision := server.controlState.revision
			server.controlState.mu.Unlock()
			server.registry.mu.Lock()
			rule := server.registry.groupRules["api"]
			server.registry.mu.Unlock()
			if latest == nil || !bytes.Equal(latest.bytes, v3) || rule.version != 3 || rule.policy != test.policy || revision != 1 && test.firstVersion == 3 || revision != 2 && test.firstVersion == 2 {
				t.Fatalf("rollout latest record/rule/revision = %v, %+v, %d", latest, rule, revision)
			}
			if groups, size := server.declarations.snapshot(); groups != 2 || size != int64(len(v2)+len(v3)) {
				t.Fatalf("online version retention = %d/%d", groups, size)
			}
			if stale != nil {
				server.registry.mu.Lock()
				old := server.registry.clients["instance-a"].current.Load()
				server.registry.mu.Unlock()
				server.registry.PublishForwarding(old, ForwardingEligibility{SessionReady: true, DeclarationReady: true})
				if !server.registry.publishL4Healthy(old) {
					t.Fatal("late L4 health publication failed")
				}
				lease, err := stale.next()
				if test.policy == config.MeshOutdatedClientPolicyPauseNewTraffic {
					if !errors.Is(err, ErrNoMeshCandidate) {
						t.Fatalf("pre-upgrade selection admitted paused v2: %v", err)
					}
				} else if err != nil || lease.Generation() != old {
					t.Fatalf("apply-latest rejected old v2: %v, %v", lease, err)
				}
				if lease != nil {
					lease.release()
				}
			}
			changedV2 := versionTestDeclaration(t, files, 2, config.MeshOutdatedClientPolicyApplyLatestRules, 23)
			registration := protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
				TargetServerID: "edge-a", InstanceID: "instance-c", GroupID: "api",
			}
			conflict, _, ack, err := rawMeshRegisterOpenWithInitial(t, server.Address(), files, registration, false, nil, nil,
				func(stream *quic.Stream) error { return sendClientInitial(stream, changedV2) })
			if conflict != nil {
				_ = conflict.CloseWithError(0, "conflict")
			}
			if err != nil || ack.Success || !strings.Contains(ack.Message, "conflict") {
				t.Fatalf("online old-version canonical conflict = %+v, %v", ack, err)
			}
			if groups, size := server.declarations.snapshot(); groups != 2 || size != int64(len(v2)+len(v3)) {
				t.Fatalf("conflict changed retained records = %d/%d", groups, size)
			}
			awaitMeshCondition(t, "conflicting instance-c released", func() bool {
				snapshot := server.Snapshot().Registry
				return snapshot.ClientBindings == 2 && snapshot.ClientCurrent == 2 && snapshot.ClientPrepared == 0 && snapshot.ClientTotal == 2
			})
			identical := registerVersionTestClient(t, server, files, "instance-c", v3)
			server.controlState.mu.Lock()
			identicalRevision := server.controlState.revision
			server.controlState.mu.Unlock()
			if identicalRevision != revision {
				t.Fatalf("same-version identical registration broadcast revision %d -> %d", revision, identicalRevision)
			}
			_ = identical.CloseWithError(0, "same-version done")
			awaitMeshCondition(t, "same-version exact retirement", func() bool { return server.Snapshot().Registry.ClientCurrent == 2 })
			old := first
			if test.firstVersion == 3 {
				old = second
			}
			_ = old.CloseWithError(0, "old version retired")
			awaitMeshCondition(t, "old exact retirement and record release", func() bool {
				groups, size := server.declarations.snapshot()
				return server.Snapshot().Registry.ClientCurrent == 1 && groups == 1 && size == int64(len(v3))
			})
			newest := second
			if test.firstVersion == 3 {
				newest = first
			}
			_ = newest.CloseWithError(0, "last client retired")
			awaitMeshRegistryEmpty(t, server, "version rollout retirement")
			if groups, size := server.declarations.snapshot(); groups != 1 || size != int64(len(v3)) || server.registry.GroupAvailability("api") {
				t.Fatalf("retired highest record/availability = %d/%d, %t", groups, size, server.registry.GroupAvailability("api"))
			}
		})
	}
}

func TestMeshClientGroupUpgradeRejectsInsufficientTransitionCapacity(t *testing.T) {
	files := testMeshMaterial(t)
	v2 := versionTestDeclaration(t, files, 2, config.MeshOutdatedClientPolicyApplyLatestRules, 22)
	v3 := versionTestDeclaration(t, files, 3, config.MeshOutdatedClientPolicyPauseNewTraffic, 33)
	for _, test := range []struct {
		name         string
		firstVersion uint64
		maxGroups    int
		maxBytes     int64
	}{
		{"v2 first byte limit", 2, 2, int64(len(v2) + len(v3) - 1)},
		{"v3 first byte limit", 3, 2, int64(len(v2) + len(v3) - 1)},
		{"group count limit", 2, 1, int64(len(v2) + len(v3))},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testMeshServerConfig("edge-a", reserveMeshUDPAddress(t), config.ClientAuthMethodToken, files, nil, 1)
			cfg.Tunnel.HealthTimeout = 15 * time.Second
			cfg.Limits.MaxGroups = test.maxGroups
			cfg.Limits.MaxGroupDeclarationBytes = int64(max(len(v2), len(v3)))
			cfg.Limits.MaxTotalGroupDeclarationBytes = test.maxBytes
			server, err := NewServer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			startMeshServer(t, server)
			firstData, secondData := v2, v3
			if test.firstVersion == 3 {
				firstData, secondData = v3, v2
			}
			first := registerVersionTestClient(t, server, files, "instance-a", firstData)
			registration := protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
				TargetServerID: "edge-a", InstanceID: "instance-b", GroupID: "api",
			}
			rejected, _, ack, err := rawMeshRegisterOpenWithInitial(t, server.Address(), files, registration, false, nil, nil,
				func(stream *quic.Stream) error { return sendClientInitial(stream, secondData) })
			if rejected != nil {
				_ = rejected.CloseWithError(0, "capacity rejection")
			}
			if err != nil || ack.Success || !strings.Contains(ack.Message, "capacity") {
				t.Fatalf("transition capacity failure ACK = %+v, %v", ack, err)
			}
			awaitMeshCondition(t, "failed transition cleanup", func() bool { return server.Snapshot().Registry.ClientTotal == 1 })
			server.controlState.mu.Lock()
			current := server.controlState.groups["api"]
			server.controlState.mu.Unlock()
			if current == nil || !bytes.Equal(current.bytes, firstData) {
				t.Fatal("rejected transition replaced published group")
			}
			if groups, size := server.declarations.snapshot(); groups != 1 || size != int64(len(firstData)) {
				t.Fatalf("failed transition retained %d/%d", groups, size)
			}
			_ = first.CloseWithError(0, "test complete")
			awaitMeshRegistryEmpty(t, server, "capacity test retirement")
		})
	}
}

func TestMeshClientGroupSnapshotRetainsTransitionCapacityRealQUIC(t *testing.T) {
	files := testMeshMaterial(t)
	v2 := versionTestDeclaration(t, files, 2, config.MeshOutdatedClientPolicyApplyLatestRules, 22)
	v3 := versionTestDeclaration(t, files, 3, config.MeshOutdatedClientPolicyPauseNewTraffic, 33)
	v4 := versionTestDeclaration(t, files, 4, config.MeshOutdatedClientPolicyPauseNewTraffic, 44)
	if len(v2) == len(v3) {
		t.Fatal("snapshot transition must exercise different retained sizes")
	}
	limit := int64(max(len(v2)+len(v3), len(v3)+len(v4)))
	cfg := testMeshServerConfig("edge-a", reserveMeshUDPAddress(t), config.ClientAuthMethodToken, files, nil, 1)
	cfg.Tunnel.HealthTimeout = 15 * time.Second
	cfg.Limits.MaxGroups = 3
	cfg.Limits.MaxGroupDeclarationBytes = int64(max(len(v2), len(v3), len(v4)))
	cfg.Limits.MaxTotalGroupDeclarationBytes = limit
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	startMeshServer(t, server)
	first := registerVersionTestClient(t, server, files, "instance-a", v2)
	snapshot := server.controlState.subscribe(func() {})
	t.Cleanup(func() {
		snapshot.release()
		server.controlState.unsubscribe(snapshot.link)
	})
	if snapshot.revision != 1 || len(snapshot.groups) != 1 || !bytes.Equal(snapshot.groups[0].bytes, v2) {
		t.Fatalf("v2 peer snapshot = revision %d, groups %d", snapshot.revision, len(snapshot.groups))
	}
	second := registerVersionTestClient(t, server, files, "instance-b", v3)
	server.controlState.mu.Lock()
	current, revision := server.controlState.groups["api"], server.controlState.revision
	server.controlState.mu.Unlock()
	if current == nil || !bytes.Equal(current.bytes, v3) || revision != 2 {
		t.Fatalf("v3 replacement = %v, revision %d", current, revision)
	}
	if groups, size := server.declarations.snapshot(); groups != 2 || size != int64(len(v2)+len(v3)) {
		t.Fatalf("v2/v3 snapshot transition = %d/%d", groups, size)
	}
	_ = first.CloseWithError(0, "v2 exact retired")
	awaitMeshCondition(t, "v2 exact retirement with snapshot retained", func() bool {
		s := server.Snapshot().Registry
		if s.ClientBindings != 1 || s.ClientCurrent != 1 || s.ClientTotal != 1 {
			return false
		}
		server.declarations.mu.Lock()
		old := server.declarations.records[groupKey{id: "api", version: 2}]
		snapshotOnly := old == snapshot.groups[0] && old.refs == 1
		server.declarations.mu.Unlock()
		return snapshotOnly
	})
	server.declarations.mu.Lock()
	old := server.declarations.records[groupKey{id: "api", version: 2}]
	snapshotOnly := old == snapshot.groups[0] && old.refs == 1
	server.declarations.mu.Unlock()
	if !snapshotOnly {
		t.Fatalf("retired v2 not solely held by peer snapshot: %v", old)
	}
	if groups, size := server.declarations.snapshot(); groups != 2 || size != int64(len(v2)+len(v3)) {
		t.Fatalf("snapshot did not retain v2 budget = %d/%d", groups, size)
	}
	registration := protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
		TargetServerID: "edge-a", InstanceID: "instance-c", GroupID: "api",
	}
	rejected, _, ack, err := rawMeshRegisterOpenWithInitial(t, server.Address(), files, registration, false, nil, nil,
		func(stream *quic.Stream) error { return sendClientInitial(stream, v4) })
	if rejected != nil {
		_ = rejected.CloseWithError(0, "three-version capacity rejection")
	}
	if err != nil || ack.Success || !strings.Contains(ack.Message, "capacity") {
		t.Fatalf("three-version transition ACK = %+v, %v", ack, err)
	}
	awaitMeshCondition(t, "rejected v4 binding released", func() bool {
		s := server.Snapshot().Registry
		return s.ClientBindings == 1 && s.ClientCurrent == 1 && s.ClientPrepared == 0 && s.ClientTotal == 1
	})
	server.controlState.mu.Lock()
	current, revision = server.controlState.groups["api"], server.controlState.revision
	server.controlState.mu.Unlock()
	server.registry.mu.Lock()
	rule := server.registry.groupRules["api"]
	server.registry.mu.Unlock()
	if current == nil || !bytes.Equal(current.bytes, v3) || revision != 2 || rule.version != 3 || rule.policy != config.MeshOutdatedClientPolicyPauseNewTraffic {
		t.Fatalf("rejected v4 changed highest group/rule/revision = %v, %+v, %d", current, rule, revision)
	}
	if groups, size := server.declarations.snapshot(); groups != 2 || size != int64(len(v2)+len(v3)) {
		t.Fatalf("rejected v4 changed retained records = %d/%d", groups, size)
	}
	snapshot.release()
	server.controlState.unsubscribe(snapshot.link)
	if groups, size := server.declarations.snapshot(); groups != 1 || size != int64(len(v3)) {
		t.Fatalf("released snapshot did not reclaim v2 = %d/%d", groups, size)
	}
	third := registerVersionTestClient(t, server, files, "instance-c", v4)
	server.controlState.mu.Lock()
	current, revision = server.controlState.groups["api"], server.controlState.revision
	server.controlState.mu.Unlock()
	if current == nil || !bytes.Equal(current.bytes, v4) || revision != 3 {
		t.Fatalf("v4 after snapshot release = %v, revision %d", current, revision)
	}
	if groups, size := server.declarations.snapshot(); groups != 2 || size != int64(len(v3)+len(v4)) {
		t.Fatalf("v3/v4 transition after snapshot release = %d/%d, limit %d", groups, size, limit)
	}
	_ = second.CloseWithError(0, "v3 exact retired")
	_ = third.CloseWithError(0, "v4 exact retired")
	awaitMeshRegistryEmpty(t, server, "snapshot transition retirement")
}

func TestMeshClientL4HealthStartsAfterDelivery(t *testing.T) {
	files := testMeshMaterial(t)
	cfg := testMeshServerConfig("edge-a", reserveMeshUDPAddress(t), config.ClientAuthMethodToken, files, nil, 1)
	cfg.Tunnel.HealthTimeout = 800 * time.Millisecond
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server.sessions = make(chan *Session)
	beforeDelivery := make(chan *Session, 2)
	server.beforeInboundDelivery = func(session *Session) { beforeDelivery <- session }
	startMeshServer(t, server)
	declaration := versionTestDeclaration(t, files, 2, config.MeshOutdatedClientPolicyApplyLatestRules, 22)
	register := func(instance string) *quic.Conn {
		t.Helper()
		registration := protocol.MeshRegister{
			Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
			TargetServerID: "edge-a", InstanceID: instance, GroupID: "api",
		}
		conn, _, ack, err := rawMeshRegisterOpenWithInitial(t, server.Address(), files, registration, false, nil, nil,
			func(stream *quic.Stream) error { return sendClientInitial(stream, declaration) })
		if err != nil || !ack.Success || ack.State != protocol.MeshStateAccepted {
			t.Fatalf("blocked-delivery client ACK = %+v, %v", ack, err)
		}
		t.Cleanup(func() { _ = conn.CloseWithError(0, "test complete") })
		awaitMeshSession(t, beforeDelivery, "committed client before delivery")
		server.registry.mu.Lock()
		entry := server.registry.clients[instance]
		var ready ForwardingEligibility
		if entry != nil && !entry.current.Empty() {
			ready = entry.current.Load().forwarding
		}
		server.registry.mu.Unlock()
		if !ready.SessionReady || !ready.DeclarationReady || ready.L4Healthy || server.registry.GroupAvailability("api") {
			t.Fatalf("client eligible before delivery: %+v", ready)
		}
		return conn
	}
	first := register("instance-a")
	_ = first.CloseWithError(0, "delivery canceled")
	awaitMeshRegistryEmpty(t, server, "canceled delivery")
	if groups, size := server.declarations.snapshot(); groups != 1 || size != int64(len(declaration)) {
		t.Fatalf("canceled delivery highest record = %d/%d", groups, size)
	}
	second := register("instance-b")
	awaitMeshSession(t, server.Sessions(), "delivered client")
	awaitMeshCondition(t, "initial L4 health", func() bool { return server.registry.GroupAvailability("api") })
	awaitMeshRegistryEmpty(t, server, "client heartbeat timeout")
	if server.registry.GroupAvailability("api") {
		t.Fatal("timed-out client remained available")
	}
	_ = second.CloseWithError(0, "test complete")
}

func TestMeshClientMaxRevisionDeclarationPreflight(t *testing.T) {
	for _, size := range []int{protocol.MaxMeshChunkDataSize, 2 * protocol.MaxMeshChunkDataSize, 2*protocol.MaxMeshChunkDataSize + 1, 1 << 20} {
		data := bytes.Repeat([]byte("x"), size)
		record := &groupRecord{key: groupKey{id: "api", version: 1}, digest: sha256.Sum256(data), bytes: data}
		state := newControlState(nil, config.MeshServerLimits{})
		if err := state.preflightClientGroup(record); err != nil {
			t.Fatalf("max revision preflight size %d: %v", size, err)
		}
		frames, err := declarationFrames(math.MaxUint64, data)
		if err != nil || len(frames) != (size+protocol.MaxMeshChunkDataSize-1)/protocol.MaxMeshChunkDataSize {
			t.Fatalf("max revision frames size %d = %d, %v", size, len(frames), err)
		}
		for i, frame := range frames {
			message, err := protocol.ReadMeshControl(bytes.NewReader(frame.data))
			chunk, ok := message.(protocol.MeshChunk)
			if err != nil || !ok || chunk.Sequence != math.MaxUint64 || chunk.Offset != uint32(i*protocol.MaxMeshChunkDataSize) || len(chunk.Digest) != 32 && i == 0 || len(chunk.Digest) != 0 && i > 0 {
				t.Fatalf("max revision chunk %d of %d = %+v, %v", i, size, message, err)
			}
		}
	}
}

func TestMeshPublishedGroupDuplicateAndOlderReferenceOwnership(t *testing.T) {
	v1 := testDeclaration(t, "api", 1, 0)
	v2 := testDeclaration(t, "api", 2, 0)
	limits := testControlLimits(int64(max(len(v1), len(v2))))
	limits.MaxGroups = 2
	limits.MaxTotalGroupDeclarationBytes = int64(len(v1) + len(v2))
	ledger := newDeclarationLedger(limits)
	state := newControlState(ledger, limits)
	first := retainTestDeclaration(t, ledger, v1)
	if err := state.publishGroup(first); err != nil {
		t.Fatal(err)
	}
	if err := state.publishGroup(first); err != nil {
		t.Fatal(err)
	}
	ledger.mu.Lock()
	refs := first.refs
	ledger.mu.Unlock()
	if refs != 1 || state.fence() != 1 {
		t.Fatalf("same-pointer publication changed group owner: refs %d, revision %d", refs, state.fence())
	}
	second := retainTestDeclaration(t, ledger, v2)
	if err := state.publishGroup(second); err != nil {
		t.Fatal(err)
	}
	older := retainTestDeclaration(t, ledger, v1)
	if err := state.publishGroup(older); err != nil {
		t.Fatal(err)
	}
	if groups, size := ledger.snapshot(); groups != 1 || size != int64(len(v2)) || state.fence() != 2 {
		t.Fatalf("ignored lower publication retained %d/%d, revision %d", groups, size, state.fence())
	}
	state.close()
	if groups, size := ledger.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("closed published group retained %d/%d", groups, size)
	}
}

func TestMeshPeerGroupDecisionsRemainStagedRealQUIC(t *testing.T) {
	files := testMeshMaterial(t)
	v2 := versionTestDeclaration(t, files, 2, config.MeshOutdatedClientPolicyApplyLatestRules, 22)
	v3 := versionTestDeclaration(t, files, 3, config.MeshOutdatedClientPolicyPauseNewTraffic, 33)
	v4 := versionTestDeclaration(t, files, 4, config.MeshOutdatedClientPolicyPauseNewTraffic, 44)
	conflict := versionTestDeclaration(t, files, 3, config.MeshOutdatedClientPolicyPauseNewTraffic, 34)
	other := testDeclaration(t, "other", 1, 0)
	cfg := testMeshServerConfig("edge-a", reserveMeshUDPAddress(t), config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-b"}}, 1)
	cfg.Tunnel.HealthTimeout = 15 * time.Second
	cfg.Limits.MaxGroups = 3
	cfg.Limits.MaxGroupDeclarationBytes = int64(max(len(v2), len(v3), len(v4), len(other)))
	cfg.Limits.MaxTotalGroupDeclarationBytes = int64(len(v3) + max(len(v2), len(v4)) + len(other))
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	published := retainTestDeclaration(t, server.declarations, v3)
	if err := server.controlState.publishGroup(published); err != nil {
		t.Fatal(err)
	}
	startMeshServer(t, server)
	registration := protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRolePeer,
		TargetServerID: "edge-a", PeerServerID: "edge-b",
	}
	register := func(groups ...[]byte) (*quic.Conn, protocol.MeshRegisterAck, error) {
		t.Helper()
		var size uint64
		for _, group := range groups {
			size += uint64(len(group))
		}
		conn, _, ack, err := rawMeshRegisterOpenWithInitial(t, server.Address(), files, registration, false, nil, nil,
			func(stream *quic.Stream) error {
				if err := protocol.WriteMeshControl(stream, protocol.MeshBegin{Groups: uint32(len(groups)), GroupBytes: size}); err != nil {
					return err
				}
				for i, group := range groups {
					if err := sendDeclaration(stream, 0, uint32(i), group); err != nil {
						return err
					}
				}
				return protocol.WriteMeshControl(stream, protocol.MeshEnd{Groups: uint32(len(groups)), GroupBytes: size})
			})
		return conn, ack, err
	}
	for _, test := range []struct {
		name string
		data []byte
		want int
	}{
		{"lower", v2, 0},
		{"equal", v3, 0},
		{"higher", v4, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn, ack, err := register(test.data)
			if err != nil || !ack.Success || ack.State != protocol.MeshStateStaged {
				t.Fatalf("peer staged ACK = %+v, %v", ack, err)
			}
			session := awaitMeshSession(t, server.Sessions(), "staged peer group decision")
			if session.staged == nil || len(session.staged.groups) != 1 || len(session.staged.groupUpdates) != test.want {
				t.Fatalf("peer provisional groups/updates = %+v", session.staged)
			}
			if test.want == 1 && session.staged.groupUpdates[0] != (groupKey{"api", 4}) {
				t.Fatal("higher peer update lost its group version")
			}
			server.controlState.mu.Lock()
			current := server.controlState.groups["api"]
			revision := server.controlState.revision
			server.controlState.mu.Unlock()
			if current != published || revision != 1 || server.registry.GroupAvailability("api") {
				t.Fatalf("staged peer activated group: current %v, revision %d", current, revision)
			}
			_ = conn.CloseWithError(0, "peer done")
			awaitMeshRegistryEmpty(t, server, "peer staged release")
			if groups, size := server.declarations.snapshot(); groups != 1 || size != int64(len(v3)) {
				t.Fatalf("peer staged record retained %d/%d", groups, size)
			}
		})
	}
	for _, test := range []struct {
		name   string
		groups [][]byte
		want   string
	}{
		{"equal-version conflict", [][]byte{conflict}, "conflict"},
		{"batch rollback", [][]byte{other, conflict}, "conflict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var input bytes.Buffer
			var total uint64
			for _, group := range test.groups {
				total += uint64(len(group))
			}
			if err := protocol.WriteMeshControl(&input, protocol.MeshBegin{Groups: uint32(len(test.groups)), GroupBytes: total}); err != nil {
				t.Fatal(err)
			}
			for i, group := range test.groups {
				if err := sendDeclaration(&input, 0, uint32(i), group); err != nil {
					t.Fatal(err)
				}
			}
			if err := protocol.WriteMeshControl(&input, protocol.MeshEnd{Groups: uint32(len(test.groups)), GroupBytes: total}); err != nil {
				t.Fatal(err)
			}
			if state, err := receiveInitial(context.Background(), &input, server.declarations, server.paths, server.config.Limits, protocol.MeshRolePeer, ""); state != nil || !errors.Is(err, errMeshGroupConflict) {
				t.Fatalf("peer parsed conflict = %+v, %v", state, err)
			}
			conn, ack, err := register(test.groups...)
			if ack.Success || err == nil && !strings.Contains(ack.Message, test.want) {
				t.Fatalf("peer rejected ACK = %+v, %v", ack, err)
			}
			if err != nil && conn != nil {
				select {
				case <-conn.Context().Done():
				case <-time.After(time.Second):
					t.Fatalf("peer read failed without remote disconnect: %v", err)
				}
			}
			if conn != nil {
				_ = conn.CloseWithError(0, "peer rejected")
			}
			awaitMeshRegistryEmpty(t, server, "peer rejection")
			if groups, size := server.declarations.snapshot(); groups != 1 || size != int64(len(v3)) {
				t.Fatalf("peer rollback retained %d/%d", groups, size)
			}
		})
	}
}

func TestMeshOutboundPeerStagesHigherGroupBeforeReady(t *testing.T) {
	files := testMeshMaterial(t)
	addressA := reserveMeshUDPAddress(t)
	addressB := reserveMeshUDPAddress(t)
	v3 := versionTestDeclaration(t, files, 3, config.MeshOutdatedClientPolicyPauseNewTraffic, 33)
	v4 := versionTestDeclaration(t, files, 4, config.MeshOutdatedClientPolicyPauseNewTraffic, 44)
	v5 := versionTestDeclaration(t, files, 5, config.MeshOutdatedClientPolicyPauseNewTraffic, 55)
	configA := testMeshServerConfig("edge-a", addressA, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-b", Address: addressB, ServerName: "localhost"}}, 1)
	configB := testMeshServerConfig("edge-b", addressB, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-a"}}, 1)
	for _, cfg := range []*config.MeshServer{configA, configB} {
		cfg.Limits.MaxGroups = 3
		cfg.Limits.MaxGroupDeclarationBytes = int64(max(len(v3), len(v4), len(v5)))
		cfg.Limits.MaxTotalGroupDeclarationBytes = int64(len(v3) + len(v4) + len(v5))
	}
	a, err := NewServer(configA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewServer(configB)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.controlState.publishGroup(retainTestDeclaration(t, a.declarations, v3)); err != nil {
		t.Fatal(err)
	}
	if err := b.controlState.publishGroup(retainTestDeclaration(t, b.declarations, v4)); err != nil {
		t.Fatal(err)
	}
	startMeshServer(t, b)
	startMeshServer(t, a)
	outbound := awaitMeshSession(t, a.Sessions(), "outbound higher peer snapshot")
	inbound := awaitMeshSession(t, b.Sessions(), "inbound lower peer snapshot")
	if outbound.staged == nil || len(outbound.staged.groups) != 1 || len(outbound.staged.groupUpdates) != 1 ||
		outbound.staged.groupUpdates[0] != (groupKey{"api", 4}) {
		t.Fatalf("outbound peer group decision = %+v", outbound.staged)
	}
	if inbound.staged == nil || len(inbound.staged.groups) != 1 || len(inbound.staged.groupUpdates) != 0 ||
		!bytes.Equal(inbound.staged.groups[0].bytes, v3) {
		t.Fatalf("inbound peer group decision = %+v", inbound.staged)
	}
	a.controlState.mu.Lock()
	aCurrent := a.controlState.groups["api"]
	a.controlState.mu.Unlock()
	b.controlState.mu.Lock()
	bCurrent := b.controlState.groups["api"]
	b.controlState.mu.Unlock()
	if aCurrent == nil || bCurrent == nil || !bytes.Equal(aCurrent.bytes, v3) || !bytes.Equal(bCurrent.bytes, v4) ||
		a.registry.GroupAvailability("api") || b.registry.GroupAvailability("api") {
		t.Fatal("staged peer decision changed an active group or forwarding availability")
	}
	if err := b.controlState.publishGroup(retainTestDeclaration(t, b.declarations, v5)); err != nil {
		t.Fatal(err)
	}
	awaitMeshCondition(t, "higher peer runtime delta replaced the initial record", func() bool {
		a.declarations.mu.Lock()
		defer a.declarations.mu.Unlock()
		return a.declarations.records[groupKey{"api", 4}] == nil && a.declarations.records[groupKey{"api", 5}] != nil &&
			a.declarations.groups == 2 && a.declarations.bytes == int64(len(v3)+len(v5))
	})
	if outbound.staged.groups[0].key != (groupKey{"api", 5}) || outbound.staged.groupUpdates[0] != (groupKey{"api", 4}) {
		t.Fatalf("peer runtime delta changed provisional decision: %+v", outbound.staged)
	}
	a.declarations.mu.Lock()
	old := a.declarations.records[groupKey{"api", 4}]
	a.declarations.mu.Unlock()
	if old != nil {
		t.Fatal("replaced peer declaration remained in the ledger")
	}
}

func TestMeshOutboundPeerConflictRollsBackBeforeReady(t *testing.T) {
	files := testMeshMaterial(t)
	addressA := reserveMeshUDPAddress(t)
	addressB := reserveMeshUDPAddress(t)
	aVersion := versionTestDeclaration(t, files, 3, config.MeshOutdatedClientPolicyPauseNewTraffic, 33)
	bVersion := versionTestDeclaration(t, files, 3, config.MeshOutdatedClientPolicyPauseNewTraffic, 34)
	earlier := testDeclaration(t, "aaa", 1, 0)
	configA := testMeshServerConfig("edge-a", addressA, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-b", Address: addressB, ServerName: "localhost"}}, 1)
	configB := testMeshServerConfig("edge-b", addressB, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-a"}}, 1)
	for _, cfg := range []*config.MeshServer{configA, configB} {
		cfg.Limits.MaxGroups = 3
		cfg.Limits.MaxGroupDeclarationBytes = int64(max(len(aVersion), len(bVersion), len(earlier)))
		cfg.Limits.MaxTotalGroupDeclarationBytes = int64(len(aVersion) + len(bVersion) + len(earlier))
	}
	a, err := NewServer(configA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewServer(configB)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.controlState.publishGroup(retainTestDeclaration(t, a.declarations, aVersion)); err != nil {
		t.Fatal(err)
	}
	if err := b.controlState.publishGroup(retainTestDeclaration(t, b.declarations, bVersion)); err != nil {
		t.Fatal(err)
	}
	if err := b.controlState.publishGroup(retainTestDeclaration(t, b.declarations, earlier)); err != nil {
		t.Fatal(err)
	}
	var initial bytes.Buffer
	groupBytes := uint64(len(earlier) + len(bVersion))
	if err := protocol.WriteMeshControl(&initial, protocol.MeshBegin{Groups: 2, GroupBytes: groupBytes}); err != nil {
		t.Fatal(err)
	}
	if err := sendDeclaration(&initial, 0, 0, earlier); err != nil {
		t.Fatal(err)
	}
	if err := sendDeclaration(&initial, 0, 1, bVersion); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteMeshControl(&initial, protocol.MeshEnd{Groups: 2, GroupBytes: groupBytes}); err != nil {
		t.Fatal(err)
	}
	if state, err := receiveInitial(context.Background(), &initial, a.declarations, a.paths, a.config.Limits, protocol.MeshRolePeer, ""); state != nil || !errors.Is(err, errMeshGroupConflict) {
		t.Fatalf("outbound peer snapshot conflict = %+v, %v", state, err)
	}
	if groups, size := a.declarations.snapshot(); groups != 1 || size != int64(len(aVersion)) {
		t.Fatalf("parsed peer batch retained %d/%d", groups, size)
	}
	prepared := make(chan RegistrySnapshot, 1)
	a.beforePeerDial = func(string) {
		select {
		case prepared <- a.registry.Snapshot():
		default:
		}
	}
	a.reconnectDelay = func(int) time.Duration { return time.Hour }
	startMeshServer(t, b)
	startMeshServer(t, a)
	select {
	case snapshot := <-prepared:
		if snapshot.PeerPrepared != 1 || snapshot.PeerTotal != 1 {
			t.Fatalf("conflicting outbound peer was not prepared: %+v", snapshot)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("conflicting outbound peer registration did not begin")
	}
	awaitMeshRegistryEmpty(t, a, "conflicting outbound cleanup")
	awaitMeshRegistryEmpty(t, b, "conflicting inbound cleanup")
	for _, test := range []struct {
		server *Server
		data   []byte
		groups int
		size   int64
	}{
		{a, aVersion, 1, int64(len(aVersion))},
		{b, bVersion, 2, int64(len(bVersion) + len(earlier))},
	} {
		select {
		case session := <-test.server.Sessions():
			t.Fatalf("conflicting peer reached session delivery: %+v", session)
		default:
		}
		test.server.controlState.mu.Lock()
		current := test.server.controlState.groups["api"]
		test.server.controlState.mu.Unlock()
		if current == nil || !bytes.Equal(current.bytes, test.data) || test.server.registry.GroupAvailability("api") {
			t.Fatal("conflicting peer replaced an active group")
		}
		if groups, size := test.server.declarations.snapshot(); groups != test.groups || size != test.size {
			t.Fatalf("conflicting peer retained staged record %d/%d", groups, size)
		}
	}
	a.declarations.mu.Lock()
	stagedEarlier := a.declarations.records[groupKey{"aaa", 1}]
	a.declarations.mu.Unlock()
	if stagedEarlier != nil {
		t.Fatal("outbound peer conflict retained an earlier staged group")
	}
}

func TestMeshGroupCommitReleasesRegistryBeforeBroadcast(t *testing.T) {
	files := testMeshMaterial(t)
	v2 := versionTestDeclaration(t, files, 2, config.MeshOutdatedClientPolicyApplyLatestRules, 22)
	v3 := versionTestDeclaration(t, files, 3, config.MeshOutdatedClientPolicyPauseNewTraffic, 33)
	cfg := testMeshServerConfig("edge-a", "127.0.0.1:8443", config.ClientAuthMethodToken, files, nil, 1)
	cfg.Limits.MaxGroups = 2
	cfg.Limits.MaxGroupDeclarationBytes = int64(max(len(v2), len(v3)))
	cfg.Limits.MaxTotalGroupDeclarationBytes = int64(len(v2) + len(v3))
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := server.registry
	prepare := func(instance string, data []byte) (*Generation, *stagedState) {
		t.Helper()
		pending, err := r.BeginInbound(Owner{})
		if err != nil {
			t.Fatal(err)
		}
		g, err := r.PrepareClient(pending, instance, "api")
		if err != nil {
			t.Fatal(err)
		}
		record := retainTestDeclaration(t, server.declarations, data)
		if err := server.controlState.preflightClientGroup(record); err != nil {
			t.Fatal(err)
		}
		return g, &stagedState{ledger: server.declarations, groups: []*groupRecord{record}}
	}
	newer, newerState := prepare("newer", v3)
	older, olderState := prepare("older", v2)
	link := &peerLink{queue: newControlQueue(server.config.Limits), close: func() {}}
	server.controlState.mu.Lock()
	server.controlState.links[link] = struct{}{}
	server.controlState.mu.Unlock()
	link.queue.mu.Lock()
	queueLocked := true
	defer func() {
		if queueLocked {
			link.queue.mu.Unlock()
		}
	}()
	newerDone := make(chan bool, 1)
	go func() {
		committed, stop := server.commitClientGroup(newer, newerState)
		newerDone <- committed && !stop
	}()
	observed := make(chan struct{}, 1)
	stopObserve := make(chan struct{})
	defer close(stopObserve)
	go func() {
		for {
			select {
			case <-stopObserve:
				return
			default:
			}
			r.mu.Lock()
			version := r.groupRules["api"].version
			r.mu.Unlock()
			if version == 3 {
				observed <- struct{}{}
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	select {
	case <-observed:
	case <-time.After(3 * time.Second):
		link.queue.mu.Unlock()
		queueLocked = false
		<-newerDone
		t.Fatal("registry lock remained held during blocked notification queue")
	}
	if server.controlState.mu.TryLock() {
		server.controlState.mu.Unlock()
		t.Fatal("notification lost the control-state ordering fence")
	}
	olderDone := make(chan bool, 1)
	go func() {
		committed, stop := server.commitClientGroup(older, olderState)
		olderDone <- committed && !stop
	}()
	link.queue.mu.Unlock()
	queueLocked = false
	if !<-newerDone || !<-olderDone {
		t.Fatal("concurrent exact group commits failed")
	}
	if server.controlState.fence() != 1 {
		t.Fatalf("late older commit changed notification revision to %d", server.controlState.fence())
	}
	link.queue.mu.Lock()
	frames := append([]queuedControlFrame(nil), link.queue.frames...)
	link.queue.mu.Unlock()
	if len(frames) == 0 {
		t.Fatal("newest group notification missing")
	}
	for _, frame := range frames {
		if frame.sequence != 1 {
			t.Fatalf("notification sequence = %d", frame.sequence)
		}
	}
	if !r.publishL4Healthy(newer) || !r.publishL4Healthy(older) {
		t.Fatal("committed exact health publication failed")
	}
	r.mu.Lock()
	newReady := r.isForwardingReadyLocked(newer)
	oldReady := r.isForwardingReadyLocked(older)
	r.mu.Unlock()
	if !newReady || oldReady {
		t.Fatalf("concurrent commit eligibility = new %t, old %t", newReady, oldReady)
	}
	source := newTCPSource(config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 1, MaxTCPConnectionsPerGeneration: 4, MaxPendingTCPSetupsPerGeneration: 4}, 0)
	flow, err := source.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	setup, err := flow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	lease, err := r.beginClientSelection("api", setup, "round-robin").next()
	if err != nil || lease.Generation() != newer {
		t.Fatalf("final reserve after concurrent commits = %v, %v", lease, err)
	}
	lease.release()
	setup.release()
	flow.release()
	r.BeginRetire(newer)
	r.BeginRetire(older)
	r.Release(newer)
	r.Release(older)
	newerState.close()
	olderState.close()
	server.controlState.close()
	if groups, size := server.declarations.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("concurrent commit cleanup retained %d/%d", groups, size)
	}
	_ = server.Stop()
}
