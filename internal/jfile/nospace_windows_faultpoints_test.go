//go:build recordprims_faultpoints && windows

package jfile

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// The errors a real write reports when the volume or quota is full.
var spaceErrnos = []syscall.Errno{windows.ERROR_DISK_FULL, windows.ERROR_HANDLE_DISK_FULL, windows.ERROR_DISK_QUOTA_EXCEEDED}
