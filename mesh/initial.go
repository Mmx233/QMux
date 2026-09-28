package mesh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
)

type stagedPath struct {
	id      string
	groupID string
	version uint64
	frame   []byte
	backing int64
}

type pathBudget struct {
	mu       sync.Mutex
	count    int
	bytes    int64
	maxCount int
	maxBytes int64
}

func newPathBudget(limits config.MeshServerLimits) *pathBudget {
	const frameSize = int64(5 + protocol.MaxControlPayloadSize)
	maxBytes := int64(math.MaxInt64)
	if int64(limits.MaxTotalPaths) <= math.MaxInt64/frameSize {
		maxBytes = int64(limits.MaxTotalPaths) * frameSize
	}
	return &pathBudget{maxCount: limits.MaxTotalPaths, maxBytes: maxBytes}
}

func (b *pathBudget) retain(backing int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.count >= b.maxCount || backing > b.maxBytes-b.bytes {
		return errors.New("mesh temporary path capacity reached")
	}
	b.count++
	b.bytes += backing
	return nil
}

func (b *pathBudget) release(backing int64) {
	b.mu.Lock()
	b.count--
	b.bytes -= backing
	b.mu.Unlock()
}

type stagedState struct {
	ledger   *declarationLedger
	paths    *pathBudget
	groups   []*groupRecord
	items    []stagedPath
	revision uint64
}

func (s *stagedState) close() {
	if s == nil {
		return
	}
	for _, group := range s.groups {
		s.ledger.release(group)
	}
	for _, path := range s.items {
		s.paths.release(path.backing)
	}
	s.groups = nil
	s.items = nil
}

func sendClientInitial(w io.Writer, declaration []byte) error {
	if len(declaration) == 0 || len(declaration) > math.MaxUint32 {
		return errors.New("mesh declaration is not representable on the control wire")
	}
	if err := protocol.WriteMeshControl(w, protocol.MeshBegin{Groups: 1, GroupBytes: uint64(len(declaration))}); err != nil {
		return err
	}
	if err := sendDeclaration(w, 0, 0, declaration); err != nil {
		return err
	}
	return protocol.WriteMeshControl(w, protocol.MeshEnd{Groups: 1, GroupBytes: uint64(len(declaration))})
}

func sendPeerInitial(w io.Writer) error {
	if err := protocol.WriteMeshControl(w, protocol.MeshBegin{}); err != nil {
		return err
	}
	return protocol.WriteMeshControl(w, protocol.MeshEnd{})
}

func sendDeclaration(w io.Writer, sequence uint64, record uint32, data []byte) error {
	if len(data) == 0 || len(data) > math.MaxUint32 {
		return errors.New("mesh declaration is not representable on the control wire")
	}
	digest := sha256.Sum256(data)
	for offset := 0; offset < len(data); offset += protocol.MaxMeshChunkDataSize {
		end := min(len(data), offset+protocol.MaxMeshChunkDataSize)
		chunk := protocol.MeshChunk{
			Sequence: sequence, Record: record, Total: uint32(len(data)),
			Offset: uint32(offset), Data: data[offset:end],
		}
		if offset == 0 {
			chunk.Digest = digest[:]
		}
		if err := protocol.WriteMeshControl(w, chunk); err != nil {
			return err
		}
	}
	return nil
}

