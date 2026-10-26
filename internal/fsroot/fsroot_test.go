package fsroot

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// testOpts admits the temp filesystem used by tests (tmpfs on Linux CI,
// overlay in containers).
var testOpts = Options{AllowTmpfs: true, AllowOverlay: true}

func newRoot(t *testing.T) (string, *Root) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir, testOpts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return dir, r
}

func TestOpenVerifiedRoot(t *testing.T) {
	newRoot(t)
}

func TestOpenRefusesSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	real := t.TempDir()
	os.Chmod(real, 0o700)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link, testOpts); !errors.Is(err, ErrSymlink) {
		t.Fatalf("want ErrSymlink, got %v", err)
	}
}

func TestOpenRefusesGroupWritableRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes; the Windows DACL equivalent is TestOpenRefusesPermissiveRoot")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o770)
	if _, err := Open(dir, testOpts); !errors.Is(err, ErrMode) {
		t.Fatalf("want ErrMode, got %v", err)
	}
	// The sticky bit is not an exception.
	os.Chmod(dir, 0o1777)
	if _, err := Open(dir, testOpts); !errors.Is(err, ErrMode) {
		t.Fatalf("sticky: want ErrMode, got %v", err)
	}
}

func TestOpenRefusesPermissiveRoot(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	makePermissive(t, dir)
	if _, err := Open(dir, testOpts); !errors.Is(err, ErrMode) {
		t.Fatalf("want ErrMode, got %v", err)
	}
}

func TestOpenRefusesFileAsRoot(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	os.WriteFile(f, nil, 0o600)
	if _, err := Open(f, testOpts); !errors.Is(err, ErrSpecialFile) {
		t.Fatalf("want ErrSpecialFile, got %v", err)
	}
}

func TestCreateExclusiveAndRead(t *testing.T) {
	_, r := newRoot(t)
	f, err := r.CreateExclusive("a")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("x"))
	f.Close()
	if runtime.GOOS != "windows" {
		fi, _ := r.Lstat("a")
		if fi.Mode().Perm() != FileMode {
			t.Fatalf("mode %v, want %v", fi.Mode().Perm(), FileMode)
		}
	}
	if _, err := r.CreateExclusive("a"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create: want ErrExist, got %v", err)
	}
	g, err := r.OpenRead("a", testOpts)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	g.Close()
}

func TestRefusesInRootSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	dir, r := newRoot(t)
	os.WriteFile(filepath.Join(dir, "target"), []byte("t"), 0o600)
	os.Symlink("target", filepath.Join(dir, "link"))
	if _, err := r.OpenRead("link", testOpts); !errors.Is(err, ErrSymlink) {
		t.Fatalf("final-component symlink: want ErrSymlink, got %v", err)
	}
	// A symlinked directory component is refused too.
	os.Mkdir(filepath.Join(dir, "realdir"), 0o700)
	os.WriteFile(filepath.Join(dir, "realdir", "f"), []byte("f"), 0o600)
	os.Symlink("realdir", filepath.Join(dir, "linkdir"))
	if _, err := r.OpenRead("linkdir/f", testOpts); !errors.Is(err, ErrSymlink) {
		t.Fatalf("component symlink: want ErrSymlink, got %v", err)
	}
	if _, err := r.CreateExclusive("linkdir/new"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("create through symlink dir: want ErrSymlink, got %v", err)
	}
}

func TestRefusesHardlinkedFile(t *testing.T) {
	dir, r := newRoot(t)
	os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o600)
	if err := os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.OpenRead("a", testOpts); !errors.Is(err, ErrMultipleLink) {
		t.Fatalf("want ErrMultipleLink, got %v", err)
	}
}

func TestRefusesPermissiveFile(t *testing.T) {
	dir, r := newRoot(t)
	os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o600)
	makePermissive(t, filepath.Join(dir, "a"))
	if _, err := r.OpenRead("a", testOpts); !errors.Is(err, ErrMode) {
		t.Fatalf("want ErrMode, got %v", err)
	}
}

func TestRefusesBadNames(t *testing.T) {
	_, r := newRoot(t)
	for _, n := range []string{"", "/abs", "../escape", "a/../../b", "a//b", "./a", `a\b`, "a/."} {
		if _, err := r.CreateExclusive(n); !errors.Is(err, ErrBadName) {
			t.Errorf("%q: want ErrBadName, got %v", n, err)
		}
	}
}

func TestRenameWithinRootIsSafeAfterAncestorRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory rename while open is restricted on Windows")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "store")
	os.Mkdir(dir, 0o700)
	r, err := Open(dir, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Move the store directory after opening; the handle still points at it.
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	// An attacker recreates the original path.
	os.Mkdir(dir, 0o700)
	f, err := r.CreateExclusive("x")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := os.Stat(filepath.Join(moved, "x")); err != nil {
		t.Fatalf("write did not follow the handle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Fatal("write went to the attacker's directory")
	}
}

func TestMkdirAndSync(t *testing.T) {
	_, r := newRoot(t)
	if err := r.Mkdir("streams", testOpts); err != nil {
		t.Fatal(err)
	}
	if err := r.Mkdir("streams", testOpts); err != nil {
		t.Fatalf("idempotent mkdir: %v", err)
	}
	if err := r.SyncDir("streams"); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncDir("."); err != nil {
		t.Fatal(err)
	}
}
