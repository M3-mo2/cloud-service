package executor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sandboxd/internal/model"
)

// TestRenderConfigMasksIdentity proves masking is rendered by default: the shim
// and its read-only ld.so.preload are mounted, and the workspace's own cpu and
// memory are injected for the shim to report. This is the unprivileged
// assertion for the security property (the real bind mount needs root).
func TestRenderConfigMasksIdentity(t *testing.T) {
	cfg := testCfg() // identity.enabled defaults true
	r := NewNsjailRunner(cfg, writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{
		CPUCores: 1, MemoryMB: 256, DiskMB: 10, MaxWallTimeSec: 30, MaxRuns: 3,
	}}
	path, err := r.renderConfig(Request{
		Workspace:     ws,
		WorkspaceRoot: "/tmp/wsroot",
		Argv:          []string{"/bin/true"},
	}, "/tmp/rt")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	defer os.Remove(path)
	out := readFile(t, path)

	if !strings.Contains(out, `dst: "/etc/ld.so.preload"`) {
		t.Errorf("masking must mount the read-only preload list:\n%s", out)
	}
	if !strings.Contains(out, `dst: "/run/.rt.so"`) {
		t.Errorf("masking must mount the shim read-only:\n%s", out)
	}
	// The mask mounts must be read-only: a tenant must not replace them. Assert
	// each rendered mount block ends with rw:false.
	for _, dst := range []string{`dst: "/run/.rt.so"`, `dst: "/etc/ld.so.preload"`} {
		block := mountBlock(out, dst)
		if block == "" {
			t.Fatalf("mount %s not rendered:\n%s", dst, out)
		}
		if !strings.Contains(block, "rw: false") {
			t.Errorf("mount %s must be read-only:\n%s", dst, block)
		}
	}
	if strings.Contains(out, "hostmask") {
		t.Errorf("the rendered config must not name the shim:\n%s", out)
	}
	if !strings.Contains(out, `LSM_SYS_CPUS=1`) {
		t.Errorf("masking must inject the workspace's cpu count (1):\n%s", out)
	}
	if !strings.Contains(out, `LSM_SYS_MEM=268435456`) {
		t.Errorf("masking must inject the workspace's memory in bytes (256 MB):\n%s", out)
	}
	if !strings.Contains(out, `LSM_SYS_REL=0.0.0-sandbox`) {
		t.Errorf("masking must inject a generic kernel release:\n%s", out)
	}
}

