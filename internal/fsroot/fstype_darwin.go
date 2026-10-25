//go:build darwin

package fsroot

import (
	"os"
	"syscall"
)

// allowedDarwin is the macOS filesystem allowlist (fstypename).
var allowedDarwin = map[string]bool{"apfs": true, "hfs": true}

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
	b := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	if !allowedDarwin[string(b)] {
		return ErrFSType
	}
	return nil
}
