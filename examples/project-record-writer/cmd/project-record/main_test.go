package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/3leaps/recordprims"
	pw "github.com/3leaps/recordprims/examples/project-record-writer/projectwriter"
)

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// E4 through the CLI: every command runs in its own process, so the store is
// closed and reopened between the write and the reads.
func TestCLIWalkThroughAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0o700)
	copyDir(t, filepath.Join("..", "..", "registry"), filepath.Join(root, "registry"))
	copyDir(t, filepath.Join("..", "..", "project"), filepath.Join(root, "project"))
	project := filepath.Join(root, "project", "harbor-demo.json")

	bin := filepath.Join(root, "project-record")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	env := append(os.Environ(), "PROJECT_RECORD_ALLOW_TMPFS=1", "PROJECT_RECORD_ALLOW_OVERLAY=1")
	cli := func(wantCode int, stdin []byte, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"--project", project}, args...)...)
		cmd.Env = env
		cmd.Stdin = bytes.NewReader(stdin)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != wantCode {
			t.Fatalf("%v: exit %d, want %d\nstdout: %s\nstderr: %s", args, code, wantCode, out.Bytes(), errb.Bytes())
		}
		return out.Bytes()
	}

	cli(0, nil, "init")
	var kinds []pw.KindInfo
	if err := json.Unmarshal(cli(0, nil, "kinds"), &kinds); err != nil || len(kinds) != 4 {
		t.Fatalf("kinds = %+v, %v", kinds, err)
	}
	var info pw.KindInfo
	json.Unmarshal(cli(0, nil, "inspect", "project-update"), &info)
	req, _ := json.Marshal(pw.Request{
		Project: "harbor-demo", Kind: "project-update", RecordID: "update-2026-11-23",
		OperationKey: "op-cli-1", Entry: info.Example,
	})
	cli(0, req, "--as", "status-bot", "validate", "-")
	cli(3, req, "--as", "vendor-liaison", "record", "-")
	var res pw.Result
	if err := json.Unmarshal(cli(0, req, "--as", "status-bot", "record", "-"), &res); err != nil || res.Pin == nil {
		t.Fatalf("record = %+v, %v", res, err)
	}
	pinArg, _ := json.Marshal(res.Pin)

	// A second process resolves the pin; the library, opened separately,
	// returns the same structured entry and the same page.
	var viaCLI pw.Entry
	if err := json.Unmarshal(cli(0, nil, "resolve", string(pinArg)), &viaCLI); err != nil {
		t.Fatal(err)
	}
	page := cli(0, nil, "render", string(pinArg))
	w, err := pw.Open(project, pw.Options{AllowTmpfs: true, AllowOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	viaLib, err := w.Lookup(context.Background(), *res.Pin)
	if err != nil {
		t.Fatal(err)
	}
	libPage, err := w.Render(context.Background(), *res.Pin)
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(viaCLI)
	b, _ := json.Marshal(viaLib)
	if !bytes.Equal(a, b) {
		t.Fatalf("CLI and library entries differ:\n%s\n%s", a, b)
	}
	if string(page) != libPage {
		t.Fatalf("CLI and library pages differ:\n%s\n%s", page, libPage)
	}

	// Retrying the same request from a new process returns the original pin.
	var again pw.Result
	json.Unmarshal(cli(0, req, "--as", "status-bot", "record", "-"), &again)
	if again.Pin == nil || *again.Pin != *res.Pin {
		t.Fatalf("retry = %+v, want pin %+v", again.Pin, res.Pin)
	}
	cli(0, nil, "reindex")
	var hits []pw.SearchItem
	json.Unmarshal(cli(0, nil, "search", "intake queue"), &hits)
	if len(hits) != 1 || hits[0].Pin != *res.Pin {
		t.Fatalf("search = %+v", hits)
	}
	// A hold placed after the view was built: search no longer returns the
	// revision, although the persisted view still lists it.
	var hold recordprims.ProtectionReceipt
	json.Unmarshal(cli(0, nil, "--as", "rowan", "hold", string(pinArg), "op-hold-cli", "case-9",
		"sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "2026-11-01T00:00:00Z"), &hold)
	if hold.Outcome != "tombstoned" {
		t.Fatalf("hold = %+v", hold)
	}
	hits = nil
	json.Unmarshal(cli(0, nil, "search", "intake queue"), &hits)
	if len(hits) != 0 {
		t.Fatalf("search after a hold = %+v", hits)
	}
	cli(3, nil, "resolve", string(pinArg))

	var unknown pw.Result
	json.Unmarshal(cli(3, nil, "inspect", "incident"), &unknown)
	if unknown.Reason != pw.ReasonUnknownKind {
		t.Fatalf("inspect unknown = %+v", unknown)
	}
	missing, _ := json.Marshal(recordprims.Pin{Stream: res.Pin.Stream, RecordID: "none", Revision: 1, Digest: res.Pin.Digest})
	cli(3, nil, "resolve", string(missing))
}
