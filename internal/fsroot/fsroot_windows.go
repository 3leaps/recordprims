//go:build windows

package fsroot

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// oNoFollow has no open-flag equivalent on Windows; reparse points are refused
// by checkObject, by the component Lstat checks and by checkHandle instead.
const oNoFollow = 0

// checkObject verifies an opened object's type on Windows: a regular file or
// directory, not a reparse point. Owner, DACL and link count need the open
// handle and are checked by checkHandle.
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
	return nil
}

// checkHandle verifies an open file or directory through its handle: not a
// reparse point, exactly one link for regular files, owned by the current
// principal, and a DACL that grants access to no one but the owner and the
// local administrators (the Windows equivalent of the Unix owner and mode
// checks).
func checkHandle(f *os.File, isDir bool, opts Options) error {
	h := windows.Handle(f.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrSymlink
	}
	if isDir != (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
		return ErrSpecialFile
	}
	if !isDir && info.NumberOfLinks != 1 {
		return ErrMultipleLink
	}
	return checkSecurity(h, opts)
}

// principal holds the SIDs of the current process token.
type principal struct {
	user         *windows.SID // the token's user
	defaultOwner *windows.SID // the owner the token assigns to new objects
	trusted      []*windows.SID
	err          error
}

var (
	principalOnce sync.Once
	current       principal
)

func currentPrincipal() (*principal, error) {
	principalOnce.Do(func() {
		tok := windows.GetCurrentProcessToken()
		u, err := tok.GetTokenUser()
		if err != nil {
			current.err = err
			return
		}
		if current.user, err = u.User.Sid.Copy(); err != nil {
			current.err = err
			return
		}
		if current.defaultOwner, err = tokenDefaultOwner(tok); err != nil {
			current.err = err
			return
		}
		// SYSTEM and the local administrators can read any owner-only file,
		// like root on Unix. CREATOR OWNER and OWNER RIGHTS resolve to the
		// object's owner.
		for _, w := range []windows.WELL_KNOWN_SID_TYPE{
			windows.WinLocalSystemSid,
			windows.WinBuiltinAdministratorsSid,
			windows.WinCreatorOwnerSid,
			windows.WinCreatorOwnerRightsSid,
		} {
			s, err := windows.CreateWellKnownSid(w)
			if err != nil {
				current.err = err
				return
			}
			current.trusted = append(current.trusted, s)
		}
	})
	return &current, current.err
}

// tokenDefaultOwner returns the TokenOwner SID: the owner Windows assigns to
// objects this token creates (the user, or the Administrators group for an
// elevated administrator).
func tokenDefaultOwner(tok windows.Token) (*windows.SID, error) {
	n := uint32(64)
	for {
		b := make([]byte, n)
		err := windows.GetTokenInformation(tok, windows.TokenOwner, &b[0], uint32(len(b)), &n)
		if err == nil {
			// TOKEN_OWNER is a single pointer to a SID stored in b.
			sid := *(**windows.SID)(unsafe.Pointer(&b[0]))
			return sid.Copy()
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || n <= uint32(len(b)) {
			return nil, err
		}
	}
}

func (p *principal) isOwner(s *windows.SID) bool {
	return s.Equals(p.user) || s.Equals(p.defaultOwner)
}

func (p *principal) isTrusted(s, owner *windows.SID) bool {
	if s.Equals(owner) || p.isOwner(s) {
		return true
	}
	for _, t := range p.trusted {
		if s.Equals(t) {
			return true
		}
	}
	return false
}

// checkSecurity verifies the owner and DACL of an open handle. Every ACE that
// grants access, including inheritable ones (they define what new store files
// receive), must name a trusted principal. Deny ACEs are always acceptable.
// A missing or NULL DACL grants everyone access and is refused.
func checkSecurity(h windows.Handle, opts Options) error {
	p, err := currentPrincipal()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return ErrOwner
	}
	if !opts.SkipOwnerCheck && !p.isOwner(owner) {
		return ErrOwner
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return ErrMode
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return ErrMode
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !p.isTrusted(sid, owner) {
				return ErrMode
			}
		case windows.ACCESS_DENIED_ACE_TYPE, aceTypeAccessDeniedObject,
			aceTypeAccessDeniedCallback, aceTypeAccessDeniedCallbackObject:
		default:
			// Object and callback allow ACEs are not expected on a local
			// store; refuse rather than interpret them.
			return ErrMode
		}
	}
	return nil
}

// Deny ACE types beyond ACCESS_DENIED_ACE_TYPE (winnt.h).
const (
	aceTypeAccessDeniedObject         = 0x6
	aceTypeAccessDeniedCallback       = 0xA
	aceTypeAccessDeniedCallbackObject = 0xC
)

// syncDir is a no-op on Windows: directory handles cannot be flushed through
// the portable API, and NTFS journals metadata. The profile's failure model
// (survives_process_loss) does not depend on it.
func syncDir(r *os.Root, name string) error { return nil }

// allowedWindowsFS is the Windows filesystem allowlist.
var allowedWindowsFS = map[string]bool{"NTFS": true, "ReFS": true}

// checkFSType admits local NTFS and ReFS volumes, read from the opened root
// handle. Network shares are refused even when the server reports NTFS.
func checkFSType(r *os.Root, opts Options) error {
	d, err := r.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	h := windows.Handle(d.Fd())
	name := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, nil, &name[0], uint32(len(name))); err != nil {
		return err
	}
	if !allowedWindowsFS[windows.UTF16ToString(name)] {
		return ErrFSType
	}
	remote, err := isRemote(h)
	if err != nil {
		return err
	}
	if remote {
		return ErrFSType
	}
	return nil
}

// isRemote reports whether the handle resolves to a UNC path (a network share
// or a mapped network drive): local paths come back as \\?\C:\..., remote ones
// as \\?\UNC\server\share\....
func isRemote(h windows.Handle) (bool, error) {
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return false, err
		}
		if n < uint32(len(buf)) {
			return strings.HasPrefix(windows.UTF16ToString(buf[:n]), `\\?\UNC\`), nil
		}
		buf = make([]uint16, n+1)
	}
}

// ProcessIdentityAmbiguous reports whether the process identity is ambiguous
// for the same-owner default: an elevated process token, a thread that is
// impersonating, or a token that cannot be queried (fail closed).
func ProcessIdentityAmbiguous() bool {
	var elevated uint32
	var n uint32
	err := windows.GetTokenInformation(windows.GetCurrentProcessToken(), windows.TokenElevation,
		(*byte)(unsafe.Pointer(&elevated)), uint32(unsafe.Sizeof(elevated)), &n)
	if err != nil || elevated != 0 {
		return true
	}
	var t windows.Token
	err = windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &t)
	if err == nil {
		t.Close()
		return true
	}
	return !errors.Is(err, windows.ERROR_NO_TOKEN)
}
