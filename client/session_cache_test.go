package client

import "testing"

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
