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

// ErrMaskUnavailable is returned when host-identity masking is enabled but the
// shim cannot be applied to a sandbox. The engine refuses the run rather than
// launch a sandbox that would leak the host's identity: a missing mask is a
// loud failure, never a silent leak.
var ErrMaskUnavailable = errors.New("host-identity masking unavailable")

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

// PTYErrorMarker is the prefix the PTY relay helper writes to its stderr when it
// cannot allocate a PTY inside the sandbox (typically because /dev/ptmx or
// devpts was not mounted). It lets the executor recognise a terminal-session
// startup failure and surface it as a failed session rather than a running one
// whose output is a Python traceback.
const PTYErrorMarker = "sandboxd-pty-error:"

// Request describes a single execution inside a workspace sandbox.
type Request struct {
	Workspace     *model.Workspace
	WorkspaceRoot string
	Argv          []string
	Cwd           string
	Env           map[string]string
	Stdin         string
	Timeout       time.Duration
	// NoTimeout, when set, requests a wall-clock-unbounded run: nsjail is
	// rendered with `time_limit: 0` (no limit) and no timeout context is
	// layered. A run then ends only when it exits or its context is cancelled
	// (an explicit stop). Timeout is ignored when NoTimeout is set.
	NoTimeout bool

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

	// SharedPackagesDir, when non-empty, is a host directory mounted read-only
	// into the sandbox as the shared package tier. A missing/unreadable dir is
	// resolved by the service before the request reaches the runner.
	SharedPackagesDir string
	// PrivatePackagesDir, when non-empty, is the workspace's own writable
	// package directory, mounted read-write into the sandbox as the private
	// tier. It is never the shared, read-only location.
	PrivatePackagesDir string

	// PTY, when set, runs an interactive terminal session: the PTY relay helper
	// becomes the entry command, stdin stays an open pipe the caller owns, and
	// Shell/Cols/Rows configure the shell started on the PTY.
	PTY   bool
	Shell string
	Cols  int
	Rows  int

	// NetnsPath, when set, is the path of the workspace's engine-managed
	// private network namespace (/run/netns/...). The runner enters it before
	// exec'ing nsjail and renders `clone_newnet: false`, so the workload stays
	// in the workspace's own namespace. When empty, nsjail creates a fresh
	// empty network namespace (never the host's).
	NetnsPath string
	// ResolverConf, when set, is a host path bind-mounted read-only at
	// /etc/resolv.conf so an allow_net workspace can resolve names. It names
	// public resolvers, never the host's: the workspace reaches them through
	// the chained slirp4netns egress. Empty renders no resolver mount.
	ResolverConf string

	// ProcessKind / ProcessSource tag the sandbox with the identity of the work
	// it belongs to ("run"/"session"/"exec" and the run/session id). The tag is
	// inherited by every descendant, so the process inventory can attribute a
	// detached process back to its owning workspace and source even when it left
	// its process group. Empty values render no tag.
	ProcessKind   string
	ProcessSource string
	// SessionOutputCap bounds the persisted PTY output log. It overrides the
	// default run-log cap for a session.
	SessionOutputCap int64
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
	// MaskStatus reports host-identity masking truthfully: enabled is the
	// configured switch, ready is whether the shim can be applied to a sandbox,
	// and reason explains a not-ready state. A run is refused when masking is
	// enabled but not ready.
	MaskStatus() (enabled bool, ready bool, reason string)
	// Run executes a request to completion.
	Run(req Request) (Result, error)
	// Start launches a long-lived interactive process (e.g. a PTY session) and
	// returns a handle whose stdin the caller owns. Run is for one-shot
	// execution and is unaffected.
	Start(req Request) (LiveProcess, error)
	// Kill terminates a running process tree by run ID. It returns true if a
	// live process was signalled.
	Kill(runID string) bool
	// ListProcesses returns every live process currently inside a sandbox
	// (nsjail) cgroup on the host, with its attribution tags. The service
	// filters by workspace. An unreadable process is omitted, never guessed.
	ListProcesses() []ProcessInfo
	// KillProcessTree stops the process tree a host pid belongs to: the pid's
	// sandbox cgroup when it is jailed (so a process that escaped its process
	// group with setsid() is still caught), else its process group. It returns
	// true when something was signalled.
	KillProcessTree(pid int) bool
	// ReapOrphans kills and removes every orphaned sandbox cgroup left by a hard
	// (SIGKILL) daemon death, and reports how many it reaped.
	ReapOrphans() int
}
