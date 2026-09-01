package protocol

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"pgregory.net/rapid"
)

func drawPropertyData(t *rapid.T, minSize, maxSize int) []byte {
	data := make([]byte, rapid.IntRange(minSize, maxSize).Draw(t, "dataLen"))
	for i := range data {
		data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
	}
	return data
}

func reassemblePropertyResults(t *rapid.T, sessionID uint32, results []DatagramResult, order []int, checkSession bool) []byte {
	if order == nil {
		order = make([]int, len(results))
		for i := range order {
			order[i] = i
		}
	}

	assembler := NewFragmentAssembler()
	var reassembled []byte
	for _, i := range order {
		parsed, err := DecodeUDPDatagram(results[i].Data)
		if err != nil {
			t.Fatalf("DecodeUDPDatagram failed for fragment %d: %v", i, err)
		}
		if !parsed.IsFragmented {
			t.Errorf("Fragment %d is not marked as fragmented", i)
		}
		if checkSession && parsed.SessionID != sessionID {
			t.Errorf("Fragment %d has session ID %d, expected %d", i, parsed.SessionID, sessionID)
		}

		assembled, err := assembler.AddFragment(parsed.SessionID, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
		if err != nil {
			t.Fatalf("AddFragment failed for fragment %d: %v", i, err)
		}
		if assembled != nil {
			reassembled = assembled
		}
	}
	return reassembled
}

func assertPropertyPayloadEqual(t *rapid.T, got, want []byte) {
	if got == nil {
		t.Fatal("Reassembly did not complete")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Reassembled payload differs from original: got %d bytes, want %d", len(got), len(want))
	}
}

func collectConcurrentFragmentIDs(
	sessionID uint32,
	data []byte,
	counters []*atomic.Uint32,
	numGoroutines, callsPerGoroutine int,
	simultaneousStart bool,
	stopOnError bool,
) [][]uint16 {
	collected := make([][]uint16, len(counters))
	var mu sync.Mutex
	var wg sync.WaitGroup

	var start <-chan struct{}
	var barrier chan struct{}
	if simultaneousStart {
		barrier = make(chan struct{})
		start = barrier
	}

	for counterIndex := range counters {
		for range numGoroutines {
			wg.Go(func() {
				if start != nil {
					<-start
				}
				for range callsPerGoroutine {
					results, err := FragmentUDPPooled(sessionID, data, counters[counterIndex], true)
					if err != nil {
						if stopOnError {
							return
						}
						continue
					}
					if len(results) > 0 {
						parsed, parseErr := DecodeUDPDatagram(results[0].Data)
						if parseErr == nil && parsed.IsFragmented {
							mu.Lock()
							collected[counterIndex] = append(collected[counterIndex], parsed.FragmentID)
							mu.Unlock()
						}
					}
					ReleaseDatagramResults(results)
				}
			})
		}
	}

	if simultaneousStart {
		close(barrier)
	}
	wg.Wait()
	return collected
}

// Feature: udp-performance-optimization, Property 1: Pooled Fragment Lifecycle
// *For any* valid UDP data and session ID, calling `FragmentUDPPooled` SHALL return
// `DatagramResult` objects where each result has a non-nil `Buffer` pointer, and
// calling `ReleaseDatagramResults` on those results SHALL complete without error.
// **Validates: Requirements 1.1, 1.2**

// TestPooledFragmentLifecycle_Property verifies that FragmentUDPPooled returns
// DatagramResult objects with non-nil Buffer pointers, and ReleaseDatagramResults
// completes without error.
func TestPooledFragmentLifecycle_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random session ID
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		// Generate random UDP data (0 to 10KB to cover both fragmented and unfragmented cases)
		dataLen := rapid.IntRange(0, 10*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		// Create atomic counter for fragment IDs
		var fragIDCounter atomic.Uint32

		// Call FragmentUDPPooled with fragmentation enabled
		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)

		// For empty data, we still expect a valid result
		if len(data) == 0 {
			if err != nil {
				t.Fatalf("FragmentUDPPooled failed for empty data: %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("Expected 1 result for empty data, got %d", len(results))
			}
		} else {
			if err != nil {
				t.Fatalf("FragmentUDPPooled failed: %v", err)
			}
		}

		// Property 1: All results must have non-nil Buffer pointers
		for i, result := range results {
			if result.Buffer == nil {
				t.Errorf("Result %d has nil Buffer pointer", i)
			}
			// Also verify Data is not nil and has content
			if result.Data == nil {
				t.Errorf("Result %d has nil Data", i)
			}
		}

		// Property 2: ReleaseDatagramResults must complete without panic
		// (errors would manifest as panics in this context)
		ReleaseDatagramResults(results)

		// Property 3: After release, Buffer pointers should be nil
		for i, result := range results {
			if result.Buffer != nil {
				t.Errorf("Result %d still has non-nil Buffer after release", i)
			}
		}
	})
}

