package tlsreload

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
)

const (
	watchBuffer = 128
	quietPeriod = 200 * time.Millisecond
	maximumWait = time.Second
)

var (
	retryDelays = [...]time.Duration{
		100 * time.Millisecond,
		250 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
	}
	errAtomicGenerationChanged = errors.New("TLS AtomicWriter generation changed during read")
	// ErrStopped reports that owned shutdown stopped the reloader.
	ErrStopped = errors.New("TLS reloader is stopped")
)

// Paths identifies the TLS files owned by a Reloader. Empty paths are ignored.
type Paths struct {
	CAFile   string
	CertFile string
	KeyFile  string
}

// Bundle is one validated TLS material snapshot.
type Bundle struct {
	Certificate *tls.Certificate
	CAPool      *x509.CertPool
	digest      [sha256.Size]byte
}

// Reloader loads and optionally watches one TLS material bundle.
type Reloader struct {
	role    string
	paths   Paths
	dirs    []string
	logger  zerolog.Logger
	publish func(*Bundle) error

	loadMu    sync.Mutex
	digest    [sha256.Size]byte
	published bool

	stateMu   sync.Mutex
	watcher   *fsnotify.Watcher
	cancel    context.CancelFunc
	done      chan struct{}
	waitErr   error
	starting  bool
	started   bool
	stopping  bool
	stopped   bool
	completed bool
}

// New freezes the input paths and creates a TLS material reloader.
func New(role string, paths Paths, logger zerolog.Logger, publish func(*Bundle) error) (*Reloader, error) {
	if publish == nil {
		return nil, errors.New("TLS reload publisher is nil")
	}
	if (paths.CertFile == "") != (paths.KeyFile == "") {
		return nil, errors.New("TLS certificate and key paths must be set together")
	}

	var err error
	if paths.CAFile, err = absolutePath(paths.CAFile); err != nil {
		return nil, fmt.Errorf("normalize TLS CA path: %w", err)
	}
	if paths.CertFile, err = absolutePath(paths.CertFile); err != nil {
		return nil, fmt.Errorf("normalize TLS certificate path: %w", err)
	}
	if paths.KeyFile, err = absolutePath(paths.KeyFile); err != nil {
		return nil, fmt.Errorf("normalize TLS key path: %w", err)
	}

	dirSet := make(map[string]struct{}, 3)
	for _, path := range []string{paths.CAFile, paths.CertFile, paths.KeyFile} {
		if path != "" {
			dirSet[filepath.Dir(path)] = struct{}{}
		}
	}
	dirs := make([]string, 0, len(dirSet))
	for dir := range dirSet {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	return &Reloader{
		role:    role,
		paths:   paths,
		dirs:    dirs,
		logger:  logger,
		publish: publish,
		done:    make(chan struct{}),
	}, nil
}

func absolutePath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}

// LoadInitial synchronously validates and publishes the initial bundle.
func (r *Reloader) LoadInitial() error {
	return r.load("initial", 0)
}

