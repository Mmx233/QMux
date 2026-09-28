package mesh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"github.com/Mmx233/QMux/config"
)

var (
	errMeshGroupCapacity = errors.New("mesh group declaration capacity reached")
	errMeshGroupConflict = errors.New("mesh group rule version conflicts with retained declaration")
	meshValidationSlot   = make(chan struct{}, 1)
)

type groupKey struct {
	id      string
	version uint64
}

type groupRecord struct {
	key    groupKey
	policy string
	digest [32]byte
	bytes  []byte
	refs   int
}

// declarationLedger accounts for staged, published, and retiring references.
// Validation work buffers have separate bounded ownership in the receiver.
type declarationLedger struct {
	mu           sync.Mutex
	maxGroups    int
	maxBytes     int64
	maxSingle    int64
	groups       int
	bytes        int64
	reserved     int
	reservedSize int64
	workBytes    int64
	workPeak     int64
	parsePeak    int64
	records      map[groupKey]*groupRecord
	byDigest     map[[32]byte][]*groupRecord
}

type declarationClaim struct {
	ledger    *declarationLedger
	candidate *groupRecord
	matched   bool
	reserved  bool
	size      int64
	workSize  int64
}

func newDeclarationLedger(limits config.MeshServerLimits) *declarationLedger {
	return &declarationLedger{
		maxGroups: limits.MaxGroups,
		maxBytes:  limits.MaxTotalGroupDeclarationBytes,
		maxSingle: limits.MaxGroupDeclarationBytes,
		records:   make(map[groupKey]*groupRecord),
		byDigest:  make(map[[32]byte][]*groupRecord),
	}
}

func (l *declarationLedger) begin(size uint32, digest []byte) (*declarationClaim, error) {
	if size == 0 || int64(size) > l.maxSingle {
		return nil, fmt.Errorf("mesh declaration length %d exceeds limit %d", size, l.maxSingle)
	}
	claim := &declarationClaim{ledger: l, size: int64(size)}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(digest) == sha256.Size {
		var key [sha256.Size]byte
		copy(key[:], digest)
		for _, record := range l.byDigest[key] {
			if len(record.bytes) != int(size) {
				continue
			}
			record.refs++
			claim.candidate = record
			claim.matched = true
			return claim, nil
		}
	}
	if l.canReserveLocked(int64(size)) {
		l.reserved++
		l.reservedSize += int64(size)
		claim.reserved = true
	}
	return claim, nil
}

func (claim *declarationClaim) compare(offset uint32, data []byte) {
	if claim.candidate != nil && claim.matched && !bytes.Equal(claim.candidate.bytes[offset:int(offset)+len(data)], data) {
		claim.matched = false
	}
}

func (claim *declarationClaim) trackInput(raw, digest []byte) {
	claim.workSize = int64(cap(raw) + cap(digest))
	l := claim.ledger
	l.mu.Lock()
	l.workBytes += claim.workSize
	l.workPeak = max(l.workPeak, l.workBytes)
	l.mu.Unlock()
}

func (claim *declarationClaim) releaseWorkLocked() {
	claim.ledger.workBytes -= claim.workSize
	claim.workSize = 0
}

func (l *declarationLedger) changeWork(size int64) {
	l.mu.Lock()
	l.workBytes += size
	l.workPeak = max(l.workPeak, l.workBytes)
	l.mu.Unlock()
}

func (l *declarationLedger) canReserveLocked(size int64) bool {
	return l.groups+l.reserved < l.maxGroups &&
		size <= l.maxBytes-l.bytes-l.reservedSize
}

func (l *declarationLedger) releaseLocked(record *groupRecord) {
	if record == nil {
		return
	}
	record.refs--
	if record.refs == 0 {
		delete(l.records, record.key)
		bucket := l.byDigest[record.digest]
		for i, item := range bucket {
			if item == record {
				copy(bucket[i:], bucket[i+1:])
				bucket[len(bucket)-1] = nil
				bucket = bucket[:len(bucket)-1]
				break
			}
		}
		if len(bucket) == 0 {
			delete(l.byDigest, record.digest)
		} else {
			l.byDigest[record.digest] = bucket
		}
		l.groups--
		l.bytes -= int64(cap(record.bytes))
	}
}

