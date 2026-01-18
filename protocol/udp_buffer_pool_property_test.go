package protocol

import (
	"testing"

	"pgregory.net/rapid"
)

// Feature: udp-performance-optimization, Property 2: Datagram Buffer Size Invariant
// *For any* call to GetDatagramBuffer, the returned buffer SHALL have a length of
// exactly MaxDatagramSize (1200 bytes).
// **Validates: Requirements 1.4**

// TestDatagramBufferSizeInvariant_Property verifies that GetDatagramBuffer always
// returns buffers of exactly 1200 bytes (MaxDatagramSize).
func TestDatagramBufferSizeInvariant_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate a random number of buffer requests to simulate various usage patterns
		numRequests := rapid.IntRange(1, 10).Draw(t, "numRequests")

		for i := 0; i < numRequests; i++ {
			// Get a datagram buffer from the pool
			buf := GetDatagramBuffer()

			// Property: Buffer must not be nil
			if buf == nil {
				t.Fatal("GetDatagramBuffer returned nil")
			}

			// Property: Buffer must have exactly MaxDatagramSize (1200) bytes
			if len(*buf) != MaxDatagramSize {
				t.Errorf("GetDatagramBuffer returned buffer with length %d, expected %d (MaxDatagramSize)",
					len(*buf), MaxDatagramSize)
			}

			// Property: Buffer must have exactly DatagramBufferSize bytes (same as MaxDatagramSize)
			if len(*buf) != DatagramBufferSize {
				t.Errorf("GetDatagramBuffer returned buffer with length %d, expected %d (DatagramBufferSize)",
					len(*buf), DatagramBufferSize)
			}

			// Return buffer to pool for reuse
			PutDatagramBuffer(buf)
		}
	})
}

// TestDatagramBufferSizeInvariant_ReusedBuffers_Property verifies that buffers
// returned to the pool and retrieved again still maintain the size invariant.
func TestDatagramBufferSizeInvariant_ReusedBuffers_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Get a buffer, modify it (simulate usage), return it, get it again
		iterations := rapid.IntRange(1, 20).Draw(t, "iterations")

		for i := 0; i < iterations; i++ {
			buf := GetDatagramBuffer()

			// Verify size before any modification
			if len(*buf) != MaxDatagramSize {
				t.Errorf("Iteration %d: buffer length %d, expected %d",
					i, len(*buf), MaxDatagramSize)
			}

			// Simulate writing data to the buffer (common usage pattern)
			writeLen := rapid.IntRange(0, MaxDatagramSize).Draw(t, "writeLen")
			for j := 0; j < writeLen; j++ {
				(*buf)[j] = byte(j % 256)
			}

			// Return to pool
			PutDatagramBuffer(buf)

			// Get another buffer (may or may not be the same one)
			buf2 := GetDatagramBuffer()

			// Property: Size invariant must hold regardless of pool state
			if len(*buf2) != MaxDatagramSize {
				t.Errorf("Iteration %d (after reuse): buffer length %d, expected %d",
					i, len(*buf2), MaxDatagramSize)
			}

			PutDatagramBuffer(buf2)
		}
	})
}

// TestDatagramBufferSizeInvariant_ConcurrentAccess_Property verifies the size
// invariant holds under concurrent access patterns.
func TestDatagramBufferSizeInvariant_ConcurrentAccess_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Get multiple buffers concurrently (simulated by getting many at once)
		numBuffers := rapid.IntRange(1, 50).Draw(t, "numBuffers")
		buffers := make([]*[]byte, numBuffers)

		// Get all buffers
		for i := 0; i < numBuffers; i++ {
			buffers[i] = GetDatagramBuffer()

			// Property: Each buffer must have exactly MaxDatagramSize bytes
			if buffers[i] == nil {
				t.Fatalf("Buffer %d is nil", i)
			}
			if len(*buffers[i]) != MaxDatagramSize {
				t.Errorf("Buffer %d has length %d, expected %d",
					i, len(*buffers[i]), MaxDatagramSize)
			}
		}

		// Return all buffers
		for i := 0; i < numBuffers; i++ {
			PutDatagramBuffer(buffers[i])
		}
	})
}

