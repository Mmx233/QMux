package protocol

import (
	"errors"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

func TestShardedFragmentAssemblerSnapshotCapacityReasons(t *testing.T) {
	assembler := NewShardedFragmentAssembler(2, 0, 0)
	defer assembler.Close()
	assembler.maxGroups = 1
	assembler.maxBytes = int64(FragmentBufferSize)

	if _, err := assembler.AddFragment(1, 1, 0, 2, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := assembler.AddFragment(2, 1, 0, 2, []byte("b")); !errors.Is(err, ErrFragmentAssemblerFull) {
		t.Fatalf("group capacity error = %v", err)
	}
	assembler.maxGroups = 2
	if _, err := assembler.AddFragment(2, 1, 0, 2, []byte("b")); !errors.Is(err, ErrFragmentAssemblerFull) {
		t.Fatalf("byte capacity error = %v", err)
	}

	want := FragmentSnapshot{
		RetainedGroups:       1,
		RetainedBackingBytes: int64(FragmentBufferSize),
		GroupCapacityDrops:   1,
		ByteCapacityDrops:    1,
	}
	if got := assembler.Snapshot(); got != want {
		t.Fatalf("Snapshot() = %+v, want %+v", got, want)
	}

	assembler.Close()
	want.RetainedGroups = 0
	want.RetainedBackingBytes = 0
	if got := assembler.Snapshot(); got != want {
		t.Fatalf("Snapshot() after Close = %+v, want %+v", got, want)
	}
}

func TestShardedFragmentAssemblerSnapshotExpiration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assembler := NewShardedFragmentAssembler(1, 0, 0)
		defer assembler.Close()
		if _, err := assembler.AddFragment(1, 1, 0, 2, []byte("fragment")); err != nil {
			t.Fatal(err)
		}
		want := FragmentSnapshot{RetainedGroups: 1, RetainedBackingBytes: int64(FragmentBufferSize)}
		if got := assembler.Snapshot(); got != want {
			t.Fatalf("Snapshot() before expiration = %+v, want %+v", got, want)
		}

		synctest.Wait()
		// Expiration requires age > FragmentTimeout, so allow two cleanup ticks.
		time.Sleep(2 * FragmentTimeout)
		synctest.Wait()
		want = FragmentSnapshot{ExpiredGroups: 1}
		if got := assembler.Snapshot(); got != want {
			t.Fatalf("Snapshot() after expiration = %+v, want %+v", got, want)
		}
		assembler.Close()
		if got := assembler.Snapshot(); got != want {
			t.Fatalf("Snapshot() after Close = %+v, want %+v", got, want)
		}
	})
}

func TestFragmentSnapshotDoesNotWaitForShardLocks(t *testing.T) {
	assembler := NewShardedFragmentAssembler(1, 0, 0)
	defer assembler.Close()
	if _, err := assembler.AddFragment(1, 1, 0, 2, []byte("fragment")); err != nil {
		t.Fatal(err)
	}
	assembler.shards[0].mu.Lock()
	done := make(chan FragmentSnapshot, 1)
	go func() { done <- assembler.Snapshot() }()
	select {
	case snapshot := <-done:
		assembler.shards[0].mu.Unlock()
		if snapshot.RetainedGroups != 1 {
			t.Fatalf("snapshot = %+v", snapshot)
		}
	case <-time.After(time.Second):
		assembler.shards[0].mu.Unlock()
		<-done
		t.Fatal("monitoring blocked on a fragment writer")
	}
}

func BenchmarkShardedFragmentAssemblerSnapshot(b *testing.B) {
	for _, groups := range []int{0, 256, maxRetainedFragmentGroups} {
		b.Run(strconv.Itoa(groups), func(b *testing.B) {
			// Retained-state fixtures omit the cleanup worker so they cannot expire.
			assembler := &ShardedFragmentAssembler{shards: make([]fragmentShard, DefaultShardCount)}
			for i := range assembler.shards {
				assembler.shards[i].fragments = make(map[fragmentKey]*fragmentGroup)
			}
			for i := range groups {
				assembler.shards[i%DefaultShardCount].fragments[fragmentKey{sessionID: uint32(i)}] = &fragmentGroup{
					retainedBytes: int64(FragmentBufferSize),
				}
			}
			want := FragmentSnapshot{
				RetainedGroups:       int64(groups),
				RetainedBackingBytes: int64(groups) * int64(FragmentBufferSize),
			}
			assembler.retainedGroups.Store(want.RetainedGroups)
			assembler.retainedBytes.Store(want.RetainedBackingBytes)
			b.ReportAllocs()
			var snapshot FragmentSnapshot
			for b.Loop() {
				snapshot = assembler.Snapshot()
			}
			if snapshot != want {
				b.Fatalf("Snapshot() = %+v, want %+v", snapshot, want)
			}
		})
	}
}
