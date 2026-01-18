package protocol

import (
	"sync"
	"sync/atomic"
	"testing"

	"pgregory.net/rapid"
)

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
		// MaxUDPPayload = MaxDatagramSize - UDPHeaderSize = 1200 - 4 = 1196
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
		// Generate random session ID
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		// Generate large data that requires fragmentation
		// MaxUDPPayload = 1196, so anything larger needs fragmentation
		// Limit to reasonable size to avoid too many fragments (max 255)
		// MaxFragPayload = 1191, so max data = 255 * 1191 = ~303KB
		// We'll test up to 50KB for reasonable test times
		dataLen := rapid.IntRange(MaxUDPPayload+1, 50*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		var fragIDCounter atomic.Uint32

		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled failed for large packet: %v", err)
		}

		// Property: Large packets should produce multiple results
		expectedFragments := (dataLen + MaxFragPayload - 1) / MaxFragPayload
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
		if err != ErrFragmentationDisabled {
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
		// Generate random session ID
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		// Generate data that requires fragmentation (> MaxUDPPayload = 1196 bytes)
		// Limit to reasonable size to avoid too many fragments (max 255)
		// MaxFragPayload = 1191, so max data = 255 * 1191 = ~303KB
		// We'll test up to 50KB for reasonable test times
		dataLen := rapid.IntRange(MaxUDPPayload+1, 50*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		var fragIDCounter atomic.Uint32

		// Fragment the data using pooled fragmentation
		results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
		if err != nil {
			t.Fatalf("FragmentUDPPooled failed: %v", err)
		}
		defer ReleaseDatagramResults(results)

		// Property: Should produce multiple fragments for large data
		if len(results) < 2 {
			t.Fatalf("Expected multiple fragments for data of size %d, got %d", dataLen, len(results))
		}

		// Create a fragment assembler for reassembly
		assembler := NewFragmentAssembler()

		// Parse and reassemble all fragments
		var reassembled []byte
		for i, result := range results {
			// Parse the datagram to extract fragment info
			parsedSessionID, isFragmented, fragID, fragIndex, fragTotal, payload, err := ParseUDPDatagram(result.Data)
			if err != nil {
				t.Fatalf("ParseUDPDatagram failed for fragment %d: %v", i, err)
			}

			// Property: All fragments should be marked as fragmented
			if !isFragmented {
				t.Errorf("Fragment %d is not marked as fragmented", i)
			}

			// Property: Session ID should match
			if parsedSessionID != sessionID {
				t.Errorf("Fragment %d has wrong session ID: expected %d, got %d", i, sessionID, parsedSessionID)
			}

			// Add fragment to assembler
			result, err := assembler.AddFragment(parsedSessionID, fragID, fragIndex, fragTotal, payload)
			if err != nil {
				t.Fatalf("AddFragment failed for fragment %d: %v", i, err)
			}

			// The last fragment should complete the reassembly
			if result != nil {
				reassembled = result
			}
		}

		// Property: Reassembly should complete
		if reassembled == nil {
			t.Fatal("Reassembly did not complete after all fragments were added")
		}

		// Property: Reassembled data should equal original data
		if len(reassembled) != len(data) {
			t.Errorf("Reassembled data length mismatch: expected %d, got %d", len(data), len(reassembled))
		}

		for i := range data {
			if i < len(reassembled) && reassembled[i] != data[i] {
				t.Errorf("Data mismatch at byte %d: expected %d, got %d", i, data[i], reassembled[i])
				break // Only report first mismatch
			}
		}
	})
}

