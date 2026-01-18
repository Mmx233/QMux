package protocol

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// UDP fragment header format:
// [4 bytes session ID][2 bytes fragment ID][1 byte flags][1 byte fragment index][1 byte total fragments][payload]
// Flags: 0x01 = more fragments follow
const (
	UDPHeaderSize     = 4                                   // Session ID only (for unfragmented packets)
	UDPFragHeaderSize = 9                                   // Full fragment header
	MaxDatagramSize   = 1200                                // Safe QUIC datagram payload size
	MaxUDPPayload     = MaxDatagramSize - UDPHeaderSize     // Max payload for unfragmented
	MaxFragPayload    = MaxDatagramSize - UDPFragHeaderSize // Max payload per fragment
	FragmentTimeout   = 5 * time.Second                     // Timeout for incomplete fragments

	// DefaultShardCount is the default number of shards for the fragment assembler
	DefaultShardCount = 16
)

const (
	FlagMoreFragments = 0x01
	FlagFragmented    = 0x80 // Indicates this is a fragmented packet
)

var (
	ErrPacketTooLarge        = errors.New("packet too large to fragment")
	ErrSessionIDMismatch     = errors.New("session ID mismatch")
	ErrInvalidFragIndex      = errors.New("invalid fragment index")
	ErrDatagramTooShort      = errors.New("datagram too short")
	ErrFragmentationDisabled = errors.New("fragmentation disabled, packet too large")
)

// DatagramResult holds a datagram and its buffer for later release.
// Data is a slice of the buffer containing the actual datagram content.
// Buffer is the underlying pooled buffer (nil if not pooled).
type DatagramResult struct {
	Data   []byte  // Slice of the buffer containing the datagram
	Buffer *[]byte // The underlying pooled buffer (nil if not pooled)
}

// ReleaseDatagramResults returns all buffers to the pool.
// This function should be called after all datagrams have been sent
// to return the pooled buffers for reuse.
func ReleaseDatagramResults(results []DatagramResult) {
	for i := range results {
		if results[i].Buffer != nil {
			PutDatagramBuffer(results[i].Buffer)
			results[i].Buffer = nil
		}
	}
}

// FragmentUDPPooled splits a UDP packet into fragments using pooled buffers.
// The caller MUST call ReleaseDatagramResults after sending all datagrams
// to return the buffers to the pool.
//
// For unfragmented packets (data <= MaxUDPPayload), returns a single DatagramResult
// with a simple 4-byte header containing only the session ID.
//
// For fragmented packets, returns multiple DatagramResults with 9-byte headers
// containing session ID, fragment ID, flags, fragment index, and total fragments.
func FragmentUDPPooled(sessionID uint32, data []byte, fragIDCounter *atomic.Uint32, enableFragmentation bool) ([]DatagramResult, error) {
	if len(data) <= MaxUDPPayload {
		// No fragmentation needed - use pooled buffer
		bufPtr := GetDatagramBuffer()
		buf := *bufPtr

		binary.BigEndian.PutUint32(buf[:4], sessionID)
		copy(buf[UDPHeaderSize:], data)

		return []DatagramResult{{
			Data:   buf[:UDPHeaderSize+len(data)],
			Buffer: bufPtr,
		}}, nil
	}

	if !enableFragmentation {
		return nil, ErrFragmentationDisabled
	}

	// Need fragmentation
	numFragments := (len(data) + MaxFragPayload - 1) / MaxFragPayload
	if numFragments > 255 {
		return nil, ErrPacketTooLarge
	}

	fragID := uint16(fragIDCounter.Add(1))
	results := make([]DatagramResult, numFragments)
	offset := 0

	for i := 0; i < numFragments; i++ {
		end := offset + MaxFragPayload
		if end > len(data) {
			end = len(data)
		}
		payloadLen := end - offset

		bufPtr := GetDatagramBuffer()
		buf := *bufPtr

		binary.BigEndian.PutUint32(buf[:4], sessionID)
		binary.BigEndian.PutUint16(buf[4:6], fragID)

		flags := byte(FlagFragmented)
		if i < numFragments-1 {
			flags |= FlagMoreFragments
		}
		buf[6] = flags
		buf[7] = byte(i)
		buf[8] = byte(numFragments)
		copy(buf[UDPFragHeaderSize:], data[offset:end])

		results[i] = DatagramResult{
			Data:   buf[:UDPFragHeaderSize+payloadLen],
			Buffer: bufPtr,
		}
		offset = end
	}

	return results, nil
}

