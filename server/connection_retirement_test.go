package server

import (
	"crypto/x509"
	"testing"
	"time"

	"github.com/Mmx233/QMux/server/auth/mtls"
	"github.com/Mmx233/QMux/server/pool"
)

func TestClientCloseReleasesRegistrationForSameID(t *testing.T) {
	clientCertificate, clientRoots := registrationTestClientCertificate(
		t,
		"client-retirement",
		false,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	)
	serverTLS, clientTLS := registrationMTLSTLSConfigs(t, clientRoots, clientCertificate)
	harness := newRegistrationHarnessWithTLS(t, mtls.New(clientRoots), time.Second, serverTLS, clientTLS)
	const clientID = "retired-client-id"
	registerMTLSClient(t, harness, clientID)

	liveDuplicate := &pool.ClientConn{ID: clientID}
	if _, err := harness.pool.Reserve(liveDuplicate); err == nil {
		t.Fatal("live duplicate registration was not rejected")
	}
	if got := harness.pool.Count(); got != 1 {
		t.Fatalf("pool Count() after live duplicate rejection = %d, want 1", got)
	}
	if err := harness.client.CloseWithError(0, "client-side retirement"); err != nil {
		t.Fatalf("close registered client: %v", err)
	}
	harness.waitForHandler(t)
	if got := harness.pool.Count(); got != 0 {
		t.Fatalf("pool Count() after exact deferred Remove = %d, want 0", got)
	}

	replacement := &pool.ClientConn{ID: clientID}
	reservation, err := harness.pool.Reserve(replacement)
	if err != nil {
		t.Fatalf("Reserve() same client ID after close: %v", err)
	}
	if !harness.pool.Abort(reservation) {
		t.Fatal("Abort() replacement reservation failed")
	}
}
