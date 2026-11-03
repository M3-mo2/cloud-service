package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sandboxd/internal/model"
)

// TestWorkspaceCgroupNameAndPath pins the derived parent name and path: the
// workspace id is embedded (a ULID, so it is stable and non-guessable), the
// prefix keeps workspace parents out of the way of the per-run NSJAIL.* dirs,
// and a path can never escape the mount.
func TestWorkspaceCgroupNameAndPath(t *testing.T) {
	wsID := "ws_01HZZZZZZZZZZZZZZZZZZZZZZZ"
	if got := WorkspaceCgroupName(wsID); got != "ws-"+wsID {
		t.Fatalf("name = %q, want ws-<id>", got)
	}
	if got := WorkspaceCgroupPath(wsID); got != filepath.Join(CgroupMount, "ws-"+wsID) {
		t.Fatalf("path = %q", got)
	}
	// The testable core must not escape the base even for a hostile id.
	base := "/sys/fs/cgroup"
	if got := workspaceCgroupPathAt(base, "../../etc"); !strings.HasPrefix(got, base+string(filepath.Separator)) {
		t.Fatalf("path %q escaped the mount %q", got, base)
	}
}

// TestEnsureWorkspaceCgroupAtWritesLimits proves the parent is created with the
// delegated controllers AND every enforced limit, using a temp dir as the cgroup
// mount. The number written to memory.max is exactly the advertised memory_mb,
// so the number the API reports is the number the kernel enforces.
func TestEnsureWorkspaceCgroupAtWritesLimits(t *testing.T) {
	base := t.TempDir()
	ws := &model.Workspace{
		ID:     "ws_unit",
		Limits: model.Limits{CPUCores: 2, MemoryMB: 256, DiskMB: 10, MaxRuns: 3},
	}
	if err := ensureWorkspaceCgroupAt(base, ws); err != nil {
		t.Fatalf("provision: %v", err)
	}
	dir := workspaceCgroupPathAt(base, ws.ID)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return strings.TrimSpace(string(b))
	}
	if got := read("cgroup.subtree_control"); got != strings.TrimSpace(workspaceCgroupControllers) {
		t.Fatalf("subtree_control = %q, want %q", got, workspaceCgroupControllers)
	}
	if got := read("memory.max"); got != "268435456" {
		t.Fatalf("memory.max = %q, want 268435456 (256 MiB)", got)
	}
	if got := read("memory.swap.max"); got != "0" {
		t.Fatalf("memory.swap.max = %q, want 0 (a real physical ceiling)", got)
	}
	if got := read("memory.oom.group"); got != "1" {
		t.Fatalf("memory.oom.group = %q, want 1 (a whole-run OOM)", got)
	}
	if got := read("pids.max"); got != "192" {
		t.Fatalf("pids.max = %q, want 192 (max_runs 3 * 64)", got)
	}
	if got := read("cpu.max"); got != "2000000 1000000" {
		t.Fatalf("cpu.max = %q, want 2000000 1000000 (2 cores)", got)
	}
	// Idempotent: a second call must not fail or change the values.
	if err := ensureWorkspaceCgroupAt(base, ws); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	if got := read("memory.max"); got != "268435456" {
		t.Fatalf("memory.max after re-provision = %q", got)
	}
}

// TestEnsureWorkspaceCgroupRejectsEmptyID proves a workspace with no id is a
// loud error, never a cgroup named only by the prefix.
func TestEnsureWorkspaceCgroupRejectsEmptyID(t *testing.T) {
	if err := ensureWorkspaceCgroupAt(t.TempDir(), &model.Workspace{}); err == nil {
		t.Fatal("expected an error for an empty workspace id")
	}
	if err := ensureWorkspaceCgroupAt(t.TempDir(), nil); err == nil {
		t.Fatal("expected an error for a nil workspace")
	}
}

// TestReadOOMKill proves the OOM counter is parsed from memory.events.local, and
// that a missing file yields -1 (no counter) rather than a fabricated 0.
func TestReadOOMKill(t *testing.T) {
	dir := t.TempDir()
	if got := readOOMKill(dir); got != -1 {
		t.Fatalf("missing events file = %d, want -1", got)
	}
	events := "low 0\nhigh 0\nmax 37\noom 1\noom_kill 2\noom_group_kill 1\n"
	if err := os.WriteFile(filepath.Join(dir, "memory.events.local"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readOOMKill(dir); got != 2 {
		t.Fatalf("oom_kill = %d, want 2", got)
	}
	if got := readOOMKill(""); got != -1 {
		t.Fatalf("empty dir = %d, want -1", got)
	}
}

// TestParseCgroupProcsLineNested proves CgroupDirOfPID logic (the membership
// parser) resolves a nested leaf, driven directly on a synthetic cgroup line so
// the layout change is unit-pinned without a live jail.
func TestParseCgroupLineNested(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"0::/ws-abc/NSJAIL.123", filepath.Join(CgroupMount, "ws-abc", "NSJAIL.123")},
		{"0::/NSJAIL.456", filepath.Join(CgroupMount, "NSJAIL.456")},
		{"0::/system.slice/foo.service", ""},
		{"1:name=systemd:/x", ""},
	}
	for _, c := range cases {
		if got := parseCgroupLine(c.line); got != c.want {
			t.Errorf("parseCgroupLine(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}
