package protocol

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// UDP wire v2 datagram formats:
//
//   - normal:   [0x20][4 bytes session ID][payload]
//   - fragment: [0x21][4 bytes session ID][2 bytes fragment ID][1 byte fragment index][1 byte total fragments][payload]
//
// All multi-byte integers use big-endian byte order.
const (
	UDPDatagramTypeNormal   = 0x20
	UDPDatagramTypeFragment = 0x21

	UDPHeaderSize     = 5                                   // Type and session ID
	UDPFragHeaderSize = 9                                   // Full fragment header
	MaxDatagramSize   = 1200                                // Safe QUIC datagram payload size
	MaxUDPPayload     = MaxDatagramSize - UDPHeaderSize     // Max payload for unfragmented
	MaxFragPayload    = MaxDatagramSize - UDPFragHeaderSize // Max payload per fragment
	FragmentTimeout   = 5 * time.Second                     // Timeout for incomplete fragments

	// DefaultShardCount is the default number of shards for the fragment assembler
	DefaultShardCount = 16
)

var (
	ErrPacketTooLarge        = errors.New("packet too large to fragment")
	ErrSessionIDMismatch     = errors.New("session ID mismatch")
	ErrInvalidFragIndex      = errors.New("invalid fragment index")
	ErrDatagramTooShort      = errors.New("datagram too short")
	ErrDatagramTooLarge      = errors.New("datagram exceeds maximum size")
	ErrUnknownDatagramType   = errors.New("unknown UDP datagram type")
	ErrInvalidFragTotal      = errors.New("invalid fragment total")
	ErrEmptyFragmentPayload  = errors.New("empty fragment payload")
	ErrFragmentAssemblerNil  = errors.New("fragment assembler is required")
	ErrFragmentationDisabled = errors.New("fragmentation disabled, packet too large")
)

// UDPDatagram is a validated UDP wire v2 datagram.
// Payload aliases the input passed to DecodeUDPDatagram.
type UDPDatagram struct {
	Type          byte
	SessionID     uint32
	IsFragmented  bool
	FragmentID    uint16
	FragmentIndex uint8
	FragmentTotal uint8
	Payload       []byte
}

// UDPFragmentAssembler is implemented by the regular and sharded fragment assemblers.
type UDPFragmentAssembler interface {
	AddFragment(sessionID uint32, fragID uint16, index, total uint8, payload []byte) ([]byte, error)
}

func writeUDPHeader(dst []byte, sessionID uint32) {
	dst[0] = UDPDatagramTypeNormal
	binary.BigEndian.PutUint32(dst[1:UDPHeaderSize], sessionID)
}

