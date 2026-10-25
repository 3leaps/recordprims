//go:build unix

package fsroot

import (
	"io/fs"
	"os"
	"syscall"
)

// oNoFollow makes open fail on a final-component symlink. The component checks
// already refuse symlinks; this closes the window between check and open.
const oNoFollow = syscall.O_NOFOLLOW

// checkObject verifies an opened object: owned by the effective uid, no group
// or other permission bits, and (for regular files) exactly one link.
func checkObject(fi fs.FileInfo, isDir bool, opts Options) error {
	if isDir != fi.IsDir() {
		return ErrSpecialFile
	}
	if !isDir && !fi.Mode().IsRegular() {
		return ErrSpecialFile
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return ErrMode
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ErrOwner
	}
	if !opts.SkipOwnerCheck && int(st.Uid) != os.Geteuid() {
		return ErrOwner
	}
	if !isDir && st.Nlink != 1 {
		return ErrMultipleLink
	}
	return nil
}

// syncDir opens the directory through the root and fsyncs it.
func syncDir(r *os.Root, name string) error {
	d, err := r.Open(name)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ProcessIdentityAmbiguous reports whether the process runs with differing
// real and effective uids (setuid or elevated), which the same-owner
// authorization default refuses.
func ProcessIdentityAmbiguous() bool {
	return os.Getuid() != os.Geteuid()
}

// checkLinks is a no-op on Unix: checkObject already checks the link count
// from the opened object's stat.
func checkLinks(f *os.File) error { return nil }
