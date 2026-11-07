//go:build recordprims_faultpoints && !windows

package jfile

import "syscall"

// The errors a real write reports when the device or quota is full.
var spaceErrnos = []syscall.Errno{syscall.ENOSPC, syscall.EDQUOT}
