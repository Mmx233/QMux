package certs

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func configureGenerator(t *testing.T, output string) {
	t.Helper()
	oldOutput, oldYears := outputDir, validYears
	oldNames := serverNames
	oldWrite, oldCommit := writeCertificate, commitCertificates
	outputDir, validYears = output, 1
	serverNames = []string{"certs.test"}
	writeCertificate = writeCertificateFile
	commitCertificates = commitCertificateDirectory
	t.Cleanup(func() {
		outputDir, validYears, serverNames = oldOutput, oldYears, oldNames
		writeCertificate, commitCertificates = oldWrite, oldCommit
	})
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lstat(%q) error = %v, want not exist", path, err)
	}
}

func TestWriteCertificateFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "certificate.pem")
	want := []byte("certificate data")

	if err := writeCertificateFile(path, want, 0644); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read certificate: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("certificate contents = %q, want %q", got, want)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat certificate: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("certificate mode = %v, want regular file", info.Mode())
	}
	if hasPOSIXModes() && info.Mode().Perm() != 0644 {
		t.Fatalf("certificate mode = %04o, want 0644", info.Mode().Perm())
	}

	if err := writeCertificateFile(path, []byte("replacement"), 0600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("exclusive rewrite error = %v, want fs.ErrExist", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read certificate after rejected rewrite: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("certificate changed after rejected rewrite: got %q, want %q", got, want)
	}

	invalid := filepath.Join(directory, "missing", "certificate.pem")
	if err := writeCertificateFile(invalid, want, 0644); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("invalid path error = %v, want fs.ErrNotExist", err)
	}
}

func TestRunGenerateCleansStageAfterWriteFailure(t *testing.T) {
	output := filepath.Join(t.TempDir(), "nested", "certs")
	configureGenerator(t, output)
	want := errors.New("injected write failure")
	var stage string
	writes := 0
	writeCertificate = func(path string, data []byte, mode fs.FileMode) error {
		stage = filepath.Dir(path)
		writes++
		if writes == 3 {
			return want
		}
		return writeCertificateFile(path, data, mode)
	}

	err := runGenerate(nil, nil)
	if !errors.Is(err, want) {
		t.Fatalf("runGenerate error = %v, want injected failure", err)
	}
	if writes != 3 {
		t.Fatalf("write calls = %d, want 3", writes)
	}
	if stage == "" {
		t.Fatal("write seam did not capture the staging directory")
	}
	assertMissing(t, output)
	assertMissing(t, stage)
}

func TestRunGenerateCommitCollisionPreservesWinner(t *testing.T) {
	output := filepath.Join(t.TempDir(), "nested", "certs")
	configureGenerator(t, output)
	want := []byte("winner")
	var stage string
	var winnerDirMode, winnerFileMode fs.FileMode
	commitCertificates = func(stageDir, finalDir string) error {
		stage = stageDir
		winner, err := os.MkdirTemp(filepath.Dir(finalDir), ".winner-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(winner) }()
		sentinel := filepath.Join(winner, "sentinel")
		if err := os.WriteFile(sentinel, want, 0600); err != nil {
			return err
		}
		if err := os.Chmod(sentinel, 0644); err != nil {
			return err
		}
		if err := os.Chmod(winner, 0750); err != nil {
			return err
		}
		if err := os.Rename(winner, finalDir); err != nil {
			return err
		}
		dirInfo, err := os.Lstat(finalDir)
		if err != nil {
			return err
		}
		fileInfo, err := os.Lstat(filepath.Join(finalDir, "sentinel"))
		if err != nil {
			return err
		}
		winnerDirMode, winnerFileMode = dirInfo.Mode(), fileInfo.Mode()
		return commitCertificateDirectory(stageDir, finalDir)
	}

	err := runGenerate(nil, nil)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("runGenerate error = %v, want fs.ErrExist", err)
	}
	if stage == "" {
		t.Fatal("commit seam did not capture the staging directory")
	}
	assertMissing(t, stage)

	got, err := os.ReadFile(filepath.Join(output, "sentinel"))
	if err != nil {
		t.Fatalf("read winner sentinel: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("winner sentinel = %q, want %q", got, want)
	}
	dirInfo, err := os.Lstat(output)
	if err != nil {
		t.Fatalf("stat winner directory: %v", err)
	}
	fileInfo, err := os.Lstat(filepath.Join(output, "sentinel"))
	if err != nil {
		t.Fatalf("stat winner sentinel: %v", err)
	}
	if dirInfo.Mode() != winnerDirMode || fileInfo.Mode() != winnerFileMode {
		t.Fatalf("winner modes changed: dir %v -> %v, file %v -> %v", winnerDirMode, dirInfo.Mode(), winnerFileMode, fileInfo.Mode())
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatalf("read winner directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "sentinel" {
		t.Fatalf("winner entries = %v, want only sentinel", entries)
	}
}
