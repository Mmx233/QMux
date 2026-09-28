package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMeshDefaultsAndLimitsSchema(t *testing.T) {
	server := validMeshServer()
	if server.Tunnel.HeartbeatInterval != DefaultHeartbeatInterval ||
		server.Tunnel.HealthTimeout != DefaultHealthTimeout ||
		server.Tunnel.TCPCopyBufferSize != DefaultTCPCopyBufferSize ||
		server.LoadBalancer != DefaultLoadBalancer ||
		server.RoutingPolicy != MeshRoutingPolicyShortestPathFirst {
		t.Fatalf("server defaults = %+v", server)
	}
	wantScheduler := MeshProbeScheduler{10 * time.Second, 3 * time.Second, 3, 2, 64, 1024}
	if server.ProbeScheduler != wantScheduler {
		t.Fatalf("probe scheduler defaults = %+v, want %+v", server.ProbeScheduler, wantScheduler)
	}
	wantLimits := MeshServerLimits{
		MaxClientGenerations:          16,
		MaxPendingRegistrations:       128,
		MaxPeers:                      32,
		MaxGroups:                     1024,
		MaxGroupDeclarationBytes:      1 << 20,
		MaxTotalGroupDeclarationBytes: 64 << 20,
		MaxPathsPerGroup:              256,
		MaxTotalPaths:                 16384,
		MaxPathHops:                   16,
		MaxControlQueueMessages:       1024,
		MaxControlQueueBytes:          16 << 20,
	}
	if server.Limits != wantLimits {
		t.Fatalf("server limits defaults = %+v, want %+v", server.Limits, wantLimits)
	}
	wantCapacity := MeshTCPCapacity{128, 128, 100, 16}
	if server.Tunnel.Capacity != wantCapacity || server.Ingress.Listeners[0].Capacity != wantCapacity {
		t.Fatalf("source capacity defaults = tunnel %+v, ingress %+v", server.Tunnel.Capacity, server.Ingress.Listeners[0].Capacity)
	}
	if got := server.Ingress.Listeners[0].MaxInflightRequests; got == nil || *got != 128 {
		t.Fatalf("HTTP in-flight default = %v", got)
	}

	client := validMeshClient()
	if client.Tunnel.Auth.Method != ClientAuthMethodToken ||
		client.Tunnel.HeartbeatInterval != DefaultHeartbeatInterval ||
		client.Tunnel.HealthTimeout != DefaultHealthTimeout ||
		client.Tunnel.TCPCopyBufferSize != DefaultTCPCopyBufferSize ||
		client.Group.OutdatedClientPolicy != MeshOutdatedClientPolicyApplyLatestRules ||
		client.Group.Probe.Type != MeshProbeTypeTCP {
		t.Fatalf("client defaults = %+v", client)
	}

	limitsType := reflect.TypeFor[MeshServerLimits]()
	wantKeys := []string{
		"max_client_generations", "max_pending_registrations", "max_peers", "max_groups",
		"max_group_declaration_bytes", "max_total_group_declaration_bytes",
		"max_paths_per_group", "max_total_paths", "max_path_hops",
		"max_control_queue_messages", "max_control_queue_bytes",
	}
	gotKeys := make([]string, 0, limitsType.NumField())
	for field := range limitsType.Fields() {
		gotKeys = append(gotKeys, field.Tag.Get("yaml"))
	}
	if !slices.Equal(gotKeys, wantKeys) {
		t.Fatalf("mesh limits YAML keys = %q, want %q", gotKeys, wantKeys)
	}
	capacityType := reflect.TypeFor[MeshTCPCapacity]()
	wantCapacityKeys := []string{"max_tcp_connections", "max_pending_tcp_setups", "max_tcp_connections_per_generation", "max_pending_tcp_setups_per_generation"}
	for i := range capacityType.NumField() {
		field := capacityType.Field(i)
		if field.Tag.Get("yaml") != wantCapacityKeys[i] {
			t.Fatalf("TCP capacity field %d = %q, want %q", i, field.Tag.Get("yaml"), wantCapacityKeys[i])
		}
	}
}

func TestMeshTypedLoadersStrictness(t *testing.T) {
	tests := []struct {
		name    string
		content string
		load    func(string) error
		want    string
	}{
		{"server unknown field", validMeshServerYAML() + "unknown: true\n", loadMeshServerError, "field unknown not found"},
		{"client unknown field", validMeshClientYAML() + "unknown: true\n", loadMeshClientError, "field unknown not found"},
		{"server UDP capacity key", strings.Replace(validMeshServerYAML(), "  max_peers: 2\n", "  max_peers: 2\n  max_udp_sessions: 2\n", 1), loadMeshServerError, "field max_udp_sessions not found"},
		{"tunnel UDP capacity key", strings.Replace(validMeshServerYAML(), "  peering:\n", "  capacity:\n    max_udp_sessions: 2\n  peering:\n", 1), loadMeshServerError, "field max_udp_sessions not found"},
		{"multiple server documents", validMeshServerYAML() + "---\nserver_id: other\n", loadMeshServerError, "multiple YAML documents"},
		{"multiple client documents", validMeshClientYAML() + "---\ninstance_id: other\n", loadMeshClientError, "multiple YAML documents"},
		{"client file as server", validMeshClientYAML(), loadMeshServerError, "field instance_id not found"},
		{"server file as client", validMeshServerYAML(), loadMeshClientError, "field server_id not found"},
		{"integer overflow", strings.Replace(validMeshServerYAML(), "  max_peers: 2", "  max_peers: 9223372036854775808", 1), loadMeshServerError, "parse config"},
	}
	for _, field := range []string{"max_tcp_connections", "max_pending_tcp_setups", "max_tcp_connections_per_generation", "max_pending_tcp_setups_per_generation"} {
		tests = append(tests, struct {
			name    string
			content string
			load    func(string) error
			want    string
		}{"old TCP limit " + field, strings.Replace(validMeshServerYAML(), "  max_peers: 2\n", "  max_peers: 2\n  "+field+": 2\n", 1), loadMeshServerError, "field " + field + " not found"})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.load(writeTestConfig(t, test.content)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("load error = %v, want %q", err, test.want)
			}
		})
	}

	server, err := LoadMeshServerConfig(writeTestConfig(t, validMeshServerYAML()))
	if err != nil {
		t.Fatalf("LoadMeshServerConfig: %v", err)
	}
	if server.Limits.MaxPeers != 2 || server.ProbeScheduler.Interval != defaultMeshProbeInterval {
		t.Fatalf("loaded server defaults/explicit values = %+v", server)
	}
	client, err := LoadMeshClientConfig(writeTestConfig(t, validMeshClientYAML()))
	if err != nil {
		t.Fatalf("LoadMeshClientConfig: %v", err)
	}
	if client.Group.OutdatedClientPolicy != MeshOutdatedClientPolicyApplyLatestRules || len(client.Group.CanonicalBytes()) == 0 {
		t.Fatalf("loaded client defaults/canonical = %+v", client.Group)
	}
}

func TestMeshSourceCapacityYAML(t *testing.T) {
	content := strings.Replace(validMeshServerYAML(), "  peering:\n", "  capacity:\n    max_tcp_connections: 1\n    max_pending_tcp_setups: 2\n    max_tcp_connections_per_generation: 3\n    max_pending_tcp_setups_per_generation: 4\n  peering:\n", 1)
	content = strings.Replace(content, "limits:\n", "ingress:\n  listeners:\n    - address: 127.0.0.1:8080\n      protocol: http\n      max_inflight_requests: 0\n      capacity:\n        max_tcp_connections: 4\n        max_pending_tcp_setups: 3\n        max_tcp_connections_per_generation: 2\n        max_pending_tcp_setups_per_generation: 1\nlimits:\n", 1)
	server, err := LoadMeshServerConfig(writeTestConfig(t, content))
	if err != nil {
		t.Fatalf("load source capacity: %v", err)
	}
	if got := server.Tunnel.Capacity; got != (MeshTCPCapacity{1, 2, 3, 4}) {
		t.Fatalf("tunnel capacity = %+v", got)
	}
	listener := server.Ingress.Listeners[0]
	if listener.Capacity != (MeshTCPCapacity{4, 3, 2, 1}) || listener.MaxInflightRequests == nil || *listener.MaxInflightRequests != 128 {
		t.Fatalf("listener capacity = %+v", listener)
	}
	content = strings.Replace(content, "protocol: http", "protocol: tls_passthrough", 1)
	assertMeshValidationError(t, loadMeshServerError(writeTestConfig(t, content)), "max_inflight_requests is not valid")
}

