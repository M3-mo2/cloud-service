package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

	// Materialize the PTY relay helper. It lives in the session's /tmp
	// bind-mount, so it appears at /run/sandboxd/pty_relay.py inside the jail.
	// It runs with the same identity, mounts and seccomp policy as any workload
	// -- the sandbox is not weakened for sessions.
	helperPath := ""
	if req.PTY {
		helperPath = filepath.Join(runTmp, "pty_relay.py")
		if err := os.WriteFile(helperPath, PTYRelayScript, 0o644); err != nil {
			os.RemoveAll(runTmp)
			return nil, fmt.Errorf("write pty relay helper: %w", err)
		}
		if err := r.identity.ChownToSandbox(helperPath); err != nil {
			os.RemoveAll(runTmp)
			return nil, fmt.Errorf("prepare pty relay ownership: %w", err)
		}
	}

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

	cmd := exec.Command(bin, "-q", "-L", "3", "--config", cfgPath)
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
	// client's byte stream.
	cmd.Stderr = outW

	if err := cmd.Start(); err != nil {
		outF.Close()
		closeWriteEnd(cmd)
		<-diagDone
		cleanupSessionArtifacts(cfgPath, runTmp)
		return nil, err
	}
	closeWriteEnd(cmd)

	lp := &liveProcess{
		cmd:       cmd,
		stdin:     stdin,
		out:       outW,
		outFile:   outF,
		runTmp:    runTmp,
		cfgPath:   cfgPath,
		diagDone:  diagDone,
		killGrace: r.cfg.Sandbox.KillGrace(),
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

// Kill terminates the process group.
func (p *liveProcess) Kill() bool {
	if p.cmd.Process == nil {
		return false
	}
	return killProcessGroup(p.cmd.Process.Pid, p.killGrace)
}

// Wait blocks for the process to exit and classifies the outcome. Artifacts
// (the rendered config and the per-session tmp) are removed once the process is
// gone, so a session leaves nothing behind.
func (p *liveProcess) Wait() (Result, error) {
	waitErr := p.cmd.Wait()
	diagnostic := waitDiagnostic(p.diagDone)
	_ = p.stdoutClose()
	cleanupSessionArtifacts(p.cfgPath, p.runTmp)

	res := Result{Streamed: true, Truncated: p.out.truncated}
	// Reuse Run's classification: a signalled exit is a kill, an nsjail exit
	// 255 with a fatal diagnostic is a sandbox launch failure.
	if err := (&NsjailRunner{}).classify(&res, waitErr, context.Background(), diagnostic); err != nil {
		return res, err
	}
	return res, nil
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
// When req.PTY is set the helper is the entry command and its JSON argv carries
// the shell and initial window size; the requested cwd is applied by nsjail.
func (r *NsjailRunner) renderSessionConfig(req Request, runTmp string) (string, error) {
	argv := req.Argv
	if req.PTY {
		cfgJSON, err := json.Marshal(sessionHelperConfig{Shell: req.Shell, Cols: req.Cols, Rows: req.Rows})
		if err != nil {
			return "", err
		}
		// nsjail's exec_bin.path goes straight to execve (no PATH lookup), so
		// resolve the interpreter on the host now. The host /usr is mounted at
		// the same path inside the jail.
		py, err := resolveCommand("python3")
		if err != nil {
			return "", fmt.Errorf("pty helper needs python3: %w", err)
		}
		argv = []string{py, "/run/sandboxd/pty_relay.py", string(cfgJSON)}
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