// TestPooledFragmentLifecycle_SmallPackets_Property verifies the lifecycle
// for packets that don't require fragmentation (data <= MaxUDPPayload).
func TestPooledFragmentLifecycle_SmallPackets_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random session ID
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		// Generate small data that won't require fragmentation
		// MaxUDPPayload = MaxDatagramSize - UDPHeaderSize = 1200 - 5 = 1195
		dataLen := rapid.IntRange(1, MaxUDPPayload).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		var fragIDCounter atomic.Uint32

		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled failed for small packet: %v", err)
		}

		// Property: Small packets should produce exactly 1 result
		if len(results) != 1 {
			t.Errorf("Expected 1 result for small packet, got %d", len(results))
		}

		// Property: The single result must have non-nil Buffer
		if results[0].Buffer == nil {
			t.Error("Small packet result has nil Buffer pointer")
		}

		// Property: Data length should be header + payload
		expectedLen := UDPHeaderSize + dataLen
		if len(results[0].Data) != expectedLen {
			t.Errorf("Expected Data length %d, got %d", expectedLen, len(results[0].Data))
		}

		// Release and verify
		ReleaseDatagramResults(results)
		if results[0].Buffer != nil {
			t.Error("Buffer not nil after release")
		}
	})
}

// TestPooledFragmentLifecycle_LargePackets_Property verifies the lifecycle
// for packets that require fragmentation (data > MaxUDPPayload).
func TestPooledFragmentLifecycle_LargePackets_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sessionID := rapid.Uint32().Draw(t, "sessionID")
		data := drawPropertyData(t, MaxUDPPayload+1, 50*1024)

		var fragIDCounter atomic.Uint32

		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled failed for large packet: %v", err)
		}

		// Property: Large packets should produce multiple results
		expectedFragments := (len(data) + MaxFragPayload - 1) / MaxFragPayload
		if len(results) != expectedFragments {
			t.Errorf("Expected %d fragments, got %d", expectedFragments, len(results))
		}

		// Property: All results must have non-nil Buffer pointers
		for i, result := range results {
			if result.Buffer == nil {
				t.Errorf("Fragment %d has nil Buffer pointer", i)
			}
			if result.Data == nil {
				t.Errorf("Fragment %d has nil Data", i)
			}
		}

		// Release and verify all buffers are returned
		ReleaseDatagramResults(results)
		for i, result := range results {
			if result.Buffer != nil {
				t.Errorf("Fragment %d Buffer not nil after release", i)
			}
		}
	})
}

// TestPooledFragmentLifecycle_FragmentationDisabled_Property verifies behavior
// when fragmentation is disabled and packet is too large.
func TestPooledFragmentLifecycle_FragmentationDisabled_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random session ID
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		// Generate data larger than MaxUDPPayload
		dataLen := rapid.IntRange(MaxUDPPayload+1, 5*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		var fragIDCounter atomic.Uint32

		// Call with fragmentation disabled
		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, false)

		// Property: Should return ErrFragmentationDisabled
		if !errors.Is(err, ErrFragmentationDisabled) {
			t.Errorf("Expected ErrFragmentationDisabled, got %v", err)
		}

		// Property: Results should be nil when error occurs
		if results != nil {
			t.Errorf("Expected nil results on error, got %d results", len(results))
		}
	})
}

// TestPooledFragmentLifecycle_MultipleReleases_Property verifies that calling
// ReleaseDatagramResults multiple times is safe (idempotent).
func TestPooledFragmentLifecycle_MultipleReleases_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sessionID := rapid.Uint32().Draw(t, "sessionID")
		dataLen := rapid.IntRange(1, 5*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		var fragIDCounter atomic.Uint32

		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled failed: %v", err)
		}

		// First release
		ReleaseDatagramResults(results)

		// Property: All buffers should be nil after first release
		for i, result := range results {
			if result.Buffer != nil {
				t.Errorf("Result %d Buffer not nil after first release", i)
			}
		}

		// Property: Second release should be safe (no panic)
		ReleaseDatagramResults(results)

		// Property: Third release should also be safe
		ReleaseDatagramResults(results)
	})
}

// TestPooledFragmentLifecycle_EmptyResults_Property verifies that
// ReleaseDatagramResults handles empty and nil slices safely.
func TestPooledFragmentLifecycle_EmptyResults_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Property: Releasing nil slice should not panic
		ReleaseDatagramResults(nil)

		// Property: Releasing empty slice should not panic
		ReleaseDatagramResults([]DatagramResult{})

		// Property: Releasing slice with nil Buffer should not panic
		results := []DatagramResult{
			{Data: []byte{1, 2, 3}, Buffer: nil},
		}
		ReleaseDatagramResults(results)
	})
}

// Feature: udp-performance-optimization, Property 4: Fragment Reassembly Round-Trip
// *For any* valid UDP data that requires fragmentation, fragmenting with `FragmentUDPPooled`
// and then reassembling all fragments with `FragmentAssembler.AddFragment` SHALL produce
// data equal to the original input.
// **Validates: Requirements 3.2**

