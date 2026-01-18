package protocol

import "sync"

// UDP buffer size constants for different operation types
const (
	// DatagramBufferSize is the size for QUIC datagram buffers
	DatagramBufferSize = MaxDatagramSize // 1200 bytes

	// ReadBufferSize is the size for UDP read buffers
	ReadBufferSize = 65535

	// FragmentBufferSize is the default size for fragment storage
	FragmentBufferSize = MaxFragPayload // ~1191 bytes
)

// UDPBufferPool provides pooled buffers for UDP operations.
// It maintains three tiers of buffer pools optimized for different use cases:
// - Datagram pool: 1200-byte buffers for QUIC datagram operations
// - Read pool: 65535-byte buffers for UDP socket read operations
// - Fragment pool: ~1191-byte buffers for fragment storage
type UDPBufferPool struct {
	datagramPool sync.Pool // 1200 byte buffers
	readPool     sync.Pool // 65535 byte buffers
	fragmentPool sync.Pool // ~1191 byte buffers
}

// udpPool is the global UDP buffer pool instance
var udpPool = &UDPBufferPool{
	datagramPool: sync.Pool{
		New: func() interface{} {
			buf := make([]byte, DatagramBufferSize)
			return &buf
		},
	},
	readPool: sync.Pool{
		New: func() interface{} {
			buf := make([]byte, ReadBufferSize)
			return &buf
		},
	},
	fragmentPool: sync.Pool{
		New: func() interface{} {
			buf := make([]byte, FragmentBufferSize)
			return &buf
		},
	},
}

// GetDatagramBuffer returns a buffer for datagram operations.
// The returned buffer has a length of exactly DatagramBufferSize (1200 bytes).
// Callers must call PutDatagramBuffer when done to return the buffer to the pool.
func GetDatagramBuffer() *[]byte {
	return udpPool.datagramPool.Get().(*[]byte)
}

// PutDatagramBuffer returns a datagram buffer to the pool.
// If buf is nil or has incorrect size, it is silently discarded.
func PutDatagramBuffer(buf *[]byte) {
	if buf == nil || len(*buf) != DatagramBufferSize {
		return
	}
	udpPool.datagramPool.Put(buf)
}

// GetReadBuffer returns a buffer for UDP read operations.
// The returned buffer has a length of exactly ReadBufferSize (65535 bytes).
// Callers must call PutReadBuffer when done to return the buffer to the pool.
func GetReadBuffer() *[]byte {
	return udpPool.readPool.Get().(*[]byte)
}

// PutReadBuffer returns a read buffer to the pool.
// If buf is nil or has incorrect size, it is silently discarded.
func PutReadBuffer(buf *[]byte) {
	if buf == nil || len(*buf) != ReadBufferSize {
		return
	}
	udpPool.readPool.Put(buf)
}

// GetFragmentBuffer returns a buffer for fragment storage.
// The returned buffer has a length of exactly FragmentBufferSize (~1191 bytes).
// Callers must call PutFragmentBuffer when done to return the buffer to the pool.
func GetFragmentBuffer() *[]byte {
	return udpPool.fragmentPool.Get().(*[]byte)
}

// PutFragmentBuffer returns a fragment buffer to the pool.
// If buf is nil, it is silently ignored.
func PutFragmentBuffer(buf *[]byte) {
	if buf == nil {
		return
	}
	udpPool.fragmentPool.Put(buf)
}