// PrepareStart rereads the frozen paths and, when requested, starts watching
// their parent directories.
func (r *Reloader) PrepareStart(ctx context.Context, watch bool) error {
	if !watch {
		r.stateMu.Lock()
		stopped := r.stopping || r.stopped
		r.stateMu.Unlock()
		if stopped {
			return ErrStopped
		}
		return r.load("startup", 0)
	}

	r.stateMu.Lock()
	if r.stopping || r.stopped {
		r.stateMu.Unlock()
		return ErrStopped
	}
	if r.starting || r.started {
		r.stateMu.Unlock()
		return errors.New("TLS reloader is already started")
	}
	r.starting = true
	r.stateMu.Unlock()

	watcher, err := fsnotify.NewBufferedWatcher(watchBuffer)
	if err != nil {
		r.logFailure("watch", 0, fmt.Errorf("create filesystem watcher: %w", err))
		r.failStart()
		return fmt.Errorf("create TLS filesystem watcher: %w", err)
	}
	for _, dir := range r.dirs {
		if err := watcher.Add(dir); err != nil {
			_ = watcher.Close()
			r.logFailure("watch", 0, fmt.Errorf("watch TLS parent directory: %w", err))
			r.failStart()
			return fmt.Errorf("watch TLS parent directory %q: %w", dir, err)
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	r.stateMu.Lock()
	if r.stopping || r.stopped {
		r.starting = false
		r.stateMu.Unlock()
		cancel()
		_ = watcher.Close()
		r.complete(nil)
		return ErrStopped
	}
	r.watcher = watcher
	r.cancel = cancel
	r.stateMu.Unlock()

	startupFailed := r.load("startup", 0) != nil
	r.stateMu.Lock()
	if r.stopping || r.stopped {
		r.starting = false
		r.stateMu.Unlock()
		cancel()
		_ = watcher.Close()
		r.complete(nil)
		return ErrStopped
	}
	r.starting = false
	r.started = true
	r.stateMu.Unlock()
	go r.run(runCtx, watcher, startupFailed)
	return nil
}

// Stop stops the owned watcher. It is safe to call more than once.
func (r *Reloader) Stop() {
	r.stateMu.Lock()
	if r.stopping || r.stopped {
		r.stateMu.Unlock()
		return
	}
	r.stopping = true
	cancel := r.cancel
	watcher := r.watcher
	starting := r.starting
	started := r.started
	r.stateMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if watcher != nil {
		_ = watcher.Close()
	}

	r.loadMu.Lock()
	r.stateMu.Lock()
	r.stopped = true
	r.stateMu.Unlock()
	r.loadMu.Unlock()
	if !starting && !started {
		r.complete(nil)
	}
}

// Wait waits for the watcher, returning only an unexpected fatal result.
func (r *Reloader) Wait() error {
	<-r.done
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.waitErr
}

func (r *Reloader) run(ctx context.Context, watcher *fsnotify.Watcher, retryStartup bool) {
	err := r.watch(ctx, watcher.Events, watcher.Errors, retryStartup)
	_ = watcher.Close()
	r.complete(err)
}

func (r *Reloader) watch(ctx context.Context, events <-chan fsnotify.Event, watcherErrors <-chan error, retryStartup bool) error {
	parents := make(map[string]struct{}, len(r.dirs))
	for _, dir := range r.dirs {
		parents[filepath.Clean(dir)] = struct{}{}
	}

	var quietTimer, maximumTimer, retryTimer *time.Timer
	defer func() {
		stopTimer(quietTimer)
		stopTimer(maximumTimer)
		stopTimer(retryTimer)
	}()

	mode := reloadIdle
	retryIndex := 0
	var maximumDeadline time.Time
	if retryStartup {
		mode = reloadRetry
		retryTimer = time.NewTimer(retryDelays[0])
	}

	for {
		var quietC, maximumC, retryC <-chan time.Time
		if quietTimer != nil {
			quietC = quietTimer.C
		}
		if maximumTimer != nil {
			maximumC = maximumTimer.C
		}
		if retryTimer != nil {
			retryC = retryTimer.C
		}

		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return r.fatal(errors.New("TLS filesystem watcher event channel closed"))
			}
			if _, watchedParent := parents[filepath.Clean(event.Name)]; watchedParent && event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				return r.fatal(fmt.Errorf("TLS watched parent was removed or renamed: %s", event.Name))
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 || mode == reloadRetry {
				continue
			}
			if mode == reloadIdle {
				mode = reloadDebounce
				quietTimer = time.NewTimer(quietPeriod)
				maximumTimer = time.NewTimer(maximumWait)
				maximumDeadline = time.Now().Add(maximumWait)
				continue
			}
			if !time.Now().Before(maximumDeadline) {
				quietTimer, maximumTimer, retryTimer, mode, retryIndex = r.reload(quietTimer, maximumTimer, 0)
				continue
			}
			resetTimer(quietTimer, quietPeriod)
		case err, ok := <-watcherErrors:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return r.fatal(errors.New("TLS filesystem watcher error channel closed"))
			}
			r.logFailure("watch", 0, fmt.Errorf("filesystem watcher: %w", err))
			if mode == reloadRetry {
				continue
			}
			if mode == reloadIdle {
				mode = reloadDebounce
				quietTimer = time.NewTimer(quietPeriod)
				maximumTimer = time.NewTimer(maximumWait)
				maximumDeadline = time.Now().Add(maximumWait)
			} else {
				resetTimer(quietTimer, quietPeriod)
			}
		case <-quietC:
			quietTimer, maximumTimer, retryTimer, mode, retryIndex = r.reload(quietTimer, maximumTimer, 0)
		case <-maximumC:
			quietTimer, maximumTimer, retryTimer, mode, retryIndex = r.reload(quietTimer, maximumTimer, 0)
		case <-retryC:
			retryTimer = nil
			attempt := retryIndex + 1
			if err := r.load("reload", attempt); err == nil {
				mode = reloadIdle
				retryIndex = 0
			} else if attempt < len(retryDelays) {
				retryIndex = attempt
				retryTimer = time.NewTimer(retryDelays[retryIndex])
			} else {
				mode = reloadIdle
				retryIndex = 0
			}
		}
	}
}

