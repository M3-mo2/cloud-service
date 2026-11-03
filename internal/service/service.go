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

	// net is the per-workspace networking + published-port layer. It is nil
	// when no networking is configured (runs then get a fresh empty namespace
	// and published ports are disabled).
	net Network

	// disk provisions each workspace's real, per-workspace filesystem (a
	// fuse2fs-mounted ext4 image sized to its disk_mb) so `df`/`stat -f`/
	// os.statvfs() report the workspace's OWN allowance and a write past it
	// fails at the boundary. nil when the FUSE backend is disabled/unavailable;
	// the counter fallback then applies.
	disk *workspace.DiskManager

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
	s := &Service{
		cfg:      cfg,
		store:    store,
		runner:   runner,
		logger:   logger,
		runs:     make(map[string]*runningRun),
		sessions: make(map[string]*liveSession),
	}
	if cfg.Disk.Enabled {
		id := executor.ResolveIdentity(cfg)
		s.disk = workspace.NewDiskManager(workspace.DiskManagerConfig{
			Backend:    workspace.DiskBackend(cfg.Disk.Backend),
			Fuse2FS:    cfg.Disk.Fuse2FSPath,
			MkfsExt4:   cfg.Disk.MkfsExt4Path,
			Resize2FS:  cfg.Disk.Resize2FSPath,
			Fusermount: cfg.Disk.FusermountPath,
			UID:        id.SandboxUID,
			GID:        id.SandboxGID,
		})
	}
	return s
}

