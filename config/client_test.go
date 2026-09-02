package config

import (
	"fmt"
	"testing"
	"time"
)

func TestClientServerValidation(t *testing.T) {
	if err := (&ClientServer{}).Validate(); err == nil {
		t.Fatal("empty server list was accepted")
	}

	servers := make([]ServerEndpoint, 32)
	for i := range servers {
		servers[i].Address = fmt.Sprintf("server%d.example.com:%d", i, 8000+i)
	}
	if err := (&ClientServer{Servers: servers}).Validate(); err != nil {
		t.Fatalf("32 valid servers: %v", err)
	}
}

func TestValidateAddress(t *testing.T) {
	tests := []struct {
		address string
		valid   bool
	}{
		{address: "server.example.com:8443", valid: true},
		{address: "127.0.0.1:1", valid: true},
		{address: "[::1]:65535", valid: true},
		{address: ""},
		{address: "noport"},
		{address: ":8080"},
		{address: "host:"},
		{address: "host:0"},
		{address: "host:65536"},
		{address: "host:abc"},
		{address: "host:-1"},
		{address: "host:8080:extra"},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			err := ValidateAddress(test.address)
			if (err == nil) != test.valid {
				t.Fatalf("ValidateAddress(%q) error = %v", test.address, err)
			}
		})
	}
}

func TestClientServerDeduplicate(t *testing.T) {
	servers := &ClientServer{Servers: []ServerEndpoint{
		{Address: "a.example.com:8443", ServerName: "first"},
		{Address: "b.example.com:8443", ServerName: "second"},
		{Address: "a.example.com:8443", ServerName: "duplicate"},
	}}
	got, duplicates := servers.DeduplicateServers()
	if !duplicates || len(got) != 2 {
		t.Fatalf("DeduplicateServers() = %+v, %t", got, duplicates)
	}
	if got[0].ServerName != "first" || got[1].ServerName != "second" {
		t.Fatalf("deduplicated order = %+v", got)
	}
}

func TestClientHeartbeatValidation(t *testing.T) {
	for _, test := range []struct {
		name              string
		heartbeat, health time.Duration
		valid             bool
	}{
		{name: "health greater", heartbeat: time.Second, health: 2 * time.Second, valid: true},
		{name: "equal", heartbeat: time.Second, health: time.Second},
		{name: "health smaller", heartbeat: 2 * time.Second, health: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := validClientValidationConfig()
			client.HeartbeatInterval = test.heartbeat
			client.HealthTimeout = test.health
			err := client.Validate()
			if (err == nil) != test.valid {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}
