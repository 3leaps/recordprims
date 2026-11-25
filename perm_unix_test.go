//go:build unix

package recordprims

import (
	"errors"
	"os"
	"path/filepath"
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

// A store entry the root refuses (symlink, group or other permission bits) is
// store_invalid wherever it is met, not only when the store is opened.
func TestStreamPathRefusalsAreStoreInvalid(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(t *testing.T, dir, id string)
	}{
		{"journal mode 0640", func(t *testing.T, dir, id string) {
			if err := os.Chmod(filepath.Join(dir, streamJournalFile(id)), 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{"stream dir symlink", func(t *testing.T, dir, id string) {
			sdir := filepath.Join(dir, streamDir(id))
			if err := os.Rename(sdir, sdir+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(sdir)+".real", sdir); err != nil {
				t.Fatal(err)
			}
		}},
		{"streams dir symlink", func(t *testing.T, dir, id string) {
			sd := filepath.Join(dir, dirStreams)
			real := filepath.Join(t.TempDir(), "streams")
			if err := os.Rename(sd, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, sd); err != nil {
				t.Fatal(err)
			}
		}},
		{"journal symlink", func(t *testing.T, dir, id string) {
			j := filepath.Join(dir, streamJournalFile(id))
			if err := os.Rename(j, j+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(j)+".real", j); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, dir, _ := openBodyStore(t)
			st, err := s.OpenStream("notes", ModeSingleRevision)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Append(ctx, Write{Record: recordJSON("notes", "rec-1", 1, nil, `{"m":1}`, nil), IdempotencyKey: "k-1"}); err != nil {
				t.Fatal(err)
			}
			tc.tamper(t, dir, "notes")
			if _, err := s.OpenStream("notes", ModeSingleRevision); !errors.Is(err, ErrStoreInvalid) {
				t.Fatalf("OpenStream: want ErrStoreInvalid, got %v", err)
			}
			if _, err := st.Release(ctx, UseRequest{Purpose: PurposeReuse, Closure: ClosureMetadataOnly}, nil); !errors.Is(err, ErrStoreInvalid) {
				t.Fatalf("Release: want ErrStoreInvalid, got %v", err)
			}
			if _, err := st.Append(ctx, Write{Record: recordJSON("notes", "rec-2", 1, nil, `{"m":2}`, nil), IdempotencyKey: "k-2"}); !errors.Is(err, ErrStoreInvalid) {
				t.Fatalf("Append: want ErrStoreInvalid, got %v", err)
			}
		})
	}
}
