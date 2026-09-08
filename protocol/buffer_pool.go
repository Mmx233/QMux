package protocol

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

// Buffer size constants for common message types
const (
	SmallBufferSize       = 256   // For heartbeats, errors
	MediumBufferSize      = 4096  // For typical messages
	LargeBufferSize       = 65536 // For large payloads
	DefaultCopyBufferSize = 128 * 1024
	MaxPooledBuffer       = 1024 * 1024 // 1MB - don't pool larger buffers
)

// bufferPool is a sync.Pool for reusing byte buffers to reduce allocations
var bufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// CopyBufferPool reuses fixed-size TCP relay buffers.
type CopyBufferPool struct {
	pool sync.Pool
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

// NewCopyBufferPool creates an immutable fixed-size copy buffer pool.
func NewCopyBufferPool(size int) *CopyBufferPool {
	if size == 0 {
		size = DefaultCopyBufferSize
	}
	p := &CopyBufferPool{}
	p.pool.New = func() any {
		buf := make([]byte, size)
		return &buf
	}
	return p
}

type readerOnly struct{ io.Reader }
type writerOnly struct{ io.Writer }

// CopyBuffered uses WriterTo or ReaderFrom when available, unless forcePooledBuffer requires the pooled buffer.
// Returns the number of bytes copied and any error encountered.
func (p *CopyBufferPool) CopyBuffered(dst io.Writer, src io.Reader, forcePooledBuffer bool) (int64, error) {
	if !forcePooledBuffer {
		if wt, ok := src.(io.WriterTo); ok {
			return wt.WriteTo(dst)
		}
		if rf, ok := dst.(io.ReaderFrom); ok {
			return rf.ReadFrom(src)
		}
	} else {
		dst = writerOnly{dst}
		src = readerOnly{src}
	}
	bufPtr := p.pool.Get().(*[]byte)
	defer p.pool.Put(bufPtr)
	return io.CopyBuffer(dst, src, *bufPtr)
}

// RelayLifecycle owns the goroutines performing a bidirectional relay.
type RelayLifecycle struct {
	wg      sync.WaitGroup
	results [2]error
}

// StartRelay copies a to b and b to a, then runs each direction's callback.
func (p *CopyBufferPool) StartRelay(a, b io.ReadWriter, onAToBComplete, onBToAComplete func(error) error) *RelayLifecycle {
	relay := &RelayLifecycle{}
	relay.wg.Add(2)

	go relay.copy(p, 0, b, a, onAToBComplete)
	go relay.copy(p, 1, a, b, onBToAComplete)

	return relay
}

func (r *RelayLifecycle) copy(pool *CopyBufferPool, index int, dst io.Writer, src io.Reader, onComplete func(error) error) {
	defer r.wg.Done()
	_, copyErr := pool.CopyBuffered(dst, src, true)
	var callbackErr error
	if onComplete != nil {
		callbackErr = onComplete(copyErr)
	}
	r.results[index] = errors.Join(copyErr, callbackErr)
}

// Wait blocks until both copy goroutines and their completion callbacks exit.
func (r *RelayLifecycle) Wait() error {
	r.wg.Wait()
	return errors.Join(r.results[:]...)
}
