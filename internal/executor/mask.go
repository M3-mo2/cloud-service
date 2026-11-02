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
// (hostmask.c -> hostmask.so) intercepts the syscall-reachable facts that a
// mount namespace cannot hide: uname, sysinfo, sysconf, sched_getaffinity,
// getauxval and the libc version strings. It is reached by naming it in a
// read-only /etc/ld.so.preload, so it applies to every dynamically-linked exec
// regardless of the LD_PRELOAD environment variable (a tenant cannot bypass it
// with `env -i`); the file and the shim are bind-mounted read-only so a tenant
// cannot replace them.
//
// Masking is ON by default (config.Identity.Enabled). When it is on but the
// shim cannot be materialized, the runner reports the masking capability as NOT
// READY (see MaskStatus) so /readyz fails loudly rather than leaking silently.
// The honest limits of the mechanism (static/musl/raw-syscall bypass) are part
// of the design and are documented in docs/DECISIONS.md.

// In-jail paths for the masking files. The shim is mounted under /sandboxd
// (a private mount point nsjail creates in its synthetic root, outside the
// read-only /usr bind); the preload list is mounted over /etc/ld.so.preload.
const (
	maskShimJailPath    = "/sandboxd/hostmask.so"
	maskPreloadJailPath = "/etc/ld.so.preload"
)

// maskEnvKeys are the environment variables the shim reads. They carry the
// WORKSPACE's own limits (cpu count, memory bytes) and generic kernel strings,
// never a host value. They are injected directly by the runner, not through the
// user env allowlist.
const (
	envMaskCPUs          = "SANDBOXD_MASK_CPUS"
	envMaskMemBytes      = "SANDBOXD_MASK_MEM_BYTES"
	envMaskKernelRelease = "SANDBOXD_MASK_KERNEL_RELEASE"
	envMaskKernelVersion = "SANDBOXD_MASK_KERNEL_VERSION"
)

// masker materializes the shim and its ld.so.preload file on the host once, and
// reports whether masking is available. The materialized files live in the
// service tmp dir (never inside a workspace), owned by the orchestrator and
// bind-mounted read-only into each sandbox, so no tenant can write them.
type masker struct {
	cfg config.IdentityConfig
	dir string // host directory holding hostmask.so and ld.so.preload

	once   sync.Once
	ready  bool
	reason string
}

// newMasker constructs a masker rooted at dir (the service tmp dir).
func newMasker(cfg config.IdentityConfig, dir string) *masker {
	return &masker{cfg: cfg, dir: filepath.Join(dir, "identity-mask")}
}

// Enabled reports whether masking is turned on by configuration.
func (m *masker) Enabled() bool { return m.cfg.Enabled }

// shimHostPath returns the host path of the materialized shim.
func (m *masker) shimHostPath() string { return filepath.Join(m.dir, "hostmask.so") }

// preloadHostPath returns the host path of the materialized ld.so.preload list.
func (m *masker) preloadHostPath() string { return filepath.Join(m.dir, "ld.so.preload") }

// prepare materializes the shim and preload file once, verifying the shim is a
// real file. It is idempotent and caches the outcome.
func (m *masker) prepare() {
	m.once.Do(func() {
		if !m.cfg.Enabled {
			m.ready = false
			m.reason = "identity masking disabled by configuration (identity.enabled=false)"
			return
		}
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
	})
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
