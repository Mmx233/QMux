package protocol

import (
	"bytes"
	"errors"
	"hash/maphash"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDatagramResult_Structure tests the DatagramResult struct fields
func TestDatagramResult_Structure(t *testing.T) {
	// Test with pooled buffer
	bufPtr := GetDatagramBuffer()
	data := (*bufPtr)[:100]

	result := DatagramResult{
		Data:   data,
		Buffer: bufPtr,
	}

	if result.Data == nil {
		t.Error("expected Data to be non-nil")
	}
	if result.Buffer == nil {
		t.Error("expected Buffer to be non-nil")
	}
	if len(result.Data) != 100 {
		t.Errorf("expected Data length 100, got %d", len(result.Data))
	}

	// Clean up
	PutDatagramBuffer(bufPtr)
}

// TestDatagramResult_NilBuffer tests DatagramResult with nil buffer
func TestDatagramResult_NilBuffer(t *testing.T) {
	// Test with non-pooled buffer (nil Buffer)
	data := make([]byte, 100)
	result := DatagramResult{
		Data:   data,
		Buffer: nil,
	}

	if result.Data == nil {
		t.Error("expected Data to be non-nil")
	}
	if result.Buffer != nil {
		t.Error("expected Buffer to be nil")
	}
}

// TestReleaseDatagramResults_ReturnsBuffersToPool tests that ReleaseDatagramResults returns buffers to pool
func TestReleaseDatagramResults_ReturnsBuffersToPool(t *testing.T) {
	// Create multiple DatagramResults with pooled buffers
	results := make([]DatagramResult, 3)
	for i := range results {
		bufPtr := GetDatagramBuffer()
		results[i] = DatagramResult{
			Data:   (*bufPtr)[:100],
			Buffer: bufPtr,
		}
	}

	// Verify all buffers are non-nil before release
	for i, r := range results {
		if r.Buffer == nil {
			t.Errorf("result %d: expected Buffer to be non-nil before release", i)
		}
	}

	// Release all buffers
	ReleaseDatagramResults(results)

	// Verify all buffers are nil after release
	for i, r := range results {
		if r.Buffer != nil {
			t.Errorf("result %d: expected Buffer to be nil after release", i)
		}
	}
}

// TestReleaseDatagramResults_HandlesNilBuffers tests that ReleaseDatagramResults handles nil buffers gracefully
func TestReleaseDatagramResults_HandlesNilBuffers(t *testing.T) {
	// Create results with mixed nil and non-nil buffers
	bufPtr := GetDatagramBuffer()
	results := []DatagramResult{
		{Data: make([]byte, 50), Buffer: nil},   // nil buffer
		{Data: (*bufPtr)[:100], Buffer: bufPtr}, // pooled buffer
		{Data: make([]byte, 75), Buffer: nil},   // nil buffer
	}

	// Should not panic
	ReleaseDatagramResults(results)

	// Verify the pooled buffer was released
	if results[1].Buffer != nil {
		t.Error("expected pooled buffer to be nil after release")
	}
}

// TestReleaseDatagramResults_EmptySlice tests that ReleaseDatagramResults handles empty slice
func TestReleaseDatagramResults_EmptySlice(t *testing.T) {
	// Should not panic with empty slice
	ReleaseDatagramResults([]DatagramResult{})
}

// TestReleaseDatagramResults_NilSlice tests that ReleaseDatagramResults handles nil slice
func TestReleaseDatagramResults_NilSlice(t *testing.T) {
	// Should not panic with nil slice
	ReleaseDatagramResults(nil)
}

func TestFragmentUDP_SmallPacket(t *testing.T) {
	// Small packet should not be fragmented
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i)
	}

	var fragID uint16
	datagrams, err := FragmentUDP(12345, data, &fragID, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(datagrams) != 1 {
		t.Fatalf("expected 1 datagram, got %d", len(datagrams))
	}

	// Parse and verify
	parsed, err := DecodeUDPDatagram(datagrams[0])
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if parsed.SessionID != 12345 {
		t.Errorf("expected session ID 12345, got %d", parsed.SessionID)
	}

	if parsed.IsFragmented {
		t.Error("small packet should not be fragmented")
	}

	if !bytes.Equal(parsed.Payload, data) {
		t.Error("payload mismatch")
	}
}

