package pool

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestReservationIsNotPublishedUntilCommit(t *testing.T) {
	pool := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()
	client := &ClientConn{ID: "client"}

	reservation, err := pool.Reserve(client)
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if got := pool.Count(); got != 0 {
		t.Fatalf("Count() while pending = %d, want 0", got)
	}
	if _, ok := pool.Get(client.ID); ok {
		t.Fatal("Get() published a pending reservation")
	}
	if _, err := pool.Select(); !errors.Is(err, ErrNoClientsAvailable) {
		t.Fatalf("Select() while pending error = %v, want %v", err, ErrNoClientsAvailable)
	}
	if client.added.Load() {
		t.Fatal("Reserve() consumed the generation token")
	}

	if err := pool.Commit(reservation); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if got, ok := pool.Get(client.ID); !ok || got != client {
		t.Fatalf("Get() after Commit = (%p, %v), want (%p, true)", got, ok, client)
	}
	if !client.added.Load() || !client.healthy.Load() {
		t.Fatal("Commit() did not consume and publish a healthy generation")
	}
}

func TestReservationAbortIsExactIdempotentAndReusable(t *testing.T) {
	pool := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()
	client := &ClientConn{ID: "client"}

	first, err := pool.Reserve(client)
	if err != nil {
		t.Fatalf("Reserve(first) error = %v", err)
	}
	if !pool.Abort(first) {
		t.Fatal("Abort(first) = false")
	}
	if pool.Abort(first) {
		t.Fatal("second Abort(first) = true")
	}
	if client.added.Load() {
		t.Fatal("Abort() consumed the generation token")
	}

	second, err := pool.Reserve(client)
	if err != nil {
		t.Fatalf("Reserve(second) error = %v", err)
	}
	if pool.Abort(first) {
		t.Fatal("stale Abort(first) removed the replacement")
	}
	if err := pool.Commit(second); err != nil {
		t.Fatalf("Commit(second) error = %v", err)
	}
	if got, ok := pool.Get(client.ID); !ok || got != client {
		t.Fatalf("replacement reservation was lost: got (%p, %v)", got, ok)
	}
}

func TestReservationAndAddRejectSamePendingID(t *testing.T) {
	pool := New("test", NewRoundRobinBalancer(), newTestLogger())
	defer pool.Stop()
	pending := &ClientConn{ID: "client"}
	reservation, err := pool.Reserve(pending)
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}

	candidate := &ClientConn{ID: pending.ID}
	if err := pool.Add(candidate); err == nil {
		t.Fatal("Add() bypassed a pending reservation")
	}
	if candidate.added.Load() {
		t.Fatal("failed Add() consumed candidate")
	}
	if !pool.Abort(reservation) {
		t.Fatal("Abort() = false")
	}
	if err := pool.Add(candidate); err != nil {
		t.Fatalf("Add() after Abort error = %v", err)
	}
}

func TestReservationDuplicateRaceHasSingleWinner(t *testing.T) {
	pool := New("test", NewLeastConnectionsBalancer(), newTestLogger())
	defer pool.Stop()
	const workers = 64
	start := make(chan struct{})
	reservations := make(chan *Reservation, workers)
	var successes atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			candidate := &ClientConn{ID: "client"}
			<-start
			reservation, err := pool.Reserve(candidate)
			if err == nil {
				successes.Add(1)
				reservations <- reservation
			}
		})
	}
	close(start)
	wg.Wait()
	close(reservations)
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful reservations = %d, want 1", got)
	}
	reservation := <-reservations
	if err := pool.Commit(reservation); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if got := pool.Count(); got != 1 {
		t.Fatalf("Count() = %d, want 1", got)
	}
}

func TestReservationConcurrentCommitAndAbortIsBounded(t *testing.T) {
	for range 100 {
		pool := New("test", NewRoundRobinBalancer(), newTestLogger())
		client := &ClientConn{ID: "client"}
		reservation, err := pool.Reserve(client)
		if err != nil {
			t.Fatalf("Reserve() error = %v", err)
		}
		start := make(chan struct{})
		var committed atomic.Bool
		var aborted atomic.Bool
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			committed.Store(pool.Commit(reservation) == nil)
		})
		wg.Go(func() {
			<-start
			aborted.Store(pool.Abort(reservation))
		})
		close(start)
		wg.Wait()
		if committed.Load() == aborted.Load() {
			t.Fatalf("Commit success = %v, Abort success = %v; want exactly one", committed.Load(), aborted.Load())
		}
		if got := pool.Count(); got != boolInt(committed.Load()) {
			t.Fatalf("Count() = %d, committed = %v", got, committed.Load())
		}
		pool.Stop()
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
