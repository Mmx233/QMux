package protocol

import "sync"

const (
	// DatagramBufferSize is the size of QUIC datagram buffers.
	DatagramBufferSize = MaxDatagramSize
	// ReadBufferSize is the size of UDP socket read buffers.
	ReadBufferSize = 65535
	// FragmentBufferSize is the size of fragment storage buffers.
	FragmentBufferSize = MaxFragPayload
)

// UDPBufferPool provides pooled buffers for UDP operations.
// It maintains three tiers of buffer pools optimized for different use cases:
// - Datagram pool: buffers for QUIC datagram operations
// - Read pool: buffers for UDP socket read operations
// - Fragment pool: buffers for fragment storage
type UDPBufferPool struct {
	datagramPool sync.Pool
	readPool     sync.Pool
	fragmentPool sync.Pool
}

var udpPool = UDPBufferPool{
	datagramPool: sync.Pool{
		New: func() any {
			buf := make([]byte, DatagramBufferSize)
			return &buf
		},
	},
	readPool: sync.Pool{
		New: func() any {
			buf := make([]byte, ReadBufferSize)
			return &buf
		},
	},
	fragmentPool: sync.Pool{
		New: func() any {
			buf := make([]byte, FragmentBufferSize)
			return &buf
		},
	},
}

// GetDatagramBuffer returns a buffer for datagram operations.
// The returned buffer has length and capacity exactly DatagramBufferSize.
// Callers must call PutDatagramBuffer when done to return the buffer to the pool.
func GetDatagramBuffer() *[]byte {
	return udpPool.datagramPool.Get().(*[]byte)
}

// PutDatagramBuffer returns a datagram buffer to the pool.
// If buf is nil or its length or capacity is not DatagramBufferSize, it is discarded.
func PutDatagramBuffer(buf *[]byte) {
	if !datagramBufferPoolable(buf) {
		return
	}
	udpPool.datagramPool.Put(buf)
}

func datagramBufferPoolable(buf *[]byte) bool {
	return buf != nil && len(*buf) == DatagramBufferSize && cap(*buf) == DatagramBufferSize
}

// GetReadBuffer returns a buffer for UDP read operations.
// The returned buffer has a length of exactly ReadBufferSize.
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
// The returned buffer has a length of exactly FragmentBufferSize.
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
