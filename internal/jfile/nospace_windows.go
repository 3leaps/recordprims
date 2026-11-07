//go:build windows

package jfile

import (
	"errors"

	"golang.org/x/sys/windows"
)

// outOfSpace reports whether err is the volume or quota refusing more bytes.
func outOfSpace(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) ||
		errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) ||
		errors.Is(err, windows.ERROR_DISK_QUOTA_EXCEEDED)
}
