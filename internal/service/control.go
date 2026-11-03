package service

import (
	"fmt"
	"strings"
	"time"

	"sandboxd/internal/model"
)

// MaxNameLen caps a workspace name. It mirrors the create-time validation
// (`max=128`) so PATCH cannot grow a name past what creation allows.
const MaxNameLen = 128

// WorkspaceSuspendedError is returned when work is requested for a suspended
// workspace. It is a policy refusal (409), distinct from an ownership or
// not-found condition.
func WorkspaceSuspendedError() error {
	return model.New(409, model.CodeWorkspaceSuspended, "workspace is suspended")
}

// SubscriptionExpiredError is returned (403) when work is requested for a
// workspace whose subscription term has lapsed. It is deliberately distinct
// from the 409 workspace_suspended refusal: a lock is the automatic result of a
// lapsed term and the way out is a renewal (POST .../subscription/extend), not
// resume. Reads, the summary, usage, file reads and manual delete stay allowed.
func SubscriptionExpiredError() error {
	return model.New(403, model.CodeSubscriptionExpired,
		"workspace subscription has lapsed (locked); renew to restore service")
}

// LimitUpdate is a partial update of a workspace's limits. A nil field keeps
// the current value; this is why every field is a pointer (so an omitted JSON
// field and an explicit null both leave the value untouched).
type LimitUpdate struct {
	CPUCores           *float64
	MemoryMB           *int
	DiskMB             *int
	MaxWallTimeSec     *int
	MaxRuns            *int
	AllowNet           *bool
	AllowUnlimitedRuns *bool
	AllowTunnels       *bool
}

// WorkspaceUpdate is a partial update of a workspace.
type WorkspaceUpdate struct {
	Name   *string
	Limits *LimitUpdate
}

// maxLimits returns the server-level maxima as a Limits value.
func (s *Service) maxLimits() model.Limits {
	m := s.cfg.MaxLimits
	return model.Limits{
		CPUCores:           m.CPUCores,
		MemoryMB:           m.MemoryMB,
		DiskMB:             m.DiskMB,
		MaxWallTimeSec:     m.MaxWallTimeSec,
		MaxRuns:            m.MaxRuns,
		AllowNet:           m.AllowNet,
		AllowUnlimitedRuns: m.AllowUnlimitedRuns,
	}
}

