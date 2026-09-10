package tlsreload

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
)

type testMaterial struct {
	ca, cert, key []byte
	serial        int64
}

func TestLoadPublishesChangesAndRetainsLastKnownGood(t *testing.T) {
	dir := t.TempDir()
	first := newTestMaterialWithExpiry(t, 1, time.Now().Add(time.Hour))
	second := newTestMaterialWithExpiry(t, 2, time.Now().Add(2*time.Hour))
	writeMaterial(t, dir, first)

	logs := newTestLogs()
	var published []*Bundle
	paths := materialPaths(dir)
	r, err := New("client", paths, zerolog.New(logs), func(bundle *Bundle) error {
		published = append(published, bundle)
		return nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paths.CAFile = "caller-mutated"
	if !filepath.IsAbs(r.paths.CAFile) || strings.Contains(r.paths.CAFile, "caller-mutated") {
		t.Fatalf("paths were not frozen as absolute values: %+v", r.paths)
	}

	if err := r.LoadInitial(); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if err := r.LoadInitial(); err != nil {
		t.Fatalf("unchanged load: %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("unchanged load published %d bundles, want 1", len(published))
	}
	assertLog(t, logs.recordsSnapshot()[0], "info", "initial", 0, true)
	assertLog(t, logs.recordsSnapshot()[1], "info", "initial", 0, false)

	const secret = "DISTINCTIVE-PRIVATE-MATERIAL"
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("-----BEGIN "+secret+"-----\nAAAA\n-----END "+secret+"-----\n"), 0600); err != nil {
		t.Fatalf("write invalid certificate: %v", err)
	}
	if err := r.LoadInitial(); err == nil || !strings.Contains(err.Error(), "invalid certificate or key") {
		t.Fatalf("invalid pair error = %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("invalid pair replaced last-known-good bundle")
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("parser-controlled PEM label leaked into logs")
	}

	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), second.cert, 0600); err != nil {
		t.Fatalf("restore certificate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), second.key, 0600); err != nil {
		t.Fatalf("replace key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), nil, 0600); err != nil {
		t.Fatalf("empty CA: %v", err)
	}
	if err := r.LoadInitial(); err == nil || !strings.Contains(err.Error(), "no certificate found") {
		t.Fatalf("empty CA error = %v", err)
	}
	if len(published) != 1 {
		t.Fatal("empty CA replaced last-known-good bundle")
	}
	if err := os.Remove(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatalf("remove CA: %v", err)
	}
	if err := r.LoadInitial(); err == nil || !strings.Contains(err.Error(), "read TLS ca file") {
		t.Fatalf("unreadable CA error = %v", err)
	}
	if len(published) != 1 {
		t.Fatal("unreadable CA replaced last-known-good bundle")
	}

	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), second.ca, 0600); err != nil {
		t.Fatalf("replace CA: %v", err)
	}
	if err := r.LoadInitial(); err != nil {
		t.Fatalf("changed load: %v", err)
	}
	if len(published) != 2 || bundleSerial(t, published[1]) != second.serial {
		t.Fatalf("changed load did not publish the second complete bundle")
	}
	if !published[1].CertificateNotAfter.After(published[0].CertificateNotAfter) || !published[1].CANotAfter.After(published[0].CANotAfter) {
		t.Fatalf("reloaded expiry metadata did not replace the initial values")
	}

	trailing := append(append([]byte(nil), second.ca...), []byte("-----BEGIN MALFORMED-----\n!\n-----END MALFORMED-----\n")...)
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), trailing, 0600); err != nil {
		t.Fatalf("write CA with malformed trailer: %v", err)
	}
	if err := r.LoadInitial(); err != nil {
		t.Fatalf("valid CA with malformed trailer: %v", err)
	}
	if len(published) != 3 {
		t.Fatalf("CA-compatible change published %d bundles, want 3", len(published))
	}

	for i, record := range logs.recordsSnapshot() {
		for _, field := range []string{"role", "phase", "attempt", "changed"} {
			if _, ok := record[field]; !ok {
				t.Fatalf("log %d lacks %q: %#v", i, field, record)
			}
		}
	}
}

