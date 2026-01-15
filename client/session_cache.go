package client

import (
	"crypto/tls"
	"sync"
)

// SessionCacheManager manages TLS session caches per server address.
// Each server gets its own isolated session cache to ensure session tickets
// encrypted by one server's STEK are not incorrectly used with another server.
type SessionCacheManager struct {
	caches sync.Map // map[string]tls.ClientSessionCache (key: "host:port")
}

// NewSessionCacheManager creates a new SessionCacheManager instance.
func NewSessionCacheManager() *SessionCacheManager {
	return &SessionCacheManager{}
}

// GetOrCreate returns the session cache for a server, creating one if needed.
// The cache is keyed by the server's address (host:port combination).
// Calling this method multiple times with the same address returns the same cache instance.
func (m *SessionCacheManager) GetOrCreate(serverAddr string) tls.ClientSessionCache {
	// Try to load existing cache first
	if cache, ok := m.caches.Load(serverAddr); ok {
		return cache.(tls.ClientSessionCache)
	}

	// Create a new cache - use LoadOrStore to handle concurrent creation
	newCache := tls.NewLRUClientSessionCache(0) // 0 means use default capacity
	actual, _ := m.caches.LoadOrStore(serverAddr, newCache)
	return actual.(tls.ClientSessionCache)
}

// Get returns the session cache for a server, or nil if not exists.
// This is useful for checking if a cache exists without creating one.
func (m *SessionCacheManager) Get(serverAddr string) tls.ClientSessionCache {
	if cache, ok := m.caches.Load(serverAddr); ok {
		return cache.(tls.ClientSessionCache)
	}
	return nil
}

// Clear removes the session cache for a server.
// This should be used when a server's session cache needs to be invalidated,
// such as when session cache corruption is detected.
func (m *SessionCacheManager) Clear(serverAddr string) {
	m.caches.Delete(serverAddr)
}

// Count returns the number of session caches currently managed.
// This is primarily useful for testing.
func (m *SessionCacheManager) Count() int {
	count := 0
	m.caches.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

// Addresses returns all server addresses that have session caches.
// This is primarily useful for testing and debugging.
func (m *SessionCacheManager) Addresses() []string {
	var addresses []string
	m.caches.Range(func(key, _ interface{}) bool {
		addresses = append(addresses, key.(string))
		return true
	})
	return addresses
}
