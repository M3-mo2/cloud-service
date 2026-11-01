package api

import (
	"fmt"
	"testing"
)

// setupTunnels rebuilds the harness with tunnels enabled and a fake network,
// and returns the fake so a test can drive URL capture and process liveness.
func (h *harness) setupTunnels(serverEnabled, workspaceDefault bool, allowNet bool) *fakeNetwork {
	fake := newFakeNetwork()
	fake.tunnelsEnabled = serverEnabled
	h.cfg.Tunnels.Enabled = serverEnabled
	h.cfg.Tunnels.MaxPerWorkspace = 2
	h.cfg.Network.Enabled = true
	h.cfg.Limits.AllowTunnels = workspaceDefault
	h.cfg.Limits.AllowNet = allowNet
	h.rebuild()
	h.svc.SetNetwork(fake)
	return fake
}

func TestCreateTunnelHappyPath(t *testing.T) {
	h := newHarness(t)
	fake := h.setupTunnels(true, true, true)
	ws := h.createWorkspace("tun")

	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/tunnels", keyA, map[string]any{"port": 5000})
	if rec.Code != 201 {
		t.Fatalf("create tunnel: %d %s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Tunnel struct {
			ID          string `json:"id"`
			WorkspaceID string `json:"workspace_id"`
			Port        int    `json:"port"`
			URL         string `json:"url"`
			Status      string `json:"status"`
		} `json:"tunnel"`
	}](t, rec)
	if resp.Tunnel.Port != 5000 {
		t.Fatalf("port mismatch: %+v", resp.Tunnel)
	}
	if resp.Tunnel.URL == "" || resp.Tunnel.Status != "active" {
		t.Fatalf("expected an observed URL and active status: %+v", resp.Tunnel)
	}
	if fake.tunnelCount() != 1 {
		t.Fatalf("expected one live tunnel, got %d", fake.tunnelCount())
	}

	// GET single returns it.
	rec = h.doJSON("GET", "/v1/workspaces/"+ws+"/tunnels/"+resp.Tunnel.ID, keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("get tunnel: %d", rec.Code)
	}
	// GET list returns it.
	rec = h.doJSON("GET", "/v1/workspaces/"+ws+"/tunnels", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("list tunnels: %d", rec.Code)
	}
	list := decode[struct {
		Tunnels []struct {
			ID string `json:"id"`
		} `json:"tunnels"`
	}](t, rec)
	if len(list.Tunnels) != 1 {
		t.Fatalf("expected one tunnel in list, got %d", len(list.Tunnels))
	}
}

func TestTunnelsDisabledByServer(t *testing.T) {
	h := newHarness(t)
	h.setupTunnels(false, true, true)
	ws := h.createWorkspace("tun")
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/tunnels", keyA, map[string]any{"port": 5000})
	if rec.Code != 422 {
		t.Fatalf("server-disabled create must be 422, got %d", rec.Code)
	}
	if c := errCode(t, rec); c != "tunnels_disabled" {
		t.Fatalf("expected tunnels_disabled, got %q", c)
	}
}

func TestTunnelsDisabledByWorkspace(t *testing.T) {
	h := newHarness(t)
	h.setupTunnels(true, false, true)
	ws := h.createWorkspace("tun")
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/tunnels", keyA, map[string]any{"port": 5000})
	if rec.Code != 422 {
		t.Fatalf("workspace-not-opted-in create must be 422, got %d", rec.Code)
	}
	if c := errCode(t, rec); c != "tunnels_disabled" {
		t.Fatalf("expected tunnels_disabled, got %q", c)
	}
}

func TestTunnelsNeedEgress(t *testing.T) {
	h := newHarness(t)
	// Opted in to tunnels but NOT to egress: cloudflared cannot reach Cloudflare.
	h.setupTunnels(true, true, false)
	ws := h.createWorkspace("tun")
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/tunnels", keyA, map[string]any{"port": 5000})
	if rec.Code != 422 {
		t.Fatalf("tunnel without egress must be 422, got %d (%s)", rec.Code, rec.Body.String())
	}
	if c := errCode(t, rec); c != "tunnels_disabled" {
		t.Fatalf("expected tunnels_disabled, got %q", c)
	}
}

