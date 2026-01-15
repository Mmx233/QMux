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

	// Verify initial keys were generated
	keys := manager.Keys.Load()
	if keys == nil {
		t.Fatal("Expected non-nil keys")
	}
	if len(*keys) != 3 {
		t.Errorf("Expected 3 initial keys, got %d", len(*keys))
	}

	// Verify each key is 32 bytes
	for i, key := range *keys {
		if len(key) != 32 {
			t.Errorf("Key %d has wrong length: expected 32, got %d", i, len(key))
		}
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
		{"zero overlap", time.Hour, 0, true},
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
	manager, err := NewRotateManager(100*time.Millisecond, 3)
	if err != nil {
		t.Fatalf("NewRotateManager failed: %v", err)
	}

	// Get initial keys
	initialKeys := manager.Keys.Load()
	firstKey := (*initialKeys)[0]

	// Manually trigger rotation
	err = manager.rotate()
	if err != nil {
		t.Fatalf("rotate() failed: %v", err)
	}

	// Verify keys changed
	newKeys := manager.Keys.Load()
	if len(*newKeys) != 3 {
		t.Errorf("Expected 3 keys after rotation, got %d", len(*newKeys))
	}

	// First key should be different (new key)
	if (*newKeys)[0] == firstKey {
		t.Error("Expected first key to change after rotation")
	}

	// Second key should be the old first key
	if (*newKeys)[1] != firstKey {
		t.Error("Expected second key to be the old first key")
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
	manager, err := NewRotateManager(time.Hour, 2)
	if err != nil {
		t.Fatalf("NewRotateManager failed: %v", err)
	}

	// Rotate multiple times
	for i := 0; i < 5; i++ {
		err = manager.rotate()
		if err != nil {
			t.Fatalf("rotate() failed on iteration %d: %v", i, err)
		}

		keys := manager.Keys.Load()
		if len(*keys) > 2 {
			t.Errorf("Expected max 2 keys, got %d after %d rotations", len(*keys), i+1)
		}
	}

	// Should have exactly 2 keys (overlap limit)
	finalKeys := manager.Keys.Load()
	if len(*finalKeys) != 2 {
		t.Errorf("Expected exactly 2 keys after multiple rotations, got %d", len(*finalKeys))
	}
}
