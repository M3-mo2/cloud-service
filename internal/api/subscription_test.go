package api

import (
	"testing"
	"time"

	"sandboxd/internal/model"
)

// subscriptionOf extracts the nested subscription object from a 201/200 body.
type subBody struct {
	Workspace struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		Subscription *struct {
			GrantedAt   string `json:"granted_at"`
			ExpiresAt   string `json:"expires_at"`
			GraceEndsAt string `json:"grace_ends_at"`
			Extensions  int    `json:"extensions"`
		} `json:"subscription"`
		Summary *struct {
			State  string `json:"state"`
			AgeSec int64  `json:"age_sec"`
			Runs   struct {
				Total  int `json:"total"`
				Active int `json:"active"`
			} `json:"runs"`
			Disk struct {
				UsedBytes  int64 `json:"used_bytes"`
				LimitBytes int64 `json:"limit_bytes"`
			} `json:"disk"`
			Subscription *struct {
				State        string `json:"state"`
				RemainingSec int64  `json:"remaining_sec"`
				Extensions   int    `json:"extensions"`
			} `json:"subscription"`
		} `json:"summary"`
	} `json:"workspace"`
}

func TestCreateWithDurationShowsSubscriptionAndSummary(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{
		"name": "sub", "subscription": map[string]any{"duration": "30d"},
	})
	if rec.Code != 201 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[subBody](t, rec)
	ws := body.Workspace
	if ws.Status != "active" {
		t.Fatalf("status = %q", ws.Status)
	}
	if ws.Subscription == nil || ws.Subscription.ExpiresAt == "" || ws.Subscription.Extensions != 0 {
		t.Fatalf("subscription = %+v", ws.Subscription)
	}
	if ws.Summary == nil || ws.Summary.Subscription == nil {
		t.Fatalf("summary missing: %+v", ws.Summary)
	}
	if ws.Summary.Subscription.State != "active" || ws.Summary.Subscription.RemainingSec <= 0 {
		t.Fatalf("summary subscription = %+v", ws.Summary.Subscription)
	}
}

func TestCreateWithoutSubscriptionIsUnlimited(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{"name": "free"})
	body := decode[subBody](t, rec)
	if body.Workspace.Subscription != nil {
		t.Fatal("expected no subscription")
	}
	if body.Workspace.Summary == nil || body.Workspace.Summary.State != "unlimited" {
		t.Fatalf("summary state = %+v", body.Workspace.Summary)
	}
	if body.Workspace.Summary.Subscription != nil {
		t.Fatal("unlimited summary must not carry a subscription object")
	}
}

func TestCreateSubscriptionValidation(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		sub  map[string]any
		code string
	}{
		{"zero duration", map[string]any{"duration": "0d"}, "subscription_invalid"},
		{"bad duration", map[string]any{"duration": "wat"}, "subscription_invalid"},
		{"past expires", map[string]any{"expires_at": "2000-01-01T00:00:00Z"}, "subscription_invalid"},
		{"both", map[string]any{"duration": "1d", "expires_at": "2100-01-01T00:00:00Z"}, "subscription_invalid"},
		{"over max", map[string]any{"duration": "400d"}, "max_duration_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{"name": "x", "subscription": tc.sub})
			if rec.Code != 422 {
				t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
			}
			if got := errCode(t, rec); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestExtendWhileLiveMovesForward(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{
		"name": "sub", "subscription": map[string]any{"duration": "10d"},
	})
	body := decode[subBody](t, rec)
	id := body.Workspace.ID
	before, _ := time.Parse(time.RFC3339, body.Workspace.Subscription.ExpiresAt)

	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/subscription/extend", keyA, map[string]any{"duration": "10d"})
	if rec.Code != 200 {
		t.Fatalf("extend code = %d body=%s", rec.Code, rec.Body.String())
	}
	after := decode[subBody](t, rec).Workspace
	got, _ := time.Parse(time.RFC3339, after.Subscription.ExpiresAt)
	if !got.After(before) {
		t.Fatalf("expiry did not move forward: %v -> %v", before, got)
	}
	if got.Sub(before) != 10*24*time.Hour {
		t.Fatalf("extension delta = %v, want 240h", got.Sub(before))
	}
	if after.Subscription.Extensions != 1 {
		t.Fatalf("extensions = %d", after.Subscription.Extensions)
	}
}

func TestExtendWouldShortenRefused(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{
		"name": "sub", "subscription": map[string]any{"duration": "30d"},
	})
	body := decode[subBody](t, rec)
	id := body.Workspace.ID
	// An absolute expiry 1h in the future is earlier than the live 30d term.
	earlier := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec = h.doJSON("POST", "/v1/workspaces/"+id+"/subscription/extend", keyA, map[string]any{"expires_at": earlier})
	if rec.Code != 422 || errCode(t, rec) != "subscription_would_shorten" {
		t.Fatalf("code = %d err = %q", rec.Code, errCode(t, rec))
	}
}

