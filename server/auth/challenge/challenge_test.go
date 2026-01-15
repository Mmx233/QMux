package challenge

import (
	"bytes"
	"testing"
	"testing/quick"
)

// Property 1: HMAC Determinism - same inputs produce same output
// For any valid token and challenge pair, computing the response multiple times
// SHALL produce identical results.
// **Validates: Requirements 2.2**
func TestProperty_HMACDeterminism(t *testing.T) {
	f := func(token, challenge []byte) bool {
		// Skip empty inputs as they're edge cases
		if len(token) == 0 || len(challenge) == 0 {
			return true
		}

		// Compute response twice with same inputs
		response1 := ComputeResponse(token, challenge)
		response2 := ComputeResponse(token, challenge)

		// Both responses must be identical
		return bytes.Equal(response1, response2)
	}

	config := &quick.Config{MaxCount: 100}
	if err := quick.Check(f, config); err != nil {
		t.Errorf("Property 1 (HMAC Determinism) failed: %v", err)
	}
}

// Property 2: Response Round-Trip - compute then verify succeeds
// For any valid token and challenge, computing the response and then verifying it
// with the same token and challenge SHALL succeed.
// **Validates: Requirements 2.3**
func TestProperty_ResponseRoundTrip(t *testing.T) {
	f := func(token, challenge []byte) bool {
		// Skip empty inputs as they're edge cases
		if len(token) == 0 || len(challenge) == 0 {
			return true
		}

		// Compute response
		response := ComputeResponse(token, challenge)

		// Verify with same token and challenge must succeed
		return VerifyResponse(token, challenge, response)
	}

	config := &quick.Config{MaxCount: 100}
	if err := quick.Check(f, config); err != nil {
		t.Errorf("Property 2 (Response Round-Trip) failed: %v", err)
	}
}

// Property 3: Invalid Response Rejection - wrong response fails
// For any valid token and challenge, verifying with a different (randomly generated)
// response SHALL fail.
// **Validates: Requirements 3.2**
func TestProperty_InvalidResponseRejection(t *testing.T) {
	f := func(token, challenge, fakeResponse []byte) bool {
		// Skip empty inputs
		if len(token) == 0 || len(challenge) == 0 {
			return true
		}

		// Compute the correct response
		correctResponse := ComputeResponse(token, challenge)

		// If fake response happens to match correct response, skip this case
		if bytes.Equal(fakeResponse, correctResponse) {
			return true
		}

		// Verification with wrong response must fail
		return !VerifyResponse(token, challenge, fakeResponse)
	}

	config := &quick.Config{MaxCount: 100}
	if err := quick.Check(f, config); err != nil {
		t.Errorf("Property 3 (Invalid Response Rejection) failed: %v", err)
	}
}

// Property 4: Wrong Token Rejection - cross-token verification fails
// For any two different tokens and any challenge, a response computed with one token
// SHALL fail verification with the other token.
// **Validates: Requirements 3.1, 3.2**
func TestProperty_WrongTokenRejection(t *testing.T) {
	f := func(token1, token2, challenge []byte) bool {
		// Skip empty inputs
		if len(token1) == 0 || len(token2) == 0 || len(challenge) == 0 {
			return true
		}

		// If tokens are identical, skip this case
		if bytes.Equal(token1, token2) {
			return true
		}

		// Compute response with token1
		response := ComputeResponse(token1, challenge)

		// Verification with token2 must fail
		return !VerifyResponse(token2, challenge, response)
	}

	config := &quick.Config{MaxCount: 100}
	if err := quick.Check(f, config); err != nil {
		t.Errorf("Property 4 (Wrong Token Rejection) failed: %v", err)
	}
}

// Property 5: Challenge Uniqueness - generated challenges are unique
// For any sequence of generated challenges, all challenges SHALL be unique
// (with overwhelming probability).
// **Validates: Requirements 1.3**
func TestProperty_ChallengeUniqueness(t *testing.T) {
	const numChallenges = 1000
	seen := make(map[string]bool, numChallenges)

	for i := 0; i < numChallenges; i++ {
		challenge, err := GenerateChallenge()
		if err != nil {
			t.Fatalf("GenerateChallenge() failed at iteration %d: %v", i, err)
		}

		key := string(challenge)
		if seen[key] {
			t.Errorf("Property 5 (Challenge Uniqueness) failed: duplicate challenge found at iteration %d", i)
			return
		}
		seen[key] = true
	}
}

// TestNew_ValidToken tests that New accepts valid tokens
func TestNew_ValidToken(t *testing.T) {
	token := make([]byte, 16) // Minimum valid size
	auth, err := New(token)
	if err != nil {
		t.Errorf("New() with valid token returned error: %v", err)
	}
	if auth == nil {
		t.Error("New() with valid token returned nil")
	}
}

// TestNew_ShortToken tests that New rejects tokens shorter than MinTokenSize
func TestNew_ShortToken(t *testing.T) {
	token := make([]byte, 15) // One byte short
	auth, err := New(token)
	if err == nil {
		t.Error("New() with short token should return error")
	}
	if auth != nil {
		t.Error("New() with short token should return nil auth")
	}
}

