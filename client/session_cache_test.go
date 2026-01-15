package client

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

// Feature: multi-server-client, Property 4: Session Cache Uniqueness
// For any set of N distinct server addresses, the SessionCacheManager SHALL create
// exactly N distinct TLS session cache instances, one per address.
// Validates: Requirements 2.1, 2.5
func TestSessionCacheUniqueness_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate N distinct server addresses (1-10)
		n := rapid.IntRange(1, 10).Draw(t, "addressCount")
		addresses := make([]string, n)
		for i := 0; i < n; i++ {
			addresses[i] = fmt.Sprintf("server%d.example.com:%d", i, 8443+i)
		}

		manager := NewSessionCacheManager()

		// Create caches for all addresses
		caches := make(map[string]interface{})
		for _, addr := range addresses {
			cache := manager.GetOrCreate(addr)
			caches[addr] = cache
		}

		// Property: exactly N distinct caches should be created
		if manager.Count() != n {
			t.Fatalf("expected %d caches, got %d", n, manager.Count())
		}

		// Property: each address should have a unique cache instance
		// We verify this by checking that all caches are non-nil and stored
		for _, addr := range addresses {
			cache := manager.Get(addr)
			if cache == nil {
				t.Fatalf("cache for address %q should not be nil", addr)
			}
		}
	})
}

// Feature: multi-server-client, Property 5: Session Cache Address Keying
// For any server address A, calling GetOrCreate(A) multiple times SHALL return the same
// cache instance. For any two different addresses A and B, GetOrCreate(A) and GetOrCreate(B)
// SHALL return different cache instances.
// Validates: Requirements 2.2, 2.3
func TestSessionCacheAddressKeying_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		manager := NewSessionCacheManager()

		// Generate two distinct addresses
		addrA := rapid.StringMatching(`server[0-9]+\.example\.com:[0-9]{4,5}`).Draw(t, "addressA")
		addrB := rapid.StringMatching(`server[0-9]+\.example\.com:[0-9]{4,5}`).Draw(t, "addressB")

		// Get cache for address A multiple times
		cacheA1 := manager.GetOrCreate(addrA)
		cacheA2 := manager.GetOrCreate(addrA)
		cacheA3 := manager.GetOrCreate(addrA)

		// Property: same address returns same cache instance
		if cacheA1 != cacheA2 || cacheA2 != cacheA3 {
			t.Fatalf("GetOrCreate(%q) should return the same cache instance on multiple calls", addrA)
		}

		// Only test different addresses if they are actually different
		if addrA != addrB {
			cacheB := manager.GetOrCreate(addrB)

			// Property: different addresses return different cache instances
			if cacheA1 == cacheB {
				t.Fatalf("GetOrCreate(%q) and GetOrCreate(%q) should return different cache instances", addrA, addrB)
			}
		}
	})
}

// Unit test: GetOrCreate creates cache if not exists
func TestSessionCacheManager_GetOrCreate(t *testing.T) {
	manager := NewSessionCacheManager()

	// Initially no caches
	if manager.Count() != 0 {
		t.Fatalf("expected 0 caches initially, got %d", manager.Count())
	}

	// GetOrCreate should create a cache
	cache := manager.GetOrCreate("server1:8443")
	if cache == nil {
		t.Fatal("GetOrCreate should return a non-nil cache")
	}

	// Count should be 1
	if manager.Count() != 1 {
		t.Fatalf("expected 1 cache after GetOrCreate, got %d", manager.Count())
	}
}

// Unit test: Get returns nil for non-existent cache
func TestSessionCacheManager_Get_NotExists(t *testing.T) {
	manager := NewSessionCacheManager()

	cache := manager.Get("nonexistent:8443")
	if cache != nil {
		t.Fatal("Get should return nil for non-existent cache")
	}
}

// Unit test: Get returns existing cache
func TestSessionCacheManager_Get_Exists(t *testing.T) {
	manager := NewSessionCacheManager()

	// Create a cache first
	created := manager.GetOrCreate("server1:8443")

	// Get should return the same cache
	retrieved := manager.Get("server1:8443")
	if retrieved != created {
		t.Fatal("Get should return the same cache instance as GetOrCreate")
	}
}

// Unit test: Clear removes cache
func TestSessionCacheManager_Clear(t *testing.T) {
	manager := NewSessionCacheManager()

	// Create a cache
	manager.GetOrCreate("server1:8443")
	if manager.Count() != 1 {
		t.Fatalf("expected 1 cache, got %d", manager.Count())
	}

	// Clear the cache
	manager.Clear("server1:8443")

	// Cache should be gone
	if manager.Get("server1:8443") != nil {
		t.Fatal("cache should be nil after Clear")
	}
	if manager.Count() != 0 {
		t.Fatalf("expected 0 caches after Clear, got %d", manager.Count())
	}
}

// Unit test: Addresses returns all managed addresses
func TestSessionCacheManager_Addresses(t *testing.T) {
	manager := NewSessionCacheManager()

	addresses := []string{"server1:8443", "server2:8443", "server3:8443"}
	for _, addr := range addresses {
		manager.GetOrCreate(addr)
	}

	result := manager.Addresses()
	if len(result) != len(addresses) {
		t.Fatalf("expected %d addresses, got %d", len(addresses), len(result))
	}

	// Check all addresses are present
	resultMap := make(map[string]bool)
	for _, addr := range result {
		resultMap[addr] = true
	}
	for _, addr := range addresses {
		if !resultMap[addr] {
			t.Errorf("address %q not found in result", addr)
		}
	}
}
