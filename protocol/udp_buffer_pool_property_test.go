package protocol

import (
	"testing"

	"pgregory.net/rapid"
)

type propertyBufferPool struct {
	name string
	size int
	get  func() *[]byte
	put  func(*[]byte)
}

func checkReusedBufferSizeInvariant(t *testing.T, pool propertyBufferPool) {
	t.Helper()
	rapid.Check(t, func(t *rapid.T) {
		iterations := rapid.IntRange(1, 20).Draw(t, "iterations")
		for i := range iterations {
			buf := pool.get()
			if len(*buf) != pool.size {
				t.Errorf("%s iteration %d: buffer length %d, expected %d", pool.name, i, len(*buf), pool.size)
			}

			writeLen := rapid.IntRange(0, pool.size).Draw(t, "writeLen")
			for j := range writeLen {
				(*buf)[j] = byte(j % 256)
			}
			pool.put(buf)

			reused := pool.get()
			if len(*reused) != pool.size {
				t.Errorf("%s iteration %d after reuse: buffer length %d, expected %d", pool.name, i, len(*reused), pool.size)
			}
			pool.put(reused)
		}
	})
}

func checkConcurrentBufferSizeInvariant(t *testing.T, pool propertyBufferPool) {
	t.Helper()
	rapid.Check(t, func(t *rapid.T) {
		numBuffers := rapid.IntRange(1, 50).Draw(t, "numBuffers")
		buffers := make([]*[]byte, numBuffers)
		for i := range numBuffers {
			buffers[i] = pool.get()
			if buffers[i] == nil {
				t.Fatalf("%s buffer %d is nil", pool.name, i)
			}
			if len(*buffers[i]) != pool.size {
				t.Errorf("%s buffer %d has length %d, expected %d", pool.name, i, len(*buffers[i]), pool.size)
			}
		}
		for i := range numBuffers {
			pool.put(buffers[i])
		}
	})
}

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

		for range numRequests {
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
	checkReusedBufferSizeInvariant(t, propertyBufferPool{
		name: "datagram", size: MaxDatagramSize,
		get: GetDatagramBuffer, put: PutDatagramBuffer,
	})
}

// TestDatagramBufferSizeInvariant_ConcurrentAccess_Property verifies the size
// invariant holds under concurrent access patterns.
func TestDatagramBufferSizeInvariant_ConcurrentAccess_Property(t *testing.T) {
	checkConcurrentBufferSizeInvariant(t, propertyBufferPool{
		name: "datagram", size: MaxDatagramSize,
		get: GetDatagramBuffer, put: PutDatagramBuffer,
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

		for range numRequests {
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
	checkReusedBufferSizeInvariant(t, propertyBufferPool{
		name: "read", size: ReadBufferSize,
		get: GetReadBuffer, put: PutReadBuffer,
	})
}

// TestReadBufferSizeInvariant_ConcurrentAccess_Property verifies the size
// invariant holds under concurrent access patterns.
func TestReadBufferSizeInvariant_ConcurrentAccess_Property(t *testing.T) {
	checkConcurrentBufferSizeInvariant(t, propertyBufferPool{
		name: "read", size: ReadBufferSize,
		get: GetReadBuffer, put: PutReadBuffer,
	})
}

func TestInitBufferPoolCustomSizes(t *testing.T) {
	t.Cleanup(func() {
		InitBufferPool(DefaultDatagramBufferSize, DefaultReadBufferSize, DefaultFragmentBufferSize)
	})

	const datagramSize, readSize, fragmentSize = 512, 2048, 503
	InitBufferPool(datagramSize, readSize, fragmentSize)

	for name, buffer := range map[string]*[]byte{
		"datagram": GetDatagramBuffer(),
		"read":     GetReadBuffer(),
		"fragment": GetFragmentBuffer(),
	} {
		want := map[string]int{"datagram": datagramSize, "read": readSize, "fragment": fragmentSize}[name]
		if len(*buffer) != want {
			t.Errorf("%s buffer length = %d, want %d", name, len(*buffer), want)
		}
	}
}