type reloadMode uint8

const (
	reloadIdle reloadMode = iota
	reloadDebounce
	reloadRetry
)

func (r *Reloader) reload(quiet, maximum *time.Timer, attempt int) (*time.Timer, *time.Timer, *time.Timer, reloadMode, int) {
	stopTimer(quiet)
	stopTimer(maximum)
	if err := r.load("reload", attempt); err == nil {
		return nil, nil, nil, reloadIdle, 0
	}
	return nil, nil, time.NewTimer(retryDelays[0]), reloadRetry, 0
}

func stopTimer(timer *time.Timer) {
	if timer != nil && !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	stopTimer(timer)
	timer.Reset(delay)
}

func (r *Reloader) fatal(err error) error {
	r.logFailure("watch", 0, err)
	return err
}

func (r *Reloader) failStart() {
	r.stateMu.Lock()
	r.starting = false
	r.stopped = true
	r.stateMu.Unlock()
	r.complete(nil)
}

func (r *Reloader) complete(err error) {
	r.stateMu.Lock()
	if r.completed {
		r.stateMu.Unlock()
		return
	}
	r.stopping = true
	r.stateMu.Unlock()

	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.completed {
		return
	}
	r.waitErr = err
	r.stopped = true
	r.completed = true
	close(r.done)
}

func (r *Reloader) load(phase string, attempt int) error {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	r.stateMu.Lock()
	stopped := r.stopping || r.stopped
	r.stateMu.Unlock()
	if stopped {
		return ErrStopped
	}

	bundle, err := loadBundle(r.paths)
	if err != nil {
		r.logFailure(phase, attempt, err)
		return err
	}
	if r.published && bundle.digest == r.digest {
		r.logSuccess(phase, attempt, false)
		return nil
	}
	if err := r.publish(bundle); err != nil {
		err = fmt.Errorf("publish TLS material: %w", err)
		r.logFailure(phase, attempt, err)
		return err
	}
	r.digest = bundle.digest
	r.published = true
	r.logSuccess(phase, attempt, true)
	return nil
}

func (r *Reloader) logSuccess(phase string, attempt int, changed bool) {
	r.logger.Info().
		Str("role", r.role).
		Str("phase", phase).
		Int("attempt", attempt).
		Bool("changed", changed).
		Msg("TLS material loaded")
}

func (r *Reloader) logFailure(phase string, attempt int, err error) {
	r.logger.Error().
		Err(err).
		Str("role", r.role).
		Str("phase", phase).
		Int("attempt", attempt).
		Bool("changed", false).
		Msg("TLS material load failed")
}

type materialFile struct {
	label string
	path  string
	data  []byte
}

