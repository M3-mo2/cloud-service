package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"sandboxd/configs"
	"sandboxd/internal/config"
	"sandboxd/internal/decoy"
	"sandboxd/internal/model"
	"sandboxd/internal/ports"
	"sandboxd/internal/profile"
)

// NsjailRunner executes commands under nsjail.
type NsjailRunner struct {
	cfg      config.Config
	tmplPath string
	tmpDir   string
	identity Identity
	mask     *masker

	// profiles is the loaded profile registry. A load failure is stored so
	// readiness reports it loudly (a workspace must never run against a
	// half-loaded allowlist).
	profiles   *profile.Registry
	profileErr error

	mu         sync.Mutex
	procs      map[string]*os.Process // runID -> direct child (nsjail) process
	cgroupDirs map[string]string      // runID -> NSJAIL cgroup directory
	avail      bool
	availMu    sync.Once

	cgroupOnce sync.Once
	cgUsable   bool
}

// NewNsjailRunner constructs a runner. tmplPath points at the nsjail config
// template; tmpDir is where per-run configs are written.
//
// The profile registry is built here from the embedded built-in profiles plus
// the operator directory in cfg.Profiles.Dir. A load failure is stored (not
// panicked) and surfaced via ProfileStatus so /readyz fails loudly.
func NewNsjailRunner(cfg config.Config, tmplPath, tmpDir string) *NsjailRunner {
	reg, err := profile.NewRegistry(configs.BuiltinProfiles(), cfg.Profiles.Dir)
	m := newMasker(cfg.Identity, tmpDir)
	r := &NsjailRunner{
		cfg:        cfg,
		tmplPath:   tmplPath,
		tmpDir:     tmpDir,
		identity:   ResolveIdentity(cfg),
		mask:       m,
		profiles:   reg,
		profileErr: err,
		procs:      make(map[string]*os.Process),
		cgroupDirs: make(map[string]string),
	}
	// Build every offered profile tree now, once. The trees are large and shared
	// (per-profile, not per-workspace), so paying the cost at construction keeps
	// the first run fast and readiness honest. A failure is retained (not fatal
	// here) and reported by ProfileStatus / /readyz.
	_ = r.WarmProfiles()
	return r
}

// WarmProfiles materializes the DEFAULT profile's tree now, once, so the first
// run is not slowed by a ~20 s materialization and /readyz reports the tree
// state honestly. Other offered profiles materialize lazily on first use (they
// share the same per-profile cache). A failure is returned; startup logs it, and
// ProfileStatus plus /readyz then report the profiles not ready.
func (r *NsjailRunner) WarmProfiles() error {
	if r.profileErr != nil {
		return r.profileErr
	}
	if dp, ok := r.profiles.Get(r.cfg.Profiles.Default); ok {
		if _, err := r.mask.profileTree(dp); err != nil {
			return err
		}
	}
	return nil
}

// Profiles returns the loaded profile registry (nil when it failed to load).
func (r *NsjailRunner) Profiles() *profile.Registry { return r.profiles }

// SetDecoyCA attaches the egress-decoy CA to the trust-store overlay so the
// stub TLS responder is trusted inside every workspace. It must be called
// before the first run (the overlay is materialized once).
func (r *NsjailRunner) SetDecoyCA(ca *decoy.CA) { r.mask.SetDecoyCA(ca) }

// ProfileStatus reports the profile registry capability truthfully. ready is
// false when the configured profiles could not be loaded or the configured
// default profile is not present; note explains why.
func (r *NsjailRunner) ProfileStatus() (ready bool, note string) {
	if r.profileErr != nil {
		return false, "profiles failed to load: " + r.profileErr.Error()
	}
	if r.profiles == nil {
		return false, "no profile registry"
	}
	if !r.profiles.Has(r.cfg.Profiles.Default) {
		return false, fmt.Sprintf("default profile %q is not loaded", r.cfg.Profiles.Default)
	}
	// Readiness also requires the default profile's tree to be materialized, so
	// a run cannot be slowed (or fail) on a tree that was never built.
	if dp, ok := r.profiles.Get(r.cfg.Profiles.Default); ok {
		if _, err := r.mask.profileTree(dp); err != nil {
			return false, "profile materialization failed: " + err.Error()
		}
	}
	return true, ""
}

// profileFor resolves the profile a request runs under: the workspace's stored
// profile, or the configured default when the record names none (an old record
// or a create that omitted it). An unknown name is an error, never a silent
// fallback to a different profile.
func (r *NsjailRunner) profileFor(ws *model.Workspace) (*profile.Profile, error) {
	name := r.cfg.Profiles.Default
	if ws != nil && ws.Profile != "" {
		name = ws.Profile
	}
	if r.profileErr != nil {
		return nil, fmt.Errorf("profiles unavailable: %w", r.profileErr)
	}
	p, ok := r.profiles.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", name)
	}
	return p, nil
}

// MaskStatus reports host-identity masking truthfully.
func (r *NsjailRunner) MaskStatus() (bool, bool, string) { return r.mask.Status() }

// OverlayStatus reports the provider-tool overlay capability truthfully:
// enabled is the configured switch, ready whether the curated trees are
// materialized, and note explains the state.
func (r *NsjailRunner) OverlayStatus() (bool, bool, string) { return r.mask.OverlayStatus() }

// Identity returns the resolved sandbox identity mapping.
func (r *NsjailRunner) Identity() Identity { return r.identity }

// CgroupMount is the location nsjail uses for the cgroup v2 hierarchy.
const CgroupMount = "/sys/fs/cgroup"

// The physical memory ceiling of a workspace is the parent cgroup's memory.max
// = memory_mb (bytes): a process that COMMITS past it is OOM-killed there. The
// per-run `cgroup_mem_max` is kept EQUAL to the workspace's ceiling rather than
// set to unlimited, because nsjail creates its leaf cgroup only when at least
// one cgroup limit is set: with cgroup_mem_max=0 AND cgroup_pids_max=0 AND
// cgroup_cpu_ms_per_sec=0, measured, nsjail creates NO leaf cgroup at all, which
// would cost the cgroup-scoped kill/inventory handle. The parent still enforces
// the SUM across concurrent runs, so one run may use up to memory_mb and N runs
// share the same memory_mb (they no longer each get their own).
//
// rlimit_as is a separate, coarser virtual-address-space backstop and is
// deliberately NOT set to memory_mb, because a modern managed runtime reserves a
// large VIRTUAL address space at startup that it never commits, and RLIMIT_AS
// counts every reservation including PROT_NONE ones. Measured: the pinned
// node/V8 build (v24.22.2) mmaps a 512 MiB PROT_NONE CodeRange plus heap
// reservations -- ~1.1 GiB of address space -- before it runs a single line, so
// rlimit_as = memory_mb aborts it with "Fatal process out of memory:
// SegmentedTable::InitializeTable (subspace allocation)" / "Failed to reserve
// virtual memory for CodeRange". This is a property of rlimit_as vs V8, NOT of
// the host-identity shim: `node -e 'console.log(42)'` fails identically with
// masking on and off, and under a bare `ulimit -v 262144` with no shim loaded.
// The address-space limit is therefore memory_mb plus a fixed runtime headroom,
// with a floor so the smallest workspace still has room to start a managed
// runtime. The cgroup remains the ceiling that actually bounds physical use.
const (
	runtimeAddressSpaceHeadroomMB = 1024
	minAddressSpaceMB             = 2048
)

