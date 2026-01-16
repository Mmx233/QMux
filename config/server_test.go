package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mmx233/QMux/server/auth/challenge"
	"github.com/Mmx233/QMux/server/auth/mtls"
	"gopkg.in/yaml.v3"
)

func TestServerAuth_YAMLParsing_MTLSConfig(t *testing.T) {
	// Test parsing mTLS config with method and ca_cert_file
	content := `method: "mtls"
ca_cert_file: "./certs/ca.crt"
`
	var auth ServerAuth
	if err := yaml.Unmarshal([]byte(content), &auth); err != nil {
		t.Fatalf("failed to unmarshal YAML: %v", err)
	}

	if auth.Method != "mtls" {
		t.Errorf("expected Method 'mtls', got %q", auth.Method)
	}
	if auth.CACertFile != "./certs/ca.crt" {
		t.Errorf("expected CACertFile './certs/ca.crt', got %q", auth.CACertFile)
	}
}

func TestServerAuth_YAMLParsing_TokenConfig(t *testing.T) {
	// Test parsing token config with method and token
	content := `method: "token"
token: "my-secret-token-at-least-16-bytes"
`
	var auth ServerAuth
	if err := yaml.Unmarshal([]byte(content), &auth); err != nil {
		t.Fatalf("failed to unmarshal YAML: %v", err)
	}

	if auth.Method != "token" {
		t.Errorf("expected Method 'token', got %q", auth.Method)
	}
	if auth.Token != "my-secret-token-at-least-16-bytes" {
		t.Errorf("expected Token 'my-secret-token-at-least-16-bytes', got %q", auth.Token)
	}
}

func TestServerAuth_YAMLParsing_EmptyMethod(t *testing.T) {
	// Test parsing config with empty method (defaults to mtls)
	content := `ca_cert_file: "./certs/ca.crt"
`
	var auth ServerAuth
	if err := yaml.Unmarshal([]byte(content), &auth); err != nil {
		t.Fatalf("failed to unmarshal YAML: %v", err)
	}

	if auth.Method != "" {
		t.Errorf("expected Method to be empty string, got %q", auth.Method)
	}
	if auth.CACertFile != "./certs/ca.crt" {
		t.Errorf("expected CACertFile './certs/ca.crt', got %q", auth.CACertFile)
	}
}

func TestServerAuth_YAMLParsing_FullServerConfig(t *testing.T) {
	// Test parsing full server config with auth section
	content := `listeners:
  - host: "0.0.0.0"
    port: 8443
    traffic_port: 8080
    protocol: "tcp"
auth:
  method: "mtls"
  ca_cert_file: "./certs/ca.crt"
tls:
  server_cert_file: "./certs/server.crt"
  server_key_file: "./certs/server.key"
`
	var server Server
	if err := yaml.Unmarshal([]byte(content), &server); err != nil {
		t.Fatalf("failed to unmarshal YAML: %v", err)
	}

	if server.Auth.Method != "mtls" {
		t.Errorf("expected Auth.Method 'mtls', got %q", server.Auth.Method)
	}
	if server.Auth.CACertFile != "./certs/ca.crt" {
		t.Errorf("expected Auth.CACertFile './certs/ca.crt', got %q", server.Auth.CACertFile)
	}
}

func TestServerAuth_YAMLParsing_TableDriven(t *testing.T) {
	tests := []struct {
		name           string
		yaml           string
		expectedMethod string
		expectedCAFile string
		expectedToken  string
	}{
		{
			name:           "mtls with explicit method",
			yaml:           `method: "mtls"` + "\n" + `ca_cert_file: "/path/to/ca.crt"`,
			expectedMethod: "mtls",
			expectedCAFile: "/path/to/ca.crt",
			expectedToken:  "",
		},
		{
			name:           "token method",
			yaml:           `method: "token"` + "\n" + `token: "secure-token-16bytes"`,
			expectedMethod: "token",
			expectedCAFile: "",
			expectedToken:  "secure-token-16bytes",
		},
		{
			name:           "empty method defaults to mtls",
			yaml:           `ca_cert_file: "/path/to/ca.crt"`,
			expectedMethod: "",
			expectedCAFile: "/path/to/ca.crt",
			expectedToken:  "",
		},
		{
			name:           "all fields populated",
			yaml:           `method: "mtls"` + "\n" + `ca_cert_file: "/ca.crt"` + "\n" + `token: "unused-token"`,
			expectedMethod: "mtls",
			expectedCAFile: "/ca.crt",
			expectedToken:  "unused-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var auth ServerAuth
			if err := yaml.Unmarshal([]byte(tt.yaml), &auth); err != nil {
				t.Fatalf("failed to unmarshal YAML: %v", err)
			}

			if auth.Method != tt.expectedMethod {
				t.Errorf("Method: got %q, want %q", auth.Method, tt.expectedMethod)
			}
			if auth.CACertFile != tt.expectedCAFile {
				t.Errorf("CACertFile: got %q, want %q", auth.CACertFile, tt.expectedCAFile)
			}
			if auth.Token != tt.expectedToken {
				t.Errorf("Token: got %q, want %q", auth.Token, tt.expectedToken)
			}
		})
	}
}

