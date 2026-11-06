package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	// Point profile materialization at a small fixture root so a unit test does
	// not copy the real host profile tree on every render (production reads the
	// real host once per service).
	c.Identity.ToolsRoot = toolFixtureRoot()
	return c
}

// toolFixtureRoot materializes a tiny host-like filesystem root once per test
// binary: a /usr/bin with the benign commands the profiles name (and the
// engine-named + content-embedding entries the engine filter must drop), plus a
// /usr/lib/x86_64-linux-gnu library dir.
var (
	toolFixOnce sync.Once
	toolFixRoot string
)

func toolFixtureRoot() string {
	toolFixOnce.Do(func() {
		dir, err := os.MkdirTemp("", "toolfix-")
		if err != nil {
			panic(err)
		}
		toolFixRoot = dir
		bin := filepath.Join(dir, "usr", "bin")
		sbin := filepath.Join(dir, "usr", "sbin")
		lib := filepath.Join(dir, "usr", "lib", "x86_64-linux-gnu")
		for _, d := range []string{bin, sbin, lib} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				panic(err)
			}
		}
		// Benign commands the profiles name: a plain file and a same-dir
		// symlink (python3 -> python3.12) that must materialize as real bytes.
		writeFixture(bin, "python3.12", 0o755, "#!/bin/sh\necho py\n")
		if err := os.Symlink("python3.12", filepath.Join(bin, "python3")); err != nil {
			panic(err)
		}
		writeFixture(bin, "bash", 0o755, "#!/bin/sh\necho sh\n")
		writeFixture(bin, "sh", 0o755, "#!/bin/sh\necho sh\n")
		writeFixture(bin, "ls", 0o755, "#!/bin/sh\necho ls\n")
		writeFixture(bin, "true", 0o755, "#!/bin/sh\nexit 0\n")
		writeFixture(bin, "sleep", 0o755, "#!/bin/sh\necho sleep\n")
		// Engine entries the materializer must never place in the tree.
		for _, n := range []string{"nsjail", "unshare", "nsenter", "setpriv", "runcon", "slirp4netns"} {
			writeFixture(bin, n, 0o755, "#!/bin/sh\n: engine\n")
		}
		writeFixture(sbin, "chroot", 0o755, "#!/bin/sh\n: engine\n")
		// A benign shared library.
		writeFixture(lib, "libc.so.6", 0o755, "ELF-ish\n")
	})
	return toolFixRoot
}

func writeFixture(dir, name string, mode os.FileMode, content string) {
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
		panic(err)
	}
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
		Env:           map[string]string{"PYTHONPATH": "/workspace/lib", "LD_PRELOAD": "/workspace/evil.so", "FOO": "bar"},
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
		`rlimit_as: 2048`,
		`rlimit_fsize: 10`,
		`use_cgroupv2: true`,
		`mount_proc: false`,
		`/tmp/wsroot`,
		`/tmp/run-tmp`,
		`exec_bin`,
		`python3`,
		`cwd: "/workspace"`,
		`PYTHONPATH=/workspace/lib`,
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
	// allow_net is now an EGRESS capability, not a host-network share. With no
	// engine-managed namespace the runner lets nsjail create a fresh empty one,
	// so isolation holds even for an allow_net workspace.
	if !strings.Contains(string(b), "clone_newnet: true") {
		t.Errorf("allow_net must never share the host network namespace:\n%s", b)
	}
	if strings.Contains(string(b), "clone_newnet: false") {
		t.Errorf("allow_net rendered a host-network share (clone_newnet:false) with no managed namespace:\n%s", b)
	}
}

