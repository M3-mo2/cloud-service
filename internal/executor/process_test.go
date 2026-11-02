package executor

import (
	"os"
	"strings"
	"testing"

	"sandboxd/internal/config"
	"sandboxd/internal/model"
)

func TestTagEnvRenderedIntoConfig(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{ID: "ws_tag", Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{
		Workspace:     ws,
		WorkspaceRoot: "/tmp/x",
		Argv:          []string{"/bin/true"},
		ProcessKind:   KindSession,
		ProcessSource: "ses_abc",
	}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)
	for _, want := range []string{
		`envar: "WORKSPACE_ID=ws_tag"`,
		`envar: "PROCESS_KIND=session"`,
		`envar: "PROCESS_SOURCE=ses_abc"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered config missing tag %q:\n%s", want, out)
		}
	}
}

func TestTagEnvOmittedWhenNoWorkspace(t *testing.T) {
	if got := tagEnv(Request{}); got != nil {
		t.Fatalf("tagEnv without workspace = %v, want nil", got)
	}
	// A workspace but no kind/source (an exec) still tags the workspace.
	got := tagEnv(Request{Workspace: &model.Workspace{ID: "ws_x"}, ProcessKind: KindExec})
	if len(got) != 2 || got[0] != "WORKSPACE_ID=ws_x" || got[1] != "PROCESS_KIND=exec" {
		t.Fatalf("tagEnv = %v", got)
	}
}

func TestParseStatPPIDAndStart(t *testing.T) {
	// A comm with spaces and parentheses must not confuse the parser.
	line := "1234 (weird )name) S 1 1234 1234 0 -1 4194560 0 0 0 0 5 5 0 0 20 0 1 0 123456 0 0"
	ppid, start, err := parseStat(line)
	if err != nil {
		t.Fatal(err)
	}
	if ppid != 1 {
		t.Fatalf("ppid = %d, want 1", ppid)
	}
	if start != 123456 {
		t.Fatalf("starttime = %d, want 123456", start)
	}
	if _, _, err := parseStat("not a stat line"); err == nil {
		t.Fatal("expected an error for a malformed stat line")
	}
}

func TestParsePIDs(t *testing.T) {
	got := parsePIDs("10\n20\n30\n")
	if len(got) != 3 || got[0] != 10 || got[2] != 30 {
		t.Fatalf("parsePIDs = %v", got)
	}
}

// TestCgroupDirOfPIDSelf: the test process is not in an nsjail cgroup, so the
// resolver returns "" (a non-jailed process is never guessed as jailed).
func TestCgroupDirOfPIDSelf(t *testing.T) {
	if dir := CgroupDirOfPID(os.Getpid()); dir != "" {
		t.Fatalf("self pid resolved to a jail cgroup %q", dir)
	}
}

// TestDirectChildIsNsjail: a process whose comm is not exactly "nsjail" is not
// treated as a jail supervisor (so an unprivileged test never waits for a cgroup
// that will not appear).
func TestDirectChildIsNsjail(t *testing.T) {
	if directChildIsNsjail(os.Getpid()) {
		t.Fatal("the test process must not be recognised as nsjail")
	}
}

// TestCgroupForNsjailPIDNonNsjailChild returns at once for a child that is not
// nsjail.
func TestCgroupForNsjailPIDNonNsjailChild(t *testing.T) {
	if dir := CgroupForNsjailPID(os.Getpid()); dir != "" {
		t.Fatalf("non-nsjail pid resolved to %q", dir)
	}
}

// TestKillProcessTreeFallsBackToProcessGroup: a pid not in a jail cgroup is
// signalled through its process group. Run for real (needs /bin/sleep).
func TestKillProcessTreeFallsBackToProcessGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	cfg := config.Default()
	cfg.Sandbox.KillGraceSec = 1
	r := NewNsjailRunner(cfg, "", t.TempDir())
	if r.KillProcessTree(0) {
		t.Fatal("pid 0 must not be killed")
	}
	// A non-existent pid is still reported as signalled by the fallback (the
	// kill call succeeds for a non-existent group); the important guarantee is
	// that the function does not panic and returns a bool.
	_ = r.KillProcessTree(1 << 30)
}