// TestFragmentReassemblyRoundTrip_Property verifies that fragmenting data and then
// reassembling it produces the original data.
func TestFragmentReassemblyRoundTrip_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sessionID := rapid.Uint32().Draw(t, "sessionID")
		data := drawPropertyData(t, MaxUDPPayload+1, 50*1024)

		var fragIDCounter atomic.Uint32

		// Fragment the data using pooled fragmentation
		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled failed: %v", err)
		}
		defer ReleaseDatagramResults(results)

		// Property: Should produce multiple fragments for large data
		if len(results) < 2 {
			t.Fatalf("Expected multiple fragments for data of size %d, got %d", len(data), len(results))
		}

		assertPropertyPayloadEqual(t, reassemblePropertyResults(t, sessionID, results, nil, true), data)
	})
}

// TestFragmentReassemblyRoundTrip_OutOfOrder_Property verifies that fragments can be
// reassembled correctly even when received out of order.
func TestFragmentReassemblyRoundTrip_OutOfOrder_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sessionID := rapid.Uint32().Draw(t, "sessionID")
		data := drawPropertyData(t, MaxUDPPayload+1, 20*1024)

		var fragIDCounter atomic.Uint32

		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled failed: %v", err)
		}
		defer ReleaseDatagramResults(results)

		// Generate a random permutation of fragment indices
		numFragments := len(results)
		order := make([]int, numFragments)
		for i := range order {
			order[i] = i
		}
		// Fisher-Yates shuffle using rapid's random generator
		for i := numFragments - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(t, "shuffleIndex")
			order[i], order[j] = order[j], order[i]
		}

		assertPropertyPayloadEqual(t, reassemblePropertyResults(t, sessionID, results, order, false), data)
	})
}

// TestFragmentReassemblyRoundTrip_BoundarySize_Property verifies round-trip for data
// at exact fragment boundaries (multiples of MaxFragPayload).
func TestFragmentReassemblyRoundTrip_BoundarySize_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		// Generate data that is an exact multiple of MaxFragPayload
		// This tests boundary conditions where fragments are exactly full
		numFragments := rapid.IntRange(2, 20).Draw(t, "numFragments")
		dataLen := numFragments * MaxFragPayload
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		var fragIDCounter atomic.Uint32

		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled failed: %v", err)
		}
		defer ReleaseDatagramResults(results)

		// Property: Number of fragments should match expected
		if len(results) != numFragments {
			t.Errorf("Expected %d fragments, got %d", numFragments, len(results))
		}

		assertPropertyPayloadEqual(t, reassemblePropertyResults(t, sessionID, results, nil, false), data)
	})
}

// TestFragmentReassemblyRoundTrip_MinFragmentation_Property verifies round-trip for
// data that just barely requires fragmentation (MaxUDPPayload + 1 byte).
func TestFragmentReassemblyRoundTrip_MinFragmentation_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		// Generate data that is just over the fragmentation threshold
		// This creates exactly 2 fragments: one full, one with 1 byte
		dataLen := MaxUDPPayload + 1
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		var fragIDCounter atomic.Uint32

		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled failed: %v", err)
		}
		defer ReleaseDatagramResults(results)

		// Property: Should produce exactly 2 fragments
		// Note: MaxUDPPayload = 1195, MaxFragPayload = 1191
		// For 1197 bytes: ceil(1197/1191) = 2 fragments
		expectedFragments := (dataLen + MaxFragPayload - 1) / MaxFragPayload
		if len(results) != expectedFragments {
			t.Errorf("Expected %d fragments, got %d", expectedFragments, len(results))
		}

		assertPropertyPayloadEqual(t, reassemblePropertyResults(t, sessionID, results, nil, false), data)
	})
}

func TestShardCalculationDeterminism_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		shardCount := rapid.IntRange(1, 256).Draw(t, "shardCount")
		key := fragmentKey{
			sessionID: rapid.Uint32().Draw(t, "sessionID"),
			fragID:    rapid.Uint16().Draw(t, "fragID"),
		}
		assembler := NewShardedFragmentAssembler(shardCount)
		defer assembler.Close()

		index := assembler.shardIndex(key)
		shard := assembler.getShard(key)
		if index < 0 || index >= shardCount || shard != &assembler.shards[index] {
			t.Fatalf("key=%+v shardCount=%d: invalid shard %d", key, shardCount, index)
		}
		for i := range 10 {
			repeatShard := assembler.getShard(key)
			if repeatShard != shard {
				t.Fatalf("key=%+v returned different shards on call %d", key, i+1)
			}
		}
	})
}

func TestShardCalculationDeterminism_DefaultShardCount_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		key := fragmentKey{
			sessionID: rapid.Uint32().Draw(t, "sessionID"),
			fragID:    rapid.Uint16().Draw(t, "fragID"),
		}
		assembler := NewShardedFragmentAssembler(0)
		defer assembler.Close()
		if len(assembler.shards) != DefaultShardCount {
			t.Fatalf("expected default shard count %d, got %d", DefaultShardCount, len(assembler.shards))
		}
		index := assembler.shardIndex(key)
		if index < 0 || index >= DefaultShardCount || assembler.getShard(key) != &assembler.shards[index] {
			t.Fatalf("key=%+v produced invalid default shard %d", key, index)
		}
	})
}

