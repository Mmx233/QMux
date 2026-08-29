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

	// FragmentAssemblerShards is the number of shards for the fragment assembler.
	// Higher values reduce lock contention but use more memory.
	// Default: 16
	FragmentAssemblerShards int `yaml:"fragment_assembler_shards"`

	// EnableBufferPooling enables buffer pooling for UDP operations.
	// Default: true
	EnableBufferPooling *bool `yaml:"enable_buffer_pooling"`

	// ReadBufferSize is the size of the UDP read buffer in bytes.
	// This should be large enough to receive any UDP packet.
	// Default: 65535 (maximum UDP packet size)
	ReadBufferSize int `yaml:"read_buffer_size"`

	// DatagramBufferSize is the size of QUIC datagram buffers in bytes.
	// This is typically set to the QUIC max datagram size.
	// Default: 1200
	DatagramBufferSize int `yaml:"datagram_buffer_size"`
}

// IsFragmentationEnabled returns whether UDP fragmentation is enabled.
// Defaults to true if not explicitly set.
func (u *UDPConfig) IsFragmentationEnabled() bool {
	if u.EnableFragmentation == nil {
		return true // Default enabled
	}
	return *u.EnableFragmentation
}

// GetFragmentAssemblerShards returns the configured shard count or default
func (u *UDPConfig) GetFragmentAssemblerShards() int {
	if u.FragmentAssemblerShards <= 0 {
		return 16
	}
	return u.FragmentAssemblerShards
}

// IsBufferPoolingEnabled returns whether buffer pooling is enabled
func (u *UDPConfig) IsBufferPoolingEnabled() bool {
	if u.EnableBufferPooling == nil {
		return true // Default enabled
	}
	return *u.EnableBufferPooling
}

// GetReadBufferSize returns the configured read buffer size or default
func (u *UDPConfig) GetReadBufferSize() int {
	if u.ReadBufferSize <= 0 {
		return DefaultReadBufferSize
	}
	return u.ReadBufferSize
}

// GetDatagramBufferSize returns the configured datagram buffer size or default
func (u *UDPConfig) GetDatagramBufferSize() int {
	if u.DatagramBufferSize <= 0 {
		return DefaultDatagramBufferSize
	}
	return u.DatagramBufferSize
}

// GetFragmentBufferSize returns the fragment buffer size calculated from datagram size.
// Fragment buffer = datagram size - 9 bytes (fragment header)
func (u *UDPConfig) GetFragmentBufferSize() int {
	return u.GetDatagramBufferSize() - 9
}
