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
	// /proc/self/mountinfo must exist (coreutils `df` reads it; without it a
	// bare `df -h` errors), be in the real mountinfo format, and name the
	// workspace's own mount with a neutral device token.
	mi, err := os.ReadFile(filepath.Join(dir, "self", "mountinfo"))
	if err != nil {
		t.Fatalf("self/mountinfo missing (df -h would break): %v", err)
	}
	mis := string(mi)
	if !strings.Contains(mis, " - fuse.ext4 "+procMountSource+" ") {
		t.Fatalf("mountinfo lacks the workspace fuse.ext4 mount:\n%s", mis)
	}
	if strings.Contains(strings.ToLower(mis), "zfs") {
		t.Fatalf("mountinfo names the host filesystem:\n%s", mis)
	}
	// Every mountinfo line must have the mandatory fields: id parent maj:min
	// root mount_point options then " - " then fstype source super_options.
	for _, line := range strings.Split(strings.TrimSpace(mis), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			t.Fatalf("mountinfo line too short (%d fields): %q", len(f), line)
		}
		sep := -1
		for i, x := range f {
			if x == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || sep+4 != len(f) {
			t.Fatalf("mountinfo line has wrong shape around '-': %q", line)
		}
	}
	// The self/limits file must agree with the same memory and disk values.
	lim, _ := os.ReadFile(filepath.Join(dir, "self", "limits"))
	if !strings.Contains(string(lim), "536870912") { // 512 MiB
		t.Fatalf("self/limits does not carry the memory ceiling:\n%s", lim)
	}
}

// TestSyntheticProcMountsAndMountinfoAgree proves the two mount views are
// rendered from one list: every target in /proc/mounts appears in
// /proc/self/mountinfo with the same fstype and device token, so they can never
// disagree (which is the property the fix relies on).
func TestSyntheticProcMountsAndMountinfoAgree(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeSyntheticProc(dir, 1, 256, 256, "0.0.0-sandbox"); err != nil {
		t.Fatal(err)
	}
	mounts, _ := os.ReadFile(filepath.Join(dir, "mounts"))
	mi, _ := os.ReadFile(filepath.Join(dir, "self", "mountinfo"))
	for _, line := range strings.Split(strings.TrimSpace(string(mounts)), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			t.Fatalf("mounts line too short: %q", line)
		}
		target := f[1]
		// The mountinfo line for this target must carry the same fstype/source.
		found := false
		for _, ml := range strings.Split(strings.TrimSpace(string(mi)), "\n") {
			mf := strings.Fields(ml)
			if len(mf) < 10 {
				continue
			}
			if mf[4] == target && strings.Contains(ml, " "+f[2]+" "+f[0]+" ") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("mounts entry %q has no matching mountinfo row:\n%s", line, mi)
		}
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
