package protocol

import (
	"bytes"
	"errors"
	"hash/maphash"
	"sync"
	"testing"
	"time"
)

type fragmentAssemblerHarness struct {
	add        func(sessionID uint32, fragID uint16, index, total uint8, payload []byte) ([]byte, error)
	group      func(sessionID uint32, fragID uint16) *fragmentGroup
	groupCount func() int
	cleanup    func(time.Time)
	setLimits  func(int, int64)
	groups     func() int64
	bytes      func() int64
}

func fragmentAssemblerHarnesses() map[string]func() fragmentAssemblerHarness {
	return map[string]func() fragmentAssemblerHarness{
		"regular": func() fragmentAssemblerHarness {
			assembler := &FragmentAssembler{fragments: make(map[fragmentKey]*fragmentGroup)}
			return fragmentAssemblerHarness{
				add: assembler.AddFragment,
				group: func(sessionID uint32, fragID uint16) *fragmentGroup {
					return assembler.fragments[fragmentKey{sessionID: sessionID, fragID: fragID}]
				},
				groupCount: func() int {
					return len(assembler.fragments)
				},
				cleanup: func(now time.Time) {
					assembler.mu.Lock()
					_, releasedBytes := cleanupExpiredFragmentGroups(assembler.fragments, now)
					assembler.retainedBytes -= releasedBytes
					assembler.mu.Unlock()
				},
				setLimits: func(groups int, bytes int64) {
					assembler.maxGroups, assembler.maxBytes = groups, bytes
				},
				groups: func() int64 { return int64(len(assembler.fragments)) },
				bytes:  func() int64 { return assembler.retainedBytes },
			}
		},
		"sharded": func() fragmentAssemblerHarness {
			assembler := &ShardedFragmentAssembler{
				shards: make([]fragmentShard, 4),
				seed:   maphash.MakeSeed(),
			}
			for i := range assembler.shards {
				assembler.shards[i].fragments = make(map[fragmentKey]*fragmentGroup)
			}
			return fragmentAssemblerHarness{
				add: assembler.AddFragment,
				group: func(sessionID uint32, fragID uint16) *fragmentGroup {
					key := fragmentKey{sessionID: sessionID, fragID: fragID}
					return assembler.getShard(key).fragments[key]
				},
				groupCount: func() int {
					count := 0
					for i := range assembler.shards {
						count += len(assembler.shards[i].fragments)
					}
					return count
				},
				cleanup: func(now time.Time) {
					for i := range assembler.shards {
						shard := &assembler.shards[i]
						shard.mu.Lock()
						releasedGroups, releasedBytes := cleanupExpiredFragmentGroups(shard.fragments, now)
						assembler.retainedGroups.Add(-releasedGroups)
						assembler.retainedBytes.Add(-releasedBytes)
						shard.mu.Unlock()
					}
				},
				setLimits: func(groups int, bytes int64) {
					assembler.maxGroups, assembler.maxBytes = groups, bytes
				},
				groups: assembler.retainedGroups.Load,
				bytes:  assembler.retainedBytes.Load,
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
					oldGroup := harness.group(123, fragID)
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
					if groups, retainedBytes := harness.groups(), harness.bytes(); groups != 0 || retainedBytes != 0 {
						t.Fatalf("mismatch retained budget: groups=%d bytes=%d", groups, retainedBytes)
					}
					assertFragmentGroupReleased(t, oldGroup)

					if result, err = harness.add(123, fragID, 1, 2, []byte("B")); err != nil || result != nil {
						t.Fatalf("rebuild out of order: result=%q, err=%v", result, err)
					}
					newGroup := harness.group(123, fragID)
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
					if groups, retainedBytes := harness.groups(), harness.bytes(); groups != 0 || retainedBytes != 0 {
						t.Fatalf("completion retained budget: groups=%d bytes=%d", groups, retainedBytes)
					}
					assertFragmentGroupReleased(t, newGroup)
				})
			}
		})
	}
}

func addSameFragmentIDForTwoSessions(t *testing.T, harness fragmentAssemblerHarness, fragID uint16) (*fragmentGroup, *fragmentGroup) {
	t.Helper()
	if _, err := harness.add(1, fragID, 0, 2, []byte("A")); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.add(2, fragID, 0, 2, []byte("B")); err != nil {
		t.Fatal(err)
	}
	return harness.group(1, fragID), harness.group(2, fragID)
}

