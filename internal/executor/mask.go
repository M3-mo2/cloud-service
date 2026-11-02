package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"sandboxd/internal/config"
)

// This file wires host-identity masking into the sandbox. The compiled shim
// (hostmask.c -> rt.so) intercepts the syscall-reachable facts that a mount
// namespace cannot hide: uname, sysinfo, sysconf, sched_getaffinity, getauxval
// and the libc version strings. It is reached by naming it in a read-only
// /etc/ld.so.preload, so it applies to every dynamically-linked exec regardless
// of the LD_PRELOAD environment variable (a tenant cannot bypass it with
// `env -i`); the file and the shim are bind-mounted read-only so a tenant cannot
// replace them.
//
// The same shim ALSO hides the masking mechanism from the tenant: it answers
// ENOENT (and drops the directory entry) for /etc/ld.so.preload, its own in-jail
// path and the engine's legacy directories, so browsing `ls /`, `cat
// /etc/ld.so.preload` or `strings`ing the shim reveals nothing. The hiding is
// scoped to already-exec'd processes, so the dynamic loader still reads the
// preload list before any shim is mapped and a real exec stays masked (asserted
// by scripts/identity-check.sh). See internal/executor/hostmask/hostmask.c.
//
// Masking is ON by default (config.Identity.Enabled). When it is on but the
// shim cannot be materialized, the runner reports the masking capability as NOT
// READY (see MaskStatus) so /readyz fails loudly rather than leaking silently.
// The honest limits of the mechanism (static/musl/raw-syscall bypass) are part
// of the design and are documented in docs/DECISIONS.md.

// In-jail paths for the masking files. The shim is mounted at /run/.rt.so (an
// opaque name nsjail creates in its synthetic root, outside the read-only /usr
// bind); the name deliberately reveals nothing, so `cat /etc/ld.so.preload`
// names no engine and no mechanism. The preload list is mounted over
// /etc/ld.so.preload. The curated /usr/local, /usr/bin, /usr/sbin and
// /etc/ssl/certs overlays are mounted over the host's own directories, so a
// workspace sees only the curated trees.
const (
	maskShimJailPath    = "/run/.rt.so"
	maskPreloadJailPath = "/etc/ld.so.preload"
	toolsJailPath       = "/usr/local"
	binJailPath         = "/usr/bin"
	sbinJailPath        = "/usr/sbin"
	libJailPath         = "/usr/lib"
	libAliasJailPath    = "/lib"
	certsJailPath       = "/etc/ssl/certs"
	passwdJailPath      = "/etc/passwd"
	groupJailPath       = "/etc/group"
	docJailPath         = "/usr/share/doc"
	manJailPath         = "/usr/share/man"
)

// maskEnvKeys are the environment variables the shim reads. They carry the
// WORKSPACE's own limits (cpu count, memory bytes) and generic kernel strings,
// never a host value. They are injected directly by the runner, not through the
// user env allowlist. The names are deliberately opaque: publishing an
// engine-named variable to the workspace would announce both the engine and the
// fact that a mask is active.
const (
	envMaskCPUs          = "LSM_SYS_CPUS"
	envMaskMemBytes      = "LSM_SYS_MEM"
	envMaskKernelRelease = "LSM_SYS_REL"
	envMaskKernelVersion = "LSM_SYS_VER"
)

// masker materializes the shim and its ld.so.preload file on the host once, and
// reports whether masking is available. The materialized files live in the
// service tmp dir (never inside a workspace), owned by the orchestrator and
// bind-mounted read-only into each sandbox, so no tenant can write them.
type masker struct {
	cfg config.IdentityConfig
	dir string // host directory holding rt.so and ld.so.preload

	once   sync.Once
	ready  bool
	reason string

	// Provider-overlay state, materialized by the same prepare() pass.
	overlayReady bool
	overlayNote  string
	overlays     providerOverlays
}

// newMasker constructs a masker rooted at dir (the service tmp dir).
func newMasker(cfg config.IdentityConfig, dir string) *masker {
	return &masker{cfg: cfg, dir: filepath.Join(dir, "identity-mask")}
}

// Enabled reports whether masking is turned on by configuration.
func (m *masker) Enabled() bool { return m.cfg.Enabled }

// shimHostPath returns the host path of the materialized shim. The materialized
// file is given an opaque name so it never appears (in a diagnostic or a mount
// source) as an engine name.
func (m *masker) shimHostPath() string { return filepath.Join(m.dir, "rt.so") }

// preloadHostPath returns the host path of the materialized ld.so.preload list.
func (m *masker) preloadHostPath() string { return filepath.Join(m.dir, "ld.so.preload") }

