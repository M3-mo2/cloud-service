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
	Server    ServerConfig   `koanf:"server"`
	Storage   StorageConfig  `koanf:"storage"`
	Sandbox   SandboxConfig  `koanf:"sandbox"`
	Limits    LimitsConfig   `koanf:"limits"`
	MaxLimits LimitsConfig   `koanf:"max_limits"`
	Caps      CapsConfig     `koanf:"caps"`
	Packages  PackagesConfig `koanf:"packages"`
	Sessions  SessionsConfig `koanf:"sessions"`
	Network   NetworkConfig  `koanf:"network"`
	Tunnels   TunnelsConfig  `koanf:"tunnels"`
	Identity  IdentityConfig `koanf:"identity"`
	Profiles  ProfilesConfig `koanf:"profiles"`
	Disk      DiskConfig     `koanf:"disk"`
	Keys      []KeyConfig    `koanf:"keys"`
}

// DiskConfig controls how each workspace's disk allowance is ENFORCED and
// REPORTED. The product requirement is that `df -h`, `stat -f` and
// os.statvfs() report the workspace's OWN allowance, and that a workspace
// cannot exceed it.
//
// The default mechanism is a real per-workspace ext4 filesystem image mounted
// with fuse2fs (kernel-enforced: a write past the allowance fails with ENOSPC
// at the boundary, no overshoot, and every reporting channel is correct for
// free because the mounted filesystem IS the allowance). It is ON by default;
// the previous behaviour (a tenant seeing the platform's ZFS volume and its
// `zfs` type) is itself a leak of the host filesystem identity.
//
// When the FUSE mechanism is unavailable (no /dev/fuse, missing tools) the
// engine falls back to its own used-bytes counter promoted to an authority over
// running processes. That fallback is REAL but SAMPLED: a process already
// running can overshoot before the next check terminates it. The measured
// overshoot is stated in docs/DECISIONS.md; it is never presented as
// equivalent to the kernel boundary.
type DiskConfig struct {
	// Enabled is the server-level switch. Default true. When false no
	// per-workspace filesystem is mounted and the disk allowance is not
	// enforced or reported as the workspace's own (a diagnostic state).
	Enabled bool `koanf:"enabled"`
	// Backend selects the enforcement mechanism: "fuse" (default, kernel
	// boundary), "counter" (engine watchdog; real but sampled), or "none".
	// "auto" behaves as "fuse" when the host supports it and "counter"
	// otherwise. An unrecognised value is treated as "auto".
	Backend string `koanf:"backend"`
	// Fuse2FSPath / MkfsExt4Path / Resize2FSPath / FusermountPath override the
	// binaries used to build and mount the image. Empty uses the conventional
	// name on PATH.
	Fuse2FSPath    string `koanf:"fuse2fs_path"`
	MkfsExt4Path   string `koanf:"mkfs_ext4_path"`
	Resize2FSPath  string `koanf:"resize2fs_path"`
	FusermountPath string `koanf:"fusermount_path"`
	// CounterPollMS bounds how often the counter watchdog audits a workspace's
	// usage and stops runs that exceed the allowance. Smaller is a tighter
	// overshoot bound and more CPU; 0 uses the default (500 ms).
	CounterPollMS int `koanf:"counter_poll_ms"`
	// ProcEnabled controls the synthetic per-workspace /proc payload (meminfo,
	// cpuinfo, uptime, loadavg, stat, version and a curated mounts list), so
	// `free -h`, `uptime` and `vmstat` report the workspace's own memory and
	// never the host's. Default true. mount_proc stays false: this is a curated
	// directory bind-mounted over /proc, not the real procfs (which leaks host
	// process information).
	ProcEnabled bool `koanf:"proc_enabled"`
}

