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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"sandboxd/configs"
	"sandboxd/internal/config"
)

// NsjailRunner executes commands under nsjail.
type NsjailRunner struct {
	cfg      config.Config
	tmplPath string
	tmpDir   string
	identity Identity

	mu      sync.Mutex
	procs   map[string]*os.Process // runID -> direct child (nsjail) process
	avail   bool
	availMu sync.Once

	cgroupOnce sync.Once
	cgroups    bool
}

// NewNsjailRunner constructs a runner. tmplPath points at the nsjail config
// template; tmpDir is where per-run configs are written.
func NewNsjailRunner(cfg config.Config, tmplPath, tmpDir string) *NsjailRunner {
	return &NsjailRunner{
		cfg:      cfg,
		tmplPath: tmplPath,
		tmpDir:   tmpDir,
		identity: ResolveIdentity(cfg),
		procs:    make(map[string]*os.Process),
	}
}

// Identity returns the resolved sandbox identity mapping.
func (r *NsjailRunner) Identity() Identity { return r.identity }

// CgroupMount is the location nsjail uses for the cgroup v2 hierarchy.
const CgroupMount = "/sys/fs/cgroup"

// CgroupsUsable reports whether the cgroup v2 hierarchy is writable by this
// process. nsjail's cgroup ceilings (memory, pids, cpu) require it, which in
// turn requires the container to be started with --cgroupns=host. The result is
// probed once and cached.
func (r *NsjailRunner) CgroupsUsable() bool {
	r.cgroupOnce.Do(func() {
		dir, err := os.MkdirTemp(CgroupMount, "sandboxd-probe-*")
		if err != nil {
			r.cgroups = false
			return
		}
		_ = os.Remove(dir)
		r.cgroups = true
	})
	return r.cgroups
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
	// bind-mounts the host's /usr, /bin, /lib and /lib64 at identical paths, so
	// a host-resolved absolute path is valid inside the jail.
	if len(req.Argv) > 0 {
		resolved, rerr := resolveCommand(req.Argv[0])
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

	// Base the run on the caller's context so an explicit stop cancels the
	// process tree, then layer the wall-clock timeout on top.
	base := context.Background()
	if req.Context != nil {
		base = req.Context
	}
	ctx := base
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		ctx, cancel = context.WithTimeout(base, req.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, bin, "-q", "-L", "3", "--config", cfgPath)
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

	res, err := r.runCapture(ctx, cmd, req)
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
			defer r.untrack(req.RunID)
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
			res.ExitCode = 255
			return &SandboxError{Diagnostic: summarizeDiagnostic(diagnostic)}
		}
		return nil
	}
	res.ExitCode = -1
	return nil
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

func (r *NsjailRunner) untrack(runID string) {
	r.mu.Lock()
	delete(r.procs, runID)
	r.mu.Unlock()
}

// Kill terminates the process group of a running run: SIGTERM, grace, SIGKILL.
func (r *NsjailRunner) Kill(runID string) bool {
	r.mu.Lock()
	p, ok := r.procs[runID]
	r.mu.Unlock()
	if !ok || p == nil {
		return false
	}
	return killProcessGroup(p.Pid, r.cfg.Sandbox.KillGrace())
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

// tmplData is the data passed to the nsjail config template.
type tmplData struct {
	Argv          []string
	Env           []string
	Cwd           string
	TimeLimitSec  int
	CPUCores      float64
	CPUMilli      int
	MemoryMB      int
	MemoryBytes   int64
	PidsMax       int
	FsizeMB       int
	AsMB          int
	AllowNet      bool
	DisableNet    bool
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
	if timeoutSec <= 0 {
		timeoutSec = lim.MaxWallTimeSec
	}
	cwd, err := sandboxCwd(req.Cwd)
	if err != nil {
		return "", err
	}

	data := tmplData{
		Argv:          req.Argv,
		Env:           formatEnv(req.Env),
		Cwd:           cwd,
		TimeLimitSec:  timeoutSec,
		CPUCores:      lim.CPUCores,
		CPUMilli:      int(lim.CPUCores * 1000),
		MemoryMB:      lim.MemoryMB,
		MemoryBytes:   int64(lim.MemoryMB) * 1024 * 1024,
		PidsMax:       lim.MaxRuns * 64,
		FsizeMB:       lim.DiskMB,
		AsMB:          lim.MemoryMB,
		AllowNet:      lim.AllowNet,
		DisableNet:    !lim.AllowNet,
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