func writeUDPFragmentHeader(dst []byte, sessionID uint32, fragID uint16, index, total uint8) {
	dst[0] = UDPDatagramTypeFragment
	binary.BigEndian.PutUint32(dst[1:5], sessionID)
	binary.BigEndian.PutUint16(dst[5:7], fragID)
	dst[7] = index
	dst[8] = total
}

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
// with a 5-byte header containing the datagram type and session ID.
//
// For fragmented packets, returns multiple DatagramResults with 9-byte headers
// containing type, session ID, fragment ID, fragment index, and total fragments.
func FragmentUDPPooled(sessionID uint32, data []byte, fragIDCounter *atomic.Uint32, enableFragmentation bool) ([]DatagramResult, error) {
	if len(data) <= MaxUDPPayload {
		// No fragmentation needed - use pooled buffer
		bufPtr := GetDatagramBuffer()
		buf := *bufPtr

		writeUDPHeader(buf, sessionID)
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

	for i := range numFragments {
		end := min(offset+MaxFragPayload, len(data))
		payloadLen := end - offset

		bufPtr := GetDatagramBuffer()
		buf := *bufPtr

		writeUDPFragmentHeader(buf, sessionID, fragID, byte(i), byte(numFragments))
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
		writeUDPHeader(dgram, sessionID)
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

	for i := range numFragments {
		end := min(offset+MaxFragPayload, len(data))
		payload := data[offset:end]
		offset = end

		dgram := make([]byte, UDPFragHeaderSize+len(payload))
		writeUDPFragmentHeader(dgram, sessionID, fragID, byte(i), byte(numFragments))
		copy(dgram[UDPFragHeaderSize:], payload)

		result[i] = dgram
	}

	return result, nil
}

// DecodeUDPDatagram strictly parses and validates a UDP wire v2 datagram.
// Legacy datagrams and unknown wire versions or packet types are rejected.
func DecodeUDPDatagram(dgram []byte) (UDPDatagram, error) {
	if len(dgram) < UDPHeaderSize {
		return UDPDatagram{}, ErrDatagramTooShort
	}
	if len(dgram) > MaxDatagramSize {
		return UDPDatagram{}, ErrDatagramTooLarge
	}

	switch dgram[0] {
	case UDPDatagramTypeNormal:
		return UDPDatagram{
			Type:      UDPDatagramTypeNormal,
			SessionID: binary.BigEndian.Uint32(dgram[1:5]),
			Payload:   dgram[UDPHeaderSize:],
		}, nil

	case UDPDatagramTypeFragment:
		if len(dgram) < UDPFragHeaderSize {
			return UDPDatagram{}, ErrDatagramTooShort
		}
		if len(dgram) == UDPFragHeaderSize {
			return UDPDatagram{}, ErrEmptyFragmentPayload
		}

		total := dgram[8]
		if total < 2 {
			return UDPDatagram{}, ErrInvalidFragTotal
		}
		index := dgram[7]
		if index >= total {
			return UDPDatagram{}, ErrInvalidFragIndex
		}

		return UDPDatagram{
			Type:          UDPDatagramTypeFragment,
			SessionID:     binary.BigEndian.Uint32(dgram[1:5]),
			IsFragmented:  true,
			FragmentID:    binary.BigEndian.Uint16(dgram[5:7]),
			FragmentIndex: index,
			FragmentTotal: total,
			Payload:       dgram[UDPFragHeaderSize:],
		}, nil

	default:
		return UDPDatagram{}, ErrUnknownDatagramType
	}
}

// ParseUDPDatagram is the compatibility adapter for the original tuple API.
// New code should use DecodeUDPDatagram so fields cannot be confused at call sites.
func ParseUDPDatagram(dgram []byte) (uint32, bool, uint16, uint8, uint8, []byte, error) {
	parsed, err := DecodeUDPDatagram(dgram)
	if err != nil {
		return 0, false, 0, 0, 0, nil, err
	}
	return parsed.SessionID, parsed.IsFragmented, parsed.FragmentID, parsed.FragmentIndex, parsed.FragmentTotal, parsed.Payload, nil
}

// DecodeAndAssembleUDPDatagram validates a UDP wire v2 datagram and, when needed,
// adds it to assembler. complete distinguishes an incomplete fragmented packet
// from a valid normal datagram with an empty payload.
func DecodeAndAssembleUDPDatagram(dgram []byte, assembler UDPFragmentAssembler) (sessionID uint32, payload []byte, complete bool, err error) {
	parsed, err := DecodeUDPDatagram(dgram)
	if err != nil {
		return 0, nil, false, err
	}
	if !parsed.IsFragmented {
		return parsed.SessionID, parsed.Payload, true, nil
	}
	if assembler == nil {
		return 0, nil, false, ErrFragmentAssemblerNil
	}

	payload, err = assembler.AddFragment(
		parsed.SessionID,
		parsed.FragmentID,
		parsed.FragmentIndex,
		parsed.FragmentTotal,
		parsed.Payload,
	)
	if err != nil {
		return 0, nil, false, err
	}
	return parsed.SessionID, payload, payload != nil, nil
}
