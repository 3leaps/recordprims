//go:build linux

package fsroot

import (
	"os"
	"syscall"
)

// Linux filesystem magic numbers (statfs f_type), from linux/magic.h.
const (
	magicExt4    = 0xEF53
	magicXFS     = 0x58465342
	magicBtrfs   = 0x9123683E
	magicTmpfs   = 0x01021994
	magicOverlay = 0x794C7630
)

func checkFSType(r *os.Root, opts Options) error {
	d, err := r.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	var st syscall.Statfs_t
	if err := syscall.Fstatfs(int(d.Fd()), &st); err != nil {
		return err
	}
	switch uint64(st.Type) {
	case magicExt4, magicXFS, magicBtrfs:
		return nil
	case magicTmpfs:
		if opts.AllowTmpfs {
			return nil
		}
	case magicOverlay:
		if opts.AllowOverlay {
			return nil
		}
	}
	return ErrFSType
}