// Feature: udp-performance-optimization, Property 5: Concurrent Fragment Correctness
// *For any* set of concurrent fragment operations on the `ShardedFragmentAssembler`,
// all complete fragment groups SHALL be reassembled correctly without data corruption.
// **Validates: Requirements 4.1**

// TestConcurrentFragmentCorrectness_Property verifies that fragments can be added
// concurrently from multiple goroutines and all complete groups are reassembled
// correctly without data corruption.
func TestConcurrentFragmentCorrectness_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random shard count for the assembler
		shardCount := rapid.IntRange(1, 32).Draw(t, "shardCount")

		// Generate random number of concurrent packets to process
		numPackets := rapid.IntRange(1, 10).Draw(t, "numPackets")

		// Generate random number of goroutines for concurrent access
		numGoroutines := rapid.IntRange(2, 8).Draw(t, "numGoroutines")

		// Create the sharded fragment assembler
		assembler := NewShardedFragmentAssembler(shardCount)

		// Generate random packets that require fragmentation
		type packetData struct {
			sessionID uint32
			data      []byte
			results   []DatagramResult
		}
		packets := make([]packetData, numPackets)

		var fragIDCounter atomic.Uint32

		for i := range numPackets {
			// Generate unique session ID for each packet
			sessionID := rapid.Uint32().Draw(t, "sessionID")

			// Generate data that requires fragmentation (> MaxUDPPayload = 1195 bytes)
			// Keep size reasonable to avoid too many fragments
			dataLen := rapid.IntRange(MaxUDPPayload+1, 10*1024).Draw(t, "dataLen")
			data := make([]byte, dataLen)
			for j := range data {
				data[j] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
			}

			// Fragment the data
			results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
			if err != nil {
				t.Fatalf("FragmentUDPPooled failed for packet %d: %v", i, err)
			}

			packets[i] = packetData{
				sessionID: sessionID,
				data:      data,
				results:   results,
			}
		}

		// Collect all fragments from all packets
		type fragmentInfo struct {
			packetIdx int
			sessionID uint32
			fragID    uint16
			fragIndex uint8
			fragTotal uint8
			payload   []byte
		}
		var allFragments []fragmentInfo

		for pktIdx, pkt := range packets {
			for _, result := range pkt.results {
				parsed, err := DecodeUDPDatagram(result.Data)
				if err != nil {
					t.Fatalf("DecodeUDPDatagram failed: %v", err)
				}
				if !parsed.IsFragmented {
					t.Fatal("Expected fragmented packet")
				}

				// Make a copy of payload since we'll release the buffers
				payloadCopy := make([]byte, len(parsed.Payload))
				copy(payloadCopy, parsed.Payload)

				allFragments = append(allFragments, fragmentInfo{
					packetIdx: pktIdx,
					sessionID: parsed.SessionID,
					fragID:    parsed.FragmentID,
					fragIndex: parsed.FragmentIndex,
					fragTotal: parsed.FragmentTotal,
					payload:   payloadCopy,
				})
			}
		}

		// Release the datagram buffers
		for _, pkt := range packets {
			ReleaseDatagramResults(pkt.results)
		}

		// Shuffle fragments to simulate out-of-order arrival
		for i := len(allFragments) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(t, "shuffleIndex")
			allFragments[i], allFragments[j] = allFragments[j], allFragments[i]
		}

		// Track reassembled results per packet
		reassembledResults := make([][]byte, numPackets)
		var resultsMu sync.Mutex

		// Add fragments concurrently from multiple goroutines
		var wg sync.WaitGroup
		fragmentsPerGoroutine := (len(allFragments) + numGoroutines - 1) / numGoroutines

		for g := range numGoroutines {
			start := g * fragmentsPerGoroutine
			end := min(start+fragmentsPerGoroutine, len(allFragments))
			if start >= len(allFragments) {
				break
			}

			wg.Add(1)
			go func(fragments []fragmentInfo) {
				defer wg.Done()

				for _, frag := range fragments {
					result, err := assembler.AddFragment(
						frag.sessionID,
						frag.fragID,
						frag.fragIndex,
						frag.fragTotal,
						frag.payload,
					)
					if err != nil {
						t.Errorf("AddFragment packet %d: %v", frag.packetIdx, err)
						return
					}

					if result != nil {
						resultsMu.Lock()
						reassembledResults[frag.packetIdx] = result
						resultsMu.Unlock()
					}
				}
			}(allFragments[start:end])
		}

		wg.Wait()

		// Property: All packets should be reassembled correctly
		for i, pkt := range packets {
			reassembled := reassembledResults[i]

			// Property: Reassembly should complete for each packet
			if reassembled == nil {
				t.Errorf("Packet %d was not reassembled", i)
				continue
			}

			// Property: Reassembled data length should match original
			if len(reassembled) != len(pkt.data) {
				t.Errorf("Packet %d: length mismatch - expected %d, got %d",
					i, len(pkt.data), len(reassembled))
				continue
			}

			// Property: Reassembled data should match original exactly (no corruption)
			for j := range pkt.data {
				if reassembled[j] != pkt.data[j] {
					t.Errorf("Packet %d: data corruption at byte %d - expected %d, got %d",
						i, j, pkt.data[j], reassembled[j])
					break
				}
			}
		}
	})
}