func TestTunnelsIsolationBetweenPrincipals(t *testing.T) {
	h := newHarness(t)
	h.setupTunnels(true, true, true)
	wA := h.createWorkspace("a")
	recB := h.doJSON("POST", "/v1/workspaces", keyB, map[string]any{
		"name":   "b",
		"limits": map[string]any{"allow_tunnels": true, "allow_net": true},
	})
	if recB.Code != 201 {
		t.Fatalf("create b: %d %s", recB.Code, recB.Body.String())
	}
	wB := decode[struct {
		Workspace struct {
			ID string `json:"id"`
		} `json:"workspace"`
	}](t, recB).Workspace.ID

	a := createTunnel(t, h, keyA, wA, 5000)
	createTunnel(t, h, keyB, wB, 5000)

	if code := h.do("GET", "/v1/workspaces/"+wA+"/tunnels/"+a.ID, reqOpts{key: keyB}).Code; code != 403 {
		t.Fatalf("foreign read must be 403, got %d", code)
	}
	if code := h.do("GET", "/v1/workspaces/"+wA+"/tunnels", reqOpts{key: keyB}).Code; code != 403 {
		t.Fatalf("foreign list must be 403, got %d", code)
	}
	if code := h.do("DELETE", "/v1/workspaces/"+wA+"/tunnels/"+a.ID, reqOpts{key: keyB}).Code; code != 403 {
		t.Fatalf("foreign delete must be 403, got %d", code)
	}
}

func TestTunnelsUnknownIDs(t *testing.T) {
	h := newHarness(t)
	h.setupTunnels(true, true, true)
	ws := h.createWorkspace("tun")
	if code := h.do("GET", "/v1/workspaces/"+ws+"/tunnels/tun_missing", reqOpts{key: keyA}).Code; code != 404 {
		t.Fatalf("unknown tunnel read must be 404, got %d", code)
	}
	if code := h.do("GET", "/v1/workspaces/ws_missing/tunnels", reqOpts{key: keyA}).Code; code != 404 {
		t.Fatalf("unknown workspace must be 404, got %d", code)
	}
	if code := h.do("DELETE", "/v1/workspaces/"+ws+"/tunnels/tun_missing", reqOpts{key: keyA}).Code; code != 404 {
		t.Fatalf("unknown tunnel delete must be 404, got %d", code)
	}
}

func TestTunnelsBadBody(t *testing.T) {
	h := newHarness(t)
	h.setupTunnels(true, true, true)
	ws := h.createWorkspace("tun")
	cases := []map[string]any{
		{"port": 0},
		{"port": 70000},
		{"port": 80}, // privileged
	}
	for _, body := range cases {
		rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/tunnels", keyA, body)
		if rec.Code != 400 {
			t.Fatalf("body %+v must be 400, got %d (%s)", body, rec.Code, rec.Body.String())
		}
	}
}

func TestTunnelsMaxPerWorkspace(t *testing.T) {
	h := newHarness(t)
	h.setupTunnels(true, true, true)
	ws := h.createWorkspace("tun")
	createTunnel(t, h, keyA, ws, 5000)
	createTunnel(t, h, keyA, ws, 5001)
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/tunnels", keyA, map[string]any{"port": 5002})
	if rec.Code != 422 {
		t.Fatalf("third tunnel must be 422, got %d", rec.Code)
	}
	if c := errCode(t, rec); c != "too_many_tunnels" {
		t.Fatalf("expected too_many_tunnels, got %q", c)
	}
}

// TestTunnelNoURLIs504 proves the engine never fabricates a URL: when
// cloudflared does not print one within the timeout, the create fails with the
// documented machine-readable 5xx.
func TestTunnelNoURLIs504(t *testing.T) {
	h := newHarness(t)
	fake := h.setupTunnels(true, true, true)
	fake.tunnelErr = errNoURL
	ws := h.createWorkspace("tun")
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/tunnels", keyA, map[string]any{"port": 5000})
	if rec.Code != 504 {
		t.Fatalf("no-URL create must be 504, got %d (%s)", rec.Code, rec.Body.String())
	}
	if c := errCode(t, rec); c != "tunnel_url_unavailable" {
		t.Fatalf("expected tunnel_url_unavailable, got %q", c)
	}
	// The failed record must not be left behind.
	list := h.doJSON("GET", "/v1/workspaces/"+ws+"/tunnels", keyA, nil)
	got := decode[struct {
		Tunnels []struct{} `json:"tunnels"`
	}](t, list)
	if len(got.Tunnels) != 0 {
		t.Fatalf("a failed create must leave no record, got %d", len(got.Tunnels))
	}
}

func TestDeleteTunnelStopsProcessAndRecord(t *testing.T) {
	h := newHarness(t)
	fake := h.setupTunnels(true, true, true)
	ws := h.createWorkspace("tun")
	p := createTunnel(t, h, keyA, ws, 5000)
	if code := h.do("DELETE", "/v1/workspaces/"+ws+"/tunnels/"+p.ID, reqOpts{key: keyA}).Code; code != 204 {
		t.Fatalf("delete tunnel must be 204, got %d", code)
	}
	if code := h.do("GET", "/v1/workspaces/"+ws+"/tunnels/"+p.ID, reqOpts{key: keyA}).Code; code != 404 {
		t.Fatalf("removed tunnel must be 404, got %d", code)
	}
	if fake.tunnelCount() != 0 {
		t.Fatalf("cloudflared must be stopped, got %d", fake.tunnelCount())
	}
}

