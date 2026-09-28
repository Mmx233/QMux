package mesh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
)

func testDeclaration(t *testing.T, groupID string, version uint64, metric int64) []byte {
	t.Helper()
	cfg := config.MeshClient{
		InstanceID: "instance-a",
		Tunnel: config.MeshClientTunnel{
			Servers: []config.MeshServerEndpoint{{ServerID: "edge-a", Address: "localhost:8443"}},
			Auth:    config.ClientAuth{Method: config.ClientAuthMethodToken, Token: meshTestToken},
			TLS:     config.ClientTLS{CACertFile: "unused.pem"},
		},
		Local: config.LocalService{Host: "127.0.0.1", Port: 8080},
		Group: config.MeshGroup{GroupID: groupID, RuleVersion: version, Metric: metric},
	}
	if err := config.FinalizeMeshClientConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Group.CanonicalBytes()
}

func testLargeDeclaration(t *testing.T, files meshTestFiles, groupID string) []byte {
	t.Helper()
	cfg := testMeshClientConfig("instance-a", groupID, config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: "localhost:8443"}})
	cfg.Group.Routes.HTTP = []config.MeshHTTPRoute{{
		Hostnames: []string{"large.example.com"},
		Matches: []config.MeshHTTPMatch{{Headers: []config.MeshHTTPHeaderMatch{{
			Name: "X-Large", Value: strings.Repeat("x", 2400),
		}}}},
	}}
	if err := config.FinalizeMeshClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	declaration := cfg.Group.CanonicalBytes()
	if len(declaration) <= protocol.MaxMeshChunkDataSize {
		t.Fatalf("large declaration is only %d bytes", len(declaration))
	}
	return declaration
}

func testCandidateDeclaration(t *testing.T, files meshTestFiles, version uint64) []byte {
	t.Helper()
	cfg := testMeshClientConfig("instance-a", "api", config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: "localhost:8443"}})
	cfg.Group.RuleVersion = version
	cfg.Group.Routes.HTTP = []config.MeshHTTPRoute{{
		Hostnames: []string{"large.example.com"},
		Matches: []config.MeshHTTPMatch{{Headers: []config.MeshHTTPHeaderMatch{
			{Name: "X-First", Value: strings.Repeat("x", 1500)},
			{Name: "X-Second", Value: strings.Repeat("x", 1500)},
			{Name: "X-Third", Value: strings.Repeat("x", 1500)},
		}}},
	}}
	if err := config.FinalizeMeshClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	declaration := cfg.Group.CanonicalBytes()
	if len(declaration) <= 2*protocol.MaxMeshChunkDataSize {
		t.Fatalf("candidate declaration is only %d bytes", len(declaration))
	}
	return declaration
}

type meshInitialWire struct {
	begin  protocol.MeshBegin
	groups [][]byte
	paths  []protocol.MeshPath
	end    protocol.MeshEnd
	chunks int
}

func readMeshInitialWire(t *testing.T, r io.Reader) meshInitialWire {
	t.Helper()
	first, err := protocol.ReadMeshControl(r)
	if err != nil {
		t.Fatal(err)
	}
	begin, ok := first.(protocol.MeshBegin)
	if !ok {
		t.Fatalf("initial wire starts with %T", first)
	}
	result := meshInitialWire{begin: begin, groups: make([][]byte, begin.Groups)}
	for {
		message, err := protocol.ReadMeshControl(r)
		if err != nil {
			t.Fatal(err)
		}
		switch value := message.(type) {
		case protocol.MeshChunk:
			if value.Sequence != 0 || value.Record >= begin.Groups || int(value.Offset) != len(result.groups[value.Record]) {
				t.Fatalf("initial chunk order = %+v", value)
			}
			result.groups[value.Record] = append(result.groups[value.Record], value.Data...)
			result.chunks++
		case protocol.MeshPath:
			if value.Sequence != 0 {
				t.Fatalf("initial path sequence = %d", value.Sequence)
			}
			result.paths = append(result.paths, value)
		case protocol.MeshEnd:
			result.end = value
			return result
		default:
			t.Fatalf("unexpected initial wire message %T", message)
		}
	}
}

func testControlLimits(size int64) config.MeshServerLimits {
	return config.MeshServerLimits{
		MaxGroups: 1, MaxGroupDeclarationBytes: size,
		MaxTotalGroupDeclarationBytes: size,
		MaxPathsPerGroup:              2, MaxTotalPaths: 4, MaxPathHops: 3,
		MaxControlQueueMessages: 16, MaxControlQueueBytes: 1 << 20,
	}
}

func retainTestDeclaration(t *testing.T, ledger *declarationLedger, data []byte) *groupRecord {
	t.Helper()
	digest := sha256.Sum256(data)
	claim, err := ledger.begin(uint32(len(data)), digest[:])
	if err != nil {
		t.Fatal(err)
	}
	defer claim.close()
	record, err := claim.finish(context.Background(), bytes.Clone(data), "")
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestMeshDeclarationLedgerFullReuseConflictAndTransition(t *testing.T) {
	v1 := testDeclaration(t, "api", 1, 1)
	changed := testDeclaration(t, "api", 1, 2)
	v2 := testDeclaration(t, "api", 2, 1)
	if len(v1) != len(changed) || len(v1) != len(v2) {
		t.Fatalf("test declarations have unequal lengths: %d %d %d", len(v1), len(changed), len(v2))
	}
	ledger := newDeclarationLedger(testControlLimits(int64(len(v1))))
	first := retainTestDeclaration(t, ledger, v1)
	second := retainTestDeclaration(t, ledger, v1)
	if first != second {
		t.Fatal("identical full-capacity declaration was copied")
	}
	if groups, size := ledger.snapshot(); groups != 1 || size != int64(len(v1)) {
		t.Fatalf("reused declaration budget = %d/%d", groups, size)
	}

	digest := sha256.Sum256(changed)
	claim, err := ledger.begin(uint32(len(changed)), digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if claim.reserved {
		t.Fatal("full ledger reserved capacity for a different declaration")
	}
	if _, err := claim.finish(context.Background(), bytes.Clone(changed), ""); !errors.Is(err, errMeshGroupConflict) {
		t.Fatalf("full-ledger same-version classification = %v", err)
	}
	claim.close()

	digest = sha256.Sum256(v2)
	claim, err = ledger.begin(uint32(len(v2)), digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claim.finish(context.Background(), bytes.Clone(v2), ""); !errors.Is(err, errMeshGroupCapacity) {
		t.Fatalf("full-ledger higher-version classification = %v", err)
	}
	claim.close()
	ledger.release(first)
	if groups, size := ledger.snapshot(); groups != 1 || size != int64(len(v1)) {
		t.Fatalf("retiring referenced version budget = %d/%d", groups, size)
	}
	ledger.release(second)
	third := retainTestDeclaration(t, ledger, v2)
	ledger.release(third)
	if groups, size := ledger.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("released declaration budget = %d/%d", groups, size)
	}
}

func TestMeshDeclarationCandidateChunkDivergenceAndOwnership(t *testing.T) {
	files := testMeshMaterial(t)
	original := testCandidateDeclaration(t, files, 1)
	next := testCandidateDeclaration(t, files, 2)
	if len(original) != len(next) {
		t.Fatalf("candidate versions have unequal lengths: %d/%d", len(original), len(next))
	}
	limits := testControlLimits(int64(len(original)))
	digest := sha256.Sum256(original)
	for chunk := range 3 {
		t.Run([]string{"first", "middle", "last"}[chunk], func(t *testing.T) {
			ledger := newDeclarationLedger(limits)
			owner := retainTestDeclaration(t, ledger, original)
			changed := bytes.Clone(original)
			start := chunk * protocol.MaxMeshChunkDataSize
			end := min(len(changed), start+protocol.MaxMeshChunkDataSize)
			position := bytes.Index(changed[start:end], []byte("xxx"))
			if position < 0 {
				t.Fatalf("chunk %d has no mutable header value", chunk)
			}
			changed[start+position] = 'y'
			claim, err := ledger.begin(uint32(len(changed)), digest[:])
			if err != nil || claim.candidate != owner {
				t.Fatalf("digest candidate = %p, %v", claim.candidate, err)
			}
			raw := make([]byte, len(changed))
			claim.trackInput(raw, digest[:])
			for offset := 0; offset < len(changed); offset += protocol.MaxMeshChunkDataSize {
				end := min(len(changed), offset+protocol.MaxMeshChunkDataSize)
				claim.compare(uint32(offset), changed[offset:end])
				copy(raw[offset:end], changed[offset:end])
				if offset < start && !claim.matched || offset >= start && claim.matched || claim.candidate != owner {
					t.Fatalf("chunk %d candidate state = matched %t, record %p", offset/protocol.MaxMeshChunkDataSize, claim.matched, claim.candidate)
				}
			}
			if _, err := claim.finish(context.Background(), raw, "api"); !errors.Is(err, errMeshGroupConflict) {
				t.Fatalf("same-digest changed content classification = %v", err)
			}
			claim.close()
			if owner.refs != 1 {
				t.Fatalf("candidate owner refs = %d", owner.refs)
			}
			if groups, size := ledger.snapshot(); groups != 1 || size != int64(len(original)) {
				t.Fatalf("candidate divergence retained %d/%d", groups, size)
			}
			if current, _, _ := ledger.workSnapshot(); current != 0 {
				t.Fatalf("candidate divergence retained %d work bytes", current)
			}
			ledger.release(owner)
		})
	}
	for _, test := range []struct {
		name       string
		otherOwner bool
	}{
		{"last candidate reference", false},
		{"concurrent owner", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ledger := newDeclarationLedger(limits)
			owner := retainTestDeclaration(t, ledger, original)
			claim, err := ledger.begin(uint32(len(next)), digest[:])
			if err != nil || claim.candidate != owner {
				t.Fatalf("transition candidate = %p, %v", claim.candidate, err)
			}
			raw := bytes.Clone(next)
			claim.trackInput(raw, digest[:])
			claim.compare(0, raw[:protocol.MaxMeshChunkDataSize])
			var concurrent *groupRecord
			if test.otherOwner {
				concurrent = retainTestDeclaration(t, ledger, original)
			}
			ledger.release(owner)
			if groups, size := ledger.snapshot(); groups != 1 || size != int64(len(original)) {
				t.Fatalf("candidate released before finish = %d/%d", groups, size)
			}
			record, err := claim.finish(context.Background(), raw, "api")
			claim.close()
			if test.otherOwner {
				if !errors.Is(err, errMeshGroupCapacity) || record != nil {
					t.Fatalf("concurrent owner transition = %p, %v", record, err)
				}
				if groups, size := ledger.snapshot(); groups != 1 || size != int64(len(original)) {
					t.Fatalf("concurrent owner budget = %d/%d", groups, size)
				}
				ledger.release(concurrent)
			} else {
				if err != nil || record == nil || record.key.version != 2 || !bytes.Equal(record.bytes, next) {
					t.Fatalf("last-reference transition = %p, %v", record, err)
				}
				ledger.release(record)
			}
			if groups, size := ledger.snapshot(); groups != 0 || size != 0 {
				t.Fatalf("transition retained %d/%d", groups, size)
			}
			if current, _, _ := ledger.workSnapshot(); current != 0 {
				t.Fatalf("transition retained %d work bytes", current)
			}
		})
	}
	t.Run("canceled candidate", func(t *testing.T) {
		ledger := newDeclarationLedger(limits)
		owner := retainTestDeclaration(t, ledger, original)
		claim, err := ledger.begin(uint32(len(original)), digest[:])
		if err != nil || claim.candidate != owner {
			t.Fatalf("canceled candidate = %p, %v", claim.candidate, err)
		}
		raw := bytes.Clone(original)
		claim.trackInput(raw, digest[:])
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := claim.finish(ctx, raw, "api"); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled candidate finish = %v", err)
		}
		claim.close()
		if owner.refs != 1 {
			t.Fatalf("canceled candidate owner refs = %d", owner.refs)
		}
		ledger.release(owner)
		if groups, size := ledger.snapshot(); groups != 0 || size != 0 {
			t.Fatalf("canceled candidate retained %d/%d", groups, size)
		}
		if current, _, _ := ledger.workSnapshot(); current != 0 {
			t.Fatalf("canceled candidate retained %d work bytes", current)
		}
	})
}