// prepare materializes the shim and preload file (when the mask is enabled) and
// the curated /usr/local + /etc/ssl/certs overlays (when provider-tool hiding is
// enabled) once, verifying the shim is a real file. It is idempotent and caches
// each outcome independently: the two switches are orthogonal.
func (m *masker) prepare() {
	m.once.Do(func() {
		if m.cfg.Enabled {
			m.prepareShim()
		} else {
			m.reason = "identity masking disabled by configuration (identity.enabled=false)"
		}
		m.prepareProviderOverlays()
	})
}

// prepareShim materializes the shim and the ld.so.preload list.
func (m *masker) prepareShim() {
	shim, err := m.shimSource()
	if err != nil {
		m.reason = "masking shim unavailable: " + err.Error()
		return
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		m.reason = "create masking dir: " + err.Error()
		return
	}
	// The shim is written world-readable but NOT world-writable; only the
	// orchestrator can replace it. It is bind-mounted read-only anyway, so
	// even this is defense in depth.
	if err := os.WriteFile(m.shimHostPath(), shim, 0o644); err != nil {
		m.reason = "write masking shim: " + err.Error()
		return
	}
	// ld.so.preload names the shim at its IN-JAIL path, because the loader
	// resolves it after the mounts are in place.
	if err := os.WriteFile(m.preloadHostPath(), []byte(maskShimJailPath+"\n"), 0o644); err != nil {
		m.reason = "write ld.so.preload: " + err.Error()
		return
	}
	m.ready = true
}

// prepareProviderOverlays materializes the curated /usr/local and /etc/ssl/certs
// trees. A failure to build /usr/local is a hard overlay-not-ready (the masked
// mount would otherwise expose the host directory); a failure to read the host
// trust store only disables the TLS overlay and is recorded, because the
// workspace then gets no /etc/ssl/certs mount at all (a closed outcome).
func (m *masker) prepareProviderOverlays() {
	if !m.cfg.HideProviderTools {
		m.overlayNote = "provider-tool hiding disabled by configuration (identity.hide_provider_tools=false)"
		return
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		m.overlayNote = "create provider-overlay dir: " + err.Error()
		return
	}
	overlays, err := materializeProviderOverlays(m.dir, m.cfg)
	// The curated /usr/local is the hard part: without it the mount would
	// expose the host directory, so its failure makes the overlay not ready.
	if err != nil {
		m.overlayNote = err.Error()
		return
	}
	m.overlays = overlays
	m.overlayReady = true
}

// shimSource returns the shim bytes: an operator-provided path when configured,
// else the copy embedded in the binary.
func (m *masker) shimSource() ([]byte, error) {
	if m.cfg.ShimPath != "" {
		b, err := os.ReadFile(m.cfg.ShimPath)
		if err != nil {
			return nil, err
		}
		if len(b) == 0 {
			return nil, fmt.Errorf("shim at %s is empty", m.cfg.ShimPath)
		}
		return b, nil
	}
	if len(HostMaskShim) == 0 {
		return nil, fmt.Errorf("embedded masking shim is empty (hostmask.so was not built)")
	}
	return HostMaskShim, nil
}

// Ready reports whether masking is active for new sandboxes. When Enabled is
// true and Ready is false, a run must be refused rather than leak: see
// StatusReason.
func (m *masker) Ready() bool {
	m.prepare()
	return m.ready
}

// Reason explains a not-ready masking capability.
func (m *masker) Reason() string {
	m.prepare()
	return m.reason
}

// Status returns the truthful masking capability for /readyz and /v1/meta.
func (m *masker) Status() (enabled, ready bool, reason string) {
	return m.cfg.Enabled, m.Ready(), m.Reason()
}

// OverlayStatus returns the provider-tool overlay capability. ready is true when
// the curated /usr/local (and, when readable, /etc/ssl/certs) trees are
// materialized; note explains the state (including that the switch is off, which
// is reported for /v1/meta's honest limits but is not a readiness failure).
func (m *masker) OverlayStatus() (enabled, ready bool, note string) {
	m.prepare()
	return m.cfg.HideProviderTools, m.overlayReady, m.overlayNote
}

// ToolsDir returns the host path of the curated /usr/local overlay (empty when
// not materialized).
func (m *masker) ToolsDir() string {
	m.prepare()
	return m.overlays.ToolsDir
}

// CertsDir returns the host path of the curated /etc/ssl/certs overlay (empty
// when not materialized).
func (m *masker) CertsDir() string {
	m.prepare()
	return m.overlays.CertsDir
}

// BinDir returns the host path of the curated /usr/bin overlay (empty when not
// materialized).
func (m *masker) BinDir() string {
	m.prepare()
	return m.overlays.BinDir
}

