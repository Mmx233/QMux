package traffic

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/connid"
)

// startTCP starts the TCP listener
func (l *Listener) startTCP() error {
	lc := net.ListenConfig{
		Control: setSocketOptions,
	}
	listener, err := lc.Listen(l.ctx, "tcp", fmt.Sprintf(":%d", l.Port))
	if err != nil {
		return fmt.Errorf("listen TCP: %w", err)
	}

	l.TCPListener = listener
	l.logger.Info().Str("protocol", "tcp").Msg("TCP listener started")

	go l.acceptTCP()
	return nil
}

// acceptTCP accepts TCP connections
func (l *Listener) acceptTCP() {
	for {
		conn, err := l.TCPListener.Accept()
		if err != nil {
			select {
			case <-l.ctx.Done():
				return
			default:
				l.logger.Error().Err(err).Msg("accept TCP connection failed")
				continue
			}
		}

		// Optimize TCP connection
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetReadBuffer(512 * 1024)
			_ = tc.SetWriteBuffer(512 * 1024)
		}

		go l.handleTCPConnection(conn)
	}
}

// handleTCPConnection handles a single TCP connection
func (l *Listener) handleTCPConnection(conn net.Conn) {
	defer conn.Close()

	logger := l.logger.With().
		Str("remote", conn.RemoteAddr().String()).
		Str("protocol", "tcp").
		Logger()

	logger.Debug().Msg("new TCP connection")

	// Select client from pool
	client, err := l.Pool.Select()
	if err != nil {
		logger.Error().Err(err).Msg("no available client")
		return
	}

	logger = logger.With().Str("client_id", client.ID).Logger()
	logger.Debug().Msg("selected client")

	// Open QUIC stream to client
	stream, err := client.Conn.OpenStreamSync(l.ctx)
	if err != nil {
		logger.Error().Err(err).Msg("open stream failed")
		l.Pool.MarkUnhealthy(client.ID)
		return
	}
	defer stream.Close()

	// Send NewConn message
	connID := connid.Generate()
	err = protocol.WriteNewConn(
		stream,
		connID,
		"tcp",
		conn.RemoteAddr().String(),
		uint16(l.Port),
		time.Now().Unix(),
	)
	if err != nil {
		logger.Error().Err(err).Msg("send NewConn message failed")
		return
	}

	logger.Debug().Uint64("conn_id", connID).Msg("forwarding connection")

	// Track active connection
	client.ActiveConns.Add(1)
	client.TotalConns.Add(1)
	defer client.ActiveConns.Add(-1)

	// Use optimized relay
	err = protocol.Relay(conn, stream)
	if err != nil && !errors.Is(err, io.EOF) {
		logger.Debug().Err(err).Msg("connection closed with error")
	} else {
		logger.Debug().Msg("connection closed")
	}
}
