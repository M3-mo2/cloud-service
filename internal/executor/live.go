package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// LiveProcess is a long-lived interactive process started with Start. It owns
// the child's stdin write end (so input is never implicitly closed, unlike a
// one-shot Run) and provides a Wait that reaches a terminal Result.
//
// The executor relays input to the process's stdin; for a PTY session those
// bytes are the line-delimited JSON control protocol understood by the PTY
// helper. The helper's stdout -- the PTY output stream -- is written to the
// request's StdoutPath by the OS, so the service reads it with the same
// offset/limit log mechanism used for runs.
type LiveProcess interface {
	// Write sends raw bytes to the process's stdin.
	Write(p []byte) error
	// Close closes stdin (the helper then exits like a session leader losing
	// its terminal). It does not by itself kill the process.
	Close() error
	// Wait blocks until the process exits and returns its Result.
	Wait() (Result, error)
	// Kill terminates the process group (SIGTERM, grace, SIGKILL).
	Kill() bool
	// Pid returns the direct child's process id (the nsjail process).
	Pid() int
	// CgroupDir returns the session's NSJAIL cgroup directory once resolved, or
	// "" when it cannot be located. It lets the metrics sampler sum the
	// session's memory without re-resolving the cgroup every tick.
	CgroupDir() string
}

// Start launches an interactive process under nsjail and returns a handle whose
// stdin the caller owns. It is the interactive counterpart of Run; Run is kept
// unchanged for one-shot execution.
func (r *NsjailRunner) Start(req Request) (LiveProcess, error) {
	if !r.Available() {
		return nil, ErrNsjailUnavailable
	}
	bin, err := r.resolveBinary()
	if err != nil {
		return nil, err
	}
	if len(req.Argv) > 0 {
		resolved, rerr := resolveCommand(req.Argv[0])
		if rerr != nil {
			return nil, rerr
		}
		req.Argv = append([]string{resolved}, req.Argv[1:]...)
	}

	runTmp, err := os.MkdirTemp(r.tmpDir, "ses-*")
	if err != nil {
		return nil, fmt.Errorf("create session tmp: %w", err)
	}
	if err := os.Chmod(runTmp, 0o1777); err != nil {
		os.RemoveAll(runTmp)
		return nil, err
	}
	if err := r.identity.ChownToSandbox(runTmp); err != nil {
		os.RemoveAll(runTmp)
		return nil, fmt.Errorf("prepare session tmp ownership: %w", err)
	}
	if r.identity.Privileged {
		if err := ensureTraversable(runTmp); err != nil {
			os.RemoveAll(runTmp)
			return nil, err
		}
		if req.WorkspaceRoot != "" {
			if err := ensureTraversable(req.WorkspaceRoot); err != nil {
				os.RemoveAll(runTmp)
				return nil, err
			}
		}
	}

	// The PTY relay no longer exists as a file. It is passed to python3 inline
	// (`python3 -c "<source>"`) as the entry command, so nothing named the
	// engine is materialized into the sandbox: there is no helper path to read,
	// no fixture directory to hide, and the source is the same bytes a
	// legitimate interpreter would see (argv[0] is `-c`). The sandbox is not
	// weakened for sessions: the relay runs with the same identity, mounts and
	// seccomp policy as any workload.
	cfgPath, err := r.renderSessionConfig(req, runTmp)
	if err != nil {
		os.RemoveAll(runTmp)
		return nil, err
	}

	if r.identity.Privileged && req.WorkspaceRoot != "" {
		if err := chownTree(req.WorkspaceRoot, r.identity.SandboxUID, r.identity.SandboxGID); err != nil {
			os.RemoveAll(runTmp)
			os.Remove(cfgPath)
			return nil, fmt.Errorf("prepare workspace ownership: %w", err)
		}
	}
	if r.identity.Privileged && req.PrivatePackagesDir != "" {
		if _, err := os.Stat(req.PrivatePackagesDir); err == nil {
			if err := ensureTraversable(req.PrivatePackagesDir); err != nil {
				os.RemoveAll(runTmp)
				os.Remove(cfgPath)
				return nil, err
			}
			if err := chownTree(req.PrivatePackagesDir, r.identity.SandboxUID, r.identity.SandboxGID); err != nil {
				os.RemoveAll(runTmp)
				os.Remove(cfgPath)
				return nil, fmt.Errorf("prepare private package ownership: %w", err)
			}
		}
	}

	cmd := nsjailCommand(context.Background(), bin, cfgPath, req.NetnsPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cleanupSessionArtifacts(cfgPath, runTmp)
		return nil, err
	}

	diagDone := attachDiagnostics(cmd)

	if req.StdoutPath == "" {
		cleanupSessionArtifacts(cfgPath, runTmp)
		return nil, fmt.Errorf("session requires a stdout path")
	}
	outF, err := os.OpenFile(req.StdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		cleanupSessionArtifacts(cfgPath, runTmp)
		return nil, err
	}
	outCap := req.StdoutCap
	if outCap <= 0 {
		outCap = req.SessionOutputCap
	}
	outW := newCappedWriter(outF, outCap)
	cmd.Stdout = outW
	// The helper's diagnostics are not part of the terminal stream; send them
	// to the same capped log so a helper failure is observable, never to the
	// client's byte stream. For a PTY session a small tail of stderr is also
	// mirrored in memory so an early PTY-allocation failure can be recognised
	// and turned into a failed session (see PTYErrorMarker).
	var helperTail *tailBuffer
	if req.PTY {
		helperTail = newTailBuffer(8192)
		cmd.Stderr = io.MultiWriter(outW, helperTail)
	} else {
		cmd.Stderr = outW
	}

	if err := cmd.Start(); err != nil {
		outF.Close()
		closeWriteEnd(cmd)
		<-diagDone
		cleanupSessionArtifacts(cfgPath, runTmp)
		return nil, err
	}
	closeWriteEnd(cmd)

	lp := &liveProcess{
		cmd:        cmd,
		stdin:      stdin,
		out:        outW,
		outFile:    outF,
		runTmp:     runTmp,
		cfgPath:    cfgPath,
		diagDone:   diagDone,
		helperTail: helperTail,
		killGrace:  r.cfg.Sandbox.KillGrace(),
		nsjailPID:  cmd.Process.Pid,
	}
	return lp, nil
}

