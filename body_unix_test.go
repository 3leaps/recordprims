//go:build unix

package recordprims

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// failTmpCleanup makes bodies/tmp read-only at phase, so removing a staged
// file fails, and optionally injects a failure at fail. It restores the
// directory mode when the test ends.
func failTmpCleanup(t *testing.T, s *Store, dir, phase, fail string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	tmpDir := filepath.Join(dir, filepath.FromSlash(dirBodyTmp))
	t.Cleanup(func() { os.Chmod(tmpDir, 0o700) })
	s.hook = func(p string) error {
		if p == phase {
			if err := os.Chmod(tmpDir, 0o500); err != nil {
				t.Fatal(err)
			}
		}
		if p == fail {
			return errors.New("injected")
		}
		return nil
	}
}

func restoreTmp(t *testing.T, s *Store, dir string) []string {
	t.Helper()
	s.hook = nil
	if err := os.Chmod(filepath.Join(dir, filepath.FromSlash(dirBodyTmp)), 0o700); err != nil {
		t.Fatal(err)
	}
	names, err := s.root.ReadDirNames(dirBodyTmp, s.fso)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func unlinkErr(err error) bool {
	var io *IOError
	return errors.As(err, &io) && io.Op == "unlink"
}

// A refusal after staging whose cleanup fails reports the cleanup failure
// and leaves a tmp leftover; body state is unchanged.
func TestPutBodyRefusalReportsStagingCleanupFailure(t *testing.T) {
	s, dir, _ := openBodyStore(t)
	b := []byte("orphan")
	setIncarnation(t, s, b, maxSafeInteger)
	failTmpCleanup(t, s, dir, "body.tmp_fsynced", "")
	_, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain")
	names := restoreTmp(t, s, dir)
	if !errors.Is(err, ErrSchema) || !unlinkErr(err) || IsIndeterminate(err) {
		t.Fatalf("want determinate ErrSchema joined with an unlink IOError, got %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("tmp leftovers %v, want one", names)
	}
	if got := state(t, s, b); got != stateUnpublished {
		t.Fatalf("state %s, want unchanged unpublished", got)
	}
}

// A staging failure whose cleanup fails is determinate and reports both.
func TestStageFailureReportsCleanupFailure(t *testing.T) {
	s, dir, _ := openBodyStore(t)
	b := []byte("half staged")
	failTmpCleanup(t, s, dir, "body.tmp_written", "body.tmp_written")
	_, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain")
	names := restoreTmp(t, s, dir)
	if err == nil || !unlinkErr(err) || IsIndeterminate(err) {
		t.Fatalf("want a determinate error joined with an unlink IOError, got %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("tmp leftovers %v, want one", names)
	}
	if got := state(t, s, b); got != stateUnpublished {
		t.Fatalf("state %s, want unpublished", got)
	}
}

// After a durable step, a failed cleanup keeps the error indeterminate.
func TestIndeterminateFailureStaysIndeterminateWithCleanupFailure(t *testing.T) {
	s, dir, _ := openBodyStore(t)
	b := []byte("mid publication")
	failTmpCleanup(t, s, dir, "body.incarnation_fsynced", "body.incarnation_fsynced")
	_, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain")
	names := restoreTmp(t, s, dir)
	var bi *ErrBodyIncomplete
	if !errors.As(err, &bi) || !unlinkErr(err) || !IsIndeterminate(err) {
		t.Fatalf("want indeterminate ErrBodyIncomplete joined with an unlink IOError, got %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("tmp leftovers %v, want one", names)
	}
	put(t, s, b)
	if got := state(t, s, b); got != stateAvailable {
		t.Fatalf("retry state %s, want available", got)
	}
}

// The length bound is checked before a chunk is written: a refused body
// never has more than the bound on disk.
func TestPutBodyLengthBoundPrecedesWrite(t *testing.T) {
	s, dir, _ := openBodyStore(t)
	old := maxBodyLength
	maxBodyLength = 4
	t.Cleanup(func() { maxBodyLength = old })
	failTmpCleanup(t, s, dir, "body.tmp_created", "")
	_, _, err := s.PutBody(ctx, bytes.NewReader([]byte("12345")), "text/plain")
	names := restoreTmp(t, s, dir)
	if !errors.Is(err, ErrSchema) || !unlinkErr(err) {
		t.Fatalf("want ErrSchema joined with an unlink IOError, got %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("tmp leftovers %v, want one", names)
	}
	fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(dirBodyTmp), names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > maxBodyLength {
		t.Fatalf("staged %d bytes, more than the %d byte bound", fi.Size(), maxBodyLength)
	}
}