func TestFragmentAssemblersTotalMismatchDoesNotAffectOtherSession(t *testing.T) {
	for assemblerName, newHarness := range fragmentAssemblerHarnesses() {
		t.Run(assemblerName, func(t *testing.T) {
			harness := newHarness()
			const fragID = 31

			firstGroup, secondGroup := addSameFragmentIDForTwoSessions(t, harness, fragID)

			if _, err := harness.add(1, fragID, 2, 3, []byte("conflict")); !errors.Is(err, ErrFragmentTotalMismatch) {
				t.Fatalf("expected ErrFragmentTotalMismatch, got %v", err)
			}
			if harness.group(1, fragID) != nil {
				t.Fatal("mismatched session group was not deleted")
			}
			assertFragmentGroupReleased(t, firstGroup)
			if harness.group(2, fragID) != secondGroup || secondGroup.received != 1 {
				t.Fatal("total mismatch changed the other session group")
			}

			result, err := harness.add(2, fragID, 1, 2, []byte("b"))
			if err != nil || !bytes.Equal(result, []byte("Bb")) {
				t.Fatalf("other session completion = (%q, %v)", result, err)
			}
			if groups, retainedBytes := harness.groups(), harness.bytes(); groups != 0 || retainedBytes != 0 {
				t.Fatalf("retained budget after completion: groups=%d bytes=%d", groups, retainedBytes)
			}
		})
	}
}

func TestFragmentAssemblersCleanupDoesNotAffectOtherSession(t *testing.T) {
	now := time.Now()
	for assemblerName, newHarness := range fragmentAssemblerHarnesses() {
		t.Run(assemblerName, func(t *testing.T) {
			harness := newHarness()
			const fragID = 37

			expired, recent := addSameFragmentIDForTwoSessions(t, harness, fragID)
			expired.createdAt = now.Add(-FragmentTimeout - time.Second)
			recent.createdAt = now

			harness.cleanup(now)
			if harness.group(1, fragID) != nil || harness.group(2, fragID) != recent {
				t.Fatal("cleanup did not isolate sessions sharing a fragment ID")
			}
			assertFragmentGroupReleased(t, expired)
			result, err := harness.add(2, fragID, 1, 2, []byte("b"))
			if err != nil || !bytes.Equal(result, []byte("Bb")) {
				t.Fatalf("recent session completion = (%q, %v)", result, err)
			}
			if groups, retainedBytes := harness.groups(), harness.bytes(); groups != 0 || retainedBytes != 0 {
				t.Fatalf("retained budget after cleanup/completion: groups=%d bytes=%d", groups, retainedBytes)
			}
		})
	}
}

func TestFragmentAssemblersConcurrentSessionsWithFixedFragmentID(t *testing.T) {
	for assemblerName, newHarness := range fragmentAssemblerHarnesses() {
		t.Run(assemblerName, func(t *testing.T) {
			harness := newHarness()
			const (
				fragID       = 41
				sessionCount = 64
			)
			results := make([][]byte, sessionCount)
			var resultsMu sync.Mutex
			start := make(chan struct{})
			var callers sync.WaitGroup

			for session := range sessionCount {
				for index := range 2 {
					callers.Go(func() {
						<-start
						result, err := harness.add(uint32(session), fragID, uint8(index), 2, []byte{byte(session), byte(index)})
						if err != nil {
							t.Errorf("session %d index %d: %v", session, index, err)
							return
						}
						if result != nil {
							resultsMu.Lock()
							results[session] = result
							resultsMu.Unlock()
						}
					})
				}
			}
			close(start)
			callers.Wait()

			for session, result := range results {
				want := []byte{byte(session), 0, byte(session), 1}
				if !bytes.Equal(result, want) {
					t.Fatalf("session %d result=%v, want %v", session, result, want)
				}
			}
			if groups, retainedBytes := harness.groups(), harness.bytes(); groups != 0 || retainedBytes != 0 {
				t.Fatalf("concurrent completion retained budget: groups=%d bytes=%d", groups, retainedBytes)
			}
		})
	}
}

func TestFragmentAssemblersGroupCapacity(t *testing.T) {
	for assemblerName, newHarness := range fragmentAssemblerHarnesses() {
		t.Run(assemblerName, func(t *testing.T) {
			harness := newHarness()
			harness.setLimits(2, 1<<20)

			for sessionID := uint32(1); sessionID <= 2; sessionID++ {
				if _, err := harness.add(sessionID, 1, 0, 2, []byte("pending")); err != nil {
					t.Fatalf("fill group %d: %v", sessionID, err)
				}
			}
			groups, retainedBytes := harness.groups(), harness.bytes()
			if result, err := harness.add(1, 1, 0, 2, []byte("duplicate")); err != nil || result != nil {
				t.Fatalf("duplicate at capacity = (%q, %v)", result, err)
			}
			if harness.groups() != groups || harness.bytes() != retainedBytes {
				t.Fatal("duplicate fragment consumed capacity")
			}
			if _, err := harness.add(3, 1, 0, 2, []byte("rejected")); !errors.Is(err, ErrFragmentAssemblerFull) {
				t.Fatalf("group capacity error = %v", err)
			}
			if harness.group(3, 1) != nil || harness.groups() != 2 {
				t.Fatal("rejected group changed assembler state")
			}

			if _, err := harness.add(1, 1, 1, 2, []byte("done")); err != nil {
				t.Fatalf("complete group: %v", err)
			}
			if _, err := harness.add(3, 1, 0, 2, []byte("accepted")); err != nil {
				t.Fatalf("capacity was not recovered: %v", err)
			}
			for sessionID := uint32(2); sessionID <= 3; sessionID++ {
				harness.group(sessionID, 1).createdAt = time.Time{}
			}
			harness.cleanup(time.Now())
			if groups, retainedBytes := harness.groups(), harness.bytes(); groups != 0 || retainedBytes != 0 {
				t.Fatalf("cleanup retained capacity: groups=%d bytes=%d", groups, retainedBytes)
			}
		})
	}
}