// DiskStatus reports the disk enforcement capability truthfully: enabled is
// the configured switch, mechanism names the effective backend, and note
// explains the state (including a fall back from the FUSE boundary to the
// sampled counter).
func (s *Service) DiskStatus() (enabled bool, mechanism, note string) {
	if !s.cfg.Disk.Enabled {
		return false, "none", "disk provisioning disabled by configuration (disk.enabled=false)"
	}
	backend := workspace.DiskBackend(s.cfg.Disk.Backend)
	if backend == "auto" || backend == "" {
		backend = workspace.DiskBackendFuse
	}
	if backend == workspace.DiskBackendNone {
		return true, "none", "disk.backend=none: the allowance is neither enforced nor reported"
	}
	if backend == workspace.DiskBackendCounter {
		return true, "counter", "disk.backend=counter: engine used-bytes watchdog (real but sampled; a running process can overshoot before the next poll)"
	}
	if s.disk == nil {
		return true, "counter", "FUSE backend requested but not initialized; using the sampled counter"
	}
	if ok, reason := s.disk.Available(); !ok {
		return true, "counter", "FUSE backend unavailable (" + reason + "); using the sampled counter"
	}
	return true, "fuse", "per-workspace fuse2fs ext4 image sized to disk_mb (kernel boundary, no overshoot)"
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

// CreateWorkspace creates a workspace owned by principalID under the named
// profile. An empty profile selects the server's configured default.
//
// The profile is validated against the loaded registry: an unknown name is
// rejected with 422 unknown_profile (never silently replaced by the default,
// which would give the caller a different toolchain than they asked for). The
// resolved name is stamped on the record so GET /v1/workspaces/{id} says which
// profile the workspace got, including when the default was applied.
func (s *Service) CreateWorkspace(principalID, name, profileName string, limits *model.Limits) (*model.Workspace, error) {
	profileName, err := s.resolveProfile(profileName)
	if err != nil {
		return nil, err
	}
	lim := s.defaultLimits()
	if limits != nil {
		lim = applyLimits(lim, *limits)
		if err := checkMaxima(lim, s.maxLimits(), s.cfg.Tunnels.Enabled); err != nil {
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
		Profile:        profileName,
		LastActivityAt: now,
	}
	if err := s.store.CreateWorkspace(w); err != nil {
		return nil, err
	}
	return w, nil
}

// resolveProfile validates a requested profile name. Empty selects the
// server default (cfg.Profiles.Default), which must itself exist. An unknown
// name (including a default that is not loaded) is 422 unknown_profile.
func (s *Service) resolveProfile(name string) (string, error) {
	reg := s.runner.Profiles()
	requested := name
	if requested == "" {
		requested = s.cfg.Profiles.Default
	}
	if reg == nil || !reg.Has(requested) {
		return "", model.Wrap(422, model.CodeUnknownProfile,
			"unknown profile "+requested,
			map[string]string{"profile": requested, "field": "profile"})
	}
	return requested, nil
}

// ResolveProfileForRead returns the profile a stored workspace runs under:
// its recorded profile, or the server default for a record that predates the
// profile field. It never errors for an empty stored value.
func (s *Service) ResolveProfileForRead(w *model.Workspace) string {
	if w != nil && w.Profile != "" {
		return w.Profile
	}
	return s.cfg.Profiles.Default
}

// ProfileCatalogue describes the offered profiles for GET /v1/meta.
func (s *Service) ProfileCatalogue() (defaultName string, profiles []map[string]any) {
	reg := s.runner.Profiles()
	if reg == nil {
		return s.cfg.Profiles.Default, nil
	}
	for _, p := range reg.All() {
		profiles = append(profiles, map[string]any{
			"name":        p.Name,
			"description": p.Description,
			"languages":   emptyIfNil(p.Languages),
			"provides":    emptyIfNil(p.Provides),
			"commands":    p.CommandNames(),
			"paths":       emptyIfNil(p.Paths),
			"notes":       p.Notes,
		})
	}
	return s.cfg.Profiles.Default, profiles
}

func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// checkMaxima rejects a limits value that exceeds any server-level maximum.
func checkMaxima(l, max model.Limits, tunnelsEnabled bool) error {
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
	case l.AllowUnlimitedRuns && !max.AllowUnlimitedRuns:
		return UnlimitedForbiddenByServer()
	case l.AllowTunnels && !tunnelsEnabled:
		return TunnelsForbiddenByServer()
	}
	return nil
}

// UnlimitedForbiddenByServer reports that the server-level configuration
// forbids wall-clock-unbounded runs (max_limits.allow_unlimited_runs is false).
func UnlimitedForbiddenByServer() error {
	return model.Wrap(422, model.CodeUnprocessable,
		"unlimited runs are forbidden by the server configuration",
		map[string]string{"config": "max_limits.allow_unlimited_runs", "field": "no_timeout"})
}

// UnlimitedForbiddenByWorkspace reports that the workspace has not opted in to
// wall-clock-unbounded runs (its limits.allow_unlimited_runs is false).
func UnlimitedForbiddenByWorkspace() error {
	return model.Wrap(422, model.CodeUnprocessable,
		"unlimited runs are not permitted for this workspace",
		map[string]string{"config": "limits.allow_unlimited_runs", "field": "no_timeout"})
}

// UnlimitedAllowed reports whether an unlimited run is permitted for the
// workspace. It enforces both levels: the server maximum AND the workspace
// opt-in. The returned error distinguishes which level refused.
func (s *Service) UnlimitedAllowed(w *model.Workspace) error {
	if !s.cfg.MaxLimits.AllowUnlimitedRuns {
		return UnlimitedForbiddenByServer()
	}
	if !w.Limits.AllowUnlimitedRuns {
		return UnlimitedForbiddenByWorkspace()
	}
	return nil
}

func (s *Service) defaultLimits() model.Limits {
	l := s.cfg.Limits
	return model.Limits{
		CPUCores:           l.CPUCores,
		MemoryMB:           l.MemoryMB,
		DiskMB:             l.DiskMB,
		MaxWallTimeSec:     l.MaxWallTimeSec,
		MaxRuns:            l.MaxRuns,
		AllowNet:           l.AllowNet,
		AllowUnlimitedRuns: l.AllowUnlimitedRuns,
		AllowTunnels:       l.AllowTunnels,
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
	base.AllowUnlimitedRuns = over.AllowUnlimitedRuns
	base.AllowTunnels = over.AllowTunnels
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
	// A record persisted before profiles existed has no profile; report the
	// effective one (the server default), so every response says which profile
	// the workspace actually runs under.
	if w.Profile == "" {
		w.Profile = s.cfg.Profiles.Default
	}
	return w, nil
}

// ListWorkspaces lists workspaces owned by principalID.
func (s *Service) ListWorkspaces(principalID string) ([]*model.Workspace, error) {
	list, err := s.store.ListWorkspaces(principalID)
	if err != nil {
		return nil, err
	}
	for _, w := range list {
		if w.Profile == "" {
			w.Profile = s.cfg.Profiles.Default
		}
	}
	return list, nil
}

// DeleteWorkspace stops all runs and sessions and removes the workspace tree.
func (s *Service) DeleteWorkspace(id, principalID string) error {
	w, err := s.GetOwnedWorkspace(id, principalID)
	if err != nil {
		return err
	}
	s.StopAllRuns(w.ID)
	s.StopWorkspaceSessions(w.ID, model.SessionReasonWorkspaceDeleted)
	// A detached process started from a session must not outlive the workspace.
	s.StopDetachedProcesses(w)
	// Tunnels die with the workspace: stop every cloudflared process and drop
	// the records before the tree is removed.
	s.ReleaseWorkspaceTunnels(w)
	// Unmount and delete the workspace's per-workspace disk image before the
	// tree is removed, so no FUSE mount and no image file is left behind.
	s.removeWorkspaceDisk(w.ID)
	if err := s.store.DeleteWorkspace(w.ID); err != nil {
		return err
	}
	// Tear the private network down last, once no process can still reference it.
	s.ReleaseWorkspaceNetwork(w.ID)
	return nil
}
