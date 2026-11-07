//go:build recordprims_faultpoints

package jfile

import (
	"bytes"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// writeErr is how a real short write reports errno: wrapped in a PathError.
func writeErr(errno error) error {
	return &os.PathError{Op: "write", Path: "journal.ndjson", Err: errno}
}

// injected is a fileOps that fails the chosen operation and passes the rest
// to the real calls. A short write really writes its first bytes.
type injected struct {
	shortWrite   int   // >= 0: write only this many bytes, then fail
	shortNoErr   bool  // with shortWrite: report the short count with a nil error
	shortErr     error // with shortWrite: the error reported; nil means the device is full
	noopTrunc    bool  // truncate reports success without truncating
	fullWriteErr bool  // write everything, then report an error
	badCount     bool  // report more bytes than requested
	truncateErr  bool
	syncErr      int // fail the n-th sync (1-based); 0 never
	syncs        int
}

func (o *injected) write(f *os.File, p []byte) (int, error) {
	switch {
	case o.badCount:
		n, _ := f.Write(p)
		return n + 1, nil
	case o.fullWriteErr:
		n, _ := f.Write(p)
		return n, errors.New("injected write error after full write")
	case o.shortWrite >= 0:
		n, _ := f.Write(p[:o.shortWrite])
		if o.shortNoErr {
			return n, nil
		}
		if o.shortErr != nil {
			return n, o.shortErr
		}
		return n, writeErr(spaceErrnos[0])
	}
	return f.Write(p)
}

func (o *injected) truncate(f *os.File, size int64) error {
	if o.truncateErr {
		return errors.New("injected truncate error")
	}
	if o.noopTrunc {
		return nil
	}
	return f.Truncate(size)
}

func (o *injected) sync(f *os.File) error {
	o.syncs++
	if o.syncErr == o.syncs {
		return errors.New("injected sync error")
	}
	return f.Sync()
}

func with(t *testing.T, o fileOps) {
	t.Helper()
	prev := ops
	ops = o
	t.Cleanup(func() { ops = prev })
}

const prior = "{\"seq\":1}\n"

var line = []byte("{\"seq\":2,\"type\":\"body_published\"}\n")

// The entarch-ruled abort matrix: only a proven partial write on the
// designated stream is rolled back, under the lock, to the recorded cut.
func TestAbortMatrix(t *testing.T) {
	for _, c := range []struct {
		name       string
		ops        injected
		designated bool
		want       error
		after      string // journal content afterwards
		fault      string // phase at which Fault fails
	}{
		{name: "short write, designated: aborted to the cut", ops: injected{shortWrite: 7}, designated: true, want: ErrAborted, after: prior},
		{name: "zero-byte write, designated: aborted", ops: injected{shortWrite: 0}, designated: true, want: ErrAborted, after: prior},
		{name: "short write, ordinary stream: no abort", ops: injected{shortWrite: 7}, designated: false, want: ErrIndeterminate, after: prior + string(line[:7])},
		{name: "full write then fsync failure: never truncated", ops: injected{shortWrite: -1, syncErr: 1}, designated: true, want: ErrIndeterminate, after: prior + string(line)},
		{name: "full write reporting an error: never truncated", ops: injected{shortWrite: -1, fullWriteErr: true}, designated: true, want: ErrIndeterminate, after: prior + string(line)},
		{name: "untrusted write count: never truncated", ops: injected{shortWrite: -1, badCount: true}, designated: true, want: ErrIndeterminate, after: prior + string(line)},
		{name: "truncate fails: poisoned", ops: injected{shortWrite: 7, truncateErr: true}, designated: true, want: ErrPoisoned, after: prior + string(line[:7])},
		{name: "fsync after truncate fails: poisoned", ops: injected{shortWrite: 7, syncErr: 1}, designated: true, want: ErrPoisoned, after: prior},
		{name: "kill point after truncate: poisoned", ops: injected{shortWrite: 7}, designated: true, want: ErrPoisoned, after: prior, fault: "journal.abort_truncated"},
		{name: "kill point after abort fsync: poisoned", ops: injected{shortWrite: 7}, designated: true, want: ErrPoisoned, after: prior, fault: "journal.abort_fsynced"},
		{name: "truncate reports success but the file is not at the cut: poisoned", ops: injected{shortWrite: 7, noopTrunc: true}, designated: true, want: ErrPoisoned, after: prior + string(line[:7])},
		{name: "short count with a nil error, designated: aborted", ops: injected{shortWrite: 7, shortNoErr: true}, designated: true, want: ErrAborted, after: prior},
		{name: "short write reporting io.ErrShortWrite, designated: aborted", ops: injected{shortWrite: 7, shortErr: io.ErrShortWrite}, designated: true, want: ErrAborted, after: prior},
		{name: "short write with an I/O error: never truncated", ops: injected{shortWrite: 7, shortErr: writeErr(syscall.EIO)}, designated: true, want: ErrIndeterminate, after: prior + string(line[:7])},
		{name: "zero-byte write with an I/O error: never truncated", ops: injected{shortWrite: 0, shortErr: writeErr(syscall.EIO)}, designated: true, want: ErrIndeterminate, after: prior},
		{name: "short write with an interrupted call: never truncated", ops: injected{shortWrite: 7, shortErr: writeErr(syscall.EINTR)}, designated: true, want: ErrIndeterminate, after: prior + string(line[:7])},
		{name: "short write with an unclassified error: never truncated", ops: injected{shortWrite: 7, shortErr: errors.New("unknown failure")}, designated: true, want: ErrIndeterminate, after: prior + string(line[:7])},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, path := newJournal(t, prior)
			o := c.ops
			with(t, &o)
			if c.fault != "" {
				a.faultHook = func(p string) error {
					if p == c.fault {
						return errors.New("injected kill point")
					}
					return nil
				}
			}
			err := a.Append("journal.ndjson", int64(len(prior)), line, c.designated)
			if !errors.Is(err, c.want) {
				t.Fatalf("%v, want %v", err, c.want)
			}
			if got := content(t, path); got != c.after {
				t.Fatalf("journal %q, want %q", got, c.after)
			}
			// Poisoned and indeterminate outcomes latch on every stream: the next
			// Append returns ErrPoisoned and leaves the file byte-identical, even
			// with healthy file operations and the same cut.
			latches := errors.Is(c.want, ErrPoisoned) || errors.Is(c.want, ErrIndeterminate)
			before := content(t, path)
			ops = realOps{}
			err = a.Append("journal.ndjson", int64(len(prior)), line, c.designated)
			if latches {
				if !errors.Is(err, ErrPoisoned) {
					t.Fatalf("after %v, next Append = %v, want latched ErrPoisoned", c.want, err)
				}
				if got := content(t, path); got != before {
					t.Fatalf("latched Append changed the journal: %q -> %q", before, got)
				}
			} else if errors.Is(c.want, ErrAborted) && err != nil {
				t.Fatalf("after an abort, next Append = %v, want success", err)
			}
			// An earlier complete entry is never lost.
			if got := content(t, path); len(got) < len(prior) || got[:len(prior)] != prior {
				t.Fatalf("prior entry damaged: %q", got)
			}
		})
	}
}

