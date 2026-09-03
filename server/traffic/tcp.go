package traffic

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/connid"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/quic-go/quic-go"
)

const (
	trafficStreamCancelCode quic.StreamErrorCode = 0
	tcpSetupTimeout                              = 5 * time.Second
)

var errNoTCPStreamCapacity = errors.New("no TCP stream capacity")

// TCPAdmissionSnapshot is a value-only view of one listener's TCP setup and
// relay admission state. The six current and high-water fields share one exact
// locked cut. Cumulative fields are independent atomic samples and are not
// collectively linearizable with each other or the gauges while traffic is
// moving; gauges and cumulative totals are exact when quiescent.
type TCPAdmissionSnapshot struct {
	FlowLimit                    int64
	FlowCurrent                  int64
	FlowHighWater                int64
	SetupLimit                   int64
	SetupCurrent                 int64
	SetupHighWater               int64
	ActiveCurrent                int64
	ActiveHighWater              int64
	Attempts                     uint64
	Retries                      uint64
	StreamLimitAttempts          uint64
	AckFailureAttempts           uint64
	Committed                    uint64
	FlowCapacity                 uint64
	ListenerCapacity             uint64
	Unavailable                  uint64
	GenerationConnectionCapacity uint64
	GenerationSetupCapacity      uint64
	PeerStreamLimit              uint64
	Deadline                     uint64
	SetupFailure                 uint64
	Canceled                     uint64
}

type tcpTerminalResult uint8

const (
	tcpTerminalCommitted tcpTerminalResult = iota
	tcpTerminalFlowCapacity
	tcpTerminalListenerCapacity
	tcpTerminalUnavailable
	tcpTerminalGenerationConnectionCapacity
	tcpTerminalGenerationSetupCapacity
	tcpTerminalPeerStreamLimit
	tcpTerminalDeadline
	tcpTerminalSetupFailure
	tcpTerminalCanceled
)

type tcpAdmissionStats struct {
	mu                           sync.Mutex
	flowCurrent                  int64
	flowHighWater                int64
	setupCurrent                 int64
	setupHighWater               int64
	activeCurrent                int64
	activeHighWater              int64
	attempts                     atomic.Uint64
	retries                      atomic.Uint64
	streamLimitAttempts          atomic.Uint64
	ackFailureAttempts           atomic.Uint64
	committed                    atomic.Uint64
	flowCapacity                 atomic.Uint64
	listenerCapacity             atomic.Uint64
	unavailable                  atomic.Uint64
	generationConnectionCapacity atomic.Uint64
	generationSetupCapacity      atomic.Uint64
	peerStreamLimit              atomic.Uint64
	deadline                     atomic.Uint64
	setupFailure                 atomic.Uint64
	canceled                     atomic.Uint64
}

func (s *tcpAdmissionStats) snapshot() TCPAdmissionSnapshot {
	s.mu.Lock()
	snapshot := TCPAdmissionSnapshot{
		FlowCurrent:     s.flowCurrent,
		FlowHighWater:   s.flowHighWater,
		SetupCurrent:    s.setupCurrent,
		SetupHighWater:  s.setupHighWater,
		ActiveCurrent:   s.activeCurrent,
		ActiveHighWater: s.activeHighWater,
	}
	s.mu.Unlock()
	snapshot.Attempts = s.attempts.Load()
	snapshot.Retries = s.retries.Load()
	snapshot.StreamLimitAttempts = s.streamLimitAttempts.Load()
	snapshot.AckFailureAttempts = s.ackFailureAttempts.Load()
	snapshot.Committed = s.committed.Load()
	snapshot.FlowCapacity = s.flowCapacity.Load()
	snapshot.ListenerCapacity = s.listenerCapacity.Load()
	snapshot.Unavailable = s.unavailable.Load()
	snapshot.GenerationConnectionCapacity = s.generationConnectionCapacity.Load()
	snapshot.GenerationSetupCapacity = s.generationSetupCapacity.Load()
	snapshot.PeerStreamLimit = s.peerStreamLimit.Load()
	snapshot.Deadline = s.deadline.Load()
	snapshot.SetupFailure = s.setupFailure.Load()
	snapshot.Canceled = s.canceled.Load()
	return snapshot
}

func (s *tcpAdmissionStats) finishActive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeCurrent--
}