// addressSpaceMB returns the rlimit_as value (MB) for a workspace's memory_mb.
// It is the physical ceiling plus a runtime address-space headroom, floored so
// a small workspace can still start node/V8. See the block above for why this
// is not simply memory_mb.
func addressSpaceMB(memoryMB int) int {
	if memoryMB < 1 {
		memoryMB = 1
	}
	as := memoryMB + runtimeAddressSpaceHeadroomMB
	if as < minAddressSpaceMB {
		as = minAddressSpaceMB
	}
	return as
}

// CgroupsUsable reports whether the cgroup v2 hierarchy is writable by this
// process. nsjail's cgroup ceilings (memory, pids, cpu) require it, which in
// turn requires the container to be started with --cgroupns=host. The result is
// probed once and cached.
func (r *NsjailRunner) CgroupsUsable() bool {
	r.cgroupOnce.Do(func() {
		dir, err := os.MkdirTemp(CgroupMount, "sandboxd-probe-*")
		if err != nil {
			r.cgUsable = false
			return
		}
		_ = os.Remove(dir)
		r.cgUsable = true
	})
	return r.cgUsable
}

// Available reports whether the nsjail binary exists and is executable.
func (r *NsjailRunner) Available() bool {
	r.availMu.Do(func() {
		path, err := exec.LookPath(r.cfg.Sandbox.NsjailPath)
		if err != nil {
			// If an absolute path was configured, check it directly.
			if fi, statErr := os.Stat(r.cfg.Sandbox.NsjailPath); statErr == nil && !fi.IsDir() {
				r.avail = true
				return
			}
			r.avail = false
			return
		}
		r.avail = path != ""
	})
	return r.avail
}

// resolveProfileCommand resolves a request's argv[0]. A path (relative or
// absolute) is passed through unchanged; the profile mount is what makes an
// absent path fail. A bare command name must be in the workspace profile's
// allowlist (when provider-tool hiding is on) or it is a clean
// command-not-found, so a tool outside the profile fails without an nsjail
// execve diagnostic.
func (r *NsjailRunner) resolveProfileCommand(pf *profile.Profile, name string) (string, error) {
	if name == "" {
		return "", ErrCommandNotFound
	}
	if strings.ContainsRune(name, '/') {
		if filepath.IsAbs(name) {
			return name, nil
		}
		return filepath.Abs(name)
	}
	if _, overlayReady, _ := r.mask.OverlayStatus(); overlayReady {
		tree, err := r.mask.profileTree(pf)
		if err != nil {
			return "", err
		}
		if !tree.Commands[name] {
			return "", ErrCommandNotFound
		}
		// The command exists in the materialized tree at /usr/bin/<name>, which
		// is what the jail sees. An alias such as `python` (-> python3) has no
		// host executable, so resolving on the host PATH would fail; the jail
		// path is the correct, profile-scoped answer.
		return binJailPath + "/" + name, nil
	}
	return resolveCommand(name)
}

// sortExtraMounts orders the profile's extra mounts by jail path so the
// rendered config is deterministic.
func sortExtraMounts(e []extraMount) {
	sort.Slice(e, func(i, j int) bool { return e[i].Jail < e[j].Jail })
}

func (r *NsjailRunner) resolveBinary() (string, error) {
	if p, err := exec.LookPath(r.cfg.Sandbox.NsjailPath); err == nil {
		return p, nil
	}
	if fi, err := os.Stat(r.cfg.Sandbox.NsjailPath); err == nil && !fi.IsDir() {
		return r.cfg.Sandbox.NsjailPath, nil
	}
	return "", ErrNsjailUnavailable
}

