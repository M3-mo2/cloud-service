package api

import (
	"testing"
	"time"

	"sandboxd/internal/model"
)

// seedRun creates a run record directly in storage with a given status so list,
// purge and usage tests can be deterministic without racing background workers.
func (h *harness) seedRun(wsID string, status model.RunStatus, kind model.RunKind, started time.Time) string {
	h.t.Helper()
	run := &model.Run{
		ID:          model.NewRunID(),
		WorkspaceID: wsID,
		Kind:        kind,
		Argv:        []string{"echo", "x"},
		Status:      status,
		StartedAt:   started.UTC(),
	}
	if status == model.RunExited {
		run.SetExitCode(0)
		run.EndedAt = started.Add(time.Second).UTC()
	}
	if err := h.store.CreateRun(wsID, run); err != nil {
		h.t.Fatalf("seed run: %v", err)
	}
	return run.ID
}

// --- 1. PATCH partial update ---

func TestPatchWorkspaceSingleField(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("patch")

	rec := h.doJSON("PATCH", "/v1/workspaces/"+id, keyA, map[string]any{
		"limits": map[string]any{"memory_mb": 256},
	})
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	ws := decode[struct {
		Workspace workspaceJSON `json:"workspace"`
	}](t, rec).Workspace
	if ws.Limits.MemoryMB != 256 {
		t.Fatalf("memory_mb = %d, want 256", ws.Limits.MemoryMB)
	}
	// Untouched fields keep their defaults.
	if ws.Limits.CPUCores != 1 || ws.Limits.DiskMB != 1024 || ws.Limits.MaxRuns != 3 {
		t.Fatalf("limits = %+v", ws.Limits)
	}

	// A second PATCH of a different field must not disturb the first.
	rec = h.doJSON("PATCH", "/v1/workspaces/"+id, keyA, map[string]any{
		"limits": map[string]any{"max_runs": 5},
	})
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	ws = decode[struct {
		Workspace workspaceJSON `json:"workspace"`
	}](t, rec).Workspace
	if ws.Limits.MaxRuns != 5 || ws.Limits.MemoryMB != 256 {
		t.Fatalf("limits = %+v", ws.Limits)
	}
}

func TestPatchWorkspaceName(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("old")
	rec := h.doJSON("PATCH", "/v1/workspaces/"+id, keyA, map[string]any{"name": "new"})
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	ws := decode[struct {
		Workspace workspaceJSON `json:"workspace"`
	}](t, rec).Workspace
	if ws.Name != "new" {
		t.Fatalf("name = %q", ws.Name)
	}
}

// --- 2. PATCH above maximum is rejected and changes nothing ---

func TestPatchWorkspaceAboveMaximumRejected(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("limits")

	rec := h.doJSON("PATCH", "/v1/workspaces/"+id, keyA, map[string]any{
		"limits": map[string]any{"memory_mb": 999999},
	})
	if rec.Code != 422 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "unprocessable" {
		t.Fatalf("code = %q", got)
	}
	// Nothing changed.
	rec = h.doJSON("GET", "/v1/workspaces/"+id, keyA, nil)
	ws := decode[struct {
		Workspace workspaceJSON `json:"workspace"`
	}](t, rec).Workspace
	if ws.Limits.MemoryMB != 512 {
		t.Fatalf("memory_mb = %d, want 512 (unchanged)", ws.Limits.MemoryMB)
	}
}