func TestMeshTypedLoadersRejectInvalidValues(t *testing.T) {
	serverTunnel := func(settings string) string {
		return strings.Replace(validMeshServerYAML(), "  peering:\n", settings+"  peering:\n", 1)
	}
	clientTunnel := func(settings string) string {
		return strings.Replace(validMeshClientYAML(), "  servers:\n", settings+"  servers:\n", 1)
	}
	serverTLS := func(settings string) string {
		return strings.Replace(validMeshServerYAML(), "      server_key_file: server-key.pem\n", "      server_key_file: server-key.pem\n"+settings, 1)
	}
	tests := []struct {
		name    string
		content string
		load    func(string) error
		want    string
	}{
		{"rule version", strings.Replace(validMeshClientYAML(), "  rule_version: 1", "  rule_version: 0", 1), loadMeshClientError, "group.rule_version must be at least 1"},
		{"outdated client policy", validMeshClientYAML() + "  outdated_client_policy: reject\n", loadMeshClientError, "group.outdated_client_policy"},
		{"load balancer", validMeshServerYAML() + "load_balancer: random\n", loadMeshServerError, "load_balancer must be"},
		{"routing policy", validMeshServerYAML() + "routing_policy: random\n", loadMeshServerError, "routing_policy must be"},
		{"server heartbeat interval", serverTunnel("  heartbeat_interval: -1s\n"), loadMeshServerError, "tunnel.heartbeat_interval must be positive"},
		{"server health timeout", serverTunnel("  health_timeout: -1s\n"), loadMeshServerError, "tunnel.health_timeout must be positive"},
		{"server heartbeat health boundary", serverTunnel("  heartbeat_interval: 10s\n  health_timeout: 10s\n"), loadMeshServerError, "tunnel.health_timeout"},
		{"server copy buffer", serverTunnel("  tcp_copy_buffer_size: -1\n"), loadMeshServerError, "tunnel.tcp_copy_buffer_size must be positive"},
		{"client heartbeat interval", clientTunnel("  heartbeat_interval: -1s\n"), loadMeshClientError, "tunnel.heartbeat_interval must be positive"},
		{"client health timeout", clientTunnel("  health_timeout: -1s\n"), loadMeshClientError, "tunnel.health_timeout must be positive"},
		{"client heartbeat health boundary", clientTunnel("  heartbeat_interval: 10s\n  health_timeout: 10s\n"), loadMeshClientError, "tunnel.health_timeout"},
		{"client copy buffer", clientTunnel("  tcp_copy_buffer_size: -1\n"), loadMeshClientError, "tunnel.tcp_copy_buffer_size must be positive"},
		{"server TLS certificate", strings.Replace(validMeshServerYAML(), "      server_cert_file: server.pem\n", "", 1), loadMeshServerError, "tunnel.listen.tls.server_cert_file is required"},
		{"server TLS key", strings.Replace(validMeshServerYAML(), "      server_key_file: server-key.pem\n", "", 1), loadMeshServerError, "tunnel.listen.tls.server_key_file is required"},
		{"negative STEK interval", serverTLS("      session_ticket_encryption_key_rotation_interval: -1s\n"), loadMeshServerError, "tunnel.listen.tls.session_ticket_encryption_key_rotation_interval"},
		{"zero STEK overlap with zero interval", serverTLS("      session_ticket_encryption_key_rotation_interval: 0s\n      session_ticket_encryption_key_rotation_overlap: 0\n"), loadMeshServerError, "tunnel.listen.tls.session_ticket_encryption_key_rotation_overlap"},
		{"positive STEK overlap with zero interval", serverTLS("      session_ticket_encryption_key_rotation_interval: 0s\n      session_ticket_encryption_key_rotation_overlap: 1\n"), loadMeshServerError, "tunnel.listen.tls.session_ticket_encryption_key_rotation_overlap"},
		{"server admin address", validMeshServerYAML() + "admin_address: bad\n", loadMeshServerError, "admin_address"},
		{"client admin address", validMeshClientYAML() + "admin_address: bad\n", loadMeshClientError, "admin_address"},
		{"blank local host", strings.Replace(validMeshClientYAML(), "  host: \"127.0.0.1\"", "  host: \"   \"", 1), loadMeshClientError, "local.host is required"},
		{"local port zero", strings.Replace(validMeshClientYAML(), "  port: 8080", "  port: 0", 1), loadMeshClientError, "local.port"},
		{"local port above maximum", strings.Replace(validMeshClientYAML(), "  port: 8080", "  port: 65536", 1), loadMeshClientError, "local.port"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.load(writeTestConfig(t, test.content)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("load error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMeshTypedLoadersRejectInvalidIntegerScalars(t *testing.T) {
	tests := []struct {
		name    string
		content string
		load    func(string) error
		want    string
	}{
		{
			"rule version overflow",
			strings.Replace(validMeshClientYAML(), "  rule_version: 1", "  rule_version: 18446744073709551616", 1),
			loadMeshClientError,
			"group.rule_version must be an integer",
		},
		{
			"rule version float",
			strings.Replace(validMeshClientYAML(), "  rule_version: 1", "  rule_version: 1.5", 1),
			loadMeshClientError,
			"group.rule_version must be an integer",
		},
		{
			"rule version negative",
			strings.Replace(validMeshClientYAML(), "  rule_version: 1", "  rule_version: -1", 1),
			loadMeshClientError,
			"group.rule_version must be an unsigned 64-bit integer",
		},
		{
			"max peers negative float",
			strings.Replace(validMeshServerYAML(), "  max_peers: 2", "  max_peers: -0.5", 1),
			loadMeshServerError,
			"limits.max_peers must be an integer",
		},
		{
			"max peers float",
			strings.Replace(validMeshServerYAML(), "  max_peers: 2", "  max_peers: 1.5", 1),
			loadMeshServerError,
			"limits.max_peers must be an integer",
		},
		{
			"unsigned QUIC negative",
			strings.Replace(validMeshServerYAML(), "  peering:\n", "  quic:\n    initial_stream_receive_window: -1\n  peering:\n", 1),
			loadMeshServerError,
			"tunnel.quic.initial_stream_receive_window must be an unsigned 64-bit integer",
		},
		{
			"peer metric underflow",
			strings.Replace(validMeshServerYAML(), "      - server_id: edge-b", "      - server_id: edge-b\n        metric: -9223372036854775809", 1),
			loadMeshServerError,
			"tunnel.peering.peers[0].metric must be an integer",
		},
		{
			"STEK overlap overflow",
			strings.Replace(validMeshServerYAML(), "      server_key_file: server-key.pem", "      server_key_file: server-key.pem\n      session_ticket_encryption_key_rotation_overlap: 256", 1),
			loadMeshServerError,
			"tunnel.listen.tls.session_ticket_encryption_key_rotation_overlap must be an unsigned 8-bit integer",
		},
		{
			"HTTP in-flight float",
			strings.Replace(validMeshServerYAML(), "limits:\n", "ingress:\n  listeners:\n    - address: 127.0.0.1:8080\n      protocol: http\n      max_inflight_requests: 1.5\nlimits:\n", 1),
			loadMeshServerError,
			"ingress.listeners[0].max_inflight_requests must be an integer",
		},
		{
			"TLS passthrough in-flight null",
			strings.Replace(validMeshServerYAML(), "limits:\n", "ingress:\n  listeners:\n    - address: 127.0.0.1:8444\n      protocol: tls_passthrough\n      max_inflight_requests: null\nlimits:\n", 1),
			loadMeshServerError,
			"ingress.listeners[0].max_inflight_requests must be an integer",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.load(writeTestConfig(t, test.content)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("load error = %v, want %q", err, test.want)
			}
		})
	}

	serverFields := []struct {
		name    string
		content string
		path    string
	}{
		{"tunnel copy buffer", strings.Replace(validMeshServerYAML(), "  peering:\n", "  tcp_copy_buffer_size: 1.5\n  peering:\n", 1), "tunnel.tcp_copy_buffer_size"},
		{"peer metric", strings.Replace(validMeshServerYAML(), "      - server_id: edge-b", "      - server_id: edge-b\n        metric: 1.5", 1), "tunnel.peering.peers[0].metric"},
		{"STEK overlap", strings.Replace(validMeshServerYAML(), "      server_key_file: server-key.pem", "      server_key_file: server-key.pem\n      session_ticket_encryption_key_rotation_overlap: 1.5", 1), "tunnel.listen.tls.session_ticket_encryption_key_rotation_overlap"},
	}
	for _, field := range []string{"max_tcp_connections", "max_pending_tcp_setups", "max_tcp_connections_per_generation", "max_pending_tcp_setups_per_generation"} {
		serverFields = append(serverFields, struct {
			name    string
			content string
			path    string
		}{"tunnel capacity " + field, strings.Replace(validMeshServerYAML(), "  peering:\n", "  capacity:\n    "+field+": 1.5\n  peering:\n", 1), "tunnel.capacity." + field})
	}
	for _, field := range []string{"initial_stream_receive_window", "max_stream_receive_window", "initial_connection_receive_window", "max_connection_receive_window", "max_incoming_streams"} {
		serverFields = append(serverFields, struct {
			name    string
			content string
			path    string
		}{
			"QUIC " + field,
			strings.Replace(validMeshServerYAML(), "  peering:\n", "  quic:\n    "+field+": 1.5\n  peering:\n", 1),
			"tunnel.quic." + field,
		})
	}
	for _, field := range []string{"failure_threshold", "success_threshold", "max_concurrent", "max_queued"} {
		serverFields = append(serverFields, struct {
			name    string
			content string
			path    string
		}{"scheduler " + field, validMeshServerYAML() + "probe_scheduler:\n  " + field + ": 1.5\n", "probe_scheduler." + field})
	}
	for _, field := range []string{
		"max_client_generations", "max_pending_registrations", "max_peers", "max_groups",
		"max_group_declaration_bytes", "max_total_group_declaration_bytes",
		"max_paths_per_group", "max_total_paths", "max_path_hops",
		"max_control_queue_messages", "max_control_queue_bytes",
	} {
		content := strings.Replace(validMeshServerYAML(), "  max_peers: 2", "  max_peers: 2\n  "+field+": 1.5", 1)
		if field == "max_peers" {
			content = strings.Replace(validMeshServerYAML(), "  max_peers: 2", "  max_peers: 1.5", 1)
		}
		serverFields = append(serverFields, struct {
			name    string
			content string
			path    string
		}{"limit " + field, content, "limits." + field})
	}
	for _, test := range serverFields {
		t.Run(test.name, func(t *testing.T) {
			if err := loadMeshServerError(writeTestConfig(t, test.content)); err == nil || !strings.Contains(err.Error(), test.path+" must be an integer") {
				t.Fatalf("load error = %v, want integer error for %s", err, test.path)
			}
		})
	}

	clientFields := []struct {
		name    string
		content string
		path    string
	}{
		{"tunnel copy buffer", strings.Replace(validMeshClientYAML(), "  servers:\n", "  tcp_copy_buffer_size: 1.5\n  servers:\n", 1), "tunnel.tcp_copy_buffer_size"},
		{"tunnel QUIC", strings.Replace(validMeshClientYAML(), "  auth:\n", "  quic:\n    max_incoming_streams: 1.5\n  auth:\n", 1), "tunnel.quic.max_incoming_streams"},
		{"local port", strings.Replace(validMeshClientYAML(), "  port: 8080", "  port: 1.5", 1), "local.port"},
		{"rule version", strings.Replace(validMeshClientYAML(), "  rule_version: 1", "  rule_version: 1.5", 1), "group.rule_version"},
		{"group metric", strings.Replace(validMeshClientYAML(), "  rule_version: 1", "  rule_version: 1\n  metric: 1.5", 1), "group.metric"},
	}
	for _, test := range clientFields {
		t.Run("client "+test.name, func(t *testing.T) {
			if err := loadMeshClientError(writeTestConfig(t, test.content)); err == nil || !strings.Contains(err.Error(), test.path+" must be an integer") {
				t.Fatalf("load error = %v, want integer error for %s", err, test.path)
			}
		})
	}

	maxRuleVersion := strings.Replace(validMeshClientYAML(), "  rule_version: 1", "  rule_version: 18446744073709551615", 1)
	if _, err := LoadMeshClientConfig(writeTestConfig(t, maxRuleVersion)); err != nil {
		t.Fatalf("maximum uint64 rule_version: %v", err)
	}
	minMetric := strings.Replace(validMeshServerYAML(), "      - server_id: edge-b", "      - server_id: edge-b\n        metric: -9223372036854775808", 1)
	if _, err := LoadMeshServerConfig(writeTestConfig(t, minMetric)); err != nil {
		t.Fatalf("minimum int64 peer metric: %v", err)
	}
	nullOverlap := strings.Replace(validMeshServerYAML(), "      server_key_file: server-key.pem", "      server_key_file: server-key.pem\n      session_ticket_encryption_key_rotation_overlap: null", 1)
	if _, err := LoadMeshServerConfig(writeTestConfig(t, nullOverlap)); err != nil {
		t.Fatalf("null STEK overlap: %v", err)
	}
}

func TestMeshIntegerAliasAndMergeSemantics(t *testing.T) {
	t.Run("mapping key alias", func(t *testing.T) {
		content := strings.Replace(
			validMeshClientYAML(),
			"      address: \"edge.example:8443\"",
			"      address: \"edge.example:8443\"\n      server_name: &integer_key rule_version",
			1,
		)
		content = strings.Replace(content, "  rule_version: 1", "  *integer_key: 1.5", 1)
		if err := loadMeshClientError(writeTestConfig(t, content)); err == nil || !strings.Contains(err.Error(), "group.rule_version must be an integer") {
			t.Fatalf("load error = %v, want aliased rule_version integer error", err)
		}
	})

	t.Run("self-referential merge", func(t *testing.T) {
		content := strings.Replace(validMeshServerYAML(), "limits:\n  max_peers: 2", "limits: &limits\n  <<: *limits", 1)
		if err := loadMeshServerError(writeTestConfig(t, content)); err == nil {
			t.Fatal("self-referential merge was accepted")
		}
	})

	t.Run("explicit field overrides merge", func(t *testing.T) {
		content := strings.Replace(validMeshServerYAML(), "limits:\n  max_peers: 2", "limits:\n  <<: {max_peers: 1.5}\n  max_peers: 2", 1)
		if _, err := LoadMeshServerConfig(writeTestConfig(t, content)); err != nil {
			t.Fatalf("explicit max_peers override: %v", err)
		}
	})

	t.Run("positive sign", func(t *testing.T) {
		server := strings.Replace(validMeshServerYAML(), "  max_peers: 2", "  max_peers: +1", 1)
		if _, err := LoadMeshServerConfig(writeTestConfig(t, server)); err != nil {
			t.Fatalf("signed +1: %v", err)
		}
		client := strings.Replace(validMeshClientYAML(), "  rule_version: 1", "  rule_version: +1", 1)
		if _, err := LoadMeshClientConfig(writeTestConfig(t, client)); err != nil {
			t.Fatalf("unsigned +1: %v", err)
		}
	})

	t.Run("valid alias and merge", func(t *testing.T) {
		content := strings.Replace(
			validMeshServerYAML(),
			"limits:\n  max_peers: 2",
			"limits:\n  <<: {max_peers: +1}\n  max_groups: &shared_limit +1024\n  max_control_queue_messages: *shared_limit",
			1,
		)
		if _, err := LoadMeshServerConfig(writeTestConfig(t, content)); err != nil {
			t.Fatalf("valid integer aliases and merge: %v", err)
		}
	})
}

func TestMeshIdentityValidation(t *testing.T) {
	valid253 := strings.Join([]string{strings.Repeat("a", 63), strings.Repeat("b", 63), strings.Repeat("c", 63), strings.Repeat("d", 61)}, ".")
	valid := []string{"a", "edge-shanghai-1", "a.b", valid253}
	for _, value := range valid {
		if err := validateMeshIdentity("id", value); err != nil {
			t.Fatalf("identity %q: %v", value, err)
		}
	}
	invalid := []string{"", "Upper", "-edge", "edge-", "edge_1", strings.Repeat("a", 64), valid253 + "a"}
	for _, value := range invalid {
		if err := validateMeshIdentity("id", value); err == nil {
			t.Fatalf("identity %q unexpectedly accepted", value)
		}
	}
}

func TestMeshEndpointAndPeerValidation(t *testing.T) {
	t.Run("client endpoints", func(t *testing.T) {
		tests := []struct {
			name string
			edit func(*MeshClient)
			want string
		}{
			{"empty", func(c *MeshClient) { c.Tunnel.Servers = nil }, "at least one endpoint"},
			{"duplicate ID", func(c *MeshClient) {
				c.Tunnel.Servers = append(c.Tunnel.Servers, MeshServerEndpoint{ServerID: "edge-a", Address: "edge-b.example:8443"})
			}, "duplicates tunnel.servers[0].server_id"},
			{"duplicate address", func(c *MeshClient) {
				c.Tunnel.Servers = append(c.Tunnel.Servers, MeshServerEndpoint{ServerID: "edge-b", Address: c.Tunnel.Servers[0].Address})
			}, "duplicates tunnel.servers[0].address"},
			{"invalid address", func(c *MeshClient) { c.Tunnel.Servers[0].Address = "bad" }, "tunnel.servers[0].address"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				client := validMeshClient()
				test.edit(&client)
				assertMeshValidationError(t, client.Validate(), test.want)
			})
		}
	})

	t.Run("peers", func(t *testing.T) {
		acceptOnly := validMeshServer()
		acceptOnly.Tunnel.Peering.Peers = []MeshPeer{{ServerID: "edge-b"}}
		if err := acceptOnly.Validate(); err != nil {
			t.Fatalf("accept-only peer: %v", err)
		}

		dial := validMeshServer()
		dial.Tunnel.Peering = validMeshPeering(MeshPeer{ServerID: "edge-b", Address: "edge-b.example:8443", ServerName: "edge-b.example"})
		if err := dial.Validate(); err != nil {
			t.Fatalf("dial peer: %v", err)
		}

		tests := []struct {
			name string
			edit func(*MeshServer)
			want string
		}{
			{"address without server name", func(s *MeshServer) {
				s.Tunnel.Peering.Peers = []MeshPeer{{ServerID: "edge-b", Address: "edge-b.example:8443"}}
			}, "must be provided together"},
			{"server name without address", func(s *MeshServer) {
				s.Tunnel.Peering.Peers = []MeshPeer{{ServerID: "edge-b", ServerName: "edge-b.example"}}
			}, "must be provided together"},
			{"self peer", func(s *MeshServer) { s.Tunnel.Peering.Peers = []MeshPeer{{ServerID: s.ServerID}} }, "must not equal local server_id"},
			{"duplicate ID", func(s *MeshServer) { s.Tunnel.Peering.Peers = []MeshPeer{{ServerID: "edge-b"}, {ServerID: "edge-b"}} }, "duplicates tunnel.peering.peers[0].server_id"},
			{"duplicate address", func(s *MeshServer) {
				s.Tunnel.Peering = validMeshPeering(MeshPeer{ServerID: "edge-b", Address: "edge.example:8443", ServerName: "edge-b.example"}, MeshPeer{ServerID: "edge-c", Address: "edge.example:8443", ServerName: "edge-c.example"})
			}, "duplicates tunnel.peering.peers[0].address"},
			{"dial credentials required", func(s *MeshServer) {
				s.Tunnel.Peering.Peers = []MeshPeer{{ServerID: "edge-b", Address: "edge.example:8443", ServerName: "edge.example"}}
			}, "tunnel.peering.tls"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				server := validMeshServer()
				test.edit(&server)
				assertMeshValidationError(t, server.Validate(), test.want)
			})
		}
	})
}

func TestMeshServerSocketAndIngressValidation(t *testing.T) {
	sameTCPAndUDP := validMeshServer()
	sameTCPAndUDP.Ingress.Listeners[0].Address = sameTCPAndUDP.Tunnel.Listen.Address
	if err := sameTCPAndUDP.Validate(); err != nil {
		t.Fatalf("same textual TCP and UDP address: %v", err)
	}

	tests := []struct {
		name string
		edit func(*MeshServer)
		want string
	}{
		{"admin ingress conflict", func(s *MeshServer) { s.AdminAddress = s.Ingress.Listeners[0].Address }, "conflicts with admin_address"},
		{"duplicate ingress", func(s *MeshServer) { s.Ingress.Listeners = append(s.Ingress.Listeners, s.Ingress.Listeners[0]) }, "conflicts with ingress.listeners[0].address"},
		{"invalid protocol", func(s *MeshServer) { s.Ingress.Listeners[0].Protocol = "tcp" }, "protocol must be"},
		{"HTTPS missing certificate", func(s *MeshServer) { s.Ingress.Listeners[0].Protocol = MeshIngressProtocolHTTPS }, "at least one certificate"},
		{"HTTP with certificate", func(s *MeshServer) {
			s.Ingress.Listeners[0].Certificates = []MeshIngressCertificate{{CertFile: "cert.pem", KeyFile: "key.pem"}}
		}, "only valid for https"},
		{"TLS passthrough with certificate", func(s *MeshServer) {
			s.Ingress.Listeners[0].Protocol = MeshIngressProtocolTLSPassthrough
			s.Ingress.Listeners[0].Certificates = []MeshIngressCertificate{{CertFile: "cert.pem", KeyFile: "key.pem"}}
		}, "only valid for https"},
		{"TLS passthrough with explicit in-flight zero", func(s *MeshServer) {
			s.Ingress.Listeners[0].Protocol = MeshIngressProtocolTLSPassthrough
			zero := 0
			s.Ingress.Listeners[0].MaxInflightRequests = &zero
		}, "max_inflight_requests is not valid"},
		{"HTTP negative in-flight", func(s *MeshServer) {
			negative := -1
			s.Ingress.Listeners[0].MaxInflightRequests = &negative
		}, "max_inflight_requests must not be negative"},
		{"incomplete certificate", func(s *MeshServer) {
			s.Ingress.Listeners[0].Protocol = MeshIngressProtocolHTTPS
			s.Ingress.Listeners[0].Certificates = []MeshIngressCertificate{{CertFile: "cert.pem"}}
		}, "cert_file and key_file are required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := validMeshServer()
			test.edit(&server)
			assertMeshValidationError(t, server.Validate(), test.want)
		})
	}

	https := validMeshServer()
	https.Ingress.Listeners[0] = MeshIngressListener{Address: "127.0.0.1:443", Protocol: MeshIngressProtocolHTTPS, Certificates: []MeshIngressCertificate{{CertFile: "cert.pem", KeyFile: "key.pem"}}}
	if err := https.Validate(); err != nil {
		t.Fatalf("valid HTTPS ingress: %v", err)
	}
}

