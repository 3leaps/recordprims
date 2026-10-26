package recordprims

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/3leaps/recordprims/internal/fsroot"
)

// Store format identifier written to STORE.
const storeFormat = "recordprims-fs/v1"

// keyLen is the length of attempt.key and diag.key: 32 bytes from the OS CSPRNG.
const keyLen = 32

// Defaults.
const (
	DefaultIdempotencyWindow = 7 * 24 * time.Hour
	DefaultPendingBodyTTL    = time.Hour
)

// Store file names, relative to the store root.
const (
	fileStore      = "STORE"
	fileProfile    = "profile.json"
	fileLock       = "lock"
	fileAttemptKey = "attempt.key"
	fileDiagKey    = "diag.key"
	dirStreams     = "streams"
	dirBodies      = "bodies"
)

// Clock supplies acceptance times. Tests inject a fake; the default is the
// system clock.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// InitOptions configure a new store. They are fixed at creation.
type InitOptions struct {
	// IdempotencyWindow is how long an accepted idempotency key stays live. It
	// must be positive (durable-record/v1 forbids none). Zero selects the
	// default of seven days.
	IdempotencyWindow time.Duration
	// Clock stamps created_at; nil uses the system clock.
	Clock Clock

	// fs carries filesystem verification overrides for tests.
	fs fsroot.Options
}

// OpenOptions configure an open store.
type OpenOptions struct {
	// LockTimeout bounds how long a mutation waits for the store lock held by
	// another process. Zero fails fast with ErrLocked.
	LockTimeout time.Duration
	// Clock stamps acceptance times; nil uses the system clock.
	Clock Clock
	// PendingBodyTTL bounds an unconsumed body publication lease. Zero selects
	// the default of one hour.
	PendingBodyTTL time.Duration
	// AllowOverlay admits overlay filesystems (CI containers).
	AllowOverlay bool

	fs fsroot.Options
}

// storeMeta is the STORE file.
type storeMeta struct {
	Format              string `json:"format"`
	StoreID             string `json:"store_id"`
	KeyGeneration       uint64 `json:"key_generation"`
	CreatedAt           string `json:"created_at"`
	IdempotencyWindowMS int64  `json:"idempotency_window_ms"`
}

// Store is an open record store.
type Store struct {
	root *fsroot.Root
	dir  string
	meta storeMeta
	opts OpenOptions
	fso  fsroot.Options

	attemptKey []byte
	diagKey    []byte
}