func TestDigestCommitsOnlyAfterPublication(t *testing.T) {
	dir := t.TempDir()
	writeMaterial(t, dir, newTestMaterial(t, 10))
	wantErr := errors.New("publisher rejected candidate")
	calls := 0
	r, err := New("server", materialPaths(dir), zerolog.Nop(), func(*Bundle) error {
		calls++
		if calls == 1 {
			return wantErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.LoadInitial(); !errors.Is(err, wantErr) {
		t.Fatalf("first load error = %v, want publisher error", err)
	}
	if err := r.LoadInitial(); err != nil {
		t.Fatalf("retry same bytes: %v", err)
	}
	if err := r.LoadInitial(); err != nil {
		t.Fatalf("unchanged load: %v", err)
	}
	if calls != 2 {
		t.Fatalf("publisher called %d times, want 2", calls)
	}
}

func TestLoadBundleReportsEarliestCertificateExpiry(t *testing.T) {
	dir := t.TempDir()
	ignored := newTestMaterialWithExpiry(t, 10, time.Now().Add(-time.Hour))
	early := newTestMaterialWithExpiry(t, 11, time.Now().Add(time.Hour))
	late := newTestMaterialWithExpiry(t, 12, time.Now().Add(2*time.Hour))
	ignoredBlock, _ := pem.Decode(ignored.ca)
	ignoredBlock.Headers = map[string]string{"X-Ignored": "true"}
	late.ca = append(late.ca, pem.EncodeToMemory(ignoredBlock)...)
	late.ca = append(late.ca, early.ca...)
	late.cert = append(late.cert, early.cert...)
	writeMaterial(t, dir, late)

	bundle, err := loadBundle(materialPaths(dir))
	if err != nil {
		t.Fatalf("loadBundle: %v", err)
	}
	block, _ := pem.Decode(early.cert)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse expected certificate: %v", err)
	}
	if !bundle.CertificateNotAfter.Equal(certificate.NotAfter) || !bundle.CANotAfter.Equal(certificate.NotAfter) {
		t.Fatalf("expiry metadata = identity %v, CA %v; want %v", bundle.CertificateNotAfter, bundle.CANotAfter, certificate.NotAfter)
	}
}

func TestWatcherCoalescingCapAndRetry(t *testing.T) {
	t.Run("continuous events reach maximum wait", func(t *testing.T) {
		dir := t.TempDir()
		first := newTestMaterial(t, 20)
		second := newTestMaterial(t, 21)
		writeMaterial(t, dir, first)
		published := make(chan int64, 4)
		logs := newTestLogs()
		r := startPublishingTestReloader(t, dir, logs, published)

		stopNoise := make(chan struct{})
		noiseStarted := make(chan struct{})
		noiseErr := make(chan error, 1)
		go func() {
			close(noiseStarted)
			for i := 0; ; i++ {
				if err := os.WriteFile(filepath.Join(dir, "noise"), []byte(fmt.Sprint(i)), 0600); err != nil {
					noiseErr <- err
					return
				}
				select {
				case <-stopNoise:
					noiseErr <- nil
					return
				case <-time.After(25 * time.Millisecond):
				}
			}
		}()
		<-noiseStarted
		started := time.Now()
		writeMaterial(t, dir, second)
		serial := receiveSerial(t, published, 3*time.Second)
		elapsed := time.Since(started)
		close(stopNoise)
		if err := <-noiseErr; err != nil {
			t.Fatalf("write noise: %v", err)
		}
		if serial != second.serial {
			t.Fatalf("published serial %d, want %d", serial, second.serial)
		}
		if elapsed < 700*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("continuous-event reload took %v, want maximum-timer publication near 1s", elapsed)
		}
		r.Stop()
		if err := r.Wait(); err != nil {
			t.Fatalf("Wait after Stop: %v", err)
		}
	})

	t.Run("failed read uses fixed retry budget", func(t *testing.T) {
		dir := t.TempDir()
		first := newTestMaterial(t, 30)
		second := newTestMaterial(t, 31)
		writeMaterial(t, dir, first)
		logs := newTestLogs()
		published := make(chan int64, 4)
		r := startPublishingTestReloader(t, dir, logs, published)
		if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte("invalid retry key"), 0600); err != nil {
			t.Fatalf("write invalid key: %v", err)
		}
		logs.wait(t, 2*time.Second, func(record map[string]any) bool {
			return record["level"] == "error" && record["phase"] == "reload" && record["attempt"] == float64(0)
		})
		retryStarted := time.Now()
		writeMaterial(t, dir, second)
		if serial := receiveSerial(t, published, time.Second); serial != second.serial {
			t.Fatalf("retry published serial %d, want %d", serial, second.serial)
		}
		logs.wait(t, time.Second, func(record map[string]any) bool {
			return record["level"] == "info" && record["phase"] == "reload" && record["attempt"] == float64(1)
		})
		if elapsed := time.Since(retryStarted); elapsed > 750*time.Millisecond {
			t.Fatalf("first bounded retry took %v", elapsed)
		}
		r.Stop()
		if err := r.Wait(); err != nil {
			t.Fatalf("Wait after retry: %v", err)
		}
	})
}

