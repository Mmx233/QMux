package mesh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
)

type deltaApplier struct {
	ctx      context.Context
	state    *stagedState
	ledger   *declarationLedger
	paths    *pathBudget
	limits   config.MeshServerLimits
	revision uint64
	claim    *declarationClaim
	raw      []byte
	digest   []byte
	offset   uint32
}

func newDeltaApplier(ctx context.Context, state *stagedState, ledger *declarationLedger, paths *pathBudget, limits config.MeshServerLimits, revision uint64) *deltaApplier {
	return &deltaApplier{ctx: ctx, state: state, ledger: ledger, paths: paths, limits: limits, revision: revision}
}

func (a *deltaApplier) close() {
	a.claim.close()
	a.claim = nil
	a.raw = nil
}

func (a *deltaApplier) apply(message any) error {
	if a.ctx.Err() != nil {
		return context.Cause(a.ctx)
	}
	switch value := message.(type) {
	case protocol.MeshChunk:
		if value.Sequence != a.revision+1 || value.Record != 0 || value.Offset != a.offset {
			return errors.New("mesh declaration delta sequence or offset gap")
		}
		if a.raw == nil {
			var err error
			a.claim, err = a.ledger.begin(value.Total, value.Digest)
			if err != nil {
				return err
			}
			a.raw = make([]byte, int(value.Total))
			a.digest = value.Digest
			a.claim.trackInput(a.raw, a.digest)
		}
		if value.Total != uint32(len(a.raw)) || uint64(value.Offset)+uint64(len(value.Data)) > uint64(len(a.raw)) {
			return errors.New("mesh declaration delta changed length")
		}
		a.claim.compare(value.Offset, value.Data)
		copy(a.raw[value.Offset:], value.Data)
		a.offset += uint32(len(value.Data))
		if a.offset == uint32(len(a.raw)) {
			sum := sha256.Sum256(a.raw)
			if !bytes.Equal(sum[:], a.digest) {
				return errors.New("mesh declaration delta digest mismatch")
			}
			record, err := a.claim.finish(a.ctx, a.raw, "")
			if err != nil {
				return err
			}
			for i, previous := range a.state.groups {
				if previous.key.id == record.key.id {
					a.state.groups[i] = record
					a.ledger.release(previous)
					a.completeChunk()
					return nil
				}
			}
			if len(a.state.groups) >= a.limits.MaxGroups {
				a.ledger.release(record)
				return errMeshGroupCapacity
			}
			a.state.groups = append(a.state.groups, record)
			a.completeChunk()
		}
	case protocol.MeshPath:
		if a.raw != nil || value.Sequence != a.revision+1 {
			return errors.New("mesh path delta sequence gap")
		}
		if err := a.state.addPath(value, a.limits, false); err != nil {
			return err
		}
		a.revision++
	case protocol.MeshWithdraw:
		if a.raw != nil || value.Sequence != a.revision+1 {
			return errors.New("mesh withdrawal delta sequence gap")
		}
		if !a.state.withdrawPath(value.PathID) {
			return fmt.Errorf("mesh withdrawal for unknown path %q", value.PathID)
		}
		a.revision++
	default:
		return fmt.Errorf("unexpected mesh delta %T", message)
	}
	return nil
}

func (a *deltaApplier) completeChunk() {
	a.claim.close()
	a.claim = nil
	a.raw = nil
	a.digest = nil
	a.offset = 0
	a.revision++
}

func (s *stagedState) addPath(path protocol.MeshPath, limits config.MeshServerLimits, snapshot bool) error {
	if len(path.Servers) == 0 || len(path.Servers) > limits.MaxPathHops {
		return errors.New("mesh path exceeds hop limit")
	}
	matched := false
	for _, group := range s.groups {
		if group.key == (groupKey{path.GroupID, path.RuleVersion}) {
			matched = true
			break
		}
	}
	if !matched {
		return errors.New("mesh path has no complete matching declaration")
	}
	for i, old := range s.items {
		if old.id == path.PathID {
			if snapshot {
				return errors.New("duplicate mesh path in snapshot")
			}
			s.paths.release(old.backing)
			copy(s.items[i:], s.items[i+1:])
			s.items[len(s.items)-1] = stagedPath{}
			s.items = s.items[:len(s.items)-1]
			break
		}
	}
	if len(s.items) >= limits.MaxTotalPaths {
		return errors.New("mesh temporary path count reached")
	}
	groupPaths := 0
	for _, item := range s.items {
		if item.groupID == path.GroupID {
			groupPaths++
		}
	}
	if groupPaths >= limits.MaxPathsPerGroup {
		return errors.New("mesh temporary paths per group reached")
	}
	frame, err := protocol.MarshalMeshControlFrame(path)
	if err != nil {
		return err
	}
	item := stagedPath{
		id: path.PathID, groupID: path.GroupID, version: path.RuleVersion, frame: frame,
		backing: int64(cap(frame) + len(path.PathID) + len(path.GroupID)),
	}
	if err := s.paths.retain(item.backing); err != nil {
		return err
	}
	s.items = append(s.items, item)
	return nil
}

func (s *stagedState) withdrawPath(id string) bool {
	for i, item := range s.items {
		if item.id == id {
			s.paths.release(item.backing)
			copy(s.items[i:], s.items[i+1:])
			s.items[len(s.items)-1] = stagedPath{}
			s.items = s.items[:len(s.items)-1]
			return true
		}
	}
	return false
}

func (s *stagedState) validatePaths() error {
	for _, path := range s.items {
		found := false
		for _, group := range s.groups {
			if group.key == (groupKey{path.groupID, path.version}) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("mesh path %q references an absent declaration version", path.id)
		}
	}
	return nil
}
