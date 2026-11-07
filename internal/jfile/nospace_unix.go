//go:build !windows

package jfile

import (
	"errors"
	"syscall"
)

// outOfSpace reports whether err is the device or quota refusing more bytes.
func outOfSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
