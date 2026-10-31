package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"sandboxd/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func sampleWorkspace(owner string) *model.Workspace {
	return &model.Workspace{
		ID:        model.NewWorkspaceID(),
		OwnerID:   owner,
		Name:      "test",
		CreatedAt: time.Now().UTC(),
		Status:    model.WorkspaceActive,
		Limits:    model.Limits{CPUCores: 1, MemoryMB: 512, DiskMB: 1024, MaxWallTimeSec: 300, MaxRuns: 3},
	}
}

func TestWorkspaceCRUD(t *testing.T) {
	s := newTestStore(t)
	w := sampleWorkspace("user_a")
	if err := s.CreateWorkspace(w); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := os.Stat(s.WorkspaceRoot(w.ID)); err != nil {
		t.Fatalf("workspace root missing: %v", err)
	}
	got, err := s.GetWorkspace(w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != w.ID || got.OwnerID != "user_a" {
		t.Fatalf("got %+v", got)
	}
	if _, err := s.GetWorkspace("ws_missing"); err == nil {
		t.Fatal("expected not found")
	}
	if err := s.DeleteWorkspace(w.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(s.WorkspaceDir(w.ID)); !os.IsNotExist(err) {
		t.Fatalf("workspace dir still exists: %v", err)
	}
}

func TestDeleteWorkspaceRejectsBadID(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteWorkspace("evil/../path"); err == nil {
		t.Fatal("expected invalid id error")
	}
}

func TestUpdateWorkspace(t *testing.T) {
	s := newTestStore(t)
	w := sampleWorkspace("u")
	if err := s.CreateWorkspace(w); err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpdateWorkspace(w.ID, func(ws *model.Workspace) error {
		ws.Name = "renamed"
		ws.Limits.MemoryMB = 128
		ws.Status = model.WorkspaceSuspended
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "renamed" || updated.Limits.MemoryMB != 128 || updated.Status != model.WorkspaceSuspended {
		t.Fatalf("updated = %+v", updated)
	}
	// The change is persisted.
	got, err := s.GetWorkspace(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.Limits.MemoryMB != 128 {
		t.Fatalf("reloaded = %+v", got)
	}
	if _, err := s.UpdateWorkspace("ws_missing", func(*model.Workspace) error { return nil }); err == nil {
		t.Fatal("expected not found")
	}
}

func TestListWorkspacesOwnership(t *testing.T) {
	s := newTestStore(t)
	mine := sampleWorkspace("user_a")
	theirs := sampleWorkspace("user_b")
	for _, w := range []*model.Workspace{mine, theirs} {
		if err := s.CreateWorkspace(w); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListWorkspaces("user_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].OwnerID != "user_a" {
		t.Fatalf("got %+v", got)
	}
}

func TestListsAreNewestFirst(t *testing.T) {
	s := newTestStore(t)
	old := sampleWorkspace("u")
	old.CreatedAt = time.Now().Add(-time.Hour)
	newer := sampleWorkspace("u")
	newer.CreatedAt = time.Now()
	for _, w := range []*model.Workspace{old, newer} {
		if err := s.CreateWorkspace(w); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.ListWorkspaces("u")
	if len(got) != 2 || got[0].ID != newer.ID {
		t.Fatalf("order wrong: %+v", got)
	}
}

func TestRunCRUDAndLogs(t *testing.T) {
	s := newTestStore(t)
	w := sampleWorkspace("u")
	if err := s.CreateWorkspace(w); err != nil {
		t.Fatal(err)
	}
	r := &model.Run{
		ID:          model.NewRunID(),
		WorkspaceID: w.ID,
		Kind:        model.RunKindCommand,
		Argv:        []string{"echo", "hi"},
		Status:      model.RunQueued,
		StartedAt:   time.Now().UTC(),
	}
	if err := s.CreateRun(w.ID, r); err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, stream := range []string{model.StreamStdout, model.StreamStderr} {
		if _, err := os.Stat(s.RunLogPath(w.ID, r.ID, stream)); err != nil {
			t.Fatalf("log %s missing: %v", stream, err)
		}
	}
	r.Status = model.RunExited
	r.SetExitCode(0)
	if err := s.SaveRun(w.ID, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(w.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.RunExited || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("got %+v", got)
	}
	if _, err := s.GetRun(w.ID, "run_missing"); err == nil {
		t.Fatal("expected not found")
	}
}

func TestPruneRuns(t *testing.T) {
	s := newTestStore(t)
	w := sampleWorkspace("u")
	if err := s.CreateWorkspace(w); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		r := &model.Run{
			ID:          model.NewRunID(),
			WorkspaceID: w.ID,
			Kind:        model.RunKindCommand,
			Argv:        []string{"true"},
			Status:      model.RunExited,
			StartedAt:   time.Now().Add(time.Duration(i) * time.Second).UTC(),
		}
		if err := s.CreateRun(w.ID, r); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	pruned, err := s.PruneRuns(w.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 7 {
		t.Fatalf("pruned = %d, want 7", pruned)
	}
	left, _ := s.ListRuns(w.ID)
	if len(left) != 3 {
		t.Fatalf("left = %d, want 3", len(left))
	}
}

func TestAtomicWriteLeavesNoTemp(t *testing.T) {
	s := newTestStore(t)
	w := sampleWorkspace("u")
	if err := s.CreateWorkspace(w); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(s.WorkspaceDir(w.ID))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}