// TestConcurrentFragmentCorrectness_HighContention_Property verifies concurrent
// fragment correctness under high contention with many goroutines accessing
// the same shards simultaneously.
func TestConcurrentFragmentCorrectness_HighContention_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Use small shard count to increase contention
		shardCount := rapid.IntRange(1, 4).Draw(t, "shardCount")

		// Generate a single large packet to fragment
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		// Generate data that creates many fragments
		dataLen := rapid.IntRange(5*1024, 20*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		var fragIDCounter atomic.Uint32

		// Fragment the data
		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled failed: %v", err)
		}

		// Parse all fragments
		type fragmentInfo struct {
			fragID    uint16
			fragIndex uint8
			fragTotal uint8
			payload   []byte
		}
		fragments := make([]fragmentInfo, len(results))

		for i, result := range results {
			parsed, err := DecodeUDPDatagram(result.Data)
			if err != nil {
				t.Fatalf("DecodeUDPDatagram failed: %v", err)
			}
			if !parsed.IsFragmented {
				t.Fatal("Expected fragmented packet")
			}

			// Copy payload before releasing buffers
			payloadCopy := make([]byte, len(parsed.Payload))
			copy(payloadCopy, parsed.Payload)

			fragments[i] = fragmentInfo{
				fragID:    parsed.FragmentID,
				fragIndex: parsed.FragmentIndex,
				fragTotal: parsed.FragmentTotal,
				payload:   payloadCopy,
			}
		}

		ReleaseDatagramResults(results)

		// Create assembler with small shard count for high contention
		assembler := NewShardedFragmentAssembler(shardCount)

		// Use many goroutines (one per fragment) to maximize contention
		numGoroutines := len(fragments)
		var reassembled []byte
		var reassembledMu sync.Mutex
		var wg sync.WaitGroup

		for i := range numGoroutines {
			wg.Add(1)
			go func(frag fragmentInfo) {
				defer wg.Done()

				result, err := assembler.AddFragment(
					sessionID,
					frag.fragID,
					frag.fragIndex,
					frag.fragTotal,
					frag.payload,
				)
				if err != nil {
					t.Errorf("AddFragment index %d: %v", frag.fragIndex, err)
					return
				}

				if result != nil {
					reassembledMu.Lock()
					reassembled = result
					reassembledMu.Unlock()
				}
			}(fragments[i])
		}

		wg.Wait()

		// Property: Reassembly should complete
		if reassembled == nil {
			t.Fatal("Reassembly did not complete under high contention")
		}

		// Property: Reassembled data should match original
		if len(reassembled) != len(data) {
			t.Errorf("Length mismatch: expected %d, got %d", len(data), len(reassembled))
		}

		// Property: No data corruption
		for i := range data {
			if i < len(reassembled) && reassembled[i] != data[i] {
				t.Errorf("Data corruption at byte %d under high contention", i)
				break
			}
		}
	})
}

