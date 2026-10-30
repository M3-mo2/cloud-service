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
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"sandboxd/internal/config"
)

// NsjailRunner executes commands under nsjail.
type NsjailRunner struct {
	cfg      config.Config
	tmplPath string
	tmpDir   string

	mu      sync.Mutex
	procs   map[string]*os.Process // runID -> direct child (nsjail) process
	avail   bool
	availMu sync.Once
}

// NewNsjailRunner constructs a runner. tmplPath points at the nsjail config
// template; tmpDir is where per-run configs are written.
func NewNsjailRunner(cfg config.Config, tmplPath, tmpDir string) *NsjailRunner {
	r := &NsjailRunner{
		cfg:      cfg,
		tmplPath: tmplPath,
		tmpDir:   tmpDir,
		procs:    make(map[string]*os.Process),
	}
	return r
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

	cfgPath, err := r.renderConfig(req)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(cfgPath)

	ctx := context.Background()
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, bin, "--config", cfgPath)
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
func (r *NsjailRunner) runCapture(ctx context.Context, cmd *exec.Cmd, req Request) (Result, error) {
	streamed := req.RunID != "" && req.StdoutPath != ""

	if streamed {
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
			return Result{}, err
		}
		if req.RunID != "" {
			r.track(req.RunID, cmd.Process)
			defer r.untrack(req.RunID)
		}
		waitErr := cmd.Wait()
		res := Result{Streamed: true, Truncated: outW.truncated || errW.truncated}
		r.classify(&res, waitErr, ctx)
		return res, nil
	}

	var stdout, stderr bytes.Buffer
	outW := newCappedWriter(&stdout, req.StdoutCap)
	errW := newCappedWriter(&stderr, req.StderrCap)
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	waitErr := cmd.Wait()
	res := Result{
		Stdout:    stdout.Bytes(),
		Stderr:    stderr.Bytes(),
		Truncated: outW.truncated || errW.truncated,
	}
	r.classify(&res, waitErr, ctx)
	return res, nil
}

// classify fills exit code and terminal-state flags from the wait error.
func (r *NsjailRunner) classify(res *Result, waitErr error, ctx context.Context) {
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = 124
		return
	}
	if ctx.Err() == context.Canceled {
		res.Killed = true
		res.ExitCode = 137
		return
	}
	if waitErr == nil {
		res.ExitCode = 0
		return
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code := exitErr.ExitCode()
		res.ExitCode = code
		if code == -1 {
			// Signalled by SIGTERM/SIGKILL (e.g. external stop).
			res.Killed = true
			res.ExitCode = 137
		}
		return
	}
	res.ExitCode = -1
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
	PidsMax       int
	FsizeBytes    int64
	AsBytes       int64
	AllowNet      bool
	DisableNet    bool
	User          string
	Group         string
	WorkspaceRoot string
	SeccompPolicy []string
}

func (r *NsjailRunner) renderConfig(req Request) (string, error) {
	if req.Workspace == nil {
		return "", fmt.Errorf("workspace is required")
	}
	tmplBytes, err := os.ReadFile(r.tmplPath)
	if err != nil {
		return "", fmt.Errorf("read nsjail template: %w", err)
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
	cwd := req.Cwd
	if cwd == "" {
		cwd = "/"
	}

	data := tmplData{
		Argv:          req.Argv,
		Env:           formatEnv(req.Env),
		Cwd:           cwd,
		TimeLimitSec:  timeoutSec,
		CPUCores:      lim.CPUCores,
		CPUMilli:      int(lim.CPUCores * 1000),
		MemoryMB:      lim.MemoryMB,
		PidsMax:       lim.MaxRuns * 64,
		FsizeBytes:    int64(lim.DiskMB) * 1024 * 1024,
		AsBytes:       int64(lim.MemoryMB) * 1024 * 1024,
		AllowNet:      lim.AllowNet,
		DisableNet:    !lim.AllowNet,
		User:          r.cfg.Sandbox.User,
		Group:         r.cfg.Sandbox.Group,
		WorkspaceRoot: req.WorkspaceRoot,
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

// formatEnv renders user environment variables as `KEY=VALUE` entries for the
// nsjail config. Invalid keys are dropped. Output is sorted for stability.
func formatEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		key := sanitizeEnvKey(k)
		if key == "" {
			continue
		}
		out = append(out, key+"="+v)
	}
	sortStrings(out)
	return out
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
func seccompPolicy() []string {
	return []string{
		"mount", "umount2", "ptrace", "bpf", "kexec_load",
		"kexec_file_load", "reboot", "swapon", "swapoff",
		"pivot_root", "setns", "unshare", "add_key", "keyctl",
		"request_key", "init_module", "finit_module", "delete_module",
		"acct", "nfsservctl", "open_by_handle_at", "perf_event_open",
		"quotactl", "uselib", "_sysctl", "iopl", "ioperm",
	}
}

// ConfigTemplatePath returns the default template location relative to a dir.
func ConfigTemplatePath(base string) string {
	return filepath.Join(base, "configs", "nsjail_python.cfg.tmpl")
}
