package outbound

import (
	"net"
	"testing"
)

func TestSessionCacheAddressIsolation(t *testing.T) {
	manager := NewSessionCacheManager()
	first := manager.GetOrCreate("server1.example.com:8443")
	if first == nil {
		t.Fatal("GetOrCreate returned a nil cache")
	}
	if again := manager.GetOrCreate("server1.example.com:8443"); again != first {
		t.Fatal("the same address returned a different cache")
	}
	if second := manager.GetOrCreate("server2.example.com:8443"); second == first {
		t.Fatal("different addresses shared a cache")
	}
}

func TestPreferredIP(t *testing.T) {
	tests := []struct {
		name      string
		addresses []net.IPAddr
		wantIP    string
		wantZone  string
	}{
		{
			name: "IPv4 preferred after IPv6",
			addresses: []net.IPAddr{
				{IP: net.ParseIP("2001:db8::1")},
				{IP: net.ParseIP("192.0.2.10")},
			},
			wantIP: "192.0.2.10",
		},
		{
			name:      "IPv6 only",
			addresses: []net.IPAddr{{IP: net.ParseIP("2001:db8::2")}},
			wantIP:    "2001:db8::2",
		},
		{
			name:      "zoned IPv6",
			addresses: []net.IPAddr{{IP: net.ParseIP("fe80::1"), Zone: "en0"}},
			wantIP:    "fe80::1",
			wantZone:  "en0",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := PreferredIP(test.addresses)
			if got.IP.String() != test.wantIP || got.Zone != test.wantZone {
				t.Fatalf("preferred address = %s zone %q, want %s zone %q", got.IP, got.Zone, test.wantIP, test.wantZone)
			}
		})
	}
}
