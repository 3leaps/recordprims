package recordprims

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/3leaps/recordprims/internal/canon"
	"github.com/3leaps/recordprims/internal/fsroot"
	"github.com/3leaps/recordprims/internal/jfile"
	"github.com/3leaps/recordprims/internal/journal"
	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/wire"
)

// Store format identifiers written to STORE.
const (
	storeFormat   = "recordprims-fs/v2"
	storeFormatV1 = "recordprims-fs/v1"
)

// keyLen is the length of attempt.key and diag.key: 32 bytes from the OS CSPRNG.
const keyLen = 32

// maxSafeInteger is the durable-record/v1 ceiling for every protocol integer,
// 2^53 - 1. The store never accepts, persists or emits a larger value, and an
// increment past it is refused, never wrapped.
const maxSafeInteger = 1<<53 - 1

// maxBodyLength bounds a staged body; a variable so tests can lower it.
var maxBodyLength int64 = maxSafeInteger

// maxJournalBytes bounds a single journal file read into memory.
const maxJournalBytes = 64 << 20

// Defaults.
const (
	DefaultIdempotencyWindow = 7 * 24 * time.Hour
	DefaultPendingBodyTTL    = time.Hour
	DefaultBodyRemovalStream = "bodies"
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
	// BodyRemovalStream names the store's designated body-removal stream in
	// profile.json. Empty selects DefaultBodyRemovalStream ("bodies").
	BodyRemovalStream string
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

// Profile is the durable-record/v1 stratum profile persisted in profile.json.
type Profile struct {
	Capabilities []string       `json:"capabilities"`
	Origin       string         `json:"origin"`
	Conformance  string         `json:"conformance"`
	Build        ProfileBuild   `json:"build"`
	Runtime      ProfileRuntime `json:"runtime"`
}

// ProfileBuild describes the immutable build capabilities of the binary.
type ProfileBuild struct {
	Name                 string   `json:"name"`
	Version              string   `json:"version"`
	Target               string   `json:"target,omitempty"`
	NativeDependencies   []string `json:"native_dependencies"`
	CompiledStrata       []string `json:"compiled_strata"`
	CompiledCapabilities []string `json:"compiled_capabilities"`
}

// ProfileRuntime describes the effective runtime configuration of the store.
type ProfileRuntime struct {
	Stratum                      string          `json:"stratum"`
	FailureModel                 string          `json:"failure_model"`
	CommitStrategy               string          `json:"commit_strategy,omitempty"`
	CommitNote                   string          `json:"commit_note,omitempty"`
	HistoryPolicy                string          `json:"history_policy"`
	EnabledCapabilities          []string        `json:"enabled_capabilities"`
	IdempotencyWindowMS          int64           `json:"idempotency_window_ms"`
	ClockSkewBoundMS             int64           `json:"clock_skew_bound_ms"`
	BodyRemovalStream            string          `json:"body_removal_stream,omitempty"`
	IncarnationAllocation        string          `json:"incarnation_allocation,omitempty"`
	QuarantinedDesignatedStreams []string        `json:"quarantined_designated_streams,omitempty"`
	Deletion                     ProfileDeletion `json:"deletion"`
}

// ProfileDeletion declares the store's supported deletion surfaces.
type ProfileDeletion struct {
	LiveHead        string `json:"live_head"`
	RetainedHistory string `json:"retained_history"`
	ManagedReplicas string `json:"managed_replicas"`
	ExternalBodies  string `json:"external_bodies"`
	Decryption      string `json:"decryption"`
	UnmanagedCopies string `json:"unmanaged_copies"`
}

// storeMeta is the STORE file.
type storeMeta struct {
	Format              string `json:"format"`
	StoreID             string `json:"store_id"`
	KeyGeneration       uint64 `json:"key_generation"`
	CreatedAt           string `json:"created_at"`
	ProfileDigest       string `json:"profile_digest,omitempty"`
	IdempotencyWindowMS int64  `json:"idempotency_window_ms"`
}

// Store is an open record store.
type Store struct {
	root        *fsroot.Root
	dir         string
	meta        storeMeta
	profile     Profile
	profileNode *wire.Node
	opts        OpenOptions
	fso         fsroot.Options
	appender    *jfile.Appender

	attemptKey []byte
	diagKey    []byte

	// mu serializes this handle's mutations in-process, so goroutines sharing
	// a Store wait for each other rather than failing fast on the file lock.
	mu sync.Mutex
	// releaseGID holds the goroutine ID currently inside a ReleaseSink.Frame
	// callback (0 when none), so same-goroutine reentry fails fast with
	// ErrSinkReentered without touching mu.
	releaseGID atomic.Uint64
	// bodyDirsReady records that the body directories exist and are verified.
	bodyDirsReady bool
	// hook, when set by tests, can fail an operation at a named phase.
	hook func(phase string) error
}

func streamHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:32]
}

