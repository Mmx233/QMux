package protocol

import (
	"encoding/binary"
	"errors"
	"hash/maphash"
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

	maxRetainedFragmentGroups = 4096
	maxRetainedFragmentBytes  = 64 << 20
)

var (
	ErrPacketTooLarge          = errors.New("packet too large to fragment")
	ErrInvalidFragIndex        = errors.New("invalid fragment index")
	ErrFragmentTotalMismatch   = errors.New("fragment total mismatch")
	ErrDatagramTooShort        = errors.New("datagram too short")
	ErrDatagramTooLarge        = errors.New("datagram exceeds maximum size")
	ErrUnknownDatagramType     = errors.New("unknown UDP datagram type")
	ErrInvalidFragTotal        = errors.New("invalid fragment total")
	ErrEmptyFragmentPayload    = errors.New("empty fragment payload")
	ErrFragmentAssemblerNil    = errors.New("fragment assembler is required")
	ErrFragmentAssemblerClosed = errors.New("fragment assembler is closed")
	ErrFragmentAssemblerFull   = errors.New("fragment assembler capacity exceeded")
	ErrFragmentationDisabled   = errors.New("fragmentation disabled, packet too large")
)

// ErrSessionIDMismatch is retained for source compatibility.
//
// Deprecated: fragment groups are keyed by session ID; this error is retained for compatibility.
//
//goland:noinspection GoUnusedGlobalVariable
var ErrSessionIDMismatch = errors.New("session ID mismatch")

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

// FragmentSnapshot is a value-only view of retained fragment assembly state.
type FragmentSnapshot struct {
	RetainedGroups       int64
	RetainedBackingBytes int64
	GroupCapacityDrops   uint64
	ByteCapacityDrops    uint64
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
	mu            sync.Mutex
	fragments     map[fragmentKey]*fragmentGroup
	retainedBytes int64
	maxGroups     int
	maxBytes      int64
	closed        bool
	stopCh        chan struct{}
	doneCh        chan struct{}
	closeOnce     sync.Once
}

type fragmentKey struct {
	sessionID uint32
	fragID    uint16
}

type fragmentGroup struct {
	total         uint8
	received      uint8
	data          [][]byte
	buffers       []*[]byte // Track pooled buffers for cleanup
	createdAt     time.Time
	retainedBytes int64
}

func validateFragmentInput(index, total uint8) error {
	if total < 2 {
		return ErrInvalidFragTotal
	}
	if index >= total {
		return ErrInvalidFragIndex
	}
	return nil
}

func fragmentGroupLimit(limit int) int {
	if limit > 0 && limit < maxRetainedFragmentGroups {
		return limit
	}
	return maxRetainedFragmentGroups
}

func fragmentByteLimit(limit int64) int64 {
	if limit > 0 && limit < maxRetainedFragmentBytes {
		return limit
	}
	return maxRetainedFragmentBytes
}

func validateFragmentGroup(group *fragmentGroup, index, total uint8) error {
	if group.total != total {
		return ErrFragmentTotalMismatch
	}
	if int(index) >= len(group.data) {
		return ErrInvalidFragIndex
	}
	return nil
}

func joinFragmentGroup(group *fragmentGroup) []byte {
	totalSize := 0
	for _, fragment := range group.data {
		totalSize += len(fragment)
	}

	result := make([]byte, 0, totalSize)
	for _, fragment := range group.data {
		result = append(result, fragment...)
	}
	return result
}

func releaseFragmentGroup(group *fragmentGroup) int64 {
	for i, bufPtr := range group.buffers {
		PutFragmentBuffer(bufPtr)
		group.buffers[i] = nil
	}
	group.buffers = nil

	for i := range group.data {
		group.data[i] = nil
	}
	group.data = nil
	group.received = 0
	releasedBytes := group.retainedBytes
	group.retainedBytes = 0
	return releasedBytes
}