func cleanupSessionArtifacts(cfgPath, runTmp string) {
	if cfgPath != "" {
		_ = os.Remove(cfgPath)
	}
	if runTmp != "" {
		_ = os.RemoveAll(runTmp)
	}
}

// liveProcess implements LiveProcess for an nsjail-hosted session.
type liveProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	out     *cappedWriter
	outFile *os.File

	runTmp  string
	cfgPath string

	// nsjailPID is the direct child (nsjail) process id. The session's cgroup
	// directory (NSJAIL.<leader>) is resolved lazily from it on first Kill/Wait,
	// because nsjail creates the directory just after clone and a very short
	// session would otherwise race the resolution.
	nsjailPID int

	// cgroupDir is the resolved NSJAIL cgroup the session runs in (cache; empty
	// until resolved). It is the cgroup-scoped handle used to kill the whole
	// session tree (including a process that escaped its process group with
	// setsid()) and to remove the cgroup when the session ends. A failed
	// resolution is retried on the next call rather than cached (see cgroup()).
	cgroupMu  sync.Mutex
	cgroupDir string

	// helperTail mirrors the tail of the helper's stderr for a PTY session so
	// an early PTY-allocation failure (PTYErrorMarker) is recognisable without
	// re-reading the capped output log.
	helperTail *tailBuffer

	diagDone  <-chan string
	killGrace time.Duration

	writeMu sync.Mutex
	closeMu sync.Once
}

// Write sends bytes to the child's stdin (the PTY helper control protocol).
func (p *liveProcess) Write(b []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err := p.stdin.Write(b)
	return err
}

// Close closes the child's stdin.
func (p *liveProcess) Close() error {
	var err error
	p.closeMu.Do(func() { err = p.stdin.Close() })
	return err
}

// Pid returns the direct child process id.
func (p *liveProcess) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// cgroup resolves the NSJAIL cgroup directory of the session's jail. It returns
// "" when the cgroup cannot be located (the process already exited, or /proc is
// restricted); Kill then falls back to the process group.
//
// A failed resolution is NOT cached: the bounded fork-race window can expire on
// a loaded host, so a later call (the sampler's CgroupDir) retries with the same
// deterministic resolver until the directory appears. Once found it is cached.
func (p *liveProcess) cgroup() string {
	p.cgroupMu.Lock()
	defer p.cgroupMu.Unlock()
	if p.cgroupDir != "" {
		return p.cgroupDir
	}
	p.cgroupDir = CgroupForNsjailPID(p.nsjailPID)
	return p.cgroupDir
}

// CgroupDir returns the session's resolved NSJAIL cgroup directory, or "" when
// it cannot be located. The metrics sampler uses it to sum the session's
// memory.current without re-resolving the cgroup from /proc on every tick.
func (p *liveProcess) CgroupDir() string { return p.cgroup() }

// Kill terminates the session's process tree. It prefers the session's cgroup
// (killing every member, including a process that escaped its process group with
// setsid()), falling back to the process group. It then removes the cgroup.
func (p *liveProcess) Kill() bool {
	signalled := false
	if dir := p.cgroup(); dir != "" {
		if _, err := os.Stat(dir); err == nil {
			KillCgroupTree(dir)
			signalled = true
		}
	}
	if p.cmd.Process != nil {
		if killProcessGroup(p.cmd.Process.Pid, p.killGrace) {
			signalled = true
		}
	}
	return signalled
}

