//go:build windows

package lock

import (
	"errors"
	"syscall"
)

const errorSharingViolation syscall.Errno = 32

// tryAcquire opens path with share mode 0 (exclusive access). While the handle
// is open, another process's open fails with a sharing violation, which is a
// cross-process lock that the OS releases when the process exits. This uses
// only the standard library (no LockFileEx).
func tryAcquire(path string) (func() error, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // no sharing: exclusive
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return func() error { return syscall.CloseHandle(h) }, nil
}

// tryLockFile is unsupported on Windows: the exclusive open in tryAcquire is
// the lock. Callers on Windows use Acquire with the store's lock path.
func tryLockFile(f FileLocker) (func() error, error) {
	return nil, errors.New("lock: AcquireFile is not supported on Windows")
}
