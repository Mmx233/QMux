package protocol

import (
	"bytes"
	"errors"
	"testing"
)

type fragmentAssemblerHarness struct {
	add        func(sessionID uint32, fragID uint16, index, total uint8, payload []byte) ([]byte, error)
	group      func(fragID uint16) *fragmentGroup
	groupCount func() int
}

func fragmentAssemblerHarnesses() map[string]func() fragmentAssemblerHarness {
	return map[string]func() fragmentAssemblerHarness{
		"regular": func() fragmentAssemblerHarness {
			assembler := &FragmentAssembler{fragments: make(map[uint16]*fragmentGroup)}
			return fragmentAssemblerHarness{
				add: assembler.AddFragment,
				group: func(fragID uint16) *fragmentGroup {
					return assembler.fragments[fragID]
				},
				groupCount: func() int {
					return len(assembler.fragments)
				},
			}
		},
		"sharded": func() fragmentAssemblerHarness {
			assembler := &ShardedFragmentAssembler{
				shards:     make([]fragmentShard, 4),
				shardCount: 4,
			}
			for i := range assembler.shards {
				assembler.shards[i].fragments = make(map[uint16]*fragmentGroup)
			}
			return fragmentAssemblerHarness{
				add: assembler.AddFragment,
				group: func(fragID uint16) *fragmentGroup {
					return assembler.getShard(fragID).fragments[fragID]
				},
				groupCount: func() int {
					count := 0
					for i := range assembler.shards {
						count += len(assembler.shards[i].fragments)
					}
					return count
				},
			}
		},
	}
}

func TestFragmentAssemblersRejectInvalidInputBeforeStateChange(t *testing.T) {
	tests := []struct {
		name  string
		index uint8
		total uint8
		want  error
	}{
		{name: "zero total", index: 0, total: 0, want: ErrInvalidFragTotal},
		{name: "single fragment", index: 0, total: 1, want: ErrInvalidFragTotal},
		{name: "single fragment with invalid index", index: 1, total: 1, want: ErrInvalidFragTotal},
		{name: "index equal to total", index: 2, total: 2, want: ErrInvalidFragIndex},
		{name: "index greater than total", index: 3, total: 2, want: ErrInvalidFragIndex},
	}

	for assemblerName, newHarness := range fragmentAssemblerHarnesses() {
		t.Run(assemblerName, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					harness := newHarness()
					result, err := harness.add(1, 7, tt.index, tt.total, []byte("payload"))
					if !errors.Is(err, tt.want) {
						t.Fatalf("expected %v, got %v", tt.want, err)
					}
					if result != nil {
						t.Fatalf("expected nil result, got %q", result)
					}
					if got := harness.groupCount(); got != 0 {
						t.Fatalf("invalid input created %d fragment groups", got)
					}
				})
			}
		})
	}
}

func TestFragmentAssemblersDropGroupOnTotalMismatch(t *testing.T) {
	tests := []struct {
		name          string
		initialTotal  uint8
		mismatchIndex uint8
		mismatchTotal uint8
	}{
		{name: "total increases", initialTotal: 2, mismatchIndex: 3, mismatchTotal: 4},
		{name: "total decreases", initialTotal: 4, mismatchIndex: 1, mismatchTotal: 2},
	}

	for assemblerName, newHarness := range fragmentAssemblerHarnesses() {
		t.Run(assemblerName, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					harness := newHarness()
					const fragID = 19

					if result, err := harness.add(123, fragID, 0, tt.initialTotal, []byte("old")); err != nil || result != nil {
						t.Fatalf("create group: result=%q, err=%v", result, err)
					}
					oldGroup := harness.group(fragID)
					if oldGroup == nil {
						t.Fatal("expected initial fragment group")
					}
					if assemblerName == "sharded" && len(oldGroup.buffers) != 1 {
						t.Fatalf("expected one pooled buffer before mismatch, got %d", len(oldGroup.buffers))
					}

					result, err := harness.add(123, fragID, tt.mismatchIndex, tt.mismatchTotal, []byte("conflict"))
					if !errors.Is(err, ErrFragmentTotalMismatch) {
						t.Fatalf("expected ErrFragmentTotalMismatch, got %v", err)
					}
					if result != nil {
						t.Fatalf("expected conflicting fragment to be discarded, got %q", result)
					}
					if got := harness.groupCount(); got != 0 {
						t.Fatalf("expected old group to be deleted, got %d groups", got)
					}
					assertFragmentGroupReleased(t, oldGroup)

					if result, err = harness.add(123, fragID, 1, 2, []byte("B")); err != nil || result != nil {
						t.Fatalf("rebuild out of order: result=%q, err=%v", result, err)
					}
					newGroup := harness.group(fragID)
					if newGroup == nil || newGroup == oldGroup {
						t.Fatal("expected a new fragment group after mismatch")
					}
					if result, err = harness.add(123, fragID, 1, 2, []byte("ignored duplicate")); err != nil || result != nil {
						t.Fatalf("duplicate fragment: result=%q, err=%v", result, err)
					}
					result, err = harness.add(123, fragID, 0, 2, []byte("A"))
					if err != nil {
						t.Fatalf("complete rebuilt group: %v", err)
					}
					if !bytes.Equal(result, []byte("AB")) {
						t.Fatalf("unexpected rebuilt payload %q", result)
					}
					if got := harness.groupCount(); got != 0 {
						t.Fatalf("completed group was not deleted: %d groups", got)
					}
					assertFragmentGroupReleased(t, newGroup)
				})
			}
		})
	}
}