// TestConcurrentFragmentCorrectness_MultiplePacketsSameShard_Property verifies
// that multiple packets whose fragments map to the same shard are handled correctly.
func TestConcurrentFragmentCorrectness_MultiplePacketsSameShard_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Use single shard to force all fragments to same shard
		assembler := NewShardedFragmentAssembler(1)

		// Generate multiple packets
		numPackets := rapid.IntRange(2, 5).Draw(t, "numPackets")

		type packetInfo struct {
			sessionID uint32
			data      []byte
			fragments []struct {
				fragID    uint16
				fragIndex uint8
				fragTotal uint8
				payload   []byte
			}
		}
		packets := make([]packetInfo, numPackets)

		var fragIDCounter atomic.Uint32

		for i := range numPackets {
			sessionID := rapid.Uint32().Draw(t, "sessionID")

			// Generate data requiring fragmentation
			dataLen := rapid.IntRange(MaxUDPPayload+1, 5*1024).Draw(t, "dataLen")
			data := make([]byte, dataLen)
			for j := range data {
				data[j] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
			}

			results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
			if err != nil {
				t.Fatalf("FragmentUDPPooled failed: %v", err)
			}

			packets[i] = packetInfo{
				sessionID: sessionID,
				data:      data,
			}

			for _, result := range results {
				parsed, err := DecodeUDPDatagram(result.Data)
				if err != nil {
					t.Fatalf("DecodeUDPDatagram failed: %v", err)
				}

				payloadCopy := make([]byte, len(parsed.Payload))
				copy(payloadCopy, parsed.Payload)

				packets[i].fragments = append(packets[i].fragments, struct {
					fragID    uint16
					fragIndex uint8
					fragTotal uint8
					payload   []byte
				}{
					fragID:    parsed.FragmentID,
					fragIndex: parsed.FragmentIndex,
					fragTotal: parsed.FragmentTotal,
					payload:   payloadCopy,
				})
			}

			ReleaseDatagramResults(results)
		}

		// Add all fragments concurrently
		reassembledResults := make([][]byte, numPackets)
		var resultsMu sync.Mutex
		var wg sync.WaitGroup

		for pktIdx, pkt := range packets {
			for _, frag := range pkt.fragments {
				wg.Add(1)
				go func(pktIdx int, sessionID uint32, frag struct {
					fragID    uint16
					fragIndex uint8
					fragTotal uint8
					payload   []byte
				}) {
					defer wg.Done()

					result, err := assembler.AddFragment(
						sessionID,
						frag.fragID,
						frag.fragIndex,
						frag.fragTotal,
						frag.payload,
					)
					if err != nil {
						t.Errorf("AddFragment packet %d index %d: %v", pktIdx, frag.fragIndex, err)
						return
					}

					if result != nil {
						resultsMu.Lock()
						reassembledResults[pktIdx] = result
						resultsMu.Unlock()
					}
				}(pktIdx, pkt.sessionID, frag)
			}
		}

		wg.Wait()

		// Property: All packets should be reassembled correctly
		for i, pkt := range packets {
			reassembled := reassembledResults[i]

			if reassembled == nil {
				t.Errorf("Packet %d was not reassembled with single shard", i)
				continue
			}

			if len(reassembled) != len(pkt.data) {
				t.Errorf("Packet %d: length mismatch - expected %d, got %d",
					i, len(pkt.data), len(reassembled))
				continue
			}

			for j := range pkt.data {
				if reassembled[j] != pkt.data[j] {
					t.Errorf("Packet %d: data corruption at byte %d with single shard",
						i, j)
					break
				}
			}
		}
	})
}

// TestConcurrentFragmentCorrectness_DuplicateFragments_Property verifies that
// duplicate fragments sent concurrently don't cause data corruption.
func TestConcurrentFragmentCorrectness_DuplicateFragments_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		shardCount := rapid.IntRange(1, 16).Draw(t, "shardCount")
		assembler := NewShardedFragmentAssembler(shardCount)

		sessionID := rapid.Uint32().Draw(t, "sessionID")

		// Generate data requiring fragmentation
		dataLen := rapid.IntRange(MaxUDPPayload+1, 10*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		var fragIDCounter atomic.Uint32

		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled failed: %v", err)
		}

		// Parse fragments
		type fragmentInfo struct {
			fragID    uint16
			fragIndex uint8
			fragTotal uint8
			payload   []byte
		}
		fragments := make([]fragmentInfo, len(results))

		for i, result := range results {
			parsed, err := DecodeUDPDatagram(result.Data)
			if err != nil {
				t.Fatalf("DecodeUDPDatagram failed: %v", err)
			}

			payloadCopy := make([]byte, len(parsed.Payload))
			copy(payloadCopy, parsed.Payload)

			fragments[i] = fragmentInfo{
				fragID:    parsed.FragmentID,
				fragIndex: parsed.FragmentIndex,
				fragTotal: parsed.FragmentTotal,
				payload:   payloadCopy,
			}
		}

		ReleaseDatagramResults(results)

		// Create duplicates of each fragment
		duplicateCount := rapid.IntRange(2, 4).Draw(t, "duplicateCount")
		var allFragments []fragmentInfo
		for _, frag := range fragments {
			for range duplicateCount {
				allFragments = append(allFragments, frag)
			}
		}

		// Shuffle all fragments including duplicates
		for i := len(allFragments) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(t, "shuffleIndex")
			allFragments[i], allFragments[j] = allFragments[j], allFragments[i]
		}

		// Add all fragments (including duplicates) concurrently
		var reassembled []byte
		var reassembledMu sync.Mutex
		var wg sync.WaitGroup

		for _, frag := range allFragments {
			wg.Add(1)
			go func(frag fragmentInfo) {
				defer wg.Done()

				result, err := assembler.AddFragment(
					sessionID,
					frag.fragID,
					frag.fragIndex,
					frag.fragTotal,
					frag.payload,
				)
				if err != nil {
					t.Errorf("AddFragment index %d: %v", frag.fragIndex, err)
					return
				}

				if result != nil {
					reassembledMu.Lock()
					if reassembled == nil {
						reassembled = result
					}
					reassembledMu.Unlock()
				}
			}(frag)
		}

		wg.Wait()

		// Property: Reassembly should complete exactly once
		if reassembled == nil {
			t.Fatal("Reassembly did not complete with duplicate fragments")
		}

		// Property: Reassembled data should match original (no corruption from duplicates)
		if len(reassembled) != len(data) {
			t.Errorf("Length mismatch: expected %d, got %d", len(data), len(reassembled))
		}

		for i := range data {
			if i < len(reassembled) && reassembled[i] != data[i] {
				t.Errorf("Data corruption at byte %d with duplicate fragments", i)
				break
			}
		}
	})
}

