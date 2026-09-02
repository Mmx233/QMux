//go:build unix

package certs

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestRunGenerateCreatesCompleteSecureGeneration(t *testing.T) {
	parent := t.TempDir()
	output := filepath.Join(parent, "missing", "nested", "certs")
	configureGenerator(t, output)
	assertMissing(t, filepath.Dir(output))

	oldUmask := syscall.Umask(0077)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	var stage string
	var initialStageMode, committedStageMode fs.FileMode
	writeCertificate = func(path string, data []byte, mode fs.FileMode) error {
		if stage == "" {
			stage = filepath.Dir(path)
			info, err := os.Lstat(stage)
			if err != nil {
				return err
			}
			initialStageMode = info.Mode()
		}
		return writeCertificateFile(path, data, mode)
	}
	commitCertificates = func(stageDir, finalDir string) error {
		info, err := os.Lstat(stageDir)
		if err != nil {
			return err
		}
		committedStageMode = info.Mode()
		return commitCertificateDirectory(stageDir, finalDir)
	}

	if err := runGenerate(nil, nil); err != nil {
		t.Fatalf("runGenerate: %v", err)
	}
	if !initialStageMode.IsDir() || initialStageMode.Perm() != 0700 {
		t.Fatalf("initial staging mode = %v, want drwx------", initialStageMode)
	}
	if !committedStageMode.IsDir() || committedStageMode.Perm() != 0755 {
		t.Fatalf("committed staging mode = %v, want drwxr-xr-x", committedStageMode)
	}
	assertMissing(t, stage)

	dirInfo, err := os.Lstat(output)
	if err != nil {
		t.Fatalf("stat output directory: %v", err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode().Perm() != 0755 {
		t.Fatalf("output mode = %v, want drwxr-xr-x", dirInfo.Mode())
	}

	wantModes := map[string]fs.FileMode{
		"ca.crt": 0644, "ca.key": 0600,
		"server.crt": 0644, "server.key": 0600,
		"client.crt": 0644, "client.key": 0600,
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatalf("read output directory: %v", err)
	}
	if len(entries) != len(wantModes) {
		t.Fatalf("output entries = %d, want %d: %v", len(entries), len(wantModes), entries)
	}
	for _, entry := range entries {
		wantMode, ok := wantModes[entry.Name()]
		if !ok {
			t.Fatalf("unexpected output file %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("stat output file %q: %v", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != wantMode {
			t.Fatalf("output file %q mode = %v, want regular %04o", entry.Name(), info.Mode(), wantMode)
		}
	}

	ca := parseCertificatePair(t, output, "ca")
	server := parseCertificatePair(t, output, "server")
	client := parseCertificatePair(t, output, "client")
	if err := ca.CheckSignatureFrom(ca); err != nil {
		t.Fatalf("verify CA self-signature: %v", err)
	}
	if err := server.CheckSignatureFrom(ca); err != nil {
		t.Fatalf("verify server signature: %v", err)
	}
	if err := client.CheckSignatureFrom(ca); err != nil {
		t.Fatalf("verify client signature: %v", err)
	}
}

func parseCertificatePair(t *testing.T, directory, name string) *x509.Certificate {
	t.Helper()
	certPEM, err := os.ReadFile(filepath.Join(directory, name+".crt"))
	if err != nil {
		t.Fatalf("read %s certificate: %v", name, err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(directory, name+".key"))
	if err != nil {
		t.Fatalf("read %s key: %v", name, err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse %s certificate pair: %v", name, err)
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("parse %s certificate: %v", name, err)
	}
	return certificate
}

func TestRunGenerateRejectsExistingTargetsWithoutMutation(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink", "dangling symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "certs")
			var paths []string
			switch kind {
			case "directory":
				if err := os.Mkdir(target, 0755); err != nil {
					t.Fatal(err)
				}
				key := filepath.Join(target, "ca.key")
				writeExistingFile(t, key, []byte("existing private key"), 0644)
				paths = []string{target, key}
			case "file":
				writeExistingFile(t, target, []byte("existing file"), 0644)
				paths = []string{target}
			case "symlink":
				backing := target + ".backing"
				writeExistingFile(t, backing, []byte("symlink backing"), 0644)
				if err := os.Symlink(backing, target); err != nil {
					t.Fatal(err)
				}
				paths = []string{target, backing}
			case "dangling symlink":
				if err := os.Symlink(target+".missing", target); err != nil {
					t.Fatal(err)
				}
				paths = []string{target}
			case "fifo":
				if err := syscall.Mkfifo(target, 0640); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, 0640); err != nil {
					t.Fatal(err)
				}
				paths = []string{target}
			}
			before := make([]pathSnapshot, len(paths))
			for i, path := range paths {
				before[i] = snapshotPath(t, path)
			}
			configureGenerator(t, target)
			writeCalls := 0
			writeCertificate = func(path string, data []byte, mode fs.FileMode) error {
				writeCalls++
				return writeCertificateFile(path, data, mode)
			}

			err := runGenerate(nil, nil)
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf("runGenerate error = %v, want fs.ErrExist", err)
			}
			if writeCalls != 0 {
				t.Fatalf("write calls = %d, want 0", writeCalls)
			}
			for _, snapshot := range before {
				snapshot.assertUnchanged(t)
			}
		})
	}
}

func writeExistingFile(t *testing.T, path string, data []byte, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

type pathSnapshot struct {
	path    string
	mode    fs.FileMode
	data    []byte
	link    string
	entries []string
}

func snapshotPath(t *testing.T, path string) pathSnapshot {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := pathSnapshot{path: path, mode: info.Mode()}
	switch {
	case info.Mode().IsRegular():
		snapshot.data, err = os.ReadFile(path)
	case info.Mode()&fs.ModeSymlink != 0:
		snapshot.link, err = os.Readlink(path)
	case info.IsDir():
		entries, readErr := os.ReadDir(path)
		err = readErr
		for _, entry := range entries {
			snapshot.entries = append(snapshot.entries, entry.Name())
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (snapshot pathSnapshot) assertUnchanged(t *testing.T) {
	t.Helper()
	current := snapshotPath(t, snapshot.path)
	if current.mode != snapshot.mode || !bytes.Equal(current.data, snapshot.data) || current.link != snapshot.link || !slices.Equal(current.entries, snapshot.entries) {
		t.Fatalf("%q changed: before=%+v after=%+v", snapshot.path, snapshot, current)
	}
}