// ProfilesConfig controls the named, explicit-allowlist workspace profiles.
//
// A profile is a reviewable list of the tools (and the data/library trees those
// tools need) a workspace gets. Creating a workspace names the profile it
// wants; the default profile is used when it names none. The profiles
// themselves live in data files (configs/profiles/*.yaml, embedded in the
// binary) plus an optional operator directory, so adding a language means
// adding a file, never editing the engine. See internal/profile and
// docs/DECISIONS.md.
type ProfilesConfig struct {
	// Dir is an optional operator directory of additional/override profile YAML
	// files. Each file's stem must equal its `name`; a file overlays a
	// same-named built-in or adds a new profile. Empty uses only the built-ins.
	Dir string `koanf:"dir"`
	// Default is the profile a workspace gets when its create request names
	// none. It must name a loaded profile. Default "python".
	Default string `koanf:"default"`
}

// Default host-identity mask values. The kernel strings are deliberately
// generic and carry no platform fingerprint; the mask is applied only to
// uname/sysinfo/sysconf, whose real values would identify the host. They name
// no engine (never `sandboxd`), because a tenant can read them back.
const (
	// DefaultMaskKernelRelease is the uname.release a workspace is told.
	DefaultMaskKernelRelease = "0.0.0-sandbox"
	// DefaultMaskKernelVersion is the uname.version a workspace is told.
	DefaultMaskKernelVersion = "#1 SMP"
	// DefaultMaskNodename is the uname.nodename a workspace is told.
	DefaultMaskNodename = "sandbox"
)

// IdentityConfig controls host-identity masking, the layer that stops a
// workspace from learning which machine it runs on. Hiding /proc and /sys (the
// mount namespace) closes the filesystem view, but a set of facts remains
// reachable by SYSCALL, which no mount hides:
//
//	uname().release/version  -> the platform kernel fingerprint
//	os.cpu_count()/sysconf    -> the HOST's real core count
//	sysinfo()/sysconf(_SC_PHYS_PAGES) -> the HOST's real total RAM
//	gnu_get_libc_version()/confstr -> the base image's distro patch level
//
// The layer is an LD_PRELOAD shim reachable for every dynamically-linked exec
// via a read-only /etc/ld.so.preload, answering with the WORKSPACE's limits.
// It is ON by default; an operator turns it off only to measure what leaks.
//
// Limits (documented, not implied away): a statically-linked binary, a musl
// runtime (which does not read /etc/ld.so.preload), or a raw syscall(2)
// bypasses the shim and sees the host values. The masking layer is
// defense-in-depth, not a security boundary; see docs/DECISIONS.md.
type IdentityConfig struct {
	// Enabled is the server-level switch. Default true: masking is the
	// isolation guarantee, so it is opt-OUT, not opt-in. When false the shim is
	// not mounted and the syscall-reachable facts are not masked (a diagnostic
	// state, documented as such).
	Enabled bool `koanf:"enabled"`
	// ShimPath optionally overrides the host path of the compiled shim. Empty
	// (the normal case) uses the shim embedded in the binary, written to the
	// service tmp dir at startup. It exists so a deployment can point at a
	// separately-built shim.
	ShimPath string `koanf:"shim_path"`
	// KernelRelease is the uname.release value a workspace is told. Empty uses
	// the generic default. It must never be a real kernel string.
	KernelRelease string `koanf:"kernel_release"`
	// KernelVersion is the uname.version value a workspace is told.
	KernelVersion string `koanf:"kernel_version"`
	// HideProviderTools selects the workspace PROFILE mechanism: when true (the
	// default) a workspace's /usr is replaced by a profile-materialized tree
	// that contains ONLY the tools the profile names (an explicit allowlist),
	// plus the engine-provided base, the shared libraries and the loaders. When
	// false the workspace sees the host /usr unchanged (a diagnostic state that
	// deliberately exposes the platform's tooling).
	//
	// The engine/platform name+content filter still applies on top of a
	// profile: a profile entry whose name or content names the engine or the
	// platform is refused (fail loud), never silently shipped.
	HideProviderTools bool `koanf:"hide_provider_tools"`
	// ToolsRoot is the host filesystem root the curated tool trees are read
	// from. Empty (the normal case) reads the real host (/usr/bin, /usr/lib,
	// ...). It is overridable so a unit test can point materialization at a
	// small fixture tree instead of the real host, exactly as the previous
	// tools_*_dir overrides did.
	ToolsRoot string `koanf:"tools_root"`
}

