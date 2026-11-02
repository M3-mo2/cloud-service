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
	"sandboxd/internal/workspace"
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
	ws, err := svc.CreateWorkspace("user_a", "w", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Limits.MemoryMB != 512 || ws.Limits.AllowNet {
		t.Fatalf("limits = %+v", ws.Limits)
	}
	over := &model.Limits{CPUCores: 4, MemoryMB: 128, AllowNet: true}
	ws2, err := svc.CreateWorkspace("user_a", "w2", "", over)
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
	ws, _ := svc.CreateWorkspace("owner", "w", "", nil)
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
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
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
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	_, _ = svc.Exec(ws, ExecSpec{Cmd: []string{"true"}, Timeout: 99999})
	if fake.LastRequest().Timeout != execHardCapSec*time.Second {
		t.Fatalf("timeout = %v", fake.LastRequest().Timeout)
	}
}

func TestStartRunPrunesOldRuns(t *testing.T) {
	svc, fake := newTestService(t)
	fake.Stdout = ""
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
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
	runs, _, err := svc.ListRuns(ws, RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) > storage.Retention {
		t.Fatalf("retention exceeded: %d runs", len(runs))
	}
}

func TestWriteFileQuota(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", &model.Limits{DiskMB: 1})
	// Write ~2MB to exceed a 1MB quota.
	big := strings.Repeat("x", 2*1024*1024)
	err := svc.WriteFile(ws, "/big", []byte(big), "text")
	if err == nil {
		t.Fatal("expected quota error")
	}
}

func TestFileLifecycle(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
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

func TestStartRunFileRejectsUnsafePath(t *testing.T) {
	svc, fake := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	_, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindFile, File: "../../etc/passwd"})
	if err == nil {
		t.Fatal("expected unsafe path rejection")
	}
	if !workspace.IsUnsafe(err) {
		t.Fatalf("err = %v, want unsafe", err)
	}
	if fake.LastRequest() != nil {
		t.Fatal("runner should not have been invoked")
	}
}

func TestRecoverRuns(t *testing.T) {
	svc, fake := newTestService(t)
	fake.Delay = 50 * time.Millisecond
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	run, err := svc.StartRun(ws, StartRunSpec{Kind: model.RunKindCommand, Argv: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for it to finish, then forcibly mark it running on disk to simulate
	// an orphaned record from a prior process.
	svc.WaitAll()
	stored, _ := svc.store.GetRun(ws.ID, run.ID)
	stored.Status = model.RunRunning
	if err := svc.store.SaveRun(ws.ID, stored); err != nil {
		t.Fatal(err)
	}
	n, err := svc.RecoverRuns()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recovered = %d, want 1", n)
	}
	got, _ := svc.store.GetRun(ws.ID, run.ID)
	if got.Status != model.RunInterrupted {
		t.Fatalf("status = %s, want interrupted", got.Status)
	}
	if got.EndedAt.IsZero() {
		t.Fatal("interrupted run must have an ended_at")
	}
}

func TestListFilesPageBoundary(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	if err := svc.Mkdir(ws, "/d"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if err := svc.WriteFile(ws, "/"+name, []byte("x"), "text"); err != nil {
			t.Fatal(err)
		}
	}
	listing, err := svc.ListFilesPage(ws, "/", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if listing.Total != 4 || len(listing.Entries) != 2 || !listing.Truncated {
		t.Fatalf("listing = %+v", listing)
	}
	if listing.Entries[0].Type != "dir" {
		t.Fatalf("dirs-first broken: %+v", listing.Entries)
	}
	// Listing a file is a not-a-directory error.
	if _, err := svc.ListFilesPage(ws, "/a", 2, 0); !workspace.IsNotDirectory(err) {
		t.Fatalf("list file err = %v", err)
	}
}

func TestReadFileRangeHonestHashAndCap(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	if err := svc.WriteFile(ws, "/f.txt", []byte("0123456789"), "text"); err != nil {
		t.Fatal(err)
	}
	full, err := svc.ReadFileRange(ws, "/f.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !full.WholeFile || full.Truncated || string(full.Data) != "0123456789" {
		t.Fatalf("full = %+v", full)
	}
	part, err := svc.ReadFileRange(ws, "/f.txt", 4, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(part.Data) != "456" || part.Offset != 4 || part.Length != 3 || part.WholeFile {
		t.Fatalf("part = %+v", part)
	}
	if part.SHA256 == full.SHA256 {
		t.Fatal("partial hash equals full hash")
	}
	// A limit above the read cap is a 413.
	_, err = svc.ReadFileRange(ws, "/f.txt", 0, svc.cfg.Caps.FileReadBytes()+1)
	if err == nil || model.AsError(err).Status != 413 {
		t.Fatalf("limit over cap err = %v", err)
	}
	// Reading a directory is a not-a-directory error.
	if err := svc.Mkdir(ws, "/d"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReadFileRange(ws, "/d", 0, 0); !workspace.IsNotDirectory(err) {
		t.Fatalf("read dir err = %v", err)
	}
}

func TestWriteFileOverDirectory(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	if err := svc.Mkdir(ws, "/d"); err != nil {
		t.Fatal(err)
	}
	if err := svc.WriteFile(ws, "/d", []byte("x"), "text"); !workspace.IsIsDirectory(err) {
		t.Fatalf("err = %v want is-directory", err)
	}
}

func TestStatFileAndOpenFile(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	if err := svc.WriteFile(ws, "/f.txt", []byte("hello"), "text"); err != nil {
		t.Fatal(err)
	}
	info, err := svc.StatFile(ws, "/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Type != "file" || info.SHA256 == "" {
		t.Fatalf("stat = %+v", info)
	}
	rc, oi, err := svc.OpenFile(ws, "/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "hello" || oi.Size != 5 {
		t.Fatalf("open = %q info=%+v", data, oi)
	}
	if _, _, err := svc.OpenFile(ws, "/missing"); !workspace.IsNotFound(err) {
		t.Fatalf("open missing err = %v", err)
	}
}

func TestReadFileLinesService(t *testing.T) {
	svc, _ := newTestService(t)
	ws, _ := svc.CreateWorkspace("u", "w", "", nil)
	if err := svc.WriteFile(ws, "/f.txt", []byte("one\ntwo\nthree\n"), "text"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.ReadFileLines(ws, "/f.txt", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Data) != "two\n" || res.TotalLines != 3 {
		t.Fatalf("lines = %+v", res)
	}
}