func TestStopSuspendDeleteReleaseTunnels(t *testing.T) {
	h := newHarness(t)
	fake := h.setupTunnels(true, true, true)
	ws := h.createWorkspace("tun")
	createTunnel(t, h, keyA, ws, 5000)
	if code := h.do("POST", "/v1/workspaces/"+ws+"/stop", reqOpts{key: keyA}).Code; code != 200 {
		t.Fatalf("stop: %d", code)
	}
	if fake.tunnelCount() != 0 {
		t.Fatalf("stop must release tunnels, %d left", fake.tunnelCount())
	}

	createTunnel(t, h, keyA, ws, 5001)
	if code := h.do("POST", "/v1/workspaces/"+ws+"/suspend", reqOpts{key: keyA}).Code; code != 200 {
		t.Fatalf("suspend: %d", code)
	}
	if fake.tunnelCount() != 0 {
		t.Fatalf("suspend must release tunnels, %d left", fake.tunnelCount())
	}

	if code := h.do("POST", "/v1/workspaces/"+ws+"/resume", reqOpts{key: keyA}).Code; code != 200 {
		t.Fatalf("resume: %d", code)
	}
	createTunnel(t, h, keyA, ws, 5002)
	if code := h.do("DELETE", "/v1/workspaces/"+ws, reqOpts{key: keyA}).Code; code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if fake.tunnelCount() != 0 {
		t.Fatalf("delete must release tunnels, %d left", fake.tunnelCount())
	}
}

func TestPatchAllowTunnelsRejectedWhenServerDisabled(t *testing.T) {
	h := newHarness(t)
	h.setupTunnels(false, false, true)
	ws := h.createWorkspace("tun")
	rec := h.doJSON("PATCH", "/v1/workspaces/"+ws, keyA, map[string]any{
		"limits": map[string]any{"allow_tunnels": true},
	})
	if rec.Code != 422 {
		t.Fatalf("PATCH allow_tunnels=true with server disabled must be 422, got %d (%s)", rec.Code, rec.Body.String())
	}
	if c := errCode(t, rec); c != "tunnels_disabled" {
		t.Fatalf("expected tunnels_disabled, got %q", c)
	}
	// The persisted workspace must NOT have the flag set (F-22 not repeated).
	rec = h.doJSON("GET", "/v1/workspaces/"+ws, keyA, nil)
	got := decode[struct {
		Workspace struct {
			Limits struct {
				AllowTunnels bool `json:"allow_tunnels"`
			} `json:"limits"`
		} `json:"workspace"`
	}](t, rec)
	if got.Workspace.Limits.AllowTunnels {
		t.Fatal("PATCH persisted allow_tunnels despite the server switch being off")
	}
}

func TestCreateWorkspaceAllowTunnelsRejectedWhenServerDisabled(t *testing.T) {
	h := newHarness(t)
	h.setupTunnels(false, false, true)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{
		"name":   "x",
		"limits": map[string]any{"allow_tunnels": true},
	})
	if rec.Code != 422 {
		t.Fatalf("create with allow_tunnels when server disabled must be 422, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestMetaReportsTunnelsCapability(t *testing.T) {
	h := newHarness(t)
	h.setupTunnels(true, true, true)
	rec := h.doJSON("GET", "/v1/meta", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("meta: %d", rec.Code)
	}
	got := decode[struct {
		Tunnels struct {
			Enabled bool `json:"enabled"`
			Ready   bool `json:"ready"`
		} `json:"tunnels"`
		Network struct {
			Available bool `json:"available"`
			Egress    bool `json:"egress"`
		} `json:"network"`
	}](t, rec)
	if !got.Tunnels.Enabled || !got.Tunnels.Ready {
		t.Fatalf("meta must report tunnels enabled+ready: %+v", got.Tunnels)
	}
	if !got.Network.Available || !got.Network.Egress {
		t.Fatalf("meta must report the network available with egress: %+v", got.Network)
	}
}

type pubTunnel struct {
	ID string
}

func createTunnel(t *testing.T, h *harness, key, ws string, port int) pubTunnel {
	t.Helper()
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/tunnels", key, map[string]any{"port": port})
	if rec.Code != 201 {
		t.Fatalf("create tunnel %d: %d %s", port, rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Tunnel struct {
			ID string `json:"id"`
		} `json:"tunnel"`
	}](t, rec)
	return pubTunnel{ID: resp.Tunnel.ID}
}

// errNoURL is the failure the real network layer returns when cloudflared does
// not print a URL within the timeout. The fake returns it directly.
var errNoURL = fmt.Errorf("no tunnel URL observed within the timeout")
