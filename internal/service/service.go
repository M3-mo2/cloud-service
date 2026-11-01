// Package service orchestrates workspaces, files, and executions. It enforces
// ownership and resource policy on top of storage and executor primitives.
package service

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/executor"
	"sandboxd/internal/model"
	"sandboxd/internal/storage"
	"sandboxd/internal/workspace"
)

// Service holds the shared dependencies for handlers.
type Service struct {
	cfg    config.Config
	store  *storage.Store
	runner executor.Runner
	logger *slog.Logger

	mu       sync.Mutex
	runs     map[string]*runningRun
	sessions map[string]*liveSession
}

type runningRun struct {
	runID  string
	wsID   string
	cancel func()
	done   chan struct{}
}

// New constructs a Service.
func New(cfg config.Config, store *storage.Store, runner executor.Runner, logger *slog.Logger) *Service {
	return &Service{
		cfg:      cfg,
		store:    store,
		runner:   runner,
		logger:   logger,
		runs:     make(map[string]*runningRun),
		sessions: make(map[string]*liveSession),
	}
}

// Config returns the service configuration.
func (s *Service) Config() config.Config { return s.cfg }

// Store returns the underlying store.
func (s *Service) Store() *storage.Store { return s.store }

// Runner returns the configured execution backend.
func (s *Service) Runner() executor.Runner { return s.runner }

// WorkspaceFs returns a filesystem view for a workspace, applying the disk
// quota and performing an audit recheck.
func (s *Service) WorkspaceFs(w *model.Workspace) (*workspace.Fs, error) {
	fs, err := workspace.NewFs(s.store.WorkspaceRoot(w.ID))
	if err != nil {
		return nil, err
	}
	fs.SetDiskLimit(int64(w.Limits.DiskMB) * 1024 * 1024)
	if err := fs.Recheck(); err != nil {
		return nil, err
	}
	return fs, nil
}

// CreateWorkspace creates a workspace owned by principalID.
func (s *Service) CreateWorkspace(principalID, name string, limits *model.Limits) (*model.Workspace, error) {
	lim := s.defaultLimits()
	if limits != nil {
		lim = applyLimits(lim, *limits)
		if err := checkMaxima(lim, s.maxLimits()); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	w := &model.Workspace{
		ID:             model.NewWorkspaceID(),
		OwnerID:        principalID,
		Name:           name,
		CreatedAt:      now,
		Status:         model.WorkspaceActive,
		Limits:         lim,
		LastActivityAt: now,
	}
	if err := s.store.CreateWorkspace(w); err != nil {
		return nil, err
	}
	return w, nil
}

// checkMaxima rejects a limits value that exceeds any server-level maximum.
func checkMaxima(l, max model.Limits) error {
	switch {
	case l.CPUCores > max.CPUCores:
		return maxExceeded("cpu_cores", l.CPUCores, max.CPUCores)
	case l.MemoryMB > max.MemoryMB:
		return maxExceeded("memory_mb", l.MemoryMB, max.MemoryMB)
	case l.DiskMB > max.DiskMB:
		return maxExceeded("disk_mb", l.DiskMB, max.DiskMB)
	case l.MaxWallTimeSec > max.MaxWallTimeSec:
		return maxExceeded("max_wall_time_sec", l.MaxWallTimeSec, max.MaxWallTimeSec)
	case l.MaxRuns > max.MaxRuns:
		return maxExceeded("max_runs", l.MaxRuns, max.MaxRuns)
	}
	return nil
}

func (s *Service) defaultLimits() model.Limits {
	l := s.cfg.Limits
	return model.Limits{
		CPUCores:       l.CPUCores,
		MemoryMB:       l.MemoryMB,
		DiskMB:         l.DiskMB,
		MaxWallTimeSec: l.MaxWallTimeSec,
		MaxRuns:        l.MaxRuns,
		AllowNet:       l.AllowNet,
	}
}

// applyLimits overlays non-zero fields from over onto base.
func applyLimits(base, over model.Limits) model.Limits {
	if over.CPUCores > 0 {
		base.CPUCores = over.CPUCores
	}
	if over.MemoryMB > 0 {
		base.MemoryMB = over.MemoryMB
	}
	if over.DiskMB > 0 {
		base.DiskMB = over.DiskMB
	}
	if over.MaxWallTimeSec > 0 {
		base.MaxWallTimeSec = over.MaxWallTimeSec
	}
	if over.MaxRuns > 0 {
		base.MaxRuns = over.MaxRuns
	}
	base.AllowNet = over.AllowNet
	return base
}

// GetOwnedWorkspace loads a workspace and verifies ownership.
func (s *Service) GetOwnedWorkspace(id, principalID string) (*model.Workspace, error) {
	w, err := s.store.GetWorkspace(id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, model.ErrNotFound
		}
		return nil, err
	}
	if w.OwnerID != principalID {
		return nil, model.ErrForbidden
	}
	return w, nil
}

// ListWorkspaces lists workspaces owned by principalID.
func (s *Service) ListWorkspaces(principalID string) ([]*model.Workspace, error) {
	return s.store.ListWorkspaces(principalID)
}

// DeleteWorkspace stops all runs and sessions and removes the workspace tree.
func (s *Service) DeleteWorkspace(id, principalID string) error {
	w, err := s.GetOwnedWorkspace(id, principalID)
	if err != nil {
		return err
	}
	s.StopAllRuns(w.ID)
	s.StopWorkspaceSessions(w.ID, model.SessionReasonWorkspaceDeleted)
	if err := s.store.DeleteWorkspace(w.ID); err != nil {
		return err
	}
	return nil
}
