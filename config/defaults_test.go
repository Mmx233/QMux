package config

import (
	"testing"
	"time"
)

func TestApplyDefaults(t *testing.T) {
	client := Client{}
	client.ApplyDefaults()
	if client.ClientID == "" || client.Auth.Method != ClientAuthMethodMTLS ||
		client.HeartbeatInterval != DefaultHeartbeatInterval || client.HealthTimeout != DefaultHealthTimeout ||
		client.TCPCopyBufferSize != DefaultTCPCopyBufferSize ||
		client.Capacity != defaultClientCapacity() {
		t.Fatalf("client defaults = %+v", client)
	}

	server := Server{Listeners: []QuicListener{{}}}
	server.ApplyDefaults()
	if server.HeartbeatInterval != DefaultHeartbeatInterval || server.HealthTimeout != DefaultHealthTimeout ||
		server.LoadBalancer != DefaultLoadBalancer || server.Listeners[0].Capacity != defaultListenerCapacity() ||
		server.TCPCopyBufferSize != DefaultTCPCopyBufferSize ||
		server.TLS.SessionTicketEncryptionKeyRotationOverlap != nil {
		t.Fatalf("server defaults = %+v", server)
	}

	if got := (Quic{}).GetConfig().MaxIdleTimeout; got != DefaultMaxIdleTimeout {
		t.Fatalf("QUIC idle timeout = %v, want %v", got, DefaultMaxIdleTimeout)
	}
}

func TestApplyDefaultsPreservesValues(t *testing.T) {
	client := Client{
		ClientID:          "client-id",
		Auth:              ClientAuth{Method: ClientAuthMethodToken},
		TCPCopyBufferSize: 11,
		Capacity: ClientCapacity{
			MaxLocalUDPSessions:                  7,
			MaxUDPFragmentGroupsPerHandler:       8,
			MaxUDPFragmentBackingBytesPerHandler: 9,
		},
		HeartbeatInterval: time.Second,
		HealthTimeout:     2 * time.Second,
	}
	client.ApplyDefaults()
	if client.ClientID != "client-id" || client.Auth.Method != ClientAuthMethodToken ||
		client.HeartbeatInterval != time.Second || client.HealthTimeout != 2*time.Second ||
		client.TCPCopyBufferSize != 11 || client.Capacity.MaxLocalUDPSessions != 7 ||
		client.Capacity.MaxUDPFragmentGroupsPerHandler != 8 ||
		client.Capacity.MaxUDPFragmentBackingBytesPerHandler != 9 {
		t.Fatalf("client values changed: %+v", client)
	}

	wantCapacity := ListenerCapacity{
		MaxClientGenerations: 1, MaxPendingRegistrations: 2, MaxTCPConnections: 3, MaxPendingTCPSetups: 4,
		MaxTCPConnectionsPerGeneration: 5, MaxPendingTCPSetupsPerGeneration: 6,
		MaxUDPSessions: 7, MaxUDPSessionsPerGeneration: 8,
		MaxUDPSenderQueuedFramesPerGeneration: 9, MaxUDPSenderQueuedBackingBytesPerGeneration: 10,
		MaxUDPFragmentGroups: 11, MaxUDPFragmentBackingBytes: 12,
	}
	overlap := uint8(2)
	server := Server{
		Listeners: []QuicListener{{Capacity: wantCapacity}}, LoadBalancer: "round-robin", TCPCopyBufferSize: 13,
		HeartbeatInterval: time.Second, HealthTimeout: 2 * time.Second,
		TLS: ServerTLS{
			SessionTicketEncryptionKeyRotationInterval: time.Hour,
			SessionTicketEncryptionKeyRotationOverlap:  &overlap,
		},
	}
	server.ApplyDefaults()
	if server.HeartbeatInterval != time.Second || server.HealthTimeout != 2*time.Second ||
		server.LoadBalancer != "round-robin" || server.Listeners[0].Capacity != wantCapacity ||
		server.TCPCopyBufferSize != 13 ||
		server.TLS.SessionTicketEncryptionKeyRotationOverlap != &overlap {
		t.Fatalf("server values changed: %+v", server)
	}

	if got := (Quic{MaxIdleTimeout: time.Second}).GetConfig().MaxIdleTimeout; got != time.Second {
		t.Fatalf("QUIC idle timeout changed to %v", got)
	}
}

func TestServerTLSRotationOldKeyLimit(t *testing.T) {
	tests := []struct {
		name  string
		value *uint8
		want  uint8
	}{
		{name: "omitted", want: DefaultSessionTicketEncryptionKeyRotationOverlap},
		{name: "zero", value: new(uint8(0)), want: 0},
		{name: "one", value: new(uint8(1)), want: 1},
		{name: "two", value: new(uint8(2)), want: 2},
		{name: "six", value: new(uint8(6)), want: 6},
		{name: "seven", value: new(uint8(7)), want: 7},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tlsConfig := ServerTLS{SessionTicketEncryptionKeyRotationOverlap: test.value}
			if got := tlsConfig.RotationOldKeyLimit(); got != test.want {
				t.Fatalf("RotationOldKeyLimit() = %d, want %d", got, test.want)
			}
			if tlsConfig.SessionTicketEncryptionKeyRotationOverlap != test.value {
				t.Fatal("RotationOldKeyLimit mutated overlap presence")
			}
		})
	}
}

