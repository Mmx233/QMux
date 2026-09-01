package config

import (
	"strings"
	"testing"
	"time"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
	"github.com/quic-go/quic-go/quicvarint"
)

func TestQuicValidateBoundaries(t *testing.T) {
	tests := []struct {
		name string
		quic Quic
		path string
	}{
		{"negative keep alive", Quic{KeepAlivePeriod: -1}, "quic.keep_alive_period"},
		{"negative handshake timeout", Quic{HandshakeIdleTimeout: -1}, "quic.handshake_idle_timeout"},
		{"negative idle timeout", Quic{MaxIdleTimeout: -1}, "quic.max_idle_timeout"},
		{"handshake timeout overflow", Quic{HandshakeIdleTimeout: time.Duration(quicvarint.Max) + 1}, "quic.handshake_idle_timeout"},
		{"negative streams", Quic{MaxIncomingStreams: -1}, "quic.max_incoming_streams"},
		{"too many streams", Quic{MaxIncomingStreams: maxIncomingStreams + 1}, "quic.max_incoming_streams"},
		{"initial stream window overflow", Quic{InitialStreamReceiveWindow: quicvarint.Max + 1}, "quic.initial_stream_receive_window"},
		{"max stream window overflow", Quic{MaxStreamReceiveWindow: quicvarint.Max + 1}, "quic.max_stream_receive_window"},
		{"initial connection window overflow", Quic{InitialConnectionReceiveWindow: quicvarint.Max + 1}, "quic.initial_connection_receive_window"},
		{"max connection window overflow", Quic{MaxConnectionReceiveWindow: quicvarint.Max + 1}, "quic.max_connection_receive_window"},
		{"stream initial exceeds max", Quic{InitialStreamReceiveWindow: 2, MaxStreamReceiveWindow: 1}, "quic.initial_stream_receive_window"},
		{"connection initial exceeds max", Quic{InitialConnectionReceiveWindow: 2, MaxConnectionReceiveWindow: 1}, "quic.initial_connection_receive_window"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.quic.Validate("quic")
			if err == nil || !strings.Contains(err.Error(), test.path) {
				t.Fatalf("Validate error = %v, want path %q", err, test.path)
			}
		})
	}

	valid := []Quic{
		{},
		{HandshakeIdleTimeout: time.Duration(quicvarint.Max), MaxIncomingStreams: maxIncomingStreams},
		{InitialStreamReceiveWindow: 2},
		{InitialConnectionReceiveWindow: 2},
		{InitialStreamReceiveWindow: 2, MaxStreamReceiveWindow: 2},
		{InitialConnectionReceiveWindow: 2, MaxConnectionReceiveWindow: 2},
		{MaxStreamReceiveWindow: quicvarint.Max, MaxConnectionReceiveWindow: quicvarint.Max},
	}
	for i, quicConfig := range valid {
		if err := quicConfig.Validate("quic"); err != nil {
			t.Fatalf("valid config %d: %v", i, err)
		}
	}
}

func TestClientValidatePathsAndOrder(t *testing.T) {
	valid := validClientValidationConfig()
	tests := []struct {
		name string
		edit func(*Client)
		path string
	}{
		{"capacity", func(c *Client) { c.Capacity.MaxLocalUDPSessions = -1 }, "capacity.max_local_udp_sessions"},
		{"empty servers", func(c *Client) { c.Server.Servers = nil }, "server.servers:"},
		{"server address", func(c *Client) { c.Server.Servers[0].Address = "missing-port" }, "server.servers[0].address"},
		{"empty local host", func(c *Client) { c.Local.Host = " \t" }, "local.host"},
		{"local port zero", func(c *Client) { c.Local.Port = 0 }, "local.port"},
		{"local port too high", func(c *Client) { c.Local.Port = 65536 }, "local.port"},
		{"heartbeat interval", func(c *Client) { c.HeartbeatInterval = 0 }, "heartbeat_interval"},
		{"negative heartbeat interval", func(c *Client) { c.HeartbeatInterval = -1 }, "heartbeat_interval"},
		{"health timeout", func(c *Client) { c.HealthTimeout = 0 }, "health_timeout"},
		{"negative health timeout", func(c *Client) { c.HealthTimeout = -1 }, "health_timeout"},
		{"heartbeat relation", func(c *Client) { c.HealthTimeout = c.HeartbeatInterval }, "health_timeout"},
		{"quic", func(c *Client) { c.Quic.MaxIdleTimeout = -1 }, "quic.max_idle_timeout"},
		{"handshake overflow", func(c *Client) { c.Quic.HandshakeIdleTimeout = time.Duration(quicvarint.Max) + 1 }, "quic.handshake_idle_timeout"},
		{"auth", func(c *Client) { c.Auth.Method = "unknown" }, "auth:"},
		{"tls", func(c *Client) { c.TLS.CACertFile = "" }, "tls:"},
		{"token certificate xor", func(c *Client) {
			c.Auth = ClientAuth{Method: ClientAuthMethodToken, Token: strings.Repeat("t", sharedtoken.MinSecretSize)}
			c.TLS.ClientKeyFile = ""
		}, "tls:"},
		{"token key xor", func(c *Client) {
			c.Auth = ClientAuth{Method: ClientAuthMethodToken, Token: strings.Repeat("t", sharedtoken.MinSecretSize)}
			c.TLS.ClientCertFile = ""
		}, "tls:"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clientConfig := valid
			clientConfig.Server.Servers = append([]ServerEndpoint(nil), valid.Server.Servers...)
			test.edit(&clientConfig)
			err := clientConfig.Validate()
			if err == nil || !strings.Contains(err.Error(), test.path) {
				t.Fatalf("Validate error = %v, want path %q", err, test.path)
			}
		})
	}

	for _, port := range []int{1, 65535} {
		clientConfig := valid
		clientConfig.Local.Port = port
		if err := clientConfig.Validate(); err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
	}
}

