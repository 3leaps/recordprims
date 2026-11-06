//go:build !recordprims_faultpoints

package jfile

// ops is the concrete real implementation in release builds: no failure can
// be injected.
var ops realOps
