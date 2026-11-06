//go:build !recordprims_faultpoints

package jfile

// In a release build ops must be the concrete real implementation, not a
// replaceable interface: this assignment compiles only if its static type is
// realOps.
var _ realOps = ops