// NetworkConfig controls the per-workspace egress fabric. Egress is delivered by
// chained slirp4netns instances, never by host interface/route changes: an
// engine-owned "relay" namespace is connected to the host through one
// slirp4netns instance, and each workspace namespace is connected to the relay
// through a second slirp4netns instance. The relay carries a blackhole route for
// every private range, so a workspace can reach the public internet and DNS but
// never the host LAN, the host's own services, or another workspace.
//
// The whole layer is opt-in per workspace (limits.allow_net). When the required
// tools are missing the layer reports unavailable and allow_net workspaces fall
// back to an empty namespace (no egress), never to the host network.
type NetworkConfig struct {
	// Enabled is the server-level switch. When false, no relay or workspace
	// namespace is created and allow_net workspaces get an empty namespace.
	Enabled bool `koanf:"enabled"`
	// NetnsDir is where the engine's namespaces are bind-mounted (default
	// /run/netns). The engine never uses `ip netns add`; it creates namespaces
	// with unshare and bind-mounts them here (see internal/network).
	NetnsDir string `koanf:"netns_dir"`
	// RelayNetns is the name of the shared intermediate namespace under
	// NetnsDir (default sbxdrelay).
	RelayNetns string `koanf:"relay_netns"`
	// SlirpPath is the slirp4netns binary (default slirp4netns). It is the only
	// way a workspace is given outbound internet: it speaks to the host over a
	// UNIX socket, so zero host interfaces or routes are created.
	SlirpPath string `koanf:"slirp_path"`
	// MTU is passed to each slirp instance (default 65520, the measured-working
	// value on the target host).
	MTU int `koanf:"mtu"`
	// Resolver is the public DNS resolver list written into each allow_net
	// workspace's /etc/resolv.conf (default 1.1.1.1 and 8.8.8.8). The host's
	// own resolver is deliberately never published to a workspace.
	Resolver []string `koanf:"resolver"`
	// StartTimeoutMS bounds how long a slirp instance is given to configure its
	// tap before the interface is treated as up (default 5000).
	StartTimeoutMS int `koanf:"start_timeout_ms"`
	// EgressProxy, when set, is an outbound proxy URL (http://host:port or
	// socks5://host:port) that an allow_net workspace's tooling is told to use,
	// via the standard HTTP_PROXY/HTTPS_PROXY/ALL_PROXY environment variables.
	// Default empty (off): the workspace egresses through the platform's shared
	// NAT, whose address is registered to the hosting provider (see
	// docs/DECISIONS.md ADR 76). Setting it changes the outbound address and
	// therefore the operator's abuse/reputation profile, so it is the operator's
	// explicit choice, never a default. It is a soft mechanism: a program that
	// ignores the proxy environment variables, or reaches an address directly,
	// is not constrained by it.
	EgressProxy string `koanf:"egress_proxy"`
	// Decoy is the egress-decoy layer: an in-namespace stub resolver and
	// stub HTTP/TLS responder that answer a workspace's attempts to learn its
	// own public IP with a plausible invented identity. DEFAULT OFF, so no
	// existing gate changes unless an operator turns it on. It is a decoy
	// layer, not a security boundary: see docs/EGRESS-DECOY.md and ADR 84.
	Decoy DecoyConfig `koanf:"decoy"`
}

