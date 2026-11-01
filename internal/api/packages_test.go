package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDistInfo(t *testing.T, dir, name, version string) {
	t.Helper()
	d := filepath.Join(dir, name+"-"+version+".dist-info")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\n"
	if err := os.WriteFile(filepath.Join(d, "METADATA"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListPackagesEmpty(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("pkgs")

	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/packages", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Packages        []map[string]string `json:"packages"`
		Count           int                 `json:"count"`
		Truncated       bool                `json:"truncated"`
		SharedEnabled   bool                `json:"shared_enabled"`
		SharedAvailable bool                `json:"shared_available"`
		PrivatePath     string              `json:"private_path"`
	}](t, rec)
	if body.Count != 0 || len(body.Packages) != 0 {
		t.Fatalf("body = %+v", body)
	}
	if body.PrivatePath != "/opt/python/private" {
		t.Fatalf("private_path = %q", body.PrivatePath)
	}
}

func TestListPackagesReportsSharedTier(t *testing.T) {
	h := newHarness(t)
	shared := t.TempDir()
	h.cfg.Packages.SharedEnabled = true
	h.cfg.Packages.SharedDir = shared
	// The harness server copies cfg at construction; rebuild the router so the
	// changed package config is observed by the service (svc holds the config).
	h.rebuild()
	id := h.createWorkspace("pkgs")

	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/packages", keyA, nil)
	body := decode[struct {
		SharedEnabled   bool   `json:"shared_enabled"`
		SharedAvailable bool   `json:"shared_available"`
		SharedPath      string `json:"shared_path"`
	}](t, rec)
	if !body.SharedEnabled || !body.SharedAvailable {
		t.Fatalf("shared tier = %+v", body)
	}
	if body.SharedPath != "/opt/python/shared" {
		t.Fatalf("shared_path = %q", body.SharedPath)
	}
}

func TestListPackagesMissingSharedUnavailable(t *testing.T) {
	h := newHarness(t)
	h.cfg.Packages.SharedEnabled = true
	h.cfg.Packages.SharedDir = filepath.Join(t.TempDir(), "nope")
	h.rebuild()
	id := h.createWorkspace("pkgs")
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/packages", keyA, nil)
	body := decode[struct {
		SharedEnabled   bool `json:"shared_enabled"`
		SharedAvailable bool `json:"shared_available"`
	}](t, rec)
	if !body.SharedEnabled || body.SharedAvailable {
		t.Fatalf("missing shared dir must be unavailable: %+v", body)
	}
}

func TestInstallPackagesHostileRejected(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("pkgs")

	hostile := []string{
		"-e", "--upgrade", "pkg; rm -rf /", "../../etc/passwd", "", " ", "\n",
		"pkg && cat /etc/passwd", "pkg`id`", "$(whoami)", "pkg/../evil",
		strings.Repeat("a", 10000),
	}
	for _, req := range hostile {
		t.Run(req, func(t *testing.T) {
			rec := h.doJSON("POST", "/v1/workspaces/"+id+"/packages/install", keyA,
				map[string]any{"packages": []string{req}})
			if rec.Code != 422 {
				t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
			}
			if errCode(t, rec) != "unprocessable" {
				t.Fatalf("code = %q", errCode(t, rec))
			}
		})
	}
	if h.runner.LastRequest() != nil {
		t.Fatal("runner must never be invoked for rejected requirements")
	}
}

func TestInstallPackagesEmptyListRejected(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("pkgs")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/packages/install", keyA,
		map[string]any{"packages": []string{}})
	if rec.Code != 422 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestInstallPackagesSuccess(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.Stdout = "Successfully installed requests-2.31.0\n"
	id := h.createWorkspace("pkgs")

	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/packages/install", keyA,
		map[string]any{"packages": []string{"requests==2.31.0"}})
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		ExitCode   int    `json:"exit_code"`
		Output     string `json:"output"`
		TimedOut   bool   `json:"timed_out"`
		Target     string `json:"target"`
		DurationMS int64  `json:"duration_ms"`
	}](t, rec)
	if body.ExitCode != 0 || !strings.Contains(body.Output, "Successfully installed") {
		t.Fatalf("body = %+v", body)
	}
	if body.Target != "/opt/python/private" {
		t.Fatalf("target = %q", body.Target)
	}
	req := h.runner.LastRequest()
	if req == nil {
		t.Fatal("runner not invoked")
	}
	joined := strings.Join(req.Argv, " ")
	if !strings.Contains(joined, "--target /opt/python/private") {
		t.Fatalf("install must target the private tier: %v", req.Argv)
	}
}

