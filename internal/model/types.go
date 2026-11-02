// Package model defines the domain types shared across sandboxd.
package model

import (
	"time"

	"github.com/oklog/ulid/v2"
)

// Version is the service version string. It tracks the highest feature group
// implemented in docs/DECISIONS.md (v0.0.15); it is independent of APIVersion,
// which is the HTTP contract prefix and changes only on a breaking API change.
const Version = "0.0.15"

// APIVersion is the HTTP API version prefix.
const APIVersion = "v1"

// ID prefixes.
const (
	WorkspaceIDPrefix = "ws_"
	RunIDPrefix       = "run_"
	SessionIDPrefix   = "ses_"
	TunnelIDPrefix    = "tun_"
)

// NewTunnelID returns a new tunnel identifier.
func NewTunnelID() string { return NewID(TunnelIDPrefix) }

// NewID returns a ULID string with the given prefix.
func NewID(prefix string) string {
	return prefix + ulid.Make().String()
}

// NewWorkspaceID returns a new workspace identifier.
func NewWorkspaceID() string { return NewID(WorkspaceIDPrefix) }

// NewRunID returns a new run identifier.
func NewRunID() string { return NewID(RunIDPrefix) }

// NewSessionID returns a new terminal-session identifier.
func NewSessionID() string { return NewID(SessionIDPrefix) }

// Limits are per-workspace resource limits.
type Limits struct {
	CPUCores       float64 `json:"cpu_cores"`
	MemoryMB       int     `json:"memory_mb"`
	DiskMB         int     `json:"disk_mb"`
	MaxWallTimeSec int     `json:"max_wall_time_sec"`
	MaxRuns        int     `json:"max_runs"`
	AllowNet       bool    `json:"allow_net"`
	// AllowUnlimitedRuns opts a workspace in to wall-clock-unbounded runs
	// (`no_timeout:true`). It is false by default. Both this workspace flag and
	// the server-level max_limits.allow_unlimited_runs must be true for an
	// unlimited run to be accepted; otherwise the request is refused with 422.
	AllowUnlimitedRuns bool `json:"allow_unlimited_runs"`
	// AllowTunnels opts a workspace in to quick Cloudflare tunnels. It is false
	// by default. Both this workspace flag and the server-level tunnels.enabled
	// must be true for POST /v1/workspaces/{id}/tunnels to create a tunnel;
	// otherwise the request is refused with 422. A tunnel additionally requires
	// allow_net, because cloudflared needs egress to reach Cloudflare. It
	// mirrors the AllowUnlimitedRuns two-key pattern.
	AllowTunnels bool `json:"allow_tunnels"`
}

// Workspace statuses.
const (
	WorkspaceActive    = "active"
	WorkspaceSuspended = "suspended"
	WorkspaceDeleted   = "deleted"
)

// Workspace is a named, owned container of files and runs.
type Workspace struct {
	ID             string    `json:"id"`
	OwnerID        string    `json:"owner_id"`
	Name           string    `json:"name"`
	CreatedAt      time.Time `json:"created_at"`
	Status         string    `json:"status"`
	Limits         Limits    `json:"limits"`
	LastActivityAt time.Time `json:"last_activity_at"`
}

// RunKind describes how a run's command was specified.
type RunKind string

// Run kinds.
const (
	RunKindFile    RunKind = "file"
	RunKindCommand RunKind = "command"
)

// RunStatus is the lifecycle state of a run.
type RunStatus string

// Run statuses.
const (
	RunQueued      RunStatus = "queued"
	RunRunning     RunStatus = "running"
	RunExited      RunStatus = "exited"
	RunKilled      RunStatus = "killed"
	RunTimeout     RunStatus = "timeout"
	RunFailed      RunStatus = "failed"
	RunInterrupted RunStatus = "interrupted"
)

// Log streams.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// RunLogs captures log byte counts for a run.
type RunLogs struct {
	StdoutBytes int64 `json:"stdout_bytes"`
	StderrBytes int64 `json:"stderr_bytes"`
	Truncated   bool  `json:"truncated"`
}