// testCACert is a valid self-signed CA certificate for testing
const testCACert = `-----BEGIN CERTIFICATE-----
MIIBkTCB+wIJAKHBfpegPjMCMA0GCSqGSIb3DQEBCwUAMBExDzANBgNVBAMMBnRl
c3RjYTAeFw0yNDAxMDEwMDAwMDBaFw0yNTAxMDEwMDAwMDBaMBExDzANBgNVBAMM
BnRlc3RjYTBcMA0GCSqGSIb3DQEBAQUAA0sAMEgCQQC7o96HtiXYxvbvjnmy5WEP
pXmfLbCcFYmMBkWNkL5ckRL8xrP5R1LZ5X5X5X5X5X5X5X5X5X5X5X5X5X5X5X5X
AgMBAAGjUzBRMB0GA1UdDgQWBBQExample0000000000000000000MB8GA1UdIwQY
MBaAFBQExample0000000000000000000MA8GA1UdEwEB/wQFMAMBAf8wDQYJKoZI
hvcNAQELBQADQQBExample000000000000000000000000000000000000000000
-----END CERTIFICATE-----`

// validTestCACert is a properly formatted test CA certificate
const validTestCACert = `-----BEGIN CERTIFICATE-----
MIICpDCCAYwCCQDU+pQ4P5cFwzANBgkqhkiG9w0BAQsFADAUMRIwEAYDVQQDDAls
b2NhbGhvc3QwHhcNMjQwMTAxMDAwMDAwWhcNMjUwMTAxMDAwMDAwWjAUMRIwEAYD
VQQDDAlsb2NhbGhvc3QwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDJ
xzLxhIJLKvBLxYKOQ7M9rQ7bKpM5VPlVxNPMizApMA6LBMV5UQXR5rLmTPs5XQLO
RrF5BcDmFkycYLMzUaKvEvoJVpZdLqmcxqMSa/x8SqBHENCDuNXNpqPLLdE7E5Cj
cPOkFjWFP5ccLmEg5a4qAP5B7ziRpqJLqvEa7PPGDjKbxHGFEMsMCEpnLSD5+3ks
kWUHygGkfwG5BB9Lohn0qcs/0xiAoEuNRqqDhN1M8y5pv1fajALngg6PGzaJYPHh
YvU9xJY5tPfnfBE3A8GaDlOrhNBnEILGUTGvTpWK8BI7bwgHU1dg7Z5F8a3sa/EO
QMxiE2pBjpLZCT1Vz5YNAgMBAAEwDQYJKoZIhvcNAQELBQADggEBAGqBhBhBLmEB
E9obQj8Ryf9e/V5lWkPkCigYrLHIJZFzNBjNEhLAhj9ie/RNHBZ3LHxrk6ljP9Xe
Pg/0nbfrNOSvGBq3F+2YIwEEsXSFFBrNZ/4HWvMrCqCL7JxFZs/VPYbCwvUaIZeC
emNjP5JBpHmFDrRAdCo+gs6r1qo/Qo0yR9z8F/AAdGFMPuMBMPU2QAiVbeL+xXSb
UuBfvWLMBCy3HWibxQR1ySgYTBKBML0BKUV0mCa/bxA8QPXrZDfCgNf3F/PBsMUk
5qztnfO3RJ0yYJvTxhj+vKHH+Wf9z1RRygZwfn7L5PwC8t5bj/L5eMzieHqR5Pf0
JZxDFLhBYmU=
-----END CERTIFICATE-----`

