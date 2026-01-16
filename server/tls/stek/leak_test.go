package stek

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain ensures no goroutine leaks across all tests in this package
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRotateManager_Stop_NoGoroutineLeak verifies that stopping a RotateManager
// properly terminates the background rotation goroutine.
func TestRotateManager_Stop_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	manager, err := NewRotateManager(100*time.Millisecond, 3)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	ctx := context.Background()
	manager.Start(ctx)

	// Let rotation run a few times
	time.Sleep(250 * time.Millisecond)

	manager.Stop()

	// Allow goroutine to terminate
	time.Sleep(50 * time.Millisecond)
}

// TestRotateManager_ContextCancellation_NoLeak verifies that context cancellation
// properly stops the rotation goroutine.
func TestRotateManager_ContextCancellation_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	manager, err := NewRotateManager(100*time.Millisecond, 3)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)

	// Let rotation run
	time.Sleep(150 * time.Millisecond)

	// Cancel context
	cancel()

	// Allow goroutine to terminate
	time.Sleep(50 * time.Millisecond)
}

// TestRotateManager_RapidStartStop_NoLeak tests rapid start/stop cycles
// to ensure no goroutine accumulation.
func TestRotateManager_RapidStartStop_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < 10; i++ {
		manager, err := NewRotateManager(50*time.Millisecond, 2)
		if err != nil {
			t.Fatalf("failed to create manager: %v", err)
		}

		ctx := context.Background()
		manager.Start(ctx)
		time.Sleep(10 * time.Millisecond)
		manager.Stop()
	}

	// Allow all goroutines to terminate
	time.Sleep(100 * time.Millisecond)
}

// TestRotateManager_MultipleStops_NoLeak verifies that calling Stop multiple times
// is safe and doesn't cause issues.
func TestRotateManager_MultipleStops_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	manager, err := NewRotateManager(100*time.Millisecond, 3)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	ctx := context.Background()
	manager.Start(ctx)

	// Multiple stops should be safe
	manager.Stop()
	manager.Stop()
	manager.Stop()

	// Allow goroutine to terminate
	time.Sleep(50 * time.Millisecond)
}

// TestRotateManager_ImmediateStop_NoLeak verifies stopping immediately after start
// doesn't leak goroutines.
func TestRotateManager_ImmediateStop_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	manager, err := NewRotateManager(time.Hour, 3)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	ctx := context.Background()
	manager.Start(ctx)
	manager.Stop()

	// Allow goroutine to terminate
	time.Sleep(50 * time.Millisecond)
}

// TestRotateManager_NoStart_NoLeak verifies that a manager that was never started
// doesn't have any goroutines.
func TestRotateManager_NoStart_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	_, err := NewRotateManager(time.Hour, 3)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	// Manager created but never started - should have no goroutines
}

// TestRotateManager_StopBeforeStart_NoLeak verifies that stopping before starting
// is safe.
func TestRotateManager_StopBeforeStart_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	manager, err := NewRotateManager(time.Hour, 3)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	// Stop before start - should be safe (stopCh is nil)
	manager.Stop()
}
