package api

import (
	"testing"
)

// setupPorts rebuilds the harness with ports enabled and a fake network, and
// returns the fake so a test can drive reachability.
func (h *harness) setupPorts(serverEnabled, workspaceDefault bool) *fakeNetwork {
	fake := newFakeNetwork()
	fake.enabled = serverEnabled
	h.cfg.Ports.Enabled = serverEnabled
	h.cfg.Ports.MaxPerWorkspace = 2
	h.cfg.Ports.MaxPerPrincipal = 5
	h.cfg.Limits.AllowPorts = workspaceDefault
	h.rebuild()
	h.svc.SetNetwork(fake)
	return fake
}

func TestPublishPortHappyPath(t *testing.T) {
	h := newHarness(t)
	fake := h.setupPorts(true, true)
	ws := h.createWorkspace("ports")

	// Before the app binds, the honest status is unreachable.
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/ports", keyA, map[string]any{"port": 3000})
	if rec.Code != 201 {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Port struct {
			ID       string `json:"id"`
			HostPort int    `json:"host_port"`
			Status   string `json:"status"`
			Port     int    `json:"port"`
		} `json:"port"`
	}](t, rec)
	if resp.Port.Port != 3000 {
		t.Fatalf("port mismatch: %+v", resp.Port)
	}
	if resp.Port.HostPort < 32000 || resp.Port.HostPort > 32999 {
		t.Fatalf("host port outside the fenced range: %d", resp.Port.HostPort)
	}
	if resp.Port.Status != "unreachable" {
		t.Fatalf("honest initial status must be unreachable, got %q", resp.Port.Status)
	}
	if fake.listenerCount() != 1 {
		t.Fatalf("expected one live forwarder, got %d", fake.listenerCount())
	}

	// Once the app binds (fake), the status flips to published.
	fake.setReachable(ws, 3000, true)
	rec = h.doJSON("GET", "/v1/workspaces/"+ws+"/ports/"+resp.Port.ID, keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("read: %d", rec.Code)
	}
	one := decode[struct {
		Port struct {
			Status string `json:"status"`
		} `json:"port"`
	}](t, rec)
	if one.Port.Status != "published" {
		t.Fatalf("status must become published, got %q", one.Port.Status)
	}
}

func TestPortsDisabledByServer(t *testing.T) {
	h := newHarness(t)
	h.setupPorts(false, true)
	ws := h.createWorkspace("ports")
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/ports", keyA, map[string]any{"port": 3000})
	if rec.Code != 422 {
		t.Fatalf("server-disabled publish must be 422, got %d", rec.Code)
	}
	if c := errCode(t, rec); c != "ports_disabled" {
		t.Fatalf("expected ports_disabled, got %q", c)
	}
}

func TestPortsDisabledByWorkspace(t *testing.T) {
	h := newHarness(t)
	h.setupPorts(true, false)
	ws := h.createWorkspace("ports")
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/ports", keyA, map[string]any{"port": 3000})
	if rec.Code != 422 {
		t.Fatalf("workspace-not-opted-in publish must be 422, got %d", rec.Code)
	}
	if c := errCode(t, rec); c != "ports_disabled" {
		t.Fatalf("expected ports_disabled, got %q", c)
	}
}

func TestPortsIsolationBetweenPrincipals(t *testing.T) {
	h := newHarness(t)
	h.setupPorts(true, true)
	// Two workspaces owned by different principals, both publishing 3000.
	wA := h.createWorkspace("a")
	recB := h.doJSON("POST", "/v1/workspaces", keyB, map[string]any{"name": "b", "limits": map[string]any{"allow_ports": true}})
	if recB.Code != 201 {
		t.Fatalf("create b: %d %s", recB.Code, recB.Body.String())
	}
	wB := decode[struct {
		Workspace struct {
			ID string `json:"id"`
		} `json:"workspace"`
	}](t, recB).Workspace.ID

	a := publish(t, h, keyA, wA, 3000)
	b := publish(t, h, keyB, wB, 3000)
	if a.HostPort == b.HostPort {
		t.Fatalf("different principals must get different host ports: %d == %d", a.HostPort, b.HostPort)
	}
	// Foreign principal cannot read, list, or delete A's port.
	if code := h.do("GET", "/v1/workspaces/"+wA+"/ports/"+a.ID, reqOpts{key: keyB}).Code; code != 403 {
		t.Fatalf("foreign read must be 403, got %d", code)
	}
	if code := h.do("GET", "/v1/workspaces/"+wA+"/ports", reqOpts{key: keyB}).Code; code != 403 {
		t.Fatalf("foreign list must be 403, got %d", code)
	}
	if code := h.do("DELETE", "/v1/workspaces/"+wA+"/ports/"+a.ID, reqOpts{key: keyB}).Code; code != 403 {
		t.Fatalf("foreign delete must be 403, got %d", code)
	}
}