func receiveInitial(ctx context.Context, r io.Reader, ledger *declarationLedger, paths *pathBudget, limits config.MeshServerLimits, role, expectedGroup string) (_ *stagedState, err error) {
	state := &stagedState{ledger: ledger, paths: paths}
	defer func() {
		if err != nil {
			state.close()
		}
	}()
	first, err := protocol.ReadMeshControl(r)
	if err != nil {
		return nil, err
	}
	begin, ok := first.(protocol.MeshBegin)
	if !ok {
		return nil, fmt.Errorf("mesh initial state must begin with Begin, got %T", first)
	}
	if role == protocol.MeshRoleClient && (begin.Groups != 1 || begin.Paths != 0) {
		return nil, errors.New("mesh client initial state must contain one group and no paths")
	}
	if begin.Groups > uint32(limits.MaxGroups) || begin.Paths > uint32(limits.MaxTotalPaths) ||
		begin.GroupBytes > uint64(limits.MaxTotalGroupDeclarationBytes) {
		return nil, errors.New("mesh initial state exceeds temporary limits")
	}
	var claim *declarationClaim
	defer func() { claim.close() }()
	var raw []byte
	var digest []byte
	var groupBytes uint64
	var snapshotGroups uint32
	var pathCount uint32
	delta := newDeltaApplier(ctx, state, ledger, paths, limits, begin.Revision)
	defer delta.close()
	for {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		message, readErr := protocol.ReadMeshControl(r)
		if readErr != nil {
			return nil, readErr
		}
		switch value := message.(type) {
		case protocol.MeshChunk:
			if value.Sequence != 0 {
				if role == protocol.MeshRoleClient {
					return nil, errors.New("mesh client initial state cannot contain declaration deltas")
				}
				if snapshotGroups != begin.Groups || pathCount != begin.Paths || raw != nil {
					return nil, errors.New("mesh delta preceded complete snapshot")
				}
				if err := delta.apply(value); err != nil {
					return nil, err
				}
				continue
			}
			if snapshotGroups == begin.Groups || value.Record != snapshotGroups || value.Record >= begin.Groups {
				return nil, errors.New("mesh declaration record sequence is not contiguous")
			}
			if raw == nil {
				if value.Offset != 0 || uint64(value.Total) > begin.GroupBytes-groupBytes {
					return nil, errors.New("mesh declaration length or offset exceeds Begin")
				}
				claim, err = ledger.begin(value.Total, value.Digest)
				if err != nil {
					return nil, err
				}
				raw = make([]byte, int(value.Total))
				digest = value.Digest
				claim.trackInput(raw, digest)
			}
			if value.Total != uint32(len(raw)) || value.Offset >= uint32(len(raw)) ||
				value.Offset != uint32(groupBytes-currentGroupStart(state.groups)) {
				return nil, errors.New("mesh declaration chunk offset is not contiguous")
			}
			claim.compare(value.Offset, value.Data)
			copy(raw[value.Offset:], value.Data)
			groupBytes += uint64(len(value.Data))
			if int(value.Offset)+len(value.Data) == len(raw) {
				sum := sha256.Sum256(raw)
				if !bytes.Equal(sum[:], digest) {
					return nil, errors.New("mesh declaration digest mismatch")
				}
				record, finishErr := claim.finish(ctx, raw, expectedGroup)
				if finishErr != nil {
					return nil, finishErr
				}
				for _, previous := range state.groups {
					if previous.key.id == record.key.id {
						ledger.release(record)
						return nil, errors.New("duplicate mesh group in initial snapshot")
					}
				}
				state.groups = append(state.groups, record)
				snapshotGroups++
				claim.close()
				claim = nil
				raw = nil
				digest = nil
			}
		case protocol.MeshPath:
			if role == protocol.MeshRoleClient {
				return nil, errors.New("mesh client initial state cannot contain paths")
			}
			if value.Sequence != 0 {
				if snapshotGroups != begin.Groups || pathCount != begin.Paths || raw != nil {
					return nil, errors.New("mesh delta preceded complete snapshot")
				}
				if err := delta.apply(value); err != nil {
					return nil, err
				}
				continue
			}
			if raw != nil || pathCount >= begin.Paths {
				return nil, errors.New("invalid mesh initial path order or limit")
			}
			if err := state.addPath(value, limits, true); err != nil {
				return nil, err
			}
			pathCount++
		case protocol.MeshWithdraw:
			if role == protocol.MeshRoleClient {
				return nil, errors.New("mesh client initial state cannot contain withdrawals")
			}
			if snapshotGroups != begin.Groups || pathCount != begin.Paths || raw != nil {
				return nil, errors.New("mesh withdrawal preceded complete snapshot")
			}
			if err := delta.apply(value); err != nil {
				return nil, err
			}
		case protocol.MeshEnd:
			if raw != nil || delta.raw != nil || snapshotGroups != begin.Groups || pathCount != begin.Paths ||
				groupBytes != begin.GroupBytes || value.Groups != begin.Groups || value.Paths != begin.Paths ||
				value.GroupBytes != groupBytes || value.FinalSequence != delta.revision {
				return nil, errors.New("mesh initial End does not match complete snapshot")
			}
			if err := state.validatePaths(); err != nil {
				return nil, err
			}
			state.revision = delta.revision
			return state, nil
		default:
			return nil, fmt.Errorf("unexpected mesh initial message %T", message)
		}
	}
}

func currentGroupStart(groups []*groupRecord) uint64 {
	var total uint64
	for _, group := range groups {
		total += uint64(len(group.bytes))
	}
	return total
}