func TestMeshFullCapacityClientAndPeerReconnect(t *testing.T) {
	files := testMeshMaterial(t)
	declaration := testDeclaration(t, "api", 1, 0)
	limit := int64(len(declaration))
	t.Run("client HA", func(t *testing.T) {
		address := reserveMeshUDPAddress(t)
		cfg := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1)
		cfg.Limits.MaxGroups = 1
		cfg.Limits.MaxGroupDeclarationBytes = limit
		cfg.Limits.MaxTotalGroupDeclarationBytes = limit
		server, err := NewServer(cfg)
		if err != nil {
			t.Fatal(err)
		}
		startMeshServer(t, server)
		register := func(instanceID string) *quic.Conn {
			t.Helper()
			registration := protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
				TargetServerID: "edge-a", InstanceID: instanceID, GroupID: "api",
			}
			conn, _, ack, err := rawMeshRegisterOpenWithInitial(t, address, files, registration, false, nil, nil,
				func(stream *quic.Stream) error { return sendClientInitial(stream, declaration) })
			if err != nil || !ack.Success || ack.State != protocol.MeshStateStaged {
				t.Fatalf("full-capacity client %s ACK = %+v, %v", instanceID, ack, err)
			}
			awaitMeshSession(t, server.Sessions(), "full-capacity client")
			return conn
		}
		first := register("instance-a")
		defer func() { _ = first.CloseWithError(0, "test complete") }()
		server.declarations.mu.Lock()
		original := server.declarations.records[groupKey{"api", 1}]
		server.declarations.mu.Unlock()
		second := register("instance-b")
		defer func() { _ = second.CloseWithError(0, "test complete") }()
		_ = first.CloseWithError(0, "reconnect")
		awaitMeshCondition(t, "first client retirement", func() bool {
			return server.Snapshot().Registry.ClientCurrent == 1
		})
		first = register("instance-a")
		server.declarations.mu.Lock()
		reused := server.declarations.records[groupKey{"api", 1}]
		server.declarations.mu.Unlock()
		if original == nil || reused != original {
			t.Fatal("full-capacity clients did not share the exact declaration record")
		}
		if groups, size := server.declarations.snapshot(); groups != 1 || size != limit {
			t.Fatalf("client HA declaration budget = %d/%d", groups, size)
		}
		_ = first.CloseWithError(0, "test complete")
		_ = second.CloseWithError(0, "test complete")
		awaitMeshRegistryEmpty(t, server, "client HA release")
		if groups, size := server.declarations.snapshot(); groups != 0 || size != 0 {
			t.Fatalf("client HA retained %d/%d", groups, size)
		}
	})
	t.Run("peer", func(t *testing.T) {
		address := reserveMeshUDPAddress(t)
		cfg := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files,
			[]config.MeshPeer{{ServerID: "edge-b"}}, 1)
		cfg.Limits.MaxGroups = 1
		cfg.Limits.MaxGroupDeclarationBytes = limit
		cfg.Limits.MaxTotalGroupDeclarationBytes = limit
		server, err := NewServer(cfg)
		if err != nil {
			t.Fatal(err)
		}
		published := retainTestDeclaration(t, server.declarations, declaration)
		if err := server.controlState.publishGroup(published); err != nil {
			t.Fatal(err)
		}
		done := startMeshServer(t, server)
		register := func() *quic.Conn {
			t.Helper()
			registration := protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRolePeer,
				TargetServerID: "edge-a", PeerServerID: "edge-b",
			}
			conn, _, ack, err := rawMeshRegisterOpenWithInitial(t, address, files, registration, false, nil, nil,
				func(stream *quic.Stream) error {
					if err := protocol.WriteMeshControl(stream, protocol.MeshBegin{Groups: 1, GroupBytes: uint64(len(declaration))}); err != nil {
						return err
					}
					if err := sendDeclaration(stream, 0, 0, declaration); err != nil {
						return err
					}
					return protocol.WriteMeshControl(stream, protocol.MeshEnd{Groups: 1, GroupBytes: uint64(len(declaration))})
				})
			if err != nil || !ack.Success || ack.State != protocol.MeshStateStaged {
				t.Fatalf("full-capacity peer ACK = %+v, %v", ack, err)
			}
			session := awaitMeshSession(t, server.Sessions(), "full-capacity peer")
			if session.staged == nil || len(session.staged.groups) != 1 || session.staged.groups[0] != published {
				t.Fatal("full-capacity peer did not reuse published declaration")
			}
			return conn
		}
		first := register()
		_ = first.CloseWithError(0, "reconnect")
		awaitMeshRegistryEmpty(t, server, "first peer retirement")
		second := register()
		if groups, size := server.declarations.snapshot(); groups != 1 || size != limit {
			t.Fatalf("peer reconnect declaration budget = %d/%d", groups, size)
		}
		_ = second.CloseWithError(0, "test complete")
		awaitMeshRegistryEmpty(t, server, "second peer retirement")
		stopMeshServer(t, server, done)
		if groups, size := server.declarations.snapshot(); groups != 0 || size != 0 {
			t.Fatalf("peer reconnect retained %d/%d", groups, size)
		}
	})
}