// Run is a single asynchronous execution.
type Run struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Kind        RunKind   `json:"kind"`
	Argv        []string  `json:"argv"`
	Status      RunStatus `json:"status"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	Logs        RunLogs   `json:"logs"`

	// Cwd is the working directory, relative to the workspace root (always
	// leading-slash normalized; "/" is the workspace root). It is a working
	// directory only: it never changes how File is resolved, which is always
	// relative to the workspace root.
	Cwd string `json:"cwd"`
	// TimeoutSec is the effective wall-clock limit in seconds, or null when
	// Unlimited is true. It lets a reader tell "no limit" (null) apart from a
	// concrete number; an unlimited run is never recorded with a fake finite
	// value.
	TimeoutSec *int `json:"timeout_sec"`
	// Unlimited is true when the run has no wall-clock limit (it runs until it
	// exits or is stopped). It is distinct from a finite timeout.
	Unlimited bool `json:"unlimited"`
	// Interpreter, for a kind=file run, is the interpreter used to run File.
	// Empty for a kind=command run or a file run performed with raw argv.
	Interpreter string `json:"interpreter,omitempty"`
}

// SetExitCode records an exit code.
func (r *Run) SetExitCode(code int) { r.ExitCode = &code }

// SetTimeoutSec records a finite wall-clock limit.
func (r *Run) SetTimeoutSec(sec int) { r.TimeoutSec = &sec; r.Unlimited = false }

// SessionStatus is the lifecycle state of a terminal session.
type SessionStatus string

// Session statuses. A session always reaches exactly one terminal state.
const (
	SessionRunning  SessionStatus = "running"
	SessionExited   SessionStatus = "exited"
	SessionKilled   SessionStatus = "killed"
	SessionTimedOut SessionStatus = "timed_out"
	// SessionFailed is a session that could not start: the sandbox could not be
	// launched, or the PTY relay could not allocate a terminal. It carries a
	// Diagnostic so a client can act on the cause instead of reading a Python
	// traceback out of the output stream.
	SessionFailed SessionStatus = "failed"
)

// Terminal reports whether the status is final.
func (s SessionStatus) Terminal() bool { return s != SessionRunning }

// Session shell size bounds. A value outside these bounds is rejected with 422.
const (
	MinSessionCols = 1
	MaxSessionCols = 1000
	MinSessionRows = 1
	MaxSessionRows = 1000
)

// Session is an interactive terminal hosted inside a workspace sandbox. Its
// output is streamed to a capped log file (reusing the run-log mechanism) that
// clients poll with offset/limit; input, signals and resizes are relayed to a
// PTY inside the sandbox by the executor's PTY helper.
type Session struct {
	ID          string        `json:"id"`
	WorkspaceID string        `json:"workspace_id"`
	Shell       string        `json:"shell"`
	Cwd         string        `json:"cwd"`
	Cols        int           `json:"cols"`
	Rows        int           `json:"rows"`
	Status      SessionStatus `json:"status"`
	CreatedAt   time.Time     `json:"created_at"`
	// LastActivityAt is refreshed on create, input, signal, resize and output
	// growth; the idle-timeout reaper compares it against the configured idle
	// duration.
	LastActivityAt time.Time `json:"last_activity_at"`
	StartedAt      time.Time `json:"started_at"`
	EndedAt        time.Time `json:"ended_at"`
	// ExitCode is the shell's exit code for a terminal session, when known.
	ExitCode *int `json:"exit_code,omitempty"`
	// Reason names the cause of a terminal transition (e.g. "user_closed",
	// "idle_timeout", "lifetime", "shell_exited", "workspace_deleted",
	// "service_shutdown", "interrupted"). It is empty while running.
	Reason string `json:"reason,omitempty"`
	// Diagnostic is set for a failed session: the actionable cause of the
	// failure (e.g. a missing PTY or a sandbox launch error). Empty otherwise.
	Diagnostic string `json:"diagnostic,omitempty"`
	// OutputBytes is the number of bytes of PTY output persisted to the log.
	OutputBytes int64 `json:"output_bytes"`
	// OutputTruncated is set when the output cap (caps.run_log_mb) was hit.
	OutputTruncated bool `json:"output_truncated"`
}

// SessionReasons names the documented terminal reasons.
const (
	SessionReasonUserClosed         = "user_closed"
	SessionReasonShellExited        = "shell_exited"
	SessionReasonIdleTimeout        = "idle_timeout"
	SessionReasonLifetime           = "lifetime"
	SessionReasonWorkspaceDeleted   = "workspace_deleted"
	SessionReasonServiceShutdown    = "service_shutdown"
	SessionReasonInterrupted        = "interrupted"
	SessionReasonWorkspaceSuspended = "workspace_suspended"
	SessionReasonWorkspaceStopped   = "workspace_stopped"
	SessionReasonSandboxUnavailable = "sandbox_unavailable"
)

// SetExitCode records a session exit code.
func (s *Session) SetExitCode(code int) { s.ExitCode = &code }

// Tunnel statuses. A tunnel is `active` while its cloudflared process is
// running and a public URL was observed; `stopped` once it has been deleted.
// The URL is only ever recorded from cloudflared's own output, never
// fabricated.
const (
	TunnelActive  = "active"
	TunnelStopped = "stopped"
)

// Tunnel is an engine-managed quick Cloudflare tunnel exposing one port inside
// a workspace's own network namespace to the public internet. The cloudflared
// process runs INSIDE the workspace's namespace and reaches Cloudflare over the
// chained slirp4netns egress, so no host interface, route or listener is used.
//
// URL is captured from cloudflared's own output (a random
// https://<name>.trycloudflare.com address) after a bounded wait; a tunnel is
// never recorded with a URL the engine did not observe.
type Tunnel struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	OwnerID     string `json:"owner_id"`
	// Port is the port the workspace listens on inside its own network
	// namespace, which cloudflared connects to at localhost:Port.
	Port int `json:"port"`
	// URL is the public https://<name>.trycloudflare.com address cloudflared
	// printed. It is empty until observed.
	URL string `json:"url"`
	// Status is TunnelActive or TunnelStopped.
	Status string `json:"status"`
	// PID is the host pid of the cloudflared process (engine-owned). It is not
	// part of the API response and is used to stop the exact process.
	PID       int       `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
