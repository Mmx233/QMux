package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mmx233/QMux/server/auth/mtls"
	"github.com/Mmx233/QMux/server/auth/tokenauth"
	"gopkg.in/yaml.v3"
)

func TestServerAuthYAML(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want ServerAuth
	}{
		{
			name: "mtls",
			yaml: "method: mtls\nca_cert_file: /path/to/ca.crt\n",
			want: ServerAuth{Method: "mtls", CACertFile: "/path/to/ca.crt"},
		},
		{
			name: "token",
			yaml: "method: token\ntoken: secure-token-16bytes\n",
			want: ServerAuth{Method: "token", Token: "secure-token-16bytes"},
		},
		{
			name: "default method",
			yaml: "ca_cert_file: /path/to/ca.crt\n",
			want: ServerAuth{CACertFile: "/path/to/ca.crt"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got ServerAuth
			if err := yaml.Unmarshal([]byte(test.yaml), &got); err != nil {
				t.Fatalf("unmarshal ServerAuth: %v", err)
			}
			if got.Method != test.want.Method || got.CACertFile != test.want.CACertFile || got.Token != test.want.Token {
				t.Fatalf("ServerAuth = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestServerAuthLoadCACertificate(t *testing.T) {
	validCert, _ := testCertificate(t)
	tests := []struct {
		name        string
		content     []byte
		missingFile bool
		wantError   string
	}{
		{name: "valid", content: validCert},
		{name: "missing file", missingFile: true, wantError: "read CA cert"},
		{name: "invalid PEM", content: []byte("not a certificate"), wantError: "failed to parse CA certificate"},
		{name: "empty file", content: nil, wantError: "failed to parse CA certificate"},
		{
			name:      "malformed PEM",
			content:   []byte("-----BEGIN CERTIFICATE-----\nnot-valid-base64-content!!!\n-----END CERTIFICATE-----"),
			wantError: "failed to parse CA certificate",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ca.crt")
			if test.missingFile {
				path = filepath.Join(t.TempDir(), "missing.crt")
			} else if err := os.WriteFile(path, test.content, 0o644); err != nil {
				t.Fatalf("write CA file: %v", err)
			}

			auth := ServerAuth{Method: "mtls", CACertFile: path}
			err := auth.LoadCACertificate()
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("LoadCACertificate error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadCACertificate: %v", err)
			}
			if auth.CACertPool == nil {
				t.Fatal("CA certificate pool was not loaded")
			}
		})
	}
}

func TestServerAuthValidate(t *testing.T) {
	tests := []struct {
		name      string
		auth      ServerAuth
		wantError string
	}{
		{name: "mtls", auth: ServerAuth{Method: "mtls", CACertFile: "/path/to/ca.crt"}},
		{name: "mtls missing CA", auth: ServerAuth{Method: "mtls"}, wantError: "ca_cert_file is required"},
		{name: "default mtls", auth: ServerAuth{CACertFile: "/path/to/ca.crt"}},
		{name: "default mtls missing CA", auth: ServerAuth{}, wantError: "ca_cert_file is required"},
		{name: "token", auth: ServerAuth{Method: "token", Token: "this-is-a-valid-token"}},
		{name: "token exactly 16 bytes", auth: ServerAuth{Method: "token", Token: "exactly16bytess!"}},
		{name: "token missing", auth: ServerAuth{Method: "token"}, wantError: "token is required"},
		{name: "token 15 bytes", auth: ServerAuth{Method: "token", Token: "exactly15bytes!"}, wantError: "at least"},
		{name: "unknown method", auth: ServerAuth{Method: "unknown"}, wantError: "unknown auth method"},
		{name: "removed challenge method", auth: ServerAuth{Method: "challenge"}, wantError: "unknown auth method"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.auth.Validate()
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Validate error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestServerAuthCreateAuthenticator(t *testing.T) {
	validCert, _ := testCertificate(t)
	validCertPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(validCertPath, validCert, 0o644); err != nil {
		t.Fatalf("write valid CA: %v", err)
	}
	invalidCertPath := filepath.Join(t.TempDir(), "invalid.crt")
	if err := os.WriteFile(invalidCertPath, []byte("invalid"), 0o644); err != nil {
		t.Fatalf("write invalid CA: %v", err)
	}

	tests := []struct {
		name      string
		auth      ServerAuth
		wantType  string
		wantError string
	}{
		{name: "mtls", auth: ServerAuth{Method: "mtls", CACertFile: validCertPath}, wantType: "mtls"},
		{name: "default mtls", auth: ServerAuth{CACertFile: validCertPath}, wantType: "mtls"},
		{name: "token", auth: ServerAuth{Method: "token", Token: "this-is-a-valid-token"}, wantType: "token"},
		{
			name:      "missing CA file",
			auth:      ServerAuth{Method: "mtls", CACertFile: filepath.Join(t.TempDir(), "missing.crt")},
			wantError: "load CA certificate",
		},
		{
			name:      "invalid CA file",
			auth:      ServerAuth{Method: "mtls", CACertFile: invalidCertPath},
			wantError: "load CA certificate",
		},
		{name: "unknown method", auth: ServerAuth{Method: "unknown"}, wantError: "unknown auth method"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator, err := test.auth.CreateAuthenticator()
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("CreateAuthenticator error = %v, want containing %q", err, test.wantError)
				}
				if authenticator != nil {
					t.Fatal("CreateAuthenticator returned an authenticator with an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateAuthenticator: %v", err)
			}
			switch test.wantType {
			case "mtls":
				if _, ok := authenticator.(*mtls.MTLSAuth); !ok {
					t.Fatalf("authenticator type = %T, want *mtls.MTLSAuth", authenticator)
				}
				if test.auth.CACertPool == nil {
					t.Fatal("CreateAuthenticator did not load the CA pool")
				}
			case "token":
				if _, ok := authenticator.(*tokenauth.TokenAuth); !ok {
					t.Fatalf("authenticator type = %T, want *tokenauth.TokenAuth", authenticator)
				}
			}
		})
	}
}