func assertFragmentsReassemble(t *testing.T, datagrams [][]byte, expected []byte) {
	t.Helper()

	assembler := NewFragmentAssembler()
	var result []byte

	for i, datagram := range datagrams {
		parsed, err := DecodeUDPDatagram(datagram)
		if err != nil {
			t.Fatalf("parse error on fragment %d: %v", i, err)
		}
		if parsed.SessionID != 12345 {
			t.Errorf("fragment %d: expected session ID 12345, got %d", i, parsed.SessionID)
		}
		if !parsed.IsFragmented {
			t.Errorf("fragment %d: expected fragmented flag", i)
		}
		if int(parsed.FragmentTotal) != len(datagrams) {
			t.Errorf("fragment %d: expected total %d, got %d", i, len(datagrams), parsed.FragmentTotal)
		}
		if int(parsed.FragmentIndex) != i {
			t.Errorf("fragment %d: expected index %d, got %d", i, i, parsed.FragmentIndex)
		}

		result, err = assembler.AddFragment(parsed.SessionID, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
		if err != nil {
			t.Fatalf("add fragment error: %v", err)
		}
		if i < len(datagrams)-1 && result != nil {
			t.Errorf("fragment %d: expected nil result (more fragments needed)", i)
		}
	}

	if result == nil {
		t.Fatal("expected complete result after all fragments")
	}
	if !bytes.Equal(result, expected) {
		t.Errorf("reassembled data mismatch: expected %d bytes, got %d bytes", len(expected), len(result))
	}
}

func TestFragmentUDP_LargePacket(t *testing.T) {
	// Large packet should be fragmented
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragID uint16
	datagrams, err := FragmentUDP(12345, data, &fragID, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(datagrams) < 2 {
		t.Fatalf("expected multiple datagrams, got %d", len(datagrams))
	}

	assertFragmentsReassemble(t, datagrams, data)
}

func TestFragmentUDP_DisabledFragmentation(t *testing.T) {
	// Large packet with fragmentation disabled should return error
	data := make([]byte, 3000)

	var fragID uint16
	_, err := FragmentUDP(12345, data, &fragID, false)
	if !errors.Is(err, ErrFragmentationDisabled) {
		t.Errorf("expected ErrFragmentationDisabled, got %v", err)
	}
}

func TestFragmentUDP_MaxSize(t *testing.T) {
	// Test packet at exactly max unfragmented size
	data := make([]byte, MaxUDPPayload)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragID uint16
	datagrams, err := FragmentUDP(12345, data, &fragID, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(datagrams) != 1 {
		t.Fatalf("expected 1 datagram for max unfragmented size, got %d", len(datagrams))
	}

	parsed, _ := DecodeUDPDatagram(datagrams[0])
	if parsed.IsFragmented {
		t.Error("packet at max unfragmented size should not be fragmented")
	}

	if !bytes.Equal(parsed.Payload, data) {
		t.Error("payload mismatch")
	}
}

func TestFragmentUDP_JustOverMaxSize(t *testing.T) {
	// Test packet just over max unfragmented size
	data := make([]byte, MaxUDPPayload+1)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragID uint16
	datagrams, err := FragmentUDP(12345, data, &fragID, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(datagrams) != 2 {
		t.Fatalf("expected 2 datagrams, got %d", len(datagrams))
	}

	// Reassemble and verify
	assembler := NewFragmentAssembler()
	var result []byte

	for _, dgram := range datagrams {
		parsed, _ := DecodeUDPDatagram(dgram)
		result, _ = assembler.AddFragment(parsed.SessionID, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
	}

	if !bytes.Equal(result, data) {
		t.Error("reassembled data mismatch")
	}
}

func TestFragmentAssembler_OutOfOrder(t *testing.T) {
	// Test receiving fragments out of order
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragID uint16
	datagrams, _ := FragmentUDP(12345, data, &fragID, true)

	// Receive in reverse order
	assembler := NewFragmentAssembler()
	var result []byte

	for _, datagram := range slices.Backward(datagrams) {
		parsed, _ := DecodeUDPDatagram(datagram)
		var err error
		result, err = assembler.AddFragment(12345, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
		if err != nil {
			t.Fatalf("add fragment error: %v", err)
		}
	}

	if result == nil {
		t.Fatal("expected complete result")
	}

	if !bytes.Equal(result, data) {
		t.Error("reassembled data mismatch")
	}
}

func assertDuplicateFragmentIgnored(
	t *testing.T,
	add func(sessionID uint32, fragID uint16, index, total uint8, payload []byte) ([]byte, error),
	sessionID uint32,
	fragID uint16,
	index, total uint8,
	payload []byte,
) {
	t.Helper()
	if result, err := add(sessionID, fragID, index, total, payload); err != nil || result != nil {
		t.Fatalf("add first fragment: result=%q, err=%v", result, err)
	}
	if result, err := add(sessionID, fragID, index, total, payload); err != nil || result != nil {
		t.Fatalf("add duplicate fragment: result=%q, err=%v", result, err)
	}
}

func TestFragmentAssembler_DuplicateFragment(t *testing.T) {
	// Test receiving duplicate fragments
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragID uint16
	datagrams, _ := FragmentUDP(12345, data, &fragID, true)

	assembler := NewFragmentAssembler()

	// Add first fragment twice
	parsed, _ := DecodeUDPDatagram(datagrams[0])
	assertDuplicateFragmentIgnored(t, assembler.AddFragment, 12345, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)

	// Add remaining fragments
	var result []byte
	for i := 1; i < len(datagrams); i++ {
		parsed, _ := DecodeUDPDatagram(datagrams[i])
		result, _ = assembler.AddFragment(12345, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
	}

	if result == nil {
		t.Fatal("expected complete result")
	}

	if !bytes.Equal(result, data) {
		t.Error("reassembled data mismatch")
	}
}

func TestFragmentAssembler_MissingFragment(t *testing.T) {
	// Test with missing fragment - should not complete
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragID uint16
	datagrams, _ := FragmentUDP(12345, data, &fragID, true)

	assembler := NewFragmentAssembler()

	// Skip middle fragment
	for i, dgram := range datagrams {
		if i == 1 {
			continue // Skip this fragment
		}
		parsed, _ := DecodeUDPDatagram(dgram)
		result, _ := assembler.AddFragment(12345, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
		if result != nil {
			t.Error("should not complete with missing fragment")
		}
	}
}

func assertSameFragmentIDDifferentSessions(t *testing.T, add func(uint32, uint16, uint8, uint8, []byte) ([]byte, error)) {
	t.Helper()
	const fragID = 17
	for _, fragment := range []struct {
		sessionID uint32
		index     uint8
		payload   string
		want      string
	}{
		{sessionID: 1, index: 0, payload: "A"},
		{sessionID: 2, index: 0, payload: "B"},
		{sessionID: 1, index: 1, payload: "a", want: "Aa"},
		{sessionID: 2, index: 1, payload: "b", want: "Bb"},
	} {
		result, err := add(fragment.sessionID, fragID, fragment.index, 2, []byte(fragment.payload))
		if err != nil {
			t.Fatalf("session %d index %d: %v", fragment.sessionID, fragment.index, err)
		}
		if string(result) != fragment.want {
			t.Fatalf("session %d index %d result=%q, want %q", fragment.sessionID, fragment.index, result, fragment.want)
		}
	}
}

func TestFragmentAssembler_SameFragmentIDDifferentSessions(t *testing.T) {
	assembler := NewFragmentAssembler()
	defer assembler.Close()
	assertSameFragmentIDDifferentSessions(t, assembler.AddFragment)
}

func TestFragmentAssembler_InvalidFragmentIndex(t *testing.T) {
	assembler := NewFragmentAssembler()

	// Try to add fragment with index >= total
	_, err := assembler.AddFragment(12345, 1, 5, 3, []byte("test"))
	if !errors.Is(err, ErrInvalidFragIndex) {
		t.Errorf("expected ErrInvalidFragIndex, got %v", err)
	}
}

func TestFragmentAssembler_Timeout(t *testing.T) {
	// This test verifies the cleanup logic conceptually
	// In real scenario, incomplete fragments would be cleaned up after timeout
	assembler := NewFragmentAssembler()

	// Add partial fragments
	if result, err := assembler.AddFragment(12345, 1, 0, 3, []byte("part1")); err != nil || result != nil {
		t.Fatalf("add first partial fragment: result=%q, err=%v", result, err)
	}
	if result, err := assembler.AddFragment(12345, 1, 1, 3, []byte("part2")); err != nil || result != nil {
		t.Fatalf("add second partial fragment: result=%q, err=%v", result, err)
	}
	// Missing fragment 2

	// Verify fragments are stored
	assembler.mu.Lock()
	if len(assembler.fragments) != 1 {
		t.Errorf("expected 1 fragment group, got %d", len(assembler.fragments))
	}
	assembler.mu.Unlock()
}

func TestDecodeUDPDatagram_TooShort(t *testing.T) {
	_, err := DecodeUDPDatagram([]byte{1, 2, 3})
	if !errors.Is(err, ErrDatagramTooShort) {
		t.Errorf("expected ErrDatagramTooShort, got %v", err)
	}
}

func TestFragmentIDCounter(t *testing.T) {
	// Test that fragment ID counter increments
	data := make([]byte, 3000)

	var fragID uint16 = 0
	if _, err := FragmentUDP(12345, data, &fragID, true); err != nil {
		t.Fatalf("first fragmentation: %v", err)
	}
	if fragID != 1 {
		t.Errorf("expected fragID 1, got %d", fragID)
	}

	if _, err := FragmentUDP(12345, data, &fragID, true); err != nil {
		t.Fatalf("second fragmentation: %v", err)
	}
	if fragID != 2 {
		t.Errorf("expected fragID 2, got %d", fragID)
	}
}

func TestFragmentUDP_VeryLargePacket(t *testing.T) {
	// Test packet that would require > 255 fragments
	// MaxFragPayload is about 1191 bytes, so 255 * 1191 = ~303KB
	// A packet larger than this should fail
	data := make([]byte, 256*MaxFragPayload)

	var fragID uint16
	_, err := FragmentUDP(12345, data, &fragID, true)
	if !errors.Is(err, ErrPacketTooLarge) {
		t.Errorf("expected ErrPacketTooLarge, got %v", err)
	}
}

func BenchmarkFragmentUDP_SmallPacket(b *testing.B) {
	data := make([]byte, 500)
	var fragID uint16

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = FragmentUDP(12345, data, &fragID, true)
	}
}

func BenchmarkFragmentUDP_LargePacket(b *testing.B) {
	data := make([]byte, 5000)
	var fragID uint16

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = FragmentUDP(12345, data, &fragID, true)
	}
}

func BenchmarkFragmentAssembler_Reassemble(b *testing.B) {
	data := make([]byte, 5000)
	var fragID uint16
	datagrams, _ := FragmentUDP(12345, data, &fragID, true)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		assembler := NewFragmentAssembler()
		for _, dgram := range datagrams {
			parsed, _ := DecodeUDPDatagram(dgram)
			_, _ = assembler.AddFragment(12345, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
		}
	}
}

func TestCleanupExpiredFragmentGroups(t *testing.T) {
	now := time.Now()
	regular := &FragmentAssembler{fragments: make(map[fragmentKey]*fragmentGroup)}
	sharded := &fragmentShard{fragments: make(map[fragmentKey]*fragmentGroup)}

	tests := []struct {
		name   string
		lock   sync.Locker
		groups map[fragmentKey]*fragmentGroup
		pooled bool
	}{
		{name: "regular", lock: &regular.mu, groups: regular.fragments},
		{name: "sharded pooled", lock: &sharded.mu, groups: sharded.fragments, pooled: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expired := &fragmentGroup{
				total:     2,
				received:  1,
				data:      make([][]byte, 2),
				createdAt: now.Add(-FragmentTimeout - time.Second),
			}
			if tt.pooled {
				bufPtr := GetFragmentBuffer()
				copy((*bufPtr)[:4], "part")
				expired.data[0] = (*bufPtr)[:4]
				expired.buffers = []*[]byte{bufPtr}
			} else {
				expired.data[0] = []byte("part")
			}

			recent := &fragmentGroup{
				total:     2,
				received:  1,
				data:      [][]byte{[]byte("recent"), nil},
				createdAt: now,
			}
			expiredKey := fragmentKey{sessionID: 12345, fragID: 1}
			recentKey := fragmentKey{sessionID: 12345, fragID: 2}
			tt.groups[expiredKey] = expired
			tt.groups[recentKey] = recent

			tt.lock.Lock()
			cleanupExpiredFragmentGroups(tt.groups, now)
			tt.lock.Unlock()

			if _, exists := tt.groups[expiredKey]; exists {
				t.Error("expected expired fragment group to be deleted")
			}
			if got := tt.groups[recentKey]; got != recent {
				t.Fatal("expected recent fragment group to remain unchanged")
			}
			assertFragmentGroupReleased(t, expired)
		})
	}
}

// ============================================================================
// FragmentUDPPooled Tests
// ============================================================================

func TestFragmentUDPPooled_SmallPacket(t *testing.T) {
	// Small packet should not be fragmented
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i)
	}

	var fragIDCounter atomic.Uint32
	results, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Verify buffer is non-nil (pooled)
	if results[0].Buffer == nil {
		t.Error("expected Buffer to be non-nil for pooled result")
	}

	// Parse and verify
	parsed, err := DecodeUDPDatagram(results[0].Data)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if parsed.SessionID != 12345 {
		t.Errorf("expected session ID 12345, got %d", parsed.SessionID)
	}

	if parsed.IsFragmented {
		t.Error("small packet should not be fragmented")
	}

	if !bytes.Equal(parsed.Payload, data) {
		t.Error("payload mismatch")
	}
}

func TestFragmentUDPPooled_LargePacket(t *testing.T) {
	// Large packet should be fragmented
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragIDCounter atomic.Uint32
	results, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results)

	if len(results) < 2 {
		t.Fatalf("expected multiple results, got %d", len(results))
	}

	// Verify all buffers are non-nil (pooled) and collect their datagram views.
	datagrams := make([][]byte, len(results))
	for i, r := range results {
		if r.Buffer == nil {
			t.Errorf("result %d: expected Buffer to be non-nil", i)
		}
		datagrams[i] = r.Data
	}
	assertFragmentsReassemble(t, datagrams, data)
}

func TestFragmentUDPPooled_DisabledFragmentation(t *testing.T) {
	// Large packet with fragmentation disabled should return error
	data := make([]byte, 3000)

	var fragIDCounter atomic.Uint32
	_, err := FragmentUDPPooled(12345, data, &fragIDCounter, false)
	if !errors.Is(err, ErrFragmentationDisabled) {
		t.Errorf("expected ErrFragmentationDisabled, got %v", err)
	}
}

func TestFragmentUDPPooled_MaxSize(t *testing.T) {
	// Test packet at exactly max unfragmented size
	data := make([]byte, MaxUDPPayload)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragIDCounter atomic.Uint32
	results, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results)

	if len(results) != 1 {
		t.Fatalf("expected 1 result for max unfragmented size, got %d", len(results))
	}

	parsed, _ := DecodeUDPDatagram(results[0].Data)
	if parsed.IsFragmented {
		t.Error("packet at max unfragmented size should not be fragmented")
	}

	if !bytes.Equal(parsed.Payload, data) {
		t.Error("payload mismatch")
	}
}

func TestFragmentUDPPooled_JustOverMaxSize(t *testing.T) {
	// Test packet just over max unfragmented size
	data := make([]byte, MaxUDPPayload+1)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragIDCounter atomic.Uint32
	results, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results)

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Reassemble and verify
	assembler := NewFragmentAssembler()
	var result []byte

	for _, r := range results {
		parsed, _ := DecodeUDPDatagram(r.Data)
		result, _ = assembler.AddFragment(parsed.SessionID, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
	}

	if !bytes.Equal(result, data) {
		t.Error("reassembled data mismatch")
	}
}

func TestFragmentUDPPooled_VeryLargePacket(t *testing.T) {
	// Test packet that would require > 255 fragments
	data := make([]byte, 256*MaxFragPayload)

	var fragIDCounter atomic.Uint32
	_, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if !errors.Is(err, ErrPacketTooLarge) {
		t.Errorf("expected ErrPacketTooLarge, got %v", err)
	}
}

func TestFragmentUDPPooled_AtomicCounterIncrement(t *testing.T) {
	// Test that atomic fragment ID counter increments correctly
	data := make([]byte, 3000) // Large enough to require fragmentation

	var fragIDCounter atomic.Uint32

	// First call
	results1, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results1)

	// Get fragment ID from first result
	first, _ := DecodeUDPDatagram(results1[0].Data)

	// Second call
	results2, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results2)

	// Get fragment ID from second result
	second, _ := DecodeUDPDatagram(results2[0].Data)

	// Fragment IDs should be different and incrementing
	if first.FragmentID == second.FragmentID {
		t.Errorf("expected different fragment IDs, both got %d", first.FragmentID)
	}

	if second.FragmentID != first.FragmentID+1 {
		t.Errorf("expected fragID2 (%d) to be fragID1+1 (%d)", second.FragmentID, first.FragmentID+1)
	}
}

