package config

import (
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

// Config is the top-level service configuration loaded from YAML.
type Config struct {
	Server    ServerConfig  `koanf:"server"`
	Storage   StorageConfig `koanf:"storage"`
	Sandbox   SandboxConfig `koanf:"sandbox"`
	Limits    LimitsConfig  `koanf:"limits"`
	MaxLimits LimitsConfig  `koanf:"max_limits"`
	Caps      CapsConfig    `koanf:"caps"`
	Keys      []KeyConfig   `koanf:"keys"`
}

// ServerConfig controls the HTTP listener.
type ServerConfig struct {
	Addr string `koanf:"addr"`
}

// StorageConfig controls on-disk persistence.
type StorageConfig struct {
	DataDir string `koanf:"data_dir"`
}

// SandboxConfig controls nsjail invocation.
type SandboxConfig struct {
	NsjailPath   string `koanf:"nsjail_path"`
	User         string `koanf:"user"`
	Group        string `koanf:"group"`
	KillGraceSec int    `koanf:"kill_grace_sec"`
	// UID/GID are optional explicit sandbox ids. When zero they are resolved
	// from User/Group, falling back to built-in defaults.
	UID int `koanf:"uid"`
	GID int `koanf:"gid"`
}

// LimitsConfig holds default per-workspace limits.
type LimitsConfig struct {
	CPUCores       float64 `koanf:"cpu_cores"`
	MemoryMB       int     `koanf:"memory_mb"`
	DiskMB         int     `koanf:"disk_mb"`
	MaxWallTimeSec int     `koanf:"max_wall_time_sec"`
	MaxRuns        int     `koanf:"max_runs"`
	AllowNet       bool    `koanf:"allow_net"`
}

// CapsConfig holds hard output/body caps. Sizes are expressed in the unit named
// by the field (KB or MB).
type CapsConfig struct {
	ExecStdoutKB int `koanf:"exec_stdout_kb"`
	ExecStderrKB int `koanf:"exec_stderr_kb"`
	RunLogMB     int `koanf:"run_log_mb"`
	JSONBodyMB   int `koanf:"json_body_mb"`
	UploadMB     int `koanf:"upload_mb"`
	FileReadMB   int `koanf:"file_read_mb"`
	FileWriteMB  int `koanf:"file_write_mb"`
}

// KeyConfig maps a static bearer key to a principal.
type KeyConfig struct {
	Key         string `koanf:"key"`
	PrincipalID string `koanf:"principal_id"`
}

// Bytes helper conversions.

// ExecStdoutBytes returns the exec stdout cap in bytes.
func (c CapsConfig) ExecStdoutBytes() int64 { return int64(c.ExecStdoutKB) * 1024 }

// ExecStderrBytes returns the exec stderr cap in bytes.
func (c CapsConfig) ExecStderrBytes() int64 { return int64(c.ExecStderrKB) * 1024 }

// RunLogBytes returns the per-stream run log cap in bytes.
func (c CapsConfig) RunLogBytes() int64 { return int64(c.RunLogMB) * 1024 * 1024 }

// JSONBodyBytes returns the JSON request body cap in bytes.
func (c CapsConfig) JSONBodyBytes() int64 { return int64(c.JSONBodyMB) * 1024 * 1024 }

// UploadBytes returns the multipart upload cap in bytes.
func (c CapsConfig) UploadBytes() int64 { return int64(c.UploadMB) * 1024 * 1024 }

// FileReadBytes returns the file read cap in bytes.
func (c CapsConfig) FileReadBytes() int64 { return int64(c.FileReadMB) * 1024 * 1024 }

// FileWriteBytes returns the file write cap in bytes.
func (c CapsConfig) FileWriteBytes() int64 { return int64(c.FileWriteMB) * 1024 * 1024 }

// KillGrace returns the SIGTERM->SIGKILL grace period.
func (s SandboxConfig) KillGrace() time.Duration {
	return time.Duration(s.KillGraceSec) * time.Second
}

// Default returns a configuration populated with safe defaults.
func Default() Config {
	return Config{
		Server:  ServerConfig{Addr: "127.0.0.1:8080"},
		Storage: StorageConfig{DataDir: "./data"},
		Sandbox: SandboxConfig{
			NsjailPath:   "nsjail",
			User:         "sandbox",
			Group:        "sandbox",
			KillGraceSec: 3,
		},
		Limits: LimitsConfig{
			CPUCores:       1,
			MemoryMB:       512,
			DiskMB:         1024,
			MaxWallTimeSec: 300,
			MaxRuns:        3,
			AllowNet:       false,
		},
		MaxLimits: LimitsConfig{
			CPUCores:       8,
			MemoryMB:       8192,
			DiskMB:         51200,
			MaxWallTimeSec: 3600,
			MaxRuns:        64,
			AllowNet:       true,
		},
		Caps: CapsConfig{
			ExecStdoutKB: 256,
			ExecStderrKB: 256,
			RunLogMB:     10,
			JSONBodyMB:   1,
			UploadMB:     50,
			FileReadMB:   5,
			FileWriteMB:  10,
		},
		Keys: []KeyConfig{},
	}
}

// Load reads a YAML config file, layering it over defaults.
func Load(path string) (Config, error) {
	cfg := Default()

	// Apply defaults as a baseline map so that partial files keep defaults.
	base := koanf.New(".")
	if err := base.Load(rawbytes.Provider(rawDefaults()), yaml.Parser()); err != nil {
		return cfg, err
	}

	f := file.Provider(path)
	if err := base.Load(f, yaml.Parser()); err != nil {
		return cfg, err
	}

	if err := base.Unmarshal("", &cfg); err != nil {
		return cfg, err
	}
	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	d := Default()
	if c.Server.Addr == "" {
		c.Server.Addr = d.Server.Addr
	}
	if c.Storage.DataDir == "" {
		c.Storage.DataDir = d.Storage.DataDir
	}
	if c.Sandbox.NsjailPath == "" {
		c.Sandbox.NsjailPath = d.Sandbox.NsjailPath
	}
	if c.Sandbox.User == "" {
		c.Sandbox.User = d.Sandbox.User
	}
	if c.Sandbox.Group == "" {
		c.Sandbox.Group = d.Sandbox.Group
	}
	if c.Sandbox.KillGraceSec <= 0 {
		c.Sandbox.KillGraceSec = d.Sandbox.KillGraceSec
	}
	if c.Limits.CPUCores <= 0 {
		c.Limits.CPUCores = d.Limits.CPUCores
	}
	if c.Limits.MemoryMB <= 0 {
		c.Limits.MemoryMB = d.Limits.MemoryMB
	}
	if c.Limits.DiskMB <= 0 {
		c.Limits.DiskMB = d.Limits.DiskMB
	}
	if c.Limits.MaxWallTimeSec <= 0 {
		c.Limits.MaxWallTimeSec = d.Limits.MaxWallTimeSec
	}
	if c.Limits.MaxRuns <= 0 {
		c.Limits.MaxRuns = d.Limits.MaxRuns
	}
	// Server-level maxima must always be present and at least the defaults, or
	// every run/exec would be clamped to nonsense.
	m := &c.MaxLimits
	if m.CPUCores < d.Limits.CPUCores {
		m.CPUCores = d.MaxLimits.CPUCores
	}
	if m.MemoryMB < d.Limits.MemoryMB {
		m.MemoryMB = d.MaxLimits.MemoryMB
	}
	if m.DiskMB < d.Limits.DiskMB {
		m.DiskMB = d.MaxLimits.DiskMB
	}
	if m.MaxWallTimeSec < d.Limits.MaxWallTimeSec {
		m.MaxWallTimeSec = d.MaxLimits.MaxWallTimeSec
	}
	if m.MaxRuns < d.Limits.MaxRuns {
		m.MaxRuns = d.MaxLimits.MaxRuns
	}
	caps := &c.Caps
	if caps.ExecStdoutKB <= 0 {
		caps.ExecStdoutKB = d.Caps.ExecStdoutKB
	}
	if caps.ExecStderrKB <= 0 {
		caps.ExecStderrKB = d.Caps.ExecStderrKB
	}
	if caps.RunLogMB <= 0 {
		caps.RunLogMB = d.Caps.RunLogMB
	}
	if caps.JSONBodyMB <= 0 {
		caps.JSONBodyMB = d.Caps.JSONBodyMB
	}
	if caps.UploadMB <= 0 {
		caps.UploadMB = d.Caps.UploadMB
	}
	if caps.FileReadMB <= 0 {
		caps.FileReadMB = d.Caps.FileReadMB
	}
	if caps.FileWriteMB <= 0 {
		caps.FileWriteMB = d.Caps.FileWriteMB
	}
}

func rawDefaults() []byte {
	return []byte(`
server:
  addr: "127.0.0.1:8080"
storage:
  data_dir: "./data"
sandbox:
  nsjail_path: "nsjail"
  user: "sandbox"
  group: "sandbox"
  kill_grace_sec: 3
limits:
  cpu_cores: 1
  memory_mb: 512
  disk_mb: 1024
  max_wall_time_sec: 300
  max_runs: 3
  allow_net: false
max_limits:
  cpu_cores: 8
  memory_mb: 8192
  disk_mb: 51200
  max_wall_time_sec: 3600
  max_runs: 64
  allow_net: true
caps:
  exec_stdout_kb: 256
  exec_stderr_kb: 256
  run_log_mb: 10
  json_body_mb: 1
  upload_mb: 50
  file_read_mb: 5
  file_write_mb: 10
`)
}
