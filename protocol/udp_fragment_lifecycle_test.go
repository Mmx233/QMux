package protocol

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fragmentLifecycleHarness struct {
	add        func() ([]byte, error)
	close      func()
	done       <-chan struct{}
	group      func() *fragmentGroup
	groupCount func() int
}

func fragmentLifecycleHarnesses() map[string]func() fragmentLifecycleHarness {
	return map[string]func() fragmentLifecycleHarness{
		"regular": func() fragmentLifecycleHarness {
			assembler := NewFragmentAssembler()
			return fragmentLifecycleHarness{
				add: func() ([]byte, error) {
					return assembler.AddFragment(1, 7, 0, 2, []byte("pending"))
				},
				close: assembler.Close,
				done:  assembler.doneCh,
				group: func() *fragmentGroup {
					return assembler.fragments[7]
				},
				groupCount: func() int {
					return len(assembler.fragments)
				},
			}
		},
		"sharded": func() fragmentLifecycleHarness {
			assembler := NewShardedFragmentAssembler(4)
			return fragmentLifecycleHarness{
				add: func() ([]byte, error) {
					return assembler.AddFragment(1, 7, 0, 2, []byte("pending"))
				},
				close: assembler.Close,
				done:  assembler.doneCh,
				group: func() *fragmentGroup {
					return assembler.getShard(7).fragments[7]
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

func TestFragmentAssemblerCloseBeforeUse(t *testing.T) {
	for name, newHarness := range fragmentLifecycleHarnesses() {
		t.Run(name, func(t *testing.T) {
			harness := newHarness()
			harness.close()
			harness.close()

			select {
			case <-harness.done:
			default:
				t.Fatal("cleanup goroutine survived Close")
			}
			if _, err := harness.add(); !errors.Is(err, ErrFragmentAssemblerClosed) {
				t.Fatalf("AddFragment() after Close error = %v, want %v", err, ErrFragmentAssemblerClosed)
			}
		})
	}
}

func TestFragmentAssemblerCloseReleasesPendingGroups(t *testing.T) {
	for name, newHarness := range fragmentLifecycleHarnesses() {
		t.Run(name, func(t *testing.T) {
			harness := newHarness()
			if result, err := harness.add(); err != nil || result != nil {
				t.Fatalf("AddFragment() = (%q, %v), want (nil, nil)", result, err)
			}
			group := harness.group()
			if group == nil {
				t.Fatal("pending fragment group was not created")
			}

			var callers sync.WaitGroup
			callers.Add(8)
			for range 8 {
				go func() {
					defer callers.Done()
					harness.close()
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
			case <-harness.done:
			default:
				t.Fatal("cleanup goroutine survived Close")
			}
			if got := harness.groupCount(); got != 0 {
				t.Fatalf("Close left %d pending fragment groups", got)
			}
			if group.data != nil || group.buffers != nil || group.received != 0 {
				t.Fatalf("Close retained released group data: data=%v buffers=%v received=%d", group.data, group.buffers, group.received)
			}
		})
	}
}
