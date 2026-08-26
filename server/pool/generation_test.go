// Generation tests exercise pointer identity independently of QUIC connections.
package pool

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenerationCurrentMutators(t *testing.T) {
	pool := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	registeredAt := time.Unix(100, 0)
	client := &ClientConn{
		ID:           "client",
		RegisteredAt: registeredAt,
		LastSeen:     time.Unix(1, 0),
	}
	if err := pool.Add(client); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if !client.healthy.Load() {
		t.Fatal("Add() did not mark the client healthy")
	}

	beforeUpdate := time.Now()
	if !pool.UpdateLastSeen(client) {
		t.Fatal("UpdateLastSeen() = false for current generation")
	}
	if client.LastSeen.Before(beforeUpdate) {
		t.Fatalf("UpdateLastSeen() timestamp = %v, want >= %v", client.LastSeen, beforeUpdate)
	}

	warmGenerationCache(t, pool, client)
	if !pool.MarkUnhealthy(client) {
		t.Fatal("MarkUnhealthy() = false for current generation")
	}
	if client.healthy.Load() {
		t.Fatal("MarkUnhealthy() left client healthy")
	}
	if pool.cachedClients.Load() != nil {
		t.Fatal("MarkUnhealthy() did not invalidate the cache")
	}

	warmGenerationCache(t, pool, client)
	if !pool.MarkHealthy(client) {
		t.Fatal("MarkHealthy() = false for current generation")
	}
	if !client.healthy.Load() {
		t.Fatal("MarkHealthy() left client unhealthy")
	}
	if pool.cachedClients.Load() != nil {
		t.Fatal("MarkHealthy() did not invalidate the cache")
	}

	warmGenerationCache(t, pool, client)
	if !pool.Remove(client) {
		t.Fatal("Remove() = false for current generation")
	}
	if client.healthy.Load() {
		t.Fatal("Remove() did not mark the removed generation unhealthy")
	}
	if pool.Count() != 0 {
		t.Fatalf("Count() = %d, want 0", pool.Count())
	}
	if pool.cachedClients.Load() != nil {
		t.Fatal("Remove() did not invalidate the cache")
	}
}

func TestGenerationStaleMutatorsAreNoOps(t *testing.T) {
	pool := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	stale := &ClientConn{ID: "client", LastSeen: time.Unix(1, 0)}
	if err := pool.Add(stale); err != nil {
		t.Fatalf("Add(stale) error = %v", err)
	}
	if !pool.Remove(stale) {
		t.Fatal("Remove(stale) = false while it is current")
	}

	currentLastSeen := time.Unix(2, 0)
	current := &ClientConn{ID: stale.ID, LastSeen: currentLastSeen}
	if err := pool.Add(current); err != nil {
		t.Fatalf("Add(current) error = %v", err)
	}

	cached := warmGenerationCache(t, pool, current)
	staleLastSeen := stale.LastSeen
	if pool.UpdateLastSeen(stale) {
		t.Fatal("UpdateLastSeen(stale) = true")
	}
	if stale.LastSeen != staleLastSeen || current.LastSeen != currentLastSeen {
		t.Fatalf("stale UpdateLastSeen changed timestamps: stale=%v current=%v", stale.LastSeen, current.LastSeen)
	}
	assertGenerationCacheUnchanged(t, pool, cached)

	if pool.MarkUnhealthy(stale) {
		t.Fatal("MarkUnhealthy(stale) = true")
	}
	if !current.healthy.Load() {
		t.Fatal("stale MarkUnhealthy changed the current generation")
	}
	assertGenerationCacheUnchanged(t, pool, cached)

	if !pool.MarkUnhealthy(current) {
		t.Fatal("MarkUnhealthy(current) = false")
	}
	cached = warmGenerationCache(t, pool, current)
	if pool.MarkHealthy(stale) {
		t.Fatal("MarkHealthy(stale) = true")
	}
	if current.healthy.Load() {
		t.Fatal("stale MarkHealthy changed the current generation")
	}
	assertGenerationCacheUnchanged(t, pool, cached)

	if pool.Remove(stale) {
		t.Fatal("Remove(stale) = true")
	}
	if pool.Count() != 1 {
		t.Fatalf("Count() = %d after stale Remove, want 1", pool.Count())
	}
	if got, ok := pool.Get(current.ID); !ok || got != current {
		t.Fatalf("Get() after stale Remove = (%p, %v), want (%p, true)", got, ok, current)
	}
	if current.healthy.Load() {
		t.Fatal("stale Remove changed the current generation's health")
	}
	assertGenerationCacheUnchanged(t, pool, cached)
}