func (l *declarationLedger) release(record *groupRecord) {
	l.mu.Lock()
	l.releaseLocked(record)
	l.mu.Unlock()
}

func (l *declarationLedger) retain(record *groupRecord) {
	l.mu.Lock()
	record.refs++
	l.mu.Unlock()
}

func (claim *declarationClaim) close() {
	if claim == nil || claim.ledger == nil {
		return
	}
	l := claim.ledger
	l.mu.Lock()
	l.releaseLocked(claim.candidate)
	claim.candidate = nil
	claim.releaseWorkLocked()
	if claim.reserved {
		l.reserved--
		l.reservedSize -= claim.size
		claim.reserved = false
	}
	l.mu.Unlock()
}

// finish takes ownership of the complete canonical input if a new record is
// needed. The candidate reference remains valid through strict validation.
func (claim *declarationClaim) finish(ctx context.Context, input []byte, expectedGroup string) (*groupRecord, error) {
	select {
	case meshValidationSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	if ctx.Err() != nil {
		<-meshValidationSlot
		return nil, context.Cause(ctx)
	}
	groupID, version, policy, work, err := config.ParseMeshGroupCanonicalPolicyMeasured(input, claim.ledger.changeWork)
	<-meshValidationSlot
	claim.ledger.mu.Lock()
	claim.ledger.parsePeak = max(claim.ledger.parsePeak, work.PeakBytes)
	claim.ledger.mu.Unlock()
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if err != nil {
		return nil, err
	}
	if expectedGroup != "" && groupID != expectedGroup {
		return nil, fmt.Errorf("mesh declaration group_id %q does not match registration %q", groupID, expectedGroup)
	}
	l := claim.ledger
	l.mu.Lock()
	defer l.mu.Unlock()
	key := groupKey{groupID, version}
	if claim.candidate != nil && claim.matched && bytes.Equal(claim.candidate.bytes, input) {
		record := claim.candidate
		claim.candidate = nil
		claim.releaseWorkLocked()
		return record, nil
	}
	conflict := claim.candidate != nil && claim.candidate.key == key
	// The full input is now retained independently of the candidate. Releasing
	// its last reference can make the transition fit without borrowing quota.
	l.releaseLocked(claim.candidate)
	claim.candidate = nil
	if conflict {
		return nil, errMeshGroupConflict
	}
	if previous := l.records[key]; previous != nil {
		if !bytes.Equal(previous.bytes, input) {
			return nil, errMeshGroupConflict
		}
		previous.refs++
		claim.releaseWorkLocked()
		return previous, nil
	}
	if claim.reserved {
		l.reserved--
		l.reservedSize -= claim.size
		claim.reserved = false
	}
	var normalizedBytes int64
	if cap(input) != len(input) {
		exact := make([]byte, len(input))
		normalizedBytes = int64(cap(exact))
		l.workBytes += normalizedBytes
		l.workPeak = max(l.workPeak, l.workBytes)
		copy(exact, input)
		input = exact
	}
	if !l.canReserveLocked(int64(cap(input))) {
		l.workBytes -= normalizedBytes
		return nil, errMeshGroupCapacity
	}
	record := &groupRecord{key: key, policy: policy, digest: sha256.Sum256(input), bytes: input, refs: 1}
	l.records[key] = record
	l.byDigest[record.digest] = append(l.byDigest[record.digest], record)
	l.groups++
	l.bytes += int64(cap(input))
	l.workBytes -= normalizedBytes
	claim.releaseWorkLocked()
	return record, nil
}

func (l *declarationLedger) snapshot() (groups int, bytes int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.groups + l.reserved, l.bytes + l.reservedSize
}

func (l *declarationLedger) workSnapshot() (current, combinedPeak, validationPeak int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.workBytes, l.workPeak, l.parsePeak
}
