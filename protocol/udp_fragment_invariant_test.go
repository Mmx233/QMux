package protocol

import (
	"bytes"
	"errors"
	"hash/maphash"
	"sync"
	"testing"
	"time"
)

func newShardedFragmentAssemblerFixture(t *testing.T) *ShardedFragmentAssembler {
	t.Helper()
	assembler := &ShardedFragmentAssembler{
		shards: make([]fragmentShard, 4),
		seed:   maphash.MakeSeed(),
	}
	for i := range assembler.shards {
		assembler.shards[i].fragments = make(map[fragmentKey]*fragmentGroup)
	}
	t.Cleanup(assembler.Close)
	return assembler
}

func shardedFragmentGroup(assembler *ShardedFragmentAssembler, sessionID uint32, fragID uint64) *fragmentGroup {
	key := fragmentKey{sessionID: sessionID, fragID: fragID}
	return assembler.getShard(key).fragments[key]
}

func shardedFragmentGroupCount(assembler *ShardedFragmentAssembler) int {
	count := 0
	for i := range assembler.shards {
		count += len(assembler.shards[i].fragments)
	}
	return count
}

func cleanupShardedFragmentAssembler(assembler *ShardedFragmentAssembler, now time.Time) {
	for i := range assembler.shards {
		shard := &assembler.shards[i]
		releasedGroups, releasedBytes := cleanupExpiredFragmentGroups(shard.fragments, now)
		assembler.expiredGroups.Add(uint64(releasedGroups))
		assembler.retainedGroups.Add(-releasedGroups)
		assembler.retainedBytes.Add(-releasedBytes)
	}
}

func TestShardedFragmentAssemblerRejectsInvalidInputBeforeStateChange(t *testing.T) {
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assembler := newShardedFragmentAssemblerFixture(t)
			result, err := assembler.AddFragment(1, 7, tt.index, tt.total, []byte("payload"))
			if !errors.Is(err, tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, err)
			}
			if result != nil {
				t.Fatalf("expected nil result, got %q", result)
			}
			if got := shardedFragmentGroupCount(assembler); got != 0 {
				t.Fatalf("invalid input created %d fragment groups", got)
			}
		})
	}
}

func TestShardedFragmentAssemblerDropsGroupOnTotalMismatch(t *testing.T) {
	tests := []struct {
		name          string
		initialTotal  uint8
		mismatchIndex uint8
		mismatchTotal uint8
	}{
		{name: "total increases", initialTotal: 2, mismatchIndex: 3, mismatchTotal: 4},
		{name: "total decreases", initialTotal: 4, mismatchIndex: 1, mismatchTotal: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assembler := newShardedFragmentAssemblerFixture(t)
			const fragID = 19

			if result, err := assembler.AddFragment(123, fragID, 0, tt.initialTotal, []byte("old")); err != nil || result != nil {
				t.Fatalf("create group: result=%q, err=%v", result, err)
			}
			oldGroup := shardedFragmentGroup(assembler, 123, fragID)
			if oldGroup == nil {
				t.Fatal("expected initial fragment group")
			}
			if len(oldGroup.buffers) != 1 {
				t.Fatalf("expected one pooled buffer before mismatch, got %d", len(oldGroup.buffers))
			}

			result, err := assembler.AddFragment(123, fragID, tt.mismatchIndex, tt.mismatchTotal, []byte("conflict"))
			if !errors.Is(err, ErrFragmentTotalMismatch) {
				t.Fatalf("expected ErrFragmentTotalMismatch, got %v", err)
			}
			if result != nil {
				t.Fatalf("expected conflicting fragment to be discarded, got %q", result)
			}
			if got := shardedFragmentGroupCount(assembler); got != 0 {
				t.Fatalf("expected old group to be deleted, got %d groups", got)
			}
			if snapshot := assembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
				t.Fatalf("mismatch retained budget: %+v", snapshot)
			}
			assertFragmentGroupReleased(t, oldGroup)

			if result, err = assembler.AddFragment(123, fragID, 1, 2, []byte("B")); err != nil || result != nil {
				t.Fatalf("rebuild out of order: result=%q, err=%v", result, err)
			}
			newGroup := shardedFragmentGroup(assembler, 123, fragID)
			if newGroup == nil || newGroup == oldGroup {
				t.Fatal("expected a new fragment group after mismatch")
			}
			if result, err = assembler.AddFragment(123, fragID, 1, 2, []byte("ignored duplicate")); err != nil || result != nil {
				t.Fatalf("duplicate fragment: result=%q, err=%v", result, err)
			}
			result, err = assembler.AddFragment(123, fragID, 0, 2, []byte("A"))
			if err != nil {
				t.Fatalf("complete rebuilt group: %v", err)
			}
			if !bytes.Equal(result, []byte("AB")) {
				t.Fatalf("unexpected rebuilt payload %q", result)
			}
			if got := shardedFragmentGroupCount(assembler); got != 0 {
				t.Fatalf("completed group was not deleted: %d groups", got)
			}
			if snapshot := assembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
				t.Fatalf("completion retained budget: %+v", snapshot)
			}
			assertFragmentGroupReleased(t, newGroup)
		})
	}
}