func TestFragmentAssemblersPreserveGroupOnSessionMismatch(t *testing.T) {
	for assemblerName, newHarness := range fragmentAssemblerHarnesses() {
		t.Run(assemblerName, func(t *testing.T) {
			harness := newHarness()
			const fragID = 23

			if _, err := harness.add(100, fragID, 0, 2, []byte("first")); err != nil {
				t.Fatalf("create group: %v", err)
			}
			group := harness.group(fragID)
			result, err := harness.add(200, fragID, 1, 3, []byte("wrong session and total"))
			if !errors.Is(err, ErrSessionIDMismatch) {
				t.Fatalf("expected ErrSessionIDMismatch, got %v", err)
			}
			if result != nil {
				t.Fatalf("expected nil result, got %q", result)
			}
			if harness.group(fragID) != group || group.received != 1 {
				t.Fatal("session mismatch modified or deleted the existing group")
			}

			result, err = harness.add(100, fragID, 1, 2, []byte("second"))
			if err != nil {
				t.Fatalf("complete original group: %v", err)
			}
			if !bytes.Equal(result, []byte("firstsecond")) {
				t.Fatalf("unexpected payload %q", result)
			}
		})
	}
}

func TestFragmentAssemblersDefendAgainstInvalidStoredDataLength(t *testing.T) {
	for assemblerName, newHarness := range fragmentAssemblerHarnesses() {
		t.Run(assemblerName, func(t *testing.T) {
			harness := newHarness()
			if _, err := harness.add(1, 29, 0, 2, []byte("first")); err != nil {
				t.Fatalf("create group: %v", err)
			}
			group := harness.group(29)
			group.data = group.data[:1]

			result, err := harness.add(1, 29, 1, 2, []byte("second"))
			if !errors.Is(err, ErrInvalidFragIndex) {
				t.Fatalf("expected ErrInvalidFragIndex, got %v", err)
			}
			if result != nil {
				t.Fatalf("expected nil result, got %q", result)
			}
			if harness.group(29) != group {
				t.Fatal("defensive bounds check unexpectedly deleted the group")
			}
			releaseFragmentGroup(group)
		})
	}
}

func TestReleaseFragmentGroupClearsReferencesAndIsIdempotent(t *testing.T) {
	bufPtr := GetFragmentBuffer()
	group := &fragmentGroup{
		received: 1,
		data:     [][]byte{(*bufPtr)[:4]},
		buffers:  []*[]byte{bufPtr},
	}

	releaseFragmentGroup(group)
	assertFragmentGroupReleased(t, group)

	// Clearing the tracked pointers makes a second release a no-op instead of
	// returning the same pooled buffer twice.
	releaseFragmentGroup(group)
	assertFragmentGroupReleased(t, group)
}

func assertFragmentGroupReleased(t *testing.T, group *fragmentGroup) {
	t.Helper()
	if group.received != 0 {
		t.Fatalf("expected received count to be cleared, got %d", group.received)
	}
	if group.data != nil {
		t.Fatalf("expected fragment data references to be cleared, got %d", len(group.data))
	}
	if group.buffers != nil {
		t.Fatalf("expected pooled buffer references to be cleared, got %d", len(group.buffers))
	}
}
