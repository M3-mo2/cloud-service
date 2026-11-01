package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/model"
)

// stubNsjail writes a shell script that parses the rendered config's
// exec_bin.path and execs it, standing in for real nsjail on an unprivileged
// host. It exercises the executor's real Start plumbing (config rendering,
// stdin pipe, capped output capture, Wait/Kill) without kernel isolation.
func stubNsjail(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nsjail-stub")
	script := `#!/bin/sh
cfg=""
while [ $# -gt 0 ]; do
  case "$1" in
    --config) cfg="$2"; shift 2;;
    *) shift;;
  esac
done
exe=$(sed -n '/exec_bin {/,/}/p' "$cfg" | sed -n 's/.*path: "\(.*\)"/\1/p')
if [ -z "$exe" ]; then echo "stub: no exec_bin path" >&2; exit 2; fi
exec "$exe"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubRunCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := testCfg()
	cfg.Sandbox.NsjailPath = stubNsjail(t)
	cfg.Sandbox.KillGraceSec = 1
	return cfg
}

// TestStartLiveProcessRoundTrip proves the real Start path: it renders the
// config, keeps the child's stdin open as a pipe the caller owns, streams the
// child's stdout to the requested log file, and returns a LiveProcess whose
// Wait reaches a terminal Result.
func TestStartLiveProcessRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	cfg := stubRunCfg(t)
	r := NewNsjailRunner(cfg, writeTemplate(t), t.TempDir())

	ws := &model.Workspace{ID: "ws_test", Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	logPath := filepath.Join(t.TempDir(), "output.log")

	lp, err := r.Start(Request{
		Workspace:        ws,
		WorkspaceRoot:    t.TempDir(),
		Argv:             []string{"/bin/cat"},
		StdoutPath:       logPath,
		SessionOutputCap: 1 << 20,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if lp.Pid() == 0 {
		t.Fatal("Start returned a zero pid")
	}
	if err := lp.Write([]byte("hello-live\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// /bin/cat echoes on stdout; give it a moment to flush through the pipe.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(logPath); strings.Contains(string(b), "hello-live") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := lp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	res, err := lp.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !res.Streamed || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	b, _ := os.ReadFile(logPath)
	if !strings.Contains(string(b), "hello-live") {
		t.Fatalf("output not streamed to log: %q", b)
	}
}

// TestStartKillProcessGroup proves LiveProcess.Kill terminates the process group
// and Wait classifies it as a kill.
func TestStartKillProcessGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	cfg := stubRunCfg(t)
	r := NewNsjailRunner(cfg, writeTemplate(t), t.TempDir())

	ws := &model.Workspace{ID: "ws_test", Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	lp, err := r.Start(Request{
		Workspace:     ws,
		WorkspaceRoot: t.TempDir(),
		Argv:          []string{"/bin/sleep", "30"},
		StdoutPath:    filepath.Join(t.TempDir(), "output.log"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !lp.Kill() {
		t.Fatal("Kill returned false")
	}
	done := make(chan Result, 1)
	go func() {
		res, _ := lp.Wait()
		done <- res
	}()
	select {
	case res := <-done:
		if !res.Killed {
			t.Fatalf("result = %+v, want Killed", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Kill")
	}
}

// TestStartUnavailable proves Start reports the unavailable backend like Run.
func TestStartUnavailable(t *testing.T) {
	r := NewNsjailRunner(testCfg(), "", t.TempDir())
	if _, err := r.Start(Request{Workspace: &model.Workspace{Limits: model.Limits{MemoryMB: 64, MaxWallTimeSec: 5}}, Argv: []string{"true"}}); err != ErrNsjailUnavailable {
		t.Fatalf("err = %v, want ErrNsjailUnavailable", err)
	}
}