func loadBundle(paths Paths) (*Bundle, error) {
	files := make([]materialFile, 0, 3)
	if paths.CAFile != "" {
		files = append(files, materialFile{label: "ca", path: paths.CAFile})
	}
	if paths.CertFile != "" {
		files = append(files,
			materialFile{label: "cert", path: paths.CertFile},
			materialFile{label: "key", path: paths.KeyFile},
		)
	}

	if projection, ok := atomicWriterProjection(files); ok {
		if err := readAtomicWriter(files, projection); err != nil {
			return nil, err
		}
	} else {
		for i := range files {
			data, err := os.ReadFile(files[i].path)
			if err != nil {
				return nil, fmt.Errorf("read TLS %s file: %w", files[i].label, err)
			}
			files[i].data = data
		}
	}

	bundle := &Bundle{}
	for _, file := range files {
		switch file.label {
		case "ca":
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(file.data) {
				return nil, errors.New("parse TLS CA file: no certificate found")
			}
			bundle.CAPool = pool
		case "cert":
			// Parsed together after all files have been collected.
		case "key":
			certPEM := findMaterial(files, "cert")
			certificate, err := tls.X509KeyPair(certPEM, file.data)
			if err != nil {
				return nil, errors.New("parse TLS certificate/key pair: invalid certificate or key")
			}
			bundle.Certificate = &certificate
		}
	}

	hash := sha256.New()
	var size [8]byte
	for _, file := range files {
		hash.Write([]byte(file.label))
		hash.Write([]byte{0})
		binary.BigEndian.PutUint64(size[:], uint64(len(file.data)))
		hash.Write(size[:])
		hash.Write(file.data)
	}
	copy(bundle.digest[:], hash.Sum(nil))
	return bundle, nil
}

func findMaterial(files []materialFile, label string) []byte {
	for _, file := range files {
		if file.label == label {
			return file.data
		}
	}
	return nil
}

type atomicProjection struct {
	dir       string
	relatives []string
}

func atomicWriterProjection(files []materialFile) (atomicProjection, bool) {
	if len(files) == 0 {
		return atomicProjection{}, false
	}
	dir := filepath.Dir(files[0].path)
	relatives := make([]string, len(files))
	for i, file := range files {
		if filepath.Dir(file.path) != dir {
			return atomicProjection{}, false
		}
		info, err := os.Lstat(file.path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return atomicProjection{}, false
		}
		target, err := os.Readlink(file.path)
		if err != nil || filepath.IsAbs(target) {
			return atomicProjection{}, false
		}
		target = filepath.Clean(target)
		relative, err := filepath.Rel("..data", target)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return atomicProjection{}, false
		}
		relatives[i] = relative
	}
	return atomicProjection{dir: dir, relatives: relatives}, true
}

func readAtomicWriter(files []materialFile, projection atomicProjection) error {
	dataLink := filepath.Join(projection.dir, "..data")
	generation, err := os.Readlink(dataLink)
	if err != nil {
		return fmt.Errorf("read TLS AtomicWriter generation: %w", err)
	}
	cleanGeneration := filepath.Clean(generation)
	if filepath.IsAbs(generation) || cleanGeneration == "." || cleanGeneration == ".." || filepath.Dir(cleanGeneration) != "." {
		return errors.New("read TLS AtomicWriter generation: invalid target")
	}
	generationDir := filepath.Join(projection.dir, cleanGeneration)
	info, err := os.Lstat(generationDir)
	if err != nil {
		return fmt.Errorf("read TLS AtomicWriter generation directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("read TLS AtomicWriter generation: target is not a directory")
	}

	for i := range files {
		data, err := os.ReadFile(filepath.Join(generationDir, projection.relatives[i]))
		if err != nil {
			return fmt.Errorf("read TLS %s file from AtomicWriter generation: %w", files[i].label, err)
		}
		files[i].data = data
	}
	after, err := os.Readlink(dataLink)
	if err != nil {
		return fmt.Errorf("recheck TLS AtomicWriter generation: %w", err)
	}
	if after != generation {
		return errAtomicGenerationChanged
	}
	return nil
}
