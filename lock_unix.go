//go:build unix

package recordprims

import (
	"errors"

	"github.com/3leaps/recordprims/internal/lock"
)

// acquireStoreLock opens the lock file through the confined root (so a
// renamed ancestor cannot redirect it) and flocks the handle.
func (s *Store) acquireStoreLock() (func() error, error) {
	f, err := s.root.OpenLockFile(fileLock, s.fso)
	if err != nil {
		return nil, mapOpenErr(err)
	}
	rel, err := lock.AcquireFile(f, s.opts.LockTimeout)
	if err != nil {
		f.Close()
		if errors.Is(err, lock.ErrLocked) {
			return nil, ErrLocked
		}
		return nil, ioErr("lock", err, false)
	}
	return func() error {
		rel()
		return f.Close()
	}, nil
}