// TestRenderConfigJoinsWorkspaceNetns proves that when the runner is given the
// workspace's own namespace it renders clone_newnet:false (stay in the already
// entered namespace) and mounts the generated egress resolver.
func TestRenderConfigJoinsWorkspaceNetns(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1, AllowNet: true}}
	path, err := r.renderConfig(Request{
		Workspace:     ws,
		WorkspaceRoot: "/tmp/x",
		Argv:          []string{"/bin/true"},
		NetnsPath:     "/run/netns/sbxdws-abc123",
		ResolverConf:  "/data/tmp/egress-resolv-abc123.conf",
	}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)
	if !strings.Contains(out, "clone_newnet: false") {
		t.Errorf("joining the workspace namespace must render clone_newnet:false:\n%s", out)
	}
	if !strings.Contains(out, "/data/tmp/egress-resolv-abc123.conf") {
		t.Errorf("the generated egress resolver must be mounted:\n%s", out)
	}
}

// TestRenderConfigRlimitUnits guards F-1 and the rlimit_as-vs-V8 finding:
// nsjail interprets rlimit_as and rlimit_fsize in megabytes, so neither may be
// byte-scaled. rlimit_as is the workspace's memory_mb PLUS a runtime
// address-space headroom (never memory_mb alone, which aborts node/V8, whose
// PROT_NONE reservations count against RLIMIT_AS), while rlimit_fsize is the
// disk limit. The physical memory ceiling remains cgroup_mem_max = memory_mb.
func TestRenderConfigRlimitUnits(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 256, DiskMB: 10, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{Workspace: ws, WorkspaceRoot: "/tmp/x", Argv: []string{"/bin/true"}}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	b, _ := os.ReadFile(path)
	out := string(b)

	// The physical ceiling is still the workspace's memory_mb.
	if !strings.Contains(out, "cgroup_mem_max: 268435456") {
		t.Errorf("cgroup_mem_max must be memory_mb in bytes (268435456), got:\n%s", out)
	}
	// rlimit_as is memory_mb + headroom, floored at minAddressSpaceMB. For a
	// 256 MB workspace that is 2048 MB.
	wantAS := fmt.Sprintf("rlimit_as: %d\n", addressSpaceMB(256))
	if !strings.Contains(out, wantAS) {
		t.Errorf("rlimit_as must be the address-space value (%s), got:\n%s", strings.TrimSpace(wantAS), out)
	}
	if !strings.Contains(out, "rlimit_fsize: 10\n") {
		t.Errorf("rlimit_fsize must be the MB value (10), got:\n%s", out)
	}
	// A byte-scaled value would be at least 1024x the intended MB figure.
	if strings.Contains(out, "rlimit_as: 268435456") || strings.Contains(out, "rlimit_fsize: 10485760") {
		t.Error("rlimit values are byte-scaled; nsjail expects megabytes")
	}
}

// TestAddressSpaceMB proves the rlimit_as formula keeps a runtime headroom over
// the physical ceiling and never falls below the floor.
func TestAddressSpaceMB(t *testing.T) {
	cases := []struct {
		memMB, want int
	}{
		{1, 2048},    // floor wins over 1+1024
		{256, 2048},  // floor wins over 1280
		{512, 2048},  // 512+1024 = 1536 < floor
		{2048, 3072}, // 2048+1024
		{8192, 9216}, // 8192+1024
	}
	for _, c := range cases {
		if got := addressSpaceMB(c.memMB); got != c.want {
			t.Errorf("addressSpaceMB(%d) = %d, want %d", c.memMB, got, c.want)
		}
	}
}

// TestRenderConfigPackageTiers proves the two-tier mount semantics are present
// in the generated nsjail config: the shared tier is mounted read-only and the
// private tier writable, both with the expected target paths and the combined
// PYTHONPATH. This is the unprivileged-environment assertion for the security
// core (the real bind mount cannot run without nsjail/root here).
func TestRenderConfigPackageTiers(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{
		Workspace:          ws,
		WorkspaceRoot:      "/tmp/wsroot",
		Argv:               []string{"/bin/true"},
		SharedPackagesDir:  "/host/shared-pkgs",
		PrivatePackagesDir: "/host/ws/packages",
	}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)

	sharedMount := `src: "/host/shared-pkgs"
  dst: "/opt/python/shared"
  is_bind: true
  rw: false`
	if !strings.Contains(out, sharedMount) {
		t.Errorf("shared tier must be mounted read-only at /opt/python/shared:\n%s", out)
	}
	privateMount := `src: "/host/ws/packages"
  dst: "/opt/python/private"
  is_bind: true
  rw: true`
	if !strings.Contains(out, privateMount) {
		t.Errorf("private tier must be mounted writable at /opt/python/private:\n%s", out)
	}
	if !strings.Contains(out, "PYTHONPATH=/opt/python/shared:/opt/python/private") {
		t.Errorf("PYTHONPATH must expose shared then private tier, got:\n%s", out)
	}
}