func streamDir(id string) string {
	return dirStreams + "/" + streamHash(id)
}

func streamJournalFile(id string) string {
	return streamDir(id) + "/journal.ndjson"
}

func defaultProfile(windowMS int64, bodyStream string) Profile {
	return Profile{
		Capabilities: []string{"contract: durable-record/v1"},
		Origin:       "declared",
		Conformance:  "none",
		Build: ProfileBuild{
			Name:                 "recordprims",
			Version:              "0.0.1",
			Target:               runtime.GOARCH + "-" + runtime.GOOS,
			NativeDependencies:   []string{},
			CompiledStrata:       []string{"journal-files", "ndjson-cas"},
			CompiledCapabilities: []string{"revisioned", "body_ref", "tombstoning"},
		},
		Runtime: ProfileRuntime{
			Stratum:               "journal-files",
			FailureModel:          "survives_process_loss",
			CommitStrategy:        "fsync_file_and_dir",
			CommitNote:            "fsync file and directory before receipt",
			HistoryPolicy:         "append_only",
			EnabledCapabilities:   []string{"revisioned", "body_ref", "tombstoning"},
			IdempotencyWindowMS:   windowMS,
			ClockSkewBoundMS:      0,
			BodyRemovalStream:     bodyStream,
			IncarnationAllocation: "prewrite_durable_ledger",
			Deletion: ProfileDeletion{
				LiveHead:        "supported",
				RetainedHistory: "unsupported",
				ManagedReplicas: "not_applicable",
				ExternalBodies:  "supported",
				Decryption:      "not_applicable",
				UnmanagedCopies: "outside_claim",
			},
		},
	}
}

