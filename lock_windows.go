//go:build windows

package recordprims

import (
	"errors"
	"path/filepath"

	"github.com/3leaps/recordprims/internal/lock"
)

// acquireStoreLock takes the exclusive-open lock. On Windows a directory with
// open handles inside it cannot be renamed, so the path is stable while the
// store root handle is held.
func (s *Store) acquireStoreLock() (func() error, error) {
	rel, err := lock.Acquire(filepath.Join(s.dir, fileLock), s.opts.LockTimeout)
	if err != nil {
		if errors.Is(err, lock.ErrLocked) {
			return nil, ErrLocked
		}
		return nil, ioErr("lock", err, false)
	}
	return rel, nil
}