// TestFragmentReassemblyRoundTrip_OutOfOrder_Property verifies that fragments can be
// reassembled correctly even when received out of order.
func TestFragmentReassemblyRoundTrip_OutOfOrder_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sessionID := rapid.Uint32().Draw(t, "sessionID")

		// Generate data that requires fragmentation
		dataLen := rapid.IntRange(MaxUDPPayload+1, 20*1024).Draw(t, "dataLen")
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

		assembler := NewFragmentAssembler()

		var reassembled []byte
		for _, idx := range order {
			result := results[idx]
			parsedSessionID, isFragmented, fragID, fragIndex, fragTotal, payload, err := ParseUDPDatagram(result.Data)
			if err != nil {
				t.Fatalf("ParseUDPDatagram failed: %v", err)
			}

			if !isFragmented {
				t.Error("Fragment not marked as fragmented")
			}

			assembled, err := assembler.AddFragment(parsedSessionID, fragID, fragIndex, fragTotal, payload)
			if err != nil {
				t.Fatalf("AddFragment failed: %v", err)
			}

			if assembled != nil {
				reassembled = assembled
			}
		}

		// Property: Reassembly should complete regardless of order
		if reassembled == nil {
			t.Fatal("Reassembly did not complete")
		}

		// Property: Reassembled data should equal original
		if len(reassembled) != len(data) {
			t.Errorf("Length mismatch: expected %d, got %d", len(data), len(reassembled))
		}

		for i := range data {
			if i < len(reassembled) && reassembled[i] != data[i] {
				t.Errorf("Data mismatch at byte %d", i)
				break
			}
		}
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

		assembler := NewFragmentAssembler()

		var reassembled []byte
		for i, result := range results {
			parsedSessionID, isFragmented, fragID, fragIndex, fragTotal, payload, err := ParseUDPDatagram(result.Data)
			if err != nil {
				t.Fatalf("ParseUDPDatagram failed for fragment %d: %v", i, err)
			}

			if !isFragmented {
				t.Errorf("Fragment %d not marked as fragmented", i)
			}

			assembled, err := assembler.AddFragment(parsedSessionID, fragID, fragIndex, fragTotal, payload)
			if err != nil {
				t.Fatalf("AddFragment failed for fragment %d: %v", i, err)
			}

			if assembled != nil {
				reassembled = assembled
			}
		}

		if reassembled == nil {
			t.Fatal("Reassembly did not complete")
		}

		// Property: Reassembled data should equal original exactly
		if len(reassembled) != len(data) {
			t.Errorf("Length mismatch: expected %d, got %d", len(data), len(reassembled))
		}

		for i := range data {
			if i < len(reassembled) && reassembled[i] != data[i] {
				t.Errorf("Data mismatch at byte %d", i)
				break
			}
		}
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
		// Note: MaxUDPPayload = 1196, MaxFragPayload = 1191
		// For 1197 bytes: ceil(1197/1191) = 2 fragments
		expectedFragments := (dataLen + MaxFragPayload - 1) / MaxFragPayload
		if len(results) != expectedFragments {
			t.Errorf("Expected %d fragments, got %d", expectedFragments, len(results))
		}

		assembler := NewFragmentAssembler()

		var reassembled []byte
		for i, result := range results {
			parsedSessionID, isFragmented, fragID, fragIndex, fragTotal, payload, err := ParseUDPDatagram(result.Data)
			if err != nil {
				t.Fatalf("ParseUDPDatagram failed for fragment %d: %v", i, err)
			}

			if !isFragmented {
				t.Errorf("Fragment %d not marked as fragmented", i)
			}

			assembled, err := assembler.AddFragment(parsedSessionID, fragID, fragIndex, fragTotal, payload)
			if err != nil {
				t.Fatalf("AddFragment failed for fragment %d: %v", i, err)
			}

			if assembled != nil {
				reassembled = assembled
			}
		}

		if reassembled == nil {
			t.Fatal("Reassembly did not complete")
		}

		// Property: Reassembled data should equal original exactly
		if len(reassembled) != len(data) {
			t.Errorf("Length mismatch: expected %d, got %d", len(data), len(reassembled))
		}

		for i := range data {
			if i < len(reassembled) && reassembled[i] != data[i] {
				t.Errorf("Data mismatch at byte %d", i)
				break
			}
		}
	})
}

// Feature: udp-performance-optimization, Property 6: Shard Calculation Determinism
// *For any* fragment ID and shard count, the shard index SHALL equal `fragID % shardCount`,
// and the same fragment ID SHALL always map to the same shard.
// **Validates: Requirements 4.3**