func TestFragmentUDPPooled_EmptyData(t *testing.T) {
	// Test with empty data
	var data []byte

	var fragIDCounter atomic.Uint32
	results, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results)

	if len(results) != 1 {
		t.Fatalf("expected 1 result for empty data, got %d", len(results))
	}

	// Verify buffer is non-nil (pooled)
	if results[0].Buffer == nil {
		t.Error("expected Buffer to be non-nil for pooled result")
	}

	// Parse and verify
	parsed, err := DecodeUDPDatagram(results[0].Data)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if parsed.SessionID != 12345 {
		t.Errorf("expected session ID 12345, got %d", parsed.SessionID)
	}

	if parsed.IsFragmented {
		t.Error("empty packet should not be fragmented")
	}

	if len(parsed.Payload) != 0 {
		t.Errorf("expected empty payload, got %d bytes", len(parsed.Payload))
	}
}

func TestFragmentUDPPooled_BuffersAreReusable(t *testing.T) {
	// Test that buffers can be released and reused
	data := make([]byte, 100)

	var fragIDCounter atomic.Uint32

	// First allocation
	results1, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Store the buffer pointer
	buf1 := results1[0].Buffer

	// Release the buffer
	ReleaseDatagramResults(results1)

	// Second allocation - may get the same buffer back from pool
	results2, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results2)

	// Verify we got a valid buffer (may or may not be the same one)
	if results2[0].Buffer == nil {
		t.Error("expected Buffer to be non-nil")
	}

	// Verify the first result's buffer was set to nil after release
	if results1[0].Buffer != nil {
		t.Error("expected released buffer to be nil")
	}

	// buf1 should still point to a valid buffer (the pool doesn't nil the pointer)
	if buf1 == nil {
		t.Error("original buffer pointer should not be nil")
	}
}