func TestMeshSchedulerAndLimitsValidation(t *testing.T) {
	scheduler := MeshProbeScheduler{Interval: time.Second, Timeout: time.Second, FailureThreshold: 1, SuccessThreshold: 1, MaxConcurrent: 1, MaxQueued: 1}
	if err := scheduler.Validate("probe_scheduler"); err != nil {
		t.Fatalf("valid scheduler: %v", err)
	}
	schedulerTests := []struct {
		name string
		edit func(*MeshProbeScheduler)
	}{
		{"interval", func(p *MeshProbeScheduler) { p.Interval = -1 }},
		{"timeout", func(p *MeshProbeScheduler) { p.Timeout = 0 }},
		{"failure_threshold", func(p *MeshProbeScheduler) { p.FailureThreshold = -1 }},
		{"success_threshold", func(p *MeshProbeScheduler) { p.SuccessThreshold = -1 }},
		{"max_concurrent", func(p *MeshProbeScheduler) { p.MaxConcurrent = -1 }},
		{"max_queued", func(p *MeshProbeScheduler) { p.MaxQueued = -1 }},
	}
	for _, test := range schedulerTests {
		t.Run("scheduler "+test.name, func(t *testing.T) {
			candidate := scheduler
			test.edit(&candidate)
			assertMeshValidationError(t, candidate.Validate("probe_scheduler"), "probe_scheduler."+test.name)
		})
	}

	limitsTests := []struct {
		name string
		edit func(*MeshServerLimits)
		want string
	}{
		{"negative", func(l *MeshServerLimits) { l.MaxGroups = -1 }, "limits.max_groups"},
		{"group declaration exceeds total", func(l *MeshServerLimits) { l.MaxGroupDeclarationBytes = 5; l.MaxTotalGroupDeclarationBytes = 4 }, "max_group_declaration_bytes must not exceed"},
		{"group paths exceed total", func(l *MeshServerLimits) { l.MaxPathsPerGroup = 5; l.MaxTotalPaths = 4 }, "max_paths_per_group must not exceed"},
	}
	for _, test := range limitsTests {
		t.Run(test.name, func(t *testing.T) {
			limits := validMeshServer().Limits
			test.edit(&limits)
			assertMeshValidationError(t, limits.Validate("limits"), test.want)
		})
	}
	for _, field := range []struct {
		name string
		edit func(*MeshTCPCapacity)
	}{
		{"max_tcp_connections", func(c *MeshTCPCapacity) { c.MaxTCPConnections = -1 }},
		{"max_pending_tcp_setups", func(c *MeshTCPCapacity) { c.MaxPendingTCPSetups = -1 }},
		{"max_tcp_connections_per_generation", func(c *MeshTCPCapacity) { c.MaxTCPConnectionsPerGeneration = -1 }},
		{"max_pending_tcp_setups_per_generation", func(c *MeshTCPCapacity) { c.MaxPendingTCPSetupsPerGeneration = -1 }},
	} {
		t.Run("negative "+field.name, func(t *testing.T) {
			server := validMeshServer()
			field.edit(&server.Tunnel.Capacity)
			assertMeshValidationError(t, server.Validate(), "tunnel.capacity."+field.name)
			server = validMeshServer()
			field.edit(&server.Ingress.Listeners[0].Capacity)
			assertMeshValidationError(t, server.Validate(), "ingress.listeners[0].capacity."+field.name)
		})
	}
	server := validMeshServer()
	server.Tunnel.Capacity = MeshTCPCapacity{1, 2, 3, 4}
	server.Ingress.Listeners[0].Capacity = MeshTCPCapacity{4, 3, 2, 1}
	if err := server.Validate(); err != nil {
		t.Fatalf("independent out-of-order TCP limits: %v", err)
	}

	for _, count := range []int{defaultMeshMaxPeers, defaultMeshMaxPeers + 1} {
		server := validMeshServer()
		server.Tunnel.Peering.Peers = make([]MeshPeer, count)
		for i := range count {
			server.Tunnel.Peering.Peers[i].ServerID = fmt.Sprintf("peer-%d", i)
		}
		err := server.Validate()
		if count == defaultMeshMaxPeers && err != nil {
			t.Fatalf("%d peers: %v", count, err)
		}
		if count > defaultMeshMaxPeers {
			assertMeshValidationError(t, err, "exceeds limits.max_peers")
		}
	}
}

