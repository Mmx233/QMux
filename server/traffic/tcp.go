package traffic

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/connid"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/quic-go/quic-go"
)

const (
	trafficStreamCancelCode quic.StreamErrorCode = 0
	tcpSetupTimeout                              = 5 * time.Second
	maxPendingTCPSetups                          = 128
)

var errNoTCPStreamCapacity = errors.New("no TCP stream capacity")

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
		return false, fmt.Errorf("invalid NewConn write count %d for %d-byte frame", n, size)
	}
	if n == size {
		return false, err
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
		setupDeadline := time.Now().Add(tcpSetupTimeout)
		releaseSetup, ok := acquireTCPSetup(l.tcpSetupSlots)
		if !ok {
			_ = conn.Close()
			continue
		}

		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetReadBuffer(512 * 1024)
			_ = tc.SetWriteBuffer(512 * 1024)
		}

		l.handlerWG.Go(func() {
			l.handleTCPConnection(conn, setupDeadline, releaseSetup)
		})
	}
}

func acquireTCPSetup(slots chan struct{}) (func(), bool) {
	select {
	case slots <- struct{}{}:
		return sync.OnceFunc(func() { <-slots }), true
	default:
		return nil, false
	}
}

func (l *Listener) addTCPFlow(conn net.Conn) (*tcpFlow, bool) {
	flow := &tcpFlow{conn: conn}
	l.flowsMu.Lock()
	if l.flowsClosing {
		l.flowsMu.Unlock()
		flow.abort()
		return nil, false
	}
	l.flows[flow] = struct{}{}
	l.flowsMu.Unlock()
	return flow, true
}

func (l *Listener) removeTCPFlow(flow *tcpFlow) {
	l.flowsMu.Lock()
	delete(l.flows, flow)
	l.flowsMu.Unlock()
}

// handleTCPConnection handles a single TCP connection.
func (l *Listener) handleTCPConnection(conn net.Conn, setupDeadline time.Time, releaseSetup func()) {
	defer releaseSetup()
	flow, ok := l.addTCPFlow(conn)
	if !ok {
		return
	}
	defer l.removeTCPFlow(flow)
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
		logger.Error().Err(err).Msg("no available client")
		return
	}
	var currentLease *pool.TCPLease
	defer func() {
		if currentLease != nil {
			currentLease.Release()
		}
	}()

	var (
		attempts int
		client   *pool.ClientConn
		stream   *quic.Stream
	)
	for {
		if l.ctx.Err() != nil || flow.isAborted() {
			logger.Debug().Int("attempts", attempts).Msg("TCP setup interrupted by shutdown")
			return
		}
		if !time.Now().Before(setupDeadline) {
			logger.Debug().Err(os.ErrDeadlineExceeded).Int("attempts", attempts).Msg("TCP setup deadline reached")
			return
		}
		currentLease, err = admission.Next()
		if err != nil {
			logger.Error().Err(err).Int("attempts", attempts).Msg("select TCP client failed")
			return
		}
		if currentLease == nil {
			logger.Debug().Err(errNoTCPStreamCapacity).Int("attempts", attempts).Msg("TCP setup rejected")
			return
		}
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
				continue
			}
			localAbort := l.ctx.Err() != nil || flow.isAborted()
			clientLogger.Error().Err(err).Int("attempts", attempts).Msg("open stream failed")
			if localAbort {
				return
			}
			continue
		}
		if !flow.setStream(stream) {
			flow.abort()
			return
		}
		if err := stream.SetWriteDeadline(setupDeadline); err != nil {
			clientLogger.Error().Err(err).Int("attempts", attempts).Msg("set NewConn write deadline failed")
			flow.abort()
			return
		}

		n, writeErr := stream.Write(newConnFrame)
		retry, writeErr := newConnWriteResult(n, len(newConnFrame), writeErr)
		if writeErr != nil {
			localAbort := l.ctx.Err() != nil || flow.isAborted()
			var streamErr *quic.StreamError
			switch {
			case localAbort:
				clientLogger.Debug().Err(writeErr).Int("attempts", attempts).Msg("NewConn write interrupted by shutdown")
			case errors.Is(writeErr, os.ErrDeadlineExceeded):
				clientLogger.Error().Err(writeErr).Int("attempts", attempts).Msg("NewConn write deadline reached")
			case errors.As(writeErr, &streamErr):
				clientLogger.Debug().Err(writeErr).Int("attempts", attempts).Msg("NewConn stream canceled")
			default:
				clientLogger.Error().Err(writeErr).Int("attempts", attempts).Msg("send NewConn message failed")
			}
		}
		if !retry {
			if writeErr != nil {
				flow.abort()
				return
			}
			logger = clientLogger
			break
		}
		if !flow.detachStream(stream) {
			flow.abort()
			return
		}
		stream.CancelRead(trafficStreamCancelCode)
		stream.CancelWrite(trafficStreamCancelCode)
		if !currentLease.Release() {
			clientLogger.Error().Int("attempts", attempts).Msg("release TCP admission lease failed")
			flow.abort()
			return
		}
		currentLease = nil
		stream = nil
	}
	if err := stream.SetWriteDeadline(time.Time{}); err != nil {
		logger.Error().Err(err).Int("attempts", attempts).Msg("clear NewConn write deadline failed")
		flow.abort()
		return
	}
	if !flow.commitStream(stream) {
		flow.abort()
		return
	}
	if !currentLease.Commit() {
		logger.Error().Int("attempts", attempts).Msg("commit TCP admission lease failed")
		flow.abort()
		return
	}
	releaseSetup()
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