// Benchmark for FragmentUDPPooled
func BenchmarkFragmentUDPPooled_SmallPacket(b *testing.B) {
	data := make([]byte, 500)
	var fragIDCounter atomic.Uint32

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		results, _ := FragmentUDPPooled(12345, data, &fragIDCounter, true)
		ReleaseDatagramResults(results)
	}
}

func BenchmarkFragmentUDPPooled_LargePacket(b *testing.B) {
	data := make([]byte, 5000)
	var fragIDCounter atomic.Uint32

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		results, _ := FragmentUDPPooled(12345, data, &fragIDCounter, true)
		ReleaseDatagramResults(results)
	}
}

// ============================================================================
// ShardedFragmentAssembler Tests
// ============================================================================

func TestNewShardedFragmentAssembler_DefaultShardCount(t *testing.T) {
	// Test with zero shard count - should use default
	sfa := NewShardedFragmentAssembler(0)
	if sfa == nil {
		t.Fatal("expected non-nil ShardedFragmentAssembler")
	}

	if len(sfa.shards) != DefaultShardCount {
		t.Errorf("expected %d shards, got %d", DefaultShardCount, len(sfa.shards))
	}
}

func TestNewShardedFragmentAssembler_NegativeShardCount(t *testing.T) {
	// Test with negative shard count - should use default
	sfa := NewShardedFragmentAssembler(-5)
	if sfa == nil {
		t.Fatal("expected non-nil ShardedFragmentAssembler")
	}

	if len(sfa.shards) != DefaultShardCount {
		t.Errorf("expected shard count %d, got %d", DefaultShardCount, len(sfa.shards))
	}
}