// Feature: udp-performance-optimization, Property 7: Atomic Counter Thread-Safety
// *For any* number of concurrent calls to `FragmentUDPPooled` with the same atomic counter,
// each call SHALL receive a unique fragment ID, and no fragment IDs SHALL be duplicated.
// **Validates: Requirements 5.1, 5.4**

// TestAtomicCounterThreadSafety_Property verifies that concurrent calls to
// FragmentUDPPooled with the same atomic counter produce unique fragment IDs
// with no duplicates.
func TestAtomicCounterThreadSafety_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random number of concurrent goroutines (2 to 32)
		numGoroutines := rapid.IntRange(2, 32).Draw(t, "numGoroutines")

		// Generate random number of calls per goroutine (1 to 10)
		callsPerGoroutine := rapid.IntRange(1, 10).Draw(t, "callsPerGoroutine")

		// Create a shared atomic counter
		var fragIDCounter atomic.Uint32

		// Generate random session ID
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		data := drawPropertyData(t, MaxUDPPayload+1, 5*1024)
		collectedFragIDs := collectConcurrentFragmentIDs(
			sessionID, data, []*atomic.Uint32{&fragIDCounter}, numGoroutines, callsPerGoroutine, false, true,
		)[0]

		// Property: Total number of fragment IDs should equal total calls
		expectedCalls := numGoroutines * callsPerGoroutine
		if len(collectedFragIDs) != expectedCalls {
			t.Errorf("Expected %d fragment IDs, got %d", expectedCalls, len(collectedFragIDs))
		}

		// Property: All fragment IDs should be unique (no duplicates)
		seen := make(map[uint16]int)
		for _, fragID := range collectedFragIDs {
			seen[fragID]++
		}

		for fragID, count := range seen {
			if count > 1 {
				t.Errorf("Fragment ID %d was duplicated %d times", fragID, count)
			}
		}

		// Property: Number of unique fragment IDs should equal total calls
		if len(seen) != expectedCalls {
			t.Errorf("Expected %d unique fragment IDs, got %d", expectedCalls, len(seen))
		}
	})
}

// TestAtomicCounterThreadSafety_HighContention_Property verifies atomic counter
// thread-safety under high contention with many goroutines making rapid calls.
func TestAtomicCounterThreadSafety_HighContention_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Use high number of goroutines for contention
		numGoroutines := rapid.IntRange(16, 64).Draw(t, "numGoroutines")

		// Multiple calls per goroutine
		callsPerGoroutine := rapid.IntRange(5, 20).Draw(t, "callsPerGoroutine")

		var fragIDCounter atomic.Uint32
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		data := drawPropertyData(t, MaxUDPPayload+1, 3*1024)
		collectedFragIDs := collectConcurrentFragmentIDs(
			sessionID, data, []*atomic.Uint32{&fragIDCounter}, numGoroutines, callsPerGoroutine, true, false,
		)[0]

		// Property: All fragment IDs should be unique
		seen := make(map[uint16]int)
		for _, fragID := range collectedFragIDs {
			seen[fragID]++
		}

		duplicates := 0
		for fragID, count := range seen {
			if count > 1 {
				duplicates++
				t.Errorf("Fragment ID %d was duplicated %d times under high contention", fragID, count)
			}
		}

		if duplicates > 0 {
			t.Errorf("Found %d duplicate fragment IDs under high contention", duplicates)
		}

		// Property: Counter value should reflect total increments
		expectedCalls := numGoroutines * callsPerGoroutine
		finalCounter := fragIDCounter.Load()
		if int(finalCounter) != expectedCalls {
			t.Errorf("Counter value %d doesn't match expected calls %d", finalCounter, expectedCalls)
		}
	})
}