func TestPatchWorkspaceNonPositiveRejected(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("limits")
	rec := h.doJSON("PATCH", "/v1/workspaces/"+id, keyA, map[string]any{
		"limits": map[string]any{"disk_mb": 0},
	})
	if rec.Code != 422 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateWorkspaceAboveMaximumRejected(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{
		"name":   "too-big",
		"limits": map[string]any{"max_runs": 9999},
	})
	if rec.Code != 422 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- 3. patched limit persists across a restart ---

func TestPatchWorkspacePersistsAcrossRestart(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("persist")
	rec := h.doJSON("PATCH", "/v1/workspaces/"+id, keyA, map[string]any{
		"limits": map[string]any{"disk_mb": 4096},
	})
	if rec.Code != 200 {
		t.Fatalf("patch code = %d body=%s", rec.Code, rec.Body.String())
	}

	// Simulate a restart by building a fresh service over the same data dir.
	h2 := newHarnessAt(t, h.dataDir)
	rec = h2.doJSON("GET", "/v1/workspaces/"+id, keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("get after restart code = %d body=%s", rec.Code, rec.Body.String())
	}
	ws := decode[struct {
		Workspace workspaceJSON `json:"workspace"`
	}](t, rec).Workspace
	if ws.Limits.DiskMB != 4096 {
		t.Fatalf("disk_mb = %d, want 4096 after restart", ws.Limits.DiskMB)
	}
}

// --- 4. suspend / resume semantics ---

func TestSuspendRefusesWorkAndResumeRestores(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("lifecycle")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/main.py", keyA, map[string]any{"content": "print(1)\n"})

	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/suspend", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("suspend code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Workspace workspaceJSON `json:"workspace"`
		Changed   bool          `json:"changed"`
	}](t, rec)
	if body.Workspace.Status != "suspended" || !body.Changed {
		t.Fatalf("suspend body = %+v", body)
	}

	// exec and run creation are refused with the documented code.
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/exec", keyA, map[string]any{"cmd": []string{"python3", "main.py"}})
	if rec.Code != 409 {
		t.Fatalf("exec code = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "workspace_suspended" {
		t.Fatalf("exec code = %q", got)
	}
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "command", "argv": []string{"x"}})
	if rec.Code != 409 || errCode(t, rec) != "workspace_suspended" {
		t.Fatalf("run code = %d code=%q", rec.Code, errCode(t, rec))
	}

	// Reads keep working.
	if rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/main.py", keyA, nil); rec.Code != 200 {
		t.Fatalf("read while suspended = %d", rec.Code)
	}
	if rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files?path=/", keyA, nil); rec.Code != 200 {
		t.Fatalf("list while suspended = %d", rec.Code)
	}
	if rec := h.doJSON("GET", "/v1/workspaces/"+id+"/runs", keyA, nil); rec.Code != 200 {
		t.Fatalf("list runs while suspended = %d", rec.Code)
	}

	// Idempotent suspend.
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/suspend", keyA, nil)
	body = decode[struct {
		Workspace workspaceJSON `json:"workspace"`
		Changed   bool          `json:"changed"`
	}](t, rec)
	if body.Changed || body.Workspace.Status != "suspended" {
		t.Fatalf("second suspend body = %+v", body)
	}

	// Resume accepts work again.
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/resume", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("resume code = %d", rec.Code)
	}
	if rec := h.doJSON("POST", "/v1/workspaces/"+id+"/exec", keyA, map[string]any{"cmd": []string{"python3", "main.py"}}); rec.Code != 200 {
		t.Fatalf("exec after resume = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSuspendStopsRunningRuns(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.Delay = 300 * time.Millisecond
	id := h.createWorkspace("lifecycle")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "command", "argv": []string{"sleep"}})
	created := decode[struct {
		Run runJSON `json:"run"`
	}](t, rec).Run
	time.Sleep(20 * time.Millisecond)

	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/suspend", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("suspend code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		StoppedCount int      `json:"stopped_run_count"`
		StoppedIDs   []string `json:"stopped_run_ids"`
	}](t, rec)
	if body.StoppedCount != 1 || len(body.StoppedIDs) != 1 || body.StoppedIDs[0] != created.ID {
		t.Fatalf("stopped = %+v", body)
	}
	h.waitRunStatus(id, created.ID, "killed", 2*time.Second)
}

// --- 5. stop workspace ---

func TestStopWorkspaceKillsRuns(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.Delay = 300 * time.Millisecond
	id := h.createWorkspace("stop")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "command", "argv": []string{"sleep"}})
	created := decode[struct {
		Run runJSON `json:"run"`
	}](t, rec).Run
	time.Sleep(20 * time.Millisecond)

	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/stop", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("stop code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Workspace    workspaceJSON `json:"workspace"`
		StoppedCount int           `json:"stopped_run_count"`
	}](t, rec)
	if body.StoppedCount != 1 {
		t.Fatalf("stopped = %+v", body)
	}
	// Stop leaves the workspace active.
	if body.Workspace.Status != "active" {
		t.Fatalf("status = %q, want active", body.Workspace.Status)
	}
	h.waitRunStatus(id, created.ID, "killed", 2*time.Second)
}

func TestStopWorkspaceNoRunsIsNoop(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("stop-empty")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/stop", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("stop code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		StoppedCount int `json:"stopped_run_count"`
	}](t, rec)
	if body.StoppedCount != 0 {
		t.Fatalf("stopped = %+v", body)
	}
}

// --- 6. suspension survives restart and is still enforced ---

func TestSuspensionSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("suspended")
	if rec := h.doJSON("POST", "/v1/workspaces/"+id+"/suspend", keyA, nil); rec.Code != 200 {
		t.Fatalf("suspend code = %d", rec.Code)
	}

	h2 := newHarnessAt(t, h.dataDir)
	rec := h2.doJSON("GET", "/v1/workspaces/"+id, keyA, nil)
	ws := decode[struct {
		Workspace workspaceJSON `json:"workspace"`
	}](t, rec).Workspace
	if ws.Status != "suspended" {
		t.Fatalf("status after restart = %q", ws.Status)
	}
	rec = h2.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "command", "argv": []string{"x"}})
	if rec.Code != 409 || errCode(t, rec) != "workspace_suspended" {
		t.Fatalf("run after restart = %d code=%q", rec.Code, errCode(t, rec))
	}
}

// --- 7. run listing filter and pagination ---

func TestListRunsFilterAndPage(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("runs")
	base := time.Now().Add(-time.Hour)
	// Oldest to newest; ListRuns returns newest first.
	h.seedRun(id, model.RunKilled, model.RunKindCommand, base)
	h.seedRun(id, model.RunExited, model.RunKindCommand, base.Add(1*time.Second))
	h.seedRun(id, model.RunExited, model.RunKindFile, base.Add(2*time.Second))
	h.seedRun(id, model.RunTimeout, model.RunKindCommand, base.Add(3*time.Second))
	h.seedRun(id, model.RunFailed, model.RunKindCommand, base.Add(4*time.Second))

	// Filter by comma-separated statuses.
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/runs?status=exited,killed", keyA, nil)
	body := decode[struct {
		Runs  []runJSON `json:"runs"`
		Total int       `json:"total"`
	}](t, rec)
	if body.Total != 3 || len(body.Runs) != 3 {
		t.Fatalf("total=%d runs=%d", body.Total, len(body.Runs))
	}

	// Repeatable status parameter.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/runs?status=exited&status=killed", keyA, nil)
	body = decode[struct {
		Runs  []runJSON `json:"runs"`
		Total int       `json:"total"`
	}](t, rec)
	if body.Total != 3 {
		t.Fatalf("total=%d, want 3", body.Total)
	}

	// Pagination over exited runs, newest first.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/runs?status=exited&limit=1&offset=0", keyA, nil)
	page := decode[struct {
		Runs  []runJSON `json:"runs"`
		Total int       `json:"total"`
		Limit int       `json:"limit"`
	}](t, rec)
	if page.Total != 2 || len(page.Runs) != 1 {
		t.Fatalf("page = %+v", page)
	}
	first := page.Runs[0].ID
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/runs?status=exited&limit=1&offset=1", keyA, nil)
	page2 := decode[struct {
		Runs  []runJSON `json:"runs"`
		Total int       `json:"total"`
	}](t, rec)
	if len(page2.Runs) != 1 || page2.Runs[0].ID == first {
		t.Fatalf("page2 = %+v", page2)
	}

	// Kind filter.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/runs?kind=file", keyA, nil)
	body = decode[struct {
		Runs  []runJSON `json:"runs"`
		Total int       `json:"total"`
	}](t, rec)
	if body.Total != 1 {
		t.Fatalf("kind filter total = %d", body.Total)
	}

	// Unknown status is a validation error.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/runs?status=bogus", keyA, nil)
	if rec.Code != 400 {
		t.Fatalf("bad status code = %d", rec.Code)
	}
}

// --- 8. purge run history ---

func TestPurgeRunsRefusesWhileRunning(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("purge")
	h.seedRun(id, model.RunExited, model.RunKindCommand, time.Now().Add(-2*time.Second))
	h.seedRun(id, model.RunRunning, model.RunKindCommand, time.Now().Add(-time.Second))

	rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/runs", keyA, nil)
	if rec.Code != 409 {
		t.Fatalf("purge code = %d body=%s", rec.Code, rec.Body.String())
	}
	if errCode(t, rec) != "conflict" {
		t.Fatalf("code = %q", errCode(t, rec))
	}

	// Finish the run, then purge removes all finished history.
	list, _ := h.store.ListRuns(id)
	for _, r := range list {
		if r.Status == model.RunRunning {
			r.Status = model.RunExited
			r.SetExitCode(0)
			r.EndedAt = time.Now().UTC()
			if err := h.store.SaveRun(id, r); err != nil {
				t.Fatal(err)
			}
		}
	}

	rec = h.doJSON("DELETE", "/v1/workspaces/"+id+"/runs", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("purge code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Deleted int `json:"deleted"`
	}](t, rec)
	if body.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2", body.Deleted)
	}
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/runs", keyA, nil)
	if runs := decode[struct {
		Runs []runJSON `json:"runs"`
	}](t, rec).Runs; len(runs) != 0 {
		t.Fatalf("runs remain: %+v", runs)
	}
}