func TestMeshDeclarationLedgerNormalizesRetainedBacking(t *testing.T) {
	declaration := testDeclaration(t, "api", 1, 0)
	ledger := newDeclarationLedger(testControlLimits(int64(len(declaration))))
	digest := sha256.Sum256(declaration)
	claim, err := ledger.begin(uint32(len(declaration)), digest[:])
	if err != nil {
		t.Fatal(err)
	}
	oversized := make([]byte, len(declaration), len(declaration)+1)
	copy(oversized, declaration)
	record, err := claim.finish(context.Background(), oversized, "api")
	if err != nil {
		t.Fatalf("exact-limit declaration with spare input capacity = %v", err)
	}
	claim.close()
	if cap(record.bytes) != len(declaration) {
		t.Fatalf("retained backing capacity = %d, want %d", cap(record.bytes), len(declaration))
	}
	if groups, size := ledger.snapshot(); groups != 1 || size != int64(cap(record.bytes)) {
		t.Fatalf("retained backing budget = %d/%d", groups, size)
	}
	if current, peak, _ := ledger.workSnapshot(); current != 0 || peak < int64(len(declaration)) {
		t.Fatalf("normalized backing work = current %d, peak %d", current, peak)
	}
	ledger.release(record)
	if groups, size := ledger.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("released backing budget = %d/%d", groups, size)
	}
}

func TestMeshInitialRejectsIncompleteOrMisorderedDeclaration(t *testing.T) {
	declaration := testDeclaration(t, "api", 1, 0)
	digest := sha256.Sum256(declaration)
	limits := testControlLimits(int64(len(declaration)))
	for _, test := range []struct {
		name string
		add  func(*bytes.Buffer) error
	}{
		{"missing chunk", func(w *bytes.Buffer) error {
			return protocol.WriteMeshControl(w, protocol.MeshEnd{Groups: 1, GroupBytes: uint64(len(declaration))})
		}},
		{"wrong offset", func(w *bytes.Buffer) error {
			return protocol.WriteMeshControl(w, protocol.MeshChunk{
				Total: uint32(len(declaration)), Offset: uint32(len(declaration)/2 + 1),
				Data: declaration[len(declaration)/2+1:],
			})
		}},
		{"half declaration", func(*bytes.Buffer) error { return nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var wire bytes.Buffer
			if err := protocol.WriteMeshControl(&wire, protocol.MeshBegin{Groups: 1, GroupBytes: uint64(len(declaration))}); err != nil {
				t.Fatal(err)
			}
			if err := protocol.WriteMeshControl(&wire, protocol.MeshChunk{
				Total: uint32(len(declaration)), Digest: digest[:], Data: declaration[:len(declaration)/2],
			}); err != nil {
				t.Fatal(err)
			}
			if err := test.add(&wire); err != nil {
				t.Fatal(err)
			}
			ledger := newDeclarationLedger(limits)
			if state, err := receiveInitial(context.Background(), &wire, ledger, newPathBudget(limits), limits, protocol.MeshRoleClient, "api"); err == nil {
				state.close()
				t.Fatal("incomplete declaration accepted")
			}
			if groups, size := ledger.snapshot(); groups != 0 || size != 0 {
				t.Fatalf("rejected declaration retained %d/%d", groups, size)
			}
			if current, peak, _ := ledger.workSnapshot(); current != 0 || peak < int64(len(declaration)) {
				t.Fatalf("rejected declaration work = current %d, peak %d", current, peak)
			}
		})
	}
	var oversized bytes.Buffer
	header := make([]byte, 5)
	header[0] = protocol.MsgTypeMeshBegin
	binary.BigEndian.PutUint32(header[1:], protocol.MaxControlPayloadSize+1)
	oversized.Write(header)
	if _, err := receiveInitial(context.Background(), &oversized, newDeclarationLedger(limits), newPathBudget(limits), limits, protocol.MeshRoleClient, "api"); err == nil {
		t.Fatal("oversized initial frame accepted")
	}
}

func TestMeshInitialRejectsSingleGroupLimitBeforeBackingAndACK(t *testing.T) {
	declaration := testDeclaration(t, "api", 1, 0)
	limits := testControlLimits(int64(len(declaration)))
	limits.MaxGroupDeclarationBytes = int64(len(declaration) - 1)
	var wire bytes.Buffer
	if err := protocol.WriteMeshControl(&wire, protocol.MeshBegin{Groups: 1, GroupBytes: uint64(len(declaration))}); err != nil {
		t.Fatal(err)
	}
	if err := sendDeclaration(&wire, 0, 0, declaration); err != nil {
		t.Fatal(err)
	}
	ledger := newDeclarationLedger(limits)
	if state, err := receiveInitial(context.Background(), &wire, ledger, newPathBudget(limits), limits, protocol.MeshRoleClient, "api"); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		if state != nil {
			state.close()
		}
		t.Fatalf("oversized group receive = %v", err)
	}
	if groups, size := ledger.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("oversized group retained %d/%d", groups, size)
	}
	if current, peak, _ := ledger.workSnapshot(); current != 0 || peak != 0 {
		t.Fatalf("oversized group allocated input backing = current %d, peak %d", current, peak)
	}

	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	serverConfig := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1)
	serverConfig.Limits.MaxGroupDeclarationBytes = limits.MaxGroupDeclarationBytes
	serverConfig.Limits.MaxTotalGroupDeclarationBytes = limits.MaxTotalGroupDeclarationBytes
	server, err := NewServer(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	startMeshServer(t, server)
	registration := protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
		TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "api",
	}
	conn, _, ack, err := rawMeshRegisterOpenWithInitial(t, address, files, registration, false, nil, nil,
		func(stream *quic.Stream) error { return sendClientInitial(stream, declaration) })
	if conn != nil {
		defer func() { _ = conn.CloseWithError(0, "test complete") }()
	}
	if err != nil || ack.Success || !strings.Contains(ack.Message, "exceeds limit") {
		t.Fatalf("oversized group failure ACK = %+v, %v", ack, err)
	}
	awaitMeshRegistryEmpty(t, server, "oversized group rollback")
	if current, peak, _ := server.declarations.workSnapshot(); current != 0 || peak != 0 {
		t.Fatalf("oversized QUIC group allocated input backing = current %d, peak %d", current, peak)
	}
}

func TestMeshInitialRejectsDuplicateGroup(t *testing.T) {
	declaration := testDeclaration(t, "api", 1, 0)
	limits := testControlLimits(2 * int64(len(declaration)))
	limits.MaxGroups = 2
	limits.MaxGroupDeclarationBytes = int64(len(declaration))
	var wire bytes.Buffer
	if err := protocol.WriteMeshControl(&wire, protocol.MeshBegin{Groups: 2, GroupBytes: uint64(2 * len(declaration))}); err != nil {
		t.Fatal(err)
	}
	for record := range uint32(2) {
		if err := sendDeclaration(&wire, 0, record, declaration); err != nil {
			t.Fatal(err)
		}
	}
	if err := protocol.WriteMeshControl(&wire, protocol.MeshEnd{Groups: 2, GroupBytes: uint64(2 * len(declaration))}); err != nil {
		t.Fatal(err)
	}
	ledger := newDeclarationLedger(limits)
	if state, err := receiveInitial(context.Background(), &wire, ledger, newPathBudget(limits), limits, protocol.MeshRolePeer, ""); err == nil {
		state.close()
		t.Fatal("duplicate group snapshot accepted")
	}
	if groups, size := ledger.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("duplicate group retained %d/%d", groups, size)
	}
	if current, _, _ := ledger.workSnapshot(); current != 0 {
		t.Fatalf("duplicate group retained %d work bytes", current)
	}
}

func TestMeshClientInitialRejectsDeltasBeforeACK(t *testing.T) {
	files := testMeshMaterial(t)
	declaration := testDeclaration(t, "api", 1, 0)
	other := testDeclaration(t, "other", 1, 0)
	digest := sha256.Sum256(other)
	for _, test := range []struct {
		name    string
		message any
	}{
		{"group delta", protocol.MeshChunk{Sequence: 1, Total: uint32(len(other)), Digest: digest[:], Data: other}},
		{"path delta", protocol.MeshPath{Sequence: 1, PathID: "path-a", GroupID: "api", RuleVersion: 1,
			TerminalServerID: "edge-a", Servers: []string{"edge-a"}, Generation: 1}},
		{"withdrawal", protocol.MeshWithdraw{Sequence: 1, PathID: "path-a"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			address := reserveMeshUDPAddress(t)
			server, err := NewServer(testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1))
			if err != nil {
				t.Fatal(err)
			}
			startMeshServer(t, server)
			registration := protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
				TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "api",
			}
			conn, _, ack, err := rawMeshRegisterOpenWithInitial(t, address, files, registration, false, nil, nil,
				func(stream *quic.Stream) error {
					if err := protocol.WriteMeshControl(stream, protocol.MeshBegin{Groups: 1, GroupBytes: uint64(len(declaration))}); err != nil {
						return err
					}
					if err := sendDeclaration(stream, 0, 0, declaration); err != nil {
						return err
					}
					if err := protocol.WriteMeshControl(stream, test.message); err != nil {
						return err
					}
					return protocol.WriteMeshControl(stream, protocol.MeshEnd{
						Groups: 1, GroupBytes: uint64(len(declaration)), FinalSequence: 1,
					})
				})
			if conn != nil {
				defer func() { _ = conn.CloseWithError(0, "test complete") }()
			}
			if err != nil || ack.Success || !strings.Contains(ack.Message, "mesh client initial state cannot contain") {
				t.Fatalf("client %s failure ACK = %+v, %v", test.name, ack, err)
			}
			awaitMeshRegistryEmpty(t, server, "client initial delta rollback")
			if groups, size := server.declarations.snapshot(); groups != 0 || size != 0 {
				t.Fatalf("client delta retained declarations %d/%d", groups, size)
			}
			if current, _, _ := server.declarations.workSnapshot(); current != 0 {
				t.Fatalf("client delta retained %d work bytes", current)
			}
			server.paths.mu.Lock()
			count, size := server.paths.count, server.paths.bytes
			server.paths.mu.Unlock()
			if count != 0 || size != 0 {
				t.Fatalf("client delta retained paths %d/%d", count, size)
			}
		})
	}
}

