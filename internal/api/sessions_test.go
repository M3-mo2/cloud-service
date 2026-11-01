package api

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"sandboxd/internal/model"
)

// createSession starts a session as usrA and returns its ID.
func (h *harness) createSession(wsID string, body map[string]any) (string, sessionJSON) {
	h.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	rec := h.doJSON("POST", "/v1/workspaces/"+wsID+"/sessions", keyA, body)
	if rec.Code != 201 {
		h.t.Fatalf("create session: %d %s", rec.Code, rec.Body.String())
	}
	sess := sessionJSONOf(h.t, rec.Body.Bytes())
	return sess.ID, sess
}

// waitSessionStatus polls a session until it reaches want.
func (h *harness) waitSessionStatus(wsID, sessionID, want string, timeout time.Duration) sessionJSON {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last sessionJSON
	for time.Now().Before(deadline) {
		rec := h.doJSON("GET", "/v1/workspaces/"+wsID+"/sessions/"+sessionID, keyA, nil)
		last = sessionJSONOf(h.t, rec.Body.Bytes())
		if last.Status == want {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("session %s never reached %q (last=%+v)", sessionID, want, last)
	return last
}

// sessionJSONOf decodes a session from a response.
func sessionJSONOf(t *testing.T, body []byte) sessionJSON {
	t.Helper()
	var resp struct {
		Session sessionJSON `json:"session"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode session: %v body=%s", err, body)
	}
	return resp.Session
}

// TestSessionPTYFailureIsLoud covers the loud-failure contract: a relay that
// cannot allocate a PTY must surface as a failed session with a diagnostic, not
// a session reported running whose output is a Python traceback. The fake
// runner models the relay dying at startup with a sandbox error.
func TestSessionPTYFailureIsLoud(t *testing.T) {
	h := newHarness(t)
	h.runner.LiveErr = errors.New("PTY allocation failed: sandboxd-pty-error: out of pty devices")
	ws := h.createWorkspace("ses")
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions", keyA, map[string]any{})
	if rec.Code != 201 {
		t.Fatalf("create = %d body=%s", rec.Code, rec.Body.String())
	}
	sess := sessionJSONOf(t, rec.Body.Bytes())
	if sess.Status != "failed" {
		t.Fatalf("session reported %q, want failed (must not stay running): %+v", sess.Status, sess)
	}
	if sess.Reason != "sandbox_unavailable" {
		t.Fatalf("reason = %q, want sandbox_unavailable", sess.Reason)
	}
	if !strings.Contains(sess.Diagnostic, "out of pty devices") {
		t.Fatalf("diagnostic = %q, want the PTY error", sess.Diagnostic)
	}
	// A failed session is terminal: no live process is left behind.
	if h.runner.LiveCount() != 0 {
		t.Fatalf("live processes = %d, want 0 after a failed start", h.runner.LiveCount())
	}
	// The failure is also visible through the get endpoint.
	rec = h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions/"+sess.ID, keyA, nil)
	got := sessionJSONOf(t, rec.Body.Bytes())
	if got.Status != "failed" || got.Diagnostic == "" {
		t.Fatalf("get session = %+v, want failed with a diagnostic", got)
	}
}

// executorError is a minimal error carrying a diagnostic string.
type executorError struct{ msg string }

func (e *executorError) Error() string { return e.msg }

// TestSessionCreateAndInspect covers scenario 1 (a session starts; the record
// reports the interactive launcher) and the inspect shape. The real-PTY proof
// (`test -t 1`) lives in internal/executor (TestPTYHelperRealTTY) because the
// fake runner cannot allocate a PTY; here we assert the session was launched
// through the PTY path with the configured shell and window size.
func TestSessionCreateAndInspect(t *testing.T) {
	h := newHarness(t)
	// Keep the relay alive so the create reports a running session.
	h.runner.LiveDelay = 10 * time.Second
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, map[string]any{"shell": "/bin/bash", "cols": 100, "rows": 40})

	req := h.runner.LastLiveRequest()
	if req == nil || !req.PTY {
		t.Fatalf("session was not launched through the PTY path: %+v", req)
	}
	if req.Shell != "/bin/bash" || req.Cols != 100 || req.Rows != 40 {
		t.Fatalf("launcher config = shell=%q cols=%d rows=%d", req.Shell, req.Cols, req.Rows)
	}
	if req.StdoutPath == "" {
		t.Fatal("session must stream output to a log path")
	}

	rec := h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions/"+id, keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("get session = %d", rec.Code)
	}
	sess := sessionJSONOf(t, rec.Body.Bytes())
	if sess.ID != id || sess.Status != "running" || sess.Cols != 100 || sess.Rows != 40 {
		t.Fatalf("session = %+v", sess)
	}
	if !strings.HasPrefix(sess.ID, "ses_") {
		t.Fatalf("session id must have the ses_ prefix: %q", sess.ID)
	}
	if sess.WorkspaceID != ws || sess.Shell != "/bin/bash" {
		t.Fatalf("session = %+v", sess)
	}
}

// TestSessionDefaultSize: an absent cols/rows gets the documented default.
func TestSessionDefaultSize(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, map[string]any{})
	rec := h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions/"+id, keyA, nil)
	sess := sessionJSONOf(t, rec.Body.Bytes())
	if sess.Cols != 80 || sess.Rows != 24 {
		t.Fatalf("default size = %dx%d, want 80x24", sess.Cols, sess.Rows)
	}
}

// TestSessionRejectsAbsurdSize covers the cols/rows bound (422).
func TestSessionRejectsAbsurdSize(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	for _, body := range []map[string]any{
		{"cols": 99999}, {"rows": 99999}, {"cols": -5}, {"rows": -1},
	} {
		rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions", keyA, body)
		if rec.Code != 422 {
			t.Fatalf("size %v = %d body=%s", body, rec.Code, rec.Body.String())
		}
		if errCode(t, rec) != "unprocessable" {
			t.Fatalf("size %v code=%q", body, errCode(t, rec))
		}
	}
}

// TestSessionCwdSafety: a cwd that escapes the root is 422 and a missing dir is
// 422, while a valid relative dir is accepted.
func TestSessionCwdSafety(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")

	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions", keyA, map[string]any{"cwd": "../../etc"})
	if rec.Code != 422 {
		t.Fatalf("escape cwd = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions", keyA, map[string]any{"cwd": "/nope"})
	if rec.Code != 422 {
		t.Fatalf("missing cwd = %d body=%s", rec.Code, rec.Body.String())
	}

	mkdir := h.doJSON("POST", "/v1/workspaces/"+ws+"/files/mkdir", keyA, map[string]any{"path": "/sub"})
	if mkdir.Code != 201 {
		t.Fatalf("mkdir = %d", mkdir.Code)
	}
	rec = h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions", keyA, map[string]any{"cwd": "/sub"})
	if rec.Code != 201 {
		t.Fatalf("valid cwd = %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestSessionInputAndOutput covers the input → output round trip and the
// offset/limit tailing semantics. The fake echoes the control message it
// receives, proving the service relays a structured input message.
func TestSessionInputAndOutput(t *testing.T) {
	h := newHarness(t)
	h.runner.LiveOutput = "welcome\n"
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)

	// Output is retrievable with offset/limit, same shape as run logs.
	rec := h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions/"+id+"/output?offset=0&limit=65536", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("output = %d body=%s", rec.Code, rec.Body.String())
	}
	chunk := decode[struct {
		Data   string `json:"data"`
		Offset int64  `json:"offset"`
		Next   int64  `json:"next"`
		EOF    bool   `json:"eof"`
	}](t, rec)
	if chunk.Data != "welcome\n" || chunk.Offset != 0 || chunk.Next != 8 || !chunk.EOF {
		t.Fatalf("chunk = %+v", chunk)
	}

	// A bounded slice.
	rec = h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions/"+id+"/output?offset=3&limit=4", keyA, nil)
	slice := decode[struct {
		Data string `json:"data"`
		Next int64  `json:"next"`
	}](t, rec)
	if slice.Data != "come" || slice.Next != 7 {
		t.Fatalf("slice = %+v", slice)
	}

	// Input is relayed as a structured control message.
	if rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions/"+id+"/input", keyA, map[string]any{"data": "echo hi\n"}); rec.Code != 204 {
		t.Fatalf("input = %d body=%s", rec.Code, rec.Body.String())
	}
	written := string(h.runner.Written(h.runner.LastLivePID()))
	if !strings.Contains(written, `"type":"input"`) || !strings.Contains(written, `echo hi\n`) {
		t.Fatalf("relayed input = %q", written)
	}
}

// TestSessionSignalRelayed proves a signal is relayed through the helper (the
// difference from run stop: the session is not killed).
func TestSessionSignalRelayed(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)
	if rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions/"+id+"/signal", keyA, map[string]any{"signal": "INT"}); rec.Code != 204 {
		t.Fatalf("signal = %d body=%s", rec.Code, rec.Body.String())
	}
	written := string(h.runner.Written(h.runner.LastLivePID()))
	if !strings.Contains(written, `"type":"signal"`) || !strings.Contains(written, `"INT"`) {
		t.Fatalf("relayed signal = %q", written)
	}
	// The session survives a signal.
	rec := h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions/"+id, keyA, nil)
	if sess := sessionJSONOf(t, rec.Body.Bytes()); sess.Status != "running" {
		t.Fatalf("session status after signal = %q, want running", sess.Status)
	}
	// An unknown signal is rejected.
	rec = h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions/"+id+"/signal", keyA, map[string]any{"signal": "KILL9"})
	if rec.Code != 422 {
		t.Fatalf("bad signal = %d", rec.Code)
	}
}

// TestSessionResizeRelayed proves resize is relayed.
func TestSessionResizeRelayed(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)
	if rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions/"+id+"/resize", keyA, map[string]any{"cols": 120, "rows": 50}); rec.Code != 204 {
		t.Fatalf("resize = %d body=%s", rec.Code, rec.Body.String())
	}
	written := string(h.runner.Written(h.runner.LastLivePID()))
	if !strings.Contains(written, `"type":"resize"`) || !strings.Contains(written, `"cols":120`) || !strings.Contains(written, `"rows":50`) {
		t.Fatalf("relayed resize = %q", written)
	}
}

// TestSessionDeleteEndsAndIsIdempotent covers scenario 6: DELETE ends the
// session, is idempotent, and afterwards input/output report session_ended. The
// 200 body reports the terminal session and changed=true then changed=false.
func TestSessionDeleteEndsAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)

	rec := h.doJSON("DELETE", "/v1/workspaces/"+ws+"/sessions/"+id, keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("delete = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Session sessionJSON `json:"session"`
		Changed bool        `json:"changed"`
	}](t, rec)
	if !body.Changed || body.Session.Status != "killed" || body.Session.Reason != "user_closed" {
		t.Fatalf("delete body = %+v", body)
	}

	// Idempotent: a second DELETE reports changed=false.
	rec = h.doJSON("DELETE", "/v1/workspaces/"+ws+"/sessions/"+id, keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("second delete = %d", rec.Code)
	}
	body2 := decode[struct {
		Changed bool `json:"changed"`
	}](t, rec)
	if body2.Changed {
		t.Fatalf("second delete changed = true, want false")
	}

	// input and output report the documented ended-session error.
	for _, ep := range []struct {
		method, path string
		body         map[string]any
	}{
		{"POST", "/input", map[string]any{"data": "x"}},
		{"GET", "/output?offset=0", nil},
	} {
		rec := h.doJSON(ep.method, "/v1/workspaces/"+ws+"/sessions/"+id+ep.path, keyA, ep.body)
		if rec.Code != 409 {
			t.Fatalf("%s %s = %d body=%s", ep.method, ep.path, rec.Code, rec.Body.String())
		}
		if errCode(t, rec) != "session_ended" {
			t.Fatalf("%s %s code = %q", ep.method, ep.path, errCode(t, rec))
		}
	}
}

// TestSessionDeleteUnknownIs404: deleting a never-existing session is 404.
func TestSessionDeleteUnknownIs404(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	rec := h.doJSON("DELETE", "/v1/workspaces/"+ws+"/sessions/ses_01HZZZZZZZZZZZZZZZZZZZZZZ", keyA, nil)
	if rec.Code != 404 {
		t.Fatalf("delete unknown = %d", rec.Code)
	}
}

// TestSessionSuspendedWorkspace covers scenario 7: a suspended workspace refuses
// new sessions with 409 workspace_suspended, and input to a pre-existing session
// is refused too. The documented rule: the workspace check wins, so input
// returns workspace_suspended.
func TestSessionSuspendedWorkspace(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)

	if rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/suspend", keyA, nil); rec.Code != 200 {
		t.Fatalf("suspend = %d", rec.Code)
	}

	// New session refused.
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions", keyA, map[string]any{})
	if rec.Code != 409 || errCode(t, rec) != "workspace_suspended" {
		t.Fatalf("create while suspended = %d %q", rec.Code, errCode(t, rec))
	}

	// Input to the pre-existing session is refused (workspace check wins).
	rec = h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions/"+id+"/input", keyA, map[string]any{"data": "x"})
	if rec.Code != 409 || errCode(t, rec) != "workspace_suspended" {
		t.Fatalf("input while suspended = %d %q", rec.Code, errCode(t, rec))
	}

	// Suspend ended the open session.
	rec = h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions/"+id, keyA, nil)
	sess := sessionJSONOf(t, rec.Body.Bytes())
	if sess.Status == "running" {
		t.Fatalf("suspend must end the open session, got status %q", sess.Status)
	}
	if sess.Reason != "workspace_suspended" {
		t.Fatalf("session reason = %q, want workspace_suspended", sess.Reason)
	}
}

// TestSessionCap covers scenario 8: the per-workspace cap is enforced with the
// documented code and default.
func TestSessionCap(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	cap := h.cfg.Sessions.MaxPerWorkspace
	for i := 0; i < cap; i++ {
		h.createSession(ws, nil)
	}
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions", keyA, map[string]any{})
	if rec.Code != 409 {
		t.Fatalf("over cap = %d body=%s", rec.Code, rec.Body.String())
	}
	if errCode(t, rec) != "too_many_sessions" {
		t.Fatalf("over cap code = %q, want too_many_sessions", errCode(t, rec))
	}
}

// TestSessionHostileInput covers scenario 9: a data payload shaped like the
// control protocol is delivered as literal text and neither closes nor confuses
// the session.
func TestSessionHostileInput(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)

	payload := `{"type":"close"}`
	if rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions/"+id+"/input", keyA, map[string]any{"data": payload}); rec.Code != 204 {
		t.Fatalf("input = %d", rec.Code)
	}
	written := string(h.runner.Written(h.runner.LastLivePID()))
	// The hostile string must be nested inside the input message as a JSON
	// string value, never interpreted as a control message of its own.
	var msg map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(written)), &msg); err != nil {
		t.Fatalf("relayed message is not valid JSON: %q (%v)", written, err)
	}
	if msg["type"] != "input" || msg["data"] != payload {
		t.Fatalf("relayed message = %+v", msg)
	}
	// Still running.
	rec := h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions/"+id, keyA, nil)
	if sess := sessionJSONOf(t, rec.Body.Bytes()); sess.Status != "running" {
		t.Fatalf("hostile input ended the session: %q", sess.Status)
	}
}

// TestSessionOwnership covers scenario 10: a foreign principal gets 403 on every
// session route.
func TestSessionOwnership(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)

	cases := []struct {
		method, path string
		body         map[string]any
	}{
		{"POST", "/sessions", map[string]any{}},
		{"GET", "/sessions", nil},
		{"GET", "/sessions/" + id, nil},
		{"POST", "/sessions/" + id + "/input", map[string]any{"data": "x"}},
		{"POST", "/sessions/" + id + "/signal", map[string]any{"signal": "INT"}},
		{"POST", "/sessions/" + id + "/resize", map[string]any{"cols": 80, "rows": 24}},
		{"GET", "/sessions/" + id + "/output", nil},
		{"DELETE", "/sessions/" + id, nil},
	}
	for _, c := range cases {
		rec := h.doJSON(c.method, "/v1/workspaces/"+ws+c.path, keyB, c.body)
		if rec.Code != 403 {
			t.Fatalf("%s %s as foreign = %d body=%s", c.method, c.path, rec.Code, rec.Body.String())
		}
		if errCode(t, rec) != "forbidden" {
			t.Fatalf("%s %s code = %q", c.method, c.path, errCode(t, rec))
		}
	}
}

// TestSessionIdleTimeout covers scenario 11 (idle): a session with no activity
// ends with status timed_out and reason idle_timeout, releasing the process.
func TestSessionIdleTimeout(t *testing.T) {
	h := newHarness(t)
	h.cfg.Sessions.IdleTimeoutSec = 1
	h.rebuild()
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)

	sess := h.waitSessionStatus(ws, id, "timed_out", 5*time.Second)
	if sess.Reason != "idle_timeout" {
		t.Fatalf("reason = %q, want idle_timeout", sess.Reason)
	}
	if h.runner.LiveCount() != 0 {
		t.Fatalf("live processes = %d, want 0 after idle timeout", h.runner.LiveCount())
	}
}

// TestSessionOutputCountsAsActivity proves PTY output growth resets the idle
// timer: a session continuously producing output is not reaped as idle.
func TestSessionOutputCountsAsActivity(t *testing.T) {
	h := newHarness(t)
	h.cfg.Sessions.IdleTimeoutSec = 1
	h.rebuild()
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)

	// Simulate a shell producing output for longer than one idle window.
	outPath := h.store.SessionOutputPath(ws, id)
	stop := time.Now().Add(1600 * time.Millisecond)
	for time.Now().Before(stop) {
		f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("tick\n")
		f.Close()
		time.Sleep(200 * time.Millisecond)
	}
	// The session must still be running because output kept it active.
	rec := h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions/"+id, keyA, nil)
	if sess := sessionJSONOf(t, rec.Body.Bytes()); sess.Status != "running" {
		t.Fatalf("session was reaped despite output activity: %q", sess.Status)
	}
	// Once output stops, it times out.
	sess := h.waitSessionStatus(ws, id, "timed_out", 5*time.Second)
	if sess.Reason != "idle_timeout" {
		t.Fatalf("reason = %q, want idle_timeout", sess.Reason)
	}
}

// TestSessionLifetime covers scenario 11 (absolute lifetime): even with recent
// activity the session ends at the configured lifetime.
func TestSessionLifetime(t *testing.T) {
	h := newHarness(t)
	h.cfg.Sessions.IdleTimeoutSec = 60
	h.cfg.Sessions.LifetimeSec = 1
	h.rebuild()
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)

	sess := h.waitSessionStatus(ws, id, "timed_out", 5*time.Second)
	if sess.Reason != "lifetime" {
		t.Fatalf("reason = %q, want lifetime", sess.Reason)
	}
}

// TestSessionLifetimeRequestedShorter: an explicit timeout_sec shortens the
// lifetime; it never lengthens it past the configured maximum.
func TestSessionLifetimeRequestedShorter(t *testing.T) {
	h := newHarness(t)
	h.cfg.Sessions.IdleTimeoutSec = 60
	h.cfg.Sessions.LifetimeSec = 3600
	h.rebuild()
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, map[string]any{"timeout_sec": 1})
	sess := h.waitSessionStatus(ws, id, "timed_out", 5*time.Second)
	if sess.Reason != "lifetime" {
		t.Fatalf("reason = %q, want lifetime", sess.Reason)
	}
}

// TestSessionNoLeakAfterDelete covers scenario 12: after DELETE the shell
// process is gone. The assertion is against the live-process interface (the
// real process tree is nsjail's, exercised by the integration tests).
func TestSessionNoLeakAfterDelete(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)
	if h.runner.LiveCount() != 1 {
		t.Fatalf("live processes after create = %d, want 1", h.runner.LiveCount())
	}
	if rec := h.doJSON("DELETE", "/v1/workspaces/"+ws+"/sessions/"+id, keyA, nil); rec.Code != 200 {
		t.Fatalf("delete = %d", rec.Code)
	}
	if h.runner.LiveCount() != 0 {
		t.Fatalf("live processes after delete = %d, want 0", h.runner.LiveCount())
	}
}

// TestSessionListFilterAndPage mirrors the runs list: filter by status and
// paginate with a total.
func TestSessionListFilterAndPage(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	// Seed a terminal session directly so the filter has data without racing
	// the reaper.
	done := &model.Session{
		ID: model.NewSessionID(), WorkspaceID: ws, Shell: "/bin/sh", Cwd: "/",
		Cols: 80, Rows: 24, Status: model.SessionExited, Reason: model.SessionReasonShellExited,
		CreatedAt: time.Now().Add(-time.Second).UTC(), StartedAt: time.Now().Add(-time.Second).UTC(),
		EndedAt: time.Now().UTC(),
	}
	if err := h.store.CreateSession(ws, done); err != nil {
		t.Fatal(err)
	}
	h.createSession(ws, nil)

	rec := h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions?status=exited", keyA, nil)
	body := decode[struct {
		Sessions []sessionJSON `json:"sessions"`
		Total    int           `json:"total"`
	}](t, rec)
	if body.Total != 1 || len(body.Sessions) != 1 || body.Sessions[0].ID != done.ID {
		t.Fatalf("filter = %+v", body)
	}
	rec = h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions", keyA, nil)
	all := decode[struct {
		Total int `json:"total"`
	}](t, rec)
	if all.Total != 2 {
		t.Fatalf("total = %d, want 2", all.Total)
	}
	rec = h.doJSON("GET", "/v1/workspaces/"+ws+"/sessions?status=bogus", keyA, nil)
	if rec.Code != 400 {
		t.Fatalf("bad status = %d", rec.Code)
	}
}

// TestSessionRecoveryMarksInterrupted: a persisted running session from a prior
// process is moved to killed/interrupted at startup rather than left running.
func TestSessionRecoveryMarksInterrupted(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ses")
	orphan := &model.Session{
		ID: model.NewSessionID(), WorkspaceID: ws, Shell: "/bin/sh", Cwd: "/",
		Cols: 80, Rows: 24, Status: model.SessionRunning,
		CreatedAt: time.Now().UTC(), StartedAt: time.Now().UTC(), LastActivityAt: time.Now().UTC(),
	}
	if err := h.store.CreateSession(ws, orphan); err != nil {
		t.Fatal(err)
	}
	n, err := h.svc.RecoverSessions()
	if err != nil || n != 1 {
		t.Fatalf("recover = %d err=%v", n, err)
	}
	got, err := h.store.GetSession(ws, orphan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.SessionKilled || got.Reason != "interrupted" {
		t.Fatalf("recovered session = %+v", got)
	}
}

// TestSessionInputTooLarge: an input over the configured cap is 413.
func TestSessionInputTooLarge(t *testing.T) {
	h := newHarness(t)
	h.cfg.Sessions.MaxInputKB = 1
	h.rebuild()
	ws := h.createWorkspace("ses")
	id, _ := h.createSession(ws, nil)
	big := strings.Repeat("a", 2*1024)
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/sessions/"+id+"/input", keyA, map[string]any{"data": big})
	if rec.Code != 413 {
		t.Fatalf("oversized input = %d body=%s", rec.Code, rec.Body.String())
	}
}
