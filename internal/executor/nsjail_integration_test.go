//go:build integration

// These tests invoke a real nsjail binary and require root (or setuid nsjail).
// They are excluded from normal builds and CI.

package executor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// TestIntegrationSessionRealPTY is the real-nsjail proof that a terminal session
// gets a working PTY: the relay allocates it, `tty` prints a /dev/pts/N device,
// and `test -t 1` succeeds -- not "out of pty devices" and not a traceback. It
// also reads a command's output through the byte-offset reader, the same
// mechanism GET .../output uses.
//
// [requires privileged container] This needs a real nsjail and root; it skips
// when either is absent and is excluded from normal builds by the integration
// build tag.
func TestIntegrationSessionRealPTY(t *testing.T) {
	if _, err := exec.LookPath("nsjail"); err != nil {
		t.Skip("nsjail not installed")
	}
	if os.Geteuid() != 0 {
		t.Skip("real nsjail session needs root [requires privileged container]")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker.txt"), []byte("marker-content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	r := NewNsjailRunner(cfg, writeTemplate(t), t.TempDir())
	ws := &model.Workspace{
		ID:     "ws_ses_it",
		Limits: model.Limits{CPUCores: 1, MemoryMB: 256, DiskMB: 10, MaxWallTimeSec: 30, MaxRuns: 1},
	}
	logPath := filepath.Join(t.TempDir(), "output.log")
	lp, err := r.Start(Request{
		Workspace:        ws,
		WorkspaceRoot:    root,
		Cwd:              "",
		PTY:              true,
		Shell:            "/bin/sh",
		Cols:             80,
		Rows:             24,
		StdoutPath:       logPath,
		SessionOutputCap: 1 << 20,
	})
	if err != nil {
		t.Fatalf("Start session: %v", err)
	}
	defer lp.Kill()

	send := func(data string) {
		msg, _ := json.Marshal(map[string]any{"type": "input", "data": data})
		if err := lp.Write(append(msg, '\n')); err != nil {
			t.Fatalf("write input: %v", err)
		}
	}
	send("tty; test -t 1 && echo TTY_OK\n")
	send("cat marker.txt\n")

	readAll := func() string {
		b, _ := os.ReadFile(logPath)
		return string(b)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out := readAll()
		if strings.Contains(out, "TTY_OK") && strings.Contains(out, "marker-content") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	out := readAll()
	if strings.Contains(out, "out of pty devices") || strings.Contains(out, "Traceback") {
		t.Fatalf("session output shows a PTY failure, not a terminal:\n%s", out)
	}
	if !strings.Contains(out, "/dev/pts/") {
		t.Fatalf("`tty` did not report a pty device:\n%s", out)
	}
	if !strings.Contains(out, "TTY_OK") {
		t.Fatalf("`test -t 1` did not succeed on the PTY:\n%s", out)
	}
	if !strings.Contains(out, "marker-content") {
		t.Fatalf("command output did not arrive:\n%s", out)
	}

	// Read the command output through the byte-offset reader (the same
	// offset/limit mechanism GET .../output uses): a slice from a later offset
	// must be a suffix, and reading from 0 must contain the marker.
	if !strings.Contains(sliceFromOffset(t, logPath, 0), "marker-content") {
		t.Fatalf("offset reader from 0 lost the command output:\n%s", out)
	}
	total := int64(len(out))
	late := sliceFromOffset(t, logPath, total-4)
	if len(late) == 0 || !strings.HasSuffix(out, late) {
		t.Fatalf("offset reader near EOF = %q, want a suffix of %q", late, out)
	}

	if err := lp.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	res, err := lp.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("session exit = %d", res.ExitCode)
	}
}

// sliceFromOffset reads a file from offset to the end, mirroring the run-log /
// session-output offset semantics.
func sliceFromOffset(t *testing.T, path string, offset int64) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(b)) {
		offset = int64(len(b))
	}
	return string(b[offset:])
}