// After an abort the journal accepts the next append at the same cut.
func TestAbortThenRetrySucceeds(t *testing.T) {
	a, path := newJournal(t, prior)
	o := injected{shortWrite: 5}
	with(t, &o)
	if err := a.Append("journal.ndjson", int64(len(prior)), line, true); !errors.Is(err, ErrAborted) {
		t.Fatalf("%v, want ErrAborted", err)
	}
	ops = realOps{}
	if err := a.Append("journal.ndjson", int64(len(prior)), line, true); err != nil {
		t.Fatal(err)
	}
	if got := content(t, path); got != prior+string(line) {
		t.Fatalf("journal %q", got)
	}
}

// The latch is per Appender: a new Appender (a store reopen) starts clear.
func TestLatchClearsOnlyOnReopen(t *testing.T) {
	a, path := newJournal(t, prior)
	o := injected{shortWrite: 7, truncateErr: true}
	with(t, &o)
	if err := a.Append("journal.ndjson", int64(len(prior)), line, true); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("%v, want ErrPoisoned", err)
	}
	ops = realOps{}
	if err := a.Append("journal.ndjson", int64(len(prior)+7), line, true); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("retry on the same Appender: %v, want latched ErrPoisoned", err)
	}
	fresh := New(a.root, a.opts)
	if err := fresh.Append("journal.ndjson", int64(len(prior)+7), []byte("{}\n"), true); err != nil {
		t.Fatalf("a new Appender is not latched: %v", err)
	}
	_ = path
}

