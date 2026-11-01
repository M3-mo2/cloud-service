package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sandboxd/internal/model"
)

func TestValidateRequirementAcceptsPlausible(t *testing.T) {
	for _, req := range []string{
		"requests",
		"requests==2.31.0",
		"flask>=2.0,<3",
		"package[extra]",
		"my-pkg~=1.2",
		"django>=4.2.0",
		"numpy!=1.24.*",
		"typing_extensions",
		"A",
		"a-1",
	} {
		if err := ValidateRequirement(req); err != nil {
			t.Errorf("ValidateRequirement(%q) = %v, want nil", req, err)
		}
	}
}

func TestValidateRequirementRejectsHostile(t *testing.T) {
	cases := []string{
		"",
		" ",
		"   ",
		"\t",
		"-e",
		"--upgrade",
		"-r requirements.txt",
		"pkg; rm -rf /",
		"pkg && rm -rf /",
		"pkg | cat /etc/passwd",
		"pkg`id`",
		"$(whoami)",
		"../../etc/passwd",
		"/etc/passwd",
		"pkg/../evil",
		"pkg name",
		"pkg\tname",
		"pkg\n-x",
		"pkg\\evil",
		"pkg\"evil",
		"pkg'evil",
		"pkg]",
		"pkg[",
		"pkg[extra",
		"pkgextra]",
		strings.Repeat("a", 10000),
		string([]byte{0x01, 'x'}),
	}
	for _, req := range cases {
		if err := ValidateRequirement(req); err == nil {
			t.Errorf("ValidateRequirement(%q) = nil, want error", req)
		}
	}
}

func TestInstallPackagesRejectsHostileWithoutRunning(t *testing.T) {
	svc, fake := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	for _, hostile := range []string{"pkg; rm -rf /", "-e", "--upgrade", "../../etc/passwd", ""} {
		_, err := svc.InstallPackages(ws, []string{hostile})
		if err == nil {
			t.Fatalf("InstallPackages(%q) = nil error, want rejection", hostile)
		}
		if oc := model.AsError(err); oc.Status != 422 {
			t.Fatalf("InstallPackages(%q) status = %d, want 422", hostile, oc.Status)
		}
	}
	if fake.LastRequest() != nil {
		t.Fatal("runner must not be invoked for rejected requirements")
	}
}

func TestInstallPackagesEmptyRejected(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	if _, err := svc.InstallPackages(ws, nil); err == nil {
		t.Fatal("expected error for empty package list")
	}
}