func TestListItemsCarrySummary(t *testing.T) {
	h := newHarness(t)
	h.createWorkspace("a")
	h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{"name": "b", "subscription": map[string]any{"duration": "1d"}})
	rec := h.doJSON("GET", "/v1/workspaces", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	list := decode[struct {
		Workspaces []struct {
			ID      string `json:"id"`
			Summary *struct {
				State string `json:"state"`
			} `json:"summary"`
		} `json:"workspaces"`
	}](t, rec)
	if len(list.Workspaces) != 2 {
		t.Fatalf("count = %d", len(list.Workspaces))
	}
	for _, w := range list.Workspaces {
		if w.Summary == nil || w.Summary.State == "" {
			t.Fatalf("list item missing summary: %+v", w)
		}
	}
}

// TestLockedRefusesComputeAllowsReads is the allow/refuse split: a locked
// (lapsed) workspace refuses exec/runs/sessions/tunnels/packages/install and
// file writes with 403 subscription_expired, while get, list, usage and file
// reads keep working, and manual delete is allowed.
func TestLockedRefusesComputeAllowsReads(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("locked")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/a.txt", keyA, map[string]any{"content": "hello"})

	// Lock it directly in storage (the reaper does this in production).
	if _, err := h.store.UpdateWorkspace(id, func(ws *model.Workspace) error {
		ws.Status = model.WorkspaceLocked
		ws.Subscription = &model.Subscription{GrantedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute), LockedAt: time.Now()}
		return nil
	}); err != nil {
		t.Fatalf("lock: %v", err)
	}

	refused := []struct {
		name, method, path string
	}{
		{"exec", "POST", "/v1/workspaces/" + id + "/exec"},
		{"runs", "POST", "/v1/workspaces/" + id + "/runs"},
		{"sessions", "POST", "/v1/workspaces/" + id + "/sessions"},
		{"tunnels", "POST", "/v1/workspaces/" + id + "/tunnels"},
		{"packages/install", "POST", "/v1/workspaces/" + id + "/packages/install"},
		{"file write", "PUT", "/v1/workspaces/" + id + "/files/content?path=/b.txt"},
		{"file mkdir", "POST", "/v1/workspaces/" + id + "/files/mkdir"},
		{"file delete", "DELETE", "/v1/workspaces/" + id + "/files?path=/a.txt"},
	}
	payloads := map[string]any{
		"exec":             map[string]any{"cmd": []string{"true"}},
		"runs":             map[string]any{"kind": "command", "argv": []string{"true"}},
		"sessions":         map[string]any{},
		"tunnels":          map[string]any{"port": 8080},
		"packages/install": map[string]any{"packages": []string{"six"}},
		"file write":       map[string]any{"content": "x"},
		"file mkdir":       map[string]any{"path": "/d"},
		"file delete":      nil,
	}
	for _, c := range refused {
		rec := h.doJSON(c.method, c.path, keyA, payloads[c.name])
		if rec.Code != 403 || errCode(t, rec) != "subscription_expired" {
			t.Fatalf("%s: code = %d err = %q (body=%s)", c.name, rec.Code, errCode(t, rec), rec.Body.String())
		}
	}

	// Reads still work, including the summary, usage and the file content.
	if rec := h.doJSON("GET", "/v1/workspaces/"+id, keyA, nil); rec.Code != 200 {
		t.Fatalf("get while locked = %d", rec.Code)
	}
	if rec := h.doJSON("GET", "/v1/workspaces", keyA, nil); rec.Code != 200 {
		t.Fatalf("list while locked = %d", rec.Code)
	}
	if rec := h.doJSON("GET", "/v1/workspaces/"+id+"/usage", keyA, nil); rec.Code != 200 {
		t.Fatalf("usage while locked = %d", rec.Code)
	}
	if rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files?path=/", keyA, nil); rec.Code != 200 {
		t.Fatalf("list files while locked = %d", rec.Code)
	}
	if rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/a.txt", keyA, nil); rec.Code != 200 {
		t.Fatalf("read file while locked = %d", rec.Code)
	}

	// The get summary says locked with remaining 0 (agrees with enforcement).
	rec := h.doJSON("GET", "/v1/workspaces/"+id, keyA, nil)
	body := decode[subBody](t, rec)
	if body.Workspace.Status != "locked" || body.Workspace.Summary.State != "locked" || body.Workspace.Summary.Subscription.RemainingSec != 0 {
		t.Fatalf("locked summary = %+v", body.Workspace)
	}

	// Manual delete of a locked workspace works and removes it.
	if rec := h.doJSON("DELETE", "/v1/workspaces/"+id, keyA, nil); rec.Code != 204 {
		t.Fatalf("delete locked = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := h.doJSON("GET", "/v1/workspaces/"+id, keyA, nil); rec.Code != 404 {
		t.Fatalf("after delete = %d", rec.Code)
	}
}