func TestMeshValidationSlotCancellationReleasesCapacity(t *testing.T) {
	declaration := testDeclaration(t, "api", 1, 0)
	ledger := newDeclarationLedger(testControlLimits(int64(len(declaration))))
	digest := sha256.Sum256(declaration)
	meshValidationSlot <- struct{}{}
	claim, err := ledger.begin(uint32(len(declaration)), digest[:])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := claim.finish(ctx, bytes.Clone(declaration), "api"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled validation wait = %v", err)
	}
	claim.close()
	<-meshValidationSlot
	claim, err = ledger.begin(uint32(len(declaration)), digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claim.finish(ctx, bytes.Clone(declaration), "api"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled validation with available slot = %v", err)
	}
	claim.close()
	record := retainTestDeclaration(t, ledger, declaration)
	ledger.release(record)
}

func TestMeshConcurrentRawAndValidationWorkPeak(t *testing.T) {
	declaration := testDeclaration(t, "api", 1, 0)
	limits := testControlLimits(2 * int64(len(declaration)))
	limits.MaxGroups = 2
	limits.MaxGroupDeclarationBytes = int64(len(declaration))
	ledger := newDeclarationLedger(limits)
	digest := sha256.Sum256(declaration)
	claims := make([]*declarationClaim, 2)
	raw := make([][]byte, 2)
	for i := range claims {
		var err error
		claims[i], err = ledger.begin(uint32(len(declaration)), digest[:])
		if err != nil {
			t.Fatal(err)
		}
		raw[i] = make([]byte, len(declaration))
		copy(raw[i], declaration)
		claims[i].trackInput(raw[i], digest[:])
	}
	baseline, _, _ := ledger.workSnapshot()
	record, err := claims[0].finish(context.Background(), raw[0], "api")
	if err != nil {
		t.Fatal(err)
	}
	claims[0].close()
	if _, err := claims[1].finish(context.Background(), raw[1], "different-group"); err == nil {
		t.Fatal("mismatched registration group accepted")
	}
	claims[1].close()
	ledger.release(record)
	if current, combinedPeak, validationPeak := ledger.workSnapshot(); current != 0 || combinedPeak <= baseline || validationPeak < int64(len(declaration)) {
		t.Fatalf("work current %d, combined peak %d, raw baseline %d, validation peak %d", current, combinedPeak, baseline, validationPeak)
	}
	if groups, size := ledger.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("concurrent validation retained %d/%d", groups, size)
	}
}

func TestMeshDeltaCancellationReleasesRawBacking(t *testing.T) {
	declaration := testDeclaration(t, "api", 1, 0)
	limits := testControlLimits(int64(len(declaration)))
	ledger := newDeclarationLedger(limits)
	state := &stagedState{ledger: ledger, paths: newPathBudget(limits)}
	applier := newDeltaApplier(context.Background(), state, ledger, state.paths, limits, 0)
	digest := sha256.Sum256(declaration)
	if err := applier.apply(protocol.MeshChunk{
		Sequence: 1, Total: uint32(len(declaration)), Digest: digest[:],
		Data: declaration[:len(declaration)/2],
	}); err != nil {
		t.Fatal(err)
	}
	if current, _, _ := ledger.workSnapshot(); current < int64(len(declaration)) {
		t.Fatalf("partial delta raw backing = %d", current)
	}
	applier.close()
	if current, _, _ := ledger.workSnapshot(); current != 0 {
		t.Fatalf("canceled delta retained %d work bytes", current)
	}
	if groups, size := ledger.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("canceled delta retained %d/%d", groups, size)
	}
}

type snapshotHookWriter struct {
	bytes.Buffer
	onFirst sync.Once
	hook    func()
}

func (w *snapshotHookWriter) Write(data []byte) (int, error) {
	n, err := w.Buffer.Write(data)
	w.onFirst.Do(w.hook)
	return n, err
}

func TestMeshPeerNonemptySnapshotAndRegistrationDeltas(t *testing.T) {
	declaration := testDeclaration(t, "api", 1, 1)
	limits := testControlLimits(int64(len(declaration)))
	senderLedger := newDeclarationLedger(limits)
	state := newControlState(senderLedger, limits)
	if err := state.publishGroup(retainTestDeclaration(t, senderLedger, declaration)); err != nil {
		t.Fatal(err)
	}
	path := protocol.MeshPath{PathID: "first", GroupID: "api", RuleVersion: 1,
		TerminalServerID: "edge-a", Servers: []string{"edge-a"}, Generation: 1}
	if err := state.publishPath(path); err != nil {
		t.Fatal(err)
	}
	snapshot := state.subscribe(func() {})
	wire := &snapshotHookWriter{hook: func() {
		if err := state.withdrawPath("first"); err != nil {
			t.Error(err)
		}
		path.PathID = "second"
		if err := state.publishPath(path); err != nil {
			t.Error(err)
		}
	}}
	if err := sendPeerSnapshot(context.Background(), wire, snapshot); err != nil {
		t.Fatal(err)
	}
	state.unsubscribe(snapshot.link)
	receiverLedger := newDeclarationLedger(limits)
	received, err := receiveInitial(context.Background(), bytes.NewReader(wire.Bytes()), receiverLedger, newPathBudget(limits), limits, protocol.MeshRolePeer, "")
	if err != nil {
		t.Fatal(err)
	}
	if received.revision != state.fence() || len(received.groups) != 1 || len(received.items) != 1 || received.items[0].id != "second" {
		t.Fatalf("received snapshot/delta = revision %d groups %d paths %+v", received.revision, len(received.groups), received.items)
	}
	if current, combinedPeak, parsePeak := receiverLedger.workSnapshot(); current != 0 || combinedPeak < int64(len(declaration)) || parsePeak < int64(len(declaration)) {
		t.Fatalf("received declaration work = current %d, combined peak %d, parse peak %d", current, combinedPeak, parsePeak)
	}
	received.close()
	state.close()
	if groups, size := receiverLedger.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("receiver declaration leak = %d/%d", groups, size)
	}
	if groups, size := senderLedger.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("sender declaration leak = %d/%d", groups, size)
	}
}

func TestMeshControlQueueCountsInFlightAndFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name        string
		maxMessages int
		maxBytes    int64
	}{
		{"message count", 1, 16},
		{"backing bytes", 2, 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := testControlLimits(1024)
			limits.MaxControlQueueMessages = test.maxMessages
			limits.MaxControlQueueBytes = test.maxBytes
			queue := newControlQueue(limits)
			first := queuedControlFrame{data: slices.Repeat([]byte{1}, 8)}
			if err := queue.push([]queuedControlFrame{first}); err != nil {
				t.Fatal(err)
			}
			held, err := queue.take(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := queue.push([]queuedControlFrame{{data: []byte{2}}}); !errors.Is(err, errMeshControlQueueFull) {
				t.Fatalf("in-flight queue overflow = %v", err)
			}
			if count, size := queue.stats(); count != 1 || size != 8 {
				t.Fatalf("failed queue budget = %d/%d", count, size)
			}
			if _, err := queue.take(context.Background()); !errors.Is(err, errMeshControlQueueFull) {
				t.Fatalf("failed queue take = %v", err)
			}
			queue.done(held)
			queue.clear()
			if count, size := queue.stats(); count != 0 || size != 0 {
				t.Fatalf("closed queue budget = %d/%d", count, size)
			}
		})
	}
}