func TestWatcherErrorAndCompletion(t *testing.T) {
	t.Run("disabled watch rereads until Stop", func(t *testing.T) {
		dir := t.TempDir()
		writeMaterial(t, dir, newTestMaterial(t, 39))
		r := newTestReloader(t, dir, newTestLogs(), func(*Bundle) error { return nil })
		if err := r.LoadInitial(); err != nil {
			t.Fatalf("initial load: %v", err)
		}
		result := make(chan error, 1)
		go func() { result <- r.Wait() }()
		assertBlocked(t, result)
		if err := r.PrepareStart(context.Background(), false); err != nil {
			t.Fatalf("disabled-watch startup reread: %v", err)
		}
		if r.watcher != nil {
			t.Fatal("disabled watch allocated a watcher")
		}
		if err := os.Remove(filepath.Join(dir, "tls.key")); err != nil {
			t.Fatalf("remove startup key: %v", err)
		}
		if err := r.PrepareStart(context.Background(), false); err == nil {
			t.Fatal("disabled-watch startup accepted an unreadable key")
		}
		assertBlocked(t, result)
		r.Stop()
		if err := receiveError(t, result, time.Second); err != nil {
			t.Fatalf("Wait after disabled-watch Stop: %v", err)
		}
	})

	t.Run("Stop before start completes without a watcher", func(t *testing.T) {
		dir := t.TempDir()
		writeMaterial(t, dir, newTestMaterial(t, 38))
		r := newTestReloader(t, dir, newTestLogs(), func(*Bundle) error { return nil })
		result := make(chan error, 1)
		go func() { result <- r.Wait() }()
		assertBlocked(t, result)
		r.Stop()
		if err := receiveError(t, result, time.Second); err != nil {
			t.Fatalf("Wait after pre-start Stop: %v", err)
		}
		if r.watcher != nil {
			t.Fatal("Stop before start allocated a watcher")
		}
		if err := r.PrepareStart(context.Background(), true); !errors.Is(err, ErrStopped) {
			t.Fatalf("PrepareStart after Stop = %v", err)
		}
	})

	t.Run("overflow schedules a full read", func(t *testing.T) {
		dir := t.TempDir()
		writeMaterial(t, dir, newTestMaterial(t, 40))
		logs := newTestLogs()
		r := newTestReloader(t, dir, logs, func(*Bundle) error { return nil })
		if err := r.LoadInitial(); err != nil {
			t.Fatalf("initial load: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		events := make(chan fsnotify.Event)
		watcherErrors := make(chan error)
		result := make(chan error, 1)
		go func() { result <- r.watch(ctx, events, watcherErrors, false) }()
		watcherErrors <- fsnotify.ErrEventOverflow
		logs.wait(t, time.Second, func(record map[string]any) bool {
			return record["level"] == "error" && record["phase"] == "watch" && strings.Contains(fmt.Sprint(record["error"]), "overflow")
		})
		logs.wait(t, time.Second, func(record map[string]any) bool {
			return record["level"] == "info" && record["phase"] == "reload" && record["changed"] == false
		})
		cancel()
		if err := receiveError(t, result, time.Second); err != nil {
			t.Fatalf("watch after cancellation: %v", err)
		}
	})

	t.Run("Wait before start observes unexpected watcher closure", func(t *testing.T) {
		dir := t.TempDir()
		writeMaterial(t, dir, newTestMaterial(t, 41))
		r := newTestReloader(t, dir, newTestLogs(), func(*Bundle) error { return nil })
		if err := r.LoadInitial(); err != nil {
			t.Fatalf("initial load: %v", err)
		}
		result := make(chan error, 1)
		go func() { result <- r.Wait() }()
		assertBlocked(t, result)
		if err := r.PrepareStart(context.Background(), true); err != nil {
			t.Fatalf("PrepareStart: %v", err)
		}
		if err := r.watcher.Close(); err != nil {
			t.Fatalf("close watcher: %v", err)
		}
		if err := receiveError(t, result, time.Second); err == nil || !strings.Contains(err.Error(), "channel closed") {
			t.Fatalf("Wait error = %v, want unexpected closure", err)
		}
	})

	t.Run("watched parent rename is fatal", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "watched")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatalf("mkdir watched parent: %v", err)
		}
		writeMaterial(t, dir, newTestMaterial(t, 42))
		r := newTestReloader(t, dir, newTestLogs(), func(*Bundle) error { return nil })
		if err := r.LoadInitial(); err != nil {
			t.Fatalf("initial load: %v", err)
		}
		if err := r.PrepareStart(context.Background(), true); err != nil {
			t.Fatalf("PrepareStart: %v", err)
		}
		if err := os.Rename(dir, filepath.Join(root, "removed")); err != nil {
			t.Fatalf("rename watched parent: %v", err)
		}
		if err := waitForReloader(t, r, 2*time.Second); err == nil || !strings.Contains(err.Error(), "parent") {
			t.Fatalf("Wait error = %v, want watched-parent failure", err)
		}
	})

	t.Run("Stop joins an in-flight publisher", func(t *testing.T) {
		dir := t.TempDir()
		writeMaterial(t, dir, newTestMaterial(t, 43))
		entered := make(chan struct{})
		release := make(chan struct{})
		r := newTestReloader(t, dir, newTestLogs(), func(*Bundle) error {
			close(entered)
			<-release
			return nil
		})
		waitResult := make(chan error, 1)
		go func() { waitResult <- r.Wait() }()
		assertBlocked(t, waitResult)
		loadResult := make(chan error, 1)
		go func() { loadResult <- r.LoadInitial() }()
		<-entered
		stopDone := make(chan struct{})
		go func() {
			r.Stop()
			close(stopDone)
		}()
		assertBlocked(t, stopDone)
		assertBlocked(t, waitResult)
		close(release)
		if err := receiveError(t, loadResult, time.Second); err != nil {
			t.Fatalf("in-flight load: %v", err)
		}
		select {
		case <-stopDone:
		case <-time.After(time.Second):
			t.Fatal("Stop did not join publisher")
		}
		if err := receiveError(t, waitResult, time.Second); err != nil {
			t.Fatalf("Wait after Stop: %v", err)
		}
	})

	t.Run("watch acquisition failure releases Wait", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "gone")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatalf("mkdir material directory: %v", err)
		}
		writeMaterial(t, dir, newTestMaterial(t, 44))
		r := newTestReloader(t, dir, newTestLogs(), func(*Bundle) error { return nil })
		if err := r.LoadInitial(); err != nil {
			t.Fatalf("initial load: %v", err)
		}
		result := make(chan error, 1)
		go func() { result <- r.Wait() }()
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("remove material directory: %v", err)
		}
		if err := r.PrepareStart(context.Background(), true); err == nil {
			t.Fatal("PrepareStart succeeded with missing watch parent")
		}
		if err := receiveError(t, result, time.Second); err != nil {
			t.Fatalf("Wait after acquisition failure: %v", err)
		}
	})
}