// Init creates a new, empty store at dir, which must exist, be empty, and be
// owned by the current user with no group or other permissions. It creates the
// store metadata, the two per-store keys and the directory layout.
func Init(dir string, opts InitOptions) error {
	window := opts.IdempotencyWindow
	if window == 0 {
		window = DefaultIdempotencyWindow
	}
	if window < 0 {
		return fmt.Errorf("%w: idempotency window must be positive", ErrSchema)
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	root, err := fsroot.Open(dir, opts.fs)
	if err != nil {
		return mapOpenErr(err)
	}
	defer root.Close()

	empty, err := isEmptyDir(dir)
	if err != nil {
		return ioErr("init", err, false)
	}
	if !empty {
		return fmt.Errorf("%w: directory is not empty", ErrStoreInvalid)
	}

	id := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, id); err != nil {
		return fmt.Errorf("recordprims: entropy unavailable: %w", err)
	}
	meta := storeMeta{
		Format:              storeFormat,
		StoreID:             hex.EncodeToString(id),
		KeyGeneration:       1,
		CreatedAt:           FormatInstant(clock.Now()),
		IdempotencyWindowMS: window.Milliseconds(),
	}

	// Keys first: generated from the CSPRNG and created exclusively, so a
	// failure leaves no usable half-store.
	for _, name := range []string{fileAttemptKey, fileDiagKey} {
		if err := writeKey(root, name); err != nil {
			return err
		}
	}
	for _, d := range []string{dirStreams, dirBodies} {
		if err := root.Mkdir(d, opts.fs); err != nil {
			return ioErr("mkdir", err, true)
		}
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	// STORE is written last: its presence marks a complete store.
	if err := writeFileDurable(root, fileStore, b); err != nil {
		return err
	}
	return nil
}

// Open opens an existing store, verifying the root, the metadata and the keys.
// It never repairs anything.
func Open(dir string, opts OpenOptions) (*Store, error) {
	fso := opts.fs
	fso.AllowOverlay = fso.AllowOverlay || opts.AllowOverlay
	root, err := fsroot.Open(dir, fso)
	if err != nil {
		return nil, mapOpenErr(err)
	}
	s := &Store{root: root, dir: dir, opts: opts, fso: fso}
	if s.opts.Clock == nil {
		s.opts.Clock = systemClock{}
	}
	if s.opts.PendingBodyTTL == 0 {
		s.opts.PendingBodyTTL = DefaultPendingBodyTTL
	}
	if err := s.load(); err != nil {
		root.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := readFileVerified(s.root, fileStore, s.fso, 1<<16)
	if err != nil {
		return fmt.Errorf("%w: store metadata unreadable", ErrStoreInvalid)
	}
	var m storeMeta
	if err := json.Unmarshal(b, &m); err != nil || m.Format != storeFormat || len(m.StoreID) != 32 || m.KeyGeneration == 0 || m.IdempotencyWindowMS <= 0 {
		return fmt.Errorf("%w: store metadata invalid", ErrStoreInvalid)
	}
	s.meta = m
	if s.attemptKey, err = readKey(s.root, fileAttemptKey, s.fso); err != nil {
		return err
	}
	if s.diagKey, err = readKey(s.root, fileDiagKey, s.fso); err != nil {
		return err
	}
	return nil
}

// Close releases the store.
func (s *Store) Close() error {
	if s.root == nil {
		return ErrClosed
	}
	err := s.root.Close()
	s.root = nil
	return err
}

// StoreID returns the store's random identifier.
func (s *Store) StoreID() string { return s.meta.StoreID }

// IdempotencyWindow returns the store's fixed key window.
func (s *Store) IdempotencyWindow() time.Duration {
	return time.Duration(s.meta.IdempotencyWindowMS) * time.Millisecond
}

// withLock runs fn under the store-wide cross-process lock.
func (s *Store) withLock(fn func() error) error {
	if s.root == nil {
		return ErrClosed
	}
	release, err := s.acquireStoreLock()
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// writeKey creates a new key file exclusively with fresh CSPRNG bytes.
func writeKey(root *fsroot.Root, name string) error {
	k := make([]byte, keyLen)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return fmt.Errorf("recordprims: entropy unavailable: %w", err)
	}
	return writeFileDurable(root, name, k)
}

// readKey reads and validates a key file: a verified regular file of exactly
// keyLen bytes. It never repairs or replaces a key.
func readKey(root *fsroot.Root, name string, fso fsroot.Options) ([]byte, error) {
	b, err := readFileVerified(root, name, fso, keyLen+1)
	if err != nil {
		return nil, fmt.Errorf("%w: key %s failed validation", ErrStoreInvalid, name)
	}
	if len(b) != keyLen {
		return nil, fmt.Errorf("%w: key %s has the wrong length", ErrStoreInvalid, name)
	}
	return b, nil
}

// writeFileDurable creates name exclusively, writes data, fsyncs the file and
// then the containing directory.
func writeFileDurable(root *fsroot.Root, name string, data []byte) error {
	f, err := root.CreateExclusive(name)
	if err != nil {
		return ioErr("create", err, false)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return ioErr("write", err, true)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return ioErr("fsync", err, true)
	}
	if err := f.Close(); err != nil {
		return ioErr("close", err, true)
	}
	dir := filepath.ToSlash(filepath.Dir(name))
	if err := root.SyncDir(dir); err != nil {
		return ioErr("fsync_dir", err, true)
	}
	return nil
}

// readFileVerified reads a verified regular file, refusing anything larger
// than max bytes.
func readFileVerified(root *fsroot.Root, name string, fso fsroot.Options, max int64) ([]byte, error) {
	f, err := root.OpenRead(name, fso)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("file exceeds %d bytes", max)
	}
	return b, nil
}

func isEmptyDir(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	return false, err
}

// mapOpenErr maps root verification failures to ErrStoreInvalid, keeping the
// reason as a class without the path.
func mapOpenErr(err error) error {
	switch {
	case errors.Is(err, fsroot.ErrSymlink):
		return fmt.Errorf("%w: symlink", ErrStoreInvalid)
	case errors.Is(err, fsroot.ErrSpecialFile):
		return fmt.Errorf("%w: not a directory", ErrStoreInvalid)
	case errors.Is(err, fsroot.ErrOwner):
		return fmt.Errorf("%w: wrong owner", ErrStoreInvalid)
	case errors.Is(err, fsroot.ErrMode):
		return fmt.Errorf("%w: group or other permissions", ErrStoreInvalid)
	case errors.Is(err, fsroot.ErrFSType):
		return fmt.Errorf("%w: filesystem type not allowed", ErrStoreInvalid)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: not found", ErrStoreInvalid)
	}
	return ioErr("open", err, false)
}

// ioErr wraps a filesystem error as a path-free IOError.
func ioErr(op string, err error, indeterminate bool) error {
	class := "other"
	switch {
	case errors.Is(err, fs.ErrNotExist):
		class = "not_found"
	case errors.Is(err, fs.ErrPermission):
		class = "permission"
	case errors.Is(err, fs.ErrExist):
		class = "exists"
	}
	return &IOError{Op: op, Class: class, indeterminate: indeterminate}
}