// TestShardCalculationDeterminism_Property verifies that shard calculation is deterministic
// and follows the formula: shardIndex = fragID % shardCount
func TestShardCalculationDeterminism_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random shard count (1 to 256, covering typical configurations)
		shardCount := rapid.IntRange(1, 256).Draw(t, "shardCount")

		// Generate random fragment ID (full uint16 range)
		fragID := rapid.Uint16().Draw(t, "fragID")

		// Create a sharded fragment assembler with the given shard count
		assembler := NewShardedFragmentAssembler(shardCount)

		// Get the shard for this fragment ID
		shard := assembler.getShard(fragID)

		// Property 1: Shard index should equal fragID % shardCount
		// We verify this by checking that the shard pointer matches the expected index
		expectedIndex := int(fragID) % shardCount
		expectedShard := &assembler.shards[expectedIndex]

		if shard != expectedShard {
			t.Errorf("Shard mismatch for fragID=%d, shardCount=%d: expected shard at index %d, got different shard",
				fragID, shardCount, expectedIndex)
		}

		// Property 2: Same fragment ID should always map to the same shard (determinism)
		// Call getShard multiple times and verify consistency
		for i := 0; i < 10; i++ {
			repeatShard := assembler.getShard(fragID)
			if repeatShard != shard {
				t.Errorf("Non-deterministic shard calculation: fragID=%d returned different shards on call %d",
					fragID, i+1)
			}
		}
	})
}

// TestShardCalculationDeterminism_DefaultShardCount_Property verifies shard calculation
// with the default shard count (16).
func TestShardCalculationDeterminism_DefaultShardCount_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random fragment ID
		fragID := rapid.Uint16().Draw(t, "fragID")

		// Create assembler with default shard count (passing 0 or negative uses default)
		assembler := NewShardedFragmentAssembler(0)

		// Verify default shard count is 16
		if assembler.shardCount != DefaultShardCount {
			t.Errorf("Expected default shard count %d, got %d", DefaultShardCount, assembler.shardCount)
		}

		// Get the shard
		shard := assembler.getShard(fragID)

		// Property: Shard index should equal fragID % 16
		expectedIndex := int(fragID) % DefaultShardCount
		expectedShard := &assembler.shards[expectedIndex]

		if shard != expectedShard {
			t.Errorf("Shard mismatch for fragID=%d with default shard count: expected index %d",
				fragID, expectedIndex)
		}
	})
}

// TestShardCalculationDeterminism_Distribution_Property verifies that fragment IDs
// are distributed across shards according to the modulo formula.
func TestShardCalculationDeterminism_Distribution_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random shard count
		shardCount := rapid.IntRange(1, 64).Draw(t, "shardCount")

		// Generate a batch of random fragment IDs
		numFragIDs := rapid.IntRange(10, 100).Draw(t, "numFragIDs")

		assembler := NewShardedFragmentAssembler(shardCount)

		// Track which shard each fragment ID maps to
		for i := 0; i < numFragIDs; i++ {
			fragID := rapid.Uint16().Draw(t, "fragID")

			shard := assembler.getShard(fragID)
			expectedIndex := int(fragID) % shardCount
			expectedShard := &assembler.shards[expectedIndex]

			// Property: Every fragment ID should map to the correct shard
			if shard != expectedShard {
				t.Errorf("Shard mismatch for fragID=%d, shardCount=%d: expected index %d",
					fragID, shardCount, expectedIndex)
			}
		}
	})
}

