package e2e

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/config"
	"github.com/rs/zerolog"
)

const (
	tokenE2ETestSecret      = "0123456789abcdef0123456789abcdef"
	wrongTokenE2ETestSecret = "fedcba9876543210fedcba9876543210"
)

func newTokenServerConfig(
	certDir, networkProtocol, secret string,
	quicPort, trafficPort int,
) *config.Server {
	cfg := newMTLSServerConfig(certDir, networkProtocol, quicPort, trafficPort, 50*time.Millisecond, 2*time.Second)
	cfg.Auth = config.ServerAuth{
		Method: config.ClientAuthMethodToken,
		Token:  secret,
	}
	return cfg
}

func newTokenClientConfig(certDir, clientID, secret string, localPort, quicPort int) *config.Client {
	cfg := newMTLSClientConfig(certDir, clientID, localPort, 50*time.Millisecond, 2*time.Second, quicPort)
	cfg.Auth = config.ClientAuth{
		Method: config.ClientAuthMethodToken,
		Token:  secret,
	}
	cfg.TLS.ClientCertFile = ""
	cfg.TLS.ClientKeyFile = ""
	return cfg
}

func waitForHealthyTokenClient(
	t *testing.T,
	ctx context.Context,
	c *client.Client,
	clientErr, serverErr <-chan error,
) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		if c.HealthyConnectionCount() == 1 {
			return
		}
		select {
		case err := <-clientErr:
			t.Fatalf("token client failed before becoming healthy: %v", err)
		case err := <-serverErr:
			t.Fatalf("token server failed before client became healthy: %v", err)
		case <-deadline.C:
			t.Fatalf(
				"token client did not become healthy: healthy=%d total=%d",
				c.HealthyConnectionCount(),
				c.TotalConnectionCount(),
			)
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("token client context ended before registration: %v", context.Cause(ctx))
		}
	}
}

func TestTCPReverseProxy_TokenWithoutClientCertificate(t *testing.T) {
	certDir := generateTestCertificates(t)
	_, localPort := startTCPEchoListener(t)
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverErr := startTestServerReporting(
		ctx,
		newTokenServerConfig(certDir, "tcp", tokenE2ETestSecret, quicPort, trafficPort),
	)
	time.Sleep(300 * time.Millisecond)

	clientConfig := newTokenClientConfig(
		certDir,
		"token-e2e-client",
		tokenE2ETestSecret,
		localPort,
		quicPort,
	)
	c := newTestClient(t, clientConfig)
	clientErr := startTestClientReporting(ctx, c)
	waitForHealthyTokenClient(t, ctx, c, clientErr, serverErr)

	assertTCPEcho(
		t,
		fmt.Sprintf("127.0.0.1:%d", trafficPort),
		[]byte("exporter-bound token authentication carried tunnel traffic"),
	)
}

func TestTokenAuthenticationRejectsWrongSecret(t *testing.T) {
	certDir := generateTestCertificates(t)
	_, localPort := startTCPEchoListener(t)
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverErr := startTestServerReporting(
		ctx,
		newTokenServerConfig(certDir, "tcp", tokenE2ETestSecret, quicPort, trafficPort),
	)
	time.Sleep(300 * time.Millisecond)

	c := newTestClient(t, newTokenClientConfig(
		certDir,
		"wrong-token-e2e-client",
		wrongTokenE2ETestSecret,
		localPort,
		quicPort,
	))
	if err := c.ConnectionManager().Start(ctx); err != nil {
		t.Fatalf("run wrong-token connection attempt: %v", err)
	}
	if got := c.HealthyConnectionCount(); got != 0 {
		t.Fatalf("wrong-token client became healthy: %d connections", got)
	}
	if got := c.TotalConnectionCount(); got != 0 {
		t.Fatalf("wrong-token connection was published: %d connections", got)
	}
	if err := c.ConnectionManager().Stop(); err != nil {
		t.Fatalf("stop wrong-token connection manager: %v", err)
	}
	select {
	case err := <-serverErr:
		t.Fatalf("token server failed during wrong-secret test: %v", err)
	default:
	}
}

func TestTokenAuthenticationStillVerifiesServerCertificate(t *testing.T) {
	serverCertDir := generateTestCertificates(t)
	untrustedCertDir := generateTestCertificates(t)
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	serverErr := startTestServerReporting(
		ctx,
		newTokenServerConfig(serverCertDir, "tcp", tokenE2ETestSecret, quicPort, trafficPort),
	)
	waitForQUICListener(t, ctx, newTokenClientConfig(
		serverCertDir,
		"trusted-readiness-probe",
		tokenE2ETestSecret,
		1,
		quicPort,
	), serverErr)

	clientConfig := newTokenClientConfig(untrustedCertDir, "untrusted-server-client", tokenE2ETestSecret, 1, quicPort)
	var logs bytes.Buffer
	manager, err := client.NewConnectionManager(clientConfig, zerolog.New(zerolog.SyncWriter(&logs)))
	if err != nil {
		t.Fatalf("create untrusted client connection manager: %v", err)
	}
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start untrusted client connection manager: %v", err)
	}
	if got := manager.HealthyCount(); got != 0 {
		t.Fatalf("untrusted client became healthy: %d connections", got)
	}
	if got := manager.TotalCount(); got != 0 {
		t.Fatalf("untrusted connection was published: %d connections", got)
	}
	if err := manager.Stop(); err != nil {
		t.Fatalf("stop untrusted client connection manager: %v", err)
	}
	if !strings.Contains(logs.String(), "certificate signed by unknown authority") {
		t.Fatalf("untrusted client error did not preserve unknown-authority cause: %s", logs.String())
	}
}