// Init creates a new, empty store at dir, which must exist, be empty, and be
// owned by the current user with no group or other permissions. It creates the
// store metadata, the two per-store keys, profile.json, and the designated
// body-removal stream before writing STORE.
func Init(dir string, opts InitOptions) error {
	window := opts.IdempotencyWindow
	if window == 0 {
		window = DefaultIdempotencyWindow
	}
	if window < 0 {
		return fmt.Errorf("%w: idempotency window must be positive", ErrSchema)
	}
	windowMS := window.Milliseconds()
	if windowMS < 1 || windowMS > maxSafeInteger {
		return fmt.Errorf("%w: idempotency window out of domain", ErrSchema)
	}
	bodyStream := opts.BodyRemovalStream
	if bodyStream == "" {
		bodyStream = DefaultBodyRemovalStream
	}
	if !reStream.MatchString(bodyStream) {
		return fmt.Errorf("%w: invalid body_removal_stream", ErrSchema)
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
	createdAt := FormatInstant(clock.Now())

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

	prof := defaultProfile(windowMS, bodyStream)
	profBytes, err := json.Marshal(prof)
	if err != nil {
		return err
	}
	profDigest, err := canon.Digest(profBytes)
	if err != nil {
		return err
	}
	if err := writeFileDurable(root, fileProfile, profBytes); err != nil {
		return err
	}

	// G7: Init makes the designated body-removal stream and its stream_opened
	// entry durable before STORE commits.
	app := jfile.New(root, opts.fs)
	if err := createStreamJournal(root, opts.fs, app, bodyStream, "single_revision", prof.Runtime.FailureModel, createdAt, nil, true); err != nil {
		return err
	}

	meta := storeMeta{
		Format:              storeFormat,
		StoreID:             hex.EncodeToString(id),
		KeyGeneration:       1,
		CreatedAt:           createdAt,
		ProfileDigest:       profDigest,
		IdempotencyWindowMS: windowMS,
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

func createStreamJournal(root *fsroot.Root, fso fsroot.Options, app *jfile.Appender, streamID, mode, failureModel, at string, continues *wire.Node, designated bool) error {
	sdir := streamDir(streamID)
	if err := root.Mkdir(sdir, fso); err != nil {
		return ioErr("mkdir", err, true)
	}
	if err := root.SyncDir(dirStreams); err != nil {
		return ioErr("fsync_dir", err, true)
	}
	jpath := streamJournalFile(streamID)
	f, err := root.CreateExclusive(jpath)
	if err != nil {
		return ioErr("create", err, true)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return ioErr("fsync", err, true)
	}
	if err := f.Close(); err != nil {
		return ioErr("close", err, true)
	}
	if err := root.SyncDir(sdir); err != nil {
		return ioErr("fsync_dir", err, true)
	}

	fields := []wire.Member{
		{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
		{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
		{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
		{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
		{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: streamID}},
		{Name: "type", Value: &wire.Node{Kind: wire.String, Str: "stream_opened"}},
		{Name: "at", Value: &wire.Node{Kind: wire.String, Str: at}},
		{Name: "mode", Value: &wire.Node{Kind: wire.String, Str: mode}},
		{Name: "failure_model", Value: &wire.Node{Kind: wire.String, Str: failureModel}},
	}
	if continues != nil {
		fields = append(fields, wire.Member{Name: "continues", Value: continues})
	}
	entry := &wire.Node{Kind: wire.Object, Fields: fields}
	line := append(wire.Encode(entry), '\n')
	if err := app.Append(jpath, 0, line, designated); err != nil {
		return mapJFileErr(err)
	}
	if err := root.SyncDir(sdir); err != nil {
		return ioErr("fsync_dir", err, true)
	}
	return nil
}

func mapJFileErr(err error) error {
	switch {
	case errors.Is(err, jfile.ErrAborted):
		return &IOError{Op: "write", Class: "no_space", indeterminate: false}
	case errors.Is(err, jfile.ErrCutMismatch):
		return ErrStaleCut
	default:
		return &IOError{Op: "append", Class: "other", indeterminate: true}
	}
}

// Open opens an existing store, verifying the root, the metadata, profile.json,
// and the keys. It never repairs anything.
func Open(dir string, opts OpenOptions) (*Store, error) {
	fso := opts.fs
	fso.AllowOverlay = fso.AllowOverlay || opts.AllowOverlay
	root, err := fsroot.Open(dir, fso)
	if err != nil {
		return nil, mapOpenErr(err)
	}
	s := &Store{
		root:     root,
		dir:      dir,
		opts:     opts,
		fso:      fso,
		appender: jfile.New(root, fso),
	}
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
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%w: store metadata invalid", ErrStoreInvalid)
	}
	// G5: refuse pre-journal v1 store format with a distinct reason.
	if m.Format == storeFormatV1 {
		return fmt.Errorf("%w: pre-journal store format", ErrStoreInvalid)
	}
	if m.Format != storeFormat || len(m.StoreID) != 32 || m.KeyGeneration == 0 || m.IdempotencyWindowMS <= 0 || m.IdempotencyWindowMS > maxSafeInteger {
		return fmt.Errorf("%w: store metadata invalid", ErrStoreInvalid)
	}
	s.meta = m

	profBytes, err := readFileVerified(s.root, fileProfile, s.fso, 1<<16)
	if err != nil {
		return fmt.Errorf("%w: store profile unreadable", ErrStoreInvalid)
	}
	if m.ProfileDigest != "" {
		gotDigest, err := canon.Digest(profBytes)
		if err != nil || gotDigest != m.ProfileDigest {
			return fmt.Errorf("%w: store profile digest mismatch", ErrStoreInvalid)
		}
	}
	profNode, err := wire.Ingest(profBytes, wire.KindStratumProfile)
	if err != nil {
		return fmt.Errorf("%w: store profile invalid", ErrStoreInvalid)
	}
	if err := shape.Validate("v1/stratum-profile.schema.json", profNode); err != nil {
		return fmt.Errorf("%w: store profile schema invalid", ErrStoreInvalid)
	}
	if err := journal.CheckProfile(profNode); err != nil {
		return fmt.Errorf("%w: store profile check failed", ErrStoreInvalid)
	}
	var prof Profile
	if err := json.Unmarshal(profBytes, &prof); err != nil {
		return fmt.Errorf("%w: store profile invalid", ErrStoreInvalid)
	}
	if prof.Runtime.IdempotencyWindowMS != m.IdempotencyWindowMS || prof.Runtime.BodyRemovalStream == "" {
		return fmt.Errorf("%w: store profile mismatch", ErrStoreInvalid)
	}
	s.profile = prof
	s.profileNode = profNode

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

// Profile returns a copy of the store's declared stratum profile.
func (s *Store) Profile() Profile { return s.profile }

// IdempotencyWindow returns the store's fixed key window.
func (s *Store) IdempotencyWindow() time.Duration {
	return time.Duration(s.meta.IdempotencyWindowMS) * time.Millisecond
}

func (s *Store) windowNode() *wire.Node {
	tok := strconv.FormatInt(s.meta.IdempotencyWindowMS, 10)
	return &wire.Node{Kind: wire.Number, Token: tok, IsInt: true, Int: s.meta.IdempotencyWindowMS}
}

func currentGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	const prefix = "goroutine "
	if n <= len(prefix) {
		return 0
	}
	var id uint64
	for i := len(prefix); i < n; i++ {
		c := buf[i]
		if c < '0' || c > '9' {
			break
		}
		id = id*10 + uint64(c-'0')
	}
	return id
}

// withLock runs fn under the store-wide lock: the handle's in-process mutex,
// then the cross-process file lock.
func (s *Store) withLock(fn func() error) error {
	if gid := s.releaseGID.Load(); gid != 0 && gid == currentGoroutineID() {
		return ErrSinkReentered
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