// TestShardCalculationDeterminism_EdgeCases_Property verifies shard calculation
// for edge case values of fragment IDs and shard counts.
func TestShardCalculationDeterminism_EdgeCases_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Test with shard count of 1 (all fragments go to same shard)
		assembler1 := NewShardedFragmentAssembler(1)
		fragID := rapid.Uint16().Draw(t, "fragID")

		shard := assembler1.getShard(fragID)
		// Property: With shard count 1, all fragments should go to shard 0
		if shard != &assembler1.shards[0] {
			t.Errorf("With shardCount=1, fragID=%d should map to shard 0", fragID)
		}

		// Test with maximum uint16 fragment ID
		maxFragID := uint16(65535)
		shardCount := rapid.IntRange(1, 256).Draw(t, "shardCount")
		assembler2 := NewShardedFragmentAssembler(shardCount)

		shardMax := assembler2.getShard(maxFragID)
		expectedIndex := int(maxFragID) % shardCount
		expectedShard := &assembler2.shards[expectedIndex]

		if shardMax != expectedShard {
			t.Errorf("Shard mismatch for maxFragID=%d, shardCount=%d: expected index %d",
				maxFragID, shardCount, expectedIndex)
		}

		// Test with fragment ID 0
		shard0 := assembler2.getShard(0)
		if shard0 != &assembler2.shards[0] {
			t.Error("FragID=0 should always map to shard 0")
		}
	})
}

// TestShardCalculationDeterminism_ConsecutiveFragIDs_Property verifies that
// consecutive fragment IDs are distributed across shards in a round-robin fashion.
func TestShardCalculationDeterminism_ConsecutiveFragIDs_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		shardCount := rapid.IntRange(2, 32).Draw(t, "shardCount")
		startFragID := rapid.Uint16().Draw(t, "startFragID")

		assembler := NewShardedFragmentAssembler(shardCount)

		// Check consecutive fragment IDs
		for i := 0; i < shardCount*2; i++ {
			fragID := startFragID + uint16(i)
			shard := assembler.getShard(fragID)

			expectedIndex := int(fragID) % shardCount
			expectedShard := &assembler.shards[expectedIndex]

			// Property: Consecutive IDs should follow modulo pattern
			if shard != expectedShard {
				t.Errorf("Consecutive fragID=%d (start=%d, offset=%d) should map to shard %d",
					fragID, startFragID, i, expectedIndex)
			}
		}
	})
}