func TestInstallPackagesRunsPipTargetPrivate(t *testing.T) {
	svc, fake := newTestService(t)
	fake.AvailableFlag = true
	fake.Stdout = "Successfully installed foo-1.0\n"
	ws, _ := svc.CreateWorkspace("u", "w", nil)

	res, err := svc.InstallPackages(ws, []string{"foo==1.0", "bar"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	req := fake.LastRequest()
	if req == nil {
		t.Fatal("runner not invoked")
	}
	joined := strings.Join(req.Argv, " ")
	if !strings.Contains(joined, "pip install") {
		t.Fatalf("argv = %v", req.Argv)
	}
	if !strings.Contains(joined, "--target "+PrivatePackagesMount) {
		t.Fatalf("argv must install into the private tier: %v", req.Argv)
	}
	if !strings.Contains(joined, "foo==1.0") || !strings.Contains(joined, "bar") {
		t.Fatalf("requirements missing from argv: %v", req.Argv)
	}
	if req.PrivatePackagesDir != svc.Store().WorkspacePackagesDir(ws.ID) {
		t.Fatalf("private dir = %q", req.PrivatePackagesDir)
	}
	if req.Timeout != svc.Config().Packages.InstallTimeout() {
		t.Fatalf("timeout = %v", req.Timeout)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "Successfully installed") {
		t.Fatalf("res = %+v", res)
	}
	if res.TimedOut {
		t.Fatalf("unexpected timeout: %+v", res)
	}
}

func TestInstallPackagesSuspendedRefused(t *testing.T) {
	svc, fake := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	ws.Status = model.WorkspaceSuspended
	if _, err := svc.InstallPackages(ws, []string{"foo"}); err == nil {
		t.Fatal("expected suspended refusal")
	}
	if fake.LastRequest() != nil {
		t.Fatal("runner must not be invoked for a suspended workspace")
	}
}

func TestPackageTierSharedDegradesWhenDirMissing(t *testing.T) {
	svc, _ := newTestService(t)
	svc.cfg.Packages.SharedEnabled = true
	svc.cfg.Packages.SharedDir = filepath.Join(t.TempDir(), "does-not-exist")
	ws, _ := svc.CreateWorkspace("u", "w", nil)

	tier := svc.PackageTierFor(ws)
	if !tier.SharedEnabled {
		t.Fatal("shared tier should report enabled")
	}
	if tier.SharedAvailable {
		t.Fatal("missing shared dir must report unavailable")
	}
	if tier.PrivatePath != PrivatePackagesMount {
		t.Fatalf("private path = %q", tier.PrivatePath)
	}
	// A run still resolves the private tier even with the shared tier absent.
	_, privateHost, err := svc.packageDirs(ws)
	if err != nil {
		t.Fatalf("packageDirs: %v", err)
	}
	if privateHost == "" {
		t.Fatal("private dir must resolve even when shared is missing")
	}
}

func TestPackageTierSharedAvailableWhenDirPresent(t *testing.T) {
	shared := t.TempDir()
	svc, _ := newTestService(t)
	svc.cfg.Packages.SharedEnabled = true
	svc.cfg.Packages.SharedDir = shared
	ws, _ := svc.CreateWorkspace("u", "w", nil)

	tier := svc.PackageTierFor(ws)
	if !tier.SharedAvailable || tier.SharedPath != SharedPackagesMount {
		t.Fatalf("tier = %+v", tier)
	}
	sharedHost, _, err := svc.packageDirs(ws)
	if err != nil {
		t.Fatal(err)
	}
	if sharedHost != shared {
		t.Fatalf("shared host = %q, want %q", sharedHost, shared)
	}
}

func TestPackageTierSharedDisabled(t *testing.T) {
	svc, _ := newTestService(t)
	svc.cfg.Packages.SharedEnabled = false
	svc.cfg.Packages.SharedDir = t.TempDir()
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	if tier := svc.PackageTierFor(ws); tier.SharedAvailable {
		t.Fatalf("disabled shared tier must be unavailable: %+v", tier)
	}
	sharedHost, _, err := svc.packageDirs(ws)
	if err != nil {
		t.Fatal(err)
	}
	if sharedHost != "" {
		t.Fatalf("disabled shared host = %q, want empty", sharedHost)
	}
}

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

func TestListPrivatePackages(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	dir := svc.Store().WorkspacePackagesDir(ws.ID)
	writeDistInfo(t, dir, "Requests", "2.31.0")
	writeDistInfo(t, dir, "Flask", "3.0.0")
	// An unrelated directory must not be reported.
	if err := os.MkdirAll(filepath.Join(dir, "not_a_package"), 0o755); err != nil {
		t.Fatal(err)
	}

	pkgs, truncated, err := svc.ListPrivatePackages(ws)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if len(pkgs) != 2 {
		t.Fatalf("pkgs = %+v", pkgs)
	}
	// Sorted by name.
	if pkgs[0].Name != "Flask" || pkgs[0].Version != "3.0.0" {
		t.Fatalf("pkgs[0] = %+v", pkgs[0])
	}
	if pkgs[1].Name != "Requests" || pkgs[1].Version != "2.31.0" {
		t.Fatalf("pkgs[1] = %+v", pkgs[1])
	}
}

func TestPrivateTierIsolatedBetweenWorkspaces(t *testing.T) {
	svc, _ := newTestService(t)
	wsA, _ := svc.CreateWorkspace("u", "a", nil)
	wsB, _ := svc.CreateWorkspace("u", "b", nil)

	dirA := svc.Store().WorkspacePackagesDir(wsA.ID)
	dirB := svc.Store().WorkspacePackagesDir(wsB.ID)
	if dirA == dirB {
		t.Fatal("workspaces must have distinct private package dirs")
	}
	writeDistInfo(t, dirA, "OnlyInA", "1.0.0")

	pkgsA, _, err := svc.ListPrivatePackages(wsA)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgsA) != 1 || pkgsA[0].Name != "OnlyInA" {
		t.Fatalf("ws A pkgs = %+v", pkgsA)
	}
	pkgsB, _, err := svc.ListPrivatePackages(wsB)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgsB) != 0 {
		t.Fatalf("package installed in A leaked into B: %+v", pkgsB)
	}
	// The isolation is also structural: the host paths differ and neither is
	// inside the shared tier location.
	if strings.Contains(dirA, "shared") || strings.Contains(dirB, "shared") {
		t.Fatal("private tier must not live under a shared location")
	}
}