func TestServerAuth_LoadCACertificate_Success(t *testing.T) {
	// Create a temporary CA cert file
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "ca.crt")
	if err := os.WriteFile(certPath, []byte(validTestCACert), 0644); err != nil {
		t.Fatalf("failed to write test CA cert: %v", err)
	}

	auth := ServerAuth{
		Method:     "mtls",
		CACertFile: certPath,
	}

	err := auth.LoadCACertificate()
	if err != nil {
		t.Fatalf("LoadCACertificate failed: %v", err)
	}

	if auth.CACertPool == nil {
		t.Error("expected CACertPool to be non-nil after loading")
	}
}

func TestServerAuth_LoadCACertificate_FileNotFound(t *testing.T) {
	auth := ServerAuth{
		Method:     "mtls",
		CACertFile: "/nonexistent/path/ca.crt",
	}

	err := auth.LoadCACertificate()
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
	if !strings.Contains(err.Error(), "read CA cert") {
		t.Errorf("expected error to contain 'read CA cert', got: %v", err)
	}
}

func TestServerAuth_LoadCACertificate_InvalidPEM(t *testing.T) {
	// Create a temporary file with invalid PEM content
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "invalid.crt")
	invalidContent := "this is not a valid PEM certificate"
	if err := os.WriteFile(certPath, []byte(invalidContent), 0644); err != nil {
		t.Fatalf("failed to write invalid cert: %v", err)
	}

	auth := ServerAuth{
		Method:     "mtls",
		CACertFile: certPath,
	}

	err := auth.LoadCACertificate()
	if err == nil {
		t.Fatal("expected error for invalid PEM, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse CA certificate") {
		t.Errorf("expected error to contain 'failed to parse CA certificate', got: %v", err)
	}
}

func TestServerAuth_LoadCACertificate_EmptyFile(t *testing.T) {
	// Create an empty file
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "empty.crt")
	if err := os.WriteFile(certPath, []byte(""), 0644); err != nil {
		t.Fatalf("failed to write empty cert: %v", err)
	}

	auth := ServerAuth{
		Method:     "mtls",
		CACertFile: certPath,
	}

	err := auth.LoadCACertificate()
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse CA certificate") {
		t.Errorf("expected error to contain 'failed to parse CA certificate', got: %v", err)
	}
}

func TestServerAuth_LoadCACertificate_MalformedPEM(t *testing.T) {
	// Create a file with malformed PEM (valid header/footer but invalid content)
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "malformed.crt")
	malformedContent := `-----BEGIN CERTIFICATE-----
not-valid-base64-content!!!
-----END CERTIFICATE-----`
	if err := os.WriteFile(certPath, []byte(malformedContent), 0644); err != nil {
		t.Fatalf("failed to write malformed cert: %v", err)
	}

	auth := ServerAuth{
		Method:     "mtls",
		CACertFile: certPath,
	}

	err := auth.LoadCACertificate()
	if err == nil {
		t.Fatal("expected error for malformed PEM, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse CA certificate") {
		t.Errorf("expected error to contain 'failed to parse CA certificate', got: %v", err)
	}
}

func TestServerAuth_Validate_MTLSWithValidCACertFile(t *testing.T) {
	auth := ServerAuth{
		Method:     "mtls",
		CACertFile: "/path/to/ca.crt",
	}

	err := auth.Validate()
	if err != nil {
		t.Errorf("expected validation to pass for mTLS with valid ca_cert_file, got: %v", err)
	}
}

func TestServerAuth_Validate_MTLSWithEmptyCACertFile(t *testing.T) {
	auth := ServerAuth{
		Method:     "mtls",
		CACertFile: "",
	}

	err := auth.Validate()
	if err == nil {
		t.Fatal("expected validation to fail for mTLS with empty ca_cert_file")
	}
	if !strings.Contains(err.Error(), "ca_cert_file is required") {
		t.Errorf("expected error about ca_cert_file required, got: %v", err)
	}
}

func TestServerAuth_Validate_EmptyMethodWithValidCACertFile(t *testing.T) {
	// Empty method should default to mtls
	auth := ServerAuth{
		Method:     "",
		CACertFile: "/path/to/ca.crt",
	}

	err := auth.Validate()
	if err != nil {
		t.Errorf("expected validation to pass for empty method with valid ca_cert_file, got: %v", err)
	}
}

