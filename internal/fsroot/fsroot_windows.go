//go:build windows

package fsroot

import (
	"io/fs"
	"os"
	"syscall"
)

// oNoFollow has no open-flag equivalent on Windows; reparse points are refused
// by checkObject and by the component Lstat checks instead.
const oNoFollow = 0

const fileAttributeReparsePoint = 0x400

// checkObject verifies an opened object on Windows: no reparse point, a regular
// file or directory, and exactly one link for files.
//
// Owner and DACL verification (the Windows equivalent of the Unix owner and
// mode checks) needs the security-descriptor APIs, which the standard library
// does not expose. Until that lands, Windows stores rely on the user profile's
// default ACLs; this gap is declared in the store's documentation.
func checkObject(fi fs.FileInfo, isDir bool, opts Options) error {
	if isDir != fi.IsDir() {
		return ErrSpecialFile
	}
	if !isDir && !fi.Mode().IsRegular() {
		return ErrSpecialFile
	}
	if fi.Mode()&fs.ModeSymlink != 0 || fi.Mode()&fs.ModeIrregular != 0 {
		return ErrSymlink
	}
	if a, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok && a.FileAttributes&fileAttributeReparsePoint != 0 {
		return ErrSymlink
	}
	return nil
}

// checkLinks verifies a regular file has exactly one link, using the open
// handle.
func checkLinks(f *os.File) error {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&fileAttributeReparsePoint != 0 {
		return ErrSymlink
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 && info.NumberOfLinks != 1 {
		return ErrMultipleLink
	}
	return nil
}

// syncDir is a no-op on Windows: directory handles cannot be flushed through
// the portable API, and NTFS journals metadata. The profile's failure model
// (survives_process_loss) does not depend on it.
func syncDir(r *os.Root, name string) error { return nil }

// checkFSType admits NTFS and ReFS. Volume filesystem names need
// GetVolumeInformationByHandle, which the standard library does not expose;
// until then Windows accepts the volume and documents the gap.
func checkFSType(r *os.Root, opts Options) error { return nil }

// ProcessIdentityAmbiguous reports whether the process identity is ambiguous
// for the same-owner default. Detecting an elevated or impersonating token
// needs the token APIs the standard library does not expose, so Windows fails
// closed: the same-owner default is unavailable and an Authorizer is required.
func ProcessIdentityAmbiguous() bool { return true }
