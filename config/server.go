package config

import (
	"time"

	"gopkg.in/yaml.v3"
)

type Server struct {
	Traffic Traffic    `yaml:"traffic"`
	Quic    ServerQuic `yaml:"quic"`
	Auth    ServerAuth `yaml:"auth"`
	TLS     ServerTLS  `yaml:"tls"`
}

type Traffic struct {
	Listen `yaml:",inline"`
	TCP    bool `yaml:"tcp"`
	UDP    bool `yaml:"udp"`
}

type ServerQuic struct {
	Listen `yaml:",inline"`
	Quic   `yaml:",inline"`
}

type ServerAuth struct {
	Method  string    `yaml:"method"`
	Content yaml.Node `yaml:",inline"`
	// todo support mtls or custom challenge-response auth
	// todo 0rtt on mtls
}

type ServerTLS struct {
	// todo implement rotation
	SessionTicketEncryptionKeyRotationInterval time.Duration `yaml:"session_ticket_encryption_key_rotation_interval"`
	SessionTicketEncryptionKeyRotationOverlap  uint8         `yaml:"session_ticket_encryption_key_rotation_overlap"`
}
