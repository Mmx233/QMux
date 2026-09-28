package mesh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
)

var errMeshControlQueueFull = errors.New("mesh control queue capacity reached")

type queuedControlFrame struct {
	sequence uint64
	last     bool
	data     []byte
}

type controlQueue struct {
	mu          sync.Mutex
	frames      []queuedControlFrame
	bytes       int64
	maxMessages int
	maxBytes    int64
	failed      bool
	changed     chan struct{}
}

func newControlQueue(limits config.MeshServerLimits) *controlQueue {
	return &controlQueue{
		maxMessages: limits.MaxControlQueueMessages,
		maxBytes:    limits.MaxControlQueueBytes,
		changed:     make(chan struct{}),
	}
}

func (q *controlQueue) signalLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

func (q *controlQueue) push(frames []queuedControlFrame) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failed {
		return errMeshControlQueueFull
	}
	var size int64
	for _, frame := range frames {
		size += int64(cap(frame.data))
	}
	if len(frames) > q.maxMessages-len(q.frames) || size > q.maxBytes-q.bytes {
		q.failed = true
		q.signalLocked()
		return errMeshControlQueueFull
	}
	q.frames = append(q.frames, frames...)
	q.bytes += size
	q.signalLocked()
	return nil
}

func (q *controlQueue) take(ctx context.Context) (queuedControlFrame, error) {
	for {
		q.mu.Lock()
		if q.failed {
			q.mu.Unlock()
			return queuedControlFrame{}, errMeshControlQueueFull
		}
		if len(q.frames) > 0 {
			frame := q.frames[0]
			q.mu.Unlock()
			return frame, nil
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return queuedControlFrame{}, context.Cause(ctx)
		}
	}
}

func (q *controlQueue) done(frame queuedControlFrame) {
	q.mu.Lock()
	if len(q.frames) > 0 && q.frames[0].sequence == frame.sequence && &q.frames[0].data[0] == &frame.data[0] {
		q.frames[0].data = nil
		q.frames = q.frames[1:]
		q.bytes -= int64(cap(frame.data))
		q.signalLocked()
	}
	q.mu.Unlock()
}

func (q *controlQueue) ready() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.changed
}

func (q *controlQueue) peek() (queuedControlFrame, bool, bool, <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.frames) == 0 {
		return queuedControlFrame{}, false, q.failed, q.changed
	}
	return q.frames[0], true, q.failed, q.changed
}

func (q *controlQueue) clear() {
	q.mu.Lock()
	clear(q.frames)
	q.frames = nil
	q.bytes = 0
	q.failed = true
	q.signalLocked()
	q.mu.Unlock()
}

func (q *controlQueue) stats() (int, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.frames), q.bytes
}

type publishedPath struct {
	id      string
	groupID string
	version uint64
	frame   []byte
	backing int64
	refs    int
}

type peerLink struct {
	queue *controlQueue
	close func()
}

type peerSnapshot struct {
	revision uint64
	groups   []*groupRecord
	paths    []*publishedPath
	owner    *controlState
	link     *peerLink
}

func (snapshot *peerSnapshot) release() {
	if snapshot == nil || snapshot.owner == nil {
		return
	}
	for _, group := range snapshot.groups {
		snapshot.owner.ledger.release(group)
	}
	snapshot.owner.mu.Lock()
	for _, path := range snapshot.paths {
		snapshot.owner.releasePathLocked(path)
	}
	snapshot.owner.mu.Unlock()
	snapshot.groups = nil
	snapshot.paths = nil
}

type controlState struct {
	mu           sync.Mutex
	ledger       *declarationLedger
	limits       config.MeshServerLimits
	closed       bool
	revision     uint64
	groups       map[string]*groupRecord
	paths        map[string]*publishedPath
	links        map[*peerLink]struct{}
	pathRecords  int
	pathBytes    int64
	maxPathBytes int64
}

func newControlState(ledger *declarationLedger, limits config.MeshServerLimits) *controlState {
	const frameSize = int64(5 + protocol.MaxControlPayloadSize)
	maxPathBytes := int64(^uint64(0) >> 1)
	if int64(limits.MaxTotalPaths) <= maxPathBytes/frameSize {
		maxPathBytes = int64(limits.MaxTotalPaths) * frameSize
	}
	return &controlState{
		ledger: ledger, limits: limits,
		maxPathBytes: maxPathBytes,
		groups:       make(map[string]*groupRecord), paths: make(map[string]*publishedPath),
		links: make(map[*peerLink]struct{}),
	}
}