// Run renders an nsjail config and executes the command to completion.
func (r *NsjailRunner) Run(req Request) (Result, error) {
	if !r.Available() {
		return Result{}, ErrNsjailUnavailable
	}
	bin, err := r.resolveBinary()
	if err != nil {
		return Result{}, err
	}

	// nsjail's exec_bin.path is passed straight to execve and is never looked
	// up on PATH, so resolve argv[0] to an absolute path first. The sandbox
	// bind-mounts the profile tree at /usr (and /bin, /lib, ...), so a
	// host-resolved absolute path is valid inside the jail. A bare command name
	// is additionally checked against the workspace profile's allowlist, so a
	// command outside the profile fails with a clean command-not-found rather
	// than an nsjail execve error.
	if len(req.Argv) > 0 {
		pf, perr := r.profileFor(req.Workspace)
		if perr != nil {
			return Result{}, perr
		}
		resolved, rerr := r.resolveProfileCommand(pf, req.Argv[0])
		if rerr != nil {
			return Result{}, rerr
		}
		req.Argv = append([]string{resolved}, req.Argv[1:]...)
	}

	// Create a private, per-run temporary directory for the sandbox's /tmp.
	runTmp, err := os.MkdirTemp(r.tmpDir, "run-*")
	if err != nil {
		return Result{}, fmt.Errorf("create run tmp: %w", err)
	}
	defer os.RemoveAll(runTmp)
	// The sandbox process needs to write here; make it world-writable with the
	// sticky bit (standard /tmp semantics) and, when privileged, hand it to the
	// sandbox identity.
	if err := os.Chmod(runTmp, 0o1777); err != nil {
		return Result{}, err
	}
	if err := r.identity.ChownToSandbox(runTmp); err != nil {
		return Result{}, fmt.Errorf("prepare run tmp ownership: %w", err)
	}
	// A privileged orchestrator mounts these paths as the sandbox uid, so every
	// ancestor directory must be traversable (o+x) by that uid.
	if r.identity.Privileged {
		if err := ensureTraversable(runTmp); err != nil {
			return Result{}, err
		}
		if req.WorkspaceRoot != "" {
			if err := ensureTraversable(req.WorkspaceRoot); err != nil {
				return Result{}, err
			}
		}
	}

	cfgPath, err := r.renderConfig(req, runTmp)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(cfgPath)

	// When the orchestrator is privileged it creates workspace files as root.
	// The jailed process runs as the sandbox uid, so hand ownership of the
	// workspace tree to the sandbox identity. Unprivileged orchestrators need
	// no chown: the kernel maps their files to the sandbox identity inside the
	// namespace.
	if r.identity.Privileged && req.WorkspaceRoot != "" {
		if err := chownTree(req.WorkspaceRoot, r.identity.SandboxUID, r.identity.SandboxGID); err != nil {
			return Result{}, fmt.Errorf("prepare workspace ownership: %w", err)
		}
	}
	// The private package tier is mounted writable, so (when privileged) it must
	// be owned by the sandbox identity for the workload to install into it. The
	// shared tier is read-only and is deliberately never chowned: it must stay
	// exactly as the operator installed it.
	if r.identity.Privileged && req.PrivatePackagesDir != "" {
		if _, err := os.Stat(req.PrivatePackagesDir); err == nil {
			if err := ensureTraversable(req.PrivatePackagesDir); err != nil {
				return Result{}, err
			}
			if err := chownTree(req.PrivatePackagesDir, r.identity.SandboxUID, r.identity.SandboxGID); err != nil {
				return Result{}, fmt.Errorf("prepare private package ownership: %w", err)
			}
		}
	}

	// Base the run on the caller's context so an explicit stop cancels the
	// process tree, then layer the wall-clock timeout on top. An unbounded run
	// (NoTimeout) layers no timeout: only an explicit stop ends it.
	base := context.Background()
	if req.Context != nil {
		base = req.Context
	}
	ctx := base
	var cancel context.CancelFunc
	if !req.NoTimeout && req.Timeout > 0 {
		ctx, cancel = context.WithTimeout(base, req.Timeout)
		defer cancel()
	}

	cmd := nsjailCommand(ctx, bin, cfgPath, req.NetnsPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Ensure context cancellation (timeout or explicit stop) tears down the whole
	// process group rather than only the direct child.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = r.cfg.Sandbox.KillGrace() + time.Second
	// Always drain stdin so a child that reads never blocks forever.
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	} else {
		cmd.Stdin = strings.NewReader("")
	}

	// Read the workspace parent cgroup's cumulative oom_kill counter before the
	// run so an OOM at the ceiling can be told apart from a clean exit. The
	// parent persists for the workspace's lifetime, so it is readable after too.
	oomBefore := readOOMKill(req.WorkspaceCgroup)

	res, err := r.runCapture(ctx, cmd, req)
	// An OOM at the ceiling is not a clean exit. The kernel's OOM killer sends
	// SIGKILL to the run's process group (memory.oom.group is set on the parent),
	// which nsjail reports as exit 137 -- the same code an ordinary SIGKILL
	// produces. The parent's oom_kill counter is the authoritative signal that it
	// was the CEILING, so the outcome is observable rather than a silent
	// disappearance.
	if err == nil && res.ExitCode == 137 && oomBefore >= 0 {
		if after := readOOMKill(req.WorkspaceCgroup); after > oomBefore {
			res.OOM = true
		}
	}
	return res, err
}

// runCapture wires up stdout/stderr either to capped buffers or to log files.
// nsjail's own diagnostics are routed to a dedicated pipe (its log fd 3) so a
// setup or launch failure is captured rather than silently discarded.
func (r *NsjailRunner) runCapture(ctx context.Context, cmd *exec.Cmd, req Request) (Result, error) {
	diagDone := attachDiagnostics(cmd)

	if req.RunID != "" && req.StdoutPath != "" {
		outF, err := os.OpenFile(req.StdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return Result{}, err
		}
		defer outF.Close()
		errF, err := os.OpenFile(req.StderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return Result{}, err
		}
		defer errF.Close()

		outW := newCappedWriter(outF, req.StdoutCap)
		errW := newCappedWriter(errF, req.StderrCap)
		cmd.Stdout = outW
		cmd.Stderr = errW

		if err := cmd.Start(); err != nil {
			closeWriteEnd(cmd)
			<-diagDone
			return Result{}, err
		}
		closeWriteEnd(cmd)
		if req.RunID != "" {
			r.track(req.RunID, cmd.Process)
			r.trackCgroup(req.RunID, resolveCgroup(cmd.Process.Pid))
			defer r.untrack(req.RunID)
			// On completion, reap the run's cgroup: nsjail normally removes it on
			// a clean exit, but a workload that left a detached descendant (or
			// that escaped its process group with setsid()) would otherwise keep
			// the cgroup populated. Killing the cgroup tree and removing the
			// directory is what makes "a run leaves nothing behind" true.
			defer r.reapTrackedCgroup(req.RunID)
		}
		waitErr := cmd.Wait()
		diagnostic := waitDiagnostic(diagDone)
		res := Result{Streamed: true, Truncated: outW.truncated || errW.truncated}
		if err := r.classify(&res, waitErr, ctx, diagnostic); err != nil {
			return res, err
		}
		return res, nil
	}

	var stdout, stderr bytes.Buffer
	outW := newCappedWriter(&stdout, req.StdoutCap)
	errW := newCappedWriter(&stderr, req.StderrCap)
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		closeWriteEnd(cmd)
		<-diagDone
		return Result{}, err
	}
	closeWriteEnd(cmd)
	waitErr := cmd.Wait()
	diagnostic := waitDiagnostic(diagDone)
	res := Result{
		Stdout:    stdout.Bytes(),
		Stderr:    stderr.Bytes(),
		Truncated: outW.truncated || errW.truncated,
	}
	if err := r.classify(&res, waitErr, ctx, diagnostic); err != nil {
		return res, err
	}
	return res, nil
}

// attachDiagnostics routes nsjail's log fd (3) to a pipe so its own warnings and
// errors are captured separately from the sandboxed child's stdout/stderr. The
// returned channel yields the captured text once the process has exited and the
// pipe reaches EOF. The child cannot forge this: nsjail closes log fd 3 before
// executing the jailed process.
func attachDiagnostics(cmd *exec.Cmd) <-chan string {
	r, w, err := os.Pipe()
	if err != nil {
		// Without a diagnostics pipe the runner still works; failures then
		// surface as exit code 255 with an empty diagnostic.
		done := make(chan string, 1)
		done <- ""
		close(done)
		return done
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, w)
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		r.Close()
		done <- strings.TrimSpace(string(b))
	}()
	return done
}

