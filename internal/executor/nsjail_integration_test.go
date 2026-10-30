//go:build integration

// These tests invoke a real nsjail binary and require root (or setuid nsjail).
// They are excluded from normal builds and CI.

package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/model"
)

func TestIntegrationPythonHello(t *testing.T) {
	if _, err := exec.LookPath("nsjail"); err != nil {
		t.Skip("nsjail not installed")
	}
	root := t.TempDir()
	script := filepath.Join(root, "main.py")
	if err := os.WriteFile(script, []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	r := NewNsjailRunner(cfg, writeTemplate(t), t.TempDir())
	ws := &model.Workspace{
		ID:     "ws_it",
		Limits: model.Limits{CPUCores: 1, MemoryMB: 256, DiskMB: 10, MaxWallTimeSec: 30, MaxRuns: 1},
	}
	res, err := r.Run(Request{
		Workspace:     ws,
		WorkspaceRoot: root,
		Argv:          []string{"/usr/bin/python3", "main.py"},
		Cwd:           "/",
		Timeout:       20 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	if string(res.Stdout) != "hi\n" {
		t.Fatalf("stdout=%q", res.Stdout)
	}
}

func TestIntegrationTimeout(t *testing.T) {
	if _, err := exec.LookPath("nsjail"); err != nil {
		t.Skip("nsjail not installed")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "loop.py"), []byte("while True: pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	r := NewNsjailRunner(cfg, writeTemplate(t), t.TempDir())
	ws := &model.Workspace{
		ID:     "ws_it2",
		Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 10, MaxWallTimeSec: 3, MaxRuns: 1},
	}
	res, err := r.Run(Request{
		Workspace:     ws,
		WorkspaceRoot: root,
		Argv:          []string{"/usr/bin/python3", "loop.py"},
		Cwd:           "/",
		Timeout:       2 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected timeout, got %+v", res)
	}
}