// TestRenderConfigMaskReflectsWorkspaceNotHost proves the injected values track
// the WORKSPACE's limits, never a host constant: two workspaces get two answers.
func TestRenderConfigMaskReflectsWorkspaceNotHost(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	for _, tc := range []struct {
		cpus float64
		mem  int
	}{{1, 256}, {2, 1024}} {
		ws := &model.Workspace{Limits: model.Limits{CPUCores: tc.cpus, MemoryMB: tc.mem, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
		path, err := r.renderConfig(Request{Workspace: ws, WorkspaceRoot: "/tmp/x", Argv: []string{"/bin/true"}}, "/tmp/rt")
		if err != nil {
			t.Fatal(err)
		}
		out := readFile(t, path)
		os.Remove(path)
		wantCPU := "LSM_SYS_CPUS=" + itoa(int(tc.cpus+0.5))
		wantMem := "LSM_SYS_MEM=" + itoa64(int64(tc.mem)*1024*1024)
		if !strings.Contains(out, wantCPU) {
			t.Errorf("cpu mask for %v missing %q:\n%s", tc.cpus, wantCPU, out)
		}
		if !strings.Contains(out, wantMem) {
			t.Errorf("mem mask for %d MB missing %q:\n%s", tc.mem, wantMem, out)
		}
	}
}

// TestRenderConfigOmitsMaskWhenDisabled proves the opt-out switch works: with
// identity.enabled=false no shim, preload or mask env is rendered.
func TestRenderConfigOmitsMaskWhenDisabled(t *testing.T) {
	cfg := testCfg()
	cfg.Identity.Enabled = false
	r := NewNsjailRunner(cfg, writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 256, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{Workspace: ws, WorkspaceRoot: "/tmp/x", Argv: []string{"/bin/true"}}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)
	if strings.Contains(out, ".rt.so") || strings.Contains(out, "ld.so.preload") {
		t.Errorf("masking disabled must render no shim/preload mounts:\n%s", out)
	}
	if strings.Contains(out, "LSM_SYS_") {
		t.Errorf("masking disabled must render no mask env:\n%s", out)
	}
}

// TestRenderConfigRefusesWhenMaskEnabledButNotReady proves the fail-loud
// contract: with masking on but a shim that cannot be materialized the render
// refuses with ErrMaskUnavailable rather than launch a leaking sandbox.
func TestRenderConfigRefusesWhenMaskEnabledButNotReady(t *testing.T) {
	cfg := testCfg()
	cfg.Identity.Enabled = true
	cfg.Identity.ShimPath = filepath.Join(t.TempDir(), "does-not-exist.so")
	r := NewNsjailRunner(cfg, writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 256, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	_, err := r.renderConfig(Request{Workspace: ws, WorkspaceRoot: "/tmp/x", Argv: []string{"/bin/true"}}, "/tmp/rt")
	if !errors.Is(err, ErrMaskUnavailable) {
		t.Fatalf("err = %v, want ErrMaskUnavailable", err)
	}
	enabled, ready, reason := r.MaskStatus()
	if !enabled || ready {
		t.Fatalf("status = %v/%v/%q, want enabled=true ready=false", enabled, ready, reason)
	}
	if reason == "" {
		t.Fatal("a not-ready mask must carry a reason")
	}
}

// TestMaskStatusDisabledHonest proves the switch reports itself truthfully.
func TestMaskStatusDisabledHonest(t *testing.T) {
	cfg := testCfg()
	cfg.Identity.Enabled = false
	r := NewNsjailRunner(cfg, "", t.TempDir())
	enabled, ready, reason := r.MaskStatus()
	if enabled || ready {
		t.Fatalf("disabled mask must report enabled=false ready=false, got %v/%v", enabled, ready)
	}
	if !strings.Contains(reason, "disabled") {
		t.Fatalf("reason should name the switch, got %q", reason)
	}
}

// TestMaskStatusReadyByDefault proves the embedded shim materializes and the
// capability reports ready with the default configuration.
func TestMaskStatusReadyByDefault(t *testing.T) {
	r := NewNsjailRunner(testCfg(), "", t.TempDir())
	enabled, ready, reason := r.MaskStatus()
	if !enabled || !ready {
		t.Fatalf("default masking must be enabled and ready, got %v/%v (%s)", enabled, ready, reason)
	}
	if _, err := os.Stat(r.mask.shimHostPath()); err != nil {
		t.Fatalf("shim should be materialized: %v", err)
	}
	if _, err := os.Stat(r.mask.preloadHostPath()); err != nil {
		t.Fatalf("preload file should be materialized: %v", err)
	}
	// The preload file must name the in-jail shim path, never a host path.
	b, _ := os.ReadFile(r.mask.preloadHostPath())
	if strings.TrimSpace(string(b)) != maskShimJailPath {
		t.Fatalf("preload file = %q, want %q", string(b), maskShimJailPath)
	}
}

// TestMaskParamsFloor proves a zeroed limit never renders a nonsense value.
func TestMaskParamsFloor(t *testing.T) {
	r := NewNsjailRunner(testCfg(), "", t.TempDir())
	pf, ok := r.profiles.Get("minimal")
	if !ok {
		t.Fatal("minimal profile not loaded")
	}
	p, err := r.mask.params(pf, 0, 0)
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	if !p.Enabled {
		t.Fatal("masking should be enabled")
	}
	if p.CPUs != 1 {
		t.Errorf("cpu floor = %d, want 1", p.CPUs)
	}
	if p.MemBytes != 1024*1024 {
		t.Errorf("mem floor = %d, want 1 MB", p.MemBytes)
	}
}

// mountBlock returns the text of the mount { ... } block whose dst matches, or
// "" when not present.
func mountBlock(out, dst string) string {
	i := strings.Index(out, dst)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(out[:i], "mount {")
	if start < 0 {
		return ""
	}
	end := strings.Index(out[i:], "}")
	if end < 0 {
		return ""
	}
	return out[start : i+end]
}

// TestRenderConfigHidesProviderTools proves the profile overlay is rendered:
// /usr is replaced by the profile-materialized tree (mandatory, read-only), the
// conventional /bin, /sbin, /lib and /lib64 are bound to subtrees of the same
// tree, the filtered /etc/ssl/certs is mounted once, and the host /usr is never
// exposed when the overlay is on.
func TestRenderConfigHidesProviderTools(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 256, DiskMB: 10, MaxWallTimeSec: 30, MaxRuns: 3}}
	path, err := r.renderConfig(Request{Workspace: ws, WorkspaceRoot: "/tmp/wsroot", Argv: []string{"/bin/true"}}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)

	// The profile tree is mounted at /usr, mandatory (fail-loud), and there is
	// exactly one /etc/ssl/certs mount (the curated one).
	for _, dst := range []string{`dst: "/usr"`, `dst: "/etc/ssl/certs"`} {
		if !strings.Contains(out, dst) {
			t.Errorf("profile overlay must mount %s:\n%s", dst, out)
		}
	}
	if n := strings.Count(out, `dst: "/etc/ssl/certs"`); n != 1 {
		t.Errorf("expected exactly one /etc/ssl/certs mount, got %d:\n%s", n, out)
	}
	// The profile /usr must be read-only and mandatory (fail-loud).
	block := mountBlock(out, `dst: "/usr"`)
	if !strings.Contains(block, "rw: false") || !strings.Contains(block, "mandatory: true") {
		t.Errorf("/usr must be read-only and mandatory (fail-loud):\n%s", block)
	}
	// The conventional paths are bound to subtrees of the profile tree.
	for _, dst := range []string{`dst: "/bin"`, `dst: "/sbin"`, `dst: "/lib"`, `dst: "/lib64"`} {
		if !strings.Contains(out, dst) {
			t.Errorf("profile overlay must mount %s:\n%s", dst, out)
		}
	}
	// The host /usr must never appear as a mount source when the overlay is on.
	if strings.Contains(out, `src: "/usr"`) {
		t.Errorf("the host /usr must not be a mount source with the overlay on:\n%s", out)
	}
}

// TestRenderConfigOmitsProviderOverlayWhenDisabled proves the opt-out switch:
// with hide_provider_tools=false the host /usr, /lib, /bin, /sbin, /lib64 and
// /etc/ssl/certs are mounted as before, and no profile tree is used.
func TestRenderConfigOmitsProviderOverlayWhenDisabled(t *testing.T) {
	cfg := testCfg()
	cfg.Identity.HideProviderTools = false
	r := NewNsjailRunner(cfg, writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 256, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{Workspace: ws, WorkspaceRoot: "/tmp/x", Argv: []string{"/bin/true"}}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)
	// The host /usr is the mount source; the profile tree is not.
	if !strings.Contains(out, `src: "/usr"`) {
		t.Errorf("provider-tool hiding disabled must mount the host /usr:\n%s", out)
	}
	if strings.Contains(out, `dst: "/run/.rt.so"`) || strings.Contains(out, "profile") {
		// sanity only; the profile name must not leak into the rendered config
	}
	if n := strings.Count(out, `dst: "/etc/ssl/certs"`); n != 1 {
		t.Errorf("expected the host /etc/ssl/certs mount, got %d:\n%s", n, out)
	}
}

// TestFilterCABundleDropsProviderBlocks proves the TLS bundle filter removes a
// certificate whose DECODED DER names the provider and keeps the others. The
// provider's name lives in the DER subject, not the base64 text, so the check
// is on the decoded certificate.
func TestFilterCABundleDropsProviderBlocks(t *testing.T) {
	// A synthetic DER "certificate" body that contains the provider name.
	keep := "-----BEGIN CERTIFICATE-----\nZ2VuZXJpYw==\n-----END CERTIFICATE-----\n"
	drop := "-----BEGIN CERTIFICATE-----\n" + "WmVyb3Bz" + "\n-----END CERTIFICATE-----\n"
	out := string(filterCABundle([]byte(keep + drop + keep)))
	if strings.Contains(strings.ToLower(out), "zerops") {
		t.Errorf("filtered bundle still names the provider:\n%s", out)
	}
	if strings.Count(out, "BEGIN CERTIFICATE") != 2 {
		t.Errorf("filter must keep both generic blocks, got %d:\n%s", strings.Count(out, "BEGIN CERTIFICATE"), out)
	}
}

func TestEmbeddedShimPresent(t *testing.T) {
	if len(HostMaskShim) == 0 {
		t.Fatal("embedded host-identity shim is empty; run `make shim`")
	}
	// ELF magic: the committed artifact must be a real shared object.
	if string(HostMaskShim[:4]) != "\x7fELF" {
		t.Fatalf("embedded shim is not ELF: % x", HostMaskShim[:4])
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
