package protocol

import (
	"bytes"
	"sync"
)

// Buffer size constants for common message types
const (
	SmallBufferSize  = 256         // For heartbeats, errors
	MediumBufferSize = 4096        // For typical messages
	LargeBufferSize  = 65536       // For large payloads
	MaxPooledBuffer  = 1024 * 1024 // 1MB - don't pool larger buffers
)

// bufferPool is a sync.Pool for reusing byte buffers to reduce allocations
var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// GetBuffer retrieves a buffer from the pool.
// The buffer is reset and ready for use.
func GetBuffer() *bytes.Buffer {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// PutBuffer returns a buffer to the pool.
// Buffers larger than MaxPooledBuffer are not pooled to prevent memory bloat.
func PutBuffer(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	// Don't pool oversized buffers to prevent memory bloat
	if buf.Cap() > MaxPooledBuffer {
		return
	}
	buf.Reset()
	bufferPool.Put(buf)
}

// GetBufferWithSize retrieves a buffer from the pool and grows it to the specified size hint.
// This helps reduce reallocations when the approximate size is known.
func GetBufferWithSize(sizeHint int) *bytes.Buffer {
	buf := GetBuffer()
	if sizeHint > 0 && buf.Cap() < sizeHint {
		buf.Grow(sizeHint)
	}
	return buf
}
