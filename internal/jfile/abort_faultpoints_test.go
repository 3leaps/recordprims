//go:build recordprims_faultpoints

package jfile

import (
	"errors"
	"os"
	"testing"
)

// injected is a fileOps that fails the chosen operation and passes the rest
// to the real calls. A short write really writes its first bytes.
type injected struct {
	shortWrite   int  // >= 0: write only this many bytes, then fail
	shortNoErr   bool // with shortWrite: report the short count with a nil error
	noopTrunc    bool // truncate reports success without truncating
	fullWriteErr bool // write everything, then report an error
	badCount     bool // report more bytes than requested
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
		return n, errors.New("injected short write")
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

func with(t *testing.T, o *injected) {
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
	} {
		t.Run(c.name, func(t *testing.T) {
			a, path := newJournal(t, prior)
			o := c.ops
			with(t, &o)
			if c.fault != "" {
				a.Fault = func(p string) error {
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
			// Poisoned, and indeterminate on the designated stream, latch: the
			// next Append returns ErrPoisoned and leaves the file byte-identical,
			// even with healthy file operations and the same cut.
			latches := errors.Is(c.want, ErrPoisoned) || (c.designated && errors.Is(c.want, ErrIndeterminate))
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
	fresh := &Appender{Root: a.Root, Opts: a.Opts}
	if err := fresh.Append("journal.ndjson", int64(len(prior)+7), []byte("{}\n"), true); err != nil {
		t.Fatalf("a new Appender is not latched: %v", err)
	}
	_ = path
}