func TestFragmentAssemblersByteCapacity(t *testing.T) {
	for assemblerName, newHarness := range fragmentAssemblerHarnesses() {
		t.Run(assemblerName, func(t *testing.T) {
			harness := newHarness()
			payload := []byte("ab")
			byteLimit := int64(2 * len(payload))
			if assemblerName == "sharded" {
				payload = []byte("a")
				byteLimit = 2 * int64(FragmentBufferSize)
			}
			harness.setLimits(10, byteLimit)

			if _, err := harness.add(1, 1, 0, 2, payload); err != nil {
				t.Fatal(err)
			}
			group := harness.group(1, 1)
			wantCharge := int64(len(payload))
			if assemblerName == "sharded" {
				wantCharge = int64(cap(*group.buffers[0]))
			}
			if group.retainedBytes != wantCharge || harness.bytes() != wantCharge {
				t.Fatalf("retained bytes = group %d assembler %d, want %d", group.retainedBytes, harness.bytes(), wantCharge)
			}
			if result, err := harness.add(1, 1, 0, 2, []byte("duplicate")); err != nil || result != nil {
				t.Fatalf("duplicate at byte capacity = (%q, %v)", result, err)
			}
			if harness.bytes() != wantCharge {
				t.Fatal("duplicate fragment consumed byte capacity")
			}

			if _, err := harness.add(2, 1, 0, 2, payload); err != nil {
				t.Fatalf("exact byte boundary: %v", err)
			}
			if harness.bytes() != byteLimit {
				t.Fatalf("retained bytes at boundary = %d, want %d", harness.bytes(), byteLimit)
			}
			if _, err := harness.add(3, 1, 0, 2, []byte("x")); !errors.Is(err, ErrFragmentAssemblerFull) {
				t.Fatalf("byte capacity +1 error = %v", err)
			}
			if harness.group(3, 1) != nil || harness.groups() != 2 || harness.bytes() != byteLimit {
				t.Fatal("byte-cap rejection changed assembler state")
			}
			if _, err := harness.add(1, 1, 1, 2, payload); !errors.Is(err, ErrFragmentAssemblerFull) {
				t.Fatalf("existing group byte capacity error = %v", err)
			}
			if harness.group(1, 1) != group || group.received != 1 || harness.bytes() != byteLimit {
				t.Fatal("byte-cap rejection changed the existing group")
			}

			if _, err := harness.add(2, 1, 2, 3, []byte("mismatch")); !errors.Is(err, ErrFragmentTotalMismatch) {
				t.Fatalf("release at byte capacity: %v", err)
			}
			if harness.bytes() != wantCharge || harness.groups() != 1 {
				t.Fatalf("mismatch retained groups=%d bytes=%d, want 1/%d", harness.groups(), harness.bytes(), wantCharge)
			}
			if _, err := harness.add(1, 1, 1, 2, payload); err != nil {
				t.Fatalf("completion after capacity recovery: %v", err)
			}
			if groups, retainedBytes := harness.groups(), harness.bytes(); groups != 0 || retainedBytes != 0 {
				t.Fatalf("completion retained capacity: groups=%d bytes=%d", groups, retainedBytes)
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
			group := harness.group(1, 29)
			group.data = group.data[:1]

			result, err := harness.add(1, 29, 1, 2, []byte("second"))
			if !errors.Is(err, ErrInvalidFragIndex) {
				t.Fatalf("expected ErrInvalidFragIndex, got %v", err)
			}
			if result != nil {
				t.Fatalf("expected nil result, got %q", result)
			}
			if harness.group(1, 29) != group {
				t.Fatal("defensive bounds check unexpectedly deleted the group")
			}
			releaseFragmentGroup(group)
		})
	}
}

func TestReleaseFragmentGroupClearsReferencesAndIsIdempotent(t *testing.T) {
	bufPtr := GetFragmentBuffer()
	group := &fragmentGroup{
		received:      1,
		data:          [][]byte{(*bufPtr)[:4]},
		buffers:       []*[]byte{bufPtr},
		retainedBytes: int64(cap(*bufPtr)),
	}

	if released := releaseFragmentGroup(group); released != int64(cap(*bufPtr)) {
		t.Fatalf("released bytes = %d, want %d", released, cap(*bufPtr))
	}
	assertFragmentGroupReleased(t, group)

	// Clearing the tracked pointers makes a second release a no-op instead of
	// returning the same pooled buffer twice.
	if released := releaseFragmentGroup(group); released != 0 {
		t.Fatalf("second release returned %d bytes", released)
	}
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
	if group.retainedBytes != 0 {
		t.Fatalf("expected retained bytes to be cleared, got %d", group.retainedBytes)
	}
}
