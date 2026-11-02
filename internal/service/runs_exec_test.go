package service

import (
	"io"
	"log/slog"
	"testing"

	"sandboxd/internal/config"
	"sandboxd/internal/executor"
	"sandboxd/internal/model"
	"sandboxd/internal/storage"
)

// newTestServiceCfg builds a service over a fresh data dir with cfg mutated by
// mutate, so tests can exercise config-dependent behaviour.
func newTestServiceCfg(t *testing.T, mutate func(*config.Config)) (*Service, *executor.FakeRunner) {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.DataDir = t.TempDir()
	if mutate != nil {
		mutate(&cfg)
	}
	store, err := storage.New(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	fake := &executor.FakeRunner{AvailableFlag: true, Stdout: "hi\n"}
	svc := New(cfg, store, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(svc.WaitAll)
	return svc, fake
}

func apiStatus(err error) int { return model.AsError(err).Status }

func TestStartRunFileInterpreterByExtension(t *testing.T) {
	svc, fake := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	for _, tc := range []struct {
		file    string
		interp  string
		content string
	}{
		{"/x.py", "python3", "print(1)\n"},
		{"/x.js", "node", "console.log(1)\n"},
		{"/x.sh", "bash", "echo hi\n"},
		{"/x.pl", "perl", "print 1\n"},
	} {
		if err := svc.WriteFile(ws, tc.file, []byte(tc.content), "text"); err != nil {
			t.Fatal(err)
		}
		run, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: tc.file})
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if run.Interpreter != tc.interp {
			t.Fatalf("%s interpreter = %q want %q", tc.file, run.Interpreter, tc.interp)
		}
		want := []string{tc.interp, "/workspace" + tc.file}
		svc.WaitAll()
		req := fake.LastRequest()
		if req == nil || len(req.Argv) < 2 || req.Argv[0] != want[0] || req.Argv[1] != want[1] {
			t.Fatalf("%s argv = %v", tc.file, req.Argv)
		}
	}
}

