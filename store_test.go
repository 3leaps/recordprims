package recordprims

import (
	"errors"
	"os"
	"path/filepath"
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
	prof := s.Profile()
	if prof.Runtime.Stratum != "journal-files" || prof.Runtime.BodyRemovalStream != DefaultBodyRemovalStream || prof.Runtime.CommitStrategy != "fsync_file_and_dir" {
		t.Fatalf("profile runtime %+v", prof.Runtime)
	}
}

func TestOpenRefusesV1StoreFormat(t *testing.T) {
	dir := initStore(t)
	p := filepath.Join(dir, fileStore)
	os.WriteFile(p, []byte(`{"format":"recordprims-fs/v1","store_id":"0123456789abcdef0123456789abcdef","key_generation":1,"created_at":"2026-11-10T00:00:00.000000Z","idempotency_window_ms":60000}`), 0o600)
	_, err := Open(dir, OpenOptions{fs: testFS})
	if !errors.Is(err, ErrStoreInvalid) || !contains(err.Error(), "pre-journal store format") {
		t.Fatalf("want ErrStoreInvalid pre-journal store format, got %v", err)
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
	dir := initStore(t)
	makePermissive(t, filepath.Join(dir, fileDiagKey))
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
}

// The canonical v1 spelling is the only one ParseInstant accepts. The table
// agrees with the contract's reference checker (the vendored v1 interpreter);
// rows taken from contract vectors name them.
func TestParseInstantCanonicalSpelling(t *testing.T) {
	cases := []struct {
		in  string
		ok  bool
		why string
	}{
		{"2026-10-20T00:00:01.000000Z", true, "canonical"},
		{"2026-10-25T13:00:00.123456Z", true, "six fractional digits"},
		{"2024-02-29T12:00:00.000000Z", true, "leap day"},
		{"0001-01-01T00:00:00.000000Z", true, "first year"},
		{"9999-12-31T23:59:59.999999Z", true, "last instant"},
		{"2026-10-20T00:00:03Z", false, "no fraction (vector instant-journal-at)"},
		{"2026-10-20T00:00:01.25Z", false, "two fractional digits (vector produced-at-instant-spelling)"},
		{"2026-10-20T00:00:03.0000000Z", false, "seven digits, trailing zero (vector instant-observed-at-seven-digits)"},
		{"2026-10-25T13:00:00.1234567Z", false, "seven digits"},
		{"2026-10-20T00:00:01.000000+00:00", false, "numeric offset (vector instant-stream-opened-offset)"},
		{"2026-10-20t00:00:01.000000Z", false, "lowercase t"},
		{"2026-10-20T00:00:01.000000z", false, "lowercase z"},
		{"2026-10-20 00:00:01.000000Z", false, "space separator"},
		{"2026-10-20T00:00:01,000000Z", false, "comma fraction"},
		{" 2026-10-20T00:00:01.000000Z", false, "leading space"},
		{"2026-10-20T00:00:01.000000Z\n", false, "trailing newline"},
		{"2026-02-30T00:00:01.000000Z", false, "February 30 (vector instant-impossible-date)"},
		{"2026-02-29T00:00:01.000000Z", false, "February 29 in a common year"},
		{"2026-10-20T24:00:00.000000Z", false, "hour 24 (vector instant-hour-24)"},
		{"2026-10-20T23:60:00.000000Z", false, "minute 60"},
		{"2026-12-31T23:59:60.000000Z", false, "leap second"},
		{"2026-13-01T00:00:00.000000Z", false, "month 13"},
		{"2026-00-01T00:00:00.000000Z", false, "month 00"},
		{"2026-10-00T00:00:00.000000Z", false, "day 00"},
		{"0000-01-01T00:00:00.000000Z", false, "year 0000"},
		{"\uff12026-10-20T00:00:01.000000Z", false, "non-ASCII digit"},
		{"", false, "empty"},
	}
	for _, c := range cases {
		got, err := ParseInstant(c.in)
		if c.ok {
			if err != nil {
				t.Errorf("%s: ParseInstant(%q) = %v, want accepted", c.why, c.in, err)
			} else if FormatInstant(got) != c.in {
				t.Errorf("%s: round trip %q -> %q", c.why, c.in, FormatInstant(got))
			}
			continue
		}
		if !errors.Is(err, ErrSchema) {
			t.Errorf("%s: ParseInstant(%q) = %v, want ErrSchema", c.why, c.in, err)
		}
	}
}
