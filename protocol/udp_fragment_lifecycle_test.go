package protocol

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestShardedFragmentAssemblerCloseBeforeUse(t *testing.T) {
	assembler := NewShardedFragmentAssembler(4, 0, 0)
	done := assembler.doneCh
	assembler.Close()
	assembler.Close()

	select {
	case <-done:
	default:
		t.Fatal("cleanup goroutine survived Close")
	}
	if _, err := assembler.AddFragment(1, 7, 0, 2, []byte("pending")); !errors.Is(err, ErrFragmentAssemblerClosed) {
		t.Fatalf("AddFragment() after Close error = %v, want %v", err, ErrFragmentAssemblerClosed)
	}
}

func TestShardedFragmentAssemblerCloseReleasesPendingGroups(t *testing.T) {
	assembler := NewShardedFragmentAssembler(4, 0, 0)
	if result, err := assembler.AddFragment(1, 7, 0, 2, []byte("pending")); err != nil || result != nil {
		t.Fatalf("AddFragment() = (%q, %v), want (nil, nil)", result, err)
	}
	group := shardedFragmentGroup(assembler, 1, 7)
	if group == nil {
		t.Fatal("pending fragment group was not created")
	}
	done := assembler.doneCh

	var callers sync.WaitGroup
	callers.Add(8)
	for range 8 {
		go func() {
			defer callers.Done()
			assembler.Close()
		}()
	}
	closed := make(chan struct{})
	go func() {
		callers.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("concurrent Close calls did not finish")
	}

	select {
	case <-done:
	default:
		t.Fatal("cleanup goroutine survived Close")
	}
	if got := shardedFragmentGroupCount(assembler); got != 0 {
		t.Fatalf("Close left %d pending fragment groups", got)
	}
	if snapshot := assembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
		t.Fatalf("Close retained budget: %+v", snapshot)
	}
	if group.data != nil || group.buffers != nil || group.received != 0 {
		t.Fatalf("Close retained released group data: data=%v buffers=%v received=%d", group.data, group.buffers, group.received)
	}
}

func TestShardedFragmentAssemblerAddRacesWithClose(t *testing.T) {
	assembler := NewShardedFragmentAssembler(4, 0, 0)
	start := make(chan struct{})
	var callers sync.WaitGroup
	for range 32 {
		callers.Go(func() {
			<-start
			_, err := assembler.AddFragment(1, 7, 0, 2, []byte("pending"))
			if err != nil && !errors.Is(err, ErrFragmentAssemblerClosed) {
				t.Errorf("AddFragment error = %v", err)
			}
		})
	}
	callers.Go(func() {
		<-start
		assembler.Close()
	})
	close(start)
	callers.Wait()
	assembler.Close()

	if snapshot := assembler.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
		t.Fatalf("Add/Close retained budget: %+v", snapshot)
	}
	if got := shardedFragmentGroupCount(assembler); got != 0 {
		t.Fatalf("Add/Close left %d groups", got)
	}
}