// Feature: udp-performance-optimization, Property 3: Read Buffer Size Invariant
// *For any* call to GetReadBuffer, the returned buffer SHALL have a length of
// exactly 65535 bytes.
// **Validates: Requirements 2.4**

// TestReadBufferSizeInvariant_Property verifies that GetReadBuffer always
// returns buffers of exactly 65535 bytes (ReadBufferSize).
func TestReadBufferSizeInvariant_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate a random number of buffer requests to simulate various usage patterns
		numRequests := rapid.IntRange(1, 10).Draw(t, "numRequests")

		for i := 0; i < numRequests; i++ {
			// Get a read buffer from the pool
			buf := GetReadBuffer()

			// Property: Buffer must not be nil
			if buf == nil {
				t.Fatal("GetReadBuffer returned nil")
			}

			// Property: Buffer must have exactly ReadBufferSize (65535) bytes
			if len(*buf) != ReadBufferSize {
				t.Errorf("GetReadBuffer returned buffer with length %d, expected %d (ReadBufferSize)",
					len(*buf), ReadBufferSize)
			}

			// Property: Buffer must have exactly 65535 bytes (explicit check)
			if len(*buf) != 65535 {
				t.Errorf("GetReadBuffer returned buffer with length %d, expected 65535",
					len(*buf))
			}

			// Return buffer to pool for reuse
			PutReadBuffer(buf)
		}
	})
}

// TestReadBufferSizeInvariant_ReusedBuffers_Property verifies that buffers
// returned to the pool and retrieved again still maintain the size invariant.
func TestReadBufferSizeInvariant_ReusedBuffers_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Get a buffer, modify it (simulate usage), return it, get it again
		iterations := rapid.IntRange(1, 20).Draw(t, "iterations")

		for i := 0; i < iterations; i++ {
			buf := GetReadBuffer()

			// Verify size before any modification
			if len(*buf) != ReadBufferSize {
				t.Errorf("Iteration %d: buffer length %d, expected %d",
					i, len(*buf), ReadBufferSize)
			}

			// Simulate writing data to the buffer (common usage pattern for UDP reads)
			writeLen := rapid.IntRange(0, ReadBufferSize).Draw(t, "writeLen")
			for j := 0; j < writeLen; j++ {
				(*buf)[j] = byte(j % 256)
			}

			// Return to pool
			PutReadBuffer(buf)

			// Get another buffer (may or may not be the same one)
			buf2 := GetReadBuffer()

			// Property: Size invariant must hold regardless of pool state
			if len(*buf2) != ReadBufferSize {
				t.Errorf("Iteration %d (after reuse): buffer length %d, expected %d",
					i, len(*buf2), ReadBufferSize)
			}

			PutReadBuffer(buf2)
		}
	})
}

// TestReadBufferSizeInvariant_ConcurrentAccess_Property verifies the size
// invariant holds under concurrent access patterns.
func TestReadBufferSizeInvariant_ConcurrentAccess_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Get multiple buffers concurrently (simulated by getting many at once)
		numBuffers := rapid.IntRange(1, 50).Draw(t, "numBuffers")
		buffers := make([]*[]byte, numBuffers)

		// Get all buffers
		for i := 0; i < numBuffers; i++ {
			buffers[i] = GetReadBuffer()

			// Property: Each buffer must have exactly ReadBufferSize (65535) bytes
			if buffers[i] == nil {
				t.Fatalf("Buffer %d is nil", i)
			}
			if len(*buffers[i]) != ReadBufferSize {
				t.Errorf("Buffer %d has length %d, expected %d",
					i, len(*buffers[i]), ReadBufferSize)
			}
		}

		// Return all buffers
		for i := 0; i < numBuffers; i++ {
			PutReadBuffer(buffers[i])
		}
	})
}
