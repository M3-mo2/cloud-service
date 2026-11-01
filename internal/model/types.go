// Package model defines the domain types shared across sandboxd.
package model

import (
	"time"

	"github.com/oklog/ulid/v2"
)

// Version is the service version string.
const Version = "0.0.5"

// APIVersion is the HTTP API version prefix.
const APIVersion = "v1"

// ID prefixes.
const (
	WorkspaceIDPrefix = "ws_"
	RunIDPrefix       = "run_"
	SessionIDPrefix   = "ses_"
)

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
}

// SetExitCode records an exit code.
func (r *Run) SetExitCode(code int) { r.ExitCode = &code }

// SessionStatus is the lifecycle state of a terminal session.
type SessionStatus string

// Session statuses. A session always reaches exactly one terminal state.
const (
	SessionRunning  SessionStatus = "running"
	SessionExited   SessionStatus = "exited"
	SessionKilled   SessionStatus = "killed"
	SessionTimedOut SessionStatus = "timed_out"
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
	SessionReasonSandboxUnavailable = "sandbox_unavailable"
)

// SetExitCode records a session exit code.
func (s *Session) SetExitCode(code int) { s.ExitCode = &code }
