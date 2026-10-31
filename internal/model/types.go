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
)

// NewID returns a ULID string with the given prefix.
func NewID(prefix string) string {
	return prefix + ulid.Make().String()
}

// NewWorkspaceID returns a new workspace identifier.
func NewWorkspaceID() string { return NewID(WorkspaceIDPrefix) }

// NewRunID returns a new run identifier.
func NewRunID() string { return NewID(RunIDPrefix) }

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
