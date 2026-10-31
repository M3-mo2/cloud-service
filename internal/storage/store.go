// Package storage implements the filesystem JSON metadata backend.
package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sandboxd/internal/model"
)

// Retention is the number of runs kept per workspace.
const Retention = 100

// Store persists workspaces and runs as JSON on the filesystem.
//
// Layout:
//
//	{data}/workspaces/{ws_id}/meta.json
//	{data}/workspaces/{ws_id}/files/...          (workspace root)
//	{data}/workspaces/{ws_id}/runs/{run_id}/meta.json
//	{data}/workspaces/{ws_id}/runs/{run_id}/stdout.log
//	{data}/workspaces/{ws_id}/runs/{run_id}/stderr.log
type Store struct {
	dataDir string
}

// New returns a store rooted at dataDir, creating it if needed.
func New(dataDir string) (*Store, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "workspaces"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "tmp"), 0o755); err != nil {
		return nil, err
	}
	return &Store{dataDir: abs}, nil
}

// DataDir returns the absolute data directory.
func (s *Store) DataDir() string { return s.dataDir }

// TmpDir returns the directory used for transient files (nsjail configs).
func (s *Store) TmpDir() string { return filepath.Join(s.dataDir, "tmp") }

// WorkspacesDir returns the directory containing all workspaces.
func (s *Store) WorkspacesDir() string { return filepath.Join(s.dataDir, "workspaces") }

// WorkspaceDir returns the metadata directory for a workspace.
func (s *Store) WorkspaceDir(id string) string {
	return filepath.Join(s.WorkspacesDir(), id)
}

// WorkspaceRoot returns the filesystem root visible to a workspace.
func (s *Store) WorkspaceRoot(id string) string {
	return filepath.Join(s.WorkspaceDir(id), "files")
}

// WorkspaceMetaPath returns the path to a workspace's meta.json.
func (s *Store) WorkspaceMetaPath(id string) string {
	return filepath.Join(s.WorkspaceDir(id), "meta.json")
}

// runsDir returns the runs directory for a workspace.
func (s *Store) runsDir(wsID string) string {
	return filepath.Join(s.WorkspaceDir(wsID), "runs")
}

// RunDir returns the directory for a single run.
func (s *Store) RunDir(wsID, runID string) string {
	return filepath.Join(s.runsDir(wsID), runID)
}

// RunLogPath returns the path of a run log stream.
func (s *Store) RunLogPath(wsID, runID, stream string) string {
	return filepath.Join(s.RunDir(wsID, runID), stream+".log")
}

func writeJSONAtomic(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// CreateWorkspace creates the workspace metadata and files root.
func (s *Store) CreateWorkspace(w *model.Workspace) error {
	if err := os.MkdirAll(s.WorkspaceRoot(w.ID), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(s.runsDir(w.ID), 0o755); err != nil {
		return err
	}
	return writeJSONAtomic(s.WorkspaceMetaPath(w.ID), w)
}

// GetWorkspace loads a workspace by ID.
func (s *Store) GetWorkspace(id string) (*model.Workspace, error) {
	var w model.Workspace
	if err := readJSON(s.WorkspaceMetaPath(id), &w); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, model.ErrNotFound
		}
		return nil, err
	}
	return &w, nil
}

// SaveWorkspace atomically rewrites workspace metadata.
func (s *Store) SaveWorkspace(w *model.Workspace) error {
	return writeJSONAtomic(s.WorkspaceMetaPath(w.ID), w)
}

// ListAllWorkspaces returns every workspace whose metadata is present.
func (s *Store) ListAllWorkspaces() ([]*model.Workspace, error) {
	entries, err := os.ReadDir(s.WorkspacesDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []*model.Workspace{}, nil
		}
		return nil, err
	}
	out := make([]*model.Workspace, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), model.WorkspaceIDPrefix) {
			continue
		}
		w, err := s.GetWorkspace(e.Name())
		if err != nil {
			continue
		}
		out = append(out, w)
	}
	return out, nil
}

// ListWorkspaces returns all workspaces owned by ownerID, newest first.
func (s *Store) ListWorkspaces(ownerID string) ([]*model.Workspace, error) {
	entries, err := os.ReadDir(s.WorkspacesDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []*model.Workspace{}, nil
		}
		return nil, err
	}
	out := make([]*model.Workspace, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), model.WorkspaceIDPrefix) {
			continue
		}
		w, err := s.GetWorkspace(e.Name())
		if err != nil {
			continue
		}
		if w.Status == model.WorkspaceDeleted {
			continue
		}
		if w.OwnerID != ownerID {
			continue
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// DeleteWorkspace removes the entire workspace directory tree.
func (s *Store) DeleteWorkspace(id string) error {
	if !strings.HasPrefix(id, model.WorkspaceIDPrefix) {
		return fmt.Errorf("invalid workspace id %q", id)
	}
	if err := os.RemoveAll(s.WorkspaceDir(id)); err != nil {
		return err
	}
	return nil
}

// CreateRun creates a run directory and writes its metadata.
func (s *Store) CreateRun(wsID string, r *model.Run) error {
	if err := os.MkdirAll(s.RunDir(wsID, r.ID), 0o755); err != nil {
		return err
	}
	for _, stream := range []string{model.StreamStdout, model.StreamStderr} {
		f, err := os.OpenFile(s.RunLogPath(wsID, r.ID, stream), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		f.Close()
	}
	return s.SaveRun(wsID, r)
}

// SaveRun atomically rewrites run metadata.
func (s *Store) SaveRun(wsID string, r *model.Run) error {
	return writeJSONAtomic(filepath.Join(s.RunDir(wsID, r.ID), "meta.json"), r)
}

// GetRun loads a run by ID.
func (s *Store) GetRun(wsID, runID string) (*model.Run, error) {
	var r model.Run
	if err := readJSON(filepath.Join(s.RunDir(wsID, runID), "meta.json"), &r); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, model.ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

// ListRuns returns all runs for a workspace, newest first.
func (s *Store) ListRuns(wsID string) ([]*model.Run, error) {
	entries, err := os.ReadDir(s.runsDir(wsID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []*model.Run{}, nil
		}
		return nil, err
	}
	out := make([]*model.Run, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), model.RunIDPrefix) {
			continue
		}
		r, err := s.GetRun(wsID, e.Name())
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// DeleteRun removes a run directory.
func (s *Store) DeleteRun(wsID, runID string) error {
	return os.RemoveAll(s.RunDir(wsID, runID))
}

// PruneRuns keeps only the newest keep runs for a workspace and removes the
// rest, returning the number pruned.
func (s *Store) PruneRuns(wsID string, keep int) (int, error) {
	runs, err := s.ListRuns(wsID)
	if err != nil {
		return 0, err
	}
	if len(runs) <= keep {
		return 0, nil
	}
	pruned := 0
	for _, r := range runs[keep:] {
		if err := s.DeleteRun(wsID, r.ID); err != nil {
			return pruned, err
		}
		pruned++
	}
	return pruned, nil
}
