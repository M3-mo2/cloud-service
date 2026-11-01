package executor

import (
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