func TestInstallPackagesSuspended(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	id := h.createWorkspace("pkgs")
	if rec := h.doJSON("POST", "/v1/workspaces/"+id+"/suspend", keyA, nil); rec.Code != 200 {
		t.Fatalf("suspend = %d", rec.Code)
	}
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/packages/install", keyA,
		map[string]any{"packages": []string{"requests"}})
	if rec.Code != 409 || errCode(t, rec) != "workspace_suspended" {
		t.Fatalf("code = %d err = %q", rec.Code, errCode(t, rec))
	}
}

func TestListPackagesSuspended(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("pkgs")
	if rec := h.doJSON("POST", "/v1/workspaces/"+id+"/suspend", keyA, nil); rec.Code != 200 {
		t.Fatalf("suspend = %d", rec.Code)
	}
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/packages", keyA, nil)
	if rec.Code != 409 || errCode(t, rec) != "workspace_suspended" {
		t.Fatalf("code = %d err = %q", rec.Code, errCode(t, rec))
	}
}

func TestDeleteAllPackagesSuspended(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("pkgs")
	if rec := h.doJSON("POST", "/v1/workspaces/"+id+"/suspend", keyA, nil); rec.Code != 200 {
		t.Fatalf("suspend = %d", rec.Code)
	}
	rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/packages?all=true", keyA, nil)
	if rec.Code != 409 || errCode(t, rec) != "workspace_suspended" {
		t.Fatalf("code = %d err = %q", rec.Code, errCode(t, rec))
	}
}

func TestDeletePackage(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("pkgs")
	dir := h.store.WorkspacePackagesDir(id)
	writeDistInfo(t, dir, "foo", "1.0.0")
	if err := os.WriteFile(filepath.Join(dir, "foo.py"), []byte("x=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "foo-1.0.0.dist-info", "RECORD"),
		[]byte("foo.py,sha256=x,4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/packages?name=foo", keyA, nil); rec.Code != 204 {
		t.Fatalf("delete code = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/packages?name=foo", keyA, nil); rec.Code != 404 {
		t.Fatalf("missing delete code = %d", rec.Code)
	}
}

func TestDeletePackagesRequiresNameOrFlag(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("pkgs")
	rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/packages", keyA, nil)
	if rec.Code != 400 {
		t.Fatalf("ambiguous delete code = %d body=%s", rec.Code, rec.Body.String())
	}
	if errCode(t, rec) != "validation" {
		t.Fatalf("code = %q", errCode(t, rec))
	}
}

func TestDeleteAllPackages(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("pkgs")
	dir := h.store.WorkspacePackagesDir(id)
	writeDistInfo(t, dir, "foo", "1.0.0")
	writeDistInfo(t, dir, "bar", "2.0.0")

	rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/packages?all=true", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Removed int  `json:"removed"`
		All     bool `json:"all"`
	}](t, rec)
	if body.Removed != 2 || !body.All {
		t.Fatalf("body = %+v", body)
	}
}

func TestDeletePackagesNameAndAllConflict(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("pkgs")
	rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/packages?name=foo&all=true", keyA, nil)
	if rec.Code != 400 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestPackagesCrossOwnerForbidden(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("pkgs")
	cases := []struct {
		method string
		path   string
		body   any
	}{
		{"GET", "/v1/workspaces/" + id + "/packages", nil},
		{"POST", "/v1/workspaces/" + id + "/packages/install", map[string]any{"packages": []string{"x"}}},
		{"DELETE", "/v1/workspaces/" + id + "/packages?name=x", nil},
		{"DELETE", "/v1/workspaces/" + id + "/packages?all=true", nil},
	}
	for _, c := range cases {
		rec := h.doJSON(c.method, c.path, keyB, c.body)
		if rec.Code != 403 {
			t.Fatalf("%s %s = %d body=%s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

func TestPackageIsolationAcrossWorkspaces(t *testing.T) {
	h := newHarness(t)
	a := h.createWorkspace("a")
	b := h.createWorkspace("b")
	writeDistInfo(t, h.store.WorkspacePackagesDir(a), "onlyina", "1.0.0")

	recA := h.doJSON("GET", "/v1/workspaces/"+a+"/packages", keyA, nil)
	bodyA := decode[struct {
		Packages []map[string]string `json:"packages"`
	}](t, recA)
	if len(bodyA.Packages) != 1 || bodyA.Packages[0]["name"] != "onlyina" {
		t.Fatalf("workspace A packages = %+v", bodyA.Packages)
	}

	recB := h.doJSON("GET", "/v1/workspaces/"+b+"/packages", keyA, nil)
	bodyB := decode[struct {
		Packages []map[string]string `json:"packages"`
		Count    int                 `json:"count"`
	}](t, recB)
	if bodyB.Count != 0 || len(bodyB.Packages) != 0 {
		t.Fatalf("workspace B sees A's packages: %+v", bodyB.Packages)
	}
}
