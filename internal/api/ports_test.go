package api

import (
	"testing"

	"sandboxd/internal/executor"
	"sandboxd/internal/ports"
)

// setPortProcesses configures the fake runner's process registry and port
// tables for a workspace, so the port/service inventory can be exercised through
// the HTTP contract without a real /proc or nsjail.
func setPortProcesses(h *harness, wsID string, procs []executor.ProcessInfo, tables ports.NetTables) {
	h.runner.SetProcesses(procs)
	h.runner.PortTables = tables
}

func TestListPortsEmptyReturns200(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ports-empty")
	rec := h.do("GET", "/v1/workspaces/"+ws+"/ports", reqOpts{key: keyA})
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Ports    []any  `json:"ports"`
		Count    int    `json:"count"`
		Complete bool   `json:"complete"`
		Unix     []any  `json:"unix"`
		Reach    string `json:"reachability"`
	}](t, rec)
	if body.Count != 0 || len(body.Ports) != 0 {
		t.Fatalf("want an empty list, got %+v", body)
	}
	if !body.Complete {
		t.Fatalf("complete must be true for an empty workspace")
	}
	if body.Reach == "" {
		t.Fatalf("the reachability note must be present so a client cannot mistake this for a public port")
	}
}

func TestListPortsAttributed(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ports-attr")
	procs := []executor.ProcessInfo{
		{PID: 7, WorkspaceID: ws, Kind: "run", Argv: []string{"/usr/bin/python3", "-m", "http.server", "8080"}},
	}
	tables := ports.NetTables{
		TCP: []byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
			"   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 100 2 0\n"),
		Holders:  map[uint64][]int{100: {7}},
		Complete: true,
	}
	setPortProcesses(h, ws, procs, tables)

	rec := h.do("GET", "/v1/workspaces/"+ws+"/ports", reqOpts{key: keyA})
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Ports []struct {
			Port       int    `json:"port"`
			Protocol   string `json:"protocol"`
			Scope      string `json:"scope"`
			State      string `json:"state"`
			Attributed bool   `json:"attributed"`
			PID        int    `json:"pid"`
			Kind       string `json:"kind"`
			Label      string `json:"label"`
		} `json:"ports"`
		Count int `json:"count"`
	}](t, rec)
	if body.Count != 1 {
		t.Fatalf("want 1 port, got %+v", body)
	}
	p := body.Ports[0]
	if p.Port != 8080 || p.Protocol != "tcp" || p.Scope != "wildcard" || p.State != "LISTEN" {
		t.Fatalf("endpoint = %+v", p)
	}
	if !p.Attributed || p.PID != 7 || p.Kind != "run" {
		t.Fatalf("attribution = %+v", p)
	}
	if p.Label != "python3 -m http.server 8080" {
		t.Fatalf("label = %q", p.Label)
	}
}

func TestListPortsUnattributedStillReported(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ports-unattr")
	// A registry with a process, but the socket inode is held by nobody.
	procs := []executor.ProcessInfo{{PID: 7, WorkspaceID: ws, Kind: "run", Argv: []string{"sleep", "10"}}}
	tables := ports.NetTables{
		TCP: []byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
			"   0: 0100007F:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 555 2 0\n"),
		Holders:  map[uint64][]int{},
		Complete: true,
	}
	setPortProcesses(h, ws, procs, tables)
	rec := h.do("GET", "/v1/workspaces/"+ws+"/ports", reqOpts{key: keyA})
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := decode[struct {
		Ports []struct {
			Port       int  `json:"port"`
			Attributed bool `json:"attributed"`
		} `json:"ports"`
		Count int `json:"count"`
	}](t, rec)
	if body.Count != 1 || body.Ports[0].Attributed {
		t.Fatalf("an unattributable port must still be reported, attributed=false: %+v", body)
	}
}

func TestListPortsUDPHasNoListenState(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ports-udp")
	procs := []executor.ProcessInfo{{PID: 9, WorkspaceID: ws, Kind: "run", Argv: []string{"dnsmasq"}}}
	tables := ports.NetTables{
		UDP: []byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
			"   0: 00000000:2000 00000000:0000 07 00000000:00000000 00:00000000 00000000 0 0 103 2 0\n"),
		Holders:  map[uint64][]int{103: {9}},
		Complete: true,
	}
	setPortProcesses(h, ws, procs, tables)
	rec := h.do("GET", "/v1/workspaces/"+ws+"/ports", reqOpts{key: keyA})
	body := decode[struct {
		Ports []struct {
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
			State    string `json:"state"`
		} `json:"ports"`
	}](t, rec)
	if len(body.Ports) != 1 || body.Ports[0].Protocol != "udp" {
		t.Fatalf("want one udp endpoint: %+v", body)
	}
	if body.Ports[0].State != "" {
		t.Fatalf("UDP must not be reported as LISTEN; state = %q", body.Ports[0].State)
	}
}