// UpdateWorkspace applies a partial update to a workspace without recreating
// it. Absent fields keep their current value. A numeric limit above the
// server maximum is rejected with 422 and nothing is persisted. Lowering a
// limit takes effect for the next run/exec; a run already in flight is not
// retroactively killed (see docs/DECISIONS.md).
func (s *Service) UpdateWorkspace(id string, upd WorkspaceUpdate) (*model.Workspace, error) {
	max := s.maxLimits()
	var apiErr error
	w, err := s.store.UpdateWorkspace(id, func(w *model.Workspace) error {
		if upd.Name != nil {
			name := strings.TrimSpace(*upd.Name)
			if name == "" {
				apiErr = model.Wrap(400, model.CodeValidation, "name must not be empty", nil)
				return apiErr
			}
			if len(name) > MaxNameLen {
				apiErr = model.Wrap(400, model.CodeValidation, "name too long", map[string]string{"name": "max", "max": fmt.Sprintf("%d", MaxNameLen)})
				return apiErr
			}
			w.Name = name
		}
		if upd.Limits != nil {
			if err := applyLimitUpdate(&w.Limits, *upd.Limits, max, s.cfg.Tunnels.Enabled); err != nil {
				apiErr = err
				return err
			}
		}
		w.LastActivityAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		if apiErr != nil {
			return nil, apiErr
		}
		return nil, err
	}
	s.metricsTouch(w)
	return w, nil
}

// applyLimitUpdate overlays a partial limit update, enforcing the maxima.
// tunnelsEnabled is the server-level tunnels.enabled switch: opting a workspace
// in to tunnels while the server switch is off is refused with 422, the same
// discipline as allow_unlimited_runs.
func applyLimitUpdate(cur *model.Limits, upd LimitUpdate, max model.Limits, tunnelsEnabled bool) error {
	if upd.CPUCores != nil {
		v := *upd.CPUCores
		if v <= 0 {
			return model.Wrap(422, model.CodeUnprocessable, "cpu_cores must be > 0", map[string]string{"field": "cpu_cores"})
		}
		if v > max.CPUCores {
			return maxExceeded("cpu_cores", v, max.CPUCores)
		}
		cur.CPUCores = v
	}
	if upd.MemoryMB != nil {
		v := *upd.MemoryMB
		if v <= 0 {
			return model.Wrap(422, model.CodeUnprocessable, "memory_mb must be > 0", map[string]string{"field": "memory_mb"})
		}
		if v > max.MemoryMB {
			return maxExceeded("memory_mb", v, max.MemoryMB)
		}
		cur.MemoryMB = v
	}
	if upd.DiskMB != nil {
		v := *upd.DiskMB
		if v <= 0 {
			return model.Wrap(422, model.CodeUnprocessable, "disk_mb must be > 0", map[string]string{"field": "disk_mb"})
		}
		if v > max.DiskMB {
			return maxExceeded("disk_mb", v, max.DiskMB)
		}
		cur.DiskMB = v
	}
	if upd.MaxWallTimeSec != nil {
		v := *upd.MaxWallTimeSec
		if v <= 0 {
			return model.Wrap(422, model.CodeUnprocessable, "max_wall_time_sec must be > 0", map[string]string{"field": "max_wall_time_sec"})
		}
		if v > max.MaxWallTimeSec {
			return maxExceeded("max_wall_time_sec", v, max.MaxWallTimeSec)
		}
		cur.MaxWallTimeSec = v
	}
	if upd.MaxRuns != nil {
		v := *upd.MaxRuns
		if v <= 0 {
			return model.Wrap(422, model.CodeUnprocessable, "max_runs must be > 0", map[string]string{"field": "max_runs"})
		}
		if v > max.MaxRuns {
			return maxExceeded("max_runs", v, max.MaxRuns)
		}
		cur.MaxRuns = v
	}
	if upd.AllowNet != nil {
		cur.AllowNet = *upd.AllowNet
	}
	if upd.AllowUnlimitedRuns != nil {
		cur.AllowUnlimitedRuns = *upd.AllowUnlimitedRuns
	}
	if upd.AllowTunnels != nil {
		if *upd.AllowTunnels && !tunnelsEnabled {
			return TunnelsForbiddenByServer()
		}
		cur.AllowTunnels = *upd.AllowTunnels
	}
	return nil
}

func maxExceeded[T int | float64](field string, value, max T) error {
	return model.Wrap(422, model.CodeUnprocessable, field+" exceeds server maximum",
		map[string]string{
			"field": field,
			"value": fmt.Sprintf("%v", value),
			"max":   fmt.Sprintf("%v", max),
		})
}

// SetSuspended sets the workspace status and persists it. It returns the
// updated workspace and whether the status actually changed. It does not stop
// runs; callers that need the suspend semantics use SuspendWorkspace.
func (s *Service) SetSuspended(id string, suspended bool) (*model.Workspace, bool, error) {
	target := model.WorkspaceActive
	if suspended {
		target = model.WorkspaceSuspended
	}
	changed := false
	w, err := s.store.UpdateWorkspace(id, func(w *model.Workspace) error {
		if w.Status != target {
			w.Status = target
			changed = true
		}
		w.LastActivityAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return w, changed, nil
}

// SuspendWorkspace sets the workspace suspended and stops its running runs and
// live terminal sessions. Suspension refuses new work; a session already open
// when the suspend happens is ended (reason workspace_suspended), because a
// suspended workspace must not keep running work. Idempotent: suspending an
// already-suspended workspace reports changed=false (and still stops anything
// running). It returns the updated workspace, the IDs of runs that were
// signalled, and whether the status changed.
func (s *Service) SuspendWorkspace(id string) (*model.Workspace, []string, bool, error) {
	stopped := s.runningRunIDs(id)
	w, changed, err := s.SetSuspended(id, true)
	if err != nil {
		return nil, nil, false, err
	}
	s.StopAllRuns(id)
	s.StopWorkspaceSessions(id, model.SessionReasonWorkspaceSuspended)
	// A suspended workspace keeps no public tunnels: cloudflared processes die
	// with the suspend and the URLs stop working.
	s.ReleaseWorkspaceTunnels(w)
	// A detached process started from a session is part of the workspace's work
	// too; suspending must not leave it running. Its ids are not part of the
	// suspend response (whose shape is fixed), but the processes are stopped.
	if w != nil {
		s.StopDetachedProcesses(w)
	}
	return w, stopped, changed, nil
}

// ResumeWorkspace sets the workspace active. Idempotent: resuming an
// already-active workspace reports changed=false.
func (s *Service) ResumeWorkspace(id string) (*model.Workspace, bool, error) {
	return s.SetSuspended(id, false)
}

// StopSummary reports what a workspace "stop everything" control stopped.
type StopSummary struct {
	RunIDs     []string
	SessionIDs []string
	ProcessIDs []string
}

// StopWorkspace stops **everything** in the workspace: every running/queued run,
// every live terminal session, and every detached process (a process started
// from a session that carries no run/session attribution). It returns the ids of
// each that were signalled and leaves the workspace active. It is idempotent and
// a no-op success when nothing is running.
//
// This closes the gap that made "stop my thing" impossible: a process a user
// started in their own terminal was invisible and unreachable, so stopping the
// workspace left it running. Detached processes are stopped last, after runs and
// sessions, so a descendant that reappears is still caught.
func (s *Service) StopWorkspace(id string) (StopSummary, error) {
	stoppedRuns := s.runningRunIDs(id)
	s.StopAllRuns(id)
	stoppedSessions := s.StopWorkspaceSessions(id, model.SessionReasonWorkspaceStopped)
	stoppedProcs := s.stopAllProcesses(id)
	// A stop releases the workspace's public tunnels (their cloudflared
	// processes die and the URLs stop working).
	if w, err := s.store.GetWorkspace(id); err == nil {
		s.ReleaseWorkspaceTunnels(w)
	}
	return StopSummary{RunIDs: stoppedRuns, SessionIDs: stoppedSessions, ProcessIDs: stoppedProcs}, nil
}

// stopAllProcesses stops every live process tagged for the workspace, returning
// the stopped process ids. Runs and sessions are stopped through their own
// paths (which also remove their cgroups); anything still present here is a
// detached process, but every current member is addressed so nothing is missed.
func (s *Service) stopAllProcesses(id string) []string {
	w, err := s.store.GetWorkspace(id)
	if err != nil {
		return nil
	}
	return s.StopDetachedProcesses(w)
}

// runningRunIDs returns the IDs of runs currently tracked for a workspace.
func (s *Service) runningRunIDs(wsID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.runs))
	for id, rr := range s.runs {
		if rr.wsID == wsID {
			out = append(out, id)
		}
	}
	return out
}

// touchActivity records the most recent run/mutation time.
func (s *Service) touchActivity(id string) {
	if _, err := s.store.UpdateWorkspace(id, func(w *model.Workspace) error {
		w.LastActivityAt = time.Now().UTC()
		return nil
	}); err != nil {
		s.logger.Warn("touch activity failed", "workspace_id", id, "error", err)
	}
}

// ensureActive refuses work for a suspended OR subscription-locked workspace.
// The refusal order is deliberate: suspension (a manual operator action, 409)
// is reported before a lock (a lapsed term, 403) so a client can tell the two
// causes apart. The lock is DERIVED from the stored instants (now >= expires_at,
// or a recorded LockedAt), so it holds even before the reaper has run.
func (s *Service) ensureActive(w *model.Workspace) error {
	if w.Status == model.WorkspaceSuspended {
		return WorkspaceSuspendedError()
	}
	if w.EffectiveStatus(s.now().UTC()) == model.WorkspaceLocked {
		return SubscriptionExpiredError()
	}
	return nil
}

// WorkspaceUsage reports disk use and run counters for a workspace.
type WorkspaceUsage struct {
	DiskBytes         int64
	DiskBytesComplete bool
	DiskMB            int
	DiskPercent       float64
	RunsTotal         int
	RunsByStatus      map[string]int
	ActiveRuns        int
	MaxRuns           int
	Status            string
	CreatedAt         time.Time
	LastActivityAt    time.Time
	Limits            model.Limits
}

// usageWalkEntries bounds the disk walk performed by Usage. It is large enough
// to be exact for typical workspaces and bounded so a huge tree cannot block a
// request; an incomplete walk yields a lower bound (documented).
const usageWalkEntries = 100000

// Usage computes disk usage and run counters for a workspace. The disk size is
// measured with a bounded walk (see usageWalkEntries); when the walk is cut
// short, DiskBytesComplete is false and DiskBytes is a lower bound.
func (s *Service) Usage(w *model.Workspace) (WorkspaceUsage, error) {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return WorkspaceUsage{}, err
	}
	size, complete, err := fs.DiskUsageBounded(usageWalkEntries)
	if err != nil {
		return WorkspaceUsage{}, err
	}
	runs, err := s.store.ListRuns(w.ID)
	if err != nil {
		return WorkspaceUsage{}, err
	}
	byStatus := map[string]int{}
	active := 0
	for _, r := range runs {
		byStatus[string(r.Status)]++
		if r.Status == model.RunQueued || r.Status == model.RunRunning {
			active++
		}
	}
	diskBytes := int64(w.Limits.DiskMB) * 1024 * 1024
	pct := 0.0
	if diskBytes > 0 {
		pct = float64(size) / float64(diskBytes) * 100
	}
	return WorkspaceUsage{
		DiskBytes:         size,
		DiskBytesComplete: complete,
		DiskMB:            w.Limits.DiskMB,
		DiskPercent:       pct,
		RunsTotal:         len(runs),
		RunsByStatus:      byStatus,
		ActiveRuns:        active,
		MaxRuns:           w.Limits.MaxRuns,
		Status:            w.Status,
		CreatedAt:         w.CreatedAt,
		LastActivityAt:    w.LastActivityAt,
		Limits:            w.Limits,
	}, nil
}