func TestAtomicWriterGenerationSwitchRetriesCompleteBundle(t *testing.T) {
	t.Run("rename publishes one complete generation", func(t *testing.T) {
		dir := t.TempDir()
		first := newTestMaterial(t, 50)
		second := newTestMaterial(t, 51)
		writeGeneration(t, dir, "..gen-one", first)
		writeGeneration(t, dir, "..gen-two", second)
		installProjection(t, dir, "..gen-one")

		published := make(chan int64, 4)
		logs := newTestLogs()
		r := newTestReloader(t, dir, logs, func(bundle *Bundle) error {
			published <- bundleSerial(t, bundle)
			return nil
		})
		if err := r.LoadInitial(); err != nil {
			t.Fatalf("initial AtomicWriter load: %v", err)
		}
		<-published
		if err := r.PrepareStart(context.Background(), true); err != nil {
			t.Fatalf("PrepareStart: %v", err)
		}
		waitResult := make(chan error, 1)
		go func() { waitResult <- r.Wait() }()
		time.Sleep(25 * time.Millisecond)
		switchProjection(t, dir, "..gen-two")
		select {
		case serial := <-published:
			if serial != second.serial {
				t.Fatalf("published serial %d, want %d", serial, second.serial)
			}
		case err := <-waitResult:
			t.Fatalf("watcher stopped before AtomicWriter publication: %v; logs: %s", err, logs.String())
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for AtomicWriter publication; logs: %s", logs.String())
		}
		r.Stop()
		if err := receiveError(t, waitResult, time.Second); err != nil {
			t.Fatalf("Wait after AtomicWriter rename: %v", err)
		}
	})

	t.Run("switch during read is rejected then retried", func(t *testing.T) {
		mkfifo, err := exec.LookPath("mkfifo")
		if err != nil {
			t.Skip("mkfifo is required to synchronize the AtomicWriter read")
		}
		dir := t.TempDir()
		first := newTestMaterial(t, 60)
		second := newTestMaterial(t, 61)
		writeGeneration(t, dir, "..gen-one", first)
		writeGeneration(t, dir, "..gen-two", second)
		installProjection(t, dir, "..gen-one")

		var serials []int64
		r := newTestReloader(t, dir, newTestLogs(), func(bundle *Bundle) error {
			serials = append(serials, bundleSerial(t, bundle))
			return nil
		})
		if err := r.LoadInitial(); err != nil {
			t.Fatalf("initial AtomicWriter load: %v", err)
		}
		fifo := filepath.Join(dir, "..gen-one", "tls.crt")
		if err := os.Remove(fifo); err != nil {
			t.Fatalf("remove generation certificate: %v", err)
		}
		if output, err := exec.Command(mkfifo, fifo).CombinedOutput(); err != nil {
			t.Fatalf("mkfifo: %v: %s", err, output)
		}

		writerReady := make(chan struct{})
		releaseWriter := make(chan struct{})
		writerResult := make(chan error, 1)
		go func() {
			file, err := os.OpenFile(fifo, os.O_WRONLY, 0)
			if err != nil {
				writerResult <- err
				return
			}
			close(writerReady)
			<-releaseWriter
			_, writeErr := file.Write(first.cert)
			closeErr := file.Close()
			writerResult <- errors.Join(writeErr, closeErr)
		}()
		loadResult := make(chan error, 1)
		go func() { loadResult <- r.LoadInitial() }()
		select {
		case <-writerReady:
		case err := <-writerResult:
			t.Fatalf("open FIFO writer: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("load did not reach the captured generation")
		}
		switchProjection(t, dir, "..gen-two")
		close(releaseWriter)
		if err := receiveError(t, writerResult, time.Second); err != nil {
			t.Fatalf("write blocked generation: %v", err)
		}
		if err := receiveError(t, loadResult, time.Second); !errors.Is(err, errAtomicGenerationChanged) {
			t.Fatalf("switching load error = %v", err)
		}
		if fmt.Sprint(serials) != "[60]" {
			t.Fatalf("rejected generation changed publications: %v", serials)
		}
		if err := r.LoadInitial(); err != nil {
			t.Fatalf("retry stable generation: %v", err)
		}
		if fmt.Sprint(serials) != "[60 61]" {
			t.Fatalf("retried AtomicWriter generations %v, want [60 61]", serials)
		}
		r.Stop()
		if err := r.Wait(); err != nil {
			t.Fatalf("Wait after AtomicWriter retry: %v", err)
		}
	})
}

type testLogs struct {
	mu      sync.Mutex
	raw     strings.Builder
	records []map[string]any
	notify  chan struct{}
}

func newTestLogs() *testLogs {
	return &testLogs{notify: make(chan struct{}, 1)}
}

func (logs *testLogs) Write(data []byte) (int, error) {
	record := make(map[string]any)
	if err := json.Unmarshal(data, &record); err != nil {
		return 0, err
	}
	logs.mu.Lock()
	_, _ = logs.raw.Write(data)
	logs.records = append(logs.records, record)
	logs.mu.Unlock()
	select {
	case logs.notify <- struct{}{}:
	default:
	}
	return len(data), nil
}

func (logs *testLogs) String() string {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return logs.raw.String()
}

func (logs *testLogs) recordsSnapshot() []map[string]any {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return append([]map[string]any(nil), logs.records...)
}

func (logs *testLogs) wait(t *testing.T, timeout time.Duration, match func(map[string]any) bool) map[string]any {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		for _, record := range logs.recordsSnapshot() {
			if match(record) {
				return record
			}
		}
		select {
		case <-logs.notify:
		case <-timer.C:
			t.Fatalf("timed out waiting for log; records: %s", logs.String())
		}
	}
}