// FragmentAssembler reassembles fragmented UDP packets
type FragmentAssembler struct {
	mu        sync.Mutex
	fragments map[uint16]*fragmentGroup // fragmentID -> group
}

type fragmentGroup struct {
	sessionID uint32
	total     uint8
	received  uint8
	data      [][]byte
	buffers   []*[]byte // Track pooled buffers for cleanup
	createdAt time.Time
}

// NewFragmentAssembler creates a new fragment assembler
func NewFragmentAssembler() *FragmentAssembler {
	fa := &FragmentAssembler{
		fragments: make(map[uint16]*fragmentGroup),
	}
	go fa.cleanupLoop()
	return fa
}

// cleanupLoop removes expired fragment groups
func (fa *FragmentAssembler) cleanupLoop() {
	ticker := time.NewTicker(FragmentTimeout)
	defer ticker.Stop()

	for range ticker.C {
		fa.mu.Lock()
		now := time.Now()
		for id, group := range fa.fragments {
			if now.Sub(group.createdAt) > FragmentTimeout {
				delete(fa.fragments, id)
			}
		}
		fa.mu.Unlock()
	}
}

// AddFragment adds a fragment and returns the complete packet if all fragments received
// Returns (nil, nil) if more fragments are needed
func (fa *FragmentAssembler) AddFragment(sessionID uint32, fragID uint16, index, total uint8, payload []byte) ([]byte, error) {
	if index >= total {
		return nil, ErrInvalidFragIndex
	}

	fa.mu.Lock()
	defer fa.mu.Unlock()

	group, exists := fa.fragments[fragID]
	if !exists {
		group = &fragmentGroup{
			sessionID: sessionID,
			total:     total,
			data:      make([][]byte, total),
			createdAt: time.Now(),
		}
		fa.fragments[fragID] = group
	}

	if group.sessionID != sessionID {
		return nil, ErrSessionIDMismatch
	}

	if group.data[index] == nil {
		group.data[index] = make([]byte, len(payload))
		copy(group.data[index], payload)
		group.received++
	}

	if group.received == group.total {
		// All fragments received, reassemble
		delete(fa.fragments, fragID)

		totalSize := 0
		for _, frag := range group.data {
			totalSize += len(frag)
		}

		result := make([]byte, 0, totalSize)
		for _, frag := range group.data {
			result = append(result, frag...)
		}
		return result, nil
	}

	return nil, nil // More fragments needed
}

// fragmentShard holds fragments for a subset of fragment IDs
type fragmentShard struct {
	mu        sync.Mutex
	fragments map[uint16]*fragmentGroup
}

// ShardedFragmentAssembler reassembles fragmented UDP packets with reduced lock contention
type ShardedFragmentAssembler struct {
	shards     []fragmentShard
	shardCount uint16
}

// NewShardedFragmentAssembler creates a new sharded fragment assembler
func NewShardedFragmentAssembler(shardCount int) *ShardedFragmentAssembler {
	if shardCount <= 0 {
		shardCount = DefaultShardCount
	}

	sfa := &ShardedFragmentAssembler{
		shards:     make([]fragmentShard, shardCount),
		shardCount: uint16(shardCount),
	}

	for i := range sfa.shards {
		sfa.shards[i].fragments = make(map[uint16]*fragmentGroup)
	}

	go sfa.cleanupLoop()
	return sfa
}

// getShard returns the shard for a given fragment ID
func (sfa *ShardedFragmentAssembler) getShard(fragID uint16) *fragmentShard {
	return &sfa.shards[fragID%sfa.shardCount]
}

// cleanupLoop removes expired fragment groups from all shards
func (sfa *ShardedFragmentAssembler) cleanupLoop() {
	ticker := time.NewTicker(FragmentTimeout)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		for i := range sfa.shards {
			shard := &sfa.shards[i]
			shard.mu.Lock()
			for id, group := range shard.fragments {
				if now.Sub(group.createdAt) > FragmentTimeout {
					// Return buffers to pool before deleting
					for _, bufPtr := range group.buffers {
						PutFragmentBuffer(bufPtr)
					}
					delete(shard.fragments, id)
				}
			}
			shard.mu.Unlock()
		}
	}
}

