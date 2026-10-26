package lock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The helper process holds the lock until its stdin closes.
func TestMain(m *testing.M) {
	if p := os.Getenv("RECORDPRIMS_LOCK_HELPER"); p != "" {
		rel, err := Acquire(p, 0)
		if err != nil {
			os.Stdout.WriteString("fail\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("held\n")
		buf := make([]byte, 1)
		os.Stdin.Read(buf)
		rel()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExclusiveAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "RECORDPRIMS_LOCK_HELPER="+path)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := stdout.Read(buf); err != nil || string(buf[:4]) != "held" {
		t.Fatalf("helper did not acquire: %q %v", buf, err)
	}
	// Fail fast while the other process holds it.
	if _, err := Acquire(path, 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	// A timed acquire succeeds once the holder releases.
	go func() { time.Sleep(100 * time.Millisecond); stdin.Close() }()
	rel, err := Acquire(path, 5*time.Second)
	if err != nil {
		t.Fatalf("timed acquire after release: %v", err)
	}
	rel()
	cmd.Wait()
}

func TestReleaseOnProcessExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "RECORDPRIMS_LOCK_HELPER="+path)
	stdout, _ := cmd.StdoutPipe()
	cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	stdout.Read(buf)
	// Kill the holder: the OS must release the lock (no stale lock).
	cmd.Process.Kill()
	cmd.Wait()
	rel, err := Acquire(path, 2*time.Second)
	if err != nil {
		t.Fatalf("lock not released after holder was killed: %v", err)
	}
	rel()
}
