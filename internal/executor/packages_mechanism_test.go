package executor

import (
	"archive/zip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackagePathMechanismPython proves the chosen sys.path mechanism —
// injecting the tier directories via the allowlisted PYTHONPATH variable —
// actually makes a package importable with no installation step, and that it
// works for `python3 -c`, `python3 file.py` and `python3 -m module` alike.
//
// nsjail cannot run unprivileged in this environment, so this test drives the
// host `python3` with the exact environment the executor builds (withPackagePath
// over a real directory). The sandbox paths themselves are asserted against the
// generated nsjail config in TestRenderConfigPackageTiers.
func TestPackagePathMechanismPython(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	root := t.TempDir()

	// A tiny fixture package: <root>/fixture_pkg/__init__.py with a marker.
	pkgDir := filepath.Join(root, "fixture_pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "__init__.py"),
		[]byte("MARKER = 'shared-tier-ok'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A module usable with `python3 -m` must expose a __main__.
	if err := os.WriteFile(filepath.Join(pkgDir, "__main__.py"),
		[]byte("print('module-ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build the environment exactly as the runner does.
	env := withPackagePath(os.Environ(), root)
	hasPath := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PYTHONPATH=") && strings.Contains(kv, root) {
			hasPath = true
		}
	}
	if !hasPath {
		t.Fatalf("withPackagePath did not inject PYTHONPATH containing %q", root)
	}

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(py, args...)
		cmd.Env = env
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("python3 %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	if got := run("-c", "import fixture_pkg; print(fixture_pkg.MARKER)"); strings.TrimSpace(got) != "shared-tier-ok" {
		t.Fatalf("python3 -c import output = %q", got)
	}

	script := filepath.Join(root, "use.py")
	if err := os.WriteFile(script, []byte("import fixture_pkg\nprint(fixture_pkg.MARKER)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run("use.py"); strings.TrimSpace(got) != "shared-tier-ok" {
		t.Fatalf("python3 use.py output = %q", got)
	}

	if got := run("-m", "fixture_pkg"); strings.TrimSpace(got) != "module-ok" {
		t.Fatalf("python3 -m fixture_pkg output = %q", got)
	}
}

// TestBarePipInstallMechanism proves the acceptance requirement end to end on
// the host interpreter: with EXACTLY the engine's pip defaults (pipEnvDefaults
// minus the sandbox-mount target, swapped for a temp dir) and NOTHING else, a
// bare `pip install <wheel>` succeeds and the interpreter can import what it
// installed. No --break-system-packages, no --user, no venv and no --target on
// the command line: all of that comes from the environment, which is what the
// engine injects.
//
// The install is offline (a locally built pure-python wheel + --no-index) so it
// is deterministic and needs no egress; the network-dependent live case lives
// in the integration tests. If python3/pip are absent the test skips.
func TestBarePipInstallMechanism(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	if err := exec.Command(py, "-m", "pip", "--version").Run(); err != nil {
		t.Skip("python3 has no pip")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "private")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	wheel := filepath.Join(dir, "tiny_pkg-0.0.1-py3-none-any.whl")
	writeTinyWheel(t, wheel)

	// The engine's env, exactly. PIP_TARGET names the temp tier instead of the
	// sandbox mount because /opt/python/private does not exist on the test host.
	var env []string
	for _, kv := range pipEnvDefaultsFor(target) {
		env = append(env, kv)
	}
	// A minimal base like the sandbox template provides.
	env = append(env, "PATH="+os.Getenv("PATH"), "HOME="+dir)

	// THE bare command: no flags at all beyond the offline source.
	cmd := exec.Command(py, "-m", "pip", "install", "--no-index", "--find-links", dir, "tiny_pkg")
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bare pip install failed: %v\n%s", err, out)
	}
	if !strings.Contains(strings.ToLower(string(out)), "successfully installed") {
		t.Fatalf("pip did not report a successful install:\n%s", out)
	}

	// The tier directory is where the package landed.
	if _, err := os.Stat(filepath.Join(target, "tiny_pkg", "__init__.py")); err != nil {
		t.Fatalf("package not installed into the tier: %v\n%s", err, out)
	}

	// The acceptance test: the interpreter imports what pip installed, with the
	// tier on PYTHONPATH (as withPackagePath puts it in the sandbox).
	imp := exec.Command(py, "-c", "import tiny_pkg; print(tiny_pkg.VALUE)")
	imp.Env = append(append([]string{}, env...), "PYTHONPATH="+target)
	imp.Dir = dir
	got, err := imp.CombinedOutput()
	if err != nil {
		t.Fatalf("import after install failed: %v\n%s", err, got)
	}
	if strings.TrimSpace(string(got)) != "tiny-ok" {
		t.Fatalf("import output = %q, want tiny-ok", got)
	}
}

// TestBarePipInstallExplicitTargetWins proves the bot's own path is unaffected:
// with PIP_TARGET set AND an explicit --target, pip installs into the explicit
// --target (measured on pip 24), so the double-target risk is a non-issue.
// Neither directory is ever /usr.
func TestBarePipInstallExplicitTargetWins(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	if err := exec.Command(py, "-m", "pip", "--version").Run(); err != nil {
		t.Skip("python3 has no pip")
	}
	dir := t.TempDir()
	envTarget := filepath.Join(dir, "env-tier")
	cliTarget := filepath.Join(dir, "cli-tier")
	for _, d := range []string{envTarget, cliTarget} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wheel := filepath.Join(dir, "tiny_pkg-0.0.1-py3-none-any.whl")
	writeTinyWheel(t, wheel)

	env := append(pipEnvDefaultsFor(envTarget), "PATH="+os.Getenv("PATH"), "HOME="+dir)
	cmd := exec.Command(py, "-m", "pip", "install", "--no-index", "--find-links", dir,
		"--target", cliTarget, "tiny_pkg")
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pip install with explicit target failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(cliTarget, "tiny_pkg", "__init__.py")); err != nil {
		t.Fatalf("explicit --target did not receive the package: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(envTarget, "tiny_pkg")); !os.IsNotExist(err) {
		t.Fatalf("PIP_TARGET also received the package, so --target did not win:\n%s", out)
	}
}

// writeTinyWheel writes a minimal, dependency-free pure-python wheel whose
// module prints a known marker. No network, no build backend, no compiler.
func writeTinyWheel(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	add("tiny_pkg/__init__.py", "VALUE = 'tiny-ok'\n")
	add("tiny_pkg-0.0.1.dist-info/METADATA",
		"Metadata-Version: 2.1\nName: tiny-pkg\nVersion: 0.0.1\n\n")
	add("tiny_pkg-0.0.1.dist-info/WHEEL",
		"Wheel-Version: 1.0\nGenerator: test\nRoot-Is-Purelib: true\nTag: py3-none-any\n")
	add("tiny_pkg-0.0.1.dist-info/RECORD", "")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}
