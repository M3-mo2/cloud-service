package api

import (
	"testing"
	"time"
)

func (h *harness) waitRunStatus(wsID, runID, want string, timeout time.Duration) runJSON {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec := h.doJSON("GET", "/v1/workspaces/"+wsID+"/runs/"+runID, keyA, nil)
		var resp struct {
			Run runJSON `json:"run"`
		}
		resp = decode[struct {
			Run runJSON `json:"run"`
		}](h.t, rec)
		if resp.Run.Status == want {
			return resp.Run
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("run %s never reached status %q", runID, want)
	return runJSON{}
}

func TestCreateRunCommand(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.Stdout = "hi\n"
	id := h.createWorkspace("runs")

	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{
		"kind": "command",
		"argv": []string{"python3", "main.py"},
	})
	if rec.Code != 202 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	created := decode[struct {
		Run runJSON `json:"run"`
	}](t, rec).Run
	if created.ID == "" || created.Kind != "command" || created.Status != "queued" {
		t.Fatalf("run = %+v", created)
	}
	got := h.waitRunStatus(id, created.ID, "exited", 2*time.Second)
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("exit = %v", got.ExitCode)
	}
}

func TestCreateRunFileRequiresFile(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "file"})
	if rec.Code != 400 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestCreateRunBadKind(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "weird"})
	if rec.Code != 400 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestRunLogsPolling(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.Stdout = "line1\nline2\n"
	id := h.createWorkspace("runs")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "command", "argv": []string{"echo"}})
	created := decode[struct {
		Run runJSON `json:"run"`
	}](t, rec).Run
	h.waitRunStatus(id, created.ID, "exited", 2*time.Second)

	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/runs/"+created.ID+"/logs?stream=stdout&offset=0&limit=65536", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("logs code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Data   string `json:"data"`
		Offset int64  `json:"offset"`
		Next   int64  `json:"next"`
		EOF    bool   `json:"eof"`
	}](t, rec)
	if body.Data != "line1\nline2\n" {
		t.Fatalf("data = %q", body.Data)
	}
	if body.Offset != 0 || body.Next != int64(len(body.Data)) || !body.EOF {
		t.Fatalf("body = %+v", body)
	}
}

func TestRunLogsOffset(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.Stdout = "abcdef"
	id := h.createWorkspace("runs")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "command", "argv": []string{"x"}})
	created := decode[struct {
		Run runJSON `json:"run"`
	}](t, rec).Run
	h.waitRunStatus(id, created.ID, "exited", 2*time.Second)

	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/runs/"+created.ID+"/logs?stream=stdout&offset=2&limit=2", keyA, nil)
	body := decode[struct {
		Data   string `json:"data"`
		Offset int64  `json:"offset"`
		Next   int64  `json:"next"`
		EOF    bool   `json:"eof"`
	}](t, rec)
	if body.Data != "cd" || body.Offset != 2 || body.Next != 4 || body.EOF {
		t.Fatalf("body = %+v", body)
	}
}

func TestRunLogsBadStream(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "command", "argv": []string{"x"}})
	created := decode[struct {
		Run runJSON `json:"run"`
	}](t, rec).Run
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/runs/"+created.ID+"/logs?stream=sideways", keyA, nil)
	if rec.Code != 400 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestListRuns(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "command", "argv": []string{"a"}})
	h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "command", "argv": []string{"b"}})
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/runs", keyA, nil)
	body := decode[struct {
		Runs []runJSON `json:"runs"`
	}](t, rec)
	if len(body.Runs) != 2 {
		t.Fatalf("runs = %+v", body.Runs)
	}
}

func TestGetRunNotFound(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("runs")
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/runs/run_01HZZZZZZZZZZZZZZZZZZZZZZ", keyA, nil)
	if rec.Code != 404 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestStopRun(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.Delay = 300 * time.Millisecond
	id := h.createWorkspace("runs")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "command", "argv": []string{"sleep"}})
	created := decode[struct {
		Run runJSON `json:"run"`
	}](t, rec).Run

	time.Sleep(20 * time.Millisecond)
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/runs/"+created.ID+"/stop", keyA, nil)
	if rec.Code != 202 {
		t.Fatalf("stop code = %d body=%s", rec.Code, rec.Body.String())
	}
	got := h.waitRunStatus(id, created.ID, "killed", 2*time.Second)
	if got.Status != "killed" {
		t.Fatalf("status = %q", got.Status)
	}

	// Stop is idempotent.
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/runs/"+created.ID+"/stop", keyA, nil)
	if rec.Code != 202 {
		t.Fatalf("second stop code = %d", rec.Code)
	}
}

func TestStopRunNotFound(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("runs")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs/run_01HZZZZZZZZZZZZZZZZZZZZZZ/stop", keyA, nil)
	if rec.Code != 404 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestCreateRunFileValidatesPath(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")

	// Missing file is a 404 at request time, not a deferred failure.
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "file", "file": "/nope.py"})
	if rec.Code != 404 {
		t.Fatalf("missing file code = %d body=%s", rec.Code, rec.Body.String())
	}

	// Traversal is rejected as unsafe.
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "file", "file": "../../etc/passwd"})
	if rec.Code != 422 {
		t.Fatalf("traversal code = %d body=%s", rec.Code, rec.Body.String())
	}

	// An existing file is accepted.
	rec = h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/main.py", keyA, map[string]any{"content": "print(1)\n"})
	if rec.Code != 204 {
		t.Fatalf("put code = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyA, map[string]any{"kind": "file", "file": "/main.py"})
	if rec.Code != 202 {
		t.Fatalf("valid file code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunsCrossOwner(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("runs")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/runs", keyB, map[string]any{"kind": "command", "argv": []string{"x"}})
	if rec.Code != 403 {
		t.Fatalf("code = %d", rec.Code)
	}
}
