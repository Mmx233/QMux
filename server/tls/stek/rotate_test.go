package stek

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNewRotateManager(t *testing.T) {
	manager, err := NewRotateManager(time.Hour, 3)
	if err != nil {
		t.Fatalf("NewRotateManager failed: %v", err)
	}
	if manager == nil {
		t.Fatal("Expected non-nil manager")
	}

	keys := manager.Keys.Load()
	if keys == nil {
		t.Fatal("Expected non-nil keys")
	}
	if len(*keys) != 1 {
		t.Errorf("Expected 1 initial key, got %d", len(*keys))
	}
	if _, err := NewRotateManager(0, 2); err == nil {
		t.Fatal("zero rotation interval was accepted")
	}
}

func newTestRotateManager(t *testing.T, interval time.Duration, overlap uint8) *RotateManager {
	t.Helper()
	manager, err := NewRotateManager(interval, overlap)
	if err != nil {
		t.Fatalf("NewRotateManager failed: %v", err)
	}
	return manager
}

func TestRotateManager_Rotation(t *testing.T) {
	// overlap=2 means: 1 current + up to 2 old keys = max 3 keys
	manager := newTestRotateManager(t, 100*time.Millisecond, 2)

	// Get initial key (should be only 1)
	initialKeys := manager.Keys.Load()
	if len(*initialKeys) != 1 {
		t.Fatalf("Expected 1 initial key, got %d", len(*initialKeys))
	}
	key0 := (*initialKeys)[0]

	// First rotation: [key1, key0]
	err := manager.rotate()
	if err != nil {
		t.Fatalf("rotate() failed: %v", err)
	}
	keys := manager.Keys.Load()
	if len(*keys) != 2 {
		t.Errorf("Expected 2 keys after 1st rotation, got %d", len(*keys))
	}
	key1 := (*keys)[0]
	if key1 == key0 {
		t.Error("Expected first key to change after rotation")
	}
	if (*keys)[1] != key0 {
		t.Error("Expected second key to be the old first key")
	}

	// Second rotation: [key2, key1, key0]
	err = manager.rotate()
	if err != nil {
		t.Fatalf("rotate() failed: %v", err)
	}
	keys = manager.Keys.Load()
	if len(*keys) != 3 {
		t.Errorf("Expected 3 keys after 2nd rotation (1 current + 2 old), got %d", len(*keys))
	}
	key2 := (*keys)[0]
	if (*keys)[1] != key1 {
		t.Error("Expected second key to be key1")
	}
	if (*keys)[2] != key0 {
		t.Error("Expected third key to be key0")
	}

	// Third rotation: [key3, key2, key1] - key0 should be dropped
	err = manager.rotate()
	if err != nil {
		t.Fatalf("rotate() failed: %v", err)
	}
	keys = manager.Keys.Load()
	if len(*keys) != 3 {
		t.Errorf("Expected 3 keys after 3rd rotation, got %d", len(*keys))
	}
	if (*keys)[1] != key2 {
		t.Error("Expected second key to be key2")
	}
	if (*keys)[2] != key1 {
		t.Error("Expected third key to be key1")
	}
	// key0 should no longer be present
	for i, k := range *keys {
		if k == key0 {
			t.Errorf("key0 should have been dropped, but found at index %d", i)
		}
	}
}

func TestRotateManager_StartStop(t *testing.T) {
	manager := newTestRotateManager(t, time.Millisecond, 2)
	initial := (*manager.Keys.Load())[0]
	manager.Start(context.Background())
	deadline := time.After(time.Second)
	for (*manager.Keys.Load())[0] == initial {
		select {
		case <-deadline:
			t.Fatal("periodic rotation did not replace the initial key")
		case <-time.After(time.Millisecond):
		}
	}
	manager.Stop()
}

func TestRotateManager_StopJoinsAndIsIdempotent(t *testing.T) {
	manager := newTestRotateManager(t, time.Hour, 2)
	manager.Start(context.Background())

	var callers sync.WaitGroup
	callers.Add(8)
	for range 8 {
		go func() {
			defer callers.Done()
			manager.Stop()
		}()
	}
	callers.Wait()

	select {
	case <-manager.doneCh:
	default:
		t.Fatal("Stop returned before the background goroutine exited")
	}
}

func TestRotateManager_StopBeforeStart(t *testing.T) {
	manager := newTestRotateManager(t, time.Hour, 2)
	manager.Stop()
	manager.Start(context.Background())
	manager.Stop()

	manager.mu.Lock()
	started := manager.started
	manager.mu.Unlock()
	if started {
		t.Fatal("Start launched rotation after Stop")
	}
	select {
	case <-manager.doneCh:
	default:
		t.Fatal("Stop before Start did not complete the lifecycle")
	}
}

func TestRotateManager_ContextCancellationIsJoinable(t *testing.T) {
	manager := newTestRotateManager(t, time.Hour, 2)
	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)
	cancel()

	select {
	case <-manager.doneCh:
	case <-time.After(time.Second):
		t.Fatal("rotation goroutine did not exit after context cancellation")
	}

	manager.Stop()
}

func TestRotateManager_ZeroOverlap(t *testing.T) {
	manager := newTestRotateManager(t, time.Hour, 0)
	keys := manager.Keys.Load()
	if len(*keys) != 1 {
		t.Errorf("Expected 1 initial key, got %d", len(*keys))
	}
	key0 := (*keys)[0]

	err := manager.rotate()
	if err != nil {
		t.Fatalf("rotate() failed: %v", err)
	}
	keys = manager.Keys.Load()
	if len(*keys) != 1 {
		t.Errorf("Expected 1 key after rotation with overlap=0, got %d", len(*keys))
	}
	if (*keys)[0] == key0 {
		t.Error("Expected key to change after rotation")
	}
}