func TestServerAuth_Validate_EmptyMethodWithEmptyCACertFile(t *testing.T) {
	// Empty method defaults to mtls, which requires ca_cert_file
	auth := ServerAuth{
		Method:     "",
		CACertFile: "",
	}

	err := auth.Validate()
	if err == nil {
		t.Fatal("expected validation to fail for empty method with empty ca_cert_file")
	}
	if !strings.Contains(err.Error(), "ca_cert_file is required") {
		t.Errorf("expected error about ca_cert_file required, got: %v", err)
	}
}

func TestServerAuth_Validate_TokenWithValidToken(t *testing.T) {
	// Token must be at least 16 bytes
	auth := ServerAuth{
		Method: "token",
		Token:  "this-is-a-valid-token-16bytes",
	}

	err := auth.Validate()
	if err != nil {
		t.Errorf("expected validation to pass for token with valid token (>=16 bytes), got: %v", err)
	}
}

func TestServerAuth_Validate_TokenWithExactly16Bytes(t *testing.T) {
	// Token with exactly 16 bytes should pass
	auth := ServerAuth{
		Method: "token",
		Token:  "exactly16bytess!", // exactly 16 bytes
	}

	err := auth.Validate()
	if err != nil {
		t.Errorf("expected validation to pass for token with exactly 16 bytes, got: %v", err)
	}
}

func TestServerAuth_Validate_TokenWithEmptyToken(t *testing.T) {
	auth := ServerAuth{
		Method: "token",
		Token:  "",
	}

	err := auth.Validate()
	if err == nil {
		t.Fatal("expected validation to fail for token with empty token")
	}
	if !strings.Contains(err.Error(), "token is required") {
		t.Errorf("expected error about token required, got: %v", err)
	}
}

func TestServerAuth_Validate_TokenWithShortToken(t *testing.T) {
	// Token less than 16 bytes should fail
	auth := ServerAuth{
		Method: "token",
		Token:  "short", // only 5 bytes
	}

	err := auth.Validate()
	if err == nil {
		t.Fatal("expected validation to fail for token with short token (<16 bytes)")
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Errorf("expected error about minimum token length, got: %v", err)
	}
}

func TestServerAuth_Validate_TokenWith15Bytes(t *testing.T) {
	// Token with 15 bytes (one less than minimum) should fail
	auth := ServerAuth{
		Method: "token",
		Token:  "exactly15bytes!", // 15 bytes
	}

	err := auth.Validate()
	if err == nil {
		t.Fatal("expected validation to fail for token with 15 bytes")
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Errorf("expected error about minimum token length, got: %v", err)
	}
}

func TestServerAuth_Validate_UnknownMethod(t *testing.T) {
	auth := ServerAuth{
		Method: "unknown",
	}

	err := auth.Validate()
	if err == nil {
		t.Fatal("expected validation to fail for unknown method")
	}
	if !strings.Contains(err.Error(), "unknown auth method") {
		t.Errorf("expected error about unknown auth method, got: %v", err)
	}
}

func TestServerAuth_Validate_TableDriven(t *testing.T) {
	tests := []struct {
		name        string
		auth        ServerAuth
		expectError bool
		errorMsg    string
	}{
		{
			name: "mtls with valid ca_cert_file",
			auth: ServerAuth{
				Method:     "mtls",
				CACertFile: "/path/to/ca.crt",
			},
			expectError: false,
		},
		{
			name: "mtls with empty ca_cert_file",
			auth: ServerAuth{
				Method:     "mtls",
				CACertFile: "",
			},
			expectError: true,
			errorMsg:    "ca_cert_file is required",
		},
		{
			name: "empty method with valid ca_cert_file (defaults to mtls)",
			auth: ServerAuth{
				Method:     "",
				CACertFile: "/path/to/ca.crt",
			},
			expectError: false,
		},
		{
			name: "empty method with empty ca_cert_file",
			auth: ServerAuth{
				Method:     "",
				CACertFile: "",
			},
			expectError: true,
			errorMsg:    "ca_cert_file is required",
		},
		{
			name: "token with valid token (>=16 bytes)",
			auth: ServerAuth{
				Method: "token",
				Token:  "this-is-a-valid-token-16bytes",
			},
			expectError: false,
		},
		{
			name: "token with exactly 16 bytes",
			auth: ServerAuth{
				Method: "token",
				Token:  "exactly16bytess!",
			},
			expectError: false,
		},
		{
			name: "token with empty token",
			auth: ServerAuth{
				Method: "token",
				Token:  "",
			},
			expectError: true,
			errorMsg:    "token is required",
		},
		{
			name: "token with short token (<16 bytes)",
			auth: ServerAuth{
				Method: "token",
				Token:  "short",
			},
			expectError: true,
			errorMsg:    "at least",
		},
		{
			name: "unknown method",
			auth: ServerAuth{
				Method: "invalid-method",
			},
			expectError: true,
			errorMsg:    "unknown auth method",
		},
		{
			name: "challenge method (should be unknown)",
			auth: ServerAuth{
				Method: "challenge",
			},
			expectError: true,
			errorMsg:    "unknown auth method",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.auth.Validate()
			if tt.expectError {
				if err == nil {
					t.Fatal("expected validation to fail, got nil error")
				}
				if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("expected error to contain %q, got: %v", tt.errorMsg, err)
				}
			} else {
				if err != nil {
					t.Errorf("expected validation to pass, got: %v", err)
				}
			}
		})
	}
}