func TestMeshPeerQueueFullClosesExactAndRecoversSnapshot(t *testing.T) {
	files := testMeshMaterial(t)
	declaration := testLargeDeclaration(t, files, "api")
	for _, test := range []struct {
		name        string
		maxMessages int
		maxBytes    int64
	}{
		{"message count", 1, 1 << 20},
		{"backing bytes", 16, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			addressA := reserveMeshUDPAddress(t)
			addressB := reserveMeshUDPAddress(t)
			cfgA := testMeshServerConfig("edge-a", addressA, config.ClientAuthMethodToken, files,
				[]config.MeshPeer{{ServerID: "edge-b", Address: addressB, ServerName: "localhost"}}, 1)
			cfgA.Limits.MaxControlQueueMessages = test.maxMessages
			cfgA.Limits.MaxControlQueueBytes = test.maxBytes
			a, err := NewServer(cfgA)
			if err != nil {
				t.Fatal(err)
			}
			a.reconnectDelay = func(int) time.Duration { return 20 * time.Millisecond }
			b, err := NewServer(testMeshServerConfig("edge-b", addressB, config.ClientAuthMethodToken, files,
				[]config.MeshPeer{{ServerID: "edge-a"}}, 1))
			if err != nil {
				t.Fatal(err)
			}
			doneB := startMeshServer(t, b)
			doneA := startMeshServer(t, a)
			old := awaitMeshSession(t, a.Sessions(), "peer before queue overflow")
			awaitMeshSession(t, b.Sessions(), "receiver before queue overflow")
			if err := a.controlState.publishGroup(retainTestDeclaration(t, a.declarations, declaration)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-old.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("queue overflow did not close the exact peer session")
			}
			if count, size := old.outbound.stats(); count != 0 || size != 0 {
				t.Fatalf("retired peer queue retained %d/%d", count, size)
			}
			newSender := awaitMeshSession(t, a.Sessions(), "peer after queue overflow")
			newReceiver := awaitMeshSession(t, b.Sessions(), "recovered peer snapshot")
			if newSender == old || newSender.Connection() == old.Connection() || newReceiver.staged == nil ||
				len(newReceiver.staged.groups) != 1 || !bytes.Equal(newReceiver.staged.groups[0].bytes, declaration) ||
				b.registry.GroupAvailability("api") {
				t.Fatal("queue overflow did not recover the full staged snapshot on a new exact session")
			}
			stopMeshServer(t, b, doneB)
			stopMeshServer(t, a, doneA)
			if groups, size := b.declarations.snapshot(); groups != 0 || size != 0 {
				t.Fatalf("recovered peer retained %d/%d", groups, size)
			}
		})
	}
}

func TestMeshPathBackingCountsIdentityAndSnapshotReferences(t *testing.T) {
	declaration := testDeclaration(t, "api", 1, 0)
	limits := testControlLimits(int64(len(declaration)))
	limits.MaxTotalPaths = 1
	limits.MaxPathsPerGroup = 1
	ledger := newDeclarationLedger(limits)
	budget := newPathBudget(limits)
	staged := &stagedState{ledger: ledger, paths: budget,
		groups: []*groupRecord{retainTestDeclaration(t, ledger, declaration)}}
	path := protocol.MeshPath{PathID: strings.Repeat("p", 3500), GroupID: "api", RuleVersion: 1,
		TerminalServerID: "edge-a", Servers: []string{"edge-a"}, Generation: 1}
	frame, err := protocol.MarshalMeshControlFrame(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) > 5+protocol.MaxControlPayloadSize {
		t.Fatal("path fixture exceeds the frame limit")
	}
	if err := staged.addPath(path, limits, true); err == nil {
		t.Fatal("path identity backing escaped the temporary byte limit")
	}
	budget.mu.Lock()
	count, size := budget.count, budget.bytes
	budget.mu.Unlock()
	if count != 0 || size != 0 {
		t.Fatalf("rejected staged path retained %d/%d", count, size)
	}
	path.PathID = "path-a"
	if err := staged.addPath(path, limits, true); err != nil {
		t.Fatal(err)
	}
	budget.mu.Lock()
	count, size = budget.count, budget.bytes
	budget.mu.Unlock()
	if count != 1 || size != staged.items[0].backing || size <= int64(cap(staged.items[0].frame)) {
		t.Fatalf("staged identity backing = %d/%d", count, size)
	}
	staged.close()
	budget.mu.Lock()
	count, size = budget.count, budget.bytes
	budget.mu.Unlock()
	if count != 0 || size != 0 {
		t.Fatalf("staged path close retained %d/%d", count, size)
	}

	state := newControlState(ledger, limits)
	if err := state.publishGroup(retainTestDeclaration(t, ledger, declaration)); err != nil {
		t.Fatal(err)
	}
	if err := state.publishPath(path); err != nil {
		t.Fatal(err)
	}
	snapshot := state.subscribe(func() {})
	state.mu.Lock()
	retained := state.pathBytes
	item := state.paths[path.PathID]
	state.mu.Unlock()
	if item == nil || retained != item.backing || retained <= int64(cap(item.frame)) {
		t.Fatalf("published identity backing = %d, item %+v", retained, item)
	}
	if err := state.withdrawPath(path.PathID); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	count, size = state.pathRecords, state.pathBytes
	state.mu.Unlock()
	if count != 1 || size != retained {
		t.Fatalf("withdrawal released pinned snapshot path = %d/%d", count, size)
	}
	snapshot.release()
	state.unsubscribe(snapshot.link)
	state.mu.Lock()
	count, size = state.pathRecords, state.pathBytes
	state.mu.Unlock()
	if count != 0 || size != 0 {
		t.Fatalf("snapshot path release retained %d/%d", count, size)
	}
	state.close()
	if groups, size := ledger.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("path test retained declaration %d/%d", groups, size)
	}
}

func TestMeshTypedNewClientSendsFrozenDeclaration(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	server, err := NewServer(testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1))
	if err != nil {
		t.Fatal(err)
	}
	declarations := make(chan []byte, 1)
	server.beforeSuccessAck = func(_ protocol.MeshRegister, _ *quic.Conn) {
		server.declarations.mu.Lock()
		for _, record := range server.declarations.records {
			select {
			case declarations <- bytes.Clone(record.bytes):
			default:
			}
		}
		server.declarations.mu.Unlock()
	}
	startMeshServer(t, server)
	conf := testMeshClientConfig("instance-a", "api", config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: address, ServerName: "localhost"}})
	conf.Group.Routes.HTTP = []config.MeshHTTPRoute{{
		Hostnames: []string{"z.example.com", "a.example.com"},
		Matches: []config.MeshHTTPMatch{{
			Path:    &config.MeshHTTPPathMatch{Value: "/v1"},
			Headers: []config.MeshHTTPHeaderMatch{{Name: "X-Test", Value: "before"}},
		}},
	}}
	before := config.CloneMeshClientConfig(conf)
	client, err := NewClient(conf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*conf, before) {
		t.Fatal("NewClient modified its typed caller configuration")
	}
	expected := bytes.Clone(client.declaration)
	conf.Group.Routes.HTTP[0].Hostnames[0] = "changed.example.com"
	conf.Group.Routes.HTTP[0].Matches[0].Path.Value = "/changed"
	conf.Group.Routes.HTTP[0].Matches[0].Headers[0].Value = "changed"
	startMeshClient(t, client)
	select {
	case got := <-declarations:
		if !bytes.Equal(got, expected) {
			t.Fatalf("received declaration changed after caller mutation:\n%s\n%s", got, expected)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive the frozen declaration")
	}
	awaitMeshSession(t, server.Sessions(), "server staged typed client")
	server.registry.mu.Lock()
	entry := server.registry.clients["instance-a"]
	if entry == nil || entry.current.Empty() {
		server.registry.mu.Unlock()
		t.Fatal("typed client did not publish a control session")
	}
	ready := entry.current.Load().forwarding
	server.registry.mu.Unlock()
	if !ready.SessionReady || ready.DeclarationReady || ready.VersionEligible || server.registry.GroupAvailability("api") {
		t.Fatalf("staged declaration activated forwarding: %+v", ready)
	}
}

func TestMeshClientLargeInitialRealQUIC(t *testing.T) {
	files := testMeshMaterial(t)
	declaration := testLargeDeclaration(t, files, "api")
	var wire bytes.Buffer
	if err := sendClientInitial(&wire, declaration); err != nil {
		t.Fatal(err)
	}
	initial := readMeshInitialWire(t, &wire)
	if initial.begin != (protocol.MeshBegin{Groups: 1, GroupBytes: uint64(len(declaration))}) ||
		initial.end != (protocol.MeshEnd{Groups: 1, GroupBytes: uint64(len(declaration))}) ||
		initial.chunks < 2 || len(initial.groups) != 1 || !bytes.Equal(initial.groups[0], declaration) || wire.Len() != 0 {
		t.Fatalf("large client wire = begin %+v, end %+v, chunks %d, groups %d, remaining %d",
			initial.begin, initial.end, initial.chunks, len(initial.groups), wire.Len())
	}
	address := reserveMeshUDPAddress(t)
	server, err := NewServer(testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1))
	if err != nil {
		t.Fatal(err)
	}
	startMeshServer(t, server)
	registration := protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
		TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "api",
	}
	conn, _, ack, err := rawMeshRegisterOpenWithInitial(t, address, files, registration, false, nil, nil,
		func(stream *quic.Stream) error { return sendClientInitial(stream, declaration) })
	if err != nil || !ack.Success || ack.State != protocol.MeshStateStaged {
		t.Fatalf("large client registration ACK = %+v, %v", ack, err)
	}
	defer func() { _ = conn.CloseWithError(0, "test complete") }()
	awaitMeshSession(t, server.Sessions(), "large client declaration")
	server.declarations.mu.Lock()
	var received []byte
	for _, record := range server.declarations.records {
		received = bytes.Clone(record.bytes)
	}
	server.declarations.mu.Unlock()
	if !bytes.Equal(received, declaration) || server.registry.GroupAvailability("api") {
		t.Fatal("large client declaration changed or became eligible")
	}
	_ = conn.CloseWithError(0, "test complete")
	awaitMeshRegistryEmpty(t, server, "large client release")
	if groups, size := server.declarations.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("large client retained declarations %d/%d", groups, size)
	}

	updated := testDeclaration(t, "api", 2, 0)
	conn, _, ack, err = rawMeshRegisterOpenWithInitial(t, address, files, registration, false, nil, nil,
		func(stream *quic.Stream) error { return sendClientInitial(stream, updated) })
	if err != nil || !ack.Success || ack.State != protocol.MeshStateStaged {
		t.Fatalf("same-group new rule_version ACK = %+v, %v", ack, err)
	}
	defer func() { _ = conn.CloseWithError(0, "test complete") }()
	awaitMeshSession(t, server.Sessions(), "new rule_version declaration")
}