func TestPortsUnknownIDs(t *testing.T) {
	h := newHarness(t)
	h.setupPorts(true, true)
	ws := h.createWorkspace("ports")
	if code := h.do("GET", "/v1/workspaces/"+ws+"/ports/prt_missing", reqOpts{key: keyA}).Code; code != 404 {
		t.Fatalf("unknown port read must be 404, got %d", code)
	}
	if code := h.do("GET", "/v1/workspaces/ws_missing/ports", reqOpts{key: keyA}).Code; code != 404 {
		t.Fatalf("unknown workspace must be 404, got %d", code)
	}
	if code := h.do("DELETE", "/v1/workspaces/"+ws+"/ports/prt_missing", reqOpts{key: keyA}).Code; code != 404 {
		t.Fatalf("unknown port delete must be 404, got %d", code)
	}
}

func TestPortsBadBody(t *testing.T) {
	h := newHarness(t)
	h.setupPorts(true, true)
	ws := h.createWorkspace("ports")
	cases := []map[string]any{
		{"port": 0},
		{"port": 70000},
		{"port": 80}, // privileged
		{"port": 3000, "protocol": "udp"},
	}
	for _, body := range cases {
		rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/ports", keyA, body)
		if rec.Code != 400 {
			t.Fatalf("body %+v must be 400, got %d (%s)", body, rec.Code, rec.Body.String())
		}
	}
}

func TestPortsMaxPerWorkspace(t *testing.T) {
	h := newHarness(t)
	h.setupPorts(true, true)
	ws := h.createWorkspace("ports")
	publish(t, h, keyA, ws, 3000)
	publish(t, h, keyA, ws, 3001)
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/ports", keyA, map[string]any{"port": 3002})
	if rec.Code != 422 {
		t.Fatalf("third publish must be 422, got %d", rec.Code)
	}
	if c := errCode(t, rec); c != "too_many_ports" {
		t.Fatalf("expected too_many_ports, got %q", c)
	}
	// Listing shows exactly the cap, nothing extra was published.
	list := h.doJSON("GET", "/v1/workspaces/"+ws+"/ports", keyA, nil)
	got := decode[struct {
		Ports []struct{} `json:"ports"`
	}](t, list)
	if len(got.Ports) != 2 {
		t.Fatalf("expected exactly 2 ports, got %d", len(got.Ports))
	}
}

func TestUnpublishFreesPortAndRecord(t *testing.T) {
	h := newHarness(t)
	fake := h.setupPorts(true, true)
	ws := h.createWorkspace("ports")
	p := publish(t, h, keyA, ws, 3000)
	if code := h.do("DELETE", "/v1/workspaces/"+ws+"/ports/"+p.ID, reqOpts{key: keyA}).Code; code != 204 {
		t.Fatalf("unpublish must be 204, got %d", code)
	}
	if code := h.do("GET", "/v1/workspaces/"+ws+"/ports/"+p.ID, reqOpts{key: keyA}).Code; code != 404 {
		t.Fatalf("removed port must be 404, got %d", code)
	}
	if fake.listenerCount() != 0 {
		t.Fatalf("forwarder must be stopped, got %d", fake.listenerCount())
	}
}