func TestServerValidateListeners(t *testing.T) {
	valid := validServerValidationConfig()
	tests := []struct {
		name      string
		listeners []QuicListener
		path      string
	}{
		{"zero listeners", nil, "listeners"},
		{"capacity first", []QuicListener{{QuicAddr: "bad", Capacity: ListenerCapacity{MaxUDPSessions: -1}}}, "listeners[0].capacity.max_udp_sessions"},
		{"invalid QUIC address", []QuicListener{{QuicAddr: "bad", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp"}}, "listeners[0].quic_addr"},
		{"invalid traffic address", []QuicListener{{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:0", Protocol: "tcp"}}, "listeners[0].traffic_addr"},
		{"invalid protocol", []QuicListener{{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080"}}, "listeners[0].protocol"},
		{"inline QUIC", []QuicListener{{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp", MaxIncomingStreams: -1}}, "listeners[0].max_incoming_streams"},
		{"handshake overflow", []QuicListener{{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp", HandshakeIdleTimeout: time.Duration(quicvarint.Max) + 1}}, "listeners[0].handshake_idle_timeout"},
		{"duplicate QUIC address", []QuicListener{
			{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp"},
			{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8081", Protocol: "tcp"},
		}, "listeners[1].quic_addr"},
		{"overlapping traffic socket", []QuicListener{
			{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp"},
			{QuicAddr: "127.0.0.1:8444", TrafficAddr: "127.0.0.1:8080", Protocol: "both"},
		}, "listeners[1].traffic_addr"},
		{"QUIC overlaps UDP traffic", []QuicListener{
			{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp"},
			{QuicAddr: "127.0.0.1:8444", TrafficAddr: "127.0.0.1:8443", Protocol: "udp"},
		}, "listeners[1].traffic_addr"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serverConfig := valid
			serverConfig.Listeners = test.listeners
			err := serverConfig.Validate()
			if err == nil || !strings.Contains(err.Error(), test.path) {
				t.Fatalf("Validate error = %v, want path %q", err, test.path)
			}
		})
	}

	validListeners := [][]QuicListener{
		{{QuicAddr: ":8443", TrafficAddr: ":8080", Protocol: "both"}},
		{
			{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp"},
			{QuicAddr: "127.0.0.1:8444", TrafficAddr: "127.0.0.1:8080", Protocol: "udp"},
		},
	}
	for i, listeners := range validListeners {
		serverConfig := valid
		serverConfig.Listeners = listeners
		if err := serverConfig.Validate(); err != nil {
			t.Fatalf("valid listeners %d: %v", i, err)
		}
	}
}

func TestServerValidateRemainingPaths(t *testing.T) {
	valid := validServerValidationConfig()
	roundRobin := valid
	roundRobin.LoadBalancer = "round-robin"
	if err := roundRobin.Validate(); err != nil {
		t.Fatalf("round-robin: %v", err)
	}

	tests := []struct {
		name string
		edit func(*Server)
		path string
	}{
		{"load balancer", func(s *Server) { s.LoadBalancer = "random" }, "load_balancer"},
		{"heartbeat interval", func(s *Server) { s.HeartbeatInterval = 0 }, "heartbeat_interval"},
		{"negative heartbeat interval", func(s *Server) { s.HeartbeatInterval = -1 }, "heartbeat_interval"},
		{"health timeout", func(s *Server) { s.HealthTimeout = 0 }, "health_timeout"},
		{"negative health timeout", func(s *Server) { s.HealthTimeout = -1 }, "health_timeout"},
		{"heartbeat relation", func(s *Server) { s.HealthTimeout = s.HeartbeatInterval }, "health_timeout"},
		{"server certificate", func(s *Server) { s.TLS.ServerCertFile = "" }, "tls.server_cert_file"},
		{"server key", func(s *Server) { s.TLS.ServerKeyFile = "" }, "tls.server_key_file"},
		{"negative STEK", func(s *Server) { s.TLS.SessionTicketEncryptionKeyRotationInterval = -1 }, "tls.session_ticket_encryption_key_rotation_interval"},
		{"auth", func(s *Server) { s.Auth.Token = "short" }, "auth:"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serverConfig := valid
			test.edit(&serverConfig)
			err := serverConfig.Validate()
			if err == nil || !strings.Contains(err.Error(), test.path) {
				t.Fatalf("Validate error = %v, want path %q", err, test.path)
			}
		})
	}
}

func validClientValidationConfig() Client {
	return Client{
		Server:            ClientServer{Servers: []ServerEndpoint{{Address: "server.example.com:8443"}}},
		Local:             LocalService{Host: "127.0.0.1", Port: 8080},
		HeartbeatInterval: time.Second,
		HealthTimeout:     2 * time.Second,
		TLS: ClientTLS{
			CACertFile:     "ca.pem",
			ClientCertFile: "client.pem",
			ClientKeyFile:  "client-key.pem",
		},
	}
}

func validServerValidationConfig() Server {
	return Server{
		Listeners:         []QuicListener{{QuicAddr: "127.0.0.1:8443", TrafficAddr: "127.0.0.1:8080", Protocol: "tcp"}},
		LoadBalancer:      "least-connections",
		HeartbeatInterval: time.Second,
		HealthTimeout:     2 * time.Second,
		TLS:               ServerTLS{ServerCertFile: "server.pem", ServerKeyFile: "server-key.pem"},
		Auth:              ServerAuth{Method: "token", Token: strings.Repeat("t", sharedtoken.MinSecretSize)},
	}
}