func TestMeshACKRetainsCanonicalWithoutValidationWork(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	server, err := NewServer(testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1))
	if err != nil {
		t.Fatal(err)
	}
	startMeshServer(t, server)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	registration := protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
		TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "api",
	}
	conn, _, ack, err := rawMeshRegisterOpen(t, address, files, registration, false, nil)
	if err != nil || !ack.Success || ack.State != protocol.MeshStateStaged {
		t.Fatalf("canonical registration ACK = %+v, %v", ack, err)
	}
	defer func() { _ = conn.CloseWithError(0, "test complete") }()
	awaitMeshSession(t, server.Sessions(), "ACK heap retention")
	runtime.GC()
	runtime.ReadMemStats(&after)
	heapGrowth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	groups, retained := server.declarations.snapshot()
	current, combinedPeak, parsePeak := server.declarations.workSnapshot()
	t.Logf("post_ACK_heap_growth=%d canonical_retained=%d combined_work_peak=%d parser_byte_peak=%d", heapGrowth, retained, combinedPeak, parsePeak)
	if groups != 1 || retained != int64(len(testDeclaration(t, "api", 1, 0))) || current != 0 || heapGrowth > 64<<20 {
		t.Fatalf("ACK retention = groups %d, canonical %d, work %d, heap growth %d", groups, retained, current, heapGrowth)
	}
	_ = conn.CloseWithError(0, "test complete")
	awaitMeshRegistryEmpty(t, server, "ACK retention release")
	if groups, retained := server.declarations.snapshot(); groups != 0 || retained != 0 {
		t.Fatalf("retired ACK retained %d/%d", groups, retained)
	}
}

func TestMeshInboundHeaderAndInitialDeadlines(t *testing.T) {
	files := testMeshMaterial(t)
	t.Run("silent unauthenticated header", func(t *testing.T) {
		address := reserveMeshUDPAddress(t)
		server, err := NewServer(testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1))
		if err != nil {
			t.Fatal(err)
		}
		server.registrationTimeout = 120 * time.Millisecond
		server.initialTimeout = 2 * time.Second
		startMeshServer(t, server)
		registration := protocol.MeshRegister{
			Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
			TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "api",
		}
		conn, _, ack, registerErr := rawMeshRegisterOpen(t, address, files, registration, false,
			func(*quic.Conn) {
				awaitMeshCondition(t, "silent pending registration", func() bool {
					return server.Snapshot().Registry.Pending == 1
				})
				time.Sleep(240 * time.Millisecond)
			})
		if conn != nil {
			defer func() { _ = conn.CloseWithError(0, "test complete") }()
		}
		if registerErr == nil && ack.Success {
			t.Fatalf("silent registration exceeded header deadline: %+v", ack)
		}
		awaitMeshRegistryEmpty(t, server, "silent header deadline")
	})
	for _, test := range []struct {
		name       string
		initial    time.Duration
		wait       time.Duration
		wantAccept bool
	}{
		{"authenticated initial outlives header", 2 * time.Second, 240 * time.Millisecond, true},
		{"absolute initial deadline", 400 * time.Millisecond, 550 * time.Millisecond, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			address := reserveMeshUDPAddress(t)
			server, err := NewServer(testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1))
			if err != nil {
				t.Fatal(err)
			}
			server.registrationTimeout = 120 * time.Millisecond
			server.initialTimeout = test.initial
			startMeshServer(t, server)
			registration := protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
				TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "api",
			}
			conn, _, ack, registerErr := rawMeshRegisterOpen(t, address, files, registration, false, nil,
				func(*quic.Stream) {
					awaitMeshCondition(t, "authenticated prepared client", func() bool {
						return server.Snapshot().Registry.ClientPrepared == 1
					})
					time.Sleep(test.wait)
				})
			if conn != nil {
				defer func() { _ = conn.CloseWithError(0, "test complete") }()
			}
			if test.wantAccept {
				if registerErr != nil || !ack.Success || ack.State != protocol.MeshStateStaged {
					t.Fatalf("authenticated initial after header deadline = %+v, %v", ack, registerErr)
				}
				awaitMeshSession(t, server.Sessions(), "client after header deadline")
			} else {
				if registerErr == nil && ack.Success {
					t.Fatalf("expired total deadline accepted: %+v", ack)
				}
				awaitMeshRegistryEmpty(t, server, "expired initial deadline")
				if current, _, _ := server.declarations.workSnapshot(); current != 0 {
					t.Fatalf("expired initial retained %d work bytes", current)
				}
			}
		})
	}
}

func TestMeshFailureACKUsesRemainingRegistrationDeadline(t *testing.T) {
	files := testMeshMaterial(t)
	for _, test := range []struct {
		name  string
		total time.Duration
		min   time.Duration
		max   time.Duration
	}{
		{"one second cap", 3 * time.Second, 700 * time.Millisecond, 1700 * time.Millisecond},
		{"remaining total", 450 * time.Millisecond, 100 * time.Millisecond, 800 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			address := reserveMeshUDPAddress(t)
			server, err := NewServer(testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1))
			if err != nil {
				t.Fatal(err)
			}
			server.initialTimeout = test.total
			rejected := make(chan time.Time, 1)
			server.beforeReject = func(protocol.MeshRegister, error) { rejected <- time.Now() }
			startMeshServer(t, server)
			registration := protocol.MeshRegister{
				Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
				TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "api",
			}
			conn, _, ack, err := rawMeshRegisterOpenWithInitial(t, address, files, registration, false, nil, nil,
				func(stream *quic.Stream) error { return protocol.WriteMeshControl(stream, protocol.MeshEnd{}) })
			if conn != nil {
				defer func() { _ = conn.CloseWithError(0, "test complete") }()
			}
			if err != nil || ack.Success {
				t.Fatalf("failure ACK = %+v, %v", ack, err)
			}
			started := <-rejected
			awaitMeshRegistryEmpty(t, server, "failure ACK deadline")
			if elapsed := time.Since(started); elapsed < test.min || elapsed > test.max {
				t.Fatalf("failure ACK cleanup after %s, want %s..%s", elapsed, test.min, test.max)
			}
		})
	}
}

func TestMeshInboundSuccessClearsRegistrationDeadline(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	cfg := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1)
	cfg.Tunnel.HealthTimeout = 2 * time.Second
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server.initialTimeout = 500 * time.Millisecond
	startMeshServer(t, server)
	registration := protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
		TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "api",
	}
	conn, _, ack, err := rawMeshRegisterOpen(t, address, files, registration, false, nil)
	if err != nil || !ack.Success || ack.State != protocol.MeshStateStaged {
		t.Fatalf("inbound registration ACK = %+v, %v", ack, err)
	}
	defer func() { _ = conn.CloseWithError(0, "test complete") }()
	session := awaitMeshSession(t, server.Sessions(), "inbound registration deadline")
	select {
	case <-session.Done():
		t.Fatalf("inbound session retained registration deadline: %v", session.Err())
	case <-time.After(750 * time.Millisecond):
	}
	if snapshot := server.Snapshot().Registry; snapshot.ClientCurrent != 1 {
		t.Fatalf("inbound current after registration deadline = %+v", snapshot)
	}
	_ = conn.CloseWithError(0, "test complete")
	awaitMeshRegistryEmpty(t, server, "inbound deadline release")
}

