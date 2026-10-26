// Package lock provides the store-wide cross-process lock.
//
// One advisory lock guards every mutation of a store. It is held by an open
// file handle, so the operating system releases it when the holding process
// exits; there is no stale-lock stealing. Acquire fails fast by default and
// retries up to a timeout when one is given.
package lock

import (
	"errors"
	"time"
)

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("lock: held by another process")

// pollInterval bounds how often a timed Acquire retries.
const pollInterval = 20 * time.Millisecond

// Acquire takes the lock on the file at path, waiting up to timeout (zero
// means try once). The returned function releases it.
func Acquire(path string, timeout time.Duration) (release func() error, err error) {
	return acquire(func() (func() error, error) { return tryAcquire(path) }, timeout)
}

// AcquireFile takes the lock on an already-open file handle (opened through
// the confined store root), waiting up to timeout. The returned function
// releases the lock; it does not close f.
func AcquireFile(f FileLocker, timeout time.Duration) (release func() error, err error) {
	return acquire(func() (func() error, error) { return tryLockFile(f) }, timeout)
}

func acquire(try func() (func() error, error), timeout time.Duration) (func() error, error) {
	deadline := time.Now().Add(timeout)
	for {
		rel, err := try()
		if err == nil {
			return rel, nil
		}
		if !errors.Is(err, ErrLocked) || timeout <= 0 || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(pollInterval)
	}
}

// FileLocker is the part of *os.File the lock needs.
type FileLocker interface{ Fd() uintptr }
