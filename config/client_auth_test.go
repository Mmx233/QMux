package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
)

func TestClientAuthDefaultsToMTLS(t *testing.T) {
	client := Client{}
	client.ApplyDefaults()
	if client.Auth.Method != ClientAuthMethodMTLS {
		t.Fatalf("auth method = %q, want %q", client.Auth.Method, ClientAuthMethodMTLS)
	}
}

func TestClientAuthValidationAndConditionalCertificates(t *testing.T) {
	validMTLS := Client{
		Server: ClientServer{Servers: []ServerEndpoint{{Address: "server.example.com:8443"}}},
		Local:  LocalService{Host: "127.0.0.1", Port: 8080},
		Auth:   ClientAuth{Method: ClientAuthMethodMTLS},
		TLS: ClientTLS{
			CACertFile:     "ca.pem",
			ClientCertFile: "client.pem",
			ClientKeyFile:  "client-key.pem",
		},
		HeartbeatInterval: time.Second,
		HealthTimeout:     2 * time.Second,
	}
	tests := []struct {
		name      string
		mutate    func(*Client)
		wantError string
	}{
		{name: "mtls"},
		{
			name: "empty method is effectively mtls",
			mutate: func(client *Client) {
				client.Auth.Method = ""
			},
		},
		{
			name: "token without client keypair",
			mutate: func(client *Client) {
				client.Auth = ClientAuth{Method: ClientAuthMethodToken, Token: strings.Repeat("t", sharedtoken.MinSecretSize)}
				client.TLS.ClientCertFile = ""
				client.TLS.ClientKeyFile = ""
			},
		},
		{
			name: "short token",
			mutate: func(client *Client) {
				client.Auth = ClientAuth{Method: ClientAuthMethodToken, Token: strings.Repeat("t", sharedtoken.MinSecretSize-1)}
			},
			wantError: "at least 16 bytes",
		},
		{
			name: "unknown method",
			mutate: func(client *Client) {
				client.Auth.Method = "password"
			},
			wantError: "unknown auth method",
		},
		{
			name: "token requires CA",
			mutate: func(client *Client) {
				client.Auth = ClientAuth{Method: ClientAuthMethodToken, Token: strings.Repeat("t", sharedtoken.MinSecretSize)}
				client.TLS.CACertFile = ""
			},
			wantError: "ca_cert_file is required",
		},
		{
			name: "mtls requires certificate",
			mutate: func(client *Client) {
				client.TLS.ClientCertFile = ""
			},
			wantError: "client_cert_file is required",
		},
		{
			name: "mtls requires key",
			mutate: func(client *Client) {
				client.TLS.ClientKeyFile = ""
			},
			wantError: "client_key_file is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := validMTLS
			if test.mutate != nil {
				test.mutate(&client)
			}
			err := client.Validate()
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

func TestLoadCredentialsSelectsFilesByAuthMethod(t *testing.T) {
	certPEM, keyPEM := testCertificate(t)
	tempDir := t.TempDir()
	caPath := filepath.Join(tempDir, "ca.pem")
	certPath := filepath.Join(tempDir, "client.pem")
	keyPath := filepath.Join(tempDir, "client-key.pem")
	for path, contents := range map[string][]byte{
		caPath:   certPEM,
		certPath: certPEM,
		keyPath:  keyPEM,
	} {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	t.Run("token loads CA only", func(t *testing.T) {
		client := Client{
			Auth: ClientAuth{Method: ClientAuthMethodToken, Token: strings.Repeat("t", sharedtoken.MinSecretSize)},
			TLS: ClientTLS{
				CACertFile:     caPath,
				ClientCertFile: filepath.Join(tempDir, "does-not-exist.pem"),
				ClientKeyFile:  filepath.Join(tempDir, "does-not-exist-key.pem"),
			},
		}
		if err := client.LoadCredentials(); err != nil {
			t.Fatalf("LoadCredentials: %v", err)
		}
		if client.TLS.CACertPool == nil {
			t.Fatal("CA certificate was not loaded")
		}
		if len(client.TLS.ClientCert.Certificate) != 0 {
			t.Fatal("token auth unexpectedly loaded a client certificate")
		}
	})

	t.Run("mtls retains combined loader behavior", func(t *testing.T) {
		client := Client{
			Auth: ClientAuth{Method: ClientAuthMethodMTLS},
			TLS: ClientTLS{
				CACertFile:     caPath,
				ClientCertFile: certPath,
				ClientKeyFile:  keyPath,
			},
		}
		if err := client.LoadCredentials(); err != nil {
			t.Fatalf("LoadCredentials: %v", err)
		}
		if client.TLS.CACertPool == nil || len(client.TLS.ClientCert.Certificate) == 0 {
			t.Fatal("mTLS credentials were not fully loaded")
		}
	})
}

func TestLoadClientConfigTokenAuthWithoutClientKeyPair(t *testing.T) {
	content := `client_id: token-client
server:
  servers:
    - address: "server.example.com:8443"
      server_name: "server.example.com"
local:
  host: "127.0.0.1"
  port: 8080
auth:
  method: token
  token: "0123456789abcdef"
tls:
  ca_cert_file: "ca.pem"
`
	path := filepath.Join(t.TempDir(), "client.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadClientConfig(path)
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}
	if cfg.Auth.Method != ClientAuthMethodToken || cfg.TLS.ClientCertFile != "" || cfg.TLS.ClientKeyFile != "" {
		t.Fatalf("loaded token config = %+v, TLS = %+v", cfg.Auth, cfg.TLS)
	}
}

func TestLoadClientConfigRejectsAuthMisconfiguration(t *testing.T) {
	tests := []struct {
		name      string
		authYAML  string
		tlsYAML   string
		wantError string
	}{
		{
			name:      "unknown method",
			authYAML:  "  method: password\n",
			tlsYAML:   "  ca_cert_file: ca.pem\n",
			wantError: "unknown auth method",
		},
		{
			name:      "short token",
			authYAML:  "  method: token\n  token: short\n",
			tlsYAML:   "  ca_cert_file: ca.pem\n",
			wantError: "at least 16 bytes",
		},
		{
			name:      "token missing CA",
			authYAML:  "  method: token\n  token: 0123456789abcdef\n",
			wantError: "ca_cert_file is required",
		},
		{
			name:      "legacy mtls missing keypair",
			tlsYAML:   "  ca_cert_file: ca.pem\n",
			wantError: "client_cert_file is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := "client_id: test-client\n" +
				"server:\n  servers:\n    - address: server.example.com:8443\n" +
				"local:\n  host: 127.0.0.1\n  port: 8080\n" +
				"auth:\n" + test.authYAML +
				"tls:\n" + test.tlsYAML
			path := filepath.Join(t.TempDir(), "client.yaml")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := LoadClientConfig(path)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("LoadClientConfig error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func testCertificate(t *testing.T) ([]byte, []byte) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