// SbinDir returns the host path of the curated /usr/sbin overlay (empty when
// not materialized).
func (m *masker) SbinDir() string {
	m.prepare()
	return m.overlays.SbinDir
}

// LibDir / DocDir / ManDir return the host paths of the curated
// /usr/lib, /usr/share/doc and /usr/share/man overlays.
func (m *masker) LibDir() string { m.prepare(); return m.overlays.LibDir }
func (m *masker) DocDir() string { m.prepare(); return m.overlays.DocDir }
func (m *masker) ManDir() string { m.prepare(); return m.overlays.ManDir }

// PasswdHost / GroupHost return the host paths of the filtered /etc/passwd and
// /etc/group overlays (empty when not materialized).
func (m *masker) PasswdHost() string {
	m.prepare()
	return m.overlays.Passwd
}
func (m *masker) GroupHost() string {
	m.prepare()
	return m.overlays.Group
}

// maskParams are the per-request masking values derived from a workspace.
type maskParams struct {
	Enabled       bool
	CPUs          int
	MemBytes      int64
	KernelRelease string
	KernelVersion string

	ShimHost    string
	PreloadHost string
	ShimJail    string
	PreloadJail string

	// Provider/engine overlays. OverlayEnabled mounts the curated /usr/local,
	// /usr/bin, /usr/sbin, filtered /etc/passwd and /etc/group and (when
	// present) /etc/ssl/certs over the host's own, so a workspace sees no
	// platform or engine material. The *Dir/*Host fields are host paths, empty
	// when the overlay is unavailable or the switch is off.
	OverlayEnabled bool
	ToolsDir       string
	CertsDir       string
	BinDir         string
	SbinDir        string
	LibDir         string
	DocDir         string
	ManDir         string
	PasswdHost     string
	GroupHost      string
	ToolsJail      string
	CertsJail      string
	BinJail        string
	SbinJail       string
	LibJail        string
	LibAliasJail   string
	DocJail        string
	ManJail        string
	PasswdJail     string
	GroupJail      string
}

// params builds the per-request masking parameters for a workspace's limits.
// When masking is off (or not ready) Enabled is false and no mounts/env are
// rendered.
func (m *masker) params(cpus float64, memMB int) maskParams {
	p := maskParams{
		KernelRelease: m.cfg.KernelRelease,
		KernelVersion: m.cfg.KernelVersion,
		ShimJail:      maskShimJailPath,
		PreloadJail:   maskPreloadJailPath,
		ToolsJail:     toolsJailPath,
		CertsJail:     certsJailPath,
		BinJail:       binJailPath,
		SbinJail:      sbinJailPath,
		LibJail:       libJailPath,
		LibAliasJail:  libAliasJailPath,
		DocJail:       docJailPath,
		ManJail:       manJailPath,
		PasswdJail:    passwdJailPath,
		GroupJail:     groupJailPath,
	}
	// Provider-tool overlays are independent of the syscall mask: they apply
	// whenever hiding is enabled and the trees materialized.
	if _, overlayReady, _ := m.OverlayStatus(); overlayReady {
		p.OverlayEnabled = true
		p.ToolsDir = m.ToolsDir()
		p.CertsDir = m.CertsDir()
		p.BinDir = m.BinDir()
		p.SbinDir = m.SbinDir()
		p.LibDir = m.LibDir()
		p.DocDir = m.DocDir()
		p.ManDir = m.ManDir()
		p.PasswdHost = m.PasswdHost()
		p.GroupHost = m.GroupHost()
	}
	if !m.Ready() {
		return p
	}
	p.Enabled = true
	// A workspace is always given at least one cpu and one MB, so a zeroed
	// limit never renders a nonsense "0 cpus".
	p.CPUs = int(cpus + 0.5)
	if p.CPUs < 1 {
		p.CPUs = 1
	}
	if memMB < 1 {
		memMB = 1
	}
	p.MemBytes = int64(memMB) * 1024 * 1024
	if p.KernelRelease == "" {
		p.KernelRelease = config.DefaultMaskKernelRelease
	}
	if p.KernelVersion == "" {
		p.KernelVersion = config.DefaultMaskKernelVersion
	}
	p.ShimHost = m.shimHostPath()
	p.PreloadHost = m.preloadHostPath()
	return p
}

// envEntries renders the masking environment variables as `KEY=VALUE` pairs.
func (p maskParams) envEntries() []string {
	if !p.Enabled {
		return nil
	}
	return []string{
		envMaskCPUs + "=" + strconv.Itoa(p.CPUs),
		envMaskMemBytes + "=" + strconv.FormatInt(p.MemBytes, 10),
		envMaskKernelRelease + "=" + p.KernelRelease,
		envMaskKernelVersion + "=" + p.KernelVersion,
	}
}