func TestServerAuth_CreateAuthenticator_MTLSWithValidCACert(t *testing.T) {
	// Create a temporary CA cert file
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "ca.crt")
	if err := os.WriteFile(certPath, []byte(validTestCACert), 0644); err != nil {
		t.Fatalf("failed to write test CA cert: %v", err)
	}

	auth := ServerAuth{
		Method:     "mtls",
		CACertFile: certPath,
	}

	authenticator, err := auth.CreateAuthenticator()
	if err != nil {
		t.Fatalf("CreateAuthenticator failed: %v", err)
	}

	if authenticator == nil {
		t.Fatal("expected authenticator to be non-nil")
	}

	// Verify it's an mTLS authenticator by type assertion
	_, ok := authenticator.(*mtls.MTLSAuth)
	if !ok {
		t.Errorf("expected authenticator to be *mtls.MTLSAuth, got %T", authenticator)
	}

	// Verify CA cert pool was loaded
	if auth.CACertPool == nil {
		t.Error("expected CACertPool to be loaded after CreateAuthenticator")
	}
}

func TestServerAuth_CreateAuthenticator_EmptyMethodDefaultsToMTLS(t *testing.T) {
	// Create a temporary CA cert file
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "ca.crt")
	if err := os.WriteFile(certPath, []byte(validTestCACert), 0644); err != nil {
		t.Fatalf("failed to write test CA cert: %v", err)
	}

	auth := ServerAuth{
		Method:     "", // Empty method should default to mtls
		CACertFile: certPath,
	}

	authenticator, err := auth.CreateAuthenticator()
	if err != nil {
		t.Fatalf("CreateAuthenticator failed: %v", err)
	}

	if authenticator == nil {
		t.Fatal("expected authenticator to be non-nil")
	}

	// Verify it's an mTLS authenticator
	_, ok := authenticator.(*mtls.MTLSAuth)
	if !ok {
		t.Errorf("expected authenticator to be *mtls.MTLSAuth for empty method, got %T", authenticator)
	}
}

func TestServerAuth_CreateAuthenticator_TokenMethod(t *testing.T) {
	auth := ServerAuth{
		Method: "token",
		Token:  "this-is-a-valid-token-16bytes",
	}

	authenticator, err := auth.CreateAuthenticator()
	if err != nil {
		t.Fatalf("CreateAuthenticator failed: %v", err)
	}

	if authenticator == nil {
		t.Fatal("expected authenticator to be non-nil")
	}

	// Verify it's a challenge authenticator by type assertion
	_, ok := authenticator.(*challenge.ChallengeAuth)
	if !ok {
		t.Errorf("expected authenticator to be *challenge.ChallengeAuth, got %T", authenticator)
	}
}

func TestServerAuth_CreateAuthenticator_MTLSCACertLoadingFails(t *testing.T) {
	auth := ServerAuth{
		Method:     "mtls",
		CACertFile: "/nonexistent/path/ca.crt",
	}

	authenticator, err := auth.CreateAuthenticator()
	if err == nil {
		t.Fatal("expected error when CA cert loading fails")
	}
	if authenticator != nil {
		t.Error("expected authenticator to be nil when error occurs")
	}
	if !strings.Contains(err.Error(), "load CA certificate") {
		t.Errorf("expected error to contain 'load CA certificate', got: %v", err)
	}
}

