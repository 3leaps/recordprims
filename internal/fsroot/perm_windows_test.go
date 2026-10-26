//go:build windows

package fsroot

import (
	"testing"

	"golang.org/x/sys/windows"
)

// makePermissive adds an ACE granting Everyone read access to path.
func makePermissive(t *testing.T, path string) {
	t.Helper()
	if err := grantEveryone(path); err != nil {
		t.Fatal(err)
	}
}

func grantEveryone(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, dacl)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

func TestProcessIdentityAmbiguousDoesNotPanic(t *testing.T) {
	t.Logf("ambiguous=%v", ProcessIdentityAmbiguous())
}

func TestCurrentPrincipalResolves(t *testing.T) {
	p, err := currentPrincipal()
	if err != nil {
		t.Fatal(err)
	}
	if p.user == nil || p.defaultOwner == nil || len(p.trusted) == 0 {
		t.Fatal("principal incomplete")
	}
}
