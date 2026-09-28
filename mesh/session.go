package mesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/internal/outbound"
	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
)

const (
	meshApplicationError = quic.ApplicationErrorCode(0x4d03)
	meshStreamError      = quic.StreamErrorCode(0x4d03)
)

// Session is one authenticated and published mesh connection generation.
type Session struct {
	role       Role
	direction  Direction
	serverID   string
	instanceID string
	groupID    string
	peerID     string

	conn     *quic.Conn
	control  *quic.Stream
	outbound *controlQueue
	apply    *controlQueue
	staged   *stagedState
	ledger   *declarationLedger
	paths    *pathBudget
	limits   config.MeshServerLimits

	ctx            context.Context
	cancel         context.CancelFunc
	closeOnce      sync.Once
	done           chan struct{}
	started        atomic.Bool
	stable         atomic.Bool
	heartbeatWrite atomic.Pointer[func(io.Writer, int64) error]
	controlWrite   atomic.Pointer[func(io.Writer, []byte) (int, error)]
	errMu          sync.Mutex
	err            error

	stableGrace time.Duration
}

func newSession(
	parent context.Context,
	role Role,
	direction Direction,
	serverID, instanceID, groupID, peerID string,
	conn *quic.Conn,
	control *quic.Stream,
	stableGrace time.Duration,
) *Session {
	ctx, cancel := context.WithCancel(parent)
	return &Session{
		role:        role,
		direction:   direction,
		serverID:    serverID,
		instanceID:  instanceID,
		groupID:     groupID,
		peerID:      peerID,
		conn:        conn,
		control:     control,
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
		stableGrace: stableGrace,
	}
}

func (s *Session) Role() Role                  { return s.role }
func (s *Session) Direction() Direction        { return s.direction }
func (s *Session) ServerID() string            { return s.serverID }
func (s *Session) InstanceID() string          { return s.instanceID }
func (s *Session) GroupID() string             { return s.groupID }
func (s *Session) PeerID() string              { return s.peerID }
func (s *Session) Connection() *quic.Conn      { return s.conn }
func (s *Session) ControlStream() *quic.Stream { return s.control }
func (s *Session) Done() <-chan struct{}       { return s.done }
func (s *Session) ControlStarted() bool        { return s.started.Load() }
func (s *Session) ReconnectStable() bool       { return s.stable.Load() }

func (s *Session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.outbound != nil {
			s.outbound.clear()
		}
		if s.apply != nil {
			s.apply.clear()
		}
		s.control.CancelRead(meshStreamError)
		s.control.CancelWrite(meshStreamError)
		_ = s.conn.CloseWithError(meshApplicationError, "mesh session closed")
	})
}

func (s *Session) configurePeerControl(outbound *controlQueue, staged *stagedState, ledger *declarationLedger, paths *pathBudget, limits config.MeshServerLimits) {
	s.outbound = outbound
	s.apply = newControlQueue(limits)
	s.staged = staged
	s.ledger = ledger
	s.paths = paths
	s.limits = limits
}

func (s *Session) Wait() { <-s.done }

func (s *Session) run(heartbeatInterval, healthTimeout time.Duration) error {
	s.started.Store(true)
	startedAt := time.Now()
	err := s.controlLoop(startedAt, heartbeatInterval, healthTimeout)
	s.errMu.Lock()
	s.err = err
	s.errMu.Unlock()
	s.Close()
	close(s.done)
	return err
}

type meshControlRead struct {
	msgType byte
	err     error
}