func TestServerAuth_CreateAuthenticator_MTLSInvalidCACert(t *testing.T) {
	// Create a temporary file with invalid PEM content
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "invalid.crt")
	if err := os.WriteFile(certPath, []byte("not a valid certificate"), 0644); err != nil {
		t.Fatalf("failed to write invalid cert: %v", err)
	}

	auth := ServerAuth{
		Method:     "mtls",
		CACertFile: certPath,
	}

	authenticator, err := auth.CreateAuthenticator()
	if err == nil {
		t.Fatal("expected error when CA cert is invalid")
	}
	if authenticator != nil {
		t.Error("expected authenticator to be nil when error occurs")
	}
	if !strings.Contains(err.Error(), "load CA certificate") {
		t.Errorf("expected error to contain 'load CA certificate', got: %v", err)
	}
}

func TestServerAuth_CreateAuthenticator_UnknownMethod(t *testing.T) {
	auth := ServerAuth{
		Method: "unknown-method",
	}

	authenticator, err := auth.CreateAuthenticator()
	if err == nil {
		t.Fatal("expected error for unknown method")
	}
	if authenticator != nil {
		t.Error("expected authenticator to be nil when error occurs")
	}
	if !strings.Contains(err.Error(), "unknown auth method") {
		t.Errorf("expected error to contain 'unknown auth method', got: %v", err)
	}
}

func TestServerAuth_CreateAuthenticator_TableDriven(t *testing.T) {
	// Create a temporary CA cert file for tests that need it
	tmpDir := t.TempDir()
	validCertPath := filepath.Join(tmpDir, "ca.crt")
	if err := os.WriteFile(validCertPath, []byte(validTestCACert), 0644); err != nil {
		t.Fatalf("failed to write test CA cert: %v", err)
	}

	invalidCertPath := filepath.Join(tmpDir, "invalid.crt")
	if err := os.WriteFile(invalidCertPath, []byte("invalid"), 0644); err != nil {
		t.Fatalf("failed to write invalid cert: %v", err)
	}

	tests := []struct {
		name             string
		auth             ServerAuth
		expectError      bool
		errorMsg         string
		expectedAuthType string // "mtls" or "challenge"
	}{
		{
			name: "mtls with valid CA cert",
			auth: ServerAuth{
				Method:     "mtls",
				CACertFile: validCertPath,
			},
			expectError:      false,
			expectedAuthType: "mtls",
		},
		{
			name: "empty method with valid CA cert (defaults to mtls)",
			auth: ServerAuth{
				Method:     "",
				CACertFile: validCertPath,
			},
			expectError:      false,
			expectedAuthType: "mtls",
		},
		{
			name: "token method with valid token",
			auth: ServerAuth{
				Method: "token",
				Token:  "this-is-a-valid-token-16bytes",
			},
			expectError:      false,
			expectedAuthType: "challenge",
		},
		{
			name: "mtls with nonexistent CA cert file",
			auth: ServerAuth{
				Method:     "mtls",
				CACertFile: "/nonexistent/ca.crt",
			},
			expectError: true,
			errorMsg:    "load CA certificate",
		},
		{
			name: "mtls with invalid CA cert",
			auth: ServerAuth{
				Method:     "mtls",
				CACertFile: invalidCertPath,
			},
			expectError: true,
			errorMsg:    "load CA certificate",
		},
		{
			name: "unknown method",
			auth: ServerAuth{
				Method: "invalid",
			},
			expectError: true,
			errorMsg:    "unknown auth method",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authenticator, err := tt.auth.CreateAuthenticator()

			if tt.expectError {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("expected error to contain %q, got: %v", tt.errorMsg, err)
				}
				if authenticator != nil {
					t.Error("expected authenticator to be nil when error occurs")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if authenticator == nil {
					t.Fatal("expected authenticator to be non-nil")
				}

				// Verify authenticator type
				switch tt.expectedAuthType {
				case "mtls":
					if _, ok := authenticator.(*mtls.MTLSAuth); !ok {
						t.Errorf("expected *mtls.MTLSAuth, got %T", authenticator)
					}
				case "challenge":
					if _, ok := authenticator.(*challenge.ChallengeAuth); !ok {
						t.Errorf("expected *challenge.ChallengeAuth, got %T", authenticator)
					}
				}
			}
		})
	}
}
