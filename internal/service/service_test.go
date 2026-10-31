package service

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/executor"
	"sandboxd/internal/model"
	"sandboxd/internal/storage"
)

func newTestService(t *testing.T) (*Service, *executor.FakeRunner) {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.DataDir = t.TempDir()
	store, err := storage.New(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	fake := &executor.FakeRunner{AvailableFlag: true, Stdout: "hi\n"}
	svc := New(cfg, store, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(svc.WaitAll)
	return svc, fake
}

func TestCreateWorkspaceLimits(t *testing.T) {
	svc, _ := newTestService(t)
	ws, err := svc.CreateWorkspace("user_a", "w", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Limits.MemoryMB != 512 || ws.Limits.AllowNet {
		t.Fatalf("limits = %+v", ws.Limits)
	}
	over := &model.Limits{CPUCores: 4, MemoryMB: 128, AllowNet: true}
	ws2, err := svc.CreateWorkspace("user_a", "w2", over)
	if err != nil {
		t.Fatal(err)
	}
	if ws2.Limits.CPUCores != 4 || ws2.Limits.MemoryMB != 128 || !ws2.Limits.AllowNet {
		t.Fatalf("limits = %+v", ws2.Limits)
	}
	// Zero fields keep defaults.
	if ws2.Limits.DiskMB != 1024 {
		t.Fatalf("disk default lost: %d", ws2.Limits.DiskMB)
	}
}

func TestOwnershipEnforced(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("owner", "w", nil)
	if _, err := svc.GetOwnedWorkspace(ws.ID, "other"); err != model.ErrForbidden {
		t.Fatalf("err = %v", err)
	}
	if got, err := svc.GetOwnedWorkspace(ws.ID, "owner"); err != nil || got.ID != ws.ID {
		t.Fatalf("got %v err %v", got, err)
	}
	if _, err := svc.GetOwnedWorkspace("ws_missing", "owner"); err != model.ErrNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestExecUsesConfiguredTimeout(t *testing.T) {
	svc, fake := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	out, err := svc.Exec(ws, ExecSpec{Cmd: []string{"true"}, Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stdout != "hi\n" {
		t.Fatalf("stdout = %q", out.Stdout)
	}
	if fake.LastRequest().Timeout != 5*time.Second {
		t.Fatalf("timeout = %v", fake.LastRequest().Timeout)
	}
}

func TestExecHardCap(t *testing.T) {
	svc, fake := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	_, _ = svc.Exec(ws, ExecSpec{Cmd: []string{"true"}, Timeout: 99999})
	if fake.LastRequest().Timeout != execHardCapSec*time.Second {
		t.Fatalf("timeout = %v", fake.LastRequest().Timeout)
	}
}

func TestStartRunPrunesOldRuns(t *testing.T) {
	svc, fake := newTestService(t)
	fake.Stdout = ""
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	for i := 0; i < storage.Retention+3; i++ {
		run, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindCommand, Argv: []string{"x"}})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
		_ = run
	}
	// Allow background goroutines to finish.
	time.Sleep(200 * time.Millisecond)
	runs, err := svc.ListRuns(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) > storage.Retention {
		t.Fatalf("retention exceeded: %d runs", len(runs))
	}
}

func TestWriteFileQuota(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", &model.Limits{DiskMB: 1})
	// Write ~2MB to exceed a 1MB quota.
	big := strings.Repeat("x", 2*1024*1024)
	err := svc.WriteFile(ws, "/big", []byte(big), "text")
	if err == nil {
		t.Fatal("expected quota error")
	}
}

func TestFileLifecycle(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", nil)
	if err := svc.Mkdir(ws, "/dir"); err != nil {
		t.Fatal(err)
	}
	if err := svc.WriteFile(ws, "/dir/a.txt", []byte("hello"), "text"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.ReadFile(ws, "/dir/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Data) != "hello" || res.SHA256 == "" {
		t.Fatalf("res = %+v", res)
	}
	if err := svc.Rename(ws, "/dir/a.txt", "/dir/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteFile(ws, "/dir"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListFiles(ws, "/dir"); err == nil {
		t.Fatal("expected dir to be gone")
	}
}
