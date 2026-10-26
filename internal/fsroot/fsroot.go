// Package fsroot confines every filesystem operation to one verified store
// root.
//
// The root is opened once as a directory handle (os.Root) and held for the
// store's lifetime, so renaming an ancestor afterwards cannot redirect writes.
// os.Root already refuses names that escape the root; this package adds the
// store's stricter rules on top:
//
//   - every path component is checked with Lstat and refused if it is a
//     symlink (os.Root would follow an in-root symlink; the store refuses);
//   - only regular files and directories are accepted (no FIFOs, devices,
//     sockets or Windows reparse points);
//   - store-created regular files must have exactly one link;
//   - new files are created exclusively (O_CREAT|O_EXCL) with owner-only mode;
//   - ownership and mode (on Windows, owner and DACL) are checked on the
//     opened object, not on an earlier path stat.
//
// Errors never carry paths: they are mapped to an operation and a class.
package fsroot

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// FileMode is the mode for every file the store creates.
const FileMode fs.FileMode = 0o600

// DirMode is the mode for every directory the store creates.
const DirMode fs.FileMode = 0o700

// Error kinds. Callers map these to the public error sets.
var (
	ErrSymlink      = errors.New("fsroot: symlink refused")
	ErrSpecialFile  = errors.New("fsroot: special file refused")
	ErrMultipleLink = errors.New("fsroot: hardlinked file refused")
	ErrOwner        = errors.New("fsroot: wrong owner")
	ErrMode         = errors.New("fsroot: group/other permissions refused")
	ErrBadName      = errors.New("fsroot: invalid name")
	ErrFSType       = errors.New("fsroot: filesystem type refused")
)

// Root is a verified, confined store root.
type Root struct {
	r *os.Root
}

// Open opens dir as the store root and verifies it: a real directory (not a
// symlink), owned by the current principal, with no group or other permission
// bits, on an allowed filesystem type.
func Open(dir string, opts Options) (*Root, error) {
	// Refuse a symlinked root path itself: os.OpenRoot follows symlinks in the
	// directory name, so check the final component first.
	li, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return nil, ErrSymlink
	}
	if !li.IsDir() {
		return nil, ErrSpecialFile
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	root := &Root{r: r}
	// Verify the OPENED directory: stat through the handle, not the path.
	fi, err := r.Stat(".")
	if err != nil {
		r.Close()
		return nil, err
	}
	if err := checkObject(fi, true, opts); err != nil {
		r.Close()
		return nil, err
	}
	if err := root.checkDirHandle(".", opts); err != nil {
		r.Close()
		return nil, err
	}
	if err := checkFSType(r, opts); err != nil {
		r.Close()
		return nil, err
	}
	return root, nil
}

// Options controls verification.
type Options struct {
	// AllowOverlay admits overlay filesystems (CI containers only).
	AllowOverlay bool
	// AllowTmpfs admits tmpfs (tests only).
	AllowTmpfs bool
	// SkipOwnerCheck disables the owner check. Tests only; never set by the
	// public API.
	SkipOwnerCheck bool
}

// Close releases the root handle.
func (r *Root) Close() error { return r.r.Close() }

// validName refuses absolute names, empty components, "." and "..", and
// backslashes, so every name is a clean, root-relative, slash-separated path.
func validName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return ErrBadName
	}
	for _, c := range strings.Split(name, "/") {
		if c == "" || c == "." || c == ".." {
			return ErrBadName
		}
	}
	return nil
}

// checkComponents Lstats every ancestor directory of name and refuses any that
// is a symlink or not a directory. The final component is checked by the
// caller against the opened object.
func (r *Root) checkComponents(name string) error {
	dir := path.Dir(name)
	if dir == "." {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		fi, err := r.r.Lstat(p)
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return ErrSymlink
		}
		if !fi.IsDir() {
			return ErrSpecialFile
		}
	}
	return nil
}

// checkFinal Lstats the final component and refuses a symlink or special file.
func (r *Root) checkFinal(name string) (fs.FileInfo, error) {
	fi, err := r.r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, ErrSymlink
	}
	if !fi.Mode().IsRegular() && !fi.IsDir() {
		return nil, ErrSpecialFile
	}
	return fi, nil
}

