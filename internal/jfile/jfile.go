// Package jfile appends complete lines to a journal file under the store lock,
// at a cut the caller has already verified.
//
// A successful Append has written and fsynced the whole line. Every other
// outcome is classified, because a reader refuses a damaged designated
// body-removal stream for the whole store:
//
//   - The file is not at the verified cut: ErrCutMismatch, nothing written.
//   - A write that provably did not complete the line (short or zero-byte):
//     on the designated stream, the writer still holds the lock and knows the
//     cut, so it truncates back to it and fsyncs. If that succeeds the result
//     is ErrAborted, determinate: nothing was appended. If the truncate or its
//     fsync fails, ErrPoisoned. On an ordinary stream there is no abort; the
//     result is ErrIndeterminate and the damaged tail is left for the stream's
//     own recovery.
//   - The whole line written but the fsync failed: ErrIndeterminate. The
//     entry may be durable, so it is never truncated.
//
// A poisoned or indeterminate designated-stream outcome latches: every later
// Append to that journal on the same Appender returns ErrPoisoned without
// touching the file, because neither a retry nor a re-read of the page cache
// can tell whether the earlier bytes are durable. Only a new Appender, that
// is a store reopen, clears it. A poisoned ordinary stream latches too.
//
// An in-memory cut offset is not recovery evidence after a restart; a crash
// during an abort falls back to the reopen rule.
package jfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/3leaps/recordprims/internal/fsroot"
)

// Outcome errors. ErrAborted is determinate: nothing was appended and the file
// is back at the cut. ErrIndeterminate and ErrPoisoned are not: the journal
// must be re-read before anything relies on it.
var (
	ErrCutMismatch   = errors.New("jfile: journal is not at the verified cut")
	ErrAborted       = errors.New("jfile: partial append aborted; journal restored to the cut")
	ErrIndeterminate = errors.New("jfile: append outcome indeterminate")
	ErrPoisoned      = errors.New("jfile: partial append could not be rolled back")
)

// Appender appends lines to journal files under one verified root. The caller
// holds the store lock for the whole call.
type Appender struct {
	Root *fsroot.Root
	Opts fsroot.Options
	// Fault, when set, is called at each named phase; an error it returns
	// fails the operation there. Production stores leave it nil.
	Fault func(phase string) error

	mu      sync.Mutex
	latched map[string]error // journal name -> the outcome that latched it
}

func (a *Appender) fault(phase string) error {
	if a.Fault == nil {
		return nil
	}
	return a.Fault(phase)
}

// Append writes line, which must be one complete JSON line ending in '\n', to
// the journal file name, whose verified complete cut ends at offset cut.
// designated marks the store's body-removal stream, the only stream on which
// a partial write is rolled back.
func (a *Appender) Append(name string, cut int64, line []byte, designated bool) error {
	a.mu.Lock()
	prior := a.latched[name]
	a.mu.Unlock()
	if prior != nil {
		return fmt.Errorf("%w: latched by an earlier outcome (%v); reopen the store", ErrPoisoned, prior)
	}
	err := a.appendOnce(name, cut, line, designated)
	if errors.Is(err, ErrPoisoned) || (designated && errors.Is(err, ErrIndeterminate)) {
		a.mu.Lock()
		if a.latched == nil {
			a.latched = map[string]error{}
		}
		a.latched[name] = err
		a.mu.Unlock()
	}
	return err
}

func (a *Appender) appendOnce(name string, cut int64, line []byte, designated bool) error {
	if len(line) < 2 || line[len(line)-1] != '\n' || bytes.IndexByte(line[:len(line)-1], '\n') >= 0 {
		return fmt.Errorf("jfile: line must be one non-empty line ending in a newline")
	}
	f, err := a.Root.OpenAppend(name, a.Opts)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() != cut {
		return ErrCutMismatch
	}
	n, werr := ops.write(f, line)
	if werr == nil && n == len(line) {
		if err := a.fault("journal.line_written"); err != nil {
			return fmt.Errorf("%w: %v", ErrIndeterminate, err)
		}
		if err := ops.sync(f); err != nil {
			return fmt.Errorf("%w: fsync: %v", ErrIndeterminate, err)
		}
		// The entry is durable here; a failure reported now still leaves the
		// caller unable to tell, so it is indeterminate.
		if err := a.fault("journal.fsynced"); err != nil {
			return fmt.Errorf("%w: %v", ErrIndeterminate, err)
		}
		return nil
	}
	if n < 0 || n >= len(line) {
		// A full-length write that still reported an error, or a count that
		// cannot be trusted: not a proven partial write, so never truncated.
		return fmt.Errorf("%w: write reported %d of %d bytes: %v", ErrIndeterminate, n, len(line), werr)
	}
	// A write that provably did not complete the line.
	if !designated {
		return fmt.Errorf("%w: partial write (%d of %d bytes): %v", ErrIndeterminate, n, len(line), werr)
	}
	if err := ops.truncate(f, cut); err != nil {
		return fmt.Errorf("%w: truncate: %v", ErrPoisoned, err)
	}
	if err := a.fault("journal.abort_truncated"); err != nil {
		return fmt.Errorf("%w: %v", ErrPoisoned, err)
	}
	if err := ops.sync(f); err != nil {
		return fmt.Errorf("%w: fsync after truncate: %v", ErrPoisoned, err)
	}
	if err := a.fault("journal.abort_fsynced"); err != nil {
		return fmt.Errorf("%w: %v", ErrPoisoned, err)
	}
	fi, err = f.Stat()
	if err != nil || fi.Size() != cut {
		return fmt.Errorf("%w: journal not back at the cut", ErrPoisoned)
	}
	return fmt.Errorf("%w (wrote %d of %d bytes: %v)", ErrAborted, n, len(line), werr)
}

// realOps are the real file operations. In release builds ops is this
// concrete type and cannot be replaced (seam_release.go); builds tagged
// recordprims_faultpoints make ops an injectable interface
// (seam_faultpoints.go).
type realOps struct{}

func (realOps) write(f *os.File, p []byte) (int, error) { return f.Write(p) }
func (realOps) truncate(f *os.File, size int64) error   { return f.Truncate(size) }
func (realOps) sync(f *os.File) error                   { return f.Sync() }