func newTestReloader(t *testing.T, dir string, logs *testLogs, publish func(*Bundle) error) *Reloader {
	t.Helper()
	r, err := New("test", materialPaths(dir), zerolog.New(logs), publish)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func startPublishingTestReloader(t *testing.T, dir string, logs *testLogs, published chan int64) *Reloader {
	t.Helper()
	r := newTestReloader(t, dir, logs, func(bundle *Bundle) error {
		published <- bundleSerial(t, bundle)
		return nil
	})
	if err := r.LoadInitial(); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	<-published
	if err := r.PrepareStart(context.Background(), true); err != nil {
		t.Fatalf("PrepareStart: %v", err)
	}
	return r
}

func newTestMaterial(t *testing.T, serial int64) testMaterial {
	return newTestMaterialWithExpiry(t, serial, time.Now().Add(time.Hour))
}

func newTestMaterialWithExpiry(t *testing.T, serial int64, notAfter time.Time) testMaterial {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: fmt.Sprintf("test-%d", serial)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return testMaterial{
		ca:     cert,
		cert:   cert,
		key:    pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		serial: serial,
	}
}

func materialPaths(dir string) Paths {
	return Paths{
		CAFile:   filepath.Join(dir, "ca.crt"),
		CertFile: filepath.Join(dir, "tls.crt"),
		KeyFile:  filepath.Join(dir, "tls.key"),
	}
}

func writeMaterial(t *testing.T, dir string, material testMaterial) {
	t.Helper()
	for name, data := range map[string][]byte{
		"ca.crt": material.ca, "tls.crt": material.cert, "tls.key": material.key,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func writeGeneration(t *testing.T, root, name string, material testMaterial) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("mkdir generation: %v", err)
	}
	writeMaterial(t, dir, material)
}

func installProjection(t *testing.T, dir, generation string) {
	t.Helper()
	if err := os.Symlink(generation, filepath.Join(dir, "..data")); err != nil {
		t.Skipf("create AtomicWriter symlink: %v", err)
	}
	for _, name := range []string{"ca.crt", "tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatalf("create projected %s: %v", name, err)
		}
	}
}