// DecoyConfig controls the egress-decoy layer. Every field has a safe default;
// turning the layer on is a single `network.decoy.enabled: true`.
type DecoyConfig struct {
	// Enabled turns the whole layer on. When false nothing is started and the
	// workspace's resolv.conf names the configured public resolvers as before.
	Enabled bool `koanf:"enabled"`
	// AddressRange is the CIDR decoy addresses are drawn from. Documentation
	// ranges (198.51.100.0/24, 203.0.113.0/24, 192.0.2.0/24) are the safe
	// default; a knowledgeable tenant can recognise them, so an operator may
	// prefer a range he owns. See the ADR.
	AddressRange string `koanf:"address_range"`
	// Org / ASN / City / Country / Region / RDNS are the shared decoy profile
	// fields served by every channel.
	Org     string `koanf:"org"`
	ASN     string `koanf:"asn"`
	City    string `koanf:"city"`
	Country string `koanf:"country"`
	Region  string `koanf:"region"`
	RDNS    string `koanf:"rdns"`
	// ResolverPort / HTTPPort / HTTPSPort are the in-namespace ports the stubs
	// bind; the nft rules redirect 53/80/443/853 to them. (DNS-over-TLS is
	// answered on HTTPSPort, which distinguishes a DNS-over-TLS frame from
	// HTTP, so no separate port is needed.)
	ResolverPort int `koanf:"resolver_port"`
	HTTPPort     int `koanf:"http_port"`
	HTTPSPort    int `koanf:"https_port"`
	// ExtraHosts / ExtraPatterns add hostnames or regular expressions to the
	// intercepted set (the built-in shape patterns already cover the common
	// services and invented variants).
	ExtraHosts    []string `koanf:"extra_hosts"`
	ExtraPatterns []string `koanf:"extra_patterns"`
}

// TunnelsConfig controls quick Cloudflare tunnels (cloudflared), exposing one
// workspace port to the public internet. It is default-deny, exactly like
// allow_unlimited_runs: both Enabled (server switch) and the workspace's
// limits.allow_tunnels must be true to create a tunnel. A tunnel also requires
// the workspace to have allow_net (cloudflared needs egress to Cloudflare).
//
// Quick tunnels need no Cloudflare account and no token: cloudflared prints a
// random https://<name>.trycloudflare.com URL. The engine only ever returns a
// URL it actually observed on cloudflared's own output.
type TunnelsConfig struct {
	// Enabled is the server-level switch. False (the default) refuses every
	// create with 422 tunnels_disabled.
	Enabled bool `koanf:"enabled"`
	// MaxPerWorkspace caps concurrent tunnels per workspace (default 2).
	MaxPerWorkspace int `koanf:"max_per_workspace"`
	// URLTimeoutSec bounds how long the engine waits for cloudflared to print
	// its public URL before failing with 504 tunnel_url_not_observed (default
	// 45). It bounds the wait; it never fabricates a URL.
	URLTimeoutSec int `koanf:"url_timeout_sec"`
	// CloudflaredPath is the cloudflared binary (default cloudflared). The
	// engine runs it from the HOST inside the workspace's network namespace; it
	// is not on PATH inside the workspace when identity.hide_provider_tools is
	// on (the curated /usr/local does not carry it), which is deliberate: the
	// workspace uses the tunnel API rather than the binary.
	CloudflaredPath string `koanf:"cloudflared_path"`
}

// URLTimeout returns the bounded wait for a tunnel URL.
func (t TunnelsConfig) URLTimeout() time.Duration {
	return time.Duration(t.URLTimeoutSec) * time.Second
}

// StartTimeout returns the slirp tap-configuration bound.
func (n NetworkConfig) StartTimeout() time.Duration {
	return time.Duration(n.StartTimeoutMS) * time.Millisecond
}