func TestGenerationNilEmptyAndUnknownAreNoOps(t *testing.T) {
	pool := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	if err := pool.Add(nil); err == nil {
		t.Fatal("Add(nil) error = nil")
	}
	empty := &ClientConn{}
	if err := pool.Add(empty); err == nil {
		t.Fatal("Add(empty ID) error = nil")
	}
	if empty.added.Load() {
		t.Fatal("failed Add(empty ID) consumed the pointer")
	}

	current := &ClientConn{ID: "current", LastSeen: time.Unix(1, 0)}
	if err := pool.Add(current); err != nil {
		t.Fatalf("Add(current) error = %v", err)
	}
	cached := warmGenerationCache(t, pool, current)
	unknown := &ClientConn{ID: "unknown", LastSeen: time.Unix(2, 0)}

	tests := []struct {
		name      string
		candidate *ClientConn
	}{
		{name: "nil"},
		{name: "empty", candidate: empty},
		{name: "unknown", candidate: unknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := test.candidate
			lastSeen := candidateLastSeen(candidate)
			if pool.UpdateLastSeen(candidate) {
				t.Fatal("UpdateLastSeen() = true")
			}
			if pool.MarkUnhealthy(candidate) {
				t.Fatal("MarkUnhealthy() = true")
			}
			if pool.MarkHealthy(candidate) {
				t.Fatal("MarkHealthy() = true")
			}
			if pool.Remove(candidate) {
				t.Fatal("Remove() = true")
			}
			if candidateLastSeen(candidate) != lastSeen {
				t.Fatal("no-op mutators changed candidate LastSeen")
			}
			if !current.healthy.Load() {
				t.Fatal("no-op mutators changed current health")
			}
			assertGenerationCacheUnchanged(t, pool, cached)
		})
	}

	empty.ID = "reusable"
	if err := pool.Add(empty); err != nil {
		t.Fatalf("Add() after failed empty-ID Add error = %v", err)
	}
}

func TestGenerationClientConnPointerIsSingleUse(t *testing.T) {
	first := New("first", NewRoundRobinBalancer(), newTestLogger())
	defer first.Stop()
	second := New("second", NewRoundRobinBalancer(), newTestLogger())
	defer second.Stop()

	client := &ClientConn{ID: "client"}
	if err := first.Add(client); err != nil {
		t.Fatalf("first Add() error = %v", err)
	}
	if !first.Remove(client) {
		t.Fatal("first Remove() = false")
	}
	if err := first.Add(client); err == nil {
		t.Fatal("same-pool Add() reused a removed pointer")
	}
	if err := second.Add(client); err == nil {
		t.Fatal("cross-pool Add() reused a removed pointer")
	}
	if first.Count() != 0 || second.Count() != 0 {
		t.Fatalf("single-use failures changed pool counts: first=%d second=%d", first.Count(), second.Count())
	}
}