func switchProjection(t *testing.T, dir, generation string) {
	t.Helper()
	temporary := filepath.Join(dir, "..data-next")
	if err := os.Symlink(generation, temporary); err != nil {
		t.Fatalf("stage AtomicWriter generation: %v", err)
	}
	// Give kqueue time to observe the temporary entry before rename; inotify
	// reports the fast create/rename pair without this scheduling allowance.
	time.Sleep(50 * time.Millisecond)
	if err := os.Rename(temporary, filepath.Join(dir, "..data")); err != nil {
		t.Fatalf("switch AtomicWriter generation: %v", err)
	}
}

func bundleSerial(t *testing.T, bundle *Bundle) int64 {
	t.Helper()
	if bundle.Certificate == nil || bundle.CAPool == nil || len(bundle.Certificate.Certificate) == 0 {
		t.Fatalf("incomplete bundle: %+v", bundle)
	}
	certificate, err := x509.ParseCertificate(bundle.Certificate.Certificate[0])
	if err != nil {
		t.Fatalf("parse published certificate: %v", err)
	}
	if _, err := certificate.Verify(x509.VerifyOptions{
		Roots:     bundle.CAPool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		t.Fatalf("published mixed generation: %v", err)
	}
	return certificate.SerialNumber.Int64()
}

func assertLog(t *testing.T, record map[string]any, level, phase string, attempt int, changed bool) {
	t.Helper()
	if record["level"] != level || record["role"] != "client" || record["phase"] != phase ||
		record["attempt"] != float64(attempt) || record["changed"] != changed {
		t.Fatalf("log = %#v, want level=%s phase=%s attempt=%d changed=%v", record, level, phase, attempt, changed)
	}
}

func receiveSerial(t *testing.T, values <-chan int64, timeout time.Duration) int64 {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(timeout):
		t.Fatal("timed out waiting for publication")
		return 0
	}
}

func receiveError(t *testing.T, values <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-values:
		return err
	case <-time.After(timeout):
		t.Fatal("timed out waiting for result")
		return nil
	}
}

func waitForReloader(t *testing.T, r *Reloader, timeout time.Duration) error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- r.Wait() }()
	return receiveError(t, result, timeout)
}

func assertBlocked[T any](t *testing.T, values <-chan T) {
	t.Helper()
	select {
	case <-values:
		t.Fatal("operation completed before its owner released it")
	case <-time.After(50 * time.Millisecond):
	}
}
