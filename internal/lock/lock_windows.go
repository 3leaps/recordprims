//go:build windows

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryAcquire takes a non-blocking exclusive byte-range lock on path, creating
// the file if it does not exist.
func tryAcquire(path string) (func() error, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	rel, err := tryLockFile(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() error {
		rel()
		return f.Close()
	}, nil
}

// tryLockFile takes a non-blocking exclusive LockFileEx lock on the first byte
// of an open file. Windows releases it when the handle closes or the process
// exits.
func tryLockFile(f FileLocker) (func() error, error) {
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return func() error { return windows.UnlockFileEx(h, 0, 1, 0, ol) }, nil
}