func TestMeshRouteCollectionLimits(t *testing.T) {
	baseRoute := MeshHTTPRoute{Hostnames: []string{"api.example.com"}, Matches: []MeshHTTPMatch{{Path: &MeshHTTPPathMatch{Type: MeshPathMatchPathPrefix, Value: "/"}}}}
	tests := []struct {
		name  string
		build func(int) MeshGroup
		limit int
		want  string
	}{
		{"HTTP routes", func(n int) MeshGroup { g := validMeshGroup(); g.Routes.HTTP = repeatValue(baseRoute, n); return g }, maxMeshHTTPRoutes, "routes.http"},
		{"HTTP hostnames", func(n int) MeshGroup {
			g := validMeshGroup()
			route := baseRoute
			route.Hostnames = numberedHostnames(n)
			g.Routes.HTTP = []MeshHTTPRoute{route}
			return g
		}, maxMeshRouteHostnames, "hostnames"},
		{"HTTP matches", func(n int) MeshGroup {
			g := validMeshGroup()
			route := baseRoute
			route.Matches = repeatValue(MeshHTTPMatch{}, n)
			g.Routes.HTTP = []MeshHTTPRoute{route}
			return g
		}, maxMeshHTTPMatches, "matches"},
		{"headers", func(n int) MeshGroup {
			g := validMeshGroup()
			route := baseRoute
			route.Matches = []MeshHTTPMatch{{Headers: numberedHeaders(n)}}
			g.Routes.HTTP = []MeshHTTPRoute{route}
			return g
		}, maxMeshHTTPMatchHeaders, "headers"},
		{"query params", func(n int) MeshGroup {
			g := validMeshGroup()
			route := baseRoute
			route.Matches = []MeshHTTPMatch{{QueryParams: numberedQueries(n)}}
			g.Routes.HTTP = []MeshHTTPRoute{route}
			return g
		}, maxMeshHTTPMatchQueries, "query_params"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, count := range []int{test.limit, test.limit + 1} {
				group := test.build(count)
				group.ApplyDefaults()
				err := group.Validate("group")
				if count == test.limit && err != nil {
					t.Fatalf("count %d: %v", count, err)
				}
				if count > test.limit {
					assertMeshValidationError(t, err, test.want)
				}
			}
		})
	}

	for _, count := range []int{0, 1, maxMeshRouteHostnames, maxMeshRouteHostnames + 1} {
		group := validMeshGroup()
		group.Routes.TLSPassthrough = &MeshTLSRoute{Hostnames: numberedHostnames(count)}
		err := group.Validate("group")
		switch count {
		case 1, maxMeshRouteHostnames:
			if err != nil {
				t.Fatalf("TLS hostnames count %d: %v", count, err)
			}
		default:
			assertMeshValidationError(t, err, "tls_passthrough.hostnames")
		}
	}
}

