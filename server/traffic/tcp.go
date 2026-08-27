package traffic

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/connid"
	"github.com/quic-go/quic-go"
)

const trafficStreamCancelCode quic.StreamErrorCode = 0

type tcpFlow struct {
	conn net.Conn

	mu      sync.Mutex
	aborted bool
	stream  *quic.Stream
}

func (f *tcpFlow) setStream(stream *quic.Stream) {
	f.mu.Lock()
	if !f.aborted {
		f.stream = stream
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()

	stream.CancelRead(trafficStreamCancelCode)
	stream.CancelWrite(trafficStreamCancelCode)
}

// abort forcefully tears down both relay directions. It is reserved for
// listener shutdown, where all blocked copy goroutines must be interrupted.
func (f *tcpFlow) abort() {
	_ = f.conn.Close()

	f.mu.Lock()
	f.aborted = true
	stream := f.stream
	if stream != nil {
		stream.CancelRead(trafficStreamCancelCode)
		stream.CancelWrite(trafficStreamCancelCode)
	}
	f.mu.Unlock()
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

		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetReadBuffer(512 * 1024)
			_ = tc.SetWriteBuffer(512 * 1024)
		}

		l.handlerWG.Go(func() {
			l.handleTCPConnection(conn)
		})
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
func (l *Listener) handleTCPConnection(conn net.Conn) {
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

	client, err := l.Pool.Select()
	if err != nil {
		logger.Error().Err(err).Msg("no available client")
		return
	}

	logger = logger.With().Str("client_id", client.ID).Logger()
	logger.Debug().Msg("selected client")

	stream, err := client.Conn.OpenStreamSync(l.ctx)
	if err != nil {
		logger.Error().Err(err).Msg("open stream failed")
		if l.ctx.Err() == nil && !l.Pool.MarkUnhealthy(client) {
			logger.Debug().Msg("ignored stale client stream-open failure")
		}
		return
	}
	flow.setStream(stream)
	defer func() { _ = flow.closeSendGracefully() }()

	connID := connid.Generate()
	err = protocol.WriteNewConn(
		stream,
		connID,
		"tcp",
		conn.RemoteAddr().String(),
		l.Addr,
		time.Now().Unix(),
	)
	if err != nil {
		logger.Error().Err(err).Msg("send NewConn message failed")
		if l.ctx.Err() == nil && !l.Pool.MarkUnhealthy(client) {
			logger.Debug().Msg("ignored stale client NewConn write failure")
		}
		return
	}

	logger.Debug().Uint64("conn_id", connID).Msg("forwarding connection")

	client.ActiveConns.Add(1)
	client.TotalConns.Add(1)
	defer client.ActiveConns.Add(-1)

	relay := protocol.StartRelay(conn, stream)
	err = relay.WaitFirst()
	// Preserve first-completion behavior while allowing data already accepted by
	// the QUIC send side to finish reliably. Manager shutdown may concurrently
	// call flow.abort, which also interrupts a flow-controlled stream.Write.
	_ = conn.Close()
	stream.CancelRead(trafficStreamCancelCode)
	_ = relay.Wait()
	_ = flow.closeSendGracefully()

	if err != nil && !errors.Is(err, io.EOF) {
		logger.Debug().Err(err).Msg("connection closed with error")
	} else {
		logger.Debug().Msg("connection closed")
	}
}
