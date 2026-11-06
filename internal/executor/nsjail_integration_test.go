//go:build integration

// These tests invoke a real nsjail binary and require root (or setuid nsjail).
// They are excluded from normal builds and CI.

package executor

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
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

// TestIntegrationProcessInventoryAndCgroupKill is the real-nsjail proof of the
// native process inventory and cgroup-scoped stop: a workspace-tagged process
// started inside a jail appears in ListJailProcesses, and its cgroup kill stops
// it even though it was started with setsid() and left its process group.
//
// [requires privileged container] It needs a real nsjail and root; it skips when
// either is absent and is excluded from normal builds by the integration tag.
func TestIntegrationProcessInventoryAndCgroupKill(t *testing.T) {
	if _, err := exec.LookPath("nsjail"); err != nil {
		t.Skip("nsjail not installed")
	}
	if os.Geteuid() != 0 {
		t.Skip("real nsjail needs root [requires privileged container]")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	r := NewNsjailRunner(cfg, writeTemplate(t), t.TempDir())
	ws := &model.Workspace{
		ID:     "ws_proc_it",
		Limits: model.Limits{CPUCores: 1, MemoryMB: 256, DiskMB: 10, MaxWallTimeSec: 120, MaxRuns: 4},
	}
	out := filepath.Join(t.TempDir(), "o.log")
	errOut := filepath.Join(t.TempDir(), "e.log")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Run(Request{
			Workspace:     ws,
			WorkspaceRoot: root,
			Argv:          []string{"/bin/sh", "-c", "setsid sleep 120 </dev/null >/dev/null 2>&1 & sleep 120"},
			Cwd:           "/",
			Context:       ctx,
			NoTimeout:     true,
			RunID:         "run_proc_it",
			StdoutPath:    out,
			StderrPath:    errOut,
			ProcessKind:   KindRun,
			ProcessSource: "run_proc_it",
		})
	}()

	// Poll the host inventory for a process tagged for this workspace whose argv
	// is a sleep (the setsid child, which left the process group of its shell).
	var target ProcessInfo
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range ListJailProcesses() {
			if p.WorkspaceID != ws.ID {
				continue
			}
			if len(p.Argv) > 0 && strings.Contains(p.Argv[0], "sleep") {
				target = p
				break
			}
		}
		if target.PID != 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if target.PID == 0 {
		cancel()
		<-done
		t.Fatal("no process tagged for the workspace appeared in the inventory")
	}
	if target.Kind != KindRun || target.SourceID != "run_proc_it" {
		t.Fatalf("attribution = kind %q source %q, want run/run_proc_it", target.Kind, target.SourceID)
	}

	// The cgroup-scoped kill must stop the setsid-escaping process.
	if !r.KillProcessTree(target.PID) {
		t.Fatal("KillProcessTree reported no signal")
	}
	deadline = time.Now().Add(10 * time.Second)
	alive := true
	for time.Now().Before(deadline) {
		alive = false
		for _, p := range ListJailProcesses() {
			if p.PID == target.PID {
				alive = true
				break
			}
		}
		if !alive {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if alive {
		t.Fatalf("setsid-escaping process %d survived the cgroup kill", target.PID)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// tinyWheel writes a dependency-free pure-python wheel under dir and returns
// its path. It needs no index, no build backend and no network.
func tinyWheel(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "tiny_pkg-0.0.1-py3-none-any.whl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	writeEntry := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	writeEntry("tiny_pkg/__init__.py", "VALUE = 'tiny-ok'\n")
	writeEntry("tiny_pkg-0.0.1.dist-info/METADATA",
		"Metadata-Version: 2.1\nName: tiny-pkg\nVersion: 0.0.1\n\n")
	writeEntry("tiny_pkg-0.0.1.dist-info/WHEEL",
		"Wheel-Version: 1.0\nGenerator: test\nRoot-Is-Purelib: true\nTag: py3-none-any\n")
	writeEntry("tiny_pkg-0.0.1.dist-info/RECORD", "")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// integrationPipRunner builds a real runner against the host, or skips when the
// environment cannot run nsjail.
func integrationPipRunner(t *testing.T) (*NsjailRunner, func()) {
	t.Helper()
	if _, err := exec.LookPath("nsjail"); err != nil {
		t.Skip("nsjail not installed")
	}
	if os.Geteuid() != 0 {
		t.Skip("real nsjail needs root [requires privileged container]")
	}
	cfg := config.Default()
	return NewNsjailRunner(cfg, writeTemplate(t), t.TempDir()), func() {}
}

// TestIntegrationBarePipInstallInSandbox is the ticket's acceptance test in a
// REAL sandbox: a bare `pip install <wheel>` (no --break-system-packages, no
// --user, no venv, no --target) exits 0, the engine's PIP_TARGET is active,
// and the workspace's own python imports the package afterwards. /usr is
// read-only and stays unwritten.
//
// [requires privileged container]
func TestIntegrationBarePipInstallInSandbox(t *testing.T) {
	r, cleanup := integrationPipRunner(t)
	defer cleanup()
	root := t.TempDir()
	wheel := tinyWheel(t, root)
	private := filepath.Join(t.TempDir(), "packages")
	if err := os.MkdirAll(private, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := &model.Workspace{
		ID:     "ws_pip_it",
		Limits: model.Limits{CPUCores: 1, MemoryMB: 512, DiskMB: 50, MaxWallTimeSec: 120, MaxRuns: 2, AllowNet: false},
	}
	base := Request{
		Workspace:          ws,
		WorkspaceRoot:      root,
		Cwd:                "/",
		Timeout:            90 * time.Second,
		PrivatePackagesDir: private,
	}

	// 1. The engine's defaults are what a bare command sees.
	envRes, err := r.Run(withArgv(base, []string{"python3", "-c",
		"import os; print(os.environ.get('PIP_TARGET')); print(os.environ.get('PIP_BREAK_SYSTEM_PACKAGES'))"}))
	if err != nil {
		t.Fatalf("env run: %v", err)
	}
	if envRes.ExitCode != 0 {
		t.Fatalf("env run exit=%d stderr=%s", envRes.ExitCode, envRes.Stderr)
	}
	if got := string(envRes.Stdout); !strings.Contains(got, "/opt/python/private") || !strings.Contains(got, "1") {
		t.Fatalf("engine pip defaults missing from the sandbox env: %q", got)
	}

	// 2. THE bare install: only the wheel path, no flags at all.
	installRes, err := r.Run(withArgv(base, []string{"python3", "-m", "pip", "install",
		"/workspace/" + filepath.Base(wheel)}))
	if err != nil {
		t.Fatalf("install run: %v", err)
	}
	if installRes.ExitCode != 0 {
		t.Fatalf("bare pip install exit=%d\nstdout=%s\nstderr=%s",
			installRes.ExitCode, installRes.Stdout, installRes.Stderr)
	}
	t.Logf("bare pip install output:\n%s\n%s", installRes.Stdout, installRes.Stderr)
	if !strings.Contains(strings.ToLower(string(installRes.Stdout)+string(installRes.Stderr)), "successfully installed") {
		t.Fatalf("pip did not report success:\n%s\n%s", installRes.Stdout, installRes.Stderr)
	}

	// 3. The acceptance test: the workspace python imports it (PYTHONPATH).
	importRes, err := r.Run(withArgv(base, []string{"python3", "-c",
		"import tiny_pkg; print(tiny_pkg.VALUE)"}))
	if err != nil {
		t.Fatalf("import run: %v", err)
	}
	if importRes.ExitCode != 0 || strings.TrimSpace(string(importRes.Stdout)) != "tiny-ok" {
		t.Fatalf("import after install exit=%d stdout=%q stderr=%s",
			importRes.ExitCode, importRes.Stdout, importRes.Stderr)
	}

	// 4. /usr is read-only: a write there must fail, not silently succeed.
	usrRes, err := r.Run(withArgv(base, []string{"python3", "-c",
		"open('/usr/pip-must-not-write','w').write('x')"}))
	if err != nil {
		t.Fatalf("usr write run: %v", err)
	}
	if usrRes.ExitCode == 0 {
		t.Fatalf("/usr accepted a write inside the sandbox:\n%s", usrRes.Stdout)
	}
	if out := string(usrRes.Stderr); !strings.Contains(out, "Read-only file system") && !strings.Contains(out, "Permission denied") {
		t.Fatalf("unexpected /usr write failure mode: %s", out)
	}
}

// TestIntegrationBarePipInstallNoNetFailsOnNetwork is the ticket's key
// distinction: with allow_net=false the install must fail HONESTLY on network
// reachability, not on PEP 668 or a read-only filesystem. That way a user can
// tell "I need network" apart from a real configuration problem.
//
// [requires privileged container]
func TestIntegrationBarePipInstallNoNetFailsOnNetwork(t *testing.T) {
	r, cleanup := integrationPipRunner(t)
	defer cleanup()
	root := t.TempDir()
	private := filepath.Join(t.TempDir(), "packages")
	if err := os.MkdirAll(private, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := &model.Workspace{
		ID:     "ws_pip_nonet_it",
		Limits: model.Limits{CPUCores: 1, MemoryMB: 512, DiskMB: 50, MaxWallTimeSec: 90, MaxRuns: 2, AllowNet: false},
	}
	res, err := r.Run(Request{
		Workspace:          ws,
		WorkspaceRoot:      root,
		Argv:               []string{"python3", "-m", "pip", "install", "--retries", "0", "--timeout", "5", "six"},
		Cwd:                "/",
		Timeout:            60 * time.Second,
		PrivatePackagesDir: private,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("install with no network unexpectedly succeeded:\n%s", res.Stdout)
	}
	out := string(res.Stdout) + string(res.Stderr)
	t.Logf("no-network pip output:\n%s", out)
	lower := strings.ToLower(out)
	for _, forbidden := range []string{"externally-managed-environment", "read-only file system"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("no-network failure is %q, not a network failure:\n%s", forbidden, out)
		}
	}
	for _, want := range []string{"could not find a version", "connection", "network", "resolution", "timed out", "temporary failure"} {
		if strings.Contains(lower, want) {
			return
		}
	}
	t.Fatalf("failure does not mention network/index reachability:\n%s", out)
}

// withArgv returns a copy of base with argv set (Request is a value type, but
// its slices are shared; this keeps each run independent and explicit).
func withArgv(base Request, argv []string) Request {
	base.Argv = argv
	return base
}
