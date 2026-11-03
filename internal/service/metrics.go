package service

import (
	"log/slog"
	"sync"
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/metrics"
	"sandboxd/internal/model"
)

// MetricsMarker is an opaque build marker embedded in the binary and logged at
// startup. An acceptance harness greps the built binary for it as a positive
// control, proving the tested artifact is a fresh build of the intended commit.
// It names no engine.
const MetricsMarker = "zcp-metrics-stream-v0.0.19"

// metricsProvider supplies the metrics sampler with its candidate set. It joins
// three cheap, in-memory sources per tick:
//
//   - the executor's EXISTING run -> cgroup tracking (never a /sys/fs/cgroup
//     scan), joined against the service's run table (run -> workspace);
//   - the service's live sessions (each already resolves its own cgroup once);
//   - a workspace inventory (owner, limits) refreshed on a SLOWER reconcile
//     cadence, because the inventory changes rarely and the live-cgroup set
//     changes every tick.
//
// A workspace with no live run and no live session is still a candidate (its
// disk is one statfs) but contributes ZERO cgroup reads.
type metricsProvider struct {
	svc *Service

	mu    sync.Mutex
	cache map[string]cachedWorkspace
}

type cachedWorkspace struct {
	ownerID     string
	diskLimit   int64
	memoryLimit int64
}

// newMetricsProvider builds the provider and does one synchronous reconcile so
// the first tick already sees the inventory.
func newMetricsProvider(svc *Service) *metricsProvider {
	p := &metricsProvider{svc: svc, cache: map[string]cachedWorkspace{}}
	p.reconcile()
	return p
}

// reconcile refreshes the workspace inventory (owner + limits). It never walks a
// workspace tree: it reads the stored metadata only.
func (p *metricsProvider) reconcile() {
	list, err := p.svc.store.ListAllWorkspaces()
	if err != nil {
		return
	}
	next := make(map[string]cachedWorkspace, len(list))
	for _, w := range list {
		if w.Status == model.WorkspaceDeleted {
			continue
		}
		next[w.ID] = cachedWorkspace{
			ownerID:     w.OwnerID,
			diskLimit:   int64(w.Limits.DiskMB) * 1024 * 1024,
			memoryLimit: int64(w.Limits.MemoryMB) * 1024 * 1024,
		}
	}
	p.mu.Lock()
	p.cache = next
	p.mu.Unlock()
}

// upsert inserts or refreshes one workspace in the inventory immediately, so a
// newly created (or patched) workspace is visible on the very next tick without
// waiting for the reconcile cadence. It is called on the create/patch path.
func (p *metricsProvider) upsert(w *model.Workspace) {
	if w == nil || w.Status == model.WorkspaceDeleted {
		return
	}
	p.mu.Lock()
	p.cache[w.ID] = cachedWorkspace{
		ownerID:     w.OwnerID,
		diskLimit:   int64(w.Limits.DiskMB) * 1024 * 1024,
		memoryLimit: int64(w.Limits.MemoryMB) * 1024 * 1024,
	}
	p.mu.Unlock()
}

// remove drops a workspace from the inventory immediately (on delete).
func (p *metricsProvider) remove(id string) {
	p.mu.Lock()
	delete(p.cache, id)
	p.mu.Unlock()
}

// metricsTouch refreshes the metrics inventory for a workspace after a create,
// patch or delete. It is a no-op when the feature is disabled.
func (s *Service) metricsTouch(w *model.Workspace) {
	if s.metricsProvider != nil {
		s.metricsProvider.upsert(w)
	}
}

// metricsForget drops a deleted workspace from the metrics inventory.
func (s *Service) metricsForget(id string) {
	if s.metricsProvider != nil {
		s.metricsProvider.remove(id)
	}
}

// Candidates assembles the per-tick candidate set. The live-cgroup join is
// fresh every tick; the inventory is read from the reconcile cache.
func (p *metricsProvider) Candidates() []metrics.WorkspaceInfo {
	// Snapshot the inventory.
	p.mu.Lock()
	inv := make(map[string]cachedWorkspace, len(p.cache))
	for id, c := range p.cache {
		inv[id] = c
	}
	p.mu.Unlock()

	// Live run cgroups: the executor's own map (runID -> NSJAIL cgroup dir),
	// joined against the service run table for the workspace id.
	runCgroups := map[string][]string{}
	if dirs := p.svc.runner.RunCgroupDirs(); len(dirs) > 0 {
		p.svc.mu.Lock()
		for runID, dir := range dirs {
			if rr, ok := p.svc.runs[runID]; ok && dir != "" {
				runCgroups[rr.wsID] = append(runCgroups[rr.wsID], dir)
			}
		}
		p.svc.mu.Unlock()
	}

	// Live session cgroups: each live session already resolves its own cgroup
	// once (never re-resolved from /proc per tick).
	sessionCgroups := map[string][]string{}
	p.svc.mu.Lock()
	for _, ls := range p.svc.sessions {
		if dir := ls.proc.CgroupDir(); dir != "" {
			sessionCgroups[ls.wsID] = append(sessionCgroups[ls.wsID], dir)
		}
	}
	p.svc.mu.Unlock()

	out := make([]metrics.WorkspaceInfo, 0, len(inv))
	for id, c := range inv {
		out = append(out, metrics.WorkspaceInfo{
			ID:               id,
			OwnerID:          c.ownerID,
			DiskRoot:         p.svc.store.WorkspaceRoot(id),
			DiskLimitBytes:   c.diskLimit,
			MemoryLimitBytes: c.memoryLimit,
			RunCgroups:       runCgroups[id],
			SessionCgroups:   sessionCgroups[id],
		})
	}
	return out
}