func TestMeshPeerRegistrationBlockedWriteStopsAtTotalDeadline(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	cfg := testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-b"}}, 1)
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	clientCfg := testMeshClientConfig("instance-a", "api", config.ClientAuthMethodToken, files,
		[]config.MeshServerEndpoint{{ServerID: "edge-a", Address: address, ServerName: "localhost"}})
	for route := range 8 {
		var headers []config.MeshHTTPHeaderMatch
		for header := range 16 {
			headers = append(headers, config.MeshHTTPHeaderMatch{
				Name: fmt.Sprintf("X-%02d", header), Value: strings.Repeat("x", 3000),
			})
		}
		clientCfg.Group.Routes.HTTP = append(clientCfg.Group.Routes.HTTP, config.MeshHTTPRoute{
			Hostnames: []string{fmt.Sprintf("route-%d.example.com", route)},
			Matches:   []config.MeshHTTPMatch{{Headers: headers}},
		})
	}
	if err := config.FinalizeMeshClientConfig(clientCfg); err != nil {
		t.Fatal(err)
	}
	declaration := clientCfg.Group.CanonicalBytes()
	quicConfig := config.Quic{}.GetConfig()
	quicConfig.InitialStreamReceiveWindow = 1024
	quicConfig.MaxStreamReceiveWindow = 1024
	quicConfig.InitialConnectionReceiveWindow = 1024
	quicConfig.MaxConnectionReceiveWindow = 1024
	if len(declaration) <= 128<<10 || uint64(len(declaration)) <= quicConfig.MaxStreamReceiveWindow {
		t.Fatalf("blocked snapshot fixture is only %d bytes", len(declaration))
	}
	if err := server.controlState.publishGroup(retainTestDeclaration(t, server.declarations, declaration)); err != nil {
		t.Fatal(err)
	}
	server.initialTimeout = 350 * time.Millisecond
	type rejection struct {
		err error
		at  time.Time
	}
	rejected := make(chan rejection, 1)
	server.beforeReject = func(_ protocol.MeshRegister, err error) {
		rejected <- rejection{err: err, at: time.Now()}
	}
	startMeshServer(t, server)
	registration := protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRolePeer,
		TargetServerID: "edge-a", PeerServerID: "edge-b",
	}
	observed := errors.New("blocked snapshot writer rejection observed")
	var rejectedWrite rejection
	var sentAt time.Time
	conn, _, _, err := rawMeshRegisterOpenWithInitial(t, address, files, registration, false, nil, quicConfig,
		func(stream *quic.Stream) error {
			sentAt = time.Now()
			if err := sendPeerInitial(stream); err != nil {
				return err
			}
			select {
			case rejectedWrite = <-rejected:
				return observed
			case <-time.After(time.Second):
				return errors.New("blocked snapshot writer was not rejected")
			}
		})
	if conn != nil {
		defer func() { _ = conn.CloseWithError(0, "test complete") }()
	}
	if !errors.Is(err, observed) {
		t.Fatalf("blocked registration result = %v", err)
	}
	if !errors.Is(rejectedWrite.err, os.ErrDeadlineExceeded) ||
		strings.HasPrefix(rejectedWrite.err.Error(), "read header:") ||
		strings.HasPrefix(rejectedWrite.err.Error(), "read payload:") {
		t.Fatalf("blocked snapshot rejection = %v, want write deadline", rejectedWrite.err)
	}
	if elapsed := rejectedWrite.at.Sub(sentAt); elapsed > time.Second {
		t.Fatalf("blocked writer rejection took %s, want at most 1s", elapsed)
	}
	awaitMeshRegistryEmpty(t, server, "blocked registration write deadline")
	if groups, size := server.declarations.snapshot(); groups != 1 || size != int64(len(declaration)) {
		t.Fatalf("blocked registration retained %d/%d", groups, size)
	}
	if current, _, _ := server.declarations.workSnapshot(); current != 0 {
		t.Fatalf("blocked registration retained %d work bytes", current)
	}
}

func TestMeshAuthenticatedMalformedInitialGetsFailureACK(t *testing.T) {
	files := testMeshMaterial(t)
	address := reserveMeshUDPAddress(t)
	server, err := NewServer(testMeshServerConfig("edge-a", address, config.ClientAuthMethodToken, files, nil, 1))
	if err != nil {
		t.Fatal(err)
	}
	startMeshServer(t, server)
	registration := protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRoleClient,
		TargetServerID: "edge-a", InstanceID: "instance-a", GroupID: "api",
	}
	conn, _, ack, err := rawMeshRegisterOpen(t, address, files, registration, false, nil,
		func(stream *quic.Stream) {
			if err := protocol.WriteMeshControl(stream, protocol.MeshEnd{}); err != nil {
				t.Fatal(err)
			}
		})
	if conn != nil {
		defer func() { _ = conn.CloseWithError(0, "test complete") }()
	}
	if err != nil || ack.Success || !strings.Contains(ack.Message, "must begin with Begin") {
		t.Fatalf("malformed initial failure ACK = %+v, %v", ack, err)
	}
	awaitMeshRegistryEmpty(t, server, "malformed initial rollback")
	if groups, size := server.declarations.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("malformed initial retained %d/%d", groups, size)
	}
}

func TestMeshPeerBidirectionalNonemptyInitialExchange(t *testing.T) {
	files := testMeshMaterial(t)
	addressA := reserveMeshUDPAddress(t)
	addressB := reserveMeshUDPAddress(t)
	a, err := NewServer(testMeshServerConfig("edge-a", addressA, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-b", Address: addressB, ServerName: "localhost"}, {ServerID: "edge-c"}}, 2))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewServer(testMeshServerConfig("edge-b", addressB, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-a"}}, 1))
	if err != nil {
		t.Fatal(err)
	}
	declarations := make(map[string][]byte)
	for _, test := range []struct {
		server *Server
		groups [2]string
		paths  [2]string
		id     string
	}{
		{a, [2]string{"group-a-1", "group-a-2"}, [2]string{"path-a-1", "path-a-2"}, "edge-a"},
		{b, [2]string{"group-b-1", "group-b-2"}, [2]string{"path-b-1", "path-b-2"}, "edge-b"},
	} {
		var groupBytes uint64
		for i, groupID := range test.groups {
			declaration := testLargeDeclaration(t, files, groupID)
			declarations[groupID] = declaration
			groupBytes += uint64(len(declaration))
			if err := test.server.controlState.publishGroup(retainTestDeclaration(t, test.server.declarations, declaration)); err != nil {
				t.Fatal(err)
			}
			if err := test.server.controlState.publishPath(protocol.MeshPath{
				PathID: test.paths[i], GroupID: groupID, RuleVersion: 1,
				TerminalServerID: test.id, Servers: []string{test.id}, Generation: 1,
			}); err != nil {
				t.Fatal(err)
			}
		}
		snapshot := test.server.controlState.subscribe(func() {})
		var wire bytes.Buffer
		if err := sendPeerSnapshot(context.Background(), &wire, snapshot); err != nil {
			t.Fatal(err)
		}
		test.server.controlState.unsubscribe(snapshot.link)
		initial := readMeshInitialWire(t, &wire)
		if initial.begin != (protocol.MeshBegin{Revision: 4, Groups: 2, Paths: 2, GroupBytes: groupBytes}) ||
			initial.end != (protocol.MeshEnd{Groups: 2, Paths: 2, GroupBytes: groupBytes, FinalSequence: 4}) ||
			initial.chunks < 4 || len(initial.paths) != 2 || wire.Len() != 0 {
			t.Fatalf("%s peer wire = begin %+v, end %+v, chunks %d, paths %d, remaining %d",
				test.id, initial.begin, initial.end, initial.chunks, len(initial.paths), wire.Len())
		}
		for i, groupID := range test.groups {
			if !bytes.Equal(initial.groups[i], declarations[groupID]) || initial.paths[i].PathID != test.paths[i] {
				t.Fatalf("%s peer wire record %d changed", test.id, i)
			}
		}
	}
	startMeshServer(t, b)
	startMeshServer(t, a)
	for _, test := range []struct {
		server *Server
		groups [2]string
		paths  [2]string
	}{
		{a, [2]string{"group-b-1", "group-b-2"}, [2]string{"path-b-1", "path-b-2"}},
		{b, [2]string{"group-a-1", "group-a-2"}, [2]string{"path-a-1", "path-a-2"}},
	} {
		session := awaitMeshSession(t, test.server.Sessions(), "nonempty peer control session")
		if session.staged == nil || len(session.staged.groups) != 2 || len(session.staged.items) != 2 ||
			session.staged.revision != 4 {
			t.Fatalf("%s peer initial stage = %+v", test.server.config.ServerID, session.staged)
		}
		for i, groupID := range test.groups {
			if session.staged.groups[i].key.id != groupID ||
				!bytes.Equal(session.staged.groups[i].bytes, declarations[groupID]) ||
				session.staged.items[i].id != test.paths[i] || test.server.registry.GroupAvailability(groupID) {
				t.Fatalf("%s peer initial record %d changed or became eligible", test.server.config.ServerID, i)
			}
		}
	}
	registration := protocol.MeshRegister{
		Version: protocol.MeshProtocolVersion, Capabilities: protocol.MeshCapabilities(), Role: protocol.MeshRolePeer,
		TargetServerID: "edge-a", PeerServerID: "edge-c",
	}
	conn, _, ack, err := rawMeshRegisterOpen(t, addressA, files, registration, false, nil)
	if err != nil || !ack.Success || ack.State != protocol.MeshStateStaged {
		t.Fatalf("large peer snapshot ACK = %+v, %v", ack, err)
	}
	defer func() { _ = conn.CloseWithError(0, "test complete") }()
	awaitMeshSession(t, a.Sessions(), "large peer snapshot staged ACK")
}