func (s *controlState) subscribe(close func()) *peerSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	link := &peerLink{queue: newControlQueue(s.limits), close: close}
	s.links[link] = struct{}{}
	snapshot := &peerSnapshot{revision: s.revision, owner: s, link: link}
	for _, record := range s.groups {
		s.ledger.mu.Lock()
		record.refs++
		s.ledger.mu.Unlock()
		snapshot.groups = append(snapshot.groups, record)
	}
	for _, path := range s.paths {
		path.refs++
		snapshot.paths = append(snapshot.paths, path)
	}
	slices.SortFunc(snapshot.groups, func(a, b *groupRecord) int { return compareGroupKey(a.key, b.key) })
	slices.SortFunc(snapshot.paths, func(a, b *publishedPath) int {
		if a.id < b.id {
			return -1
		}
		if a.id > b.id {
			return 1
		}
		return 0
	})
	return snapshot
}

func (s *controlState) releasePathLocked(path *publishedPath) {
	path.refs--
	if path.refs == 0 {
		s.pathRecords--
		s.pathBytes -= path.backing
		path.frame = nil
	}
}

func (s *controlState) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	groups := s.groups
	s.groups = nil
	for _, path := range s.paths {
		s.releasePathLocked(path)
	}
	s.paths = nil
	s.links = nil
	s.mu.Unlock()
	for _, group := range groups {
		s.ledger.release(group)
	}
}

func compareGroupKey(a, b groupKey) int {
	if a.id < b.id {
		return -1
	}
	if a.id > b.id {
		return 1
	}
	if a.version < b.version {
		return -1
	}
	if a.version > b.version {
		return 1
	}
	return 0
}

func (s *controlState) unsubscribe(link *peerLink) {
	if link == nil {
		return
	}
	s.mu.Lock()
	delete(s.links, link)
	s.mu.Unlock()
}

func (s *controlState) fence() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

func (s *controlState) publishGroup(record *groupRecord) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrMeshServerStopped
	}
	sequence := s.revision + 1
	frames, err := declarationFrames(sequence, record.bytes)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	previous := s.groups[record.key.id]
	s.groups[record.key.id] = record
	s.revision = sequence
	failed := s.broadcastLocked(frames)
	s.mu.Unlock()
	if previous != nil {
		s.ledger.release(previous)
	}
	for _, link := range failed {
		link.close()
	}
	return nil
}

func (s *controlState) publishPath(path protocol.MeshPath) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrMeshServerStopped
	}
	if group := s.groups[path.GroupID]; group == nil || group.key.version != path.RuleVersion {
		s.mu.Unlock()
		return errors.New("mesh path requires a published matching declaration")
	}
	if len(path.Servers) > s.limits.MaxPathHops {
		s.mu.Unlock()
		return errors.New("mesh published path limit reached")
	}
	groupPaths := 0
	for _, item := range s.paths {
		if item.groupID == path.GroupID && item.id != path.PathID {
			groupPaths++
		}
	}
	if groupPaths >= s.limits.MaxPathsPerGroup {
		s.mu.Unlock()
		return errors.New("mesh published paths per group reached")
	}
	sequence := s.revision + 1
	path.Sequence = sequence
	frame, err := protocol.MarshalMeshControlFrame(path)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	previous := s.paths[path.PathID]
	count := s.pathRecords
	bytes := s.pathBytes
	if previous != nil && previous.refs == 1 {
		count--
		bytes -= previous.backing
	}
	backing := int64(cap(frame) + len(path.PathID) + len(path.GroupID))
	if count >= s.limits.MaxTotalPaths || backing > s.maxPathBytes-bytes {
		s.mu.Unlock()
		return errors.New("mesh published path transition capacity reached")
	}
	if previous != nil {
		s.releasePathLocked(previous)
	}
	s.paths[path.PathID] = &publishedPath{id: path.PathID, groupID: path.GroupID, version: path.RuleVersion, frame: frame, backing: backing, refs: 1}
	s.pathRecords++
	s.pathBytes += backing
	s.revision = sequence
	failed := s.broadcastLocked([]queuedControlFrame{{sequence: sequence, last: true, data: frame}})
	s.mu.Unlock()
	for _, link := range failed {
		link.close()
	}
	return nil
}

