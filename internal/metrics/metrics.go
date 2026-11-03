// Package metrics implements the per-workspace resource stream (RAM + disk).
//
// The design is driven by MEASUREMENT, not intuition (docs/DECISIONS.md ADR 93):
//
//   - disk comes from statfs (O(1), ~1.7 µs), never a tree walk (153 µs/file,
//     82x worse per file and O(files));
//   - a workspace's RAM is the SUM over its per-run and per-session cgroups,
//     because the cgroup is per-run/per-session, not per-workspace;
//   - ONE service-wide sampler goroutine starts lazily on the first subscriber
//     and stops on the last, so a service nobody watches costs exactly zero;
//   - it samples ONCE per tick and fans out, so cost is independent of the
//     number of subscribers;
//   - idle workspaces (no live cgroup) cost zero cgroup reads, only one statfs;
//   - each subscriber holds at most ONE pending frame (coalesce or drop), so a
//     slow client can never turn into unbounded engine memory.
//
// A subscriber is SCOPED: a service-wide stream carries only the workspaces its
// principal owns, and a per-workspace stream carries exactly one workspace, so
// the shared sampler never leaks one owner's data to another.
//
// The package is pure with respect to the outside world: it reads through two
// narrow interfaces (Provider and Reader) that the service and a test supply,
// so the whole thing is unit-testable without nsjail, cgroups or a real disk.
package metrics

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// WorkspaceInfo is one workspace the sampler may sample, with the live cgroup
// directories it currently owns. RunCgroups and SessionCgroups are the ONLY
// place RAM is read from; a workspace with neither reports RAM 0 and costs zero
// cgroup file reads.
type WorkspaceInfo struct {
	ID               string
	OwnerID          string
	DiskRoot         string
	DiskLimitBytes   int64
	MemoryLimitBytes int64
	RunCgroups       []string
	SessionCgroups   []string
}

// Provider supplies the sampler with the candidate set each tick. It is
// implemented by the service, which owns the workspace inventory and the
// executor's existing run/cgroup tracking (never a /sys/fs/cgroup scan). The
// cgroup list is O(live) and read from in-memory maps; the workspace inventory
// is cached behind a slower reconcile inside the provider.
type Provider interface {
	Candidates() []WorkspaceInfo
}

// Reader reads the cheap runtime sources. The production implementation reads
// /sys/fs/cgroup and statfs; a test injects deterministic values so the sum,
// the parsing and the idle elision are unit-tested without a live cgroup.
type Reader interface {
	// MemoryCurrent reads a cgroup's memory.current (bytes).
	MemoryCurrent(dir string) (int64, error)
	// MemoryMax reads a cgroup's memory.max (bytes).
	MemoryMax(dir string) (int64, error)
	// CPUUsageUsec reads a cgroup's cpu.stat usage_usec.
	CPUUsageUsec(dir string) (int64, error)
	// PidsCurrent reads a cgroup's pids.current.
	PidsCurrent(dir string) (int64, error)
	// DiskUsed returns the used bytes of a live mounted workspace filesystem
	// and whether the path was a live mount (ok=false otherwise).
	DiskUsed(root string) (used int64, ok bool)
}

// WorkspaceSnapshot is one workspace's frame.
type WorkspaceSnapshot struct {
	WorkspaceID string `json:"workspace_id"`
	// ownerID is the workspace's owner. It is NOT serialized: the shared frame
	// never carries another owner's id. It exists only so the sampler can filter
	// one shared sample pass per subscriber scope.
	ownerID string
	// RAMUsedBytes is the SUM of memory.current over the live run AND session
	// cgroups. Reading one cgroup would be a wrong number, not an
	// approximation.
	RAMUsedBytes int64 `json:"ram_used_bytes"`
	// RAMLimitBytes is the workspace's configured allowance (memory_mb). It is
	// what the tenant is advertised, and it is stable whether or not a cgroup
	// is live.
	RAMLimitBytes int64 `json:"ram_limit_bytes"`
	// RAMCgroupLimitBytes is the SUM of the live cgroups' memory.max (the
	// ceiling actually enforced). It can exceed RAMLimitBytes because the
	// cgroup is per-run: a workspace with N runs may consume N x memory_mb.
	RAMCgroupLimitBytes int64   `json:"ram_cgroup_limit_bytes"`
	RAMPercent          float64 `json:"ram_percent"`
	// RAMComplete is false when a cgroup read did not succeed, so the figure is
	// a lower bound.
	RAMComplete bool `json:"ram_complete"`

	DiskUsedBytes  int64   `json:"disk_used_bytes"`
	DiskLimitBytes int64   `json:"disk_limit_bytes"`
	DiskPercent    float64 `json:"disk_percent"`
	// DiskComplete is false when the workspace root was not a live mount, so
	// DiskUsedBytes (0) is unknown rather than measured.
	DiskComplete bool `json:"disk_complete"`

	LiveRuns    int `json:"live_runs"`
	LiveCgroups int `json:"live_cgroups"`

	// CPUUsageUsec and PidsCurrent are the opt-in extras (include_cpu). They are
	// the most expensive files, so they are omitted unless asked for.
	CPUUsageUsec int64 `json:"cpu_usage_usec,omitempty"`
	PidsCurrent  int64 `json:"pids_current,omitempty"`
}

