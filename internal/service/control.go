package service

import (
	"fmt"
	"strings"
	"time"

	"sandboxd/internal/model"
	"sandboxd/internal/workspace"
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

// LimitUpdate is a partial update of a workspace's limits. A nil field keeps
// the current value; this is why every field is a pointer (so an omitted JSON
// field and an explicit null both leave the value untouched).
type LimitUpdate struct {
	CPUCores       *float64
	MemoryMB       *int
	DiskMB         *int
	MaxWallTimeSec *int
	MaxRuns        *int
	AllowNet       *bool
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
		CPUCores:       m.CPUCores,
		MemoryMB:       m.MemoryMB,
		DiskMB:         m.DiskMB,
		MaxWallTimeSec: m.MaxWallTimeSec,
		MaxRuns:        m.MaxRuns,
		AllowNet:       m.AllowNet,
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
			if err := applyLimitUpdate(&w.Limits, *upd.Limits, max); err != nil {
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
	return w, nil
}

// applyLimitUpdate overlays a partial limit update, enforcing the maxima.
func applyLimitUpdate(cur *model.Limits, upd LimitUpdate, max model.Limits) error {
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

// SuspendWorkspace sets the workspace suspended and stops its running runs.
// Idempotent: suspending an already-suspended workspace reports changed=false
// (and still stops anything running). It returns the updated workspace, the
// IDs of runs that were signalled, and whether the status changed.
func (s *Service) SuspendWorkspace(id string) (*model.Workspace, []string, bool, error) {
	stopped := s.runningRunIDs(id)
	w, changed, err := s.SetSuspended(id, true)
	if err != nil {
		return nil, nil, false, err
	}
	s.StopAllRuns(id)
	return w, stopped, changed, nil
}

// ResumeWorkspace sets the workspace active. Idempotent: resuming an
// already-active workspace reports changed=false.
func (s *Service) ResumeWorkspace(id string) (*model.Workspace, bool, error) {
	return s.SetSuspended(id, false)
}

// StopWorkspace stops every running/queued run of the workspace and leaves the
// workspace active. It returns the IDs of the runs that were signalled. It is
// idempotent and a no-op success when nothing is running.
func (s *Service) StopWorkspace(id string) ([]string, error) {
	stopped := s.runningRunIDs(id)
	s.StopAllRuns(id)
	return stopped, nil
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

// ensureActive refuses work for a suspended workspace.
func (s *Service) ensureActive(w *model.Workspace) error {
	if w.Status == model.WorkspaceSuspended {
		return WorkspaceSuspendedError()
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
	fs, err := workspace.NewFs(s.store.WorkspaceRoot(w.ID))
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