func TestMeshHTTPMatchSyntax(t *testing.T) {
	validHostnames := []string{"api.example.com", "*.api.example.com", "a", strings.Repeat("a", 63) + ".example"}
	for _, hostname := range validHostnames {
		if err := validateMeshHostname(hostname); err != nil {
			t.Fatalf("hostname %q: %v", hostname, err)
		}
	}
	invalidHostnames := []string{"", "API.example.com", "*", "api.*.example.com", "127.0.0.1", "*.127.0.0.1", strings.Repeat("a", 64) + ".example"}
	for _, hostname := range invalidHostnames {
		if err := validateMeshHostname(hostname); err == nil {
			t.Fatalf("hostname %q unexpectedly accepted", hostname)
		}
	}

	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH"} {
		if !validMeshHTTPMethod(method) {
			t.Fatalf("method %q rejected", method)
		}
	}
	for _, method := range []string{"get", "PURGE", ""} {
		if method != "" && validMeshHTTPMethod(method) {
			t.Fatalf("method %q unexpectedly accepted", method)
		}
	}

	validPaths := []string{"/", "/v1/items", "/a:b@c;d=e", "/percent%20space", "/" + strings.Repeat("a", maxMeshHTTPPathLength-1)}
	for _, path := range validPaths {
		if err := validateMeshHTTPPath(path); err != nil {
			t.Fatalf("path %q: %v", path, err)
		}
	}
	invalidPaths := []string{"", "relative", "//", "/a//b", "/./a", "/a/.", "/../a", "/a/..", "/%2f", "/%2F", "/bad%", "/bad%xx", "/query?x=1", "/fragment#x", "/" + strings.Repeat("a", maxMeshHTTPPathLength)}
	for _, path := range invalidPaths {
		if err := validateMeshHTTPPath(path); err == nil {
			t.Fatalf("path %q unexpectedly accepted", path)
		}
	}

	valid := MeshHTTPMatch{
		Headers: []MeshHTTPHeaderMatch{
			{Name: strings.Repeat("a", maxMeshHTTPNameLength), Value: strings.Repeat("v", maxMeshHeaderValue)},
			{Name: "x-whitespace", Value: "a  \t\t b"},
		},
		QueryParams: []MeshHTTPQueryParamMatch{
			{Name: strings.Repeat("q", maxMeshHTTPNameLength), Value: strings.Repeat("v", maxMeshQueryValue)},
			{Name: "unicode", Value: strings.Repeat("界", maxMeshQueryValue)},
		},
	}
	if err := valid.validate("match"); err != nil {
		t.Fatalf("valid HTTP conditions: %v", err)
	}
	tests := []struct {
		name string
		edit func(*MeshHTTPMatch)
		want string
	}{
		{"path type", func(m *MeshHTTPMatch) { m.Path = &MeshHTTPPathMatch{Type: "Regex", Value: "/"} }, "path.type"},
		{"method", func(m *MeshHTTPMatch) { m.Method = "get" }, "method"},
		{"header type", func(m *MeshHTTPMatch) { m.Headers = []MeshHTTPHeaderMatch{{Type: "Regex", Name: "x", Value: "v"}} }, "headers[0].type"},
		{"header empty name", func(m *MeshHTTPMatch) { m.Headers = []MeshHTTPHeaderMatch{{Name: "", Value: "v"}} }, "headers[0].name"},
		{"header long name", func(m *MeshHTTPMatch) {
			m.Headers = []MeshHTTPHeaderMatch{{Name: strings.Repeat("a", maxMeshHTTPNameLength+1), Value: "v"}}
		}, "headers[0].name"},
		{"header invalid name", func(m *MeshHTTPMatch) { m.Headers = []MeshHTTPHeaderMatch{{Name: "bad:name", Value: "v"}} }, "headers[0].name"},
		{"header empty value", func(m *MeshHTTPMatch) { m.Headers = []MeshHTTPHeaderMatch{{Name: "x", Value: ""}} }, "headers[0].value"},
		{"header leading space", func(m *MeshHTTPMatch) { m.Headers = []MeshHTTPHeaderMatch{{Name: "x", Value: " value"}} }, "headers[0].value"},
		{"header trailing tab", func(m *MeshHTTPMatch) { m.Headers = []MeshHTTPHeaderMatch{{Name: "x", Value: "value\t"}} }, "headers[0].value"},
		{"header control", func(m *MeshHTTPMatch) { m.Headers = []MeshHTTPHeaderMatch{{Name: "x", Value: "a\nb"}} }, "headers[0].value"},
		{"header non-ASCII", func(m *MeshHTTPMatch) { m.Headers = []MeshHTTPHeaderMatch{{Name: "x", Value: "café"}} }, "headers[0].value"},
		{"header long value", func(m *MeshHTTPMatch) {
			m.Headers = []MeshHTTPHeaderMatch{{Name: "x", Value: strings.Repeat("v", maxMeshHeaderValue+1)}}
		}, "headers[0].value"},
		{"query type", func(m *MeshHTTPMatch) {
			m.QueryParams = []MeshHTTPQueryParamMatch{{Type: "Regex", Name: "q", Value: "v"}}
		}, "query_params[0].type"},
		{"query invalid name", func(m *MeshHTTPMatch) { m.QueryParams = []MeshHTTPQueryParamMatch{{Name: "bad:name", Value: "v"}} }, "query_params[0].name"},
		{"query empty value", func(m *MeshHTTPMatch) { m.QueryParams = []MeshHTTPQueryParamMatch{{Name: "q"}} }, "query_params[0].value"},
		{"query long value", func(m *MeshHTTPMatch) {
			m.QueryParams = []MeshHTTPQueryParamMatch{{Name: "q", Value: strings.Repeat("v", maxMeshQueryValue+1)}}
		}, "query_params[0].value"},
		{"query long Unicode value", func(m *MeshHTTPMatch) {
			m.QueryParams = []MeshHTTPQueryParamMatch{{Name: "q", Value: strings.Repeat("界", maxMeshQueryValue+1)}}
		}, "query_params[0].value"},
		{"query invalid UTF-8", func(m *MeshHTTPMatch) {
			m.QueryParams = []MeshHTTPQueryParamMatch{{Name: "q", Value: string([]byte{0xff})}}
		}, "query_params[0].value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			match := MeshHTTPMatch{}
			test.edit(&match)
			assertMeshValidationError(t, match.validate("match"), test.want)
		})
	}
}

