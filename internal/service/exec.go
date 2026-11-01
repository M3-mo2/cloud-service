package service

import (
	"time"

	"sandboxd/internal/executor"
	"sandboxd/internal/model"
)

// ExecSpec is a synchronous exec request.
type ExecSpec struct {
	Cmd     []string
	Cwd     string
	Env     map[string]string
	Timeout int
	Stdin   string
}

// ExecOutcome is the result of a synchronous exec.
type ExecOutcome struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	DurationMS int64
	Truncated  bool
	TimedOut   bool
}

const execHardCapSec = 60

// Exec runs a command synchronously with a hard timeout cap.
func (s *Service) Exec(w *model.Workspace, spec ExecSpec) (ExecOutcome, error) {
	if err := s.ensureActive(w); err != nil {
		return ExecOutcome{}, err
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = execHardCapSec
	}
	if timeout > execHardCapSec {
		timeout = execHardCapSec
	}
	caps := s.cfg.Caps
	sharedHost, privateHost, err := s.packageDirs(w)
	if err != nil {
		return ExecOutcome{}, err
	}
	req := executor.Request{
		Workspace:     w,
		WorkspaceRoot: s.store.WorkspaceRoot(w.ID),
		Argv:          spec.Cmd,
		Cwd:           spec.Cwd,
		Env:           spec.Env,
		Stdin:         spec.Stdin,
		Timeout:       time.Duration(timeout) * time.Second,
		StdoutCap:     caps.ExecStdoutBytes(),
		StderrCap:     caps.ExecStderrBytes(),

		// Tag the sandbox so a process a synchronous exec leaves behind (a
		// detached child) is still attributed to this workspace. Exec has no
		// run/session id, so such a process is reported as "detached".
		ProcessKind: executor.KindExec,

		SharedPackagesDir:  sharedHost,
		PrivatePackagesDir: privateHost,
	}
	// Give the exec the workspace's own isolated network. A setup failure is
	// returned (never a silent fall back to sharing the host network).
	req, err = s.ApplyNetwork(w, req)
	if err != nil {
		return ExecOutcome{}, err
	}
	start := time.Now()
	res, err := s.runner.Run(req)
	if err != nil {
		return ExecOutcome{}, err
	}
	return ExecOutcome{
		ExitCode:   res.ExitCode,
		Stdout:     string(res.Stdout),
		Stderr:     string(res.Stderr),
		DurationMS: time.Since(start).Milliseconds(),
		Truncated:  res.Truncated,
		TimedOut:   res.TimedOut,
	}, nil
}
