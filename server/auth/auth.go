package auth

import (
	"context"

	"github.com/quic-go/quic-go"
)

type Auth interface {
	VerifyConn(ctx context.Context, conn *quic.Conn) (valid bool, err error)
}