// TestShardCalculationDeterminism_AcrossAssemblers_Property verifies that
// different assembler instances with the same shard count produce consistent results.
func TestShardCalculationDeterminism_AcrossAssemblers_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		shardCount := rapid.IntRange(1, 64).Draw(t, "shardCount")
		fragID := rapid.Uint16().Draw(t, "fragID")

		// Create two separate assemblers with the same shard count
		assembler1 := NewShardedFragmentAssembler(shardCount)
		assembler2 := NewShardedFragmentAssembler(shardCount)

		// Get shard indices (not pointers, since they're different instances)
		shard1 := assembler1.getShard(fragID)
		shard2 := assembler2.getShard(fragID)

		// Find the index of each shard
		var index1, index2 int
		for i := range assembler1.shards {
			if &assembler1.shards[i] == shard1 {
				index1 = i
				break
			}
		}
		for i := range assembler2.shards {
			if &assembler2.shards[i] == shard2 {
				index2 = i
				break
			}
		}

		// Property: Same fragID and shardCount should produce same shard index
		if index1 != index2 {
			t.Errorf("Different assemblers produced different shard indices for fragID=%d, shardCount=%d: %d vs %d",
				fragID, shardCount, index1, index2)
		}

		// Property: Index should match the expected formula
		expectedIndex := int(fragID) % shardCount
		if index1 != expectedIndex {
			t.Errorf("Shard index %d doesn't match expected %d for fragID=%d, shardCount=%d",
				index1, expectedIndex, fragID, shardCount)
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

		for i := 0; i < numPackets; i++ {
			// Generate unique session ID for each packet
			sessionID := rapid.Uint32().Draw(t, "sessionID")

			// Generate data that requires fragmentation (> MaxUDPPayload = 1196 bytes)
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
				parsedSessionID, isFragmented, fragID, fragIndex, fragTotal, payload, err := ParseUDPDatagram(result.Data)
				if err != nil {
					t.Fatalf("ParseUDPDatagram failed: %v", err)
				}
				if !isFragmented {
					t.Fatal("Expected fragmented packet")
				}

				// Make a copy of payload since we'll release the buffers
				payloadCopy := make([]byte, len(payload))
				copy(payloadCopy, payload)

				allFragments = append(allFragments, fragmentInfo{
					packetIdx: pktIdx,
					sessionID: parsedSessionID,
					fragID:    fragID,
					fragIndex: fragIndex,
					fragTotal: fragTotal,
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

		for g := 0; g < numGoroutines; g++ {
			start := g * fragmentsPerGoroutine
			end := start + fragmentsPerGoroutine
			if end > len(allFragments) {
				end = len(allFragments)
			}
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
						// Session ID mismatch can happen if fragment IDs collide
						// across different packets - this is expected behavior
						continue
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
			_, isFragmented, fragID, fragIndex, fragTotal, payload, err := ParseUDPDatagram(result.Data)
			if err != nil {
				t.Fatalf("ParseUDPDatagram failed: %v", err)
			}
			if !isFragmented {
				t.Fatal("Expected fragmented packet")
			}

			// Copy payload before releasing buffers
			payloadCopy := make([]byte, len(payload))
			copy(payloadCopy, payload)

			fragments[i] = fragmentInfo{
				fragID:    fragID,
				fragIndex: fragIndex,
				fragTotal: fragTotal,
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

		for i := 0; i < numGoroutines; i++ {
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

		for i := 0; i < numPackets; i++ {
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
				_, _, fragID, fragIndex, fragTotal, payload, err := ParseUDPDatagram(result.Data)
				if err != nil {
					t.Fatalf("ParseUDPDatagram failed: %v", err)
				}

				payloadCopy := make([]byte, len(payload))
				copy(payloadCopy, payload)

				packets[i].fragments = append(packets[i].fragments, struct {
					fragID    uint16
					fragIndex uint8
					fragTotal uint8
					payload   []byte
				}{
					fragID:    fragID,
					fragIndex: fragIndex,
					fragTotal: fragTotal,
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
			_, _, fragID, fragIndex, fragTotal, payload, err := ParseUDPDatagram(result.Data)
			if err != nil {
				t.Fatalf("ParseUDPDatagram failed: %v", err)
			}

			payloadCopy := make([]byte, len(payload))
			copy(payloadCopy, payload)

			fragments[i] = fragmentInfo{
				fragID:    fragID,
				fragIndex: fragIndex,
				fragTotal: fragTotal,
				payload:   payloadCopy,
			}
		}

		ReleaseDatagramResults(results)

		// Create duplicates of each fragment
		duplicateCount := rapid.IntRange(2, 4).Draw(t, "duplicateCount")
		var allFragments []fragmentInfo
		for _, frag := range fragments {
			for d := 0; d < duplicateCount; d++ {
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

		// Generate data that requires fragmentation (> MaxUDPPayload = 1196 bytes)
		// This ensures FragmentUDPPooled will use the atomic counter
		dataLen := rapid.IntRange(MaxUDPPayload+1, 5*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		// Collect all fragment IDs from all goroutines
		var collectedFragIDs []uint16
		var mu sync.Mutex
		var wg sync.WaitGroup

		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				for c := 0; c < callsPerGoroutine; c++ {
					// Call FragmentUDPPooled with the shared counter
					results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
					if err != nil {
						return
					}

					// Extract fragment ID from the first fragment
					// All fragments in a single call share the same fragment ID
					if len(results) > 0 {
						_, isFragmented, fragID, _, _, _, err := ParseUDPDatagram(results[0].Data)
						if err == nil && isFragmented {
							mu.Lock()
							collectedFragIDs = append(collectedFragIDs, fragID)
							mu.Unlock()
						}
					}

					// Release the buffers
					ReleaseDatagramResults(results)
				}
			}()
		}

		wg.Wait()

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

		// Use data that requires fragmentation
		dataLen := rapid.IntRange(MaxUDPPayload+1, 3*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		var collectedFragIDs []uint16
		var mu sync.Mutex
		var wg sync.WaitGroup

		// Start all goroutines simultaneously using a barrier
		startBarrier := make(chan struct{})

		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				// Wait for all goroutines to be ready
				<-startBarrier

				for c := 0; c < callsPerGoroutine; c++ {
					results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
					if err != nil {
						continue
					}

					if len(results) > 0 {
						_, isFragmented, fragID, _, _, _, err := ParseUDPDatagram(results[0].Data)
						if err == nil && isFragmented {
							mu.Lock()
							collectedFragIDs = append(collectedFragIDs, fragID)
							mu.Unlock()
						}
					}

					ReleaseDatagramResults(results)
				}
			}()
		}

		// Release all goroutines at once for maximum contention
		close(startBarrier)
		wg.Wait()

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

		// Data requiring fragmentation
		dataLen := rapid.IntRange(MaxUDPPayload+1, 4*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		var collectedFragIDs []uint16
		var mu sync.Mutex
		var wg sync.WaitGroup

		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				for c := 0; c < callsPerGoroutine; c++ {
					results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
					if err != nil {
						continue
					}

					if len(results) > 0 {
						_, isFragmented, fragID, _, _, _, err := ParseUDPDatagram(results[0].Data)
						if err == nil && isFragmented {
							mu.Lock()
							collectedFragIDs = append(collectedFragIDs, fragID)
							mu.Unlock()
						}
					}

					ReleaseDatagramResults(results)
				}
			}()
		}

		wg.Wait()

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

		// Data requiring fragmentation
		dataLen := rapid.IntRange(MaxUDPPayload+1, 3*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		// Collect fragment IDs per counter
		collectedFragIDsPerCounter := make([][]uint16, numCounters)
		for i := range collectedFragIDsPerCounter {
			collectedFragIDsPerCounter[i] = make([]uint16, 0)
		}
		var mu sync.Mutex
		var wg sync.WaitGroup

		for counterIdx := 0; counterIdx < numCounters; counterIdx++ {
			for g := 0; g < numGoroutines; g++ {
				wg.Add(1)
				go func(cIdx int) {
					defer wg.Done()

					for c := 0; c < callsPerGoroutine; c++ {
						results, err := FragmentUDPPooled(sessionID, data, counters[cIdx], true)
						if err != nil {
							continue
						}

						if len(results) > 0 {
							_, isFragmented, fragID, _, _, _, err := ParseUDPDatagram(results[0].Data)
							if err == nil && isFragmented {
								mu.Lock()
								collectedFragIDsPerCounter[cIdx] = append(collectedFragIDsPerCounter[cIdx], fragID)
								mu.Unlock()
							}
						}

						ReleaseDatagramResults(results)
					}
				}(counterIdx)
			}
		}

		wg.Wait()

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

		// Data requiring fragmentation
		dataLen := rapid.IntRange(MaxUDPPayload+1, 3*1024).Draw(t, "dataLen")
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(rapid.IntRange(0, 255).Draw(t, "dataByte"))
		}

		// Make enough calls to wrap around uint16
		numCalls := rapid.IntRange(10, 20).Draw(t, "numCalls")

		var collectedFragIDs []uint16
		var mu sync.Mutex
		var wg sync.WaitGroup

		numGoroutines := rapid.IntRange(2, 8).Draw(t, "numGoroutines")
		callsPerGoroutine := (numCalls + numGoroutines - 1) / numGoroutines

		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				for c := 0; c < callsPerGoroutine; c++ {
					results, err := FragmentUDPPooled(sessionID, data, &fragIDCounter, true)
					if err != nil {
						continue
					}

					if len(results) > 0 {
						_, isFragmented, fragID, _, _, _, err := ParseUDPDatagram(results[0].Data)
						if err == nil && isFragmented {
							mu.Lock()
							collectedFragIDs = append(collectedFragIDs, fragID)
							mu.Unlock()
						}
					}

					ReleaseDatagramResults(results)
				}
			}()
		}

		wg.Wait()

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
