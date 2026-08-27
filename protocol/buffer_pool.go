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
	New: func() any {
		return new(bytes.Buffer)
	},
}

// copyBufferPool is a sync.Pool for reusing copy buffers
var copyBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, CopyBufferSize)
		return &buf
	},
}

// GetBuffer retrieves a buffer from the pool.
// The buffer is reset and ready for use.
func GetBuffer() *bytes.Buffer {
	return GetBufferWithSize(0)
}

// GetBufferWithSize retrieves a buffer from the pool and grows it to the specified size hint.
// This helps reduce reallocations when the approximate size is known.
func GetBufferWithSize(sizeHint int) *bytes.Buffer {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	if sizeHint > 0 && buf.Cap() < sizeHint {
		buf.Grow(sizeHint)
	}
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

// RelayLifecycle owns the goroutines performing a bidirectional relay.
// WaitFirst preserves Relay's first-completion behavior, while Wait joins both
// copy goroutines after the caller has closed or cancelled the relay endpoints.
type RelayLifecycle struct {
	results chan error
	wg      sync.WaitGroup

	firstOnce sync.Once
	firstErr  error
}

// StartRelay starts bidirectional copy between two io.ReadWriters.
func StartRelay(a, b io.ReadWriter) *RelayLifecycle {
	relay := &RelayLifecycle{
		results: make(chan error, 2),
	}
	relay.wg.Add(2)

	go relay.copy(a, b)
	go relay.copy(b, a)

	return relay
}

func (r *RelayLifecycle) copy(dst io.Writer, src io.Reader) {
	defer r.wg.Done()
	_, err := CopyBuffered(dst, src)
	r.results <- err
}

// WaitFirst waits for the first copy direction to finish and returns its error.
// Repeated and concurrent calls return the same first result.
func (r *RelayLifecycle) WaitFirst() error {
	r.firstOnce.Do(func() {
		r.firstErr = <-r.results
	})
	return r.firstErr
}

// Wait blocks until both copy goroutines have exited and returns the first
// completion result, matching WaitFirst and Relay.
func (r *RelayLifecycle) Wait() error {
	r.wg.Wait()
	return r.WaitFirst()
}

// Relay performs bidirectional copy between two io.ReadWriter.
// It uses optimized buffers and returns when either direction closes.
func Relay(a, b io.ReadWriter) error {
	return StartRelay(a, b).WaitFirst()
}
