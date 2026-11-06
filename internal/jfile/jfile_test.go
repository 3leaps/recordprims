package jfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/3leaps/recordprims/internal/fsroot"
)

var testOpts = fsroot.Options{AllowTmpfs: true, AllowOverlay: true}

// newJournal returns an appender over a fresh root holding one journal file
// with the given initial content.
func newJournal(t *testing.T, initial string) (*Appender, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := fsroot.Open(dir, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	f, err := r.CreateExclusive("journal.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(initial); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return &Appender{Root: r, Opts: testOpts}, filepath.Join(dir, "journal.ndjson")
}

func content(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAppendWritesAtTheCut(t *testing.T) {
	a, path := newJournal(t, "{\"seq\":1}\n")
	if err := a.Append("journal.ndjson", 10, []byte("{\"seq\":2}\n"), true); err != nil {
		t.Fatal(err)
	}
	if got := content(t, path); got != "{\"seq\":1}\n{\"seq\":2}\n" {
		t.Fatalf("journal %q", got)
	}
}

func TestAppendRefusesAStaleCut(t *testing.T) {
	a, path := newJournal(t, "{\"seq\":1}\n")
	for _, cut := range []int64{0, 9, 11} {
		if err := a.Append("journal.ndjson", cut, []byte("{}\n"), true); !errors.Is(err, ErrCutMismatch) {
			t.Errorf("cut %d: %v, want ErrCutMismatch", cut, err)
		}
	}
	if got := content(t, path); got != "{\"seq\":1}\n" {
		t.Fatalf("journal changed: %q", got)
	}
}

func TestAppendRefusesMalformedLines(t *testing.T) {
	a, path := newJournal(t, "")
	for _, line := range []string{"", "{}", "{}\n{}\n", "\n{}\n"} {
		if err := a.Append("journal.ndjson", 0, []byte(line), true); err == nil {
			t.Errorf("line %q accepted", line)
		}
	}
	if got := content(t, path); got != "" {
		t.Fatalf("journal changed: %q", got)
	}
}

// A failure after the whole line is written is indeterminate, never rolled
// back: the entry may be durable.
func TestFailureAfterFullWriteIsIndeterminate(t *testing.T) {
	a, path := newJournal(t, "")
	a.Fault = func(p string) error {
		if p == "journal.line_written" {
			return errors.New("injected")
		}
		return nil
	}
	if err := a.Append("journal.ndjson", 0, []byte("{}\n"), true); !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("%v, want ErrIndeterminate", err)
	}
	if got := content(t, path); got != "{}\n" {
		t.Fatalf("journal %q: a fully written line must not be truncated", got)
	}
}
