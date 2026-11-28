package recordprims

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/3leaps/recordprims/internal/fsroot"
)

// A refusal is ErrStoreInvalid and keeps the caller's outcome marker; an
// ordinary I/O failure stays an IOError with its marker.
func TestIOErrKeepsOutcomeMarker(t *testing.T) {
	for _, ind := range []bool{false, true} {
		for _, ref := range []error{fsroot.ErrSymlink, fsroot.ErrSpecialFile, fsroot.ErrMultipleLink,
			fsroot.ErrOwner, fsroot.ErrMode, fsroot.ErrFSType, fsroot.ErrBadName} {
			got := ioErr("open", ref, ind)
			var ioe *IOError
			if !errors.Is(got, ErrStoreInvalid) || errors.As(got, &ioe) || IsIndeterminate(got) != ind {
				t.Fatalf("%v (indeterminate %v): got %v, indeterminate %v", ref, ind, got, IsIndeterminate(got))
			}
			if !strings.HasPrefix(got.Error(), "recordprims: invalid store: ") {
				t.Fatalf("%v: message %q", ref, got.Error())
			}
		}
		got := ioErr("read", fs.ErrPermission, ind)
		var ioe *IOError
		if !errors.As(got, &ioe) || ioe.Class != "permission" || errors.Is(got, ErrStoreInvalid) || IsIndeterminate(got) != ind {
			t.Fatalf("ordinary I/O (indeterminate %v): got %v", ind, got)
		}
	}
}