// closeWriteEnd closes the parent's copy of the diagnostics pipe write end so
// the reader observes EOF once nsjail (and its children) exit.
func closeWriteEnd(cmd *exec.Cmd) {
	for _, f := range cmd.ExtraFiles {
		if f != nil {
			f.Close()
		}
	}
	cmd.ExtraFiles = nil
}

// waitDiagnostic reads the captured nsjail diagnostic, bounded so a leaked pipe
// write end (e.g. a stray fd inherited by a detached process) can never block a
// run past a short deadline.
func waitDiagnostic(done <-chan string) string {
	select {
	case d := <-done:
		return d
	case <-time.After(2 * time.Second):
		return ""
	}
}

// classify fills exit code and terminal-state flags from the wait error. A
// failed sandbox setup or launch (nsjail exit 255 with its own diagnostic) is
// reported as ErrSandboxUnavailable rather than a normal exit.
func (r *NsjailRunner) classify(res *Result, waitErr error, ctx context.Context, diagnostic string) error {
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = 124
		return nil
	}
	if ctx.Err() == context.Canceled {
		res.Killed = true
		res.ExitCode = 137
		return nil
	}
	if waitErr == nil {
		res.ExitCode = 0
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code := exitErr.ExitCode()
		res.ExitCode = code
		if code == -1 {
			// Signalled by SIGTERM/SIGKILL (e.g. external stop).
			res.Killed = true
			res.ExitCode = 137
			return nil
		}
		// nsjail exits 255 when it cannot set up or launch the sandbox. Its log
		// carries a fatal message in that case; a sandboxed program that simply
		// exits 255 produces stdout/stderr but no nsjail fatal line.
		if code == 255 && isLaunchFailure(diagnostic) {
			// A cwd that does not exist (or is not a directory) makes nsjail's
			// chdir fail *inside* the sandbox, which nsjail reports as a launch
			// failure. That is a bad request path, not a broken isolation layer,
			// so classify it as ErrInvalidCwd (422) rather than a sandbox
			// failure (503). The distinction matters because cwd is
			// caller-controlled: a typo must never read as "sandbox unavailable".
			if isChdirFailure(diagnostic) {
				res.ExitCode = 255
				return ErrInvalidCwd
			}
			res.ExitCode = 255
			return &SandboxError{Diagnostic: summarizeDiagnostic(diagnostic)}
		}
		return nil
	}
	res.ExitCode = -1
	return nil
}

// isChdirFailure reports whether nsjail's diagnostic indicates it failed to
// change into the requested working directory (as opposed to a genuine sandbox
// setup failure such as an unusable cgroup hierarchy). nsjail logs a line
// containing "chdir" and the target path when the cwd is missing.
func isChdirFailure(diagnostic string) bool {
	for _, line := range strings.Split(diagnostic, "\n") {
		if strings.Contains(line, "chdir") || strings.Contains(line, "chdir(") {
			return true
		}
	}
	return false
}

// isLaunchFailure reports whether nsjail's own log indicates it failed to set up
// or launch the sandbox (as opposed to the jailed program running and exiting).
func isLaunchFailure(diagnostic string) bool {
	for _, line := range strings.Split(diagnostic, "\n") {
		if strings.Contains(line, "[F]") || strings.Contains(line, "[E]") {
			return true
		}
	}
	return false
}

// summarizeDiagnostic keeps the most useful nsjail log lines, preferring the
// last fatal/error line and capping the length.
func summarizeDiagnostic(diagnostic string) string {
	const maxLen = 512
	var chosen string
	for _, line := range strings.Split(diagnostic, "\n") {
		if strings.Contains(line, "[F]") || strings.Contains(line, "[E]") {
			chosen = strings.TrimSpace(line)
		}
	}
	if chosen == "" {
		chosen = strings.TrimSpace(diagnostic)
	}
	if len(chosen) > maxLen {
		chosen = chosen[:maxLen]
	}
	return chosen
}

func (r *NsjailRunner) track(runID string, p *os.Process) {
	r.mu.Lock()
	r.procs[runID] = p
	r.mu.Unlock()
}

// KillProcessTree stops the process tree a host pid belongs to. When the pid is
// inside a sandbox cgroup the whole cgroup is killed (so a process that escaped
// its process group with setsid() is caught); otherwise the pid's process group
// is signalled. It returns true when a signal was sent.
func (r *NsjailRunner) KillProcessTree(pid int) bool {
	if pid <= 0 {
		return false
	}
	if dir := CgroupDirOfPID(pid); dir != "" {
		KillCgroupTree(dir)
		return true
	}
	// Not in a sandbox cgroup: fall back to the process group.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(r.cfg.Sandbox.KillGrace())
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return true
}

// ListProcesses returns every live process currently inside a sandbox cgroup.
func (r *NsjailRunner) ListProcesses() []ProcessInfo { return ListJailProcesses() }

// NetTablesForPids reads the /proc/<pid>/net/* tables and attributes socket
// inodes to the supplied candidate pids. See executor/ports.go.
func (r *NsjailRunner) NetTablesForPids(pids []int, limits ports.Limits) ports.NetTables {
	return NetTablesForPids(pids, limits)
}

// PortReads returns the process-wide port-table read counter.
func (r *NsjailRunner) PortReads() int64 { return PortReads() }

// ReapOrphans kills and removes every orphaned sandbox cgroup on the host.
func (r *NsjailRunner) ReapOrphans() int { return ReapOrphans() }

// EnsureWorkspaceCgroup creates (idempotently) the workspace's parent cgroup and
// writes its enforced limits. It is a no-op when cgroup v2 is not usable (an
// unprivileged process, or a host where the hierarchy is not writable): nsjail
// cannot enforce a cgroup ceiling there either, and readiness reports it.
func (r *NsjailRunner) EnsureWorkspaceCgroup(ws *model.Workspace) error {
	if !r.CgroupsUsable() {
		return nil
	}
	return EnsureWorkspaceCgroup(ws)
}

// RemoveWorkspaceCgroup kills any remaining member of the workspace's parent
// cgroup and removes it. It is a no-op when cgroup v2 is not usable.
func (r *NsjailRunner) RemoveWorkspaceCgroup(wsID string) {
	if !r.CgroupsUsable() {
		return
	}
	RemoveWorkspaceCgroup(wsID)
}

// trackCgroup records the NSJAIL cgroup directory for a run so a later stop can
// kill the whole cgroup (not just the process group) and the completion path can
// remove it.
func (r *NsjailRunner) trackCgroup(runID, dir string) {
	if runID == "" || dir == "" {
		return
	}
	r.mu.Lock()
	r.cgroupDirs[runID] = dir
	r.mu.Unlock()
}