// TestRenderConfigNoPackageTiers proves a request without tier dirs renders no
// package mounts and no injected PYTHONPATH, so the private-only / shared-only
// degradation paths are clean.
func TestRenderConfigNoPackageTiers(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{Workspace: ws, WorkspaceRoot: "/tmp/x", Argv: []string{"/bin/true"}}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)
	if strings.Contains(out, "/opt/python/shared") || strings.Contains(out, "/opt/python/private") {
		t.Errorf("no tier dirs given, so no package mounts may be rendered:\n%s", out)
	}
	if strings.Contains(out, "PYTHONPATH=") {
		t.Errorf("no tiers mounted, so PYTHONPATH must not be injected:\n%s", out)
	}
}

// TestRenderConfigSharedOnly proves a missing/disabled private tier renders the
// shared mount (read-only) and exposes only the shared path.
func TestRenderConfigSharedOnly(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{
		Workspace:         ws,
		WorkspaceRoot:     "/tmp/x",
		Argv:              []string{"/bin/true"},
		SharedPackagesDir: "/host/shared-pkgs",
	}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)
	if !strings.Contains(out, `dst: "/opt/python/shared"`) {
		t.Errorf("shared mount missing:\n%s", out)
	}
	if strings.Contains(out, `/opt/python/private`) {
		t.Errorf("private mount must be absent when no private dir is given:\n%s", out)
	}
	if !strings.Contains(out, `envar: "PYTHONPATH=/opt/python/shared"`) {
		t.Errorf("PYTHONPATH should be exactly the shared path:\n%s", out)
	}
}

// TestRenderConfigPipEnvDefaults proves a run with a writable private tier
// carries the engine's pip defaults, so a bare `pip install X` targets the
// workspace's own tier (already on PYTHONPATH) and never the read-only system
// scheme. A run with no private tier must NOT carry PIP_TARGET (the sandbox
// path would not exist).
func TestRenderConfigPipEnvDefaults(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}

	path, err := r.renderConfig(Request{
		Workspace:          ws,
		WorkspaceRoot:      "/tmp/wsroot",
		Argv:               []string{"/bin/true"},
		PrivatePackagesDir: "/host/ws/packages",
	}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)
	for _, want := range []string{
		`envar: "PIP_TARGET=/opt/python/private"`,
		`envar: "PIP_BREAK_SYSTEM_PACKAGES=1"`,
		`envar: "PIP_NO_INPUT=1"`,
		`envar: "PIP_DISABLE_PIP_VERSION_CHECK=1"`,
		`envar: "PIP_ROOT_USER_ACTION=ignore"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("private-tier run missing pip default %q:\n%s", want, out)
		}
	}

	pathNoTier, err := r.renderConfig(Request{
		Workspace:     ws,
		WorkspaceRoot: "/tmp/x",
		Argv:          []string{"/bin/true"},
	}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(pathNoTier)
	if out := readFile(t, pathNoTier); strings.Contains(out, `envar: "PIP_`) {
		t.Errorf("a run with no private tier must not inject PIP_* defaults:\n%s", out)
	}
}