func TestCapacityValidation(t *testing.T) {
	tests := []struct {
		name string
		set  func(*ListenerCapacity)
	}{
		{"max_client_generations", func(c *ListenerCapacity) { c.MaxClientGenerations = -1 }},
		{"max_pending_registrations", func(c *ListenerCapacity) { c.MaxPendingRegistrations = -1 }},
		{"max_tcp_connections", func(c *ListenerCapacity) { c.MaxTCPConnections = -1 }},
		{"max_pending_tcp_setups", func(c *ListenerCapacity) { c.MaxPendingTCPSetups = -1 }},
		{"max_tcp_connections_per_generation", func(c *ListenerCapacity) { c.MaxTCPConnectionsPerGeneration = -1 }},
		{"max_pending_tcp_setups_per_generation", func(c *ListenerCapacity) { c.MaxPendingTCPSetupsPerGeneration = -1 }},
		{"max_udp_sessions", func(c *ListenerCapacity) { c.MaxUDPSessions = -1 }},
		{"max_udp_sessions_per_generation", func(c *ListenerCapacity) { c.MaxUDPSessionsPerGeneration = -1 }},
		{"max_udp_sender_queued_frames_per_generation", func(c *ListenerCapacity) { c.MaxUDPSenderQueuedFramesPerGeneration = -1 }},
		{"max_udp_sender_queued_backing_bytes_per_generation", func(c *ListenerCapacity) { c.MaxUDPSenderQueuedBackingBytesPerGeneration = -1 }},
		{"max_udp_fragment_groups", func(c *ListenerCapacity) { c.MaxUDPFragmentGroups = -1 }},
		{"max_udp_fragment_backing_bytes", func(c *ListenerCapacity) { c.MaxUDPFragmentBackingBytes = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var capacity ListenerCapacity
			test.set(&capacity)
			want := "listeners[3].capacity." + test.name + " must not be negative"
			if err := capacity.Validate("listeners[3].capacity"); err == nil || err.Error() != want {
				t.Fatalf("Validate() error = %v, want %q", err, want)
			}
		})
	}

	if err := (ListenerCapacity{}).Validate("listeners[0].capacity"); err != nil {
		t.Fatalf("zero listener capacity validation: %v", err)
	}
	if err := (ClientCapacity{}).Validate("capacity"); err != nil {
		t.Fatalf("zero client capacity validation: %v", err)
	}
	clientTests := []struct {
		name string
		set  func(*ClientCapacity)
	}{
		{"max_local_udp_sessions", func(c *ClientCapacity) { c.MaxLocalUDPSessions = -1 }},
		{"max_udp_fragment_groups_per_handler", func(c *ClientCapacity) { c.MaxUDPFragmentGroupsPerHandler = -1 }},
		{"max_udp_fragment_backing_bytes_per_handler", func(c *ClientCapacity) { c.MaxUDPFragmentBackingBytesPerHandler = -1 }},
	}
	for _, test := range clientTests {
		t.Run("client "+test.name, func(t *testing.T) {
			var capacity ClientCapacity
			test.set(&capacity)
			want := "capacity." + test.name + " must not be negative"
			if err := capacity.Validate("capacity"); err == nil || err.Error() != want {
				t.Fatalf("Validate() error = %v, want %q", err, want)
			}
		})
	}
}

func defaultListenerCapacity() ListenerCapacity {
	return ListenerCapacity{
		MaxClientGenerations:                        DefaultMaxClientGenerations,
		MaxPendingRegistrations:                     DefaultMaxPendingRegistrations,
		MaxTCPConnections:                           DefaultMaxTCPConnections,
		MaxPendingTCPSetups:                         DefaultMaxPendingTCPSetups,
		MaxTCPConnectionsPerGeneration:              DefaultMaxTCPConnectionsPerGeneration,
		MaxPendingTCPSetupsPerGeneration:            DefaultMaxPendingTCPSetupsPerGeneration,
		MaxUDPSessions:                              DefaultMaxUDPSessions,
		MaxUDPSessionsPerGeneration:                 DefaultMaxUDPSessionsPerGeneration,
		MaxUDPSenderQueuedFramesPerGeneration:       DefaultMaxUDPSenderQueuedFramesPerGeneration,
		MaxUDPSenderQueuedBackingBytesPerGeneration: DefaultMaxUDPSenderQueuedBackingBytesPerGeneration,
		MaxUDPFragmentGroups:                        DefaultMaxUDPFragmentGroups,
		MaxUDPFragmentBackingBytes:                  DefaultMaxUDPFragmentBackingBytes,
	}
}

func defaultClientCapacity() ClientCapacity {
	return ClientCapacity{
		MaxLocalUDPSessions:                  DefaultMaxLocalUDPSessions,
		MaxUDPFragmentGroupsPerHandler:       DefaultMaxUDPFragmentGroupsPerHandler,
		MaxUDPFragmentBackingBytesPerHandler: DefaultMaxUDPFragmentBackingBytesPerHandler,
	}
}
