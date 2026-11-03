package service

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRestoreStagedContentSurvivesAFifo is the regression test for a real hang.
//
// A workspace legitimately contains a FIFO: the terminal session creates
// `.term/in`. When an existing workspace is migrated onto the FUSE disk backend,
// its files are renamed into a sibling `.disk-stage`, the ext4 image is created,
// and the entries are copied back. Opening a FIFO for reading BLOCKS until a
// writer appears, so a copy implemented with os.ReadFile hangs forever on that
// workspace, the staged directory is never removed, and from the user's point of
// view every file has vanished. Recreating special nodes is the fix.
func TestRestoreStagedContentSurvivesAFifo(t *testing.T) {
	base := t.TempDir()
	stage := filepath.Join(base, ".disk-stage")
	root := filepath.Join(base, "files")
	for _, d := range []string{stage, root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stage, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(stage, ".term", "in")
	if err := os.MkdirAll(filepath.Dir(fifo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable here: %v", err)
	}
	// A FIFO must come through as a FIFO, in a nested directory too.
	if err := syscall.Mkfifo(filepath.Join(stage, "top"), 0o644); err != nil {
		t.Skipf("mkfifo unavailable here: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- restoreStagedContent(root, stage) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("restoreStagedContent: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("restoreStagedContent HUNG on a FIFO: a special file must be " +
			"recreated, never opened for reading")
	}

	for _, p := range []string{"top", filepath.Join(".term", "in")} {
		fi, err := os.Lstat(filepath.Join(root, p))
		if err != nil {
			t.Fatalf("%s did not survive the restore: %v", p, err)
		}
		if fi.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("%s came back as %v, want a FIFO", p, fi.Mode().Type())
		}
	}
	if got, err := os.ReadFile(filepath.Join(root, "main.py")); err != nil || string(got) != "print(1)\n" {
		t.Fatalf("main.py = %q (err %v), want the staged bytes", got, err)
	}
}