func TestListPortsUnixSeparateSection(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ports-unix")
	procs := []executor.ProcessInfo{{PID: 3, WorkspaceID: ws, Kind: "run", Argv: []string{"appd"}}}
	tables := ports.NetTables{
		Unix: []byte("Num       RefCount Protocol Flags    Type St Inode Path\n" +
			"0000000000000000: 00000003 00000000 00000000 0001 03 4242 /run/app.sock\n"),
		Holders:  map[uint64][]int{4242: {3}},
		Complete: true,
	}
	setPortProcesses(h, ws, procs, tables)
	rec := h.do("GET", "/v1/workspaces/"+ws+"/ports", reqOpts{key: keyA})
	body := decode[struct {
		Ports []any `json:"ports"`
		Count int   `json:"count"`
		Unix  []struct {
			Path       string `json:"path"`
			Type       string `json:"type"`
			Attributed bool   `json:"attributed"`
			PID        int    `json:"pid"`
		} `json:"unix"`
		UnixCount int `json:"unix_count"`
	}](t, rec)
	if body.Count != 0 || len(body.Ports) != 0 {
		t.Fatalf("unix sockets must never be mixed into the port list: %+v", body)
	}
	if body.UnixCount != 1 || len(body.Unix) != 1 {
		t.Fatalf("want one unix socket: %+v", body)
	}
	if body.Unix[0].Path != "/run/app.sock" || body.Unix[0].Type != "stream" || !body.Unix[0].Attributed || body.Unix[0].PID != 3 {
		t.Fatalf("unix socket = %+v", body.Unix[0])
	}
}

func TestListPortsIncompleteReports(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ports-incomplete")
	setPortProcesses(h, ws, nil, ports.NetTables{Complete: false, Notes: []string{"pid cap reached"}})
	rec := h.do("GET", "/v1/workspaces/"+ws+"/ports", reqOpts{key: keyA})
	body := decode[struct {
		Complete bool     `json:"complete"`
		Notes    []string `json:"notes"`
	}](t, rec)
	if body.Complete {
		t.Fatalf("complete must be false when a bound was hit")
	}
	if len(body.Notes) != 1 || body.Notes[0] != "pid cap reached" {
		t.Fatalf("notes = %+v", body.Notes)
	}
}

func TestListPortsDisabledIs404(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ports-disabled")
	h.cfg.Ports.Enabled = false
	h.rebuild()
	rec := h.do("GET", "/v1/workspaces/"+ws+"/ports", reqOpts{key: keyA})
	if rec.Code != 404 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if errCode(t, rec) != "ports_disabled" {
		t.Fatalf("code = %q", errCode(t, rec))
	}
}

func TestListPortsForeignPrincipalForbidden(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ports-foreign")
	rec := h.do("GET", "/v1/workspaces/"+ws+"/ports", reqOpts{key: keyB})
	if rec.Code != 403 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if errCode(t, rec) != "forbidden" {
		t.Fatalf("code = %q", errCode(t, rec))
	}
}

func TestListPortsUnknownWorkspaceNotFound(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/v1/workspaces/ws_does_not_exist/ports", reqOpts{key: keyA})
	if rec.Code != 404 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestListPortsRequiresAuth(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ports-auth")
	rec := h.do("GET", "/v1/workspaces/"+ws+"/ports", reqOpts{})
	if rec.Code != 401 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestListPortsAppliesConfiguredBounds(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ports-bounds")
	h.cfg.Ports.MaxPids = 5
	h.cfg.Ports.MaxFDsPerPID = 33
	h.rebuild()
	// Recreate the workspace on the rebuilt service is unnecessary: the record
	// persists. Issue the read and confirm the fake recorded the limits.
	rec := h.do("GET", "/v1/workspaces/"+ws+"/ports", reqOpts{key: keyA})
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := h.runner.LastPortLimits; got.MaxPids != 5 || got.MaxFDsPerPID != 33 {
		t.Fatalf("configured bounds not passed to the runner: %+v", got)
	}
}

func TestMetaAdvertisesPorts(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/v1/meta", reqOpts{key: keyA})
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := decode[map[string]any](t, rec)
	cap, ok := body["ports"].(map[string]any)
	if !ok {
		t.Fatalf("meta has no ports capability: %v", body)
	}
	if cap["enabled"] != true {
		t.Fatalf("ports.enabled = %v, want true by default", cap["enabled"])
	}
	if _, ok := cap["max_pids"]; !ok {
		t.Fatalf("ports capability must report max_pids: %v", cap)
	}
	reach, _ := cap["reachability"].(string)
	if reach == "" {
		t.Fatalf("ports capability must carry the reachability note: %v", cap)
	}
}

// TestPortsIsolationHostNotLeaked is the unit-level isolation assertion: an
// endpoint held by a pid OUTSIDE the workspace registry is never attributed to
// the workspace. The service only ever passes the workspace's own pids to the
// runner, so a foreign socket (the host's, or another tenant's) cannot appear.
func TestPortsIsolationHostNotLeaked(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("ports-isolation")
	// The fake returns a table whose socket is held by pid 999, which is NOT in
	// this workspace's registry (the registry holds pid 7 only).
	procs := []executor.ProcessInfo{{PID: 7, WorkspaceID: ws, Kind: "run", Argv: []string{"app"}}}
	tables := ports.NetTables{
		TCP: []byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
			"   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 777 2 0\n"),
		Holders:  map[uint64][]int{777: {999}},
		Complete: true,
	}
	setPortProcesses(h, ws, procs, tables)
	rec := h.do("GET", "/v1/workspaces/"+ws+"/ports", reqOpts{key: keyA})
	body := decode[struct {
		Ports []struct {
			Port       int  `json:"port"`
			Attributed bool `json:"attributed"`
			PID        int  `json:"pid"`
		} `json:"ports"`
		Count int `json:"count"`
	}](t, rec)
	if body.Count != 1 {
		t.Fatalf("want 1 port, got %+v", body)
	}
	if body.Ports[0].Attributed {
		t.Fatalf("a socket held by a pid outside the workspace must not be attributed: %+v", body.Ports[0])
	}
}