func TestStartRunFileUnsupportedExtensionRejected(t *testing.T) {
	svc, fake := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	if err := svc.WriteFile(ws, "/x.txt", []byte("x"), "text"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/x.txt"})
	if err == nil || apiStatus(err) != 422 {
		t.Fatalf("err = %v, want 422", err)
	}
	if fake.LastRequest() != nil {
		t.Fatal("runner must not be invoked for an unsupported extension")
	}
}

func TestStartRunExplicitInterpreterOverride(t *testing.T) {
	svc, fake := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	if err := svc.WriteFile(ws, "/data.txt", []byte("print(1)\n"), "text"); err != nil {
		t.Fatal(err)
	}
	// An explicit allowlisted interpreter runs a file whose extension is not
	// registered.
	run, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/data.txt", Interpreter: "python3"})
	if err != nil {
		t.Fatal(err)
	}
	svc.WaitAll()
	if run.Interpreter != "python3" || fake.LastRequest().Argv[0] != "python3" {
		t.Fatalf("run = %+v argv=%v", run, fake.LastRequest().Argv)
	}
	// A non-allowlisted interpreter is 422 and never invoked.
	_, err = svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/x.py", Interpreter: "/bin/sh"})
	if err == nil || apiStatus(err) != 422 {
		t.Fatalf("err = %v, want 422", err)
	}
}

func TestStartRunCwdValidation(t *testing.T) {
	svc, fake := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	if err := svc.WriteFile(ws, "/f.py", []byte("print(1)\n"), "text"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Mkdir(ws, "/sub"); err != nil {
		t.Fatal(err)
	}
	// A missing cwd is 422, not a deferred 503.
	_, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/f.py", Cwd: "/missing"})
	if err == nil || apiStatus(err) != 422 {
		t.Fatalf("missing cwd err = %v, want 422", err)
	}
	// A cwd that is a file is 422.
	_, err = svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/f.py", Cwd: "/f.py"})
	if err == nil || apiStatus(err) != 422 {
		t.Fatalf("file cwd err = %v, want 422", err)
	}
	// A valid subdirectory is normalized (leading slash) and passed through.
	run, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/f.py", Cwd: "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Cwd != "/sub" {
		t.Fatalf("cwd = %q, want /sub", run.Cwd)
	}
	svc.WaitAll()
	if fake.LastRequest().Cwd != "/sub" {
		t.Fatalf("executor cwd = %q", fake.LastRequest().Cwd)
	}
	// Default cwd is the workspace root.
	run2, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/f.py"})
	if err != nil {
		t.Fatal(err)
	}
	if run2.Cwd != "/" {
		t.Fatalf("default cwd = %q, want /", run2.Cwd)
	}
}

func TestStartRunArgvAndFileConflict(t *testing.T) {
	svc, fake := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	if err := svc.WriteFile(ws, "/f.py", []byte("print(1)\n"), "text"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/f.py", Argv: []string{"python3", "f.py"}})
	if err == nil || apiStatus(err) != 422 {
		t.Fatalf("err = %v, want 422", err)
	}
	if fake.LastRequest() != nil {
		t.Fatal("runner must not be invoked on an ambiguous request")
	}
}

func TestStartRunUnlimitedForbiddenByServerDefault(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	if err := svc.WriteFile(ws, "/f.py", []byte("print(1)\n"), "text"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/f.py", NoTimeout: true})
	if err == nil || apiStatus(err) != 422 {
		t.Fatalf("err = %v, want 422", err)
	}
	if d := model.AsError(err).Details["config"]; d != "max_limits.allow_unlimited_runs" {
		t.Fatalf("details = %v, want server-level", model.AsError(err).Details)
	}
}

func TestStartRunUnlimitedForbiddenByWorkspace(t *testing.T) {
	svc, _ := newTestServiceCfg(t, func(c *config.Config) { c.MaxLimits.AllowUnlimitedRuns = true })
	// Workspace did not opt in (default false).
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	if err := svc.WriteFile(ws, "/f.py", []byte("print(1)\n"), "text"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/f.py", NoTimeout: true})
	if err == nil || apiStatus(err) != 422 {
		t.Fatalf("err = %v, want 422", err)
	}
	if d := model.AsError(err).Details["config"]; d != "limits.allow_unlimited_runs" {
		t.Fatalf("details = %v, want workspace-level", model.AsError(err).Details)
	}
}

func TestStartRunUnlimitedAllowedRecordsHonestly(t *testing.T) {
	svc, fake := newTestServiceCfg(t, func(c *config.Config) { c.MaxLimits.AllowUnlimitedRuns = true })
	ws, err := svc.CreateWorkspace("u", "w", "", &model.Limits{AllowUnlimitedRuns: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.WriteFile(ws, "/f.py", []byte("print(1)\n"), "text"); err != nil {
		t.Fatal(err)
	}
	run, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/f.py", NoTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	if !run.Unlimited {
		t.Fatal("run.Unlimited = false, want true")
	}
	if run.TimeoutSec != nil {
		t.Fatalf("unlimited run must not record a fake timeout, got %d", *run.TimeoutSec)
	}
	svc.WaitAll()
	if !fake.LastRequest().NoTimeout {
		t.Fatal("executor request must carry NoTimeout")
	}
	// A finite run records the number honestly.
	finite, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "/f.py", Timeout: 7})
	if err != nil {
		t.Fatal(err)
	}
	if finite.Unlimited || finite.TimeoutSec == nil || *finite.TimeoutSec != 7 {
		t.Fatalf("finite run = unlimited:%v timeout:%v", finite.Unlimited, finite.TimeoutSec)
	}
}

func TestCreateWorkspaceUnlimitedAboveServerMaxima(t *testing.T) {
	svc, _ := newTestService(t) // server max false
	_, err := svc.CreateWorkspace("u", "w", "", &model.Limits{AllowUnlimitedRuns: true})
	if err == nil || apiStatus(err) != 422 {
		t.Fatalf("err = %v, want 422", err)
	}
}