func TestExtendLockedRestoresService(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("locked")
	if _, err := h.store.UpdateWorkspace(id, func(ws *model.Workspace) error {
		ws.Status = model.WorkspaceLocked
		ws.Subscription = &model.Subscription{GrantedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute), LockedAt: time.Now()}
		return nil
	}); err != nil {
		t.Fatalf("lock: %v", err)
	}
	// Exec refused while locked.
	if rec := h.doJSON("POST", "/v1/workspaces/"+id+"/exec", keyA, map[string]any{"cmd": []string{"true"}}); rec.Code != 403 {
		t.Fatalf("exec while locked = %d", rec.Code)
	}
	// Renewal restores service.
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/subscription/extend", keyA, map[string]any{"duration": "30d"})
	if rec.Code != 200 {
		t.Fatalf("extend while locked = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[subBody](t, rec)
	if body.Workspace.Status != "active" {
		t.Fatalf("status after renewal = %q", body.Workspace.Status)
	}
	if rec := h.doJSON("POST", "/v1/workspaces/"+id+"/exec", keyA, map[string]any{"cmd": []string{"true"}}); rec.Code == 403 {
		t.Fatalf("exec after renewal still refused: %s", rec.Body.String())
	}
}

func TestMetaAdvertisesSubscriptions(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/v1/meta", reqOpts{key: keyA})
	body := decode[map[string]any](t, rec)
	cap, ok := body["subscriptions"].(map[string]any)
	if !ok {
		t.Fatalf("subscriptions capability missing: %v", body)
	}
	if cap["enabled"] != true {
		t.Fatalf("subscriptions.enabled = %v", cap["enabled"])
	}
	if _, ok := cap["grace_period"]; !ok {
		t.Fatalf("grace_period missing: %v", cap)
	}
	if _, ok := cap["duration_grammar"]; !ok {
		t.Fatalf("duration_grammar missing: %v", cap)
	}
}