// OpenRead opens an existing regular file for reading, refusing symlinks,
// special files and hardlinks, and verifying the opened object.
func (r *Root) OpenRead(name string, opts Options) (*os.File, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := r.checkComponents(name); err != nil {
		return nil, err
	}
	if _, err := r.checkFinal(name); err != nil {
		return nil, err
	}
	f, err := r.r.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := checkObject(fi, false, opts); err != nil {
		f.Close()
		return nil, err
	}
	if err := checkHandle(f, false, opts); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// CreateExclusive creates a new file that must not already exist, with
// owner-only mode from the first byte.
func (r *Root) CreateExclusive(name string) (*os.File, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := r.checkComponents(name); err != nil {
		return nil, err
	}
	return r.r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, FileMode)
}

// OpenAppend opens an existing store file for appending, verifying it.
func (r *Root) OpenAppend(name string, opts Options) (*os.File, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := r.checkComponents(name); err != nil {
		return nil, err
	}
	if _, err := r.checkFinal(name); err != nil {
		return nil, err
	}
	f, err := r.r.OpenFile(name, os.O_WRONLY|os.O_APPEND|oNoFollow, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := checkObject(fi, false, opts); err != nil {
		f.Close()
		return nil, err
	}
	if err := checkHandle(f, false, opts); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Mkdir creates a directory with owner-only mode. It is not an error if the
// directory already exists as a verified directory.
func (r *Root) Mkdir(name string, opts Options) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := r.checkComponents(name); err != nil {
		return err
	}
	err := r.r.Mkdir(name, DirMode)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := r.checkFinal(name)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return ErrSpecialFile
	}
	if err := checkObject(fi, true, opts); err != nil {
		return err
	}
	return r.checkDirHandle(name, opts)
}

// checkDirHandle opens a directory through the root and verifies it by handle.
func (r *Root) checkDirHandle(name string, opts Options) error {
	d, err := r.r.Open(name)
	if err != nil {
		return err
	}
	defer d.Close()
	return checkHandle(d, true, opts)
}

// Rename atomically renames oldname to newname within the root. Both names
// are validated and their ancestors checked.
func (r *Root) Rename(oldname, newname string) error {
	for _, n := range []string{oldname, newname} {
		if err := validName(n); err != nil {
			return err
		}
		if err := r.checkComponents(n); err != nil {
			return err
		}
	}
	return r.r.Rename(oldname, newname)
}

// Remove removes a file within the root, refusing symlinks.
func (r *Root) Remove(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := r.checkComponents(name); err != nil {
		return err
	}
	if _, err := r.checkFinal(name); err != nil {
		return err
	}
	return r.r.Remove(name)
}

// Lstat returns information about name without following a final symlink.
func (r *Root) Lstat(name string) (fs.FileInfo, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := r.checkComponents(name); err != nil {
		return nil, err
	}
	return r.r.Lstat(name)
}

// SyncDir fsyncs a directory within the root (or the root itself for ".").
// On platforms where directory fsync is unsupported it is a no-op.
func (r *Root) SyncDir(name string) error {
	if name != "." {
		if err := validName(name); err != nil {
			return err
		}
		if err := r.checkComponents(name); err != nil {
			return err
		}
	}
	return syncDir(r.r, name)
}

// OpenLockFile opens (creating if needed) the store's lock file through the
// confined root, refusing symlinks and special files, and verifies it.
func (r *Root) OpenLockFile(name string, opts Options) (*os.File, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := r.checkComponents(name); err != nil {
		return nil, err
	}
	if fi, err := r.r.Lstat(name); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, ErrSymlink
		}
		if !fi.Mode().IsRegular() {
			return nil, ErrSpecialFile
		}
	}
	f, err := r.r.OpenFile(name, os.O_RDWR|os.O_CREATE|oNoFollow, FileMode)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := checkObject(fi, false, opts); err != nil {
		f.Close()
		return nil, err
	}
	if err := checkHandle(f, false, opts); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ReadDirNames lists, sorted, the entries of a verified directory within the
// root.
func (r *Root) ReadDirNames(name string, opts Options) ([]string, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := r.checkComponents(name); err != nil {
		return nil, err
	}
	fi, err := r.checkFinal(name)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, ErrSpecialFile
	}
	d, err := r.r.Open(name)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	if fi, err = d.Stat(); err != nil {
		return nil, err
	}
	if err := checkObject(fi, true, opts); err != nil {
		return nil, err
	}
	if err := checkHandle(d, true, opts); err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}