func addSameFragmentIDForTwoSessions(t *testing.T, assembler *ShardedFragmentAssembler, fragID uint64) (*fragmentGroup, *fragmentGroup) {
	t.Helper()
	if _, err := assembler.AddFragment(1, fragID, 0, 2, []byte("A")); err != nil {
		t.Fatal(err)
	}
	if _, err := assembler.AddFragment(2, fragID, 0, 2, []byte("B")); err != nil {
		t.Fatal(err)
	}
	return shardedFragmentGroup(assembler, 1, fragID), shardedFragmentGroup(assembler, 2, fragID)
}

func TestShardedFragmentAssemblerTotalMismatchDoesNotAffectOtherSession(t *testing.T) {
	assembler := newShardedFragmentAssemblerFixture(t)
	const fragID = 31

	firstGroup, secondGroup := addSameFragmentIDForTwoSessions(t, assembler, fragID)

	if _, err := assembler.AddFragment(1, fragID, 2, 3, []byte("conflict")); !errors.Is(err, ErrFragmentTotalMismatch) {
		t.Fatalf("expected ErrFragmentTotalMismatch, got %v", err)
	}
	if shardedFragmentGroup(assembler, 1, fragID) != nil {
		t.Fatal("mismatched session group was not deleted")
	}
	assertFragmentGroupReleased(t, firstGroup)
	if shardedFragmentGroup(assembler, 2, fragID) != secondGroup || secondGroup.received != 1 {
		t.Fatal("total mismatch changed the other session group")
	}

	result, err := assembler.AddFragment(2, fragID, 1, 2, []byte("b"))
	if err != nil || !bytes.Equal(result, []byte("Bb")) {
		t.Fatalf("other session completion = (%q, %v)", result, err)
	}
	if snapshot := assembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
		t.Fatalf("retained budget after completion: %+v", snapshot)
	}
}

func TestShardedFragmentAssemblerCleanupDoesNotAffectOtherSession(t *testing.T) {
	now := time.Now()
	assembler := newShardedFragmentAssemblerFixture(t)
	const fragID = 37

	expired, recent := addSameFragmentIDForTwoSessions(t, assembler, fragID)
	expired.createdAt = now.Add(-FragmentTimeout - time.Second)
	recent.createdAt = now

	cleanupShardedFragmentAssembler(assembler, now)
	if shardedFragmentGroup(assembler, 1, fragID) != nil || shardedFragmentGroup(assembler, 2, fragID) != recent {
		t.Fatal("cleanup did not isolate sessions sharing a fragment ID")
	}
	assertFragmentGroupReleased(t, expired)
	result, err := assembler.AddFragment(2, fragID, 1, 2, []byte("b"))
	if err != nil || !bytes.Equal(result, []byte("Bb")) {
		t.Fatalf("recent session completion = (%q, %v)", result, err)
	}
	if snapshot := assembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
		t.Fatalf("retained budget after cleanup/completion: %+v", snapshot)
	}
}

func TestShardedFragmentAssemblerConcurrentSessionsWithFixedFragmentID(t *testing.T) {
	assembler := newShardedFragmentAssemblerFixture(t)
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
				result, err := assembler.AddFragment(uint32(session), fragID, uint8(index), 2, []byte{byte(session), byte(index)})
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
	if snapshot := assembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
		t.Fatalf("concurrent completion retained budget: %+v", snapshot)
	}
}

func TestShardedFragmentAssemblerGroupCapacity(t *testing.T) {
	assembler := newShardedFragmentAssemblerFixture(t)
	assembler.maxGroups = 2
	assembler.maxBytes = 1 << 20

	for sessionID := uint32(1); sessionID <= 2; sessionID++ {
		if _, err := assembler.AddFragment(sessionID, 1, 0, 2, []byte("pending")); err != nil {
			t.Fatalf("fill group %d: %v", sessionID, err)
		}
	}
	snapshot := assembler.Snapshot()
	if result, err := assembler.AddFragment(1, 1, 0, 2, []byte("duplicate")); err != nil || result != nil {
		t.Fatalf("duplicate at capacity = (%q, %v)", result, err)
	}
	if assembler.Snapshot() != snapshot {
		t.Fatal("duplicate fragment consumed capacity")
	}
	if _, err := assembler.AddFragment(3, 1, 0, 2, []byte("rejected")); !errors.Is(err, ErrFragmentAssemblerFull) {
		t.Fatalf("group capacity error = %v", err)
	}
	if shardedFragmentGroup(assembler, 3, 1) != nil || assembler.Snapshot().RetainedGroups != 2 {
		t.Fatal("rejected group changed assembler state")
	}

	if _, err := assembler.AddFragment(1, 1, 1, 2, []byte("done")); err != nil {
		t.Fatalf("complete group: %v", err)
	}
	if _, err := assembler.AddFragment(3, 1, 0, 2, []byte("accepted")); err != nil {
		t.Fatalf("capacity was not recovered: %v", err)
	}
	for sessionID := uint32(2); sessionID <= 3; sessionID++ {
		shardedFragmentGroup(assembler, sessionID, 1).createdAt = time.Time{}
	}
	cleanupShardedFragmentAssembler(assembler, time.Now())
	if snapshot := assembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
		t.Fatalf("cleanup retained capacity: %+v", snapshot)
	}
}