func (s *tcpAdmissionStats) finish(result tcpTerminalResult) {
	switch result {
	case tcpTerminalCommitted:
		s.committed.Add(1)
	case tcpTerminalFlowCapacity:
		s.flowCapacity.Add(1)
	case tcpTerminalListenerCapacity:
		s.listenerCapacity.Add(1)
	case tcpTerminalUnavailable:
		s.unavailable.Add(1)
	case tcpTerminalGenerationConnectionCapacity:
		s.generationConnectionCapacity.Add(1)
	case tcpTerminalGenerationSetupCapacity:
		s.generationSetupCapacity.Add(1)
	case tcpTerminalPeerStreamLimit:
		s.peerStreamLimit.Add(1)
	case tcpTerminalDeadline:
		s.deadline.Add(1)
	case tcpTerminalSetupFailure:
		s.setupFailure.Add(1)
	case tcpTerminalCanceled:
		s.canceled.Add(1)
	}
}

type tcpSetupPermit struct {
	listener *Listener
	once     sync.Once
}

func (p *tcpSetupPermit) release() {
	p.once.Do(func() {
		stats := &p.listener.tcpAdmission
		stats.mu.Lock()
		<-p.listener.tcpSetupSlots
		stats.setupCurrent--
		stats.mu.Unlock()
	})
}

func (p *tcpSetupPermit) activate() {
	p.once.Do(func() {
		stats := &p.listener.tcpAdmission
		stats.mu.Lock()
		<-p.listener.tcpSetupSlots
		stats.setupCurrent--
		stats.activeCurrent++
		stats.activeHighWater = max(stats.activeHighWater, stats.activeCurrent)
		stats.mu.Unlock()
	})
}

func (l *Listener) tcpAdmissionSnapshot() TCPAdmissionSnapshot {
	snapshot := l.tcpAdmission.snapshot()
	snapshot.FlowLimit = int64(l.tcpFlowLimit)
	snapshot.SetupLimit = int64(l.tcpSetupLimit)
	return snapshot
}

func (s *tcpAdmissionStats) attempt(retry bool) {
	s.attempts.Add(1)
	if retry {
		s.retries.Add(1)
	}
}

func (s *tcpAdmissionStats) streamLimitAttempt() {
	s.streamLimitAttempts.Add(1)
}

func (s *tcpAdmissionStats) ackFailureAttempt() {
	s.ackFailureAttempts.Add(1)
}

type tcpFlow struct {
	conn net.Conn

	mu      sync.Mutex
	aborted bool
	stream  *quic.Stream
}

func (f *tcpFlow) setStream(stream *quic.Stream) bool {
	f.mu.Lock()
	if !f.aborted && f.stream == nil {
		f.stream = stream
		f.mu.Unlock()
		return true
	}
	f.mu.Unlock()

	stream.CancelRead(trafficStreamCancelCode)
	stream.CancelWrite(trafficStreamCancelCode)
	return false
}

func (f *tcpFlow) detachStream(stream *quic.Stream) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aborted || f.stream != stream {
		return false
	}
	f.stream = nil
	return true
}

func (f *tcpFlow) isAborted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aborted
}

func (f *tcpFlow) commitStream(stream *quic.Stream) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.aborted && f.stream == stream
}

func newConnWriteResult(n, size int, err error) (bool, error) {
	if n < 0 || n > size {
		return true, fmt.Errorf("invalid NewConn write count %d for %d-byte frame", n, size)
	}
	if n == size && err == nil {
		return false, nil
	}
	if err == nil {
		err = io.ErrShortWrite
	}
	return true, err
}

// abort forcefully tears down both relay directions after a relay failure or
// listener shutdown.
func (f *tcpFlow) abort() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aborted {
		return
	}
	f.aborted = true
	_ = f.conn.Close()
	stream := f.stream
	if stream != nil {
		stream.CancelRead(trafficStreamCancelCode)
		stream.CancelWrite(trafficStreamCancelCode)
	}
}

// closeSendGracefully serializes FIN with a concurrent Manager abort. Once an
// abort starts, it owns stream teardown and reliable delivery is intentionally
// abandoned so shutdown cannot block on flow control.
func (f *tcpFlow) closeSendGracefully() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aborted || f.stream == nil {
		return nil
	}
	return f.stream.Close()
}

