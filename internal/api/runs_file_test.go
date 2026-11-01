package api

import (
	"testing"
	"time"
)

// TestRunFileUsesCwdAndInterpreter proves a kind=file run carries the requested
// working directory and an extension-selected interpreter in its argv, and that
// the run record exposes cwd/interpreter/timeout truthfully.
func TestRunFileUsesCwdAndInterpreter(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/sub/main.py", keyA, map[string]any{"content": "print(1)\n"})

	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{
		"kind": "file", "file": "/sub/main.py", "cwd": "/sub",
	})
	if rec.Code != 202 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	run := decode[struct {
		Run runJSON `json:"run"`
	}](t, rec).Run
	if run.Cwd != "/sub" {
		t.Fatalf("cwd = %q, want /sub", run.Cwd)
	}
	if run.Interpreter != "python3" {
		t.Fatalf("interpreter = %q, want python3", run.Interpreter)
	}
	if len(run.Argv) != 2 || run.Argv[0] != "python3" || run.Argv[1] != "/workspace/sub/main.py" {
		t.Fatalf("argv = %v", run.Argv)
	}
	if run.TimeoutSec == nil || *run.TimeoutSec != 300 {
		t.Fatalf("timeout_sec = %v, want 300", run.TimeoutSec)
	}
	if run.Unlimited {
		t.Fatal("finite run must not be unlimited")
	}
	h.waitRunStatus(id, run.ID, "exited", 2*time.Second)
	req := h.runner.LastRequest()
	if req == nil || req.Cwd != "/sub" {
		t.Fatalf("executor cwd = %+v", req)
	}
}

func TestRunFileExtensionSelection(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	for _, tc := range []struct {
		file string
		in   string
	}{
		{"/x.py", "python3"},
		{"/x.js", "node"},
		{"/x.sh", "bash"},
	} {
		h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path="+tc.file, keyA, map[string]any{"content": "x"})
		rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "file", "file": tc.file})
		if rec.Code != 202 {
			t.Fatalf("%s code = %d body=%s", tc.file, rec.Code, rec.Body.String())
		}
		if got := decode[struct {
			Run runJSON `json:"run"`
		}](t, rec).Run.Interpreter; got != tc.in {
			t.Fatalf("%s interpreter = %q want %q", tc.file, got, tc.in)
		}
		h.svc.WaitRuns(id)
	}
}

func TestRunFileUnsupportedExtension422(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/x.txt", keyA, map[string]any{"content": "x"})
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "file", "file": "/x.txt"})
	if rec.Code != 422 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	env := decode[errorEnvelope](t, rec)
	if env.Error.Details["supported_extensions"] == "" {
		t.Fatalf("error must name the supported set: %+v", env.Error)
	}
}

func TestRunFileExplicitInterpreterOverride(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/data.txt", keyA, map[string]any{"content": "print(1)\n"})
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{
		"kind": "file", "file": "/data.txt", "interpreter": "python3",
	})
	if rec.Code != 202 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	// A non-allowlisted interpreter is rejected.
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{
		"kind": "file", "file": "/data.txt", "interpreter": "/bin/sh",
	})
	if rec.Code != 422 {
		t.Fatalf("bad interpreter code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunCwdMissingIs422Not503(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/f.py", keyA, map[string]any{"content": "print(1)\n"})

	// A missing cwd is a client error (422), never a 503 "sandbox unavailable".
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{
		"kind": "file", "file": "/f.py", "cwd": "/nope",
	})
	if rec.Code != 422 {
		t.Fatalf("missing cwd code = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "unprocessable" {
		t.Fatalf("code = %q, want unprocessable", got)
	}

	// A cwd that is a file is also 422.
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{
		"kind": "file", "file": "/f.py", "cwd": "/f.py",
	})
	if rec.Code != 422 {
		t.Fatalf("file cwd code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunArgvAndFileConflict422(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/f.py", keyA, map[string]any{"content": "print(1)\n"})
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{
		"kind": "file", "file": "/f.py", "argv": []string{"python3", "f.py"},
	})
	if rec.Code != 422 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunNoTimeoutDeniedByDefault(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/f.py", keyA, map[string]any{"content": "print(1)\n"})
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{
		"kind": "file", "file": "/f.py", "no_timeout": true,
	})
	if rec.Code != 422 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if env := decode[errorEnvelope](t, rec); env.Error.Details["config"] != "max_limits.allow_unlimited_runs" {
		t.Fatalf("details = %+v, want server-level", env.Error.Details)
	}
}

