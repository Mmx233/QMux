package token

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func TestKnownVector(t *testing.T) {
	transcript := Transcript{
		ClientID:     "client-a",
		Version:      "2.0",
		Capabilities: []string{"tcp", "udp-wire-v2"},
	}
	canonical, err := marshalTranscript(transcript)
	if err != nil {
		t.Fatalf("marshal transcript: %v", err)
	}

	wantCanonical, err := hex.DecodeString(
		"0000001f514d757820746f6b656e2061757468656e7469636174696f6e2070726f6f66" +
			"0000001d716d75782d746f6b656e2d656b6d2d686d61632d7368613531322d7631" +
			"00000008636c69656e742d61" +
			"00000003322e30" +
			"00000002" +
			"00000003746370" +
			"0000000b7564702d776972652d7632",
	)
	if err != nil {
		t.Fatalf("decode canonical fixture: %v", err)
	}
	if !bytes.Equal(canonical, wantCanonical) {
		t.Fatalf("canonical transcript mismatch\n got: %x\nwant: %x", canonical, wantCanonical)
	}

	ekm := make([]byte, exporterLength)
	for i := range ekm {
		ekm[i] = byte(i)
	}
	proof := computeProof([]byte("0123456789abcdef"), canonical, ekm)
	wantProof, err := hex.DecodeString(
		"afdc64bacb3a33f78b6fec81fee17a160d4d2f3a0ffbea68ff28124adb03e4c6" +
			"88ed79c3dd0d33ff32c52a6cc541263916282a7ad19e9e847ffd12dc6120f9f1",
	)
	if err != nil {
		t.Fatalf("decode proof fixture: %v", err)
	}
	if !bytes.Equal(proof, wantProof) {
		t.Fatalf("proof mismatch\n got: %x\nwant: %x", proof, wantProof)
	}
}

func TestComputeAndVerify(t *testing.T) {
	clientState, serverState := tls13States(t)
	secret := []byte("0123456789abcdef")
	transcript := Transcript{
		ClientID:     "client-a",
		Version:      "2.0",
		Capabilities: []string{"tcp", "udp-wire-v2"},
	}

	proof, err := Compute(secret, transcript, clientState)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(proof) != ProofSize {
		t.Fatalf("proof size = %d, want %d", len(proof), ProofSize)
	}
	if err := Verify(secret, transcript, proof, serverState); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	resumedState := clientState
	resumedState.DidResume = true
	resumedProof, err := Compute(secret, transcript, resumedState)
	if err != nil {
		t.Fatalf("Compute with resumed state: %v", err)
	}
	if !bytes.Equal(resumedProof, proof) {
		t.Fatal("DidResume changed the post-handshake proof")
	}
}

func TestVerifyRejectsMutations(t *testing.T) {
	clientState, serverState := tls13States(t)
	secret := []byte("0123456789abcdef")
	transcript := Transcript{
		ClientID:     "client-a",
		Version:      "2.0",
		Capabilities: []string{"tcp", "udp-wire-v2"},
	}
	proof, err := Compute(secret, transcript, clientState)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	mutations := map[string]Transcript{
		"client ID":        {ClientID: "client-b", Version: "2.0", Capabilities: []string{"tcp", "udp-wire-v2"}},
		"version":          {ClientID: "client-a", Version: "2.1", Capabilities: []string{"tcp", "udp-wire-v2"}},
		"capability order": {ClientID: "client-a", Version: "2.0", Capabilities: []string{"udp-wire-v2", "tcp"}},
		"capability case":  {ClientID: "client-a", Version: "2.0", Capabilities: []string{"TCP", "udp-wire-v2"}},
	}
	for name, mutated := range mutations {
		t.Run(name, func(t *testing.T) {
			if err := Verify(secret, mutated, proof, serverState); err == nil {
				t.Fatal("Verify accepted a mutated transcript")
			}
		})
	}

	wrongProof := bytes.Clone(proof)
	wrongProof[0] ^= 0xff
	if err := Verify(secret, transcript, wrongProof, serverState); err == nil {
		t.Fatal("Verify accepted a wrong proof")
	}
	if err := Verify([]byte("fedcba9876543210"), transcript, proof, serverState); err == nil {
		t.Fatal("Verify accepted a wrong secret")
	}
	if err := Verify(secret, transcript, proof[:ProofSize-1], serverState); err == nil {
		t.Fatal("Verify accepted a truncated proof")
	}
}

func TestComputeValidation(t *testing.T) {
	clientState, _ := tls13States(t)
	transcript := Transcript{ClientID: "client-a", Version: "2.0", Capabilities: []string{"tcp"}}

	if _, err := Compute([]byte("too-short"), transcript, clientState); err == nil {
		t.Fatal("Compute accepted a short secret")
	}
	if _, err := Compute([]byte("0123456789abcdef"), transcript, tls.ConnectionState{}); err == nil || !strings.Contains(err.Error(), "not complete") {
		t.Fatalf("Compute with incomplete handshake error = %v", err)
	}
	tls12State := clientState
	tls12State.Version = tls.VersionTLS12
	if _, err := Compute([]byte("0123456789abcdef"), transcript, tls12State); err == nil || !strings.Contains(err.Error(), "TLS 1.3") {
		t.Fatalf("Compute with TLS 1.2 error = %v", err)
	}

	duplicate := transcript
	duplicate.Capabilities = []string{"tcp", "tcp"}
	if _, err := Compute([]byte("0123456789abcdef"), duplicate, clientState); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("Compute with duplicate capabilities error = %v", err)
	}
}

func TestEncodedLengthBounds(t *testing.T) {
	maxUint32 := uint64(^uint32(0))
	got, err := addEncodedLength(0, maxUint32)
	if err != nil {
		t.Fatalf("maximum uint32 field length: %v", err)
	}
	if want := uint64(4) + maxUint32; got != want {
		t.Fatalf("encoded size = %d, want %d", got, want)
	}
	if _, err := addEncodedLength(0, maxUint32+1); err == nil {
		t.Fatal("addEncodedLength accepted a field larger than uint32")
	}
	if _, err := addEncodedLength(^uint64(0), 1); err == nil {
		t.Fatal("addEncodedLength accepted an overflowing total")
	}
}

func tls13States(t *testing.T) (tls.ConnectionState, tls.ConnectionState) {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{certificateDER}, PrivateKey: privateKey}

	clientRaw, serverRaw := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	if err := clientRaw.SetDeadline(deadline); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	if err := serverRaw.SetDeadline(deadline); err != nil {
		t.Fatalf("set server deadline: %v", err)
	}
	client := tls.Client(clientRaw, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // The generated certificate is scoped to this in-memory test.
	})
	server := tls.Server(serverRaw, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	})
	t.Cleanup(func() {
		_ = clientRaw.Close()
		_ = serverRaw.Close()
	})

	errCh := make(chan error, 2)
	go func() { errCh <- client.Handshake() }()
	go func() { errCh <- server.Handshake() }()
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("TLS handshake: %v", err)
		}
	}
	return client.ConnectionState(), server.ConnectionState()
}