func TestNewShardedFragmentAssembler_CustomShardCount(t *testing.T) {
	// Test with custom shard count
	customCount := 32
	sfa := NewShardedFragmentAssembler(customCount)
	if sfa == nil {
		t.Fatal("expected non-nil ShardedFragmentAssembler")
	}

	if len(sfa.shards) != customCount {
		t.Errorf("expected %d shards, got %d", customCount, len(sfa.shards))
	}
}

func TestNewShardedFragmentAssembler_LargeShardCounts(t *testing.T) {
	for _, shardCount := range []int{65536, 65537} {
		t.Run(strconv.Itoa(shardCount), func(t *testing.T) {
			sfa := NewShardedFragmentAssembler(shardCount)
			defer sfa.Close()
			if len(sfa.shards) != shardCount {
				t.Fatalf("shard count = %d, want %d", len(sfa.shards), shardCount)
			}

			for sessionID := range uint32(128) {
				key := fragmentKey{sessionID: sessionID, fragID: 1}
				index := sfa.shardIndex(key)
				want := int(maphash.Comparable(sfa.seed, key) % uint64(shardCount))
				if index != want || sfa.getShard(key) != &sfa.shards[want] {
					t.Fatalf("key=%+v produced shard %d, want %d", key, index, want)
				}
			}
		})
	}
}

func TestNewShardedFragmentAssembler_ShardsInitialized(t *testing.T) {
	// Test that all shards have initialized fragment maps
	sfa := NewShardedFragmentAssembler(8)

	for i := range 8 {
		if sfa.shards[i].fragments == nil {
			t.Errorf("shard %d: expected fragments map to be initialized", i)
		}
	}
}

func TestShardedFragmentAssembler_GetShard_Deterministic(t *testing.T) {
	// Test that getShard returns consistent results
	sfa := NewShardedFragmentAssembler(16)

	// Same fragment ID should always return the same shard
	key := fragmentKey{sessionID: 99, fragID: 12345}
	shard1 := sfa.getShard(key)
	shard2 := sfa.getShard(key)

	if shard1 != shard2 {
		t.Error("getShard should return the same shard for the same fragment ID")
	}
}

func TestShardedFragmentAssembler_GetShard_SingleShard(t *testing.T) {
	// Test with single shard - all fragment IDs should map to the same shard
	sfa := NewShardedFragmentAssembler(1)

	shard0 := sfa.getShard(fragmentKey{})
	shard1 := sfa.getShard(fragmentKey{sessionID: 1, fragID: 1})
	shard100 := sfa.getShard(fragmentKey{sessionID: 100, fragID: 100})
	shard65535 := sfa.getShard(fragmentKey{sessionID: ^uint32(0), fragID: ^uint16(0)})

	if shard0 != shard1 || shard1 != shard100 || shard100 != shard65535 {
		t.Error("with single shard, all fragment IDs should map to the same shard")
	}
}

// ============================================================================
// ShardedFragmentAssembler.AddFragment Tests
// ============================================================================

