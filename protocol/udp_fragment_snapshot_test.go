package protocol

import (
	"errors"
	"testing"
)

func TestShardedFragmentAssemblerSnapshotCapacityReasons(t *testing.T) {
	assembler := NewShardedFragmentAssembler(2)
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