// TestPipEnvPrecedence is the security assertion for the pip defaults: a caller
// cannot override PIP_TARGET (or any PIP_* variable). PIP_* is deliberately
// absent from allowedEnvKeys, so formatEnv drops it entirely and the ENGINE's
// value is the only one that reaches the sandbox. The hostile value /usr must
// never appear as a PIP_TARGET.
func TestPipEnvPrecedence(t *testing.T) {
	for _, key := range []string{
		"PIP_TARGET", "PIP_BREAK_SYSTEM_PACKAGES", "PIP_NO_INPUT",
		"PIP_DISABLE_PIP_VERSION_CHECK", "PIP_ROOT_USER_ACTION", "PIP_CONFIG_FILE",
		"PIP_INDEX_URL",
	} {
		if allowedEnvKeys[key] {
			t.Errorf("%s must never be user-settable (allowedEnvKeys)", key)
		}
	}

	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{
		Workspace:          ws,
		WorkspaceRoot:      "/tmp/wsroot",
		Argv:               []string{"/bin/true"},
		PrivatePackagesDir: "/host/ws/packages",
		Env: map[string]string{
			"PIP_TARGET":                "/usr",
			"PIP_CONFIG_FILE":           "/workspace/evil.conf",
			"PIP_BREAK_SYSTEM_PACKAGES": "0",
		},
	}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)

	if strings.Contains(out, "PIP_TARGET=/usr") {
		t.Fatalf("caller overrode the engine pip target:\n%s", out)
	}
	if strings.Count(out, "PIP_TARGET=") != 1 {
		t.Fatalf("expected exactly one engine PIP_TARGET, got:\n%s", out)
	}
	if !strings.Contains(out, `envar: "PIP_TARGET=/opt/python/private"`) {
		t.Fatalf("engine PIP_TARGET missing:\n%s", out)
	}
	if strings.Contains(out, "evil.conf") || strings.Contains(out, "PIP_CONFIG_FILE") {
		t.Fatalf("caller-supplied PIP_CONFIG_FILE reached the sandbox:\n%s", out)
	}
	if strings.Contains(out, "PIP_BREAK_SYSTEM_PACKAGES=0") {
		t.Fatalf("caller overrode PIP_BREAK_SYSTEM_PACKAGES:\n%s", out)
	}
}

