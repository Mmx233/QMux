package config

import (
	"testing"

	"github.com/Mmx233/QMux/server/auth/challenge"
	"pgregory.net/rapid"
)

// TestProperty_MTLSValidation_ValidCACertFile verifies that mTLS config with
// a non-empty ca_cert_file always passes validation.
func TestProperty_MTLSValidation_ValidCACertFile(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate non-empty ca_cert_file path using a pattern that produces valid file paths
		caCertFile := rapid.StringMatching(`[a-zA-Z0-9/._-]+`).Filter(func(s string) bool {
			return len(s) > 0
		}).Draw(t, "ca_cert_file")

		// Test with explicit "mtls" method
		auth := ServerAuth{
			Method:     "mtls",
			CACertFile: caCertFile,
		}

		err := auth.Validate()
		if err != nil {
			t.Fatalf("expected validation to pass for mTLS with ca_cert_file %q, got: %v", caCertFile, err)
		}

		// Also test with empty method (defaults to mtls)
		authEmpty := ServerAuth{
			Method:     "",
			CACertFile: caCertFile,
		}

		err = authEmpty.Validate()
		if err != nil {
			t.Fatalf("expected validation to pass for empty method with ca_cert_file %q, got: %v", caCertFile, err)
		}
	})
}

// TestProperty_TokenValidation_ValidToken verifies that token config with
// length >= 16 bytes always passes validation.
func TestProperty_TokenValidation_ValidToken(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate token with length >= MinTokenSize (16 bytes)
		// Use a range from MinTokenSize to a reasonable max (256 bytes)
		tokenLength := rapid.IntRange(challenge.MinTokenSize, 256).Draw(t, "token_length")

		// Generate random bytes for the token
		tokenBytes := make([]byte, tokenLength)
		for i := 0; i < tokenLength; i++ {
			// Generate printable ASCII characters (32-126) for readable tokens
			tokenBytes[i] = byte(rapid.IntRange(32, 126).Draw(t, "char"))
		}
		token := string(tokenBytes)

		auth := ServerAuth{
			Method: "token",
			Token:  token,
		}

		err := auth.Validate()
		if err != nil {
			t.Fatalf("expected validation to pass for token with length %d (>= %d), got: %v",
				len(token), challenge.MinTokenSize, err)
		}
	})
}

// TestProperty_TokenValidation_ShortToken verifies that token config with
// length < 16 bytes always fails validation.
func TestProperty_TokenValidation_ShortToken(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate token with length < MinTokenSize (16 bytes)
		// Range from 1 to MinTokenSize-1 (1 to 15 bytes)
		tokenLength := rapid.IntRange(1, challenge.MinTokenSize-1).Draw(t, "token_length")

		// Generate random bytes for the token
		tokenBytes := make([]byte, tokenLength)
		for i := 0; i < tokenLength; i++ {
			// Generate printable ASCII characters (32-126) for readable tokens
			tokenBytes[i] = byte(rapid.IntRange(32, 126).Draw(t, "char"))
		}
		token := string(tokenBytes)

		auth := ServerAuth{
			Method: "token",
			Token:  token,
		}

		err := auth.Validate()
		if err == nil {
			t.Fatalf("expected validation to fail for token with length %d (< %d), but it passed",
				len(token), challenge.MinTokenSize)
		}

		// Verify the error message mentions the minimum length requirement
		expectedMsg := "at least"
		if !containsString(err.Error(), expectedMsg) {
			t.Fatalf("expected error to contain %q, got: %v", expectedMsg, err)
		}
	})
}

// containsString checks if s contains substr
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

// containsSubstring is a helper to check substring containment
func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