// SessionsConfig controls first-class server-side terminal sessions.
//
// Every key has a safe default, so the service starts with none of them set
// (see Default and applyDefaults). A session is a long-lived interactive shell
// running under a real PTY inside the workspace's nsjail sandbox; the executor
// relays a line-delimited JSON control protocol to that PTY.
type SessionsConfig struct {
	// MaxPerWorkspace bounds concurrent sessions (running or still-recorded)
	// for a single workspace. A create past the cap is rejected with
	// 409 too_many_sessions.
	MaxPerWorkspace int `koanf:"max_per_workspace"`
	// IdleTimeoutSec ends a session after this long without any input, signal,
	// resize or output activity. It ends with status timed_out and reason
	// idle_timeout.
	IdleTimeoutSec int `koanf:"idle_timeout_sec"`
	// LifetimeSec is the absolute wall-clock ceiling for a session, regardless
	// of activity. It ends with status timed_out and reason lifetime.
	LifetimeSec int `koanf:"lifetime_sec"`
	// OutputKB caps the persisted PTY output log per session. Past the cap the
	// bytes are dropped and the session is marked output_truncated.
	OutputKB int `koanf:"output_kb"`
	// MaxInputKB bounds a single input request body's data payload.
	MaxInputKB int `koanf:"max_input_kb"`
	// Shell is the default shell run inside the sandbox when a create request
	// omits one. It must be an absolute path available in the sandbox image.
	Shell string `koanf:"shell"`
	// KillGraceSec is the SIGHUP->SIGKILL grace applied when a session is
	// closed (DELETE or a timeout).
	KillGraceSec int `koanf:"kill_grace_sec"`
	// StartProbeMS is how long a session create waits for a relay that dies at
	// startup (e.g. it cannot allocate a PTY), so the create response can report
	// the session already failed rather than `running`. It is a short window: a
	// healthy shell keeps running and the create is not delayed by it.
	StartProbeMS int `koanf:"start_probe_ms"`
}

// StartProbe returns the session-start reap window.
func (s SessionsConfig) StartProbe() time.Duration {
	return time.Duration(s.StartProbeMS) * time.Millisecond
}

// IdleTimeout returns the session idle-timeout duration.
func (s SessionsConfig) IdleTimeout() time.Duration {
	return time.Duration(s.IdleTimeoutSec) * time.Second
}

// Lifetime returns the session absolute-lifetime duration.
func (s SessionsConfig) Lifetime() time.Duration {
	return time.Duration(s.LifetimeSec) * time.Second
}

// OutputBytes returns the per-session output cap in bytes.
func (s SessionsConfig) OutputBytes() int64 { return int64(s.OutputKB) * 1024 }

// MaxInputBytes returns the per-request input cap in bytes.
func (s SessionsConfig) MaxInputBytes() int64 { return int64(s.MaxInputKB) * 1024 }

// KillGrace returns the session SIGHUP->SIGKILL grace period.
func (s SessionsConfig) KillGrace() time.Duration {
	return time.Duration(s.KillGraceSec) * time.Second
}

// PackagesConfig controls the two-tier Python package model.
//
// The shared tier is a single host directory installed once and bind-mounted
// read-only into every sandbox, so heavy/common dependencies are not duplicated
// per workspace. The private tier is a per-workspace directory inside the
// workspace's own data directory; it is writable by the workspace's uid only
// and is created, backed up and deleted with the workspace.
type PackagesConfig struct {
	// SharedDir is the host directory bind-mounted read-only as the shared
	// package tier. When it is missing, empty or unreadable the service
	// degrades to private-only and runs still work.
	SharedDir string `koanf:"shared_dir"`
	// SharedEnabled toggles the shared tier mount. Defaults to true; a missing
	// SharedDir still degrades gracefully.
	SharedEnabled bool `koanf:"shared_enabled"`
	// SharedRequirements is an optional path to a requirements file that
	// documents what the shared image should contain. It is informational; the
	// service never installs from it.
	SharedRequirements string `koanf:"shared_requirements"`
	// InstallTimeoutSec bounds a single pip install wall-clock duration.
	InstallTimeoutSec int `koanf:"install_timeout_sec"`
	// InstallOutputKB bounds the captured pip stdout+stderr returned to the
	// client.
	InstallOutputKB int `koanf:"install_output_kb"`
}