func (f *tcpFlow) closeTCPWriteGracefully() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aborted {
		return nil
	}
	conn, ok := f.conn.(interface{ CloseWrite() error })
	if !ok {
		return fmt.Errorf("connection does not support TCP CloseWrite")
	}
	return conn.CloseWrite()
}

// bindTCP stages a TCP socket without starting its accept loop.
func (l *Listener) bindTCP() error {
	lc := net.ListenConfig{Control: setSocketOptions}
	listener, err := lc.Listen(l.ctx, "tcp", l.Addr)
	if err != nil {
		return fmt.Errorf("listen TCP: %w", err)
	}
	l.TCPListener = listener
	return nil
}

// acceptTCP accepts TCP connections.
func (l *Listener) acceptTCP() {
	defer l.fixedWG.Done()
	l.logger.Info().Str("protocol", "tcp").Msg("TCP listener started")

	for {
		conn, err := l.TCPListener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-l.ctx.Done():
				return
			default:
				l.logger.Error().Err(err).Msg("accept TCP connection failed")
				continue
			}
		}
		flow, ok := l.addTCPFlow(conn)
		if !ok {
			continue
		}
		setupDeadline := time.Now().Add(tcpSetupTimeout)
		setupPermit, ok := l.acquireTCPSetup()
		if !ok {
			l.tcpAdmission.finish(tcpTerminalListenerCapacity)
			_ = conn.Close()
			l.removeTCPFlow(flow)
			continue
		}

		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetReadBuffer(512 * 1024)
			_ = tc.SetWriteBuffer(512 * 1024)
		}

		l.handlerWG.Go(func() {
			l.handleTCPConnection(flow, setupDeadline, setupPermit)
		})
	}
}

func (l *Listener) acquireTCPSetup() (*tcpSetupPermit, bool) {
	stats := &l.tcpAdmission
	stats.mu.Lock()
	defer stats.mu.Unlock()
	select {
	case l.tcpSetupSlots <- struct{}{}:
		stats.setupCurrent++
		stats.setupHighWater = max(stats.setupHighWater, stats.setupCurrent)
		return &tcpSetupPermit{listener: l}, true
	default:
		return nil, false
	}
}

func (l *Listener) addTCPFlow(conn net.Conn) (*tcpFlow, bool) {
	flow := &tcpFlow{conn: conn}
	stats := &l.tcpAdmission
	stats.mu.Lock()
	defer stats.mu.Unlock()
	l.flowsMu.Lock()
	defer l.flowsMu.Unlock()
	if l.flowsClosing {
		flow.abort()
		return nil, false
	}
	if len(l.flows) >= l.tcpFlowLimit {
		stats.finish(tcpTerminalFlowCapacity)
		flow.abort()
		return nil, false
	}
	l.flows[flow] = struct{}{}
	stats.flowCurrent++
	stats.flowHighWater = max(stats.flowHighWater, stats.flowCurrent)
	return flow, true
}

func (l *Listener) removeTCPFlow(flow *tcpFlow) {
	stats := &l.tcpAdmission
	stats.mu.Lock()
	defer stats.mu.Unlock()
	l.flowsMu.Lock()
	defer l.flowsMu.Unlock()
	if _, ok := l.flows[flow]; ok {
		delete(l.flows, flow)
		stats.flowCurrent--
	}
}

