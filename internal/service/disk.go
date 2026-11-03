package service

import (
	"os"
	"path/filepath"

	"sandboxd/internal/model"
	"sandboxd/internal/workspace"
)

// Per-workspace disk provisioning.
//
// Each workspace's `files` directory is the mount point of a real ext4
// filesystem image sized to its disk_mb, mounted with fuse2fs by the
// ORCHESTRATOR (host-side). This is deliberate: the workspace only ever sees the
// mounted result and needs neither /dev/fuse nor fuse2fs inside the jail.
//
// Because the mounted filesystem IS the allowance, `df`, `stat -f` and
// os.statvfs() report the workspace's own size with no syscall interception,
// and a write past the allowance fails with ENOSPC at the boundary -- a real
// kernel-enforced limit with no overshoot. The image file lives beside the
// mount point ({wsdir}/disk.img), so it is created, backed up and deleted with
// the workspace.
//
// When the FUSE backend is unavailable the service falls back to the engine's
// used-bytes counter (see WorkspaceFs and the run watchdog), which is REAL but
// SAMPLED.

// diskBackendEffective resolves the configured backend to the one actually in
// use, falling back to the counter when FUSE is unavailable.
func (s *Service) diskActive() bool {
	enabled, mechanism, _ := s.DiskStatus()
	return enabled && mechanism == "fuse" && s.disk != nil
}

// ensureWorkspaceDisk mounts the workspace's per-workspace filesystem at its
// root if the FUSE backend is active. It is idempotent and is called before
// every run/exec/session so the executor binds a real, bounded filesystem. A
// failure to provision is returned so the caller refuses the run rather than
// run without the disk the workspace was promised.
func (s *Service) ensureWorkspaceDisk(w *model.Workspace) error {
	if !s.diskActive() {
		return nil
	}
	root := s.store.WorkspaceRoot(w.ID)
	return s.ensureWorkspaceDiskAt(root, w.ID, w.Limits.DiskMB)
}

func (s *Service) ensureWorkspaceDiskAt(root, wsID string, diskMB int) error {
	// Preserve any pre-existing content the first time an image is created for
	// an existing workspace: the mount shadows the directory, so the entries
	// are staged aside and copied back into the image afterward. A fresh
	// workspace's directory is empty, so this is a no-op.
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "disk.img")); os.IsNotExist(err) {
		if empty, err := dirEmpty(root); err == nil && !empty {
			if err := stageExistingContent(root); err != nil {
				return err
			}
		}
	}
	if err := s.disk.Ensure(root, diskMB); err != nil {
		return err
	}
	// Restore staged content into the freshly mounted image.
	stage := filepath.Join(filepath.Dir(root), ".disk-stage")
	if _, err := os.Stat(stage); err == nil {
		if err := restoreStagedContent(root, stage); err != nil {
			return err
		}
		_ = os.RemoveAll(stage)
	}
	return nil
}

// releaseWorkspaceDisk unmounts a workspace's image, keeping the image file.
func (s *Service) releaseWorkspaceDisk(wsID string) {
	if s.disk == nil {
		return
	}
	_ = s.disk.Release(s.store.WorkspaceRoot(wsID))
}

// removeWorkspaceDisk unmounts and deletes a workspace's image.
func (s *Service) removeWorkspaceDisk(wsID string) {
	if s.disk == nil {
		return
	}
	_ = s.disk.Remove(s.store.WorkspaceRoot(wsID))
}

// workspaceRoot ensures the disk is mounted and returns the workspace root the
// executor should bind. On a provisioning failure it falls back to the plain
// path so the caller can still surface a specific error; callers that require
// the mount use ensureWorkspaceDisk directly.
func (s *Service) workspaceRoot(w *model.Workspace) string {
	if err := s.ensureWorkspaceDisk(w); err != nil {
		s.logger.Error("workspace disk provisioning failed", "workspace_id", w.ID, "error", err)
	}
	return s.store.WorkspaceRoot(w.ID)
}

func dirEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// stageExistingContent moves every entry of root into a sibling `.disk-stage`
// directory so it is not shadowed by the new mount.
func stageExistingContent(root string) error {
	stage := filepath.Join(filepath.Dir(root), ".disk-stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(root, e.Name()), filepath.Join(stage, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// restoreStagedContent copies every entry of stage into the now-mounted root.
func restoreStagedContent(root, stage string) error {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := filepath.Join(stage, e.Name())
		dst := filepath.Join(root, e.Name())
		if err := copyTree(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// copyTree recursively copies src to dst, following the same conservative
// ownership-preserving rules the workspace layer uses.
func copyTree(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		_ = os.RemoveAll(dst)
		return os.Symlink(target, dst)
	case fi.IsDir():
		if err := os.MkdirAll(dst, fi.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	default:
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, fi.Mode().Perm())
	}
}

// WorkspaceDiskManager exposes the disk manager for tests and diagnostics.
func (s *Service) WorkspaceDiskManager() *workspace.DiskManager { return s.disk }

// ReapOrphanDisks unmounts every FUSE mount directly under this service's data
// directory. On the way in, no run or session exists, and the disk manager
// remounts each workspace lazily on its next use, so a mount left by a hard
// daemon death (the fuse2fs helper is daemonized and outlives the orchestrator)
// is cleaned up and its helper process exits as the mount goes away. It returns
// the number of mounts reaped.
//
// It is a no-op when the FUSE backend is not active. It never touches a mount
// outside the data directory.
func (s *Service) ReapOrphanDisks() int {
	if s.disk == nil {
		return 0
	}
	dataPrefix := s.store.DataDir()
	mounts := workspace.FuseMountsUnder(dataPrefix)
	n := 0
	for _, m := range mounts {
		if err := s.disk.ForceUnmount(m); err != nil {
			s.logger.Warn("could not reap orphan disk mount", "mount", m, "error", err)
			continue
		}
		n++
	}
	return n
}

// ReleaseAllDisks unmounts every active workspace disk mount. It is called on
// graceful shutdown so the daemon leaves no FUSE mount and no helper process
// behind; each image file persists and is remounted lazily on restart.
func (s *Service) ReleaseAllDisks() {
	if s.disk == nil {
		return
	}
	if workspaces, err := s.store.ListAllWorkspaces(); err == nil {
		for _, w := range workspaces {
			_ = s.disk.Release(s.store.WorkspaceRoot(w.ID))
		}
	}
	// A workspace that exists but whose meta failed to load still gets swept by
	// the mount-table walk.
	for _, m := range workspace.FuseMountsUnder(s.store.DataDir()) {
		_ = s.disk.ForceUnmount(m)
	}
}
