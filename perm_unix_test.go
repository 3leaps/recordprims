//go:build unix

package recordprims

import (
	"os"
	"testing"
)

// makePermissive grants the group read access to path.
func makePermissive(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, fi.Mode().Perm()|0o040); err != nil {
		t.Fatal(err)
	}
}
