package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

func TestDefaultValues(t *testing.T) {
	d := Default()
	if d.Limits.CPUCores != 1 || d.Limits.MemoryMB != 512 || d.Limits.DiskMB != 1024 {
		t.Fatalf("unexpected default limits: %+v", d.Limits)
	}
	if d.Limits.MaxWallTimeSec != 300 || d.Limits.MaxRuns != 3 {
		t.Fatalf("unexpected default limits: %+v", d.Limits)
	}
	if d.MaxLimits.MemoryMB != 8192 || d.MaxLimits.DiskMB != 51200 || d.MaxLimits.MaxRuns != 64 {
		t.Fatalf("unexpected default maxima: %+v", d.MaxLimits)
	}
	if d.Caps.ExecStdoutKB != 256 || d.Caps.RunLogMB != 10 || d.Caps.UploadMB != 50 {
		t.Fatalf("unexpected default caps: %+v", d.Caps)
	}
	if d.Server.Addr != "127.0.0.1:8080" {
		t.Fatalf("unexpected default addr: %q", d.Server.Addr)
	}
}

func TestLoadMaxLimits(t *testing.T) {
	p := writeTemp(t, `
max_limits:
  cpu_cores: 16
  memory_mb: 4096
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MaxLimits.CPUCores != 16 || cfg.MaxLimits.MemoryMB != 4096 {
		t.Fatalf("max_limits = %+v", cfg.MaxLimits)
	}
	// Unset maxima fall back to defaults.
	if cfg.MaxLimits.DiskMB != 51200 {
		t.Fatalf("disk max default lost: %d", cfg.MaxLimits.DiskMB)
	}
}

func TestLoadFullConfig(t *testing.T) {
	p := writeTemp(t, `
server:
  addr: "0.0.0.0:9000"
storage:
  data_dir: "/tmp/data"
sandbox:
  nsjail_path: "/usr/local/bin/nsjail"
  user: "sandbox"
  group: "sandbox"
  kill_grace_sec: 5
limits:
  cpu_cores: 2
  memory_mb: 256
  disk_mb: 2048
  max_wall_time_sec: 120
  max_runs: 5
  allow_net: true
caps:
  exec_stdout_kb: 128
  exec_stderr_kb: 64
  run_log_mb: 4
  json_body_mb: 2
  upload_mb: 20
  file_read_mb: 1
  file_write_mb: 3
keys:
  - key: "k1"
    principal_id: "user_a"
  - key: "k2"
    principal_id: "user_a"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Addr != "0.0.0.0:9000" {
		t.Errorf("addr = %q", cfg.Server.Addr)
	}
	if cfg.Storage.DataDir != "/tmp/data" {
		t.Errorf("data_dir = %q", cfg.Storage.DataDir)
	}
	if cfg.Sandbox.NsjailPath != "/usr/local/bin/nsjail" {
		t.Errorf("nsjail = %q", cfg.Sandbox.NsjailPath)
	}
	if cfg.Sandbox.KillGraceSec != 5 {
		t.Errorf("grace = %d", cfg.Sandbox.KillGraceSec)
	}
	if !cfg.Limits.AllowNet {
		t.Errorf("allow_net should be true")
	}
	if cfg.Limits.CPUCores != 2 || cfg.Limits.MemoryMB != 256 {
		t.Errorf("limits = %+v", cfg.Limits)
	}
	if got := cfg.Caps.ExecStdoutBytes(); got != 128*1024 {
		t.Errorf("ExecStdoutBytes = %d", got)
	}
	if got := cfg.Caps.RunLogBytes(); got != 4*1024*1024 {
		t.Errorf("RunLogBytes = %d", got)
	}
	if len(cfg.Keys) != 2 || cfg.Keys[0].PrincipalID != "user_a" {
		t.Errorf("keys = %+v", cfg.Keys)
	}
}

func TestLoadPartialKeepsDefaults(t *testing.T) {
	p := writeTemp(t, `
server:
  addr: "127.0.0.1:1111"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:1111" {
		t.Errorf("addr = %q", cfg.Server.Addr)
	}
	if cfg.Limits.MemoryMB != 512 {
		t.Errorf("memory default lost: %d", cfg.Limits.MemoryMB)
	}
	if cfg.Caps.UploadMB != 50 {
		t.Errorf("upload default lost: %d", cfg.Caps.UploadMB)
	}
	if cfg.Storage.DataDir != "./data" {
		t.Errorf("data_dir default lost: %q", cfg.Storage.DataDir)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestKillGrace(t *testing.T) {
	s := SandboxConfig{KillGraceSec: 3}
	if s.KillGrace().Seconds() != 3 {
		t.Fatalf("grace = %v", s.KillGrace())
	}
}