// RunCgroupDirs returns the live run ID -> NSJAIL cgroup directory map. It is
// the sampler's O(live runs) source for a workspace's run cgroups: the service
// joins it against its own run table (run ID -> workspace ID), so a cgroup can
// be attributed without scanning /sys/fs/cgroup or reading
// /proc/<pid>/environ.
//
// A run whose cgroup was not resolved at launch (the bounded fork-race window
// can expire on a loaded host, in which case trackCgroup stored nothing) is
// re-resolved here from the nsjail pid the runner already tracks. This is the
// SAME registry and the SAME resolver (CgroupForNsjailPID), not a second one:
// it only closes the race so a live run is never reported as RAM 0. The
// resolution happens outside the lock (it can briefly wait for the cgroup).
func (r *NsjailRunner) RunCgroupDirs() map[string]string {
	r.mu.Lock()
	out := make(map[string]string, len(r.procs))
	needPID := map[string]int{}
	for id, p := range r.procs {
		if dir := r.cgroupDirs[id]; dir != "" {
			out[id] = dir
			continue
		}
		if p != nil {
			needPID[id] = p.Pid
		}
	}
	r.mu.Unlock()

	for id, pid := range needPID {
		if dir := CgroupForNsjailPID(pid); dir != "" {
			out[id] = dir
			r.trackCgroup(id, dir)
		}
	}
	return out
}

// resolveCgroup resolves the cgroup directory nsjail created for a jail it just
// launched. It is best-effort: an empty result means the cgroup could not be
// located (the workload already exited, or /proc is restricted); nsjail still
// cleans up its own cgroup on a clean exit.
func resolveCgroup(nsjailPID int) string {
	return CgroupForNsjailPID(nsjailPID)
}

// reapTrackedCgroup kills the process tree of a run and removes its cgroup once
// the run has ended, so a detached descendant cannot survive the run.
func (r *NsjailRunner) reapTrackedCgroup(runID string) {
	r.mu.Lock()
	dir := r.cgroupDirs[runID]
	r.mu.Unlock()
	if dir != "" {
		ReapCgroup(dir)
	}
}

func (r *NsjailRunner) untrack(runID string) {
	r.mu.Lock()
	delete(r.procs, runID)
	delete(r.cgroupDirs, runID)
	r.mu.Unlock()
}

// Kill terminates the process tree of a running run. It prefers the run's
// cgroup (killing every member, including a process that escaped its process
// group with setsid()), falling back to the process group when no cgroup is
// known. It then removes the cgroup it caused to be created.
func (r *NsjailRunner) Kill(runID string) bool {
	r.mu.Lock()
	p, ok := r.procs[runID]
	dir := r.cgroupDirs[runID]
	r.mu.Unlock()

	signalled := false
	if dir != "" && killCgroup(dir) {
		signalled = true
	}
	if ok && p != nil {
		if killProcessGroup(p.Pid, r.cfg.Sandbox.KillGrace()) {
			signalled = true
		}
	}
	// Always try to remove a cgroup we created, bounded so a busy cgroup cannot
	// hang a stop request.
	if dir != "" {
		waitCgroupGone(dir, r.cfg.Sandbox.KillGrace()+time.Second)
	}
	return signalled
}

// killProcessGroup sends SIGTERM to -pgid, waits graceSec, then SIGKILL.
func killProcessGroup(pid int, grace time.Duration) bool {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		// Fall back to the direct process if the group is gone.
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return true
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil
}

// cappedWriter writes up to limit bytes then silently drops the rest.
type cappedWriter struct {
	w         io.Writer
	limit     int64
	written   int64
	truncated bool
	mu        sync.Mutex
}

func newCappedWriter(w io.Writer, limit int64) *cappedWriter {
	return &cappedWriter{w: w, limit: limit}
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit > 0 {
		remaining := c.limit - c.written
		if remaining <= 0 {
			c.truncated = true
			// Report full consumption so the caller does not treat the drop as
			// a write error; the bytes are intentionally discarded.
			return len(p), nil
		}
		if int64(len(p)) > remaining {
			toWrite := p[:remaining]
			c.truncated = true
			if _, err := c.w.Write(toWrite); err != nil {
				return 0, err
			}
			c.written += remaining
			return len(p), nil
		}
	}
	n, err := c.w.Write(p)
	c.written += int64(n)
	return n, err
}

func writeAppend(path string, rd io.Reader) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, rd)
	return err
}

// nsjailCommand builds the nsjail command, entering the workspace's private
// network namespace first when one is configured. `nsenter --no-fork` replaces
// itself (no intermediate helper process), so the direct child remains nsjail
// and the cgroup resolver keeps working unchanged. When no namespace is given,
// nsjail is executed directly and creates its own empty namespace.
func nsjailCommand(ctx context.Context, bin, cfgPath, netns string) *exec.Cmd {
	nsjailArgs := []string{"-q", "-L", "3", "--config", cfgPath}
	if netns == "" {
		return exec.CommandContext(ctx, bin, nsjailArgs...)
	}
	args := append([]string{"--no-fork", "--net=" + netns, bin}, nsjailArgs...)
	return exec.CommandContext(ctx, "nsenter", args...)
}

