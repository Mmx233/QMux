package stek

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
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

func TestRotateManagerKeyLimits(t *testing.T) {
	for _, test := range []struct {
		oldKeyLimit uint8
		wantTotal   int
	}{
		{oldKeyLimit: 0, wantTotal: 1},
		{oldKeyLimit: 1, wantTotal: 2},
		{oldKeyLimit: 2, wantTotal: 3},
		{oldKeyLimit: 6, wantTotal: 7},
		{oldKeyLimit: 7, wantTotal: 8},
	} {
		t.Run(fmt.Sprintf("old_keys_%d", test.oldKeyLimit), func(t *testing.T) {
			manager := newTestRotateManager(t, time.Hour, test.oldKeyLimit)
			for range 8 {
				if err := manager.rotate(); err != nil {
					t.Fatalf("rotate: %v", err)
				}
			}
			if got := len(*manager.Keys.Load()); got != test.wantTotal {
				t.Fatalf("total keys = %d, want %d", got, test.wantTotal)
			}
		})
	}
}

func TestRotateManagerSevenOldKeyBoundary(t *testing.T) {
	manager := newTestRotateManager(t, time.Hour, 7)
	initial := (*manager.Keys.Load())[0]
	for range 7 {
		if err := manager.rotate(); err != nil {
			t.Fatalf("rotate: %v", err)
		}
	}
	keys := *manager.Keys.Load()
	if len(keys) != 8 || keys[7] != initial {
		t.Fatalf("keys after seven rotations = %d, initial key not retained at boundary", len(keys))
	}
	if err := manager.rotate(); err != nil {
		t.Fatalf("eighth rotate: %v", err)
	}
	keys = *manager.Keys.Load()
	if len(keys) != 8 || slices.Contains(keys, initial) {
		t.Fatal("initial key remained after the eighth rotation")
	}
}

func TestRotateManagerLogsLimitAndActualKeys(t *testing.T) {
	previous := log.Logger
	var output bytes.Buffer
	log.Logger = zerolog.New(&output)
	t.Cleanup(func() { log.Logger = previous })

	manager := newTestRotateManager(t, time.Hour, 7)
	if err := manager.rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	manager.Start(context.Background())
	manager.Stop()

	want := map[string]map[string]int{
		"initialized session ticket encryption keys": {"initial_keys": 1, "old_key_limit": 7, "max_total_keys": 8},
		"rotated session ticket encryption keys":     {"old_keys": 1, "total_keys": 2, "old_key_limit": 7},
		"starting session ticket key rotation":       {"old_key_limit": 7, "max_total_keys": 8},
	}
	for line := range strings.SplitSeq(strings.TrimSpace(output.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode log event: %v", err)
		}
		if _, ok := event["overlap"]; ok {
			t.Fatalf("ambiguous overlap field in event: %v", event)
		}
		if _, ok := event["key_overlap"]; ok {
			t.Fatalf("ambiguous key_overlap field in event: %v", event)
		}
		fields, ok := want[event["message"].(string)]
		if !ok {
			continue
		}
		for field, value := range fields {
			if got := int(event[field].(float64)); got != value {
				t.Fatalf("%s %s = %d, want %d", event["message"], field, got, value)
			}
		}
		delete(want, event["message"].(string))
	}
	if len(want) != 0 {
		t.Fatalf("missing log events: %v", want)
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