// TestNew_EmptyToken tests that New rejects empty tokens
func TestNew_EmptyToken(t *testing.T) {
	auth, err := New([]byte{})
	if err == nil {
		t.Error("New() with empty token should return error")
	}
	if auth != nil {
		t.Error("New() with empty token should return nil auth")
	}
}

// TestWireProtocol_ChallengeRoundTrip tests challenge wire format encoding/decoding
func TestWireProtocol_ChallengeRoundTrip(t *testing.T) {
	challenge, err := GenerateChallenge()
	if err != nil {
		t.Fatalf("GenerateChallenge() failed: %v", err)
	}

	// Write challenge to buffer
	var buf bytes.Buffer
	if err := writeChallenge(&buf, challenge); err != nil {
		t.Fatalf("writeChallenge() failed: %v", err)
	}

	// Read challenge back
	readChallenge, err := readChallenge(&buf)
	if err != nil {
		t.Fatalf("readChallenge() failed: %v", err)
	}

	if !bytes.Equal(challenge, readChallenge) {
		t.Error("Challenge round-trip failed: values don't match")
	}
}

// TestWireProtocol_ResponseRoundTrip tests response wire format encoding/decoding
func TestWireProtocol_ResponseRoundTrip(t *testing.T) {
	token := []byte("test-token-12345678")
	challenge, _ := GenerateChallenge()
	response := ComputeResponse(token, challenge)

	// Write response to buffer
	var buf bytes.Buffer
	if err := writeResponse(&buf, response); err != nil {
		t.Fatalf("writeResponse() failed: %v", err)
	}

	// Read response back
	readResp, err := readResponse(&buf)
	if err != nil {
		t.Fatalf("readResponse() failed: %v", err)
	}

	if !bytes.Equal(response, readResp) {
		t.Error("Response round-trip failed: values don't match")
	}
}

// TestWireProtocol_InvalidChallengeSize tests that readChallenge rejects invalid sizes
func TestWireProtocol_InvalidChallengeSize(t *testing.T) {
	// Create a buffer with wrong challenge size
	var buf bytes.Buffer
	wrongSize := make([]byte, 16) // Wrong size (should be 32)
	writeChallenge(&buf, wrongSize)

	// Try to read - should fail due to size mismatch
	_, err := readChallenge(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Error("readChallenge() should reject invalid challenge size")
	}
}

// TestWireProtocol_InvalidResponseSize tests that readResponse rejects invalid sizes
func TestWireProtocol_InvalidResponseSize(t *testing.T) {
	// Create a buffer with wrong response size
	var buf bytes.Buffer
	wrongSize := make([]byte, 32) // Wrong size (should be 64)
	writeResponse(&buf, wrongSize)

	// Try to read - should fail due to size mismatch
	_, err := readResponse(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Error("readResponse() should reject invalid response size")
	}
}

// TestAuthFlow_Success tests the complete authentication flow logic
// This simulates what happens during VerifyConn without needing a real QUIC connection
func TestAuthFlow_Success(t *testing.T) {
	token := []byte("shared-secret-token-1234")

	// Server side: generate challenge
	challenge, err := GenerateChallenge()
	if err != nil {
		t.Fatalf("GenerateChallenge() failed: %v", err)
	}

	// Client side: compute response
	response := ComputeResponse(token, challenge)

	// Server side: verify response
	if !VerifyResponse(token, challenge, response) {
		t.Error("Authentication flow failed: valid response was rejected")
	}
}

// TestAuthFlow_Failure tests authentication failure with wrong token
func TestAuthFlow_Failure(t *testing.T) {
	serverToken := []byte("server-secret-token-1234")
	clientToken := []byte("wrong-client-token-5678")

	// Server side: generate challenge
	challenge, err := GenerateChallenge()
	if err != nil {
		t.Fatalf("GenerateChallenge() failed: %v", err)
	}

	// Client side: compute response with wrong token
	response := ComputeResponse(clientToken, challenge)

	// Server side: verify response - should fail
	if VerifyResponse(serverToken, challenge, response) {
		t.Error("Authentication flow should fail with wrong token")
	}
}

// TestGenerateChallenge_Size tests that generated challenges have correct size
func TestGenerateChallenge_Size(t *testing.T) {
	challenge, err := GenerateChallenge()
	if err != nil {
		t.Fatalf("GenerateChallenge() failed: %v", err)
	}
	if len(challenge) != ChallengeSize {
		t.Errorf("Challenge size = %d, want %d", len(challenge), ChallengeSize)
	}
}

// TestComputeResponse_Size tests that computed responses have correct size
func TestComputeResponse_Size(t *testing.T) {
	token := []byte("test-token-12345678")
	challenge := make([]byte, ChallengeSize)
	response := ComputeResponse(token, challenge)
	if len(response) != ResponseSize {
		t.Errorf("Response size = %d, want %d", len(response), ResponseSize)
	}
}