// tmplData is the data passed to the nsjail config template.
type tmplData struct {
	Argv         []string
	Env          []string
	Cwd          string
	TimeLimitSec int
	CPUCores     float64
	CPUMilli     int
	MemoryMB     int
	// MemoryBytes is the per-run cgroup_mem_max. It is kept EQUAL to the
	// workspace's ceiling so nsjail still creates its leaf cgroup (a leaf is
	// created only when a cgroup limit is set); the workspace PARENT enforces
	// the SUM. See the block above CgroupMount for the measurement.
	MemoryBytes int64
	PidsMax     int
	FsizeMB     int
	// Cgroupv2Mount, when non-empty, is the workspace's parent cgroup. nsjail is
	// pointed at it via cgroupv2_mount so every run's leaf cgroup is created
	// inside it and the parent's memory.max is the enforced ceiling. Empty
	// omits the field (nsjail then uses the root, the legacy per-run layout).
	Cgroupv2Mount string
	// AsMB is the rlimit_as backstop in MB: the workspace's memory_mb plus a
	// runtime address-space headroom (see addressSpaceMB), never memory_mb
	// alone -- a managed runtime reserves more VIRTUAL space than it commits.
	AsMB       int
	AllowNet   bool
	DisableNet bool
	// JoinNetns, when true, means the runner has already entered the
	// workspace's own network namespace and nsjail must not create another one
	// (clone_newnet:false). It is never a host-network share.
	JoinNetns     bool
	ResolverConf  string
	HostsConf     string
	User          string
	Group         string
	SandboxUID    int
	SandboxGID    int
	OutsideUID    int
	OutsideGID    int
	WorkspaceRoot string
	TempDir       string
	SeccompString string
	SeccompPolicy []string

	// PTY, when set, adds the host /dev/pts and /dev/ptmx bind mounts a terminal
	// session needs (pty.fork() requires a working /dev/ptmx and devpts). It is
	// false for one-shot exec, which gets the smaller /dev.
	PTY bool

	// Two-tier package mounts. SharedPackagesDir is the host directory
	// bind-mounted read-only; PrivatePackagesDir is the workspace's host
	// directory bind-mounted writable. Both are optional (empty = not mounted).
	SharedPackagesDir  string
	PrivatePackagesDir string
	// PackagesPathEnv is the interpreter search-path value passed via the
	// allowlisted PYTHONPATH variable (empty when no tier is mounted).
	PackagesPathEnv string

	// PipEnv carries the engine-controlled pip defaults for a run with a
	// writable private tier. They make a bare `pip install X` work with no
	// flags, no venv and no environment preparation: the target is the
	// workspace's own tier (already on PYTHONPATH), PEP 668 is bypassed, and
	// pip never prompts. They are emitted before any user env and PIP_* is
	// deliberately absent from allowedEnvKeys, so a caller cannot override them
	// (see pipEnvDefaults and allowedEnvKeys).
	PipEnv []string

	// TagEnv carries the process-attribution tags (workspace + kind + source)
	// as `KEY=VALUE` entries, injected into every sandbox so a process can be
	// mapped back to its owning run/session in the inventory.
	TagEnv []string

	// Synthetic /proc (see proc.go). When ProcEnabled is true a curated
	// per-workspace directory is bind-mounted read-only over /proc, carrying
	// only the workspace's own resource facts (meminfo/cpuinfo/uptime/loadavg/
	// stat/version/mounts). mount_proc stays false.
	ProcEnabled bool
	ProcRoot    string
	ProcJail    string
	// ProcMtab is the host path bound over /etc/mtab so tools that read the
	// legacy mount table (mount(8), some scripts) see the workspace's curated
	// mount list. `df` itself does NOT read it (coreutils df opens only
	// /proc/self/mountinfo, measured); it is provided because the conventional
	// /etc/mtab is a symlink to /proc/mounts and a workspace should have the
	// same view. Empty omits the mount.
	ProcMtab string

	// Host-identity masking (see mask.go). When MaskEnabled is true the shim
	// and its ld.so.preload list are bind-mounted read-only and MaskEnv carries
	// the workspace's own cpu/memory values the shim reports.
	MaskEnabled     bool
	MaskShimHost    string
	MaskPreloadHost string
	MaskShimJail    string
	MaskPreloadJail string
	MaskEnv         []string

	// Profile / provider overlays (see profile_overlay.go and provider.go).
	// When OverlayEnabled is true the profile-materialized /usr (also bound at
	// /bin, /sbin, /lib and /lib64), the profile's attached non-/usr paths, the
	// filtered /etc/passwd and /etc/group and (when present) /etc/ssl/certs are
	// bind-mounted read-only over the host's own, so a workspace sees only its
	// profile's allowlist with no platform or engine material.
	OverlayEnabled bool
	UsrRoot        string
	CertsDir       string
	PasswdHost     string
	GroupHost      string
	UsrJail        string
	BinJail        string
	SbinJail       string
	LibJail        string
	Lib64Jail      string
	PasswdJail     string
	GroupJail      string
	// ExtraMounts are the profile's attached non-/usr paths: each entry is
	// {jailPath, hostPath}. Rendered as read-only mandatory binds.
	ExtraMounts []extraMount
}

// extraMount is one profile-attached non-/usr path to bind read-only.
type extraMount struct {
	Jail string
	Host string
}

// Paths at which the package tiers are mounted inside the sandbox.
const (
	sharedPackagesMount  = "/opt/python/shared"
	privatePackagesMount = "/opt/python/private"
)

// packageSearchPath builds the PYTHONPATH value for whichever tiers are
// mounted. Ordering is deliberate: the shared (read-only) tier precedes the
// private tier, so the private tier can override a shared package for its own
// workspace without affecting any other workspace. Every mounted dir is
// included so `python3 -c`, `python3 file.py` and `python3 -m module` all see
// the same sys.path additions.
func packageSearchPath(sharedMount, privateMount, sharedDir, privateDir string) string {
	var parts []string
	if sharedDir != "" {
		parts = append(parts, sharedMount)
	}
	if privateDir != "" {
		parts = append(parts, privateMount)
	}
	return strings.Join(parts, ":")
}

// withPackagePath appends the package search path as an allowlisted PYTHONPATH
// entry. A caller-supplied PYTHONPATH is preserved and the tier paths are
// placed after it so the two tiers always remain visible. The value is not
// treated as trusted input because those paths are service-chosen mounts, not
// caller-controlled.
func withPackagePath(env []string, pkgPath string) []string {
	if pkgPath == "" {
		return env
	}
	for i, kv := range env {
		if strings.HasPrefix(kv, "PYTHONPATH=") {
			env[i] = kv + ":" + pkgPath
			return env
		}
	}
	out := append(env, "PYTHONPATH="+pkgPath)
	sortStrings(out)
	return out
}

// pipEnvDefaults are the ENGINE-provided pip defaults for a run that has a
// writable private tier. They exist so a bare `pip install X` inside a
// workspace terminal just works: no --break-system-packages, no --user, no
// venv and no environment preparation.
//
// The three measured facts that made a bare pip fail are all handled here:
//
//   - /usr is bind-mounted read-only into the sandbox, and the host carries
//     PEP 668's /usr/lib/python3.12/EXTERNALLY-MANAGED. PIP_TARGET points pip
//     at the workspace's own writable tier (/opt/python/private), which is
//     already on PYTHONPATH (withPackagePath), so `python3 -c "import X"`
//     finds what pip installed. PIP_BREAK_SYSTEM_PACKAGES=1 additionally makes
//     the PEP 668 refusal non-fatal in case a future pip consults it even with
//     an explicit target.
//   - pip may prompt (e.g. for a missing index credential); PIP_NO_INPUT=1
//     keeps it non-interactive, matching the bot's own install path.
//   - the version check is a needless network round-trip;
//     PIP_DISABLE_PIP_VERSION_CHECK=1 skips it. PIP_ROOT_USER_ACTION=ignore
//     silences the root-user warning when the sandbox uid happens to be 0
//     (harmless otherwise).
//
// SECURITY: these are emitted BEFORE any caller env and PIP_* is absent from
// allowedEnvKeys, so a caller cannot supply PIP_TARGET=/usr (or any other
// override): formatEnv drops every PIP_* key outright. See
// TestPipEnvPrecedence.
func pipEnvDefaults() []string {
	return pipEnvDefaultsFor(privatePackagesMount)
}