func TestRemovePackage(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	dir := svc.Store().WorkspacePackagesDir(ws.ID)
	writeDistInfo(t, dir, "foo", "1.0.0")
	// A top-level module recorded in RECORD.
	if err := os.WriteFile(filepath.Join(dir, "foo.py"), []byte("x=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "foo-1.0.0.dist-info", "RECORD"),
		[]byte("foo.py,sha256=abc,12\nfoo-1.0.0.dist-info/METADATA,sha256=def,20\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := svc.RemovePackage(ws, "foo"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "foo.py")); !os.IsNotExist(err) {
		t.Fatal("foo.py should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "foo-1.0.0.dist-info")); !os.IsNotExist(err) {
		t.Fatal("dist-info should be removed")
	}
	// Removing again is a not-found.
	if err := svc.RemovePackage(ws, "foo"); err != ErrPackageNotFound {
		t.Fatalf("second remove = %v, want ErrPackageNotFound", err)
	}
}

func TestRemovePackageNormalizedName(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	dir := svc.Store().WorkspacePackagesDir(ws.ID)
	writeDistInfo(t, dir, "My_Package", "1.0.0")
	if err := svc.RemovePackage(ws, "my-package"); err != nil {
		t.Fatalf("normalized remove: %v", err)
	}
}

func TestRemovePackageEmptyRejected(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	if err := svc.RemovePackage(ws, "  "); err == nil {
		t.Fatal("empty name must be rejected")
	}
}

func TestRemoveAllPackages(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	dir := svc.Store().WorkspacePackagesDir(ws.ID)
	writeDistInfo(t, dir, "foo", "1.0.0")
	writeDistInfo(t, dir, "bar", "2.0.0")
	if err := os.WriteFile(filepath.Join(dir, "loose.py"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := svc.RemoveAllPackages(ws)
	if err != nil {
		t.Fatalf("remove all: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("private dir not empty: %v", entries)
	}
}

func TestRemovePackageSuspendedRefused(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	writeDistInfo(t, svc.Store().WorkspacePackagesDir(ws.ID), "foo", "1.0.0")
	ws.Status = model.WorkspaceSuspended
	if err := svc.RemovePackage(ws, "foo"); err == nil {
		t.Fatal("expected suspended refusal")
	}
}

func TestListPrivatePackagesSuspendedRefused(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	ws.Status = model.WorkspaceSuspended
	if _, _, err := svc.ListPrivatePackages(ws); err == nil {
		t.Fatal("expected suspended refusal")
	}
}

func TestExecWiresPackageDirs(t *testing.T) {
	shared := t.TempDir()
	svc, fake := newTestService(t)
	svc.cfg.Packages.SharedEnabled = true
	svc.cfg.Packages.SharedDir = shared
	ws, _ := svc.CreateWorkspace("u", "w", nil)

	if _, err := svc.Exec(ws, ExecSpec{Cmd: []string{"python3", "-c", "1"}, Timeout: 5}); err != nil {
		t.Fatal(err)
	}
	req := fake.LastRequest()
	if req.SharedPackagesDir != shared {
		t.Fatalf("shared dir = %q, want %q", req.SharedPackagesDir, shared)
	}
	if req.PrivatePackagesDir != svc.Store().WorkspacePackagesDir(ws.ID) {
		t.Fatalf("private dir = %q", req.PrivatePackagesDir)
	}
}

func TestStartRunWiresPackageDirs(t *testing.T) {
	shared := t.TempDir()
	svc, fake := newTestService(t)
	svc.cfg.Packages.SharedEnabled = true
	svc.cfg.Packages.SharedDir = shared
	ws, _ := svc.CreateWorkspace("u", "w", nil)

	if _, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindCommand, Argv: []string{"python3", "-c", "1"}}); err != nil {
		t.Fatal(err)
	}
	svc.WaitAll()
	req := fake.LastRequest()
	if req.SharedPackagesDir != shared {
		t.Fatalf("shared dir = %q, want %q", req.SharedPackagesDir, shared)
	}
	if req.PrivatePackagesDir != svc.Store().WorkspacePackagesDir(ws.ID) {
		t.Fatalf("private dir = %q", req.PrivatePackagesDir)
	}
}
