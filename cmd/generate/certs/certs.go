package certs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	outputDir  string
	validYears int
	Cmd        = &cobra.Command{
		Use:   "certs",
		Short: "Generate mTLS certificates (CA, server, client)",
		RunE:  runGenerate,
	}
)

func init() {
	Cmd.Flags().StringVarP(&outputDir, "output", "o", "./certs", "output directory")
	Cmd.Flags().IntVarP(&validYears, "years", "y", 10, "certificate validity in years")
}

func runGenerate(cmd *cobra.Command, args []string) error {
	logger := log.With().Str("com", "generate").Logger()

	logger.Info().Str("dir", outputDir).Int("years", validYears).Msg("generating certificates")

	// 1. Generate CA
	logger.Info().Msg("generating CA certificate")
	caKey, caCert, err := GenerateCA(validYears)
	if err != nil {
		return fmt.Errorf("generate CA: %w", err)
	}

	// 2. Generate server certificate
	logger.Info().Msg("generating server certificate")
	serverKey, serverCert, err := GenerateServerCert(caKey, caCert, validYears)
	if err != nil {
		return fmt.Errorf("generate server cert: %w", err)
	}

	// 3. Generate client certificate
	logger.Info().Msg("generating client certificate")
	clientKey, clientCert, err := GenerateClientCert(caKey, caCert, validYears)
	if err != nil {
		return fmt.Errorf("generate client cert: %w", err)
	}

	// 4. Write to files
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	files := map[string][]byte{
		"ca.key":     EncodePrivateKey(caKey),
		"ca.crt":     EncodeCertificate(caCert),
		"server.key": EncodePrivateKey(serverKey),
		"server.crt": EncodeCertificate(serverCert),
		"client.key": EncodePrivateKey(clientKey),
		"client.crt": EncodeCertificate(clientCert),
	}

	for name, data := range files {
		path := filepath.Join(outputDir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
		logger.Info().Str("file", path).Msg("generated")
	}

	logger.Info().Msg("certificate generation complete")
	return nil
}

// GenerateCA generates a CA certificate
func GenerateCA(validYears int) (*rsa.PrivateKey, *x509.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"QMux CA"},
			CommonName:   "QMux Root CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(validYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse certificate: %w", err)
	}

	return key, cert, nil
}

// GenerateServerCert generates a server certificate
func GenerateServerCert(caKey *rsa.PrivateKey, caCert *x509.Certificate, validYears int) (*rsa.PrivateKey, *x509.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"QMux"},
			CommonName:   "QMux Server",
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().AddDate(validYears, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost", "*.local", "qmux-server"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse certificate: %w", err)
	}

	return key, cert, nil
}

// GenerateClientCert generates a client certificate
func GenerateClientCert(caKey *rsa.PrivateKey, caCert *x509.Certificate, validYears int) (*rsa.PrivateKey, *x509.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"QMux"},
			CommonName:   "QMux Client",
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().AddDate(validYears, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse certificate: %w", err)
	}

	return key, cert, nil
}

// EncodePrivateKey encodes a private key to PEM format
func EncodePrivateKey(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// EncodeCertificate encodes a certificate to PEM format
func EncodeCertificate(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	})
}