func TestPurgeSingleRun(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("purge-one")
	finished := h.seedRun(id, model.RunExited, model.RunKindCommand, time.Now())
	running := h.seedRun(id, model.RunRunning, model.RunKindCommand, time.Now())

	if rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/runs/"+finished, keyA, nil); rec.Code != 204 {
		t.Fatalf("delete finished code = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/runs/"+running, keyA, nil); rec.Code != 409 {
		t.Fatalf("delete running code = %d", rec.Code)
	}
	if rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/runs/run_01HZZZZZZZZZZZZZZZZZZZZZZ", keyA, nil); rec.Code != 404 {
		t.Fatalf("delete missing code = %d", rec.Code)
	}
}

// --- 9. usage ---

func TestUsageReportsDiskAndCounters(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("usage")
	// Two known files, 5 + 6 = 11 bytes.
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/a.txt", keyA, map[string]any{"content": "hello"})
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/b.txt", keyA, map[string]any{"content": "world!"})

	h.seedRun(id, model.RunExited, model.RunKindCommand, time.Now().Add(-3*time.Second))
	h.seedRun(id, model.RunExited, model.RunKindCommand, time.Now().Add(-2*time.Second))
	h.seedRun(id, model.RunKilled, model.RunKindCommand, time.Now().Add(-time.Second))

	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/usage", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("usage code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		WorkspaceID       string         `json:"workspace_id"`
		Status            string         `json:"status"`
		DiskBytes         int64          `json:"disk_bytes"`
		DiskBytesComplete bool           `json:"disk_bytes_complete"`
		DiskMB            int            `json:"disk_mb"`
		RunsTotal         int            `json:"runs_total"`
		RunsByStatus      map[string]int `json:"runs_by_status"`
		ActiveRuns        int            `json:"active_runs"`
		MaxRuns           int            `json:"max_runs"`
		Limits            limitsJSON     `json:"limits"`
	}](t, rec)
	if body.WorkspaceID != id || body.Status != "active" {
		t.Fatalf("usage = %+v", body)
	}
	if body.DiskBytes != 11 {
		t.Fatalf("disk_bytes = %d, want 11", body.DiskBytes)
	}
	if !body.DiskBytesComplete {
		t.Fatalf("disk_bytes_complete = false, want true")
	}
	if body.DiskMB != 1024 {
		t.Fatalf("disk_mb = %d", body.DiskMB)
	}
	if body.RunsTotal != 3 || body.RunsByStatus["exited"] != 2 || body.RunsByStatus["killed"] != 1 {
		t.Fatalf("counters = %+v", body)
	}
	if body.ActiveRuns != 0 || body.MaxRuns != 3 {
		t.Fatalf("active=%d max=%d", body.ActiveRuns, body.MaxRuns)
	}
	if body.Limits.MemoryMB != 512 {
		t.Fatalf("limits = %+v", body.Limits)
	}
}

// --- stop of an already-finished run is a reported no-op ---

func TestStopFinishedRunReportsStatus(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("stop-finished")
	runID := h.seedRun(id, model.RunExited, model.RunKindCommand, time.Now())
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs/"+runID+"/stop", keyA, nil)
	if rec.Code != 202 {
		t.Fatalf("stop code = %d body=%s", rec.Code, rec.Body.String())
	}
	run := decode[struct {
		Run runJSON `json:"run"`
	}](t, rec).Run
	if run.Status != "exited" {
		t.Fatalf("status = %q, want exited", run.Status)
	}
}

// --- 10. ownership on every new endpoint ---

func TestNewEndpointsEnforceOwnership(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("owned")
	runID := h.seedRun(id, model.RunExited, model.RunKindCommand, time.Now())

	cases := []struct {
		method string
		path   string
		body   map[string]any
	}{
		{"PATCH", "/v1/workspaces/" + id, map[string]any{"name": "x"}},
		{"POST", "/v1/workspaces/" + id + "/suspend", nil},
		{"POST", "/v1/workspaces/" + id + "/resume", nil},
		{"POST", "/v1/workspaces/" + id + "/stop", nil},
		{"GET", "/v1/workspaces/" + id + "/usage", nil},
		{"DELETE", "/v1/workspaces/" + id + "/runs", nil},
		{"DELETE", "/v1/workspaces/" + id + "/runs/" + runID, nil},
	}
	for _, c := range cases {
		rec := h.doJSON(c.method, c.path, keyB, c.body)
		if rec.Code != 403 {
			t.Fatalf("%s %s as foreign principal = %d body=%s", c.method, c.path, rec.Code, rec.Body.String())
		}
		if errCode(t, rec) != "forbidden" {
			t.Fatalf("%s %s code = %q", c.method, c.path, errCode(t, rec))
		}
	}
}
