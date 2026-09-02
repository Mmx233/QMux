package certs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	outputDir   string
	validYears  int
	serverNames []string

	writeCertificate   = writeCertificateFile
	commitCertificates = commitCertificateDirectory

	Cmd = &cobra.Command{
		Use:   "certs",
		Short: "Generate mTLS certificates (CA, server, client)",
		RunE:  runGenerate,
	}
)

func init() {
	Cmd.Flags().StringVarP(&outputDir, "output", "o", "./certs", "output directory")
	Cmd.Flags().IntVarP(&validYears, "years", "y", 10, "certificate validity in years")
	Cmd.Flags().StringSliceVarP(&serverNames, "server-name", "s", nil, "server DNS names for the certificate (required, can be specified multiple times)")
	_ = Cmd.MarkFlagRequired("server-name")
}

func runGenerate(_ *cobra.Command, _ []string) (err error) {
	logger := log.With().Str("com", "generate").Logger()
	finalDir := filepath.Clean(outputDir)

	logger.Info().Str("dir", finalDir).Int("years", validYears).Msg("generating certificates")

	if err := requireMissing(finalDir); err != nil {
		return err
	}

	// 1. Generate CA
	logger.Info().Msg("generating CA certificate")
	caKey, caCert, err := GenerateCA(validYears)
	if err != nil {
		return fmt.Errorf("generate CA: %w", err)
	}

	// 2. Generate server certificate
	logger.Info().Strs("dns_names", serverNames).Msg("generating server certificate")
	serverKey, serverCert, err := GenerateServerCert(caKey, caCert, validYears, serverNames)
	if err != nil {
		return fmt.Errorf("generate server cert: %w", err)
	}

	// 3. Generate client certificate
	logger.Info().Msg("generating client certificate")
	clientKey, clientCert, err := GenerateClientCert(caKey, caCert, validYears)
	if err != nil {
		return fmt.Errorf("generate client cert: %w", err)
	}

	// 4. Write the complete generation beside its final path.
	parentDir := filepath.Dir(finalDir)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("create output parent directory %q: %w", parentDir, err)
	}
	stageDir, err := os.MkdirTemp(parentDir, "."+filepath.Base(finalDir)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create staging directory beside %q: %w", finalDir, err)
	}
	cleanupStage := true
	defer func() {
		if !cleanupStage {
			return
		}
		if cleanupErr := os.RemoveAll(stageDir); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup staging directory %q: %w", stageDir, cleanupErr))
		}
	}()
	if err := chmodDirectory(stageDir, 0700); err != nil {
		return fmt.Errorf("secure staging directory %q: %w", stageDir, err)
	}

	files := []struct {
		name string
		data []byte
		mode fs.FileMode
	}{
		{"ca.crt", EncodeCertificate(caCert), 0644},
		{"ca.key", EncodePrivateKey(caKey), 0600},
		{"server.crt", EncodeCertificate(serverCert), 0644},
		{"server.key", EncodePrivateKey(serverKey), 0600},
		{"client.crt", EncodeCertificate(clientCert), 0644},
		{"client.key", EncodePrivateKey(clientKey), 0600},
	}

	for _, file := range files {
		if err := writeCertificate(filepath.Join(stageDir, file.name), file.data, file.mode); err != nil {
			return fmt.Errorf("write %s: %w", file.name, err)
		}
	}
	if err := chmodDirectory(stageDir, 0755); err != nil {
		return fmt.Errorf("finalize staging directory %q: %w", stageDir, err)
	}
	if err := commitCertificates(stageDir, finalDir); err != nil {
		return fmt.Errorf("commit certificate directory %q: %w", finalDir, err)
	}
	cleanupStage = false

	for _, file := range files {
		logger.Info().Str("file", filepath.Join(finalDir, file.name)).Msg("generated")
	}

	logger.Info().Msg("certificate generation complete")
	return nil
}

func requireMissing(path string) error {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return fmt.Errorf("output target %q: %w", path, fs.ErrExist)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("inspect output target %q: %w", path, err)
	}
}

func chmodDirectory(path string, mode fs.FileMode) error {
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	if hasPOSIXModes() && info.Mode().Perm() != mode {
		return fmt.Errorf("mode is %04o, want %04o", info.Mode().Perm(), mode)
	}
	return nil
}

func writeCertificateFile(path string, data []byte, mode fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}

	var operationErr error
	if err := file.Chmod(mode); err != nil {
		operationErr = fmt.Errorf("chmod %q: %w", path, err)
	} else if written, err := file.Write(data); err != nil {
		operationErr = fmt.Errorf("write %q: %w", path, err)
	} else if written != len(data) {
		operationErr = fmt.Errorf("write %q: %w", path, io.ErrShortWrite)
	} else if info, err := file.Stat(); err != nil {
		operationErr = fmt.Errorf("stat %q: %w", path, err)
	} else if !info.Mode().IsRegular() {
		operationErr = fmt.Errorf("validate %q: not a regular file", path)
	} else if hasPOSIXModes() && info.Mode().Perm() != mode {
		operationErr = fmt.Errorf("validate %q: mode is %04o, want %04o", path, info.Mode().Perm(), mode)
	} else if err := file.Sync(); err != nil {
		operationErr = fmt.Errorf("sync %q: %w", path, err)
	}

	closeErr := file.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close %q: %w", path, closeErr)
	}
	return errors.Join(operationErr, closeErr)
}

func commitCertificateDirectory(stageDir, finalDir string) error {
	if err := requireMissing(finalDir); err != nil {
		return err
	}
	if err := os.Rename(stageDir, finalDir); err != nil {
		return fmt.Errorf("rename %q to %q: %w", stageDir, finalDir, err)
	}
	return nil
}

func hasPOSIXModes() bool {
	switch runtime.GOOS {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios", "linux", "netbsd", "openbsd", "solaris":
		return true
	default:
		return false
	}
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

// GenerateServerCert generates a server certificate with the specified DNS names
func GenerateServerCert(caKey *rsa.PrivateKey, caCert *x509.Certificate, validYears int, dnsNames []string) (*rsa.PrivateKey, *x509.Certificate, error) {
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
		DNSNames:    dnsNames,
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