func TestGenerationConcurrentAddClaimsPointerOnce(t *testing.T) {
	first := New("first", NewRoundRobinBalancer(), newTestLogger())
	defer first.Stop()
	second := New("second", NewLeastConnectionsBalancer(), newTestLogger())
	defer second.Stop()

	client := &ClientConn{ID: "client"}
	start := make(chan struct{})
	results := make(chan bool, 2)
	for _, pool := range []*ConnectionPool{first, second} {
		go func(pool *ConnectionPool) {
			<-start
			results <- pool.Add(client) == nil
		}(pool)
	}
	close(start)

	successes := 0
	for range 2 {
		select {
		case success := <-results:
			if success {
				successes++
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent Add() did not complete")
		}
	}
	if successes != 1 {
		t.Fatalf("successful Add() calls = %d, want 1", successes)
	}
	if first.Count()+second.Count() != 1 {
		t.Fatalf("combined pool count = %d, want 1", first.Count()+second.Count())
	}
}

func TestGenerationFailedDuplicateAddDoesNotConsumePointer(t *testing.T) {
	pool := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()

	current := &ClientConn{ID: "client"}
	candidate := &ClientConn{ID: current.ID}
	if err := pool.Add(current); err != nil {
		t.Fatalf("Add(current) error = %v", err)
	}
	if err := pool.Add(candidate); err == nil {
		t.Fatal("Add(candidate) duplicate error = nil")
	}
	if candidate.added.Load() {
		t.Fatal("failed duplicate Add consumed candidate pointer")
	}
	if pool.Remove(candidate) {
		t.Fatal("failed duplicate candidate acquired a cleanup obligation")
	}
	if got, ok := pool.Get(current.ID); !ok || got != current {
		t.Fatalf("duplicate cleanup changed current entry: got (%p, %v), want (%p, true)", got, ok, current)
	}

	if !pool.Remove(current) {
		t.Fatal("Remove(current) = false")
	}
	if err := pool.Add(candidate); err != nil {
		t.Fatalf("Add(candidate) after duplicate cleared error = %v", err)
	}
	if !pool.Remove(candidate) {
		t.Fatal("Remove(candidate) = false after successful Add")
	}
}

func TestGenerationInterleavingsAcrossBalancers(t *testing.T) {
	balancers := []struct {
		name string
		new  func() LoadBalancer
	}{
		{name: "round-robin", new: func() LoadBalancer { return NewRoundRobinBalancer() }},
		{name: "least-connections", new: func() LoadBalancer { return NewLeastConnectionsBalancer() }},
	}
	tests := []struct {
		name      string
		beforeAdd func(t *testing.T, pool *ConnectionPool, stale *ClientConn)
		afterAdd  func(t *testing.T, pool *ConnectionPool, stale *ClientConn)
	}{
		{
			name: "held old TCP pointer failure and cleanup",
			afterAdd: func(t *testing.T, pool *ConnectionPool, stale *ClientConn) {
				if pool.MarkUnhealthy(stale) || pool.Remove(stale) {
					t.Fatal("late stale completion succeeded")
				}
			},
		},
		{
			name: "timeout then double cleanup",
			beforeAdd: func(t *testing.T, pool *ConnectionPool, stale *ClientConn) {
				if !pool.MarkUnhealthy(stale) {
					t.Fatal("current timeout MarkUnhealthy() = false")
				}
			},
			afterAdd: func(t *testing.T, pool *ConnectionPool, stale *ClientConn) {
				if pool.Remove(stale) {
					t.Fatal("second cleanup removed the replacement")
				}
			},
		},
		{
			name: "all stale mutator orders",
			afterAdd: func(t *testing.T, pool *ConnectionPool, stale *ClientConn) {
				if pool.MarkHealthy(stale) || pool.UpdateLastSeen(stale) || pool.Remove(stale) || pool.MarkUnhealthy(stale) {
					t.Fatal("stale mutator succeeded")
				}
			},
		},
	}

	for _, balancer := range balancers {
		for _, test := range tests {
			t.Run(balancer.name+"/"+test.name, func(t *testing.T) {
				pool := New("test", balancer.new(), newTestLogger())
				defer pool.Stop()

				stale := &ClientConn{ID: "client", LastSeen: time.Unix(1, 0)}
				if err := pool.Add(stale); err != nil {
					t.Fatalf("Add(stale) error = %v", err)
				}
				if test.beforeAdd != nil {
					test.beforeAdd(t, pool, stale)
				}
				if !pool.Remove(stale) {
					t.Fatal("Remove(stale) = false while current")
				}

				current := &ClientConn{ID: stale.ID, LastSeen: time.Unix(2, 0)}
				if err := pool.Add(current); err != nil {
					t.Fatalf("Add(current) error = %v", err)
				}
				if test.afterAdd != nil {
					test.afterAdd(t, pool, stale)
				}

				selected, err := pool.Select()
				if err != nil {
					t.Fatalf("Select() error = %v", err)
				}
				if selected != current {
					t.Fatalf("Select() = %p, want current generation %p", selected, current)
				}
				if !current.healthy.Load() {
					t.Fatal("stale interleaving changed current health")
				}
			})
		}
	}
}

func TestGenerationConcurrentStaleCompletionsAreBounded(t *testing.T) {
	balancers := []struct {
		name string
		new  func() LoadBalancer
	}{
		{name: "round-robin", new: func() LoadBalancer { return NewRoundRobinBalancer() }},
		{name: "least-connections", new: func() LoadBalancer { return NewLeastConnectionsBalancer() }},
	}

	for _, balancer := range balancers {
		t.Run(balancer.name, func(t *testing.T) {
			pool := New("test", balancer.new(), newTestLogger())
			defer pool.Stop()

			stale := &ClientConn{ID: "client", LastSeen: time.Unix(1, 0)}
			if err := pool.Add(stale); err != nil {
				t.Fatalf("Add(stale) error = %v", err)
			}
			if !pool.Remove(stale) {
				t.Fatal("Remove(stale) = false while current")
			}
			current := &ClientConn{ID: stale.ID, LastSeen: time.Unix(2, 0)}
			if err := pool.Add(current); err != nil {
				t.Fatalf("Add(current) error = %v", err)
			}
			cached := warmGenerationCache(t, pool, current)

			const workers = 128
			start := make(chan struct{})
			var wg sync.WaitGroup
			var unexpectedSuccesses atomic.Int64
			for i := range workers {
				wg.Go(func() {
					<-start
					var succeeded bool
					switch i % 4 {
					case 0:
						succeeded = pool.Remove(stale)
					case 1:
						succeeded = pool.MarkUnhealthy(stale)
					case 2:
						succeeded = pool.MarkHealthy(stale)
					case 3:
						succeeded = pool.UpdateLastSeen(stale)
					}
					if succeeded {
						unexpectedSuccesses.Add(1)
					}
				})
			}
			close(start)
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("stale completions did not finish within 2s")
			}

			if got := unexpectedSuccesses.Load(); got != 0 {
				t.Fatalf("successful stale completions = %d, want 0", got)
			}
			if got, ok := pool.Get(current.ID); !ok || got != current {
				t.Fatalf("Get() = (%p, %v), want (%p, true)", got, ok, current)
			}
			if !current.healthy.Load() {
				t.Fatal("concurrent stale completions changed current health")
			}
			assertGenerationCacheUnchanged(t, pool, cached)
		})
	}
}

func warmGenerationCache(t *testing.T, pool *ConnectionPool, expected *ClientConn) *[]*ClientConn {
	t.Helper()
	selected, err := pool.Select()
	if err != nil && expected.healthy.Load() {
		t.Fatalf("Select() while warming cache error = %v", err)
	}
	if expected.healthy.Load() && selected != expected {
		t.Fatalf("Select() while warming cache = %p, want %p", selected, expected)
	}
	cached := pool.cachedClients.Load()
	if cached == nil {
		t.Fatal("Select() did not warm the cache")
	}
	if len(*cached) != 1 || (*cached)[0] != expected {
		t.Fatalf("cached clients = %v, want only %p", *cached, expected)
	}
	return cached
}

func assertGenerationCacheUnchanged(t *testing.T, pool *ConnectionPool, expected *[]*ClientConn) {
	t.Helper()
	if got := pool.cachedClients.Load(); got != expected {
		t.Fatalf("cachedClients pointer changed from %p to %p", expected, got)
	}
}

func candidateLastSeen(candidate *ClientConn) time.Time {
	if candidate == nil {
		return time.Time{}
	}
	return candidate.LastSeen
}
