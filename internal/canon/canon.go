// Package canon is recordprims' single entry to RFC 8785 canonicalisation.
//
// Every JCS digest in the store goes through this package, which delegates to
// canonprims. The scheme, limits and hash algorithm are fixed here so callers
// cannot vary them, and no canonprims type reaches recordprims' exported API.
package canon

import (
	cp "github.com/3leaps/canonprims/canon"
)

// limits bounds one canonicalisation: canonprims' defaults.
var limits = cp.DefaultLimits()

// Canonicalize returns the RFC 8785 JCS bytes of the JSON text in input.
// Input outside the I-JSON domain is refused, never repaired.
func Canonicalize(input []byte) ([]byte, error) {
	return cp.Canonicalize(cp.JCS, input, limits)
}

// Digest returns "sha256:<hex>" of the JCS bytes of the JSON text in input.
func Digest(input []byte) (string, error) {
	return cp.Digest(cp.JCS, input, limits, "sha256")
}
