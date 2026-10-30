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
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = execHardCapSec
	}
	if timeout > execHardCapSec {
		timeout = execHardCapSec
	}
	caps := s.cfg.Caps
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