// InstallTimeout returns the pip install wall-clock bound.
func (p PackagesConfig) InstallTimeout() time.Duration {
	return time.Duration(p.InstallTimeoutSec) * time.Second
}

// InstallOutputBytes returns the pip install captured-output cap in bytes.
func (p PackagesConfig) InstallOutputBytes() int64 { return int64(p.InstallOutputKB) * 1024 }

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
	// AllowUnlimitedRuns permits wall-clock-unbounded runs. In `limits:` it is
	// the per-workspace default (opt-in); in `max_limits:` it is the server-level
	// switch an operator uses to forbid unbounded runs everywhere. A request for
	// an unlimited run is accepted only when both are true; otherwise 422.
	// Default is false (deny).
	AllowUnlimitedRuns bool `koanf:"allow_unlimited_runs"`
	// AllowTunnels opts a workspace in to quick Cloudflare tunnels (the
	// per-workspace half of the two-key switch). It is meaningful only in
	// `limits:`; in `max_limits:` the server-level switch is tunnels.enabled.
	// Both must be true to create a tunnel; otherwise 422 tunnels_disabled.
	// Default false (deny).
	AllowTunnels bool `koanf:"allow_tunnels"`
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
	// FileListMax bounds the number of entries a single directory listing
	// returns. It is a browse-time page size, not a read cap: a directory with
	// more children reports truncated=true and total so a client can page.
	FileListMax int `koanf:"file_list_max"`
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

// FileListMaxEntries returns the directory-listing page size.
func (c CapsConfig) FileListMaxEntries() int { return c.FileListMax }

// KillGrace returns the SIGTERM->SIGKILL grace period.
func (s SandboxConfig) KillGrace() time.Duration {
	return time.Duration(s.KillGraceSec) * time.Second
}