func TestShardedFragmentAssemblerByteCapacity(t *testing.T) {
	payload := []byte("a")
	byteLimit := 2 * int64(FragmentBufferSize)
	assembler := newShardedFragmentAssemblerFixture(t)
	assembler.maxGroups = 10
	assembler.maxBytes = byteLimit

	if _, err := assembler.AddFragment(1, 1, 0, 2, payload); err != nil {
		t.Fatal(err)
	}
	group := shardedFragmentGroup(assembler, 1, 1)
	wantCharge := int64(cap(*group.buffers[0]))
	if snapshot := assembler.Snapshot(); group.retainedBytes != wantCharge || snapshot.RetainedBackingBytes != wantCharge {
		t.Fatalf("retained bytes = group %d assembler %d, want %d", group.retainedBytes, snapshot.RetainedBackingBytes, wantCharge)
	}
	if result, err := assembler.AddFragment(1, 1, 0, 2, []byte("duplicate")); err != nil || result != nil {
		t.Fatalf("duplicate at byte capacity = (%q, %v)", result, err)
	}
	if assembler.Snapshot().RetainedBackingBytes != wantCharge {
		t.Fatal("duplicate fragment consumed byte capacity")
	}

	if _, err := assembler.AddFragment(2, 1, 0, 2, payload); err != nil {
		t.Fatalf("exact byte boundary: %v", err)
	}
	if got := assembler.Snapshot().RetainedBackingBytes; got != byteLimit {
		t.Fatalf("retained bytes at boundary = %d, want %d", got, byteLimit)
	}
	if _, err := assembler.AddFragment(3, 1, 0, 2, []byte("x")); !errors.Is(err, ErrFragmentAssemblerFull) {
		t.Fatalf("byte capacity +1 error = %v", err)
	}
	if snapshot := assembler.Snapshot(); shardedFragmentGroup(assembler, 3, 1) != nil || snapshot.RetainedGroups != 2 || snapshot.RetainedBackingBytes != byteLimit {
		t.Fatal("byte-cap rejection changed assembler state")
	}
	if _, err := assembler.AddFragment(1, 1, 1, 2, payload); !errors.Is(err, ErrFragmentAssemblerFull) {
		t.Fatalf("existing group byte capacity error = %v", err)
	}
	if shardedFragmentGroup(assembler, 1, 1) != group || group.received != 1 || assembler.Snapshot().RetainedBackingBytes != byteLimit {
		t.Fatal("byte-cap rejection changed the existing group")
	}

	if _, err := assembler.AddFragment(2, 1, 2, 3, []byte("mismatch")); !errors.Is(err, ErrFragmentTotalMismatch) {
		t.Fatalf("release at byte capacity: %v", err)
	}
	if snapshot := assembler.Snapshot(); snapshot.RetainedBackingBytes != wantCharge || snapshot.RetainedGroups != 1 {
		t.Fatalf("mismatch retained state = %+v, want 1 group/%d bytes", snapshot, wantCharge)
	}
	if _, err := assembler.AddFragment(1, 1, 1, 2, payload); err != nil {
		t.Fatalf("completion after capacity recovery: %v", err)
	}
	if snapshot := assembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
		t.Fatalf("completion retained capacity: %+v", snapshot)
	}
}

func TestShardedFragmentAssemblerDefendsAgainstInvalidStoredDataLength(t *testing.T) {
	assembler := newShardedFragmentAssemblerFixture(t)
	if _, err := assembler.AddFragment(1, 29, 0, 2, []byte("first")); err != nil {
		t.Fatalf("create group: %v", err)
	}
	group := shardedFragmentGroup(assembler, 1, 29)
	group.data = group.data[:1]

	result, err := assembler.AddFragment(1, 29, 1, 2, []byte("second"))
	if !errors.Is(err, ErrInvalidFragIndex) {
		t.Fatalf("expected ErrInvalidFragIndex, got %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result, got %q", result)
	}
	if shardedFragmentGroup(assembler, 1, 29) != group {
		t.Fatal("defensive bounds check unexpectedly deleted the group")
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
