// Package outbound contains the concrete connection primitives shared by
// ordinary clients, mesh clients, and active mesh peers.
package outbound

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	InitialReconnectDelay = 5 * time.Second
	MaxReconnectDelay     = 60 * time.Second
	ReconnectStableGrace  = 60 * time.Second
	MaxReconnectStage     = 4
	AttemptTimeout        = 30 * time.Second
)

// SessionCacheManager isolates TLS session tickets by configured endpoint.
type SessionCacheManager struct {
	caches sync.Map // map[string]tls.ClientSessionCache
}

func NewSessionCacheManager() *SessionCacheManager {
	return &SessionCacheManager{}
}

func (m *SessionCacheManager) GetOrCreate(address string) tls.ClientSessionCache {
	if cache, ok := m.caches.Load(address); ok {
		return cache.(tls.ClientSessionCache)
	}
	cache, _ := m.caches.LoadOrStore(address, tls.NewLRUClientSessionCache(0))
	return cache.(tls.ClientSessionCache)
}

func (m *SessionCacheManager) Count() int {
	count := 0
	m.caches.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// ResolveAddress resolves one hostname while preserving the original host for
// TLS ServerName inference. IPv4 is preferred to retain ordinary client behavior.
func ResolveAddress(ctx context.Context, resolver *net.Resolver, address string) (string, string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", "", err
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return address, host, nil
	}
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", "", err
	}
	if len(addresses) == 0 {
		return "", "", fmt.Errorf("host %q resolved without an address", host)
	}
	selected := PreferredIP(addresses)
	return net.JoinHostPort(selected.String(), port), host, nil
}

func PreferredIP(addresses []net.IPAddr) net.IPAddr {
	for _, address := range addresses {
		if address.IP.To4() != nil {
			return address
		}
	}
	return addresses[0]
}

// Dial establishes one QUIC connection with endpoint-isolated TLS resumption.
func Dial(
	ctx context.Context,
	address, serverName string,
	baseTLSConfig *tls.Config,
	sessionCache tls.ClientSessionCache,
	quicConfig *quic.Config,
) (*quic.Conn, error) {
	tlsConfig := baseTLSConfig.Clone()
	tlsConfig.ServerName = serverName
	tlsConfig.ClientSessionCache = sessionCache
	dialAddress, originalHost, err := ResolveAddress(ctx, net.DefaultResolver, address)
	if err != nil {
		return nil, fmt.Errorf("resolve server %s: %w", address, err)
	}
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = originalHost
	}
	conn, err := quic.DialAddr(ctx, dialAddress, tlsConfig, quicConfig)
	if err != nil {
		return nil, fmt.Errorf("dial server %s: %w", address, err)
	}
	return conn, nil
}

// AttemptContext combines a caller lifetime, an owner lifetime, and one fixed deadline.
func AttemptContext(parent, owner context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	stopOwnerCancellation := context.AfterFunc(owner, cancel)
	if owner.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stopOwnerCancellation()
		cancel()
	}
}

// ReconnectDelay returns the existing bounded equal-jitter delay for a retry stage.
func ReconnectDelay(stage int, int64n func(int64) int64) time.Duration {
	delayCap := InitialReconnectDelay
	for stage > 0 && delayCap < MaxReconnectDelay {
		delayCap = min(delayCap*2, MaxReconnectDelay)
		stage--
	}
	half := delayCap / 2
	return half + time.Duration(int64n(int64(half)))
}

// WaitReconnect waits for a delay unless either caller or owner stops first.
func WaitReconnect(parent, owner context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-parent.Done():
	case <-owner.Done():
	case <-timer.C:
	}
	return parent.Err() == nil && owner.Err() == nil
}
