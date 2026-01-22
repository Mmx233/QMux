package protocol

import "sync"

// Default UDP buffer size constants
const (
	// DefaultDatagramBufferSize is the default size for QUIC datagram buffers
	DefaultDatagramBufferSize = MaxDatagramSize // 1200 bytes

	// DefaultReadBufferSize is the default size for UDP read buffers
	DefaultReadBufferSize = 65535

	// DefaultFragmentBufferSize is the default size for fragment storage
	// Calculated as: DatagramSize - FragmentHeaderSize (9 bytes)
	DefaultFragmentBufferSize = DefaultDatagramBufferSize - UDPFragHeaderSize
)

// Current buffer sizes (can be configured via InitBufferPool)
var (
	DatagramBufferSize = DefaultDatagramBufferSize
	ReadBufferSize     = DefaultReadBufferSize
	FragmentBufferSize = DefaultFragmentBufferSize
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

// udpPool is the global UDP buffer pool instance
var udpPool *UDPBufferPool

func init() {
	// Initialize with default sizes
	initPool(DefaultDatagramBufferSize, DefaultReadBufferSize, DefaultFragmentBufferSize)
}

// initPool initializes the buffer pool with specified sizes
func initPool(datagramSize, readSize, fragmentSize int) {
	DatagramBufferSize = datagramSize
	ReadBufferSize = readSize
	FragmentBufferSize = fragmentSize

	udpPool = &UDPBufferPool{
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
}

// InitBufferPool initializes the buffer pool with custom sizes.
// This should be called once at startup before any buffer operations.
// If any size is <= 0, the default value will be used.
func InitBufferPool(datagramSize, readSize, fragmentSize int) {
	if datagramSize <= 0 {
		datagramSize = DefaultDatagramBufferSize
	}
	if readSize <= 0 {
		readSize = DefaultReadBufferSize
	}
	if fragmentSize <= 0 {
		fragmentSize = DefaultFragmentBufferSize
	}
	initPool(datagramSize, readSize, fragmentSize)
}

// GetDatagramBuffer returns a buffer for datagram operations.
// The returned buffer has a length of exactly DatagramBufferSize.
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
