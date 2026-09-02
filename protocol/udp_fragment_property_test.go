package protocol

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"

	"pgregory.net/rapid"
)

func drawFragmentPropertyData(t *rapid.T, minSize, maxSize int) []byte {
	data := make([]byte, rapid.IntRange(minSize, maxSize).Draw(t, "dataLen"))
	for i := range data {
		data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
	}
	return data
}

func TestPooledFragmentLifecycle_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		data := drawFragmentPropertyData(t, 0, 10*1024)
		var counter atomic.Uint32
		results, err := FragmentUDPPooled(rapid.Uint32().Draw(t, "sessionID"), data, &counter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled: %v", err)
		}
		for i, result := range results {
			if result.Buffer == nil || result.Data == nil {
				t.Fatalf("result %d did not own a pooled buffer", i)
			}
		}

		ReleaseDatagramResults(results)
		for i, result := range results {
			if result.Buffer != nil {
				t.Fatalf("result %d retained its released buffer", i)
			}
		}
	})
}

func TestFragmentReassemblyRoundTrip_OutOfOrder_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sessionID := rapid.Uint32().Draw(t, "sessionID")
		data := drawFragmentPropertyData(t, MaxUDPPayload+1, 20*1024)
		var counter atomic.Uint32
		results, err := FragmentUDPPooled(sessionID, data, &counter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled: %v", err)
		}
		defer ReleaseDatagramResults(results)

		order := make([]int, len(results))
		for i := range order {
			order[i] = i
		}
		for i := len(order) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(t, "shuffleIndex")
			order[i], order[j] = order[j], order[i]
		}

		assembler := NewFragmentAssembler()
		var got []byte
		for _, i := range order {
			fragment, err := DecodeUDPDatagram(results[i].Data)
			if err != nil {
				t.Fatalf("DecodeUDPDatagram: %v", err)
			}
			got, err = assembler.AddFragment(
				fragment.SessionID,
				fragment.FragmentID,
				fragment.FragmentIndex,
				fragment.FragmentTotal,
				fragment.Payload,
			)
			if err != nil {
				t.Fatalf("AddFragment: %v", err)
			}
			if got != nil && !bytes.Equal(got, data) {
				t.Fatal("reassembled payload differs from input")
			}
		}
		if got == nil {
			t.Fatal("reassembly did not complete")
		}
	})
}

func TestFragmentIDConcurrentUniqueness(t *testing.T) {
	const goroutines = 8
	const callsPerGoroutine = 8
	const total = goroutines * callsPerGoroutine

	data := make([]byte, MaxUDPPayload+1)
	var counter atomic.Uint32
	ids := make(chan uint16, total)
	errs := make(chan error, total)
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range callsPerGoroutine {
				results, err := FragmentUDPPooled(1, data, &counter, true)
				if err != nil {
					errs <- err
					continue
				}
				fragment, err := DecodeUDPDatagram(results[0].Data)
				ReleaseDatagramResults(results)
				if err != nil {
					errs <- err
					continue
				}
				ids <- fragment.FragmentID
			}
		})
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	close(ids)

	seen := make(map[uint16]struct{}, total)
	for id := range ids {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate fragment ID %d", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != total {
		t.Fatalf("unique fragment IDs = %d, want %d", len(seen), total)
	}
}