// AddFragment adds a fragment and returns the complete packet if all fragments received.
// It locks only the relevant shard for reduced contention.
// Uses pooled buffers for fragment storage and tracks them for cleanup.
// Returns (nil, nil) if more fragments are needed.
func (sfa *ShardedFragmentAssembler) AddFragment(sessionID uint32, fragID uint16, index, total uint8, payload []byte) ([]byte, error) {
	if index >= total {
		return nil, ErrInvalidFragIndex
	}

	shard := sfa.getShard(fragID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	group, exists := shard.fragments[fragID]
	if !exists {
		group = &fragmentGroup{
			sessionID: sessionID,
			total:     total,
			data:      make([][]byte, total),
			createdAt: time.Now(),
		}
		shard.fragments[fragID] = group
	}

	if group.sessionID != sessionID {
		return nil, ErrSessionIDMismatch
	}

	if group.data[index] == nil {
		// Use pooled buffer for fragment storage if payload fits,
		// otherwise allocate a new buffer for large payloads
		var buf []byte
		var bufPtr *[]byte
		if len(payload) <= FragmentBufferSize {
			bufPtr = GetFragmentBuffer()
			buf = (*bufPtr)[:len(payload)]
		} else {
			// Payload is larger than pool buffer, allocate directly
			buf = make([]byte, len(payload))
		}
		copy(buf, payload)
		group.data[index] = buf
		if bufPtr != nil {
			group.buffers = append(group.buffers, bufPtr)
		}
		group.received++
	}

	if group.received == group.total {
		// All fragments received, reassemble
		delete(shard.fragments, fragID)

		totalSize := 0
		for _, frag := range group.data {
			totalSize += len(frag)
		}

		result := make([]byte, 0, totalSize)
		for _, frag := range group.data {
			result = append(result, frag...)
		}

		// Return fragment buffers to pool
		for _, bufPtr := range group.buffers {
			PutFragmentBuffer(bufPtr)
		}

		return result, nil
	}

	return nil, nil
}

// FragmentUDP splits a UDP packet into fragments if needed
// Returns a slice of datagrams ready to send
// If enableFragmentation is false and packet is too large, returns error
func FragmentUDP(sessionID uint32, data []byte, fragIDCounter *uint16, enableFragmentation bool) ([][]byte, error) {
	if len(data) <= MaxUDPPayload {
		// No fragmentation needed - use simple header
		dgram := make([]byte, UDPHeaderSize+len(data))
		binary.BigEndian.PutUint32(dgram[:4], sessionID)
		copy(dgram[UDPHeaderSize:], data)
		return [][]byte{dgram}, nil
	}

	if !enableFragmentation {
		return nil, ErrFragmentationDisabled
	}

	// Need fragmentation
	numFragments := (len(data) + MaxFragPayload - 1) / MaxFragPayload
	if numFragments > 255 {
		return nil, ErrPacketTooLarge
	}

	*fragIDCounter++
	fragID := *fragIDCounter

	result := make([][]byte, numFragments)
	offset := 0

	for i := 0; i < numFragments; i++ {
		end := offset + MaxFragPayload
		if end > len(data) {
			end = len(data)
		}
		payload := data[offset:end]
		offset = end

		dgram := make([]byte, UDPFragHeaderSize+len(payload))
		binary.BigEndian.PutUint32(dgram[:4], sessionID)
		binary.BigEndian.PutUint16(dgram[4:6], fragID)

		flags := byte(FlagFragmented)
		if i < numFragments-1 {
			flags |= FlagMoreFragments
		}
		dgram[6] = flags
		dgram[7] = byte(i)
		dgram[8] = byte(numFragments)
		copy(dgram[UDPFragHeaderSize:], payload)

		result[i] = dgram
	}

	return result, nil
}

// ParseUDPDatagram parses a UDP datagram header
// Returns sessionID, isFragmented, fragID, fragIndex, fragTotal, payload
func ParseUDPDatagram(dgram []byte) (uint32, bool, uint16, uint8, uint8, []byte, error) {
	if len(dgram) < UDPHeaderSize {
		return 0, false, 0, 0, 0, nil, ErrDatagramTooShort
	}

	sessionID := binary.BigEndian.Uint32(dgram[:4])

	// Check if this is a fragmented packet by looking at the flags byte position
	// For fragmented packets, we have at least 9 bytes header
	if len(dgram) >= UDPFragHeaderSize {
		flags := dgram[6]
		if flags&FlagFragmented != 0 {
			fragID := binary.BigEndian.Uint16(dgram[4:6])
			fragIndex := dgram[7]
			fragTotal := dgram[8]
			payload := dgram[UDPFragHeaderSize:]
			return sessionID, true, fragID, fragIndex, fragTotal, payload, nil
		}
	}

	// Unfragmented packet
	payload := dgram[UDPHeaderSize:]
	return sessionID, false, 0, 0, 0, payload, nil
}