func TestMeshPeerRuntimeDeltasRemainStaged(t *testing.T) {
	files := testMeshMaterial(t)
	addressA := reserveMeshUDPAddress(t)
	addressB := reserveMeshUDPAddress(t)
	a, err := NewServer(testMeshServerConfig("edge-a", addressA, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-b", Address: addressB, ServerName: "localhost"}}, 1))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewServer(testMeshServerConfig("edge-b", addressB, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-a"}}, 1))
	if err != nil {
		t.Fatal(err)
	}
	doneB := startMeshServer(t, b)
	doneA := startMeshServer(t, a)
	awaitMeshSession(t, a.Sessions(), "outbound peer before runtime delta")
	awaitMeshSession(t, b.Sessions(), "inbound peer before runtime delta")
	declaration := testDeclaration(t, "api", 1, 0)
	if err := a.controlState.publishGroup(retainTestDeclaration(t, a.declarations, declaration)); err != nil {
		t.Fatal(err)
	}
	awaitMeshCondition(t, "runtime group delta", func() bool {
		groups, _ := b.declarations.snapshot()
		return groups == 1
	})
	path := protocol.MeshPath{PathID: "path-a", GroupID: "api", RuleVersion: 1,
		TerminalServerID: "edge-a", Servers: []string{"edge-a"}, Generation: 1}
	if err := a.controlState.publishPath(path); err != nil {
		t.Fatal(err)
	}
	awaitMeshCondition(t, "runtime path delta", func() bool {
		b.paths.mu.Lock()
		defer b.paths.mu.Unlock()
		return b.paths.count == 1
	})
	if b.registry.GroupAvailability("api") {
		t.Fatal("staged runtime delta activated forwarding")
	}
	if err := a.controlState.withdrawPath("path-a"); err != nil {
		t.Fatal(err)
	}
	awaitMeshCondition(t, "runtime path withdrawal", func() bool {
		b.paths.mu.Lock()
		defer b.paths.mu.Unlock()
		return b.paths.count == 0
	})
	stopMeshServer(t, b, doneB)
	stopMeshServer(t, a, doneA)
	if groups, size := b.declarations.snapshot(); groups != 0 || size != 0 {
		t.Fatalf("runtime delta retirement retained %d/%d", groups, size)
	}
}

func TestMeshControlSlowWriteCannotCrossHeartbeat(t *testing.T) {
	now := time.Unix(100, 0)
	if got := meshDataWriteDeadline(now, now.Add(10*time.Second), now, 10*time.Second, 11*time.Second); !got.Equal(now.Add(10 * time.Second)) {
		t.Fatalf("10s/11s data write deadline = %v", got)
	}
	files := testMeshMaterial(t)
	addressA := reserveMeshUDPAddress(t)
	addressB := reserveMeshUDPAddress(t)
	configA := testMeshServerConfig("edge-a", addressA, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-b", Address: addressB, ServerName: "localhost"}}, 1)
	configB := testMeshServerConfig("edge-b", addressB, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-a"}}, 1)
	for _, cfg := range []*config.MeshServer{configA, configB} {
		cfg.Tunnel.HeartbeatInterval = 200 * time.Millisecond
		cfg.Tunnel.HealthTimeout = 300 * time.Millisecond
	}
	a, err := NewServer(configA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewServer(configB)
	if err != nil {
		t.Fatal(err)
	}
	startMeshServer(t, b)
	startMeshServer(t, a)
	session := awaitMeshSession(t, a.Sessions(), "slow-writer outbound peer")
	awaitMeshCondition(t, "peer writer start", session.ControlStarted)
	write := func(_ io.Writer, frame []byte) (int, error) {
		time.Sleep(250 * time.Millisecond)
		return len(frame), nil
	}
	session.controlWrite.Store(&write)
	declaration := testDeclaration(t, "api", 1, 0)
	if err := a.controlState.publishGroup(retainTestDeclaration(t, a.declarations, declaration)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("slow data write did not close exact session")
	}
	if err := session.Err(); err == nil || !strings.Contains(err.Error(), "heartbeat deadline") {
		t.Fatalf("slow write terminal error = %v", err)
	}
	if count, size := session.outbound.stats(); count != 0 || size != 0 {
		t.Fatalf("slow-write outbound queue leak = %d/%d", count, size)
	}
}

func TestMeshContinuousSuccessfulWritesYieldToHeartbeat(t *testing.T) {
	files := testMeshMaterial(t)
	addressA := reserveMeshUDPAddress(t)
	addressB := reserveMeshUDPAddress(t)
	cfgA := testMeshServerConfig("edge-a", addressA, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-b", Address: addressB, ServerName: "localhost"}}, 1)
	cfgB := testMeshServerConfig("edge-b", addressB, config.ClientAuthMethodToken, files,
		[]config.MeshPeer{{ServerID: "edge-a"}}, 1)
	for _, cfg := range []*config.MeshServer{cfgA, cfgB} {
		cfg.Tunnel.HeartbeatInterval = 500 * time.Millisecond
		cfg.Tunnel.HealthTimeout = 2 * time.Second
	}
	a, err := NewServer(cfgA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewServer(cfgB)
	if err != nil {
		t.Fatal(err)
	}
	startMeshServer(t, b)
	startMeshServer(t, a)
	sender := awaitMeshSession(t, a.Sessions(), "successful slow writer")
	awaitMeshSession(t, b.Sessions(), "successful slow write receiver")
	declaration := testDeclaration(t, "api", 1, 0)
	if err := a.controlState.publishGroup(retainTestDeclaration(t, a.declarations, declaration)); err != nil {
		t.Fatal(err)
	}
	awaitMeshCondition(t, "group before slow writes", func() bool {
		groups, _ := b.declarations.snapshot()
		return groups == 1
	})
	var writes atomic.Int32
	startWrites := make(chan struct{})
	resumeWrites := sync.OnceFunc(func() { close(startWrites) })
	defer resumeWrites()
	firstWritten := make(chan struct{})
	write := func(w io.Writer, frame []byte) (int, error) {
		<-startWrites
		time.Sleep(20 * time.Millisecond)
		n, err := w.Write(frame)
		if err == nil && n == len(frame) {
			if writes.Add(1) == 1 {
				// Keep later frames queued until the next scheduler decision is overdue.
				sender.outbound.mu.Lock()
				close(firstWritten)
			}
		}
		return n, err
	}
	sender.controlWrite.Store(&write)
	heartbeats := make(chan int32, 8)
	releaseBaseline := make(chan struct{})
	resumeBaseline := sync.OnceFunc(func() { close(releaseBaseline) })
	defer resumeBaseline()
	baseline := true
	heartbeat := func(w io.Writer, timestamp int64) error {
		err := protocol.WriteHeartbeat(w, timestamp)
		if err == nil {
			select {
			case heartbeats <- writes.Load():
			default:
			}
			if baseline {
				baseline = false
				<-releaseBaseline
			}
		}
		return err
	}
	sender.heartbeatWrite.Store(&heartbeat)
	select {
	case <-heartbeats:
	case <-time.After(2 * time.Second):
		t.Fatal("peer heartbeat baseline not observed")
	}
	const frames = 5
	for i := range frames {
		if err := a.controlState.publishPath(protocol.MeshPath{
			PathID: fmt.Sprintf("path-%02d", i), GroupID: "api", RuleVersion: 1,
			TerminalServerID: "edge-a", Servers: []string{"edge-a"}, Generation: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	resumeBaseline()
	resumeWrites()
	select {
	case <-firstWritten:
	case <-time.After(time.Second):
		t.Fatal("first slow control write did not complete")
	}
	time.Sleep(cfgA.Tunnel.HeartbeatInterval)
	sender.outbound.mu.Unlock()
	select {
	case beforeHeartbeat := <-heartbeats:
		if beforeHeartbeat != 1 {
			t.Fatalf("due heartbeat ran after %d of %d successful frames", beforeHeartbeat, frames)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("due heartbeat was not written during successful frame batch")
	}
	awaitMeshCondition(t, "all successful control writes", func() bool { return writes.Load() == frames })
	awaitMeshCondition(t, "all slow-write paths received", func() bool {
		b.paths.mu.Lock()
		defer b.paths.mu.Unlock()
		return b.paths.count == frames
	})
	select {
	case <-sender.Done():
		t.Fatalf("successful slow writes closed the peer session: %v", sender.Err())
	default:
	}
}