// pipEnvDefaultsFor returns the same default set with an explicit target. It
// exists so a test can exercise the mechanism against a temporary directory
// when the real /opt/python/private mount is not present (an unprivileged or
// non-nsjail host); production callers use pipEnvDefaults, which always names
// the sandbox mount.
func pipEnvDefaultsFor(target string) []string {
	return []string{
		"PIP_TARGET=" + target,
		"PIP_BREAK_SYSTEM_PACKAGES=1",
		"PIP_NO_INPUT=1",
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
		"PIP_ROOT_USER_ACTION=ignore",
	}
}

// pipEnvFor returns the pip defaults only when the run actually mounts a
// writable private tier. Without that mount /opt/python/private does not
// exist, and pointing PIP_TARGET at a nonexistent path would turn a bare
// `pip install` into a different, confusing failure; a run with no tier keeps
// the previous behaviour (pip's own default).
func pipEnvFor(privateDir string) []string {
	if privateDir == "" {
		return nil
	}
	return pipEnvDefaults()
}

func (r *NsjailRunner) renderConfig(req Request, runTmp string) (string, error) {
	if req.Workspace == nil {
		return "", fmt.Errorf("workspace is required")
	}
	tmplBytes, err := os.ReadFile(r.tmplPath)
	if err != nil {
		// Fall back to the template compiled into the binary so a deployment
		// that ships only the executable still works.
		tmplBytes = configs.NSJailPythonTemplate
		if len(tmplBytes) == 0 {
			return "", fmt.Errorf("read nsjail template: %w", err)
		}
	}
	tmpl, err := template.New("nsjail").Parse(string(tmplBytes))
	if err != nil {
		return "", fmt.Errorf("parse nsjail template: %w", err)
	}

	lim := req.Workspace.Limits
	timeoutSec := int(req.Timeout.Seconds())
	if req.NoTimeout {
		// nsjail's `time_limit: 0` means "no limit" (verified against the
		// installed build: a process ran past the 6 s command timeout when
		// time_limit was 0, and was killed at 3 s when it was 3).
		timeoutSec = 0
	} else if timeoutSec <= 0 {
		timeoutSec = lim.MaxWallTimeSec
	}
	cwd, err := sandboxCwd(req.Cwd)
	if err != nil {
		return "", err
	}

	// The template hardcodes the sandbox mount points and uses these host
	// paths as the bind sources; an empty value omits the mount entirely.
	sharedDir := req.SharedPackagesDir
	privateDir := req.PrivatePackagesDir
	pkgPath := packageSearchPath(sharedPackagesMount, privatePackagesMount, req.SharedPackagesDir, req.PrivatePackagesDir)

	// Resolve the workspace's profile (the stored name, or the default). An
	// unknown profile is a hard error: a workspace must never silently run
	// under a different allowlist than its record says.
	pf, err := r.profileFor(req.Workspace)
	if err != nil {
		return "", err
	}

	// Host-identity masking: derive the per-workspace values from THIS
	// workspace's limits (never the host's), and refuse the run when masking is
	// enabled but not ready, so a sandbox that would leak is never launched.
	mask, err := r.mask.params(pf, lim.CPUCores, lim.MemoryMB)
	if err != nil {
		return "", err
	}
	if r.mask.Enabled() && !mask.Enabled {
		return "", fmt.Errorf("%w: %s", ErrMaskUnavailable, r.mask.Reason())
	}
	extra := make([]extraMount, 0, len(mask.ExtraMounts))
	for jail, host := range mask.ExtraMounts {
		extra = append(extra, extraMount{Jail: jail, Host: host})
	}
	sortExtraMounts(extra)

	// Synthetic per-workspace /proc (see proc.go). It is generated under the
	// run's private tmp dir (removed with it) and bind-mounted read-only over
	// /proc. It carries only the workspace's own resource facts, so `free -h`
	// and `uptime` report the workspace, never the host. mount_proc stays false.
	//
	// It is gated on the mask being enabled because the two are the SAME channel
	// viewed two ways: the shim answers uname/sysconf/sysinfo with the workspace's
	// values, and this payload answers meminfo/cpuinfo/version with the SAME
	// values. With masking OFF (a diagnostic state) neither is mounted, so the
	// channels can never disagree (and the unmasked shim's host values are the
	// only values present).
	procEnabled := r.cfg.Disk.ProcEnabled && mask.Enabled
	procRoot := ""
	procJail := ""
	procMtab := ""
	if procEnabled {
		procRoot = filepath.Join(runTmp, ".proc")
		cpuCount := mask.CPUs
		if cpuCount < 1 {
			cpuCount = 1
		}
		kr := mask.KernelRelease
		if kr == "" {
			kr = config.DefaultMaskKernelRelease
		}
		if _, err := writeSyntheticProc(procRoot, cpuCount, lim.MemoryMB, lim.DiskMB, kr); err != nil {
			return "", fmt.Errorf("synthetic proc: %w", err)
		}
		procJail = "/proc"
		// /etc/mtab is the conventional read-only view of the same curated
		// mount table (normally a symlink to /proc/self/mounts). It is a
		// separate one-file mount because /etc is not the curated /proc tree.
		procMtab = filepath.Join(procRoot, "tab")
	}

	data := tmplData{
		Argv:          req.Argv,
		Env:           withPackagePath(formatEnv(req.Env), pkgPath),
		PipEnv:        pipEnvFor(privateDir),
		Cwd:           cwd,
		TimeLimitSec:  timeoutSec,
		CPUCores:      lim.CPUCores,
		CPUMilli:      int(lim.CPUCores * 1000),
		MemoryMB:      lim.MemoryMB,
		MemoryBytes:   int64(lim.MemoryMB) * 1024 * 1024,
		Cgroupv2Mount: req.WorkspaceCgroup,
		PidsMax:       lim.MaxRuns * 64,
		FsizeMB:       lim.DiskMB,
		AsMB:          addressSpaceMB(lim.MemoryMB),
		AllowNet:      lim.AllowNet,
		DisableNet:    !lim.AllowNet,
		JoinNetns:     req.NetnsPath != "",
		ResolverConf:  req.ResolverConf,
		HostsConf:     req.HostsConf,
		User:          r.cfg.Sandbox.User,
		Group:         r.cfg.Sandbox.Group,
		SandboxUID:    r.identity.SandboxUID,
		SandboxGID:    r.identity.SandboxGID,
		OutsideUID:    r.identity.OutsideUID,
		OutsideGID:    r.identity.OutsideGID,
		WorkspaceRoot: req.WorkspaceRoot,
		TempDir:       runTmp,
		SeccompString: seccompString(),
		SeccompPolicy: seccompPolicy(),

		PTY: req.PTY,

		SharedPackagesDir:  sharedDir,
		PrivatePackagesDir: privateDir,
		PackagesPathEnv:    pkgPath,

		TagEnv: tagEnv(req),

		MaskEnabled:     mask.Enabled,
		MaskShimHost:    mask.ShimHost,
		MaskPreloadHost: mask.PreloadHost,
		MaskShimJail:    mask.ShimJail,
		MaskPreloadJail: mask.PreloadJail,
		MaskEnv:         mask.envEntries(),

		ProcEnabled: procEnabled,
		ProcRoot:    procRoot,
		ProcJail:    procJail,
		ProcMtab:    procMtab,

		OverlayEnabled: mask.OverlayEnabled,
		UsrRoot:        mask.UsrRoot,
		CertsDir:       mask.CertsDir,
		PasswdHost:     mask.PasswdHost,
		GroupHost:      mask.GroupHost,
		UsrJail:        mask.UsrJail,
		BinJail:        mask.BinJail,
		SbinJail:       mask.SbinJail,
		LibJail:        mask.LibJail,
		Lib64Jail:      mask.Lib64Jail,
		PasswdJail:     mask.PasswdJail,
		GroupJail:      mask.GroupJail,
		ExtraMounts:    extra,
	}
	if data.PidsMax <= 0 {
		data.PidsMax = 64
	}

	if err := os.MkdirAll(r.tmpDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(r.tmpDir, "nsjail-*.cfg")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := tmpl.Execute(f, data); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("render nsjail template: %w", err)
	}
	return f.Name(), nil
}

