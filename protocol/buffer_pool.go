package protocol

import (
	"bytes"
	"io"
	"sync"
)

// Buffer size constants for common message types
const (
	SmallBufferSize  = 256         // For heartbeats, errors
	MediumBufferSize = 4096        // For typical messages
	LargeBufferSize  = 65536       // For large payloads
	CopyBufferSize   = 512 * 1024  // 512KB for data copy operations
	MaxPooledBuffer  = 1024 * 1024 // 1MB - don't pool larger buffers
)

// bufferPool is a sync.Pool for reusing byte buffers to reduce allocations
var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// copyBufferPool is a sync.Pool for reusing copy buffers
var copyBufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, CopyBufferSize)
		return &buf
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

// GetCopyBuffer retrieves a copy buffer from the pool.
func GetCopyBuffer() *[]byte {
	return copyBufferPool.Get().(*[]byte)
}

// PutCopyBuffer returns a copy buffer to the pool.
func PutCopyBuffer(buf *[]byte) {
	if buf == nil {
		return
	}
	copyBufferPool.Put(buf)
}

// CopyBuffered copies from src to dst using a pooled 512KB buffer for better throughput.
// Returns the number of bytes copied and any error encountered.
func CopyBuffered(dst io.Writer, src io.Reader) (int64, error) {
	bufPtr := GetCopyBuffer()
	defer PutCopyBuffer(bufPtr)
	return io.CopyBuffer(dst, src, *bufPtr)
}

// Relay performs bidirectional copy between two io.ReadWriter.
// It uses optimized buffers and returns when either direction closes.
func Relay(a, b io.ReadWriter) error {
	errCh := make(chan error, 2)

	go func() {
		bufPtr := GetCopyBuffer()
		defer PutCopyBuffer(bufPtr)
		_, err := io.CopyBuffer(a, b, *bufPtr)
		errCh <- err
	}()

	go func() {
		bufPtr := GetCopyBuffer()
		defer PutCopyBuffer(bufPtr)
		_, err := io.CopyBuffer(b, a, *bufPtr)
		errCh <- err
	}()

	// Wait for first error/completion
	err := <-errCh
	return err
}