// Wait blocks for the process to exit and classifies the outcome. Artifacts
// (the rendered config and the per-session tmp) are removed once the process is
// gone, so a session leaves nothing behind.
func (p *liveProcess) Wait() (Result, error) {
	waitErr := p.cmd.Wait()
	diagnostic := waitDiagnostic(p.diagDone)
	_ = p.stdoutClose()
	cleanupSessionArtifacts(p.cfgPath, p.runTmp)

	// Reap the session's cgroup so a workload that outlived the shell (a
	// detached process, or one that escaped its process group) cannot survive
	// the session, and the cgroup directory does not accumulate.
	if dir := p.cgroup(); dir != "" {
		ReapCgroup(dir)
	}

	res := Result{Streamed: true, Truncated: p.out.truncated}
	// Reuse Run's classification: a signalled exit is a kill, an nsjail exit
	// 255 with a fatal diagnostic is a sandbox launch failure.
	if err := (&NsjailRunner{}).classify(&res, waitErr, context.Background(), diagnostic); err != nil {
		return res, err
	}
	// A PTY session whose helper could not allocate a terminal must surface as a
	// sandbox failure with an actionable diagnostic, never as a running session
	// whose output is a Python traceback. The helper writes PTYErrorMarker to
	// stderr and exits non-zero in that case; an ordinary shell exit is
	// unaffected (the shell's own output goes to the PTY, not the helper's
	// stderr, and any status may still be a normal exit code).
	if perr := ptyFailure(p.helperTail, res.ExitCode); perr != nil {
		return res, perr
	}
	return res, nil
}

// ptyFailure reports a PTY-allocation failure when the relay's stderr carries
// PTYErrorMarker and the process did not exit cleanly. It is nil for a healthy
// session (including one whose shell exited non-zero on its own, because the
// marker is absent).
func ptyFailure(tail *tailBuffer, exitCode int) *SandboxError {
	if tail == nil || exitCode == 0 || !tail.contains(PTYErrorMarker) {
		return nil
	}
	return &SandboxError{Diagnostic: "PTY allocation failed: " + tail.lineContaining(PTYErrorMarker)}
}

// tailBuffer keeps the last limit bytes written to it. It has a single writer
// (the os/exec copy goroutine feeding stderr) and is read only after the
// process has been waited for, so it needs no locking.
type tailBuffer struct {
	limit int
	buf   []byte
}

func newTailBuffer(limit int) *tailBuffer { return &tailBuffer{limit: limit} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailBuffer) contains(sub string) bool {
	return strings.Contains(string(t.buf), sub)
}

// lineContaining returns the trimmed line that carries sub, or the whole tail
// when no newline-delimited line does.
func (t *tailBuffer) lineContaining(sub string) string {
	for _, line := range strings.Split(string(t.buf), "\n") {
		if strings.Contains(line, sub) {
			return strings.TrimSpace(line)
		}
	}
	return strings.TrimSpace(string(t.buf))
}

// stdoutClose closes the parent's handle to the log file once the child exits.
func (p *liveProcess) stdoutClose() error {
	if p.outFile == nil {
		return nil
	}
	err := p.outFile.Close()
	p.outFile = nil
	return err
}

// renderSessionConfig renders the nsjail config for an interactive session.
// When req.PTY is set the helper source is the entry command, passed to python3
// inline (`python3 -c "<source>" <config-json>`) so no engine-named file is
// materialized in the sandbox; its JSON argv carries the shell and initial
// window size, and the requested cwd is applied by nsjail.
func (r *NsjailRunner) renderSessionConfig(req Request, runTmp string) (string, error) {
	argv := req.Argv
	if req.PTY {
		cfgJSON, err := json.Marshal(sessionHelperConfig{Shell: req.Shell, Cols: req.Cols, Rows: req.Rows})
		if err != nil {
			return "", err
		}
		// The PTY relay is a python3 program, so the workspace's PROFILE must
		// provide python3. Resolve it profile-aware (the jail path), and fail
		// with an actionable diagnostic when the profile does not offer it --
		// a session on a shell-only profile is a real, explained limitation,
		// never a mysterious nsjail execve error.
		pf, perr := r.profileFor(req.Workspace)
		if perr != nil {
			return "", perr
		}
		py, err := r.resolveProfileCommand(pf, "python3")
		if err != nil {
			return "", fmt.Errorf("%w: profile %q does not provide python3", ErrSessionNeedsInterpreter, pf.Name)
		}
		// The relay source is passed as the argument to `-c`; python executes
		// it without ever reading a file named anything, so there is no relay
		// source on disk for a tenant to read.
		argv = []string{py, "-c", string(PTYRelayScript), string(cfgJSON)}
	}
	clone := req
	clone.Argv = argv
	return r.renderConfig(clone, runTmp)
}

type sessionHelperConfig struct {
	Shell string `json:"shell"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
}
