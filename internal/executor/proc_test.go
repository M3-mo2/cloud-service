package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyntheticProcReportsWorkspaceLimits proves the curated /proc carries the
// workspace's OWN cpu/memory values and no host value, so free/uptime/top read
// the workspace, never the host.
func TestSyntheticProcReportsWorkspaceLimits(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeSyntheticProc(dir, 2, 512, 256, "0.0.0-sandbox"); err != nil {
		t.Fatalf("writeSyntheticProc: %v", err)
	}
	mem, err := os.ReadFile(filepath.Join(dir, "meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	ms := string(mem)
	if !strings.Contains(ms, "MemTotal:") || !strings.Contains(ms, "524288 kB") {
		t.Fatalf("meminfo does not carry the workspace memory:\n%s", ms)
	}
	if !strings.Contains(ms, "SwapTotal:             0 kB") {
		t.Fatalf("meminfo SwapTotal is not zero:\n%s", ms)
	}
	ci, _ := os.ReadFile(filepath.Join(dir, "cpuinfo"))
	if got := strings.Count(string(ci), "processor"); got != 2 {
		t.Fatalf("cpuinfo processors = %d, want 2", got)
	}
	st, _ := os.ReadFile(filepath.Join(dir, "stat"))
	if got := strings.Count(string(st), "cpu"); got < 3 {
		t.Fatalf("stat should carry an aggregate and 2 cpu lines:\n%s", st)
	}
	ver, _ := os.ReadFile(filepath.Join(dir, "version"))
	if !strings.Contains(string(ver), "0.0.0-sandbox") {
		t.Fatalf("version does not carry the workspace kernel: %s", ver)
	}
	mounts, _ := os.ReadFile(filepath.Join(dir, "mounts"))
	if strings.Contains(strings.ToLower(string(mounts)), "zfs") {
		t.Fatalf("synthetic mounts names the host filesystem:\n%s", mounts)
	}
	if !strings.Contains(string(mounts), procMountSource) {
		t.Fatalf("synthetic mounts lacks the neutral device token:\n%s", mounts)
	}
	// The self/limits file must agree with the same memory and disk values.
	lim, _ := os.ReadFile(filepath.Join(dir, "self", "limits"))
	if !strings.Contains(string(lim), "536870912") { // 512 MiB
		t.Fatalf("self/limits does not carry the memory ceiling:\n%s", lim)
	}
}

// TestSyntheticProcDistinctPerWorkspace proves two differently-limited
// workspaces produce DIFFERENT /proc content (the hardcoded-value guard).
func TestSyntheticProcDistinctPerWorkspace(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	if _, err := writeSyntheticProc(d1, 1, 512, 256, "0.0.0-sandbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSyntheticProc(d2, 2, 1024, 512, "0.0.0-sandbox"); err != nil {
		t.Fatal(err)
	}
	m1, _ := os.ReadFile(filepath.Join(d1, "meminfo"))
	m2, _ := os.ReadFile(filepath.Join(d2, "meminfo"))
	if string(m1) == string(m2) {
		t.Fatal("meminfo identical across differently-limited workspaces (hardcoded)")
	}
	c1, _ := os.ReadFile(filepath.Join(d1, "cpuinfo"))
	c2, _ := os.ReadFile(filepath.Join(d2, "cpuinfo"))
	if string(c1) == string(c2) {
		t.Fatal("cpuinfo identical across differently-limited workspaces (hardcoded)")
	}
}
