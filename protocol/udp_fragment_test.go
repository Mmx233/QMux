package protocol

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
)

func TestFragmentUDPPooledRejectsOversizedPacket(t *testing.T) {
	data := make([]byte, 256*MaxFragPayload)
	var counter atomic.Uint32
	if _, err := FragmentUDPPooled(12345, data, &counter, true); !errors.Is(err, ErrPacketTooLarge) {
		t.Fatalf("expected ErrPacketTooLarge, got %v", err)
	}
}

func TestShardedFragmentAssemblerChargesPooledBufferCapacity(t *testing.T) {
	shortened := make([]byte, 1, FragmentBufferSize)
	bufferCapacity := cap(shortened)
	if retainedBytes := fragmentBufferRetainedBytes(&shortened); retainedBytes != int64(bufferCapacity) {
		t.Fatalf("shortened buffer retained bytes = %d, want capacity %d", retainedBytes, bufferCapacity)
	}

	assembler := NewShardedFragmentAssembler(1)
	assembler.maxBytes = int64(bufferCapacity - 1)
	defer assembler.Close()
	if _, err := assembler.AddFragment(1, 1, 0, 2, []byte("x")); !errors.Is(err, ErrFragmentAssemblerFull) {
		t.Fatalf("capacity-sized pooled charge error = %v", err)
	}
	if groups, retainedBytes := assembler.retainedGroups.Load(), assembler.retainedBytes.Load(); groups != 0 || retainedBytes != 0 {
		t.Fatalf("rejected pooled fragment retained budget: groups=%d bytes=%d", groups, retainedBytes)
	}
}

func TestShardedFragmentAssemblerLargePayload(t *testing.T) {
	assembler := NewShardedFragmentAssembler(16)
	defer assembler.Close()
	largePayload := make([]byte, FragmentBufferSize+100)
	for i := range largePayload {
		largePayload[i] = byte(i)
	}

	result, err := assembler.AddFragment(12345, 1, 0, 2, largePayload)
	if err != nil || result != nil {
		t.Fatalf("first fragment: result=%d bytes, err=%v", len(result), err)
	}
	tail := []byte("tail")
	result, err = assembler.AddFragment(12345, 1, 1, 2, tail)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), largePayload...), tail...)
	if !bytes.Equal(result, want) {
		t.Fatal("reassembled data mismatch")
	}
	if groups, retainedBytes := assembler.retainedGroups.Load(), assembler.retainedBytes.Load(); groups != 0 || retainedBytes != 0 {
		t.Fatalf("completion retained budget: groups=%d bytes=%d", groups, retainedBytes)
	}
}

func BenchmarkFragmentUDPPooled_SmallPacket(b *testing.B) {
	data := make([]byte, 500)
	var counter atomic.Uint32
	for b.Loop() {
		results, _ := FragmentUDPPooled(12345, data, &counter, true)
		ReleaseDatagramResults(results)
	}
}

func BenchmarkFragmentUDPPooled_LargePacket(b *testing.B) {
	data := make([]byte, 5000)
	var counter atomic.Uint32
	for b.Loop() {
		results, _ := FragmentUDPPooled(12345, data, &counter, true)
		ReleaseDatagramResults(results)
	}
}

type benchmarkFragment struct {
	sessionID uint32
	fragID    uint16
	index     uint8
	total     uint8
	payload   []byte
}

func makeBenchmarkFragmentSets(tb testing.TB, count int) [][]benchmarkFragment {
	tb.Helper()
	sets := make([][]benchmarkFragment, count)
	for setIndex := range count {
		data := make([]byte, 5000)
		for i := range data {
			data[i] = byte(i + setIndex)
		}

		var counter atomic.Uint32
		counter.Store(uint32(setIndex * 1000))
		results, err := FragmentUDPPooled(uint32(12345+setIndex), data, &counter, true)
		if err != nil {
			tb.Fatalf("fragment benchmark set %d: %v", setIndex, err)
		}
		fragments := make([]benchmarkFragment, len(results))
		for i, result := range results {
			parsed, err := DecodeUDPDatagram(result.Data)
			if err != nil {
				tb.Fatalf("parse benchmark set %d fragment %d: %v", setIndex, i, err)
			}
			fragments[i] = benchmarkFragment{
				sessionID: parsed.SessionID,
				fragID:    parsed.FragmentID,
				index:     parsed.FragmentIndex,
				total:     parsed.FragmentTotal,
				payload:   append([]byte(nil), parsed.Payload...),
			}
		}
		ReleaseDatagramResults(results)
		sets[setIndex] = fragments
	}
	return sets
}

func BenchmarkShardedFragmentAssembler_AddFragment(b *testing.B) {
	fragments := makeBenchmarkFragmentSets(b, 1)[0]
	assembler := NewShardedFragmentAssembler(DefaultShardCount)
	defer assembler.Close()

	b.ResetTimer()
	for b.Loop() {
		for _, fragment := range fragments {
			_, _ = assembler.AddFragment(fragment.sessionID, fragment.fragID, fragment.index, fragment.total, fragment.payload)
		}
	}
}

func BenchmarkShardedFragmentAssembler_Concurrent(b *testing.B) {
	const fragmentSetCount = 16
	sets := makeBenchmarkFragmentSets(b, fragmentSetCount)
	for _, tc := range []struct {
		name   string
		shards int
	}{
		{"16_shards", 16},
		{"64_shards", 64},
	} {
		b.Run(tc.name, func(b *testing.B) {
			assembler := NewShardedFragmentAssembler(tc.shards)
			defer assembler.Close()
			var sessionID atomic.Uint32

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				setIndex := 0
				for pb.Next() {
					id := sessionID.Add(1)
					for _, fragment := range sets[setIndex%fragmentSetCount] {
						_, _ = assembler.AddFragment(id, fragment.fragID, fragment.index, fragment.total, fragment.payload)
					}
					setIndex++
				}
			})
		})
	}
}