// samplerConfig builds the metrics sampler configuration from the service config.
func samplerConfig(cfg config.Config) metrics.Config {
	return metrics.Config{
		Interval:          cfg.Metrics.MetricsInterval(),
		MinInterval:       cfg.Metrics.MetricsMinInterval(),
		MaxInterval:       cfg.Metrics.MetricsMaxInterval(),
		ReconcileInterval: cfg.Metrics.MetricsReconcileInterval(),
		IncludeCPU:        cfg.Metrics.IncludeCPU,
		MaxSubscribers:    cfg.Metrics.MaxSubscribers,
	}
}

// initMetrics wires the sampler and its reconcile loop. Called from New after
// the maps exist. It does not start the sampler: that is lazy, on the first
// subscriber, so an unwatched service costs exactly zero.
func (s *Service) initMetrics() {
	if !s.cfg.Metrics.Enabled {
		return
	}
	provider := newMetricsProvider(s)
	s.metricsProvider = provider
	s.metrics = metrics.New(samplerConfig(s.cfg), provider, metrics.NewFileReader())
}

// startMetricsReconcile launches the slower reconcile loop that refreshes the
// workspace inventory. It is a no-op when metrics are disabled or the sampler is
// not wired, and stops with the service clock (metricsDone).
func (s *Service) startMetricsReconcile() {
	if s.metrics == nil || s.metricsProvider == nil {
		return
	}
	interval := s.cfg.Metrics.MetricsReconcileInterval()
	if interval <= 0 {
		interval = 15 * time.Second
	}
	s.metricsReconcileOnce = true
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.metricsDone:
				return
			case <-t.C:
				s.metricsProvider.reconcile()
			}
		}
	}()
}

// stopMetricsReconcile stops the reconcile loop (idempotent).
func (s *Service) stopMetricsReconcile() {
	if !s.metricsReconcileOnce {
		return
	}
	select {
	case <-s.metricsDone:
	default:
		close(s.metricsDone)
	}
}

// Metrics returns the metrics sampler, or nil when the feature is disabled.
func (s *Service) Metrics() *metrics.Sampler { return s.metrics }

// MetricsStatus reports the metrics capability truthfully: enabled is the
// configured switch, and the effective (clamped) cadence is exposed so a client
// renders the real interval rather than the requested one.
func (s *Service) MetricsStatus() (enabled bool, intervalSec, minSec, maxSec float64, includeCPU bool, maxSubscribers int) {
	if s.metrics == nil {
		return false, 0, 0, 0, false, 0
	}
	return true,
		s.metrics.IntervalSeconds(),
		s.metrics.MinInterval().Seconds(),
		s.metrics.MaxInterval().Seconds(),
		s.metrics.IncludeCPU(),
		s.metrics.MaxSubscribers()
}

// ReconcileMetricsInventory forces an inventory refresh. Used by the gate to
// make a newly created workspace visible without waiting for the reconcile tick.
func (s *Service) ReconcileMetricsInventory() {
	if s.metricsProvider != nil {
		s.metricsProvider.reconcile()
	}
}

// MetricsSnapshot returns a one-shot snapshot for a principal (every workspace
// it owns) or for a single workspace. It uses the exact same sampler as the
// stream, so the two can never disagree.
func (s *Service) MetricsSnapshot(scope metrics.Scope) metrics.Snapshot {
	if s.metrics == nil {
		return metrics.Snapshot{Timestamp: time.Now().UTC(), Workspaces: []metrics.WorkspaceSnapshot{}}
	}
	return s.metrics.SampleScoped(scope)
}

// ShutdownMetrics stops the reconcile loop and the sampler so no goroutine is
// left behind.
func (s *Service) ShutdownMetrics() {
	s.stopMetricsReconcile()
	if s.metrics != nil {
		s.metrics.Shutdown()
	}
}

// metricsLogFields returns the log fields the startup marker uses.
func metricsLogFields(cfg config.Config) []any {
	return []any{
		"enabled", cfg.Metrics.Enabled,
		"interval", cfg.Metrics.MetricsInterval().String(),
		"min_interval", cfg.Metrics.MetricsMinInterval().String(),
		"max_interval", cfg.Metrics.MetricsMaxInterval().String(),
		"include_cpu", cfg.Metrics.IncludeCPU,
	}
}

// metricsMarkerLog is a small helper so main can log one consistent line.
func MetricsStartupLog(logger *slog.Logger, cfg config.Config) {
	logger.Info("per-workspace metrics stream configured",
		append([]any{"build_marker", MetricsMarker}, metricsLogFields(cfg)...)...)
}
