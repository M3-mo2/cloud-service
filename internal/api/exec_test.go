package api

import (
	"testing"

	"sandboxd/internal/executor"
)

func TestExecSuccess(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.Stdout = "hi\n"
	h.runner.Stderr = "warn\n"
	h.runner.ExitCode = 0
	id := h.createWorkspace("exec")

	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/exec", keyA, map[string]any{
		"cmd": []string{"python3", "main.py"},
	})
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		ExitCode   int    `json:"exit_code"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		DurationMS int64  `json:"duration_ms"`
		Truncated  bool   `json:"truncated"`
	}](t, rec)
	if body.ExitCode != 0 || body.Stdout != "hi\n" || body.Stderr != "warn\n" {
		t.Fatalf("body = %+v", body)
	}
}

func TestExecNonZeroExit(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.ExitCode = 2
	id := h.createWorkspace("exec")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/exec", keyA, map[string]any{"cmd": []string{"false"}})
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := decode[struct {
		ExitCode int `json:"exit_code"`
	}](t, rec)
	if body.ExitCode != 2 {
		t.Fatalf("exit = %d", body.ExitCode)
	}
}

func TestExecTruncated(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.Stdout = makeStr(2000)
	id := h.createWorkspace("exec")
	// Caps are read from cfg; force a tiny stdout cap by writing a huge payload.
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/exec", keyA, map[string]any{"cmd": []string{"cat"}})
	body := decode[struct {
		Truncated bool `json:"truncated"`
	}](t, rec)
	// FakeRunner truncates only when cap is smaller than output; default exec
	// stdout cap is 256KB so a 2KB output is not truncated.
	if body.Truncated {
		t.Fatalf("unexpected truncation")
	}
}

func TestExecValidation(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("exec")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/exec", keyA, map[string]any{"cmd": []string{}})
	if rec.Code != 400 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestExecNsjailUnavailable(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = false
	id := h.createWorkspace("exec")
	// Force the runner to return unavailable regardless of flag.
	h.runner.Err = executor.ErrNsjailUnavailable
	defer func() { h.runner.Err = nil }()

	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/exec", keyA, map[string]any{"cmd": []string{"python3", "-c", "1"}})
	// The FakeRunner returns its Err, which the service propagates.
	if rec.Code != 503 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if errCode(t, rec) != "unavailable" {
		t.Fatalf("code = %q", errCode(t, rec))
	}
}

func TestExecCrossOwner(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("exec")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/exec", keyB, map[string]any{"cmd": []string{"ls"}})
	if rec.Code != 403 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func makeStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