func TestMeshProbeAndOriginTLSValidation(t *testing.T) {
	validProbes := []MeshProbe{{Type: MeshProbeTypeTCP}, {Type: MeshProbeTypeHTTP, Host: "api.example.com:8443", Path: "/healthz"}}
	for _, probe := range validProbes {
		if err := probe.validate("group.probe"); err != nil {
			t.Fatalf("probe %+v: %v", probe, err)
		}
	}
	invalidProbes := []MeshProbe{
		{Type: MeshProbeTypeTCP, Host: "api.example.com"},
		{Type: MeshProbeTypeHTTP, Path: "/healthz"},
		{Type: MeshProbeTypeHTTP, Host: "api.example.com"},
		{Type: MeshProbeTypeHTTP, Host: "api.example.com", Path: "healthz"},
		{Type: MeshProbeTypeHTTP, Host: "   ", Path: "/healthz"},
		{Type: "grpc"},
	}
	for _, probe := range invalidProbes {
		if err := probe.validate("group.probe"); err == nil {
			t.Fatalf("probe %+v unexpectedly accepted", probe)
		}
	}

	validTLS := []MeshOriginTLS{{}, {Enabled: true}, {Enabled: true, Verify: true}, {Enabled: true, Verify: true, ExtraCAFiles: []string{"ca.pem"}}}
	for _, tlsConfig := range validTLS {
		if err := tlsConfig.validate("group.origin_tls"); err != nil {
			t.Fatalf("origin TLS %+v: %v", tlsConfig, err)
		}
	}
	invalidTLS := []MeshOriginTLS{{Verify: true}, {ExtraCAFiles: []string{"ca.pem"}}, {Enabled: true, ExtraCAFiles: []string{"ca.pem"}}}
	for _, tlsConfig := range invalidTLS {
		if err := tlsConfig.validate("group.origin_tls"); err == nil {
			t.Fatalf("origin TLS %+v unexpectedly accepted", tlsConfig)
		}
	}
}

