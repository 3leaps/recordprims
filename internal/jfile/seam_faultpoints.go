//go:build recordprims_faultpoints

package jfile

import "os"

// fileOps is the failure-injection seam, present only in builds tagged
// recordprims_faultpoints.
type fileOps interface {
	write(f *os.File, p []byte) (int, error)
	truncate(f *os.File, size int64) error
	sync(f *os.File) error
}

// ops is replaceable in tagged builds so tests can inject write, truncate and
// fsync failures.
var ops fileOps = realOps{}
