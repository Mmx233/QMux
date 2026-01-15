package challenge

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Mmx233/QMux/server/auth"
	"github.com/quic-go/quic-go"
)

const (
	ChallengeSize = 32               // 256 bits
	ResponseSize  = 64               // SHA512 output size
	AuthTimeout   = 10 * time.Second // Maximum time for auth handshake
	MinTokenSize  = 16               // Minimum token length in bytes
)

// ChallengeAuth implements challenge-response authentication using HMAC-SHA512
type ChallengeAuth struct {
	token []byte
}

// Ensure ChallengeAuth implements auth.Auth interface
var _ auth.Auth = (*ChallengeAuth)(nil)

// New creates a new challenge-response authenticator.
// The token must be at least MinTokenSize (16) bytes.
// The token is copied to prevent external modification.
func New(token []byte) (auth.Auth, error) {
	if len(token) < MinTokenSize {
		return nil, errors.New("token must be at least 16 bytes")
	}
	// Copy token to prevent external modification
	tokenCopy := make([]byte, len(token))
	copy(tokenCopy, token)
	return &ChallengeAuth{token: tokenCopy}, nil
}

// GenerateChallenge creates a cryptographically secure random challenge
func GenerateChallenge() ([]byte, error) {
	challenge := make([]byte, ChallengeSize)
	if _, err := rand.Read(challenge); err != nil {
		return nil, fmt.Errorf("generate challenge: %w", err)
	}
	return challenge, nil
}

// ComputeResponse computes HMAC-SHA512(token, challenge)
func ComputeResponse(token, challenge []byte) []byte {
	mac := hmac.New(sha512.New, token)
	mac.Write(challenge)
	return mac.Sum(nil)
}

// VerifyResponse verifies the response using constant-time comparison
func VerifyResponse(token, challenge, response []byte) bool {
	expected := ComputeResponse(token, challenge)
	return hmac.Equal(response, expected)
}

// writeChallenge writes a challenge to the writer using wire format: [4 bytes length][challenge bytes]
func writeChallenge(w io.Writer, challenge []byte) error {
	length := uint32(len(challenge))
	if err := binary.Write(w, binary.BigEndian, length); err != nil {
		return fmt.Errorf("write challenge length: %w", err)
	}
	if _, err := w.Write(challenge); err != nil {
		return fmt.Errorf("write challenge: %w", err)
	}
	return nil
}

// readChallenge reads a challenge from the reader using wire format: [4 bytes length][challenge bytes]
func readChallenge(r io.Reader) ([]byte, error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, fmt.Errorf("read challenge length: %w", err)
	}
	if length != ChallengeSize {
		return nil, fmt.Errorf("invalid challenge size: %d", length)
	}
	challenge := make([]byte, length)
	if _, err := io.ReadFull(r, challenge); err != nil {
		return nil, fmt.Errorf("read challenge: %w", err)
	}
	return challenge, nil
}

// writeResponse writes a response to the writer using wire format: [4 bytes length][response bytes]
func writeResponse(w io.Writer, response []byte) error {
	length := uint32(len(response))
	if err := binary.Write(w, binary.BigEndian, length); err != nil {
		return fmt.Errorf("write response length: %w", err)
	}
	if _, err := w.Write(response); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}

// readResponse reads a response from the reader using wire format: [4 bytes length][response bytes]
func readResponse(r io.Reader) ([]byte, error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, fmt.Errorf("read response length: %w", err)
	}
	if length != ResponseSize {
		return nil, fmt.Errorf("invalid response size: %d", length)
	}
	response := make([]byte, length)
	if _, err := io.ReadFull(r, response); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return response, nil
}

// VerifyConn implements the auth.Auth interface.
// It performs the complete challenge-response authentication flow:
// 1. Opens an auth stream on the QUIC connection
// 2. Generates and sends a random challenge
// 3. Reads and verifies the client's response
func (c *ChallengeAuth) VerifyConn(ctx context.Context, conn *quic.Conn) (bool, error) {
	// Create timeout context
	ctx, cancel := context.WithTimeout(ctx, AuthTimeout)
	defer cancel()

	// Open auth stream
	stream, err := (*conn).OpenStreamSync(ctx)
	if err != nil {
		return false, fmt.Errorf("open auth stream: %w", err)
	}
	defer stream.Close()

	// Generate and send challenge
	challenge, err := GenerateChallenge()
	if err != nil {
		return false, err
	}

	if err := writeChallenge(stream, challenge); err != nil {
		return false, err
	}

	// Read response
	response, err := readResponse(stream)
	if err != nil {
		return false, err
	}

	// Verify response
	if !VerifyResponse(c.token, challenge, response) {
		return false, nil
	}

	return true, nil
}