func TestLoadMeshExtraCACertificates(t *testing.T) {
	dir := t.TempDir()
	certA := testCertificatePEM(t, 1)
	certB := testCertificatePEM(t, 2)
	bundle := filepath.Join(dir, "bundle.pem")
	spacedBundle := append(bytes.Clone(certB), []byte("\n \t\r\n")...)
	spacedBundle = append(spacedBundle, certA...)
	spacedBundle = append(spacedBundle, certA...)
	if err := os.WriteFile(bundle, spacedBundle, 0o600); err != nil {
		t.Fatal(err)
	}
	certificates, err := loadMeshExtraCACertificates([]string{bundle})
	if err != nil {
		t.Fatalf("load certificates: %v", err)
	}
	if len(certificates) != 2 || bytes.Compare(certificates[0], certificates[1]) >= 0 {
		t.Fatalf("certificates not sorted and deduplicated: %d", len(certificates))
	}

	nonCertificate := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(nonCertificate, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("bad")}), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(dir, "invalid.pem")
	if err := os.WriteFile(invalid, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	garbagePrefix := filepath.Join(dir, "garbage-prefix.pem")
	if err := os.WriteFile(garbagePrefix, append([]byte("not pem\n"), certA...), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidBase64 := filepath.Join(dir, "invalid-base64.pem")
	invalidBase64Data := append([]byte("-----BEGIN CERTIFICATE-----\n!!!\n-----END CERTIFICATE-----\n"), certA...)
	if err := os.WriteFile(invalidBase64, invalidBase64Data, 0o600); err != nil {
		t.Fatal(err)
	}
	unclosedBlock := filepath.Join(dir, "unclosed-block.pem")
	unclosedBlockData := append([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n"), certA...)
	if err := os.WriteFile(unclosedBlock, unclosedBlockData, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
		want string
	}{
		{"missing", filepath.Join(dir, "missing.pem"), "read certificate file"},
		{"invalid", invalid, "invalid PEM certificate data"},
		{"garbage prefix", garbagePrefix, "invalid PEM certificate data"},
		{"invalid base64 before certificate", invalidBase64, "invalid PEM certificate data"},
		{"unclosed block before certificate", unclosedBlock, "invalid PEM certificate data"},
		{"wrong block", nonCertificate, "must be an unadorned CERTIFICATE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := loadMeshExtraCACertificates([]string{test.path}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("load error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMeshGroupCanonicalEquivalenceAndPrivacy(t *testing.T) {
	dir := t.TempDir()
	certA := testCertificatePEM(t, 11)
	certB := testCertificatePEM(t, 12)
	bundleAB := filepath.Join(dir, "ab.pem")
	bundleBAA := filepath.Join(dir, "baa.pem")
	if err := os.WriteFile(bundleAB, append(bytes.Clone(certA), certB...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundleBAA, append(append(bytes.Clone(certB), certA...), certA...), 0o600); err != nil {
		t.Fatal(err)
	}

	implicit := canonicalMeshClientYAML(canonicalYAMLOptions{instanceID: "instance-a", endpoint: "edge.example:8443", localHost: "127.0.0.1", localPort: 8080, token: "0123456789abcdef", caFile: bundleAB})
	explicit := canonicalMeshClientYAML(canonicalYAMLOptions{instanceID: "instance-b", endpoint: "other.example:9443", localHost: "localhost", localPort: 9090, token: "fedcba9876543210", caFile: bundleBAA, explicitDefaults: true, reverseSets: true})
	first := loadMeshCanonical(t, implicit)
	second := loadMeshCanonical(t, explicit)
	if !bytes.Equal(first, second) {
		t.Fatalf("equivalent declarations differ:\n%s\n%s", first, second)
	}

	loaded, err := LoadMeshClientConfig(writeTestConfig(t, implicit))
	if err != nil {
		t.Fatal(err)
	}
	copyBytes := loaded.Group.CanonicalBytes()
	copyBytes[0] ^= 0xff
	if bytes.Equal(copyBytes, loaded.Group.CanonicalBytes()) {
		t.Fatal("CanonicalBytes returned mutable internal storage")
	}

	changes := []struct {
		name string
		opts canonicalYAMLOptions
	}{
		{"policy", canonicalYAMLOptions{instanceID: "instance-a", endpoint: "edge.example:8443", localHost: "127.0.0.1", localPort: 8080, token: "0123456789abcdef", caFile: bundleAB, policy: MeshOutdatedClientPolicyPauseNewTraffic}},
		{"route", canonicalYAMLOptions{instanceID: "instance-a", endpoint: "edge.example:8443", localHost: "127.0.0.1", localPort: 8080, token: "0123456789abcdef", caFile: bundleAB, path: "/v2"}},
		{"CA", canonicalYAMLOptions{instanceID: "instance-a", endpoint: "edge.example:8443", localHost: "127.0.0.1", localPort: 8080, token: "0123456789abcdef", caFile: writeCertificateFile(t, certA)}},
	}
	for _, test := range changes {
		t.Run(test.name, func(t *testing.T) {
			if got := loadMeshCanonical(t, canonicalMeshClientYAML(test.opts)); bytes.Equal(first, got) {
				t.Fatalf("public %s change did not alter canonical bytes", test.name)
			}
		})
	}
}

func TestMeshTypedDeclarationFreezeAndStrictWireParse(t *testing.T) {
	client := validMeshClient()
	client.Group.Routes.HTTP = []MeshHTTPRoute{{
		Hostnames: []string{"z.example.com", "a.example.com"},
		Matches: []MeshHTTPMatch{{
			Headers: []MeshHTTPHeaderMatch{{Name: "X-Test", Value: "value"}},
		}},
	}}
	client.Group.OriginTLS = MeshOriginTLS{
		Enabled: true, Verify: true,
		ExtraCAFiles: []string{writeCertificateFile(t, testCertificatePEM(t, 19))},
	}
	before := CloneMeshClientConfig(&client)
	owned := CloneMeshClientConfig(&client)
	if err := FinalizeMeshClientConfig(&owned); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client, before) {
		t.Fatal("finalization changed the typed caller configuration")
	}
	canonical := owned.Group.CanonicalBytes()
	groupID, version, err := ParseMeshGroupCanonical(canonical)
	if err != nil || groupID != client.Group.GroupID || version != client.Group.RuleVersion {
		t.Fatalf("wire parse = %q/%d, %v", groupID, version, err)
	}
	_, _, work, err := ParseMeshGroupCanonicalMeasured(canonical)
	if err != nil || work.PeakBytes <= int64(len(canonical)) {
		t.Fatalf("canonical/DER validation backing = %+v, %v", work, err)
	}
	client.Group.Routes.HTTP[0].Hostnames[0] = "changed.example.com"
	client.Group.Routes.HTTP[0].Matches[0].Headers[0].Value = "changed"
	client.Group.OriginTLS.ExtraCAFiles[0] = "changed.pem"
	if !bytes.Equal(canonical, owned.Group.CanonicalBytes()) {
		t.Fatal("caller mutation changed frozen declaration")
	}

	for name, mutated := range map[string][]byte{
		"unknown field":      bytes.Replace(canonical, []byte(`"group_id":`), []byte(`"unknown":1,"group_id":`), 1),
		"duplicate field":    bytes.Replace(canonical, []byte(`"group_id":`), []byte(`"group_id":"other","group_id":`), 1),
		"non canonical":      bytes.Replace(canonical, []byte(`"metric":0`), []byte(`"metric": 0`), 1),
		"invalid DER":        bytes.Replace(canonical, []byte(`"extra_ca_certificates":[`), []byte(`"extra_ca_certificates":["YQ==",`), 1),
		"CA requires verify": bytes.Replace(canonical, []byte(`"verify":true`), []byte(`"verify":false`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(mutated, canonical) {
				t.Fatal("mutation did not change declaration")
			}
			if _, _, err := ParseMeshGroupCanonical(mutated); err == nil {
				t.Fatal("invalid declaration accepted")
			}
			if name == "invalid DER" {
				_, _, work, err := ParseMeshGroupCanonicalMeasured(mutated)
				if err == nil || work.PeakBytes == 0 {
					t.Fatalf("invalid DER work accounting = %+v, %v", work, err)
				}
			}
		})
	}
}

func TestMeshCanonicalJSONV2Escaping(t *testing.T) {
	client := validMeshClient()
	client.Group.Routes.HTTP = []MeshHTTPRoute{{
		Hostnames: []string{"api.example.com"},
		Matches: []MeshHTTPMatch{{
			Headers: []MeshHTTPHeaderMatch{{Name: "X-Test", Value: "<>&"}},
		}},
	}}
	if err := FinalizeMeshClientConfig(&client); err != nil {
		t.Fatal(err)
	}
	canonical := client.Group.CanonicalBytes()
	value := []byte(`"value":"<>&"`)
	if !bytes.Contains(canonical, value) {
		t.Fatalf("canonical declaration did not use JSON v2 escaping: %s", canonical)
	}
	if _, _, err := ParseMeshGroupCanonical(canonical); err != nil {
		t.Fatalf("parse canonical declaration: %v", err)
	}
}

func TestMeshCanonicalValidationHeapEnvelope(t *testing.T) {
	plain := validMeshClient()
	if err := FinalizeMeshClientConfig(&plain); err != nil {
		t.Fatal(err)
	}
	base := plain.Group.CanonicalBytes()
	if !bytes.Contains(base, []byte(`"http":[]`)) {
		t.Fatal("canonical fixture has no empty HTTP array")
	}
	overschema := bytes.Replace(base, []byte(`"http":[]`),
		[]byte(`"http":[`+strings.Join(slices.Repeat([]string{"{}"}, 100000), ",")+`]`), 1)

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 128)
	for i := range names {
		names[i] = fmt.Sprintf("name-%03d.%s.example", i, strings.Repeat("a", 90))
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(41), Subject: pkix.Name{CommonName: "complex-ca"},
		NotBefore: time.Unix(1, 0), NotAfter: time.Unix(2, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		DNSNames: names,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	withCA := validMeshClient()
	withCA.Group.OriginTLS = MeshOriginTLS{
		Enabled: true, Verify: true,
		ExtraCAFiles: []string{writeCertificateFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))},
	}
	if err := FinalizeMeshClientConfig(&withCA); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		input     []byte
		wantError bool
	}{
		{"overschema HTTP array", overschema, true},
		{"complex DER", withCA.Group.CanonicalBytes(), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime.GC()
			var before, sampled, after runtime.MemStats
			runtime.ReadMemStats(&before)
			peak := before.HeapAlloc
			var active int64
			_, _, work, err := ParseMeshGroupCanonicalMeasured(test.input, func(delta int64) {
				active += delta
				runtime.ReadMemStats(&sampled)
				peak = max(peak, sampled.HeapAlloc)
			})
			if (err != nil) != test.wantError {
				t.Fatalf("validation result = %v, want error %t", err, test.wantError)
			}
			if active != 0 {
				t.Fatalf("validation returned with %d owned work bytes", active)
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			peakGrowth := int64(peak) - int64(before.HeapAlloc)
			retainedGrowth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			t.Logf("input=%d sampled_heap_peak_growth=%d post_gc_retained_growth=%d explicit_byte_peak=%d", len(test.input), peakGrowth, retainedGrowth, work.PeakBytes)
			if peakGrowth > 64<<20 || retainedGrowth > 8<<20 {
				t.Fatalf("validation heap envelope exceeded: peak %d, retained %d", peakGrowth, retainedGrowth)
			}
		})
	}
}

type canonicalYAMLOptions struct {
	instanceID       string
	endpoint         string
	localHost        string
	localPort        int
	token            string
	caFile           string
	policy           string
	path             string
	explicitDefaults bool
	reverseSets      bool
}

func canonicalMeshClientYAML(options canonicalYAMLOptions) string {
	policy := ""
	probe := ""
	pathType := ""
	headerType := ""
	queryType := ""
	if options.explicitDefaults {
		policy = "  outdated_client_policy: apply_latest_rules\n"
		probe = "  probe:\n    type: tcp\n"
		pathType = "type: PathPrefix, "
		headerType = "type: Exact, "
		queryType = "type: Exact, "
	}
	if options.policy != "" {
		policy = "  outdated_client_policy: " + options.policy + "\n"
	}
	path := options.path
	if path == "" {
		path = "/v1"
	}
	hostnames := "[api.example.com, '*.api.example.com']"
	headers := fmt.Sprintf("              - {%sname: X-Environment, value: production}\n              - {%sname: x-environment, value: ignored}\n              - {%sname: X-Region, value: east}", headerType, headerType, headerType)
	queries := fmt.Sprintf("              - {%sname: region, value: east}\n              - {%sname: region, value: ignored}\n              - {%sname: Region, value: upper}", queryType, queryType, queryType)
	if options.reverseSets {
		hostnames = "['*.api.example.com', api.example.com, api.example.com]"
		headers = fmt.Sprintf("              - {%sname: X-Region, value: east}\n              - {%sname: X-Environment, value: production}", headerType, headerType)
		queries = fmt.Sprintf("              - {%sname: Region, value: upper}\n              - {%sname: region, value: east}", queryType, queryType)
	}
	return fmt.Sprintf(`instance_id: %s
admin_address: "127.0.0.1:9090"
tunnel:
  servers:
    - server_id: edge-a
      address: %q
      server_name: edge.example
  auth:
    method: token
    token: %q
  tls:
    ca_cert_file: %q
local:
  host: %q
  port: %d
group:
  group_id: api
  rule_version: 3
%s  routes:
    http:
      - hostnames: %s
        matches:
          - path: {%svalue: %s}
            method: GET
            headers:
%s
            query_params:
%s
    tls_passthrough:
      hostnames: [raw.example.com]
%s  origin_tls:
    enabled: true
    verify: true
    extra_ca_files: [%q]
`, options.instanceID, options.endpoint, options.token, "private-ca-path.pem", options.localHost, options.localPort, policy, hostnames, pathType, path, headers, queries, probe, options.caFile)
}

func validMeshServer() MeshServer {
	server := MeshServer{
		ServerID: "edge-a",
		Tunnel: MeshServerTunnel{
			Listen: MeshTunnelListen{
				Address: "127.0.0.1:8443",
				Auth:    ServerAuth{Method: "token", Token: "0123456789abcdef"},
				TLS:     ServerTLS{ServerCertFile: "server.pem", ServerKeyFile: "server-key.pem"},
			},
		},
		Ingress: MeshIngress{Listeners: []MeshIngressListener{{Address: "127.0.0.1:8080", Protocol: MeshIngressProtocolHTTP}}},
	}
	server.ApplyDefaults()
	return server
}

func validMeshClient() MeshClient {
	client := MeshClient{
		InstanceID: "instance-a",
		Tunnel: MeshClientTunnel{
			Servers: []MeshServerEndpoint{{ServerID: "edge-a", Address: "edge.example:8443"}},
			Auth:    ClientAuth{Method: ClientAuthMethodToken, Token: "0123456789abcdef"},
			TLS:     ClientTLS{CACertFile: "ca.pem"},
		},
		Local: LocalService{Host: "127.0.0.1", Port: 8080},
		Group: validMeshGroup(),
	}
	client.ApplyDefaults()
	return client
}

func validMeshGroup() MeshGroup {
	return MeshGroup{GroupID: "api", RuleVersion: 1, OutdatedClientPolicy: MeshOutdatedClientPolicyApplyLatestRules, Probe: MeshProbe{Type: MeshProbeTypeTCP}}
}

func validMeshPeering(peers ...MeshPeer) MeshPeering {
	return MeshPeering{
		Peers: peers,
		Auth:  ClientAuth{Method: ClientAuthMethodToken, Token: "0123456789abcdef"},
		TLS:   ClientTLS{CACertFile: "ca.pem"},
	}
}

func validMeshServerYAML() string {
	return `server_id: edge-a
tunnel:
  listen:
    address: "127.0.0.1:8443"
    auth:
      method: token
      token: "0123456789abcdef"
    tls:
      server_cert_file: server.pem
      server_key_file: server-key.pem
  peering:
    peers:
      - server_id: edge-b
limits:
  max_peers: 2
`
}

func validMeshClientYAML() string {
	return `instance_id: instance-a
tunnel:
  servers:
    - server_id: edge-a
      address: "edge.example:8443"
  auth:
    method: token
    token: "0123456789abcdef"
  tls:
    ca_cert_file: ca.pem
local:
  host: "127.0.0.1"
  port: 8080
group:
  group_id: api
  rule_version: 1
`
}

func loadMeshServerError(path string) error {
	_, err := LoadMeshServerConfig(path)
	return err
}

func loadMeshClientError(path string) error {
	_, err := LoadMeshClientConfig(path)
	return err
}

func assertMeshValidationError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("validation error = %v, want %q", err, want)
	}
}

func repeatValue[T any](value T, count int) []T {
	values := make([]T, count)
	for i := range values {
		values[i] = value
	}
	return values
}

func numberedHostnames(count int) []string {
	values := make([]string, count)
	for i := range values {
		values[i] = fmt.Sprintf("host-%d.example.com", i)
	}
	return values
}

func numberedHeaders(count int) []MeshHTTPHeaderMatch {
	values := make([]MeshHTTPHeaderMatch, count)
	for i := range values {
		values[i] = MeshHTTPHeaderMatch{Type: MeshValueMatchExact, Name: fmt.Sprintf("x-header-%d", i), Value: "value"}
	}
	return values
}

func numberedQueries(count int) []MeshHTTPQueryParamMatch {
	values := make([]MeshHTTPQueryParamMatch, count)
	for i := range values {
		values[i] = MeshHTTPQueryParamMatch{Type: MeshValueMatchExact, Name: fmt.Sprintf("query-%d", i), Value: "value"}
	}
	return values
}

func testCertificatePEM(t *testing.T, serial int64) []byte {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: fmt.Sprintf("test-ca-%d", serial)},
		NotBefore:             time.Unix(1, 0),
		NotAfter:              time.Unix(2, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writeCertificateFile(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadMeshCanonical(t *testing.T, content string) []byte {
	t.Helper()
	config, err := LoadMeshClientConfig(writeTestConfig(t, content))
	if err != nil {
		t.Fatalf("LoadMeshClientConfig: %v\n%s", err, content)
	}
	return config.Group.CanonicalBytes()
}
