//go:build unix && !darwin && !linux

package fsroot

import "os"

// Other Unix systems are outside the estate platform matrix; refuse rather
// than guess.
func checkFSType(r *os.Root, opts Options) error { return ErrFSType }
