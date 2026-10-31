// Package executor builds nsjail configurations and manages sandboxed process
// lifecycles, including process-group termination and capped log capture.
package executor

import (
	"context"
	"errors"
	"time"

	"sandboxd/internal/model"
)

// ErrNsjailUnavailable is returned when the nsjail binary cannot be found.
var ErrNsjailUnavailable = errors.New("nsjail binary not available")

// ErrSandboxUnavailable is returned when nsjail starts but fails to set up or
// launch the sandbox (e.g. cgroup v2 is not usable, namespace creation is
// denied). It wraps nsjail's own diagnostic so a launch failure is never
// surfaced as a silent non-zero exit code.
var ErrSandboxUnavailable = errors.New("sandbox unavailable")

// ErrInvalidCwd is returned when a requested working directory would escape the
// workspace root.
var ErrInvalidCwd = errors.New("cwd escapes workspace root")

// SandboxError reports that nsjail could not set up or launch the sandbox.
// Diagnostic carries the relevant nsjail log lines.
type SandboxError struct {
	Diagnostic string
}

func (e *SandboxError) Error() string {
	if e.Diagnostic == "" {
		return "sandbox unavailable"
	}
	return "sandbox unavailable: " + e.Diagnostic
}

// Is lets errors.Is(err, ErrSandboxUnavailable) match a *SandboxError.
func (e *SandboxError) Is(target error) bool { return target == ErrSandboxUnavailable }

// Request describes a single execution inside a workspace sandbox.
type Request struct {
	Workspace     *model.Workspace
	WorkspaceRoot string
	Argv          []string
	Cwd           string
	Env           map[string]string
	Stdin         string
	Timeout       time.Duration

	// Context, when non-nil, dictates cancellation of the process tree. A
	// cancelled context is classified as a kill (e.g. explicit stop).
	Context context.Context

	// RunID, when non-empty, indicates an async run whose logs must be streamed
	// to StdoutPath/StderrPath with the given caps.
	RunID      string
	StdoutPath string
	StderrPath string
	StdoutCap  int64
	StderrCap  int64
}

// Result is the outcome of an execution.
type Result struct {
	ExitCode  int
	Stdout    []byte
	Stderr    []byte
	Truncated bool
	TimedOut  bool
	Killed    bool
	// Streamed reports that output was written to disk rather than returned.
	Streamed bool
}

// Runner abstracts process execution so the service can be tested without nsjail.
type Runner interface {
	// Available reports whether the sandbox backend is usable.
	Available() bool
	// CgroupsUsable reports whether cgroup v2 ceilings can be applied. When
	// false, nsjail runs without cgroup memory/pids/cpu limits.
	CgroupsUsable() bool
	// Run executes a request to completion.
	Run(req Request) (Result, error)
	// Kill terminates a running process tree by run ID. It returns true if a
	// live process was signalled.
	Kill(runID string) bool
}
