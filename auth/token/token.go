// Package token implements the TLS-exporter-bound proof used by QMux token
// authentication.
package token

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha512"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	// Scheme identifies the token proof carried by a registration message.
	Scheme = "qmux-token-ekm-hmac-sha512-v1"
	// ExporterLabel isolates QMux token key material from other TLS exporters.
	ExporterLabel = "EXPERIMENTAL-QMux-token-auth-v1"
	// MinSecretSize is the minimum accepted token length in bytes.
	MinSecretSize = 16
	// ProofSize is the size of an HMAC-SHA512 proof in bytes.
	ProofSize = sha512.Size

	proofDomain    = "QMux token authentication proof"
	exporterLength = 32
)

// Transcript contains the semantic registration fields authenticated by a
// token proof. Capability order is significant.
type Transcript struct {
	ClientID     string
	Version      string
	Capabilities []string
}

// Compute returns the token proof for transcript on a completed TLS 1.3
// connection.
func Compute(secret []byte, transcript Transcript, state tls.ConnectionState) ([]byte, error) {
	if len(secret) < MinSecretSize {
		return nil, fmt.Errorf("token secret must be at least %d bytes", MinSecretSize)
	}
	if !state.HandshakeComplete {
		return nil, errors.New("TLS handshake is not complete")
	}
	if state.Version != tls.VersionTLS13 {
		return nil, fmt.Errorf("token authentication requires TLS 1.3, got 0x%04x", state.Version)
	}

	canonical, err := marshalTranscript(transcript)
	if err != nil {
		return nil, err
	}
	ekm, err := state.ExportKeyingMaterial(ExporterLabel, nil, exporterLength)
	if err != nil {
		return nil, fmt.Errorf("export TLS keying material: %w", err)
	}

	return computeProof(secret, canonical, ekm), nil
}

// Verify checks that proof is the exact token proof for transcript on a
// completed TLS 1.3 connection.
func Verify(secret []byte, transcript Transcript, proof []byte, state tls.ConnectionState) error {
	if len(proof) != ProofSize {
		return fmt.Errorf("invalid token proof size: got %d, require %d", len(proof), ProofSize)
	}
	expected, err := Compute(secret, transcript, state)
	if err != nil {
		return err
	}
	if !hmac.Equal(proof, expected) {
		return errors.New("invalid token proof")
	}
	return nil
}

func marshalTranscript(transcript Transcript) ([]byte, error) {
	fields := []string{proofDomain, Scheme, transcript.ClientID, transcript.Version}
	total := uint64(4) // Capability count.
	for _, field := range fields {
		var err error
		total, err = addEncodedStringSize(total, field)
		if err != nil {
			return nil, err
		}
	}
	if uint64(len(transcript.Capabilities)) > math.MaxUint32 {
		return nil, errors.New("too many capabilities to encode")
	}

	seen := make(map[string]struct{}, len(transcript.Capabilities))
	for _, capability := range transcript.Capabilities {
		if _, exists := seen[capability]; exists {
			return nil, fmt.Errorf("duplicate capability %q", capability)
		}
		seen[capability] = struct{}{}

		var err error
		total, err = addEncodedStringSize(total, capability)
		if err != nil {
			return nil, err
		}
	}
	if total > uint64(maxInt()) {
		return nil, errors.New("token transcript is too large to encode")
	}

	var canonical bytes.Buffer
	canonical.Grow(int(total))
	for _, field := range fields {
		writeString(&canonical, field)
	}
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], uint32(len(transcript.Capabilities)))
	canonical.Write(count[:])
	for _, capability := range transcript.Capabilities {
		writeString(&canonical, capability)
	}
	return canonical.Bytes(), nil
}

func addEncodedStringSize(total uint64, value string) (uint64, error) {
	return addEncodedLength(total, uint64(len(value)))
}

func addEncodedLength(total, length uint64) (uint64, error) {
	if length > math.MaxUint32 {
		return 0, errors.New("token transcript field is too large to encode")
	}
	encodedSize := uint64(4) + length
	if total > math.MaxUint64-encodedSize {
		return 0, errors.New("token transcript is too large to encode")
	}
	return total + encodedSize, nil
}

func writeString(dst *bytes.Buffer, value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	dst.Write(length[:])
	dst.WriteString(value)
}

func computeProof(secret, canonical, ekm []byte) []byte {
	mac := hmac.New(sha512.New, secret)
	_, _ = mac.Write(canonical)
	_, _ = mac.Write(ekm)
	return mac.Sum(nil)
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