// DefaultDecoy returns the neutral, DISABLED egress-decoy configuration. The
// documentation range 198.51.100.0/24 is the safe default; the ADR notes that a
// knowledgeable tenant can recognise it, so an operator may prefer a range he
// owns.
func DefaultDecoy() DecoyConfig {
	return DecoyConfig{
		Enabled:       false,
		AddressRange:  "198.51.100.0/24",
		Org:           "Example Transit Networks",
		ASN:           "AS64500",
		City:          "Frankfurt",
		Country:       "DE",
		Region:        "Hesse",
		RDNS:          "example-transit.net",
		ResolverPort:  15353,
		HTTPPort:      18080,
		HTTPSPort:     18443,
		ExtraHosts:    nil,
		ExtraPatterns: nil,
	}
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
			CPUCores:           1,
			MemoryMB:           512,
			DiskMB:             1024,
			MaxWallTimeSec:     300,
			MaxRuns:            3,
			AllowNet:           false,
			AllowUnlimitedRuns: false,
		},
		MaxLimits: LimitsConfig{
			CPUCores:           8,
			MemoryMB:           8192,
			DiskMB:             51200,
			MaxWallTimeSec:     3600,
			MaxRuns:            64,
			AllowNet:           true,
			AllowUnlimitedRuns: false,
		},
		Caps: CapsConfig{
			ExecStdoutKB: 256,
			ExecStderrKB: 256,
			RunLogMB:     10,
			JSONBodyMB:   1,
			UploadMB:     50,
			FileReadMB:   5,
			FileWriteMB:  10,
			FileListMax:  1000,
		},
		Packages: PackagesConfig{
			SharedDir:         "",
			SharedEnabled:     true,
			InstallTimeoutSec: 120,
			InstallOutputKB:   256,
		},
		Sessions: SessionsConfig{
			MaxPerWorkspace: 4,
			IdleTimeoutSec:  900,
			LifetimeSec:     3600,
			OutputKB:        1024,
			MaxInputKB:      64,
			Shell:           "/bin/sh",
			KillGraceSec:    3,
			StartProbeMS:    250,
		},
		Network: NetworkConfig{
			Enabled:        false,
			NetnsDir:       "/run/netns",
			RelayNetns:     "sbxdrelay",
			SlirpPath:      "slirp4netns",
			MTU:            65520,
			Resolver:       []string{"1.1.1.1", "8.8.8.8"},
			StartTimeoutMS: 5000,
			EgressProxy:    "",
			Decoy:          DefaultDecoy(),
		},
		Tunnels: TunnelsConfig{
			Enabled:         false,
			MaxPerWorkspace: 2,
			URLTimeoutSec:   45,
			CloudflaredPath: "cloudflared",
		},
		Identity: IdentityConfig{
			Enabled:           true,
			HideProviderTools: true,
			KernelRelease:     DefaultMaskKernelRelease,
			KernelVersion:     DefaultMaskKernelVersion,
		},
		Profiles: ProfilesConfig{
			Dir:     "",
			Default: "python",
		},
		Disk: DiskConfig{
			Enabled:       true,
			Backend:       "auto",
			CounterPollMS: 500,
			ProcEnabled:   true,
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
	if caps.FileListMax <= 0 {
		caps.FileListMax = d.Caps.FileListMax
	}
	pkgs := &c.Packages
	if pkgs.InstallTimeoutSec <= 0 {
		pkgs.InstallTimeoutSec = d.Packages.InstallTimeoutSec
	}
	if pkgs.InstallOutputKB <= 0 {
		pkgs.InstallOutputKB = d.Packages.InstallOutputKB
	}
	sess := &c.Sessions
	if sess.MaxPerWorkspace <= 0 {
		sess.MaxPerWorkspace = d.Sessions.MaxPerWorkspace
	}
	if sess.IdleTimeoutSec <= 0 {
		sess.IdleTimeoutSec = d.Sessions.IdleTimeoutSec
	}
	if sess.LifetimeSec <= 0 {
		sess.LifetimeSec = d.Sessions.LifetimeSec
	}
	if sess.OutputKB <= 0 {
		sess.OutputKB = d.Sessions.OutputKB
	}
	if sess.MaxInputKB <= 0 {
		sess.MaxInputKB = d.Sessions.MaxInputKB
	}
	if sess.Shell == "" {
		sess.Shell = d.Sessions.Shell
	}
	if sess.KillGraceSec <= 0 {
		sess.KillGraceSec = d.Sessions.KillGraceSec
	}
	if sess.StartProbeMS < 0 {
		sess.StartProbeMS = d.Sessions.StartProbeMS
	}
	net := &c.Network
	if net.NetnsDir == "" {
		net.NetnsDir = d.Network.NetnsDir
	}
	if net.RelayNetns == "" {
		net.RelayNetns = d.Network.RelayNetns
	}
	if net.SlirpPath == "" {
		net.SlirpPath = d.Network.SlirpPath
	}
	if net.MTU <= 0 {
		net.MTU = d.Network.MTU
	}
	if len(net.Resolver) == 0 {
		net.Resolver = d.Network.Resolver
	}
	if net.StartTimeoutMS <= 0 {
		net.StartTimeoutMS = d.Network.StartTimeoutMS
	}
	// Egress decoy: fill every unset field from the neutral defaults so a
	// partial `network.decoy` block (e.g. only `enabled: true`) still yields a
	// complete, safe profile. Enabled is deliberately NOT defaulted here: it
	// lives in rawDefaults so an explicit `enabled: true` survives.
	dec := &net.Decoy
	dd := d.Network.Decoy
	if dec.AddressRange == "" {
		dec.AddressRange = dd.AddressRange
	}
	if dec.Org == "" {
		dec.Org = dd.Org
	}
	if dec.ASN == "" {
		dec.ASN = dd.ASN
	}
	if dec.City == "" {
		dec.City = dd.City
	}
	if dec.Country == "" {
		dec.Country = dd.Country
	}
	if dec.Region == "" {
		dec.Region = dd.Region
	}
	if dec.RDNS == "" {
		dec.RDNS = dd.RDNS
	}
	if dec.ResolverPort <= 0 || dec.ResolverPort > 65535 {
		dec.ResolverPort = dd.ResolverPort
	}
	if dec.HTTPPort <= 0 || dec.HTTPPort > 65535 {
		dec.HTTPPort = dd.HTTPPort
	}
	if dec.HTTPSPort <= 0 || dec.HTTPSPort > 65535 {
		dec.HTTPSPort = dd.HTTPSPort
	}
	tun := &c.Tunnels
	if tun.MaxPerWorkspace <= 0 {
		tun.MaxPerWorkspace = d.Tunnels.MaxPerWorkspace
	}
	if tun.URLTimeoutSec <= 0 {
		tun.URLTimeoutSec = d.Tunnels.URLTimeoutSec
	}
	if tun.CloudflaredPath == "" {
		tun.CloudflaredPath = d.Tunnels.CloudflaredPath
	}
	// Identity.Enabled is deliberately NOT touched here: its default lives in
	// rawDefaults so an explicit `identity.enabled: false` survives, while the
	// empty kernel strings are safe to fill (an empty string is never valid).
	if c.Identity.KernelRelease == "" {
		c.Identity.KernelRelease = d.Identity.KernelRelease
	}
	if c.Identity.KernelVersion == "" {
		c.Identity.KernelVersion = d.Identity.KernelVersion
	}
	if c.Profiles.Default == "" {
		c.Profiles.Default = d.Profiles.Default
	}
	if c.Disk.Backend == "" {
		c.Disk.Backend = d.Disk.Backend
	}
	if c.Disk.CounterPollMS <= 0 {
		c.Disk.CounterPollMS = d.Disk.CounterPollMS
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
  allow_unlimited_runs: false
  allow_tunnels: false
max_limits:
  cpu_cores: 8
  memory_mb: 8192
  disk_mb: 51200
  max_wall_time_sec: 3600
  max_runs: 64
  allow_net: true
  allow_unlimited_runs: false
caps:
  exec_stdout_kb: 256
  exec_stderr_kb: 256
  run_log_mb: 10
  json_body_mb: 1
  upload_mb: 50
  file_read_mb: 5
  file_write_mb: 10
  file_list_max: 1000
packages:
  shared_dir: ""
  shared_enabled: true
  shared_requirements: ""
  install_timeout_sec: 120
  install_output_kb: 256
sessions:
  max_per_workspace: 4
  idle_timeout_sec: 900
  lifetime_sec: 3600
  output_kb: 1024
  max_input_kb: 64
  shell: "/bin/sh"
  kill_grace_sec: 3
  start_probe_ms: 250
network:
  enabled: false
  netns_dir: "/run/netns"
  relay_netns: "sbxdrelay"
  slirp_path: "slirp4netns"
  mtu: 65520
  resolver: ["1.1.1.1", "8.8.8.8"]
  start_timeout_ms: 5000
  egress_proxy: ""
  decoy:
    enabled: false
    address_range: "198.51.100.0/24"
    org: "Example Transit Networks"
    asn: "AS64500"
    city: "Frankfurt"
    country: "DE"
    region: "Hesse"
    rdns: "example-transit.net"
    resolver_port: 15353
    http_port: 18080
    https_port: 18443
    extra_hosts: []
    extra_patterns: []
tunnels:
  enabled: false
  max_per_workspace: 2
  url_timeout_sec: 45
  cloudflared_path: "cloudflared"
identity:
  enabled: true
  hide_provider_tools: true
  tools_root: ""
  kernel_release: "0.0.0-sandbox"
  kernel_version: "#1 SMP"
profiles:
  dir: ""
  default: "python"
disk:
  enabled: true
  backend: "auto"
  fuse2fs_path: ""
  mkfs_ext4_path: ""
  resize2fs_path: ""
  fusermount_path: ""
  counter_poll_ms: 500
  proc_enabled: true
`)
}