// The latch is per journal: a poisoned journal does not block another journal
// on the same Appender.
func TestLatchIsPerJournal(t *testing.T) {
	a, path := newJournal(t, prior)
	other, err := a.root.CreateExclusive("other.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	o := injected{shortWrite: 7, truncateErr: true}
	with(t, &o)
	if err := a.Append("journal.ndjson", int64(len(prior)), line, true); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("%v, want ErrPoisoned", err)
	}
	ops = realOps{}
	if err := a.Append("other.ndjson", 0, line, false); err != nil {
		t.Fatalf("another journal on the same Appender: %v, want success", err)
	}
	if err := a.Append("journal.ndjson", int64(len(prior)+7), line, true); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("the poisoned journal: %v, want latched ErrPoisoned", err)
	}
	_ = path
}

// swapOps writes part of the line, then replaces the journal path with a
// different file before reporting the short write, so the abort's second
// handle opens a file that is not the journal.
type swapOps struct {
	path      string
	foreign   []byte
	renameErr error
}

func (o *swapOps) write(f *os.File, p []byte) (int, error) {
	n, _ := f.Write(p[:7])
	if err := os.Rename(o.path, o.path+".moved"); err != nil {
		o.renameErr = err
		return n, err
	}
	if err := os.WriteFile(o.path, o.foreign, 0o600); err != nil {
		return n, err
	}
	return n, writeErr(spaceErrnos[0])
}
func (o *swapOps) truncate(f *os.File, size int64) error { return f.Truncate(size) }
func (o *swapOps) sync(f *os.File) error                 { return f.Sync() }

// A truncate handle that is not the journal (a path swapped mid-append) is
// refused: the abort poisons and the foreign file is left untouched.
func TestAbortRefusesASwappedJournal(t *testing.T) {
	a, path := newJournal(t, prior)
	foreign := bytes.Repeat([]byte("X"), 200)
	sw := &swapOps{path: path, foreign: foreign}
	with(t, sw)
	err := a.Append("journal.ndjson", int64(len(prior)), line, true)
	if sw.renameErr != nil {
		// Windows refuses to rename a file another handle holds open, so this
		// swap cannot happen there; everywhere else it must succeed.
		if runtime.GOOS == "windows" {
			t.Skipf("cannot swap an open journal on Windows: %v", sw.renameErr)
		}
		t.Fatalf("swap setup failed: %v", sw.renameErr)
	}
	if !errors.Is(err, ErrPoisoned) || !strings.Contains(err.Error(), "truncate handle is not the journal") {
		t.Fatalf("%v, want ErrPoisoned for a swapped journal", err)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(got, foreign) {
		t.Fatalf("the foreign file was modified: %d bytes", len(got))
	}
}

// Every out-of-space or out-of-quota errno the platform reports is a proven
// partial write: the designated stream is rolled back to the cut.
func TestAbortOnEverySpaceErrno(t *testing.T) {
	for _, errno := range spaceErrnos {
		t.Run(errno.Error(), func(t *testing.T) {
			a, path := newJournal(t, prior)
			o := injected{shortWrite: 7, shortErr: writeErr(errno)}
			with(t, &o)
			if err := a.Append("journal.ndjson", int64(len(prior)), line, true); !errors.Is(err, ErrAborted) {
				t.Fatalf("%v, want ErrAborted", err)
			}
			if got := content(t, path); got != prior {
				t.Fatalf("journal %q, want %q", got, prior)
			}
		})
	}
}