// sandboxCwd translates a workspace-relative cwd into an absolute path inside
// the jail, where the workspace is mounted at /workspace. An empty cwd means the
// workspace root; any path that would escape the workspace is rejected.
func sandboxCwd(cwd string) (string, error) {
	if cwd == "" {
		return workspaceMount, nil
	}
	p := strings.ReplaceAll(cwd, "\\", "/")
	// A relative cwd is resolved against the workspace root.
	stack := make([]string, 0, 4)
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return "", ErrInvalidCwd
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, part)
		}
	}
	if len(stack) == 0 {
		return workspaceMount, nil
	}
	return workspaceMount + "/" + strings.Join(stack, "/"), nil
}

// workspaceMount is the path at which the workspace is bind-mounted in the jail.
const workspaceMount = "/workspace"

// formatEnv renders user environment variables as `KEY=VALUE` entries for the
// nsjail config. Only allowlisted keys are passed; everything else is dropped.
// Output is sorted for stability.
func formatEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		if !allowEnvKey(k) {
			continue
		}
		out = append(out, k+"="+v)
	}
	sortStrings(out)
	return out
}

// allowedEnvKeys is the allowlist of user-supplied environment variables that
// may reach the sandbox. It deliberately excludes loader-injection variables
// (LD_PRELOAD, LD_LIBRARY_PATH, DYLD_*, GLIBC_TUNABLES, ...) which would let the
// caller alter the behaviour of the interpreter the service chose to run.
var allowedEnvKeys = map[string]bool{
	"PYTHONPATH":              true,
	"PYTHONUNBUFFERED":        true,
	"PYTHONDONTWRITEBYTECODE": true,
	"PYTHONHASHSEED":          true,
	"LANG":                    true,
	"LC_ALL":                  true,
	"LC_CTYPE":                true,
	"TZ":                      true,
	// The standard proxy variables, used only when an operator sets
	// network.egress_proxy (off by default; see ADR 76). They are a soft
	// opt-in mechanism, never set by the engine unless the operator asks.
	"HTTP_PROXY":  true,
	"HTTPS_PROXY": true,
	"ALL_PROXY":   true,
	"NO_PROXY":    true,
	"http_proxy":  true,
	"https_proxy": true,
	"all_proxy":   true,
	"no_proxy":    true,
}

// allowEnvKey reports whether a variable name may be passed into the sandbox.
// The name must also be a syntactically valid identifier.
func allowEnvKey(k string) bool {
	if sanitizeEnvKey(k) == "" {
		return false
	}
	return allowedEnvKeys[k]
}

func sanitizeEnvKey(k string) string {
	if k == "" {
		return ""
	}
	for _, c := range k {
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return ""
		}
	}
	return k
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// tagEnv builds the process-attribution environment entries injected into the
// sandbox. The workspace id is always available (from the request's workspace);
// the kind/source are set for a run or a session. The values are service-chosen
// (a workspace id, a run/session id, a fixed kind word) and are rendered as
// quoted nsjail envar values, so no caller-supplied data reaches them.
func tagEnv(req Request) []string {
	wsID := ""
	if req.Workspace != nil {
		wsID = req.Workspace.ID
	}
	if wsID == "" {
		return nil
	}
	out := []string{TagEnvWorkspace + "=" + wsID}
	if req.ProcessKind != "" {
		out = append(out, TagEnvKind+"="+req.ProcessKind)
	}
	if req.ProcessSource != "" {
		out = append(out, TagEnvSource+"="+req.ProcessSource)
	}
	return out
}

// seccompPolicy returns the baseline syscall deny list applied to sandboxes.
// Names are libseccomp identifiers; `umount2` is exposed as `umount` and the
// obsolete `_sysctl` is omitted because libseccomp does not know it.
func seccompPolicy() []string {
	return []string{
		"mount", "umount", "ptrace", "bpf", "kexec_load",
		"kexec_file_load", "reboot", "swapon", "swapoff",
		"pivot_root", "setns", "unshare", "add_key", "keyctl",
		"request_key", "init_module", "finit_module", "delete_module",
		"nfsservctl", "open_by_handle_at", "perf_event_open",
		"quotactl", "uselib", "iopl", "ioperm",
	}
}

// seccompString renders the deny list as a libseccomp policy: dangerous
// syscalls are KILLed, everything else is allowed so normal programs run.
// The result is a Go-quoted string literal suitable for an nsjail `seccomp_string`
// TextProto field.
func seccompString() string {
	policy := "KILL {\n  " + strings.Join(seccompPolicy(), ", ") + "\n}\nDEFAULT ALLOW"
	return strconv.Quote(policy)
}

// ConfigTemplatePath returns the default template location relative to a dir.
func ConfigTemplatePath(base string) string {
	return filepath.Join(base, "configs", "nsjail_python.cfg.tmpl")
}
