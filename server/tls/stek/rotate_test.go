package stek

import (
	"context"
	"testing"
	"time"
)

func TestNewRotateManager(t *testing.T) {
	// Test valid parameters
	manager, err := NewRotateManager(time.Hour, 3)
	if err != nil {
		t.Fatalf("NewRotateManager failed: %v", err)
	}
	if manager == nil {
		t.Fatal("Expected non-nil manager")
	}

	// Verify only 1 initial key was generated
	keys := manager.Keys.Load()
	if keys == nil {
		t.Fatal("Expected non-nil keys")
	}
	if len(*keys) != 1 {
		t.Errorf("Expected 1 initial key, got %d", len(*keys))
	}

	// Verify key is 32 bytes
	if len((*keys)[0]) != 32 {
		t.Errorf("Key has wrong length: expected 32, got %d", len((*keys)[0]))
	}
}

func TestNewRotateManager_InvalidParameters(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		overlap  uint8
		wantErr  bool
	}{
		{"zero interval", 0, 2, true},
		{"negative interval", -time.Hour, 2, true},
		{"zero overlap", time.Hour, 0, false}, // overlap=0 is valid (no old keys retained)
		{"valid parameters", time.Hour, 2, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRotateManager(tt.interval, tt.overlap)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewRotateManager() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRotateManager_Rotation(t *testing.T) {
	// overlap=2 means: 1 current + up to 2 old keys = max 3 keys
	manager, err := NewRotateManager(100*time.Millisecond, 2)
	if err != nil {
		t.Fatalf("NewRotateManager failed: %v", err)
	}

	// Get initial key (should be only 1)
	initialKeys := manager.Keys.Load()
	if len(*initialKeys) != 1 {
		t.Fatalf("Expected 1 initial key, got %d", len(*initialKeys))
	}
	key0 := (*initialKeys)[0]

	// First rotation: [key1, key0]
	err = manager.rotate()
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
	manager, err := NewRotateManager(50*time.Millisecond, 2)
	if err != nil {
		t.Fatalf("NewRotateManager failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Start rotation
	manager.Start(ctx)

	// Wait for at least one rotation
	time.Sleep(150 * time.Millisecond)

	// Stop should be idempotent
	manager.Stop()
	manager.Stop()

	// Wait for context to expire
	<-ctx.Done()
}

func TestRotateManager_OverlapLimit(t *testing.T) {
	// overlap=2 means max 3 keys (1 current + 2 old)
	manager, err := NewRotateManager(time.Hour, 2)
	if err != nil {
		t.Fatalf("NewRotateManager failed: %v", err)
	}

	// Initial: 1 key
	keys := manager.Keys.Load()
	if len(*keys) != 1 {
		t.Errorf("Expected 1 initial key, got %d", len(*keys))
	}

	// Rotate multiple times and verify max keys = 1 + overlap
	expectedCounts := []int{2, 3, 3, 3, 3} // after each rotation
	for i := 0; i < 5; i++ {
		err = manager.rotate()
		if err != nil {
			t.Fatalf("rotate() failed on iteration %d: %v", i, err)
		}

		keys := manager.Keys.Load()
		if len(*keys) != expectedCounts[i] {
			t.Errorf("After rotation %d: expected %d keys, got %d", i+1, expectedCounts[i], len(*keys))
		}
	}

	// Final check: should have exactly 3 keys (1 current + 2 old)
	finalKeys := manager.Keys.Load()
	if len(*finalKeys) != 3 {
		t.Errorf("Expected exactly 3 keys after multiple rotations, got %d", len(*finalKeys))
	}
}

func TestRotateManager_ZeroOverlap(t *testing.T) {
	// overlap=0 means only keep current key, no old keys
	manager, err := NewRotateManager(time.Hour, 0)
	if err != nil {
		t.Fatalf("NewRotateManager failed: %v", err)
	}

	// Initial: 1 key
	keys := manager.Keys.Load()
	if len(*keys) != 1 {
		t.Errorf("Expected 1 initial key, got %d", len(*keys))
	}
	key0 := (*keys)[0]

	// After rotation: still only 1 key (new one, old one dropped)
	err = manager.rotate()
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

	// Multiple rotations should always result in 1 key
	for i := 0; i < 5; i++ {
		err = manager.rotate()
		if err != nil {
			t.Fatalf("rotate() failed on iteration %d: %v", i, err)
		}
		keys = manager.Keys.Load()
		if len(*keys) != 1 {
			t.Errorf("Expected 1 key after rotation %d, got %d", i+1, len(*keys))
		}
	}
}