func (s *controlState) withdrawPath(id string) error {
	s.mu.Lock()
	if s.closed || s.paths[id] == nil {
		s.mu.Unlock()
		return fmt.Errorf("unknown mesh path %q", id)
	}
	sequence := s.revision + 1
	frame, err := protocol.MarshalMeshControlFrame(protocol.MeshWithdraw{Sequence: sequence, PathID: id})
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.releasePathLocked(s.paths[id])
	delete(s.paths, id)
	s.revision = sequence
	failed := s.broadcastLocked([]queuedControlFrame{{sequence: sequence, last: true, data: frame}})
	s.mu.Unlock()
	for _, link := range failed {
		link.close()
	}
	return nil
}

func (s *controlState) broadcastLocked(frames []queuedControlFrame) []*peerLink {
	var failed []*peerLink
	for link := range s.links {
		if err := link.queue.push(frames); err != nil {
			failed = append(failed, link)
		}
	}
	return failed
}

func declarationFrames(sequence uint64, data []byte) ([]queuedControlFrame, error) {
	if len(data) == 0 {
		return nil, errors.New("empty mesh declaration")
	}
	var frames []queuedControlFrame
	digest := sha256Digest(data)
	for offset := 0; offset < len(data); offset += protocol.MaxMeshChunkDataSize {
		end := min(len(data), offset+protocol.MaxMeshChunkDataSize)
		chunk := protocol.MeshChunk{Sequence: sequence, Total: uint32(len(data)), Offset: uint32(offset), Data: data[offset:end]}
		if offset == 0 {
			chunk.Digest = digest
		}
		frame, err := protocol.MarshalMeshControlFrame(chunk)
		if err != nil {
			return nil, err
		}
		frames = append(frames, queuedControlFrame{sequence: sequence, last: end == len(data), data: frame})
	}
	return frames, nil
}

func sha256Digest(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func sendPeerSnapshot(ctx context.Context, w io.Writer, snapshot *peerSnapshot) error {
	if snapshot == nil {
		return sendPeerInitial(w)
	}
	defer snapshot.release()
	var groupBytes uint64
	for _, group := range snapshot.groups {
		groupBytes += uint64(len(group.bytes))
	}
	begin := protocol.MeshBegin{
		Revision: snapshot.revision, Groups: uint32(len(snapshot.groups)),
		Paths: uint32(len(snapshot.paths)), GroupBytes: groupBytes,
	}
	if err := protocol.WriteMeshControl(w, begin); err != nil {
		return err
	}
	for i, group := range snapshot.groups {
		if err := sendDeclaration(w, 0, uint32(i), group.bytes); err != nil {
			return err
		}
	}
	for _, path := range snapshot.paths {
		kind, payload, err := protocol.ReadMessageLimited(bytes.NewReader(path.frame), protocol.MaxControlPayloadSize)
		if err != nil {
			return err
		}
		decoded, err := protocol.DecodeMeshControl(kind, payload)
		if err != nil {
			return err
		}
		value := decoded.(protocol.MeshPath)
		value.Sequence = 0
		if err := protocol.WriteMeshControl(w, value); err != nil {
			return err
		}
	}
	fence := snapshot.owner.fence()
	sent := snapshot.revision
	for sent < fence {
		frame, err := snapshot.link.queue.take(ctx)
		if err != nil {
			return err
		}
		if frame.sequence != sent+1 && frame.sequence != sent || frame.sequence > fence {
			return errors.New("mesh peer initial delta sequence gap")
		}
		n, err := w.Write(frame.data)
		if err != nil {
			return err
		}
		if n != len(frame.data) {
			return io.ErrShortWrite
		}
		snapshot.link.queue.done(frame)
		if frame.last {
			sent = frame.sequence
		}
	}
	return protocol.WriteMeshControl(w, protocol.MeshEnd{
		Groups: begin.Groups, Paths: begin.Paths, GroupBytes: groupBytes, FinalSequence: fence,
	})
}