// TestAtomicCounterThreadSafety_SequentialIDs_Property verifies that fragment IDs
// are sequential (no gaps) when collected from concurrent operations.
func TestAtomicCounterThreadSafety_SequentialIDs_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		numGoroutines := rapid.IntRange(4, 16).Draw(t, "numGoroutines")
		callsPerGoroutine := rapid.IntRange(2, 8).Draw(t, "callsPerGoroutine")

		var fragIDCounter atomic.Uint32
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		data := drawPropertyData(t, MaxUDPPayload+1, 4*1024)
		collectedFragIDs := collectConcurrentFragmentIDs(
			sessionID, data, []*atomic.Uint32{&fragIDCounter}, numGoroutines, callsPerGoroutine, false, false,
		)[0]

		expectedCalls := numGoroutines * callsPerGoroutine

		// Property: Should have collected all expected fragment IDs
		if len(collectedFragIDs) != expectedCalls {
			t.Errorf("Expected %d fragment IDs, got %d", expectedCalls, len(collectedFragIDs))
		}

		// Property: Fragment IDs should form a contiguous sequence from 1 to expectedCalls
		// (since atomic.Add returns the new value after incrementing)
		seen := make(map[uint16]bool)
		for _, fragID := range collectedFragIDs {
			seen[fragID] = true
		}

		// Check that all IDs from 1 to expectedCalls are present
		for i := 1; i <= expectedCalls; i++ {
			if !seen[uint16(i)] {
				t.Errorf("Missing fragment ID %d in sequence", i)
			}
		}

		// Property: No IDs outside the expected range
		for fragID := range seen {
			if fragID < 1 || int(fragID) > expectedCalls {
				t.Errorf("Fragment ID %d is outside expected range [1, %d]", fragID, expectedCalls)
			}
		}
	})
}

// TestAtomicCounterThreadSafety_MultipleCounters_Property verifies that multiple
// independent atomic counters work correctly when used concurrently.
func TestAtomicCounterThreadSafety_MultipleCounters_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Number of independent counters (simulating multiple sessions)
		numCounters := rapid.IntRange(2, 8).Draw(t, "numCounters")
		numGoroutines := rapid.IntRange(2, 8).Draw(t, "numGoroutines")
		callsPerGoroutine := rapid.IntRange(2, 5).Draw(t, "callsPerGoroutine")

		// Create multiple independent counters
		counters := make([]*atomic.Uint32, numCounters)
		for i := range counters {
			counters[i] = &atomic.Uint32{}
		}

		sessionID := rapid.Uint32().Draw(t, "sessionID")

		data := drawPropertyData(t, MaxUDPPayload+1, 3*1024)

		collectedFragIDsPerCounter := collectConcurrentFragmentIDs(
			sessionID, data, counters, numGoroutines, callsPerGoroutine, false, false,
		)

		// Property: Each counter should have unique fragment IDs within its scope
		for cIdx, fragIDs := range collectedFragIDsPerCounter {
			seen := make(map[uint16]int)
			for _, fragID := range fragIDs {
				seen[fragID]++
			}

			for fragID, count := range seen {
				if count > 1 {
					t.Errorf("Counter %d: Fragment ID %d was duplicated %d times", cIdx, fragID, count)
				}
			}

			// Property: Each counter should have the expected number of unique IDs
			expectedCalls := numGoroutines * callsPerGoroutine
			if len(seen) != expectedCalls {
				t.Errorf("Counter %d: Expected %d unique fragment IDs, got %d", cIdx, expectedCalls, len(seen))
			}
		}

		// Property: Each counter's final value should match total calls
		expectedCalls := numGoroutines * callsPerGoroutine
		for cIdx, counter := range counters {
			finalValue := counter.Load()
			if int(finalValue) != expectedCalls {
				t.Errorf("Counter %d: Final value %d doesn't match expected %d", cIdx, finalValue, expectedCalls)
			}
		}
	})
}

// TestAtomicCounterThreadSafety_CounterOverflow_Property verifies that the atomic
// counter handles wrap-around correctly (uint16 fragment ID from uint32 counter).
func TestAtomicCounterThreadSafety_CounterOverflow_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Start counter near uint16 max to test wrap-around behavior
		// Fragment ID is uint16, so we test behavior around 65535
		startValue := rapid.Uint32Range(65530, 65535).Draw(t, "startValue")

		var fragIDCounter atomic.Uint32
		fragIDCounter.Store(startValue)

		sessionID := rapid.Uint32().Draw(t, "sessionID")

		data := drawPropertyData(t, MaxUDPPayload+1, 3*1024)

		// Make enough calls to wrap around uint16
		numCalls := rapid.IntRange(10, 20).Draw(t, "numCalls")

		numGoroutines := rapid.IntRange(2, 8).Draw(t, "numGoroutines")
		callsPerGoroutine := (numCalls + numGoroutines - 1) / numGoroutines
		collectedFragIDs := collectConcurrentFragmentIDs(
			sessionID, data, []*atomic.Uint32{&fragIDCounter}, numGoroutines, callsPerGoroutine, false, false,
		)[0]

		// Property: All fragment IDs should still be unique even with wrap-around
		seen := make(map[uint16]int)
		for _, fragID := range collectedFragIDs {
			seen[fragID]++
		}

		for fragID, count := range seen {
			if count > 1 {
				t.Errorf("Fragment ID %d was duplicated %d times during counter overflow", fragID, count)
			}
		}

		// Property: Counter should have incremented by total calls
		expectedFinalValue := startValue + uint32(numGoroutines*callsPerGoroutine)
		actualFinalValue := fragIDCounter.Load()
		if actualFinalValue != expectedFinalValue {
			t.Errorf("Counter final value %d doesn't match expected %d", actualFinalValue, expectedFinalValue)
		}
	})
}