func TestShardedFragmentAssembler_AddFragment_InOrder(t *testing.T) {
	// Test receiving fragments in order
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragIDCounter atomic.Uint32
	results, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results)

	sfa := NewShardedFragmentAssembler(16)
	var result []byte

	for i, r := range results {
		parsed, err := DecodeUDPDatagram(r.Data)
		if err != nil {
			t.Fatalf("parse error on fragment %d: %v", i, err)
		}

		result, err = sfa.AddFragment(parsed.SessionID, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
		if err != nil {
			t.Fatalf("add fragment error: %v", err)
		}

		if i < len(results)-1 && result != nil {
			t.Errorf("fragment %d: expected nil result (more fragments needed)", i)
		}
	}

	if result == nil {
		t.Fatal("expected complete result after all fragments")
	}

	if !bytes.Equal(result, data) {
		t.Errorf("reassembled data mismatch: expected %d bytes, got %d bytes", len(data), len(result))
	}
}

func TestShardedFragmentAssembler_AddFragment_OutOfOrder(t *testing.T) {
	// Test receiving fragments out of order
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragIDCounter atomic.Uint32
	results, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results)

	sfa := NewShardedFragmentAssembler(16)
	var result []byte

	// Receive in reverse order
	for _, datagram := range slices.Backward(results) {
		parsed, _ := DecodeUDPDatagram(datagram.Data)
		result, err = sfa.AddFragment(12345, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
		if err != nil {
			t.Fatalf("add fragment error: %v", err)
		}
	}

	if result == nil {
		t.Fatal("expected complete result")
	}

	if !bytes.Equal(result, data) {
		t.Error("reassembled data mismatch")
	}
}

func TestShardedFragmentAssembler_AddFragment_DuplicateFragment(t *testing.T) {
	// Test receiving duplicate fragments
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragIDCounter atomic.Uint32
	results, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results)

	sfa := NewShardedFragmentAssembler(16)

	// Add first fragment twice
	parsed, _ := DecodeUDPDatagram(results[0].Data)
	assertDuplicateFragmentIgnored(t, sfa.AddFragment, 12345, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)

	// Add remaining fragments
	var result []byte
	for i := 1; i < len(results); i++ {
		parsed, _ := DecodeUDPDatagram(results[i].Data)
		result, _ = sfa.AddFragment(12345, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
	}

	if result == nil {
		t.Fatal("expected complete result")
	}

	if !bytes.Equal(result, data) {
		t.Error("reassembled data mismatch")
	}
}

func TestShardedFragmentAssembler_AddFragment_SameFragmentIDDifferentSessions(t *testing.T) {
	sfa := NewShardedFragmentAssembler(16)
	defer sfa.Close()
	assertSameFragmentIDDifferentSessions(t, sfa.AddFragment)
}

func TestShardedFragmentAssembler_AddFragment_InvalidFragmentIndex(t *testing.T) {
	sfa := NewShardedFragmentAssembler(16)

	// Try to add fragment with index >= total
	_, err := sfa.AddFragment(12345, 1, 5, 3, []byte("test"))
	if !errors.Is(err, ErrInvalidFragIndex) {
		t.Errorf("expected ErrInvalidFragIndex, got %v", err)
	}
}