// Snapshot is one tick's frame (or a one-shot response).
type Snapshot struct {
	Timestamp       time.Time           `json:"timestamp"`
	IntervalSeconds float64             `json:"interval_seconds"`
	Workspaces      []WorkspaceSnapshot `json:"workspaces"`
	Sampler         Counters            `json:"sampler"`
}

// Counters exposes the sampler's own work so the cost claims are observable and
// a gate can assert them (idle elision, fan-out, coalescing, lazy start).
type Counters struct {
	// Ticks counts sample passes. It does not advance while no subscriber is
	// connected (the lazy-start guarantee).
	Ticks int64 `json:"ticks"`
	// CgroupReads counts cgroup file reads (memory.current + memory.max, plus
	// cpu.stat + pids.current when include_cpu).
	CgroupReads int64 `json:"cgroup_reads"`
	// StatfsReads counts statfs calls (one per candidate workspace per tick).
	StatfsReads int64 `json:"statfs_reads"`
	// TreeWalkReads is a CONTRACT GUARD and is always 0: there is no code path
	// that walks a workspace directory tree, because the disk figure is a single
	// statfs (O(1)). It exists so a future change that reintroduced a walk on the
	// sampling path would show a non-zero counter and be caught.
	TreeWalkReads int64 `json:"tree_walk_reads"`
	// Subscribers is the current number of connected streams.
	Subscribers int64 `json:"subscribers"`
	// Running is 1 while the sampler goroutine is alive (at least one
	// subscriber is connected), and 0 otherwise. It is the unambiguous
	// lazy-start/disconnect signal: with no subscribers the ticker does not
	// exist and Ticks is frozen.
	Running int64 `json:"running"`
	// DroppedFrames counts frames coalesced away because a subscriber had not
	// read its previous frame. It proves coalescing rather than buffering.
	DroppedFrames int64 `json:"dropped_frames"`
	// Candidates is the number of workspaces considered this tick.
	Candidates int `json:"candidates"`
}

// Config is the sampler configuration. The interval is clamped to
// [MinInterval, MaxInterval] at construction, so no caller can drive the
// service to an unsafe cadence.
type Config struct {
	Interval          time.Duration
	MinInterval       time.Duration
	MaxInterval       time.Duration
	ReconcileInterval time.Duration
	IncludeCPU        bool
	MaxSubscribers    int
}

// DefaultConfig returns the safe defaults (3s cadence, 1s floor).
func DefaultConfig() Config {
	return Config{
		Interval:          3 * time.Second,
		MinInterval:       time.Second,
		MaxInterval:       300 * time.Second,
		ReconcileInterval: 15 * time.Second,
		IncludeCPU:        false,
		MaxSubscribers:    1024,
	}
}

// ClampInterval clamps d into [min, max]. A non-positive min yields the 1s
// default floor; a non-positive max is treated as unbounded.
func ClampInterval(d, min, max time.Duration) time.Duration {
	if min <= 0 {
		min = time.Second
	}
	if max <= 0 {
		max = time.Duration(1) << 62
	}
	if max < min {
		max = min
	}
	if d < min {
		return min
	}
	if d > max {
		return max
	}
	return d
}

// Percent returns used/limit*100, or 0 when the limit is not positive. It never
// divides by zero and never returns NaN/Inf: a percentage the numbers cannot
// support is not published.
func Percent(used, limit int64) float64 {
	if limit <= 0 {
		return 0
	}
	p := float64(used) / float64(limit) * 100
	if math.IsNaN(p) || math.IsInf(p, 0) {
		return 0
	}
	if p < 0 {
		return 0
	}
	return p
}

// parseBytes parses a cgroup integer file value (a decimal byte count, possibly
// with a trailing newline).
func parseBytes(b []byte) (int64, error) {
	s := strings.TrimSpace(string(b))
	return strconv.ParseInt(s, 10, 64)
}

// parseStatUsec extracts the usage_usec field from a cpu.stat payload. A
// missing field is 0 (not an error): a cgroup without cpu accounting reports no
// usage rather than failing the whole tick.
func parseStatUsec(b []byte) (int64, error) {
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return 0, nil
}

// BuildWorkspaceSnapshot assembles one workspace's frame from already-read
// facts. It is pure: the caller supplies the per-cgroup readings so the sum and
// the percentage arithmetic are unit-testable in isolation.
func BuildWorkspaceSnapshot(info WorkspaceInfo, diskUsed, diskLimit int64, diskOK bool, ramUsed, ramCgroupMax int64, ramOK bool, liveRuns, liveCgroups int, cpuUsec, pids int64) WorkspaceSnapshot {
	return WorkspaceSnapshot{
		WorkspaceID:         info.ID,
		ownerID:             info.OwnerID,
		RAMUsedBytes:        ramUsed,
		RAMLimitBytes:       info.MemoryLimitBytes,
		RAMCgroupLimitBytes: ramCgroupMax,
		RAMPercent:          Percent(ramUsed, info.MemoryLimitBytes),
		RAMComplete:         ramOK,
		DiskUsedBytes:       diskUsed,
		DiskLimitBytes:      diskLimit,
		DiskPercent:         Percent(diskUsed, diskLimit),
		DiskComplete:        diskOK,
		LiveRuns:            liveRuns,
		LiveCgroups:         liveCgroups,
		CPUUsageUsec:        cpuUsec,
		PidsCurrent:         pids,
	}
}