// handleTCPConnection handles a single TCP connection.
func (l *Listener) handleTCPConnection(flow *tcpFlow, setupDeadline time.Time, setupPermit *tcpSetupPermit) {
	conn := flow.conn
	defer l.removeTCPFlow(flow)
	terminal := tcpTerminalSetupFailure
	finishTerminal := sync.OnceFunc(func() { l.tcpAdmission.finish(terminal) })
	defer finishTerminal()
	defer setupPermit.release()
	defer func() { _ = conn.Close() }()

	logger := l.logger.With().
		Str("remote", conn.RemoteAddr().String()).
		Str("protocol", "tcp").
		Logger()

	logger.Debug().Msg("new TCP connection")
	connID := connid.Generate()
	var frame bytes.Buffer
	if err := protocol.WriteNewConn(
		&frame,
		connID,
		"tcp",
		conn.RemoteAddr().String(),
		l.Addr,
		time.Now().Unix(),
	); err != nil {
		logger.Error().Err(err).Msg("encode NewConn message failed")
		return
	}
	newConnFrame := frame.Bytes()

	admission, err := l.Pool.BeginTCPAdmission()
	if err != nil {
		if errors.Is(err, pool.ErrNoClientsAvailable) ||
			errors.Is(err, pool.ErrNoHealthyClients) ||
			errors.Is(err, pool.ErrNoEligibleClients) {
			terminal = tcpTerminalUnavailable
		}
		logger.Error().Err(err).Msg("no available client")
		return
	}
	var currentLease *pool.TCPLease
	active := false
	defer func() {
		if currentLease != nil {
			currentLease.Release()
		}
		if active {
			l.tcpAdmission.finishActive()
		}
	}()

	var (
		attempts      int
		setupFailed   bool
		streamLimited bool
		client        *pool.ClientConn
		stream        *quic.Stream
	)
	for {
		if l.ctx.Err() != nil || flow.isAborted() {
			terminal = tcpTerminalCanceled
			logger.Debug().Int("attempts", attempts).Msg("TCP setup interrupted by shutdown")
			return
		}
		if !time.Now().Before(setupDeadline) {
			terminal = tcpTerminalDeadline
			logger.Debug().Err(os.ErrDeadlineExceeded).Int("attempts", attempts).Msg("TCP setup deadline reached")
			return
		}
		currentLease, err = admission.Next()
		if err != nil {
			switch {
			case errors.Is(err, pool.ErrTCPGenerationConnectionCapacity):
				terminal = tcpTerminalGenerationConnectionCapacity
			case errors.Is(err, pool.ErrTCPGenerationSetupCapacity):
				terminal = tcpTerminalGenerationSetupCapacity
			}
			logger.Error().Err(err).Int("attempts", attempts).Msg("select TCP client failed")
			return
		}
		if currentLease == nil {
			switch {
			case setupFailed:
				terminal = tcpTerminalSetupFailure
			case streamLimited:
				terminal = tcpTerminalPeerStreamLimit
			default:
				terminal = tcpTerminalUnavailable
			}
			logger.Debug().Err(errNoTCPStreamCapacity).Int("attempts", attempts).Msg("TCP setup rejected")
			return
		}
		l.tcpAdmission.attempt(attempts > 0)
		attempts++
		client = currentLease.Client()
		clientLogger := logger.With().Str("client_id", client.ID).Logger()
		clientLogger.Debug().Msg("selected client")

		stream, err = client.Conn.OpenStream()
		if err != nil {
			if !currentLease.Release() {
				clientLogger.Error().Int("attempts", attempts).Msg("release TCP admission lease failed")
				flow.abort()
				return
			}
			currentLease = nil
			stream = nil

			if _, ok := errors.AsType[*quic.StreamLimitReachedError](err); ok {
				l.tcpAdmission.streamLimitAttempt()
				streamLimited = true
				continue
			}
			localAbort := l.ctx.Err() != nil || flow.isAborted()
			clientLogger.Error().Err(err).Int("attempts", attempts).Msg("open stream failed")
			if localAbort {
				terminal = tcpTerminalCanceled
				return
			}
			setupFailed = true
			continue
		}
		currentLease.RecordStream(int64(stream.StreamID()))
		if !flow.setStream(stream) {
			terminal = tcpTerminalCanceled
			flow.abort()
			return
		}
		abandonAttempt := func() bool {
			if !flow.detachStream(stream) {
				if l.ctx.Err() != nil || flow.isAborted() {
					terminal = tcpTerminalCanceled
				}
				flow.abort()
				return false
			}
			stream.CancelRead(trafficStreamCancelCode)
			stream.CancelWrite(trafficStreamCancelCode)
			if !currentLease.Release() {
				clientLogger.Error().Int("attempts", attempts).Msg("release TCP admission lease failed")
				flow.abort()
				return false
			}
			currentLease = nil
			stream = nil
			return true
		}
		if err := stream.SetDeadline(setupDeadline); err != nil {
			if l.ctx.Err() != nil || flow.isAborted() {
				terminal = tcpTerminalCanceled
				flow.abort()
				return
			}
			if errors.Is(err, os.ErrDeadlineExceeded) || !time.Now().Before(setupDeadline) {
				terminal = tcpTerminalDeadline
				flow.abort()
				return
			}
			setupFailed = true
			clientLogger.Error().Err(err).Int("attempts", attempts).Msg("set TCP setup deadline failed")
			if !abandonAttempt() {
				return
			}
			continue
		}

		n, writeErr := stream.Write(newConnFrame)
		retry, writeErr := newConnWriteResult(n, len(newConnFrame), writeErr)
		if writeErr != nil {
			localAbort := l.ctx.Err() != nil || flow.isAborted()
			switch {
			case localAbort:
				terminal = tcpTerminalCanceled
				clientLogger.Debug().Err(writeErr).Int("attempts", attempts).Msg("NewConn write interrupted by shutdown")
				flow.abort()
				return
			case errors.Is(writeErr, os.ErrDeadlineExceeded) || !time.Now().Before(setupDeadline):
				terminal = tcpTerminalDeadline
				clientLogger.Error().Err(writeErr).Int("attempts", attempts).Msg("NewConn write deadline reached")
				flow.abort()
				return
			default:
				setupFailed = true
				clientLogger.Error().Err(writeErr).Int("attempts", attempts).Msg("send NewConn message failed")
			}
			if !retry {
				flow.abort()
				return
			}
			if !abandonAttempt() {
				return
			}
			continue
		}

		// ponytail: one stalled ACK can consume the 5s setup budget; add per-attempt budgeting only if production evidence shows it blocks useful failover.
		var ack protocol.NewConnAckMsg
		ackErr := protocol.ReadTypedMessageLimited(stream, protocol.MsgTypeNewConnAck, &ack, protocol.MaxNewConnAckPayloadSize)
		if ackErr == nil && (ack.ConnID == 0 || ack.ConnID != connID) {
			ackErr = fmt.Errorf("invalid NewConn acknowledgment ID %d, want %d", ack.ConnID, connID)
		}
		if ackErr != nil {
			if l.ctx.Err() != nil || flow.isAborted() {
				terminal = tcpTerminalCanceled
				flow.abort()
				return
			}
			l.tcpAdmission.ackFailureAttempt()
			if errors.Is(ackErr, os.ErrDeadlineExceeded) || !time.Now().Before(setupDeadline) {
				terminal = tcpTerminalDeadline
				clientLogger.Error().Err(ackErr).Int("attempts", attempts).Msg("NewConn acknowledgment deadline reached")
				flow.abort()
				return
			}
			setupFailed = true
			clientLogger.Debug().Err(ackErr).Int("attempts", attempts).Msg("read NewConn acknowledgment failed")
			if !abandonAttempt() {
				return
			}
			continue
		}
		if err := stream.SetDeadline(time.Time{}); err != nil {
			if l.ctx.Err() != nil || flow.isAborted() {
				terminal = tcpTerminalCanceled
				flow.abort()
				return
			}
			if errors.Is(err, os.ErrDeadlineExceeded) || !time.Now().Before(setupDeadline) {
				terminal = tcpTerminalDeadline
				flow.abort()
				return
			}
			setupFailed = true
			clientLogger.Error().Err(err).Int("attempts", attempts).Msg("clear TCP setup deadline failed")
			if !abandonAttempt() {
				return
			}
			continue
		}
		logger = clientLogger
		break
	}
	if !flow.commitStream(stream) {
		if l.ctx.Err() != nil || flow.isAborted() {
			terminal = tcpTerminalCanceled
		}
		flow.abort()
		return
	}
	if !currentLease.Commit() {
		logger.Error().Int("attempts", attempts).Msg("commit TCP admission lease failed")
		flow.abort()
		return
	}
	setupPermit.activate()
	active = true
	terminal = tcpTerminalCommitted
	finishTerminal()
	defer func() { _ = flow.closeSendGracefully() }()

	logger.Debug().Uint64("conn_id", connID).Int("attempts", attempts).Msg("forwarding connection")

	relay := protocol.StartRelay(conn, stream,
		func(err error) error {
			if err != nil {
				flow.abort()
				return nil
			}
			if err = flow.closeSendGracefully(); err != nil {
				flow.abort()
			}
			return err
		},
		func(err error) error {
			if err != nil {
				flow.abort()
				return nil
			}
			if err = flow.closeTCPWriteGracefully(); err != nil {
				flow.abort()
			}
			return err
		},
	)
	err = relay.Wait()

	if err != nil {
		logger.Debug().Err(err).Msg("connection closed with error")
	} else {
		logger.Debug().Msg("connection closed")
	}
}