func TestRunNoTimeoutAllowedRecordsUnlimited(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	// Opt the server and the workspace in.
	h.cfg.MaxLimits.AllowUnlimitedRuns = true
	h.rebuild()
	id := h.createWorkspaceUnlimited("runs")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/f.py", keyA, map[string]any{"content": "print(1)\n"})
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{
		"kind": "file", "file": "/f.py", "no_timeout": true,
	})
	if rec.Code != 202 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	run := decode[struct {
		Run runJSON `json:"run"`
	}](t, rec).Run
	if !run.Unlimited || run.TimeoutSec != nil {
		t.Fatalf("run must record unlimited with null timeout: unlimited=%v timeout=%v", run.Unlimited, run.TimeoutSec)
	}
}

// createWorkspaceUnlimited creates a workspace with allow_unlimited_runs=true.
func (h *harness) createWorkspaceUnlimited(name string) string {
	h.t.Helper()
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{
		"name":   name,
		"limits": map[string]any{"allow_unlimited_runs": true},
	})
	if rec.Code != 201 {
		h.t.Fatalf("create unlimited workspace: %d %s", rec.Code, rec.Body.String())
	}
	return decode[struct {
		Workspace struct {
			ID string `json:"id"`
		} `json:"workspace"`
	}](h.t, rec).Workspace.ID
}

func TestFileStatAndListingRunnable(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/run.py", keyA, map[string]any{"content": "print(1)\n"})
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/notes.txt", keyA, map[string]any{"content": "hi"})

	// stat of a runnable file names the interpreter.
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/stat?path=/run.py", keyA, nil)
	if got := decode[struct {
		Runnable string `json:"runnable"`
	}](t, rec).Runnable; got != "python3" {
		t.Fatalf("stat runnable = %q, want python3", got)
	}
	// stat of a non-runnable file omits it.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/stat?path=/notes.txt", keyA, nil)
	if got := decode[struct {
		Runnable string `json:"runnable"`
	}](t, rec).Runnable; got != "" {
		t.Fatalf("stat runnable = %q, want empty", got)
	}

	// the listing exposes it per entry.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files?path=/", keyA, nil)
	type entry struct {
		Name     string `json:"name"`
		Runnable string `json:"runnable"`
	}
	list := decode[struct {
		Entries []entry `json:"entries"`
	}](t, rec)
	byName := map[string]string{}
	for _, e := range list.Entries {
		byName[e.Name] = e.Runnable
	}
	if byName["run.py"] != "python3" || byName["notes.txt"] != "" {
		t.Fatalf("listing runnable = %+v", byName)
	}
}

func TestMetaReportsInterpreters(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("GET", "/v1/meta", keyA, nil)
	body := decode[map[string]any](t, rec)
	ins, ok := body["interpreters"].([]any)
	if !ok || len(ins) < 4 {
		t.Fatalf("interpreters = %v", body["interpreters"])
	}
	exts, ok := body["runnable_extensions"].([]any)
	if !ok || len(exts) == 0 {
		t.Fatalf("runnable_extensions = %v", body["runnable_extensions"])
	}
}

func TestStopWorkspaceStopsRunsAndSessions(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.Delay = 500 * time.Millisecond
	h.runner.LiveDelay = 10 * time.Second
	id := h.createWorkspace("stop")

	// A long run and a live session.
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "command", "argv": []string{"sleep"}})
	runID := decode[struct {
		Run runJSON `json:"run"`
	}](t, rec).Run.ID
	sesID, _ := h.createSession(id, nil)
	time.Sleep(20 * time.Millisecond)

	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/stop", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("stop code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		StoppedRunCount     int      `json:"stopped_run_count"`
		StoppedRunIDs       []string `json:"stopped_run_ids"`
		StoppedSessionCount int      `json:"stopped_session_count"`
		StoppedSessionIDs   []string `json:"stopped_session_ids"`
		Workspace           workspaceJSON
	}](t, rec)
	if body.StoppedRunCount != 1 || body.StoppedRunIDs[0] != runID {
		t.Fatalf("stopped runs = %+v", body)
	}
	if body.StoppedSessionCount != 1 || body.StoppedSessionIDs[0] != sesID {
		t.Fatalf("stopped sessions = %+v, want %s", body, sesID)
	}
	if body.Workspace.Status != "active" {
		t.Fatalf("workspace status = %q, want active", body.Workspace.Status)
	}
	h.waitRunStatus(id, runID, "killed", 2*time.Second)
	sess := h.waitSessionStatus(id, sesID, "killed", 2*time.Second)
	if sess.Reason != "workspace_stopped" {
		t.Fatalf("session reason = %q, want workspace_stopped", sess.Reason)
	}
}
