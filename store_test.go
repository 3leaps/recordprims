package recordprims

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/3leaps/recordprims/internal/fsroot"
)

// testFS admits the temp filesystems tests run on (tmpfs on Linux CI, overlay
// in containers).
var testFS = fsroot.Options{AllowTmpfs: true, AllowOverlay: true}

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

func newStoreDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func initStore(t *testing.T) string {
	t.Helper()
	dir := newStoreDir(t)
	if err := Init(dir, InitOptions{fs: testFS}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return dir
}

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, OpenOptions{fs: testFS})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestInitOpenRoundTrip(t *testing.T) {
	dir := initStore(t)
	s := openStore(t, dir)
	if len(s.StoreID()) != 32 {
		t.Fatalf("store_id %q", s.StoreID())
	}
	if s.IdempotencyWindow() != DefaultIdempotencyWindow {
		t.Fatalf("window %v", s.IdempotencyWindow())
	}
	if len(s.attemptKey) != keyLen || len(s.diagKey) != keyLen {
		t.Fatal("keys not loaded")
	}
}

func TestInitRecordsCustomWindow(t *testing.T) {
	dir := newStoreDir(t)
	if err := Init(dir, InitOptions{IdempotencyWindow: time.Minute, fs: testFS}); err != nil {
		t.Fatal(err)
	}
	if got := openStore(t, dir).IdempotencyWindow(); got != time.Minute {
		t.Fatalf("window %v, want 1m", got)
	}
}

func TestInitRefusesNegativeWindow(t *testing.T) {
	if err := Init(newStoreDir(t), InitOptions{IdempotencyWindow: -time.Second, fs: testFS}); !errors.Is(err, ErrSchema) {
		t.Fatalf("want ErrSchema, got %v", err)
	}
}

func TestInitRefusesNonEmptyDir(t *testing.T) {
	dir := newStoreDir(t)
	os.WriteFile(filepath.Join(dir, "x"), nil, 0o600)
	if err := Init(dir, InitOptions{fs: testFS}); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("want ErrStoreInvalid, got %v", err)
	}
}

func TestStoreIDsAreUnique(t *testing.T) {
	a := openStore(t, initStore(t))
	b := openStore(t, initStore(t))
	if a.StoreID() == b.StoreID() {
		t.Fatal("two stores share a store_id")
	}
	if string(a.attemptKey) == string(b.attemptKey) {
		t.Fatal("two stores share an attempt key")
	}
}

func TestOpenRefusesWrongKeyLength(t *testing.T) {
	dir := initStore(t)
	p := filepath.Join(dir, fileAttemptKey)
	os.Chmod(p, 0o600)
	os.WriteFile(p, []byte("short"), 0o600)
	if _, err := Open(dir, OpenOptions{fs: testFS}); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("want ErrStoreInvalid, got %v", err)
	}
}

func TestOpenRefusesPermissiveKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes not enforced on Windows")
	}
	dir := initStore(t)
	os.Chmod(filepath.Join(dir, fileDiagKey), 0o644)
	if _, err := Open(dir, OpenOptions{fs: testFS}); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("want ErrStoreInvalid, got %v", err)
	}
}

func TestOpenRefusesTamperedMetadata(t *testing.T) {
	dir := initStore(t)
	p := filepath.Join(dir, fileStore)
	os.WriteFile(p, []byte(`{"format":"something-else"}`), 0o600)
	if _, err := Open(dir, OpenOptions{fs: testFS}); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("want ErrStoreInvalid, got %v", err)
	}
}

func TestOpenRefusesMissingStore(t *testing.T) {
	if _, err := Open(newStoreDir(t), OpenOptions{fs: testFS}); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("want ErrStoreInvalid, got %v", err)
	}
}

func TestErrorsCarryNoPath(t *testing.T) {
	dir := newStoreDir(t)
	_, err := Open(dir, OpenOptions{fs: testFS})
	if err == nil {
		t.Fatal("expected an error")
	}
	if containsPath(err.Error(), dir) {
		t.Fatalf("error discloses the store path: %v", err)
	}
}

func containsPath(msg, dir string) bool {
	return len(dir) > 0 && (contains(msg, dir) || contains(msg, filepath.Base(dir)))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestWithLockIsExclusiveInProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("same-process exclusivity on Windows is covered by the lock package")
	}
	dir := initStore(t)
	a := openStore(t, dir)
	b := openStore(t, dir)
	held := make(chan struct{})
	done := make(chan struct{})
	go a.withLock(func() error {
		close(held)
		<-done
		return nil
	})
	<-held
	if err := b.withLock(func() error { return nil }); !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked while another handle holds the lock, got %v", err)
	}
	close(done)
}

func TestInstantCanonicalAndStrict(t *testing.T) {
	ts := time.Date(2026, 10, 25, 14, 0, 0, 123456000, time.FixedZone("x", 3600))
	if got := FormatInstant(ts); got != "2026-10-25T13:00:00.123456Z" {
		t.Fatalf("FormatInstant = %s", got)
	}
	if got := FormatInstant(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); got != "2026-01-01T00:00:00.000000Z" {
		t.Fatalf("zero fraction = %s", got)
	}
	if _, err := ParseInstant("2026-10-25T13:00:00.1234567Z"); !errors.Is(err, ErrSchema) {
		t.Fatalf("sub-microsecond input must be rejected, got %v", err)
	}
	if _, err := ParseInstant("2026-10-25T13:00:00.1234560Z"); err != nil {
		t.Fatalf("exactly representable input must parse, got %v", err)
	}
}
