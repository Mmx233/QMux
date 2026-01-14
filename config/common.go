package config

import (
	"fmt"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	EnvPrefix = "QMUX_"
)

type Listen struct {
	IP   string `mapstructure:"ip"`
	Port int    `mapstructure:"port"`
}

func (l Listen) GetIP() (net.IP, error) {
	ip := net.ParseIP(l.IP)
	if ip == nil {
		return nil, fmt.Errorf("invalid ip address: %s", l.IP)
	}
	return ip, nil
}

type Quic struct {
	InitialStreamReceiveWindow     uint64
	MaxStreamReceiveWindow         uint64
	InitialConnectionReceiveWindow uint64
	MaxConnectionReceiveWindow     uint64
	MaxIncomingStreams             int64
	KeepAlivePeriod                time.Duration
	HandshakeIdleTimeout           time.Duration
	MaxIdleTimeout                 time.Duration
	Allow0RTT                      bool
}

func (q Quic) GetConfig() *quic.Config {
	if q.MaxIdleTimeout == 0 {
		q.MaxIdleTimeout = time.Minute * 5
	}
	return &quic.Config{
		InitialStreamReceiveWindow:     q.InitialStreamReceiveWindow,
		MaxStreamReceiveWindow:         q.MaxStreamReceiveWindow,
		InitialConnectionReceiveWindow: q.InitialConnectionReceiveWindow,
		MaxConnectionReceiveWindow:     q.MaxConnectionReceiveWindow,
		MaxIncomingStreams:             q.MaxIncomingStreams,
		KeepAlivePeriod:                q.KeepAlivePeriod,
		HandshakeIdleTimeout:           q.HandshakeIdleTimeout,
		MaxIdleTimeout:                 q.MaxIdleTimeout,
		Allow0RTT:                      q.Allow0RTT,
		EnableDatagrams:                true,
	}
}