// cleanupExpiredFragmentGroups requires the caller to hold the lock protecting groups.
func cleanupExpiredFragmentGroups(groups map[fragmentKey]*fragmentGroup, now time.Time) (int64, int64) {
	var releasedGroups, releasedBytes int64
	for key, group := range groups {
		if now.Sub(group.createdAt) > FragmentTimeout {
			releasedBytes += releaseFragmentGroup(group)
			delete(groups, key)
			releasedGroups++
		}
	}
	return releasedGroups, releasedBytes
}

func releaseAllFragmentGroups(groups map[fragmentKey]*fragmentGroup) (int64, int64) {
	var releasedGroups, releasedBytes int64
	for key, group := range groups {
		releasedBytes += releaseFragmentGroup(group)
		delete(groups, key)
		releasedGroups++
	}
	return releasedGroups, releasedBytes
}

// NewFragmentAssembler creates a new fragment assembler
func NewFragmentAssembler() *FragmentAssembler {
	fa := &FragmentAssembler{
		fragments: make(map[fragmentKey]*fragmentGroup),
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
	go fa.cleanupLoop()
	return fa
}

// cleanupLoop removes expired fragment groups
func (fa *FragmentAssembler) cleanupLoop() {
	ticker := time.NewTicker(FragmentTimeout)
	defer ticker.Stop()
	defer close(fa.doneCh)

	for {
		select {
		case <-ticker.C:
			fa.mu.Lock()
			_, releasedBytes := cleanupExpiredFragmentGroups(fa.fragments, time.Now())
			fa.retainedBytes -= releasedBytes
			fa.mu.Unlock()
		case <-fa.stopCh:
			return
		}
	}
}

// Close stops cleanup, waits for its goroutine to exit, and releases all
// pending fragment groups. It is safe to call multiple times.
func (fa *FragmentAssembler) Close() {
	fa.closeOnce.Do(func() {
		fa.mu.Lock()
		fa.closed = true
		_, releasedBytes := releaseAllFragmentGroups(fa.fragments)
		fa.retainedBytes -= releasedBytes
		stopCh, doneCh := fa.stopCh, fa.doneCh
		if stopCh != nil {
			close(stopCh)
		}
		fa.mu.Unlock()

		if doneCh != nil {
			<-doneCh
		}
	})
}

// AddFragment adds a fragment and returns the complete packet if all fragments received
// Returns (nil, nil) if more fragments are needed
func (fa *FragmentAssembler) AddFragment(sessionID uint32, fragID uint16, index, total uint8, payload []byte) ([]byte, error) {
	if err := validateFragmentInput(index, total); err != nil {
		return nil, err
	}

	fa.mu.Lock()
	defer fa.mu.Unlock()
	if fa.closed {
		return nil, ErrFragmentAssemblerClosed
	}

	key := fragmentKey{sessionID: sessionID, fragID: fragID}
	group, exists := fa.fragments[key]
	if exists {
		if err := validateFragmentGroup(group, index, total); err != nil {
			if errors.Is(err, ErrFragmentTotalMismatch) {
				fa.retainedBytes -= releaseFragmentGroup(group)
				delete(fa.fragments, key)
			}
			return nil, err
		}
		if group.data[index] != nil {
			return nil, nil
		}
	}

	retainedBytes := int64(len(payload))
	if (!exists && len(fa.fragments) >= fragmentGroupLimit(fa.maxGroups)) || retainedBytes > fragmentByteLimit(fa.maxBytes)-fa.retainedBytes {
		return nil, ErrFragmentAssemblerFull
	}
	fa.retainedBytes += retainedBytes
	if !exists {
		group = &fragmentGroup{
			total:     total,
			data:      make([][]byte, total),
			createdAt: time.Now(),
		}
		fa.fragments[key] = group
	}
	group.retainedBytes += retainedBytes
	group.data[index] = make([]byte, len(payload))
	copy(group.data[index], payload)
	group.received++

	if group.received == group.total {
		// All fragments received, reassemble
		result := joinFragmentGroup(group)
		delete(fa.fragments, key)
		fa.retainedBytes -= releaseFragmentGroup(group)
		return result, nil
	}

	return nil, nil // More fragments needed
}

// fragmentShard holds fragments for a subset of fragment IDs
type fragmentShard struct {
	mu        sync.Mutex
	fragments map[fragmentKey]*fragmentGroup
}

// ShardedFragmentAssembler reassembles fragmented UDP packets with reduced lock contention
type ShardedFragmentAssembler struct {
	shards []fragmentShard
	seed   maphash.Seed

	retainedGroups atomic.Int64
	retainedBytes  atomic.Int64
	groupDrops     atomic.Uint64
	byteDrops      atomic.Uint64
	maxGroups      int
	maxBytes       int64

	lifecycleMu sync.RWMutex
	closed      bool
	stopCh      chan struct{}
	doneCh      chan struct{}
	closeOnce   sync.Once
}

// NewShardedFragmentAssembler creates a new sharded fragment assembler
func NewShardedFragmentAssembler(shardCount int) *ShardedFragmentAssembler {
	if shardCount <= 0 {
		shardCount = DefaultShardCount
	}

	sfa := &ShardedFragmentAssembler{
		shards: make([]fragmentShard, shardCount),
		seed:   maphash.MakeSeed(),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	for i := range sfa.shards {
		sfa.shards[i].fragments = make(map[fragmentKey]*fragmentGroup)
	}

	go sfa.cleanupLoop()
	return sfa
}

func (sfa *ShardedFragmentAssembler) shardIndex(key fragmentKey) int {
	return int(maphash.Comparable(sfa.seed, key) % uint64(len(sfa.shards)))
}

// getShard returns the shard for a fragment identity.
func (sfa *ShardedFragmentAssembler) getShard(key fragmentKey) *fragmentShard {
	return &sfa.shards[sfa.shardIndex(key)]
}

// Snapshot returns an exact cut of retained state without exposing fragment IDs.
func (sfa *ShardedFragmentAssembler) Snapshot() FragmentSnapshot {
	sfa.lifecycleMu.RLock()
	for i := range sfa.shards {
		sfa.shards[i].mu.Lock()
	}

	snapshot := FragmentSnapshot{
		GroupCapacityDrops: sfa.groupDrops.Load(),
		ByteCapacityDrops:  sfa.byteDrops.Load(),
	}
	for i := range sfa.shards {
		snapshot.RetainedGroups += int64(len(sfa.shards[i].fragments))
		for _, group := range sfa.shards[i].fragments {
			snapshot.RetainedBackingBytes += group.retainedBytes
		}
	}

	for i := len(sfa.shards); i > 0; i-- {
		sfa.shards[i-1].mu.Unlock()
	}
	sfa.lifecycleMu.RUnlock()
	return snapshot
}

func reserveFragmentCapacity(counter *atomic.Int64, amount, limit int64) bool {
	for {
		current := counter.Load()
		if amount > limit-current {
			return false
		}
		if counter.CompareAndSwap(current, current+amount) {
			return true
		}
	}
}

func fragmentBufferRetainedBytes(bufPtr *[]byte) int64 {
	return int64(cap(*bufPtr))
}

// cleanupLoop removes expired fragment groups from all shards
func (sfa *ShardedFragmentAssembler) cleanupLoop() {
	ticker := time.NewTicker(FragmentTimeout)
	defer ticker.Stop()
	defer close(sfa.doneCh)

	for {
		select {
		case <-ticker.C:
			sfa.lifecycleMu.RLock()
			now := time.Now()
			for i := range sfa.shards {
				shard := &sfa.shards[i]
				shard.mu.Lock()
				releasedGroups, releasedBytes := cleanupExpiredFragmentGroups(shard.fragments, now)
				sfa.retainedGroups.Add(-releasedGroups)
				sfa.retainedBytes.Add(-releasedBytes)
				shard.mu.Unlock()
			}
			sfa.lifecycleMu.RUnlock()
		case <-sfa.stopCh:
			return
		}
	}
}

// Close stops cleanup, waits for its goroutine to exit, and releases all
// pending fragment groups. It is safe to call multiple times.
func (sfa *ShardedFragmentAssembler) Close() {
	sfa.closeOnce.Do(func() {
		sfa.lifecycleMu.Lock()
		sfa.closed = true
		for i := range sfa.shards {
			shard := &sfa.shards[i]
			shard.mu.Lock()
			releasedGroups, releasedBytes := releaseAllFragmentGroups(shard.fragments)
			shard.mu.Unlock()
			sfa.retainedGroups.Add(-releasedGroups)
			sfa.retainedBytes.Add(-releasedBytes)
		}
		stopCh, doneCh := sfa.stopCh, sfa.doneCh
		if stopCh != nil {
			close(stopCh)
		}
		sfa.lifecycleMu.Unlock()

		if doneCh != nil {
			<-doneCh
		}
	})
}

// AddFragment adds a fragment and returns the complete packet if all fragments received.
// It locks only the relevant shard for reduced contention.
// Uses pooled buffers for fragment storage and tracks them for cleanup.
// Returns (nil, nil) if more fragments are needed.
func (sfa *ShardedFragmentAssembler) AddFragment(sessionID uint32, fragID uint16, index, total uint8, payload []byte) ([]byte, error) {
	if err := validateFragmentInput(index, total); err != nil {
		return nil, err
	}

	sfa.lifecycleMu.RLock()
	defer sfa.lifecycleMu.RUnlock()
	if sfa.closed {
		return nil, ErrFragmentAssemblerClosed
	}

	key := fragmentKey{sessionID: sessionID, fragID: fragID}
	shard := sfa.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	group, exists := shard.fragments[key]
	if exists {
		if err := validateFragmentGroup(group, index, total); err != nil {
			if errors.Is(err, ErrFragmentTotalMismatch) {
				releasedBytes := releaseFragmentGroup(group)
				delete(shard.fragments, key)
				sfa.retainedGroups.Add(-1)
				sfa.retainedBytes.Add(-releasedBytes)
			}
			return nil, err
		}
		if group.data[index] != nil {
			return nil, nil
		}
	}

	pooled := len(payload) <= FragmentBufferSize
	if !exists && !reserveFragmentCapacity(&sfa.retainedGroups, 1, int64(fragmentGroupLimit(sfa.maxGroups))) {
		sfa.groupDrops.Add(1)
		return nil, ErrFragmentAssemblerFull
	}

	retainedBytes := int64(len(payload))
	var bufPtr *[]byte
	if pooled {
		bufPtr = GetFragmentBuffer()
		retainedBytes = fragmentBufferRetainedBytes(bufPtr)
	}
	if !reserveFragmentCapacity(&sfa.retainedBytes, retainedBytes, fragmentByteLimit(sfa.maxBytes)) {
		sfa.byteDrops.Add(1)
		if bufPtr != nil {
			PutFragmentBuffer(bufPtr)
		}
		if !exists {
			sfa.retainedGroups.Add(-1)
		}
		return nil, ErrFragmentAssemblerFull
	}
	if !exists {
		group = &fragmentGroup{
			total:     total,
			data:      make([][]byte, total),
			createdAt: time.Now(),
		}
		shard.fragments[key] = group
	}
	group.retainedBytes += retainedBytes

	// Use pooled buffer for fragment storage if payload fits,
	// otherwise allocate a new buffer for large payloads.
	var buf []byte
	if pooled {
		buf = (*bufPtr)[:len(payload)]
	} else {
		buf = make([]byte, len(payload))
	}
	copy(buf, payload)
	group.data[index] = buf
	if bufPtr != nil {
		group.buffers = append(group.buffers, bufPtr)
	}
	group.received++

	if group.received == group.total {
		// All fragments received, reassemble
		result := joinFragmentGroup(group)
		delete(shard.fragments, key)
		releasedBytes := releaseFragmentGroup(group)
		sfa.retainedGroups.Add(-1)
		sfa.retainedBytes.Add(-releasedBytes)

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