// TestRenderConfigPipEnvOrder confirms the pip defaults are emitted before the
// tag/mask/user env blocks (defence in depth on top of the allowlist gate).
func TestRenderConfigPipEnvOrder(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{ID: "ws_order", Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{
		Workspace:          ws,
		WorkspaceRoot:      "/tmp/wsroot",
		Argv:               []string{"/bin/true"},
		PrivatePackagesDir: "/host/ws/packages",
		Env:                map[string]string{"TZ": "UTC"},
	}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)
	pip := strings.Index(out, "PIP_TARGET=")
	user := strings.Index(out, "TZ=UTC")
	tag := strings.Index(out, "WORKSPACE_ID=")
	if pip < 0 || user < 0 {
		t.Fatalf("missing expected env entries:\n%s", out)
	}
	if pip > user {
		t.Errorf("pip defaults must precede user env:\n%s", out)
	}
	if tag >= 0 && pip > tag {
		t.Errorf("pip defaults must precede tag env:\n%s", out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPackageSearchPath(t *testing.T) {
	cases := []struct {
		shared, private string
		want            string
	}{
		{"", "", ""},
		{"/s", "", "/opt/python/shared"},
		{"", "/p", "/opt/python/private"},
		{"/s", "/p", "/opt/python/shared:/opt/python/private"},
	}
	for _, c := range cases {
		got := packageSearchPath(sharedPackagesMount, privatePackagesMount, c.shared, c.private)
		if got != c.want {
			t.Errorf("packageSearchPath(%q,%q) = %q, want %q", c.shared, c.private, got, c.want)
		}
	}
}

func TestWithPackagePath(t *testing.T) {
	// No package path leaves the env untouched.
	base := []string{"LANG=C.UTF-8"}
	if got := withPackagePath(base, ""); len(got) != 1 || got[0] != "LANG=C.UTF-8" {
		t.Fatalf("empty pkg path changed env: %v", got)
	}
	// A caller PYTHONPATH is preserved and the tiers follow it.
	got := withPackagePath([]string{"PYTHONPATH=/workspace/lib", "TZ=UTC"}, "/opt/python/shared")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "PYTHONPATH=/workspace/lib:/opt/python/shared") {
		t.Fatalf("caller PYTHONPATH not preserved: %v", got)
	}
	// With no caller PYTHONPATH a new entry is appended.
	got = withPackagePath([]string{"TZ=UTC"}, "/opt/python/shared")
	if strings.Join(got, " ") != "PYTHONPATH=/opt/python/shared TZ=UTC" {
		t.Fatalf("unexpected env: %v", got)
	}
}

func TestFormatEnvAllowlist(t *testing.T) {
	got := formatEnv(map[string]string{
		"TZ":                    "UTC",
		"LANG":                  "C.UTF-8",
		"PYTHONPATH":            "/workspace/lib",
		"LD_PRELOAD":            "/workspace/evil.so",
		"LD_LIBRARY_PATH":       "/workspace",
		"DYLD_INSERT_LIBRARIES": "/workspace/evil.dylib",
		"GLIBC_TUNABLES":        "glibc.malloc.check=3",
		"bad-key":               "x",
		"FOO":                   "bar",
	})
	want := []string{"LANG=C.UTF-8", "PYTHONPATH=/workspace/lib", "TZ=UTC"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestSandboxCwd(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "/workspace", false},
		{"/", "/workspace", false},
		{"/src", "/workspace/src", false},
		{"src/nested", "/workspace/src/nested", false},
		{"/src/../lib", "/workspace/lib", false},
		{"../etc", "", true},
		{"/../etc", "", true},
	}
	for _, c := range cases {
		got, err := sandboxCwd(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("sandboxCwd(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("sandboxCwd(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestIsLaunchFailure(t *testing.T) {
	if isLaunchFailure("") {
		t.Error("empty diagnostic must not be a launch failure")
	}
	clean := "[I][t][1] Mode: STANDALONE_ONCE\n[I][t] pid=1 exited with status: 255"
	if isLaunchFailure(clean) {
		t.Error("clean run must not be classified as a launch failure")
	}
	fatal := "[E][t][1] newProc():262 execve failed\n[F][t][1] runChild():561 Launching child process failed"
	if !isLaunchFailure(fatal) {
		t.Error("fatal diagnostic must be classified as a launch failure")
	}
}

func TestClassifyLaunchFailure(t *testing.T) {
	r := &NsjailRunner{}
	var exitErr *exec.ExitError
	// Simulate nsjail exit 255 with a fatal diagnostic.
	cmd := exec.Command("sh", "-c", "exit 255")
	err := cmd.Run()
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exit error, got %v", err)
	}
	res := Result{}
	if rerr := r.classify(&res, err, context.Background(), "[F][t][1] runChild():561 Launching child process failed"); !errors.Is(rerr, ErrSandboxUnavailable) {
		t.Fatalf("classify err = %v, want ErrSandboxUnavailable", rerr)
	}
	// A child cleanly exiting 255 must not be a sandbox error.
	res2 := Result{}
	if rerr := r.classify(&res2, err, context.Background(), ""); rerr != nil {
		t.Fatalf("clean 255 classified as error: %v", rerr)
	}
	if res2.ExitCode != 255 {
		t.Fatalf("exit code = %d, want 255", res2.ExitCode)
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

// TestRenderConfigMountsResolverWhenNetAllowed is a regression guard for a real
// breakage found by exercising the running service: an allow_net workspace that
// stays in the host network namespace (clone_newnet:false) had TCP to a raw IP
// work, but name resolution failed outright with "Temporary failure in name
// resolution" because no /etc/resolv.conf was mounted into the jail. That broke
// `pip install` and any hostname-using workload.
//
// The model changed with isolated networking: allow_net no longer shares the
// host namespace (it never does now). Instead a workspace with allow_net joins
// its OWN namespace and gets a generated resolver naming the engine's userspace
// egress proxy. /etc/hosts is still mounted; the host's /etc/resolv.conf is
// deliberately never mounted into an isolated workspace.
func TestRenderConfigMountsResolverWhenNetAllowed(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{
		CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1, AllowNet: true}}
	path, err := r.renderConfig(Request{
		Workspace:     ws,
		WorkspaceRoot: "/tmp/wsroot",
		Argv:          []string{"/bin/true"},
		NetnsPath:     "/run/netns/sbxdws-abc",
		ResolverConf:  "/data/tmp/egress-resolv-abc.conf",
		HostsConf:     "/data/tmp/egress-hosts-abc",
	}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)

	// A GENERIC /etc/hosts is mounted (never the host's own, which names the
	// operator's server FQDN and LAN address).
	hostsMount := `src: "/data/tmp/egress-hosts-abc"
  dst: "/etc/hosts"
  is_bind: true
  rw: false`
	if !strings.Contains(out, hostsMount) {
		t.Errorf("allow_net must bind-mount the generated generic /etc/hosts read-only:\n%s", out)
	}
	if strings.Contains(out, `src: "/etc/hosts"`) {
		t.Errorf("the host's own /etc/hosts must never be mounted into a workspace:\n%s", out)
	}
	// The generated egress resolver is mounted at /etc/resolv.conf.
	if !strings.Contains(out, `src: "/data/tmp/egress-resolv-abc.conf"`) {
		t.Errorf("allow_net must mount the generated egress resolver at /etc/resolv.conf:\n%s", out)
	}
	// Never the host's own resolver file.
	if strings.Contains(out, `src: "/etc/resolv.conf"`) {
		t.Errorf("the host resolver must never be mounted into an isolated workspace:\n%s", out)
	}
	if !strings.Contains(out, "clone_newnet: false") {
		t.Errorf("a workspace with its own namespace must stay in it (clone_newnet:false):\n%s", out)
	}
}

// A workspace without network must not see the host resolver at all: leaking it
// would publish the host's search domain and nameservers into a jailed workload
// that has no network to use them with.
func TestRenderConfigOmitsResolverWhenNetDenied(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{
		CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1, AllowNet: false}}
	path, err := r.renderConfig(Request{
		Workspace:     ws,
		WorkspaceRoot: "/tmp/wsroot",
		Argv:          []string{"/bin/true"},
	}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)

	if strings.Contains(out, "/etc/resolv.conf") {
		t.Errorf("a workspace without allow_net must not get the host resolver:\n%s", out)
	}
	if !strings.Contains(out, "clone_newnet: true") {
		t.Errorf("allow_net=false must isolate the network namespace:\n%s", out)
	}
}

// TestRenderSessionConfigPipEnvDefaults proves a terminal SESSION carries the
// same pip defaults as a run: a user typing `pip install X` at the PTY gets the
// same no-preparation behaviour, because renderSessionConfig delegates to the
// same renderer.
func TestRenderSessionConfigPipEnvDefaults(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{ID: "ws_ses_pip", Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderSessionConfig(Request{
		Workspace:          ws,
		WorkspaceRoot:      "/tmp/wsroot",
		Cwd:                "/",
		PTY:                true,
		Shell:              "/bin/sh",
		Cols:               80,
		Rows:               24,
		PrivatePackagesDir: "/host/ws/packages",
	}, "/tmp/session-tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)
	for _, want := range []string{
		`envar: "PIP_TARGET=/opt/python/private"`,
		`envar: "PIP_BREAK_SYSTEM_PACKAGES=1"`,
		`envar: "PIP_NO_INPUT=1"`,
		`envar: "PIP_DISABLE_PIP_VERSION_CHECK=1"`,
		`envar: "PIP_ROOT_USER_ACTION=ignore"`,
		`envar: "PYTHONPATH=/opt/python/private"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("terminal session missing %q:\n%s", want, out)
		}
	}
}
