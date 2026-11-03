package service

import (
	"sandboxd/internal/executor"
	"sandboxd/internal/model"
)

// Per-workspace cgroup provisioning.
//
// A workspace is a server-shaped thing with its own quota. The DISK half was
// already enforced by a real per-workspace filesystem (ADR 85). This mirrors it
// for RAM/CPU/pids: ONE parent cgroup per workspace owns the workspace's limits,
// and every run's leaf cgroup is created inside it, so the SUM across concurrent
// runs is bounded by the advertised number (ADR 94).
//
// The parent is created lazily before every run/exec/session (idempotent) so a
// workspace created before this mechanism existed, or one whose limits were
// PATCHed, converges to the advertised ceiling on its next use. It is removed on
// workspace delete after every run/session has been stopped.

// ensureWorkspaceCgroup creates (idempotently) the workspace's parent cgroup and
// writes its enforced limits. It delegates to the runner, which is a no-op where
// cgroups are not usable (an unprivileged process, a test backend): nsjail cannot
// enforce a cgroup ceiling there either, and readiness reports it. When cgroups
// ARE usable a failure is returned so the caller refuses the run rather than run
// without the ceiling the workspace was promised: a workspace that advertises a
// memory_mb it does not enforce is exactly the broken contract this fixes.
func (s *Service) ensureWorkspaceCgroup(w *model.Workspace) error {
	return s.runner.EnsureWorkspaceCgroup(w)
}

// removeWorkspaceCgroup kills any member of the workspace's parent cgroup (a
// detached process that outlived the stop) and removes it, so no workspace
// cgroup survives its workspace.
func (s *Service) removeWorkspaceCgroup(wsID string) {
	s.runner.RemoveWorkspaceCgroup(wsID)
}

// workspaceCgroupPath returns the workspace's parent cgroup path (the executor's
// namespace source of truth).
func workspaceCgroupPath(wsID string) string {
	return executor.WorkspaceCgroupPath(wsID)
}