func (s *Session) controlLoop(startedAt time.Time, heartbeatInterval, healthTimeout time.Duration) error {
	readCtx, cancelRead := context.WithCancel(s.ctx)
	readDone := make(chan struct{})
	reads := make(chan meshControlRead, 1)
	applyCtx, cancelApply := context.WithCancel(s.ctx)
	applyDone := make(chan struct{})
	applyErrors := make(chan error, 1)
	if s.apply != nil {
		go func() {
			defer close(applyDone)
			applier := newDeltaApplier(applyCtx, s.staged, s.ledger, s.paths, s.limits, s.staged.revision)
			defer applier.close()
			for {
				frame, err := s.apply.take(applyCtx)
				if err != nil {
					if applyCtx.Err() == nil {
						applyErrors <- err
					}
					return
				}
				kind, payload, err := protocol.ReadMessageLimited(bytes.NewReader(frame.data), protocol.MaxControlPayloadSize)
				if err == nil {
					var message any
					message, err = protocol.DecodeMeshControl(kind, payload)
					if err == nil {
						err = applier.apply(message)
					}
				}
				if err != nil {
					applyErrors <- err
					return
				}
				s.apply.done(frame)
			}
		}()
	} else {
		close(applyDone)
	}
	go func() {
		defer close(readDone)
		for {
			msgType, payload, err := protocol.ReadMessageLimited(s.control, protocol.MaxControlPayloadSize)
			if err == nil && msgType != protocol.MsgTypeHeartbeat && s.apply != nil {
				frame := make([]byte, 5+len(payload))
				frame[0] = msgType
				binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
				copy(frame[5:], payload)
				err = s.apply.push([]queuedControlFrame{{data: frame}})
				if err == nil {
					continue
				}
			}
			select {
			case reads <- meshControlRead{msgType: msgType, err: err}:
			case <-readCtx.Done():
				return
			case <-s.conn.Context().Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		cancelRead()
		cancelApply()
		s.control.CancelRead(meshStreamError)
		<-readDone
		<-applyDone
	}()

	nextHeartbeat := startedAt.Add(heartbeatInterval)
	heartbeatTimer := time.NewTimer(heartbeatInterval)
	defer heartbeatTimer.Stop()
	health := time.NewTimer(healthTimeout)
	defer health.Stop()
	lastHealthy := startedAt
	resetHealth := func() {
		if !health.Stop() {
			select {
			case <-health.C:
			default:
			}
		}
		health.Reset(healthTimeout)
		lastHealthy = time.Now()
	}
	handleRead := func(result meshControlRead) error {
		if result.err != nil {
			return fmt.Errorf("read mesh control: %w", result.err)
		}
		if result.msgType != protocol.MsgTypeHeartbeat {
			return fmt.Errorf("unexpected mesh control message 0x%02x", result.msgType)
		}
		resetHealth()
		if time.Since(startedAt) >= s.stableGrace {
			s.stable.Store(true)
		}
		return nil
	}
	writeHeartbeat := func() error {
		now := time.Now()
		deadline := minTime(now.Add(heartbeatInterval), lastHealthy.Add(healthTimeout))
		if err := s.control.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("set mesh heartbeat deadline: %w", err)
		}
		write := protocol.WriteHeartbeat
		if injected := s.heartbeatWrite.Load(); injected != nil {
			write = *injected
		}
		if err := write(s.control, now.Unix()); err != nil {
			return fmt.Errorf("write mesh heartbeat: %w", err)
		}
		if time.Now().After(deadline) {
			return errors.New("mesh heartbeat missed its write deadline")
		}
		nextHeartbeat = time.Now().Add(heartbeatInterval)
		return nil
	}

	for {
		select {
		case result := <-reads:
			if err := handleRead(result); err != nil {
				return err
			}
			continue
		case err := <-applyErrors:
			return fmt.Errorf("apply mesh control: %w", err)
		default:
		}
		if !time.Now().Before(lastHealthy.Add(healthTimeout)) {
			return errors.New("mesh heartbeat timeout")
		}
		if !time.Now().Before(nextHeartbeat) {
			if err := writeHeartbeat(); err != nil {
				return err
			}
			continue
		}
		var changed <-chan struct{}
		if s.outbound != nil {
			frame, hasFrame, failed, signal := s.outbound.peek()
			if failed {
				return errMeshControlQueueFull
			}
			if hasFrame {
				deadline := meshDataWriteDeadline(time.Now(), nextHeartbeat, lastHealthy, heartbeatInterval, healthTimeout)
				if err := s.control.SetWriteDeadline(deadline); err != nil {
					return fmt.Errorf("set mesh control frame deadline: %w", err)
				}
				write := func(w io.Writer, data []byte) (int, error) { return w.Write(data) }
				if injected := s.controlWrite.Load(); injected != nil {
					write = *injected
				}
				n, err := write(s.control, frame.data)
				if err != nil {
					return fmt.Errorf("write mesh control frame: %w", err)
				}
				if n != len(frame.data) {
					return fmt.Errorf("write mesh control frame: %w", io.ErrShortWrite)
				}
				if time.Now().After(deadline) {
					return errors.New("mesh control frame missed heartbeat deadline")
				}
				s.outbound.done(frame)
				continue
			}
			changed = signal
		}
		heartbeatTimer.Reset(time.Until(nextHeartbeat))
		select {
		case <-s.ctx.Done():
			return context.Cause(s.ctx)
		case <-s.conn.Context().Done():
			return context.Cause(s.conn.Context())
		case <-health.C:
			return errors.New("mesh heartbeat timeout")
		case <-heartbeatTimer.C:
			if err := writeHeartbeat(); err != nil {
				return err
			}
		case result := <-reads:
			if err := handleRead(result); err != nil {
				return err
			}
		case err := <-applyErrors:
			return fmt.Errorf("apply mesh control: %w", err)
		case <-changed:
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func meshDataWriteDeadline(now, nextHeartbeat, lastHealthy time.Time, heartbeatInterval, healthTimeout time.Duration) time.Time {
	return minTime(minTime(nextHeartbeat, lastHealthy.Add(healthTimeout)), now.Add(heartbeatInterval))
}

func registryOwner(owner *outbound.Owner) Owner {
	return Owner{Stop: owner.Stop, Wait: owner.Wait}
}

func installConnectionOwner(owner *outbound.Owner, conn *quic.Conn) {
	owner.SetResource(func() {
		_ = conn.CloseWithError(meshApplicationError, "mesh generation stopped")
	}, nil)
}

func installSessionOwner(owner *outbound.Owner, session *Session) {
	owner.SetResource(session.Close, func() {
		if session.ControlStarted() {
			session.Wait()
		}
	})
}

func defaultStableGrace() time.Duration { return outbound.ReconnectStableGrace }
