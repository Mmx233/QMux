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

// Quic is nested under client quic and inlined in each server listener in YAML.
type Quic struct {
	InitialStreamReceiveWindow     uint64        `yaml:"initial_stream_receive_window"`
	MaxStreamReceiveWindow         uint64        `yaml:"max_stream_receive_window"`
	InitialConnectionReceiveWindow uint64        `yaml:"initial_connection_receive_window"`
	MaxConnectionReceiveWindow     uint64        `yaml:"max_connection_receive_window"`
	MaxIncomingStreams             int64         `yaml:"max_incoming_streams"`
	KeepAlivePeriod                time.Duration `yaml:"keep_alive_period"`
	HandshakeIdleTimeout           time.Duration `yaml:"handshake_idle_timeout"`
	MaxIdleTimeout                 time.Duration `yaml:"max_idle_timeout"`
	// Allow0RTT is passed to quic-go; QMux currently does not use 0-RTT application data.
	Allow0RTT bool `yaml:"allow_0rtt"`
}

func (q Quic) GetConfig() *quic.Config {
	if q.MaxIdleTimeout == 0 {
		q.MaxIdleTimeout = DefaultMaxIdleTimeout
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

// UDPConfig contains UDP-specific configuration
type UDPConfig struct {
	// EnableFragmentation enables automatic fragmentation of large UDP packets.
	// When enabled, packets larger than 1195 bytes will be split into multiple
	// QUIC datagrams and reassembled on the other side.
	// Default: true
	EnableFragmentation *bool `yaml:"enable_fragmentation"`
}

// IsFragmentationEnabled returns whether UDP fragmentation is enabled.
// Defaults to true if not explicitly set.
func (u *UDPConfig) IsFragmentationEnabled() bool {
	if u.EnableFragmentation == nil {
		return true // Default enabled
	}
	return *u.EnableFragmentation
}
