package executor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/model"
)

func testCfg() config.Config {
	c := config.Default()
	c.Sandbox.NsjailPath = "/nonexistent/nsjail"
	c.Sandbox.KillGraceSec = 1
	// Pin ids so rendered-config assertions are deterministic.
	c.Sandbox.UID = 993
	c.Sandbox.GID = 971
	return c
}

func TestAvailableFalseWhenMissing(t *testing.T) {
	r := NewNsjailRunner(testCfg(), "", t.TempDir())
	if r.Available() {
		t.Fatal("expected unavailable")
	}
}

func TestAvailableTrueWhenPresent(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "nsjail")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testCfg()
	cfg.Sandbox.NsjailPath = bin
	r := NewNsjailRunner(cfg, "", t.TempDir())
	if !r.Available() {
		t.Fatal("expected available")
	}
}

func TestRunReturnsUnavailable(t *testing.T) {
	r := NewNsjailRunner(testCfg(), "", t.TempDir())
	_, err := r.Run(Request{Workspace: &model.Workspace{Limits: model.Limits{MemoryMB: 128, MaxWallTimeSec: 10}}, Argv: []string{"true"}})
	if err != ErrNsjailUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func writeTemplate(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "configs", "nsjail_python.cfg.tmpl")
	dst := filepath.Join(t.TempDir(), "nsjail_python.cfg.tmpl")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestRenderConfig(t *testing.T) {
	cfg := testCfg()
	r := NewNsjailRunner(cfg, writeTemplate(t), t.TempDir())
	ws := &model.Workspace{
		ID: "ws_test",
		Limits: model.Limits{
			CPUCores: 2, MemoryMB: 256, DiskMB: 10, MaxWallTimeSec: 30, MaxRuns: 3, AllowNet: false,
		},
	}
	path, err := r.renderConfig(Request{
		Workspace:     ws,
		WorkspaceRoot: "/tmp/wsroot",
		Argv:          []string{"python3", "main.py"},
		Cwd:           "/",
		Env:           map[string]string{"FOO": "bar"},
		Timeout:       30 * time.Second,
	}, "/tmp/run-tmp")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	defer os.Remove(path)
	b, _ := os.ReadFile(path)
	out := string(b)

	for _, want := range []string{
		`clone_newnet: true`,
		`time_limit: 30`,
		`cgroup_mem_max: 268435456`,
		`cgroup_cpu_ms_per_sec: 2000`,
		`rlimit_nproc: 192`,
		`use_cgroupv2: true`,
		`mount_proc: false`,
		`/tmp/wsroot`,
		`/tmp/run-tmp`,
		`exec_bin`,
		`python3`,
		`FOO=bar`,
		`KILL`,
		`DEFAULT ALLOW`,
		`inside_id: "993"`,
		`count: 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered config missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, `chroot:`) {
		t.Error("chroot is not a valid nsjail config field and must not be rendered")
	}
	if strings.Contains(out, `disable_proc:`) {
		t.Error("disable_proc is not a valid nsjail config field (use mount_proc)")
	}
}

func TestRenderConfigAllowNet(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1, AllowNet: true}}
	path, err := r.renderConfig(Request{Workspace: ws, WorkspaceRoot: "/tmp/x", Argv: []string{"/bin/true"}}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "clone_newnet: false") {
		t.Errorf("allow_net should disable network namespace isolation:\n%s", b)
	}
}

func TestFormatEnvSortsAndSanitizes(t *testing.T) {
	got := formatEnv(map[string]string{"Z": "1", "A": "2", "bad-key": "3", "B": "4"})
	want := []string{"A=2", "B=4", "Z=1"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestSeccompPolicyHasDangerousCalls(t *testing.T) {
	pol := strings.Join(seccompPolicy(), ",")
	for _, sys := range []string{"mount", "ptrace", "bpf", "reboot", "setns", "pivot_root"} {
		if !strings.Contains(pol, sys) {
			t.Errorf("policy missing %q", sys)
		}
	}
	// libseccomp does not know these names; including them breaks compilation.
	for _, bad := range []string{"umount2", "_sysctl"} {
		for _, s := range seccompPolicy() {
			if s == bad {
				t.Errorf("policy contains libseccomp-invalid name %q", bad)
			}
		}
	}
}

func TestSeccompStringIsQuotedPolicy(t *testing.T) {
	s := seccompString()
	if !strings.HasPrefix(s, `"`) || !strings.HasSuffix(s, `"`) {
		t.Fatalf("seccomp string must be a quoted literal: %s", s)
	}
	if !strings.Contains(s, "DEFAULT ALLOW") || !strings.Contains(s, "KILL") {
		t.Fatalf("unexpected seccomp string: %s", s)
	}
}

func TestCappedWriterTruncates(t *testing.T) {
	var sb strings.Builder
	w := newCappedWriter(&sb, 5)
	n, err := w.Write([]byte("1234567890"))
	if err != nil || n != 10 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if sb.String() != "12345" {
		t.Fatalf("got %q", sb.String())
	}
	if !w.truncated {
		t.Fatal("expected truncated")
	}
	// Further writes dropped entirely.
	w.Write([]byte("xyz"))
	if sb.String() != "12345" {
		t.Fatalf("got %q", sb.String())
	}
}

func TestCappedWriterUnlimited(t *testing.T) {
	var sb strings.Builder
	w := newCappedWriter(&sb, 0)
	w.Write([]byte("hello"))
	if sb.String() != "hello" || w.truncated {
		t.Fatalf("got %q truncated=%v", sb.String(), w.truncated)
	}
}

func TestKillProcessGroupUnknown(t *testing.T) {
	r := NewNsjailRunner(testCfg(), "", t.TempDir())
	if r.Kill("run_nope") {
		t.Fatal("killing unknown run should return false")
	}
}

func TestKillProcessGroupReal(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start process: %v", err)
	}
	killed := killProcessGroup(cmd.Process.Pid, 500*time.Millisecond)
	if !killed {
		t.Fatal("expected kill")
	}
	// Reap the child; a signalled exit confirms the process was terminated.
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected signalled exit, got %v", err)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if ok && !ws.Signaled() {
		t.Fatalf("expected signal termination, status=%v", ws)
	}
}
