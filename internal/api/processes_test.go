package api

import (
	"testing"

	"sandboxd/internal/executor"
	"sandboxd/internal/service"
)

// withProcesses configures the fake runner's inventory.
func (h *harness) withProcesses(procs ...executor.ProcessInfo) {
	h.runner.SetProcesses(procs)
}

// TestListProcessesEmptyIsHonestEmptyList: a workspace with no processes returns
// an empty array (never null and never an error).
func TestListProcessesEmptyIsHonestEmptyList(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("proc-empty")
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/processes", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Processes []processJSON `json:"processes"`
		Count     int           `json:"count"`
	}](t, rec)
	if body.Processes == nil {
		t.Fatal("processes must be an empty array, not null")
	}
	if body.Count != 0 || len(body.Processes) != 0 {
		t.Fatalf("count=%d len=%d, want 0", body.Count, len(body.Processes))
	}
}

// TestListProcessesAttribution: a session-started detached process appears with
// kind/source_id set, a run process appears as kind "run", and a process inside
// the workspace with no source tag is reported as "detached" (never hidden).
func TestListProcessesAttribution(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("proc-attr")
	h.withProcesses(
		executor.ProcessInfo{PID: 100, PPID: 1, Argv: []string{"python3", "bot.py"}, WorkspaceID: id, Kind: executor.KindSession, SourceID: "ses_abc", StartTicks: 1000},
		executor.ProcessInfo{PID: 101, PPID: 100, Argv: []string{"sh"}, WorkspaceID: id, Kind: executor.KindRun, SourceID: "run_def", StartTicks: 1001},
		executor.ProcessInfo{PID: 102, PPID: 1, Argv: []string{"nohup", "worker"}, WorkspaceID: id, Kind: "detached", StartTicks: 1002},
		// A process in another workspace must not appear.
		executor.ProcessInfo{PID: 103, PPID: 1, Argv: []string{"other"}, WorkspaceID: "ws_other", Kind: executor.KindSession, SourceID: "ses_other", StartTicks: 1003},
	)
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/processes", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Processes []processJSON `json:"processes"`
		Count     int           `json:"count"`
	}](t, rec)
	if body.Count != 3 {
		t.Fatalf("count = %d, want 3 (foreign workspace excluded)", body.Count)
	}
	bySource := map[string]processJSON{}
	for _, p := range body.Processes {
		if p.ID == "" {
			t.Fatalf("process %d has no stable id", p.PID)
		}
		bySource[p.SourceID] = p
	}
	if p := bySource["ses_abc"]; p.Kind != "session" || p.PID != 100 {
		t.Fatalf("session process = %+v", p)
	}
	if p := bySource["run_def"]; p.Kind != "run" || p.PID != 101 {
		t.Fatalf("run process = %+v", p)
	}
	if p := bySource[""]; p.Kind != "detached" || p.PID != 102 {
		t.Fatalf("detached process = %+v", p)
	}
}

// TestProcessIDStable: the id is stable for a given (workspace, pid, start) and
// distinct across pids and start times.
func TestProcessIDStable(t *testing.T) {
	a := service_processID("ws_1", 42, 100)
	if a != service_processID("ws_1", 42, 100) {
		t.Fatal("id is not stable")
	}
	if a == service_processID("ws_1", 43, 100) || a == service_processID("ws_1", 42, 101) || a == service_processID("ws_2", 42, 100) {
		t.Fatal("id collides across pid/start/workspace")
	}
}

// TestProcessesCrossOwnerForbidden: a foreign principal gets 403 on both the
// inventory and the single-process stop.
func TestProcessesCrossOwnerForbidden(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("proc-owned")
	h.withProcesses(executor.ProcessInfo{PID: 200, WorkspaceID: id, Kind: "detached", StartTicks: 1})
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/processes", keyB, nil)
	if rec.Code != 403 || errCode(t, rec) != "forbidden" {
		t.Fatalf("list as foreign = %d %q", rec.Code, errCode(t, rec))
	}
	pid := service_processID(id, 200, 1)
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/processes/"+pid+"/stop", keyB, nil)
	if rec.Code != 403 || errCode(t, rec) != "forbidden" {
		t.Fatalf("stop as foreign = %d %q", rec.Code, errCode(t, rec))
	}
}

// TestStopProcessStopsTreeAndIsIdempotent: stopping a known process stops its
// tree and reports status; a second stop reports gone; an unknown id is 404.
func TestStopProcessStopsTreeAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("proc-stop")
	h.withProcesses(executor.ProcessInfo{PID: 300, PPID: 1, Argv: []string{"sleep"}, WorkspaceID: id, Kind: "detached", StartTicks: 7})
	pid := service_processID(id, 300, 7)

	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/processes/"+pid+"/stop", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("stop code = %d body=%s", rec.Code, rec.Body.String())
	}
	res := decode[struct {
		ProcessID string `json:"process_id"`
		Status    string `json:"status"`
		Stopped   bool   `json:"stopped"`
	}](t, rec)
	if res.ProcessID != pid || !res.Stopped || res.Status != "stopped" {
		t.Fatalf("stop result = %+v", res)
	}
	if len(h.runner.KilledProcesses) != 1 || h.runner.KilledProcesses[0] != 300 {
		t.Fatalf("killed = %v, want [300]", h.runner.KilledProcesses)
	}

	// Idempotent: the process is gone from the inventory, so the id is now 404.
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/processes/"+pid+"/stop", keyA, nil)
	if rec.Code != 404 || errCode(t, rec) != "not_found" {
		t.Fatalf("second stop = %d %q, want 404 not_found", rec.Code, errCode(t, rec))
	}
}

// TestStopWorkspaceStopsDetachedProcesses: the workspace stop reaches a detached
// process started from a session and reports the process fields.
func TestStopWorkspaceStopsDetachedProcesses(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("stop-all")
	h.withProcesses(
		executor.ProcessInfo{PID: 400, Argv: []string{"bot"}, WorkspaceID: id, Kind: "detached", StartTicks: 11},
		executor.ProcessInfo{PID: 401, Argv: []string{"other"}, WorkspaceID: "ws_other", Kind: "detached", StartTicks: 12},
	)
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/stop", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("stop code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Workspace           workspaceJSON `json:"workspace"`
		StoppedRunIDs       []string      `json:"stopped_run_ids"`
		StoppedRunCount     int           `json:"stopped_run_count"`
		StoppedSessionIDs   []string      `json:"stopped_session_ids"`
		StoppedSessionCount int           `json:"stopped_session_count"`
		StoppedProcessIDs   []string      `json:"stopped_process_ids"`
		StoppedProcessCount int           `json:"stopped_process_count"`
	}](t, rec)
	if body.StoppedProcessCount != 1 || len(body.StoppedProcessIDs) != 1 {
		t.Fatalf("stopped_process = %+v", body)
	}
	if body.StoppedProcessIDs[0] != service_processID(id, 400, 11) {
		t.Fatalf("stopped process id = %q", body.StoppedProcessIDs[0])
	}
	// The existing fields keep their shape.
	if body.StoppedRunCount != 0 || body.StoppedSessionCount != 0 || body.Workspace.Status != "active" {
		t.Fatalf("stop body = %+v", body)
	}
	if len(h.runner.KilledProcesses) != 1 || h.runner.KilledProcesses[0] != 400 {
		t.Fatalf("killed = %v, want only the workspace's own process", h.runner.KilledProcesses)
	}
}

// service_processID reaches the service's id derivation without exporting it from
// the api package.
func service_processID(wsID string, pid int, ticks uint64) string {
	return service.ProcessID(wsID, pid, ticks)
}