func TestStopSuspendDeleteReleasePorts(t *testing.T) {
	h := newHarness(t)
	fake := h.setupPorts(true, true)
	ws := h.createWorkspace("ports")
	publish(t, h, keyA, ws, 3000)
	if code := h.do("POST", "/v1/workspaces/"+ws+"/stop", reqOpts{key: keyA}).Code; code != 200 {
		t.Fatalf("stop: %d", code)
	}
	if fake.listenerCount() != 0 {
		t.Fatalf("stop must release ports, %d left", fake.listenerCount())
	}

	publish(t, h, keyA, ws, 3001)
	if code := h.do("POST", "/v1/workspaces/"+ws+"/suspend", reqOpts{key: keyA}).Code; code != 200 {
		t.Fatalf("suspend: %d", code)
	}
	if fake.listenerCount() != 0 {
		t.Fatalf("suspend must release ports, %d left", fake.listenerCount())
	}

	if code := h.do("POST", "/v1/workspaces/"+ws+"/resume", reqOpts{key: keyA}).Code; code != 200 {
		t.Fatalf("resume: %d", code)
	}
	publish(t, h, keyA, ws, 3002)
	if code := h.do("DELETE", "/v1/workspaces/"+ws, reqOpts{key: keyA}).Code; code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if fake.listenerCount() != 0 {
		t.Fatalf("delete must release ports, %d left", fake.listenerCount())
	}
}

func TestPatchAllowPortsRejectedWhenServerDisabled(t *testing.T) {
	h := newHarness(t)
	h.setupPorts(false, false)
	ws := h.createWorkspace("ports")
	rec := h.doJSON("PATCH", "/v1/workspaces/"+ws, keyA, map[string]any{
		"limits": map[string]any{"allow_ports": true},
	})
	if rec.Code != 422 {
		t.Fatalf("PATCH allow_ports=true with server disabled must be 422, got %d (%s)", rec.Code, rec.Body.String())
	}
	if c := errCode(t, rec); c != "ports_disabled" {
		t.Fatalf("expected ports_disabled, got %q", c)
	}
	// The persisted workspace must NOT have the flag set (F-22 not repeated).
	rec = h.doJSON("GET", "/v1/workspaces/"+ws, keyA, nil)
	got := decode[struct {
		Workspace struct {
			Limits struct {
				AllowPorts bool `json:"allow_ports"`
			} `json:"limits"`
		} `json:"workspace"`
	}](t, rec)
	if got.Workspace.Limits.AllowPorts {
		t.Fatal("PATCH persisted allow_ports despite the server switch being off")
	}
}

func TestCreateWorkspaceAllowPortsRejectedWhenServerDisabled(t *testing.T) {
	h := newHarness(t)
	h.setupPorts(false, false)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{
		"name":   "x",
		"limits": map[string]any{"allow_ports": true},
	})
	if rec.Code != 422 {
		t.Fatalf("create with allow_ports when server disabled must be 422, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestMetaReportsPortsCapability(t *testing.T) {
	h := newHarness(t)
	h.setupPorts(true, true)
	rec := h.doJSON("GET", "/v1/meta", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("meta: %d", rec.Code)
	}
	got := decode[struct {
		Ports struct {
			Enabled bool `json:"enabled"`
			Ready   bool `json:"ready"`
		} `json:"ports"`
		Network struct {
			Available bool `json:"available"`
		} `json:"network"`
	}](t, rec)
	if !got.Ports.Enabled || !got.Ports.Ready {
		t.Fatalf("meta must report ports enabled+ready: %+v", got.Ports)
	}
	if !got.Network.Available {
		t.Fatalf("meta must report the network available: %+v", got.Network)
	}
}

type pubPort struct {
	ID       string
	HostPort int
}

func publish(t *testing.T, h *harness, key, ws string, port int) pubPort {
	t.Helper()
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/ports", key, map[string]any{"port": port})
	if rec.Code != 201 {
		t.Fatalf("publish port %d: %d %s", port, rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Port struct {
			ID       string `json:"id"`
			HostPort int    `json:"host_port"`
		} `json:"port"`
	}](t, rec)
	return pubPort{ID: resp.Port.ID, HostPort: resp.Port.HostPort}
}