func TestShardedFragmentAssembler_AddFragment_MissingFragment(t *testing.T) {
	// Test with missing fragment - should not complete
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var fragIDCounter atomic.Uint32
	results, err := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ReleaseDatagramResults(results)

	sfa := NewShardedFragmentAssembler(16)

	// Skip middle fragment
	for i, r := range results {
		if i == 1 {
			continue // Skip this fragment
		}
		parsed, _ := DecodeUDPDatagram(r.Data)
		result, _ := sfa.AddFragment(12345, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
		if result != nil {
			t.Error("should not complete with missing fragment")
		}
	}
}

func TestShardedFragmentAssembler_AddFragment_UsesPooledBuffers(t *testing.T) {
	// Test that AddFragment uses pooled buffers for fragment storage
	sfa := NewShardedFragmentAssembler(16)

	// Add a fragment
	payload := []byte("test payload data")
	_, err := sfa.AddFragment(12345, 1, 0, 2, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that the fragment group has tracked buffers
	key := fragmentKey{sessionID: 12345, fragID: 1}
	shard := sfa.getShard(key)
	shard.mu.Lock()
	group, exists := shard.fragments[key]
	if !exists {
		shard.mu.Unlock()
		t.Fatal("expected fragment group to exist")
	}

	if len(group.buffers) != 1 {
		shard.mu.Unlock()
		t.Errorf("expected 1 tracked buffer, got %d", len(group.buffers))
	}

	if group.buffers[0] == nil {
		shard.mu.Unlock()
		t.Error("expected tracked buffer to be non-nil")
	}
	shard.mu.Unlock()
}

func TestShardedFragmentAssembler_ChargesPooledBufferCapacity(t *testing.T) {
	shortened := make([]byte, 1, FragmentBufferSize)
	bufferCapacity := cap(shortened)
	if retainedBytes := fragmentBufferRetainedBytes(&shortened); retainedBytes != int64(bufferCapacity) {
		t.Fatalf("shortened buffer retained bytes = %d, want capacity %d", retainedBytes, bufferCapacity)
	}

	sfa := NewShardedFragmentAssembler(1)
	sfa.maxBytes = int64(bufferCapacity - 1)
	defer sfa.Close()
	if _, err := sfa.AddFragment(1, 1, 0, 2, []byte("x")); !errors.Is(err, ErrFragmentAssemblerFull) {
		t.Fatalf("capacity-sized pooled charge error = %v", err)
	}
	if groups, retainedBytes := sfa.retainedGroups.Load(), sfa.retainedBytes.Load(); groups != 0 || retainedBytes != 0 {
		t.Fatalf("rejected pooled fragment retained budget: groups=%d bytes=%d", groups, retainedBytes)
	}
}

func TestShardedFragmentAssembler_AddFragment_ReturnsBuffersOnCompletion(t *testing.T) {
	// Test that buffers are returned to pool when fragment group completes
	sfa := NewShardedFragmentAssembler(16)

	// Add all fragments to complete a group
	payload1 := []byte("first fragment")
	payload2 := []byte("second fragment")

	_, err := sfa.AddFragment(12345, 1, 0, 2, payload1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, err := sfa.AddFragment(12345, 1, 1, 2, payload2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result == nil {
		t.Fatal("expected complete result")
	}

	// Verify the fragment group was removed from the shard
	key := fragmentKey{sessionID: 12345, fragID: 1}
	shard := sfa.getShard(key)
	shard.mu.Lock()
	_, exists := shard.fragments[key]
	shard.mu.Unlock()

	if exists {
		t.Error("expected fragment group to be removed after completion")
	}

	// Verify the result contains the correct data
	expectedData := append(payload1, payload2...)
	if !bytes.Equal(result, expectedData) {
		t.Errorf("expected result %v, got %v", expectedData, result)
	}
}

func TestShardedFragmentAssembler_AddFragment_ShardIsolation(t *testing.T) {
	// Test that fragments in different shards don't interfere with each other
	sfa := NewShardedFragmentAssembler(16)

	// Add fragments with different fragment identities.
	_, err := sfa.AddFragment(12345, 0, 0, 2, []byte("frag0-part0"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = sfa.AddFragment(12345, 1, 0, 2, []byte("frag1-part0"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify both fragment groups exist in their respective shards
	key0 := fragmentKey{sessionID: 12345, fragID: 0}
	shard0 := sfa.getShard(key0)
	shard0.mu.Lock()
	_, exists0 := shard0.fragments[key0]
	shard0.mu.Unlock()

	key1 := fragmentKey{sessionID: 12345, fragID: 1}
	shard1 := sfa.getShard(key1)
	shard1.mu.Lock()
	_, exists1 := shard1.fragments[key1]
	shard1.mu.Unlock()

	if !exists0 {
		t.Error("expected fragment group 0 to exist in shard 0")
	}

	if !exists1 {
		t.Error("expected fragment group 1 to exist in shard 1")
	}

	// Complete fragment group 0
	result0, err := sfa.AddFragment(12345, 0, 1, 2, []byte("frag0-part1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result0 == nil {
		t.Fatal("expected complete result for fragment group 0")
	}

	// Verify fragment group 1 still exists
	shard1.mu.Lock()
	_, exists1 = shard1.fragments[key1]
	shard1.mu.Unlock()

	if !exists1 {
		t.Error("expected fragment group 1 to still exist after completing group 0")
	}
}

func TestShardedFragmentAssembler_AddFragment_RejectsSingleFragment(t *testing.T) {
	// Fragmented datagrams require at least two fragments.
	sfa := NewShardedFragmentAssembler(16)

	payload := []byte("single fragment data")
	result, err := sfa.AddFragment(12345, 1, 0, 1, payload)
	if !errors.Is(err, ErrInvalidFragTotal) {
		t.Fatalf("expected ErrInvalidFragTotal, got %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result, got %q", result)
	}
}

func TestShardedFragmentAssembler_AddFragment_LargePayload(t *testing.T) {
	// Test with payload larger than FragmentBufferSize
	sfa := NewShardedFragmentAssembler(16)

	// Create a payload larger than the default fragment buffer size
	largePayload := make([]byte, FragmentBufferSize+100)
	for i := range largePayload {
		largePayload[i] = byte(i % 256)
	}

	result, err := sfa.AddFragment(12345, 1, 0, 2, largePayload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected incomplete result, got %d bytes", len(result))
	}

	key := fragmentKey{sessionID: 12345, fragID: 1}
	shard := sfa.getShard(key)
	shard.mu.Lock()
	group := shard.fragments[key]
	if group == nil {
		shard.mu.Unlock()
		t.Fatal("expected fragment group")
	}
	if len(group.buffers) != 0 {
		shard.mu.Unlock()
		t.Fatalf("large fragment should be directly allocated, got %d pooled buffers", len(group.buffers))
	}
	if group.retainedBytes != int64(len(largePayload)) || sfa.retainedBytes.Load() != int64(len(largePayload)) {
		shard.mu.Unlock()
		t.Fatalf("large fragment retained bytes = group %d assembler %d, want %d", group.retainedBytes, sfa.retainedBytes.Load(), len(largePayload))
	}
	shard.mu.Unlock()

	tail := []byte("tail")
	result, err = sfa.AddFragment(12345, 1, 1, 2, tail)
	if err != nil {
		t.Fatalf("unexpected completion error: %v", err)
	}
	expected := append(append([]byte(nil), largePayload...), tail...)
	if !bytes.Equal(result, expected) {
		t.Error("reassembled data mismatch for large payload")
	}
	if groups, retainedBytes := sfa.retainedGroups.Load(), sfa.retainedBytes.Load(); groups != 0 || retainedBytes != 0 {
		t.Fatalf("completion retained budget: groups=%d bytes=%d", groups, retainedBytes)
	}
}

// Benchmark for ShardedFragmentAssembler.AddFragment
func BenchmarkShardedFragmentAssembler_AddFragment(b *testing.B) {
	data := make([]byte, 5000)
	var fragIDCounter atomic.Uint32
	results, _ := FragmentUDPPooled(12345, data, &fragIDCounter, true)
	defer ReleaseDatagramResults(results)

	// Parse fragments once
	type parsedFrag struct {
		sessionID uint32
		fragID    uint16
		index     uint8
		total     uint8
		payload   []byte
	}
	frags := make([]parsedFrag, len(results))
	for i, r := range results {
		parsed, _ := DecodeUDPDatagram(r.Data)
		frags[i] = parsedFrag{parsed.SessionID, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sfa := NewShardedFragmentAssembler(16)
		for _, f := range frags {
			_, _ = sfa.AddFragment(f.sessionID, f.fragID, f.index, f.total, f.payload)
		}
	}
}

// BenchmarkShardedFragmentAssembler_Reassemble benchmarks the sharded assembler's
// reassembly performance, comparable to BenchmarkFragmentAssembler_Reassemble.
func BenchmarkShardedFragmentAssembler_Reassemble(b *testing.B) {
	data := make([]byte, 5000)
	var fragID uint16
	datagrams, _ := FragmentUDP(12345, data, &fragID, true)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sfa := NewShardedFragmentAssembler(16)
		for _, dgram := range datagrams {
			parsed, _ := DecodeUDPDatagram(dgram)
			_, _ = sfa.AddFragment(12345, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload)
		}
	}
}

const concurrentBenchmarkFragmentSetCount = 16

type concurrentBenchmarkFragment struct {
	sessionID uint32
	fragID    uint16
	index     uint8
	total     uint8
	payload   []byte
}

func makeConcurrentBenchmarkFragmentSets(tb testing.TB) [][]concurrentBenchmarkFragment {
	tb.Helper()
	fragmentSets := make([][]concurrentBenchmarkFragment, concurrentBenchmarkFragmentSetCount)
	for setIdx := range concurrentBenchmarkFragmentSetCount {
		data := make([]byte, 5000)
		for i := range data {
			data[i] = byte((i + setIdx) % 256)
		}

		var fragIDCounter atomic.Uint32
		fragIDCounter.Store(uint32(setIdx * 1000))
		results, err := FragmentUDPPooled(uint32(12345+setIdx), data, &fragIDCounter, true)
		if err != nil {
			tb.Fatalf("fragment benchmark set %d: %v", setIdx, err)
		}

		func() {
			defer ReleaseDatagramResults(results)
			fragments := make([]concurrentBenchmarkFragment, len(results))
			for i, result := range results {
				parsed, err := DecodeUDPDatagram(result.Data)
				if err != nil {
					tb.Fatalf("parse benchmark set %d fragment %d: %v", setIdx, i, err)
				}
				payloadCopy := append([]byte(nil), parsed.Payload...)
				fragments[i] = concurrentBenchmarkFragment{parsed.SessionID, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, payloadCopy}
			}
			fragmentSets[setIdx] = fragments
		}()
	}
	return fragmentSets
}

// BenchmarkShardedFragmentAssembler_Concurrent benchmarks the sharded assembler's
// performance under concurrent access from multiple goroutines.
// This measures the effectiveness of sharded locking in reducing contention.
func BenchmarkShardedFragmentAssembler_Concurrent(b *testing.B) {
	fragmentSets := makeConcurrentBenchmarkFragmentSets(b)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		sfa := NewShardedFragmentAssembler(16)
		setIdx := 0
		for pb.Next() {
			frags := fragmentSets[setIdx%concurrentBenchmarkFragmentSetCount]
			for _, f := range frags {
				_, _ = sfa.AddFragment(f.sessionID, f.fragID, f.index, f.total, f.payload)
			}
			setIdx++
		}
	})
}

// BenchmarkFragmentAssembler_Concurrent benchmarks the original assembler's
// performance under concurrent access for comparison with the sharded version.
func BenchmarkFragmentAssembler_Concurrent(b *testing.B) {
	fragmentSets := makeConcurrentBenchmarkFragmentSets(b)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		assembler := NewFragmentAssembler()
		setIdx := 0
		for pb.Next() {
			frags := fragmentSets[setIdx%concurrentBenchmarkFragmentSetCount]
			for _, f := range frags {
				_, _ = assembler.AddFragment(f.sessionID, f.fragID, f.index, f.total, f.payload)
			}
			setIdx++
		}
	})
}

// ============================================================================
// Atomic Counter Benchmarks
// ============================================================================

// BenchmarkAtomicCounter_Concurrent benchmarks the atomic.Uint32 counter
// performance under concurrent access from multiple goroutines.
// This simulates the fragment ID counter usage pattern in FragmentUDPPooled.
func BenchmarkAtomicCounter_Concurrent(b *testing.B) {
	var counter atomic.Uint32

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			counter.Add(1)
		}
	})
}

// BenchmarkMutexCounter_Concurrent benchmarks a mutex-protected counter
// for comparison with the atomic counter. This represents the old approach
// using sync.Mutex for fragment ID counter protection.
func BenchmarkMutexCounter_Concurrent(b *testing.B) {
	var mu sync.Mutex
	var counter uint32

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.Lock()
			counter++
			mu.Unlock()
		}
	})
}

// BenchmarkAtomicCounter_FragmentIDPattern benchmarks the atomic counter
// in a pattern that more closely matches the actual FragmentUDPPooled usage:
// increment counter and use the value for fragment ID generation.
func BenchmarkAtomicCounter_FragmentIDPattern(b *testing.B) {
	var counter atomic.Uint32
	data := make([]byte, 3000) // Large enough to require fragmentation

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Simulate FragmentUDPPooled's atomic counter usage
			results, _ := FragmentUDPPooled(12345, data, &counter, true)
			ReleaseDatagramResults(results)
		}
	})
}

// BenchmarkMutexCounter_FragmentIDPattern benchmarks a mutex-protected counter
// in a pattern that matches the old FragmentUDP usage with mutex protection.
// This provides a direct comparison for the atomic counter optimization.
func BenchmarkMutexCounter_FragmentIDPattern(b *testing.B) {
	var mu sync.Mutex
	var fragID uint16
	data := make([]byte, 3000) // Large enough to require fragmentation

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Simulate old FragmentUDP usage with mutex protection
			mu.Lock()
			_, _ = FragmentUDP(12345, data, &fragID, true)
			mu.Unlock()
		}
	})
}
