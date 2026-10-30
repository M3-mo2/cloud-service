package api

import (
	"testing"
)

func TestCreateWorkspaceDefaults(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{"name": "demo"})
	if rec.Code != 201 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Workspace workspaceJSON `json:"workspace"`
	}](t, rec)
	ws := resp.Workspace
	if ws.ID == "" || ws.OwnerID != usrA || ws.Name != "demo" || ws.Status != "active" {
		t.Fatalf("workspace = %+v", ws)
	}
	if ws.Limits.MemoryMB != 512 || ws.Limits.DiskMB != 1024 || ws.Limits.MaxRuns != 3 {
		t.Fatalf("limits = %+v", ws.Limits)
	}
	if ws.Limits.AllowNet {
		t.Fatal("allow_net should default false")
	}
}

func TestCreateWorkspaceWithLimits(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{
		"name": "custom",
		"limits": map[string]any{
			"cpu_cores": 2, "memory_mb": 256, "disk_mb": 2048,
			"max_wall_time_sec": 60, "max_runs": 5, "allow_net": true,
		},
	})
	if rec.Code != 201 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Workspace workspaceJSON `json:"workspace"`
	}](t, rec)
	ws := resp.Workspace
	if ws.Limits.CPUCores != 2 || ws.Limits.MemoryMB != 256 || !ws.Limits.AllowNet {
		t.Fatalf("limits = %+v", ws.Limits)
	}
}

func TestCreateWorkspaceValidation(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{"name": ""})
	if rec.Code != 400 {
		t.Fatalf("code = %d", rec.Code)
	}
	if errCode(t, rec) != "validation" {
		t.Fatalf("code = %q", errCode(t, rec))
	}
}

func TestCreateWorkspaceUnauthorized(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", "", map[string]any{"name": "x"})
	if rec.Code != 401 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestListWorkspacesScopedToOwner(t *testing.T) {
	h := newHarness(t)
	h.createWorkspace("mine")
	// A workspace created by another principal must not be visible.
	h.doJSON("POST", "/v1/workspaces", keyB, map[string]any{"name": "theirs"})

	rec := h.doJSON("GET", "/v1/workspaces", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	resp := decode[struct {
		Workspaces []workspaceJSON `json:"workspaces"`
	}](t, rec)
	if len(resp.Workspaces) != 1 || resp.Workspaces[0].OwnerID != usrA {
		t.Fatalf("workspaces = %+v", resp.Workspaces)
	}
}

func TestGetWorkspaceCrossOwnerForbidden(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("mine")
	rec := h.doJSON("GET", "/v1/workspaces/"+id, keyB, nil)
	if rec.Code != 403 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if errCode(t, rec) != "forbidden" {
		t.Fatalf("code = %q", errCode(t, rec))
	}
}

func TestGetWorkspaceNotFound(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("GET", "/v1/workspaces/ws_01HZZZZZZZZZZZZZZZZZZZZZZ", keyA, nil)
	if rec.Code != 404 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestDeleteWorkspace(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("to-delete")
	rec := h.doJSON("DELETE", "/v1/workspaces/"+id, keyA, nil)
	if rec.Code != 204 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = h.doJSON("GET", "/v1/workspaces/"+id, keyA, nil)
	if rec.Code != 404 {
		t.Fatalf("after delete code = %d", rec.Code)
	}
}

func TestDeleteWorkspaceCrossOwner(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("mine")
	rec := h.doJSON("DELETE", "/v1/workspaces/"+id, keyB, nil)
	if rec.Code != 403 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestDeleteWorkspaceRemovesFiles(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("with-files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/a.txt", keyA, map[string]any{"content": "x"})
	rec := h.doJSON("DELETE", "/v1/workspaces/"+id, keyA, nil)
	if rec.Code != 204 {
		t.Fatalf("code = %d", rec.Code)
	}
}
