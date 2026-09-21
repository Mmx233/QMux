package mesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

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

	conn    *quic.Conn
	control *quic.Stream

	ctx            context.Context
	cancel         context.CancelFunc
	closeOnce      sync.Once
	done           chan struct{}
	started        atomic.Bool
	stable         atomic.Bool
	heartbeatWrite atomic.Pointer[func(io.Writer, int64) error]
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
		s.control.CancelRead(meshStreamError)
		s.control.CancelWrite(meshStreamError)
		_ = s.conn.CloseWithError(meshApplicationError, "mesh session closed")
	})
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
	go func() {
		defer close(readDone)
		for {
			msgType, _, err := protocol.ReadMessageLimited(s.control, protocol.MaxControlPayloadSize)
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
		s.control.CancelRead(meshStreamError)
		<-readDone
	}()

	heartbeats := time.NewTicker(heartbeatInterval)
	defer heartbeats.Stop()
	health := time.NewTimer(healthTimeout)
	defer health.Stop()
	resetHealth := func() {
		if !health.Stop() {
			select {
			case <-health.C:
			default:
			}
		}
		health.Reset(healthTimeout)
	}

	for {
		select {
		case <-s.ctx.Done():
			return context.Cause(s.ctx)
		case <-s.conn.Context().Done():
			return context.Cause(s.conn.Context())
		case <-health.C:
			return errors.New("mesh heartbeat timeout")
		case now := <-heartbeats.C:
			if err := s.control.SetWriteDeadline(now.Add(heartbeatInterval)); err != nil {
				return fmt.Errorf("set mesh heartbeat deadline: %w", err)
			}
			write := protocol.WriteHeartbeat
			if injected := s.heartbeatWrite.Load(); injected != nil {
				write = *injected
			}
			if err := write(s.control, now.Unix()); err != nil {
				return fmt.Errorf("write mesh heartbeat: %w", err)
			}
		case result := <-reads:
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
		}
	}
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
