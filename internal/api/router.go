package api

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"sandboxd/internal/archive"
	"sandboxd/internal/auth"
	"sandboxd/internal/config"
	"sandboxd/internal/model"
	"sandboxd/internal/service"
)

// Server bundles HTTP handlers and their dependencies.
type Server struct {
	svc     *service.Service
	auth    *auth.Authenticator
	logger  *slog.Logger
	cfg     config.Config
	metrics *Metrics
}

// NewServer constructs the HTTP server.
func NewServer(svc *service.Service, a *auth.Authenticator, logger *slog.Logger) *Server {
	return &Server{
		svc:     svc,
		auth:    a,
		logger:  logger,
		cfg:     svc.Config(),
		metrics: NewMetrics(),
	}
}

// Router builds the chi router with all routes and middleware.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.metrics.Middleware)
	r.Use(auditMiddleware(s.logger))

	// Unauthenticated meta endpoints.
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Handle("/metrics", promhttp.HandlerFor(s.metrics.Registry(), promhttp.HandlerOpts{}))

	r.Route("/v1", func(r chi.Router) {
		r.Use(s.auth.Bearer)

		r.Get("/meta", s.handleMeta)

		// Per-workspace resource stream (RAM + disk). Both the stream (SSE) and
		// the one-shot snapshot are served service-wide and per workspace. They
		// are gated on metrics.enabled; a scoped stream carries only the
		// principal's own workspaces.
		r.Get("/metrics", s.handleMetricsSnapshot)
		r.Get("/metrics/stream", s.handleMetricsStream)

		r.Route("/workspaces", func(r chi.Router) {
			r.Post("/", s.handleCreateWorkspace)
			r.Get("/", s.handleListWorkspaces)

			r.Route("/{workspaceID}", func(r chi.Router) {
				r.Get("/", s.handleGetWorkspace)
				r.Patch("/", s.handlePatchWorkspace)
				r.Delete("/", s.handleDeleteWorkspace)

				r.Post("/suspend", s.handleSuspendWorkspace)
				r.Post("/resume", s.handleResumeWorkspace)
				r.Post("/stop", s.handleStopWorkspace)
				r.Post("/subscription/extend", s.handleExtendSubscription)
				r.Get("/usage", s.handleUsage)
				r.Get("/metrics", s.handleWorkspaceMetricsSnapshot)
				r.Get("/metrics/stream", s.handleWorkspaceMetricsStream)

				r.Get("/processes", s.handleListProcesses)
				r.Post("/processes/{processID}/stop", s.handleStopProcess)

				r.Get("/ports", s.handleListPorts)

				r.Post("/tunnels", s.handleCreateTunnel)
				r.Get("/tunnels", s.handleListTunnels)
				r.Get("/tunnels/{tunnelID}", s.handleGetTunnel)
				r.Delete("/tunnels/{tunnelID}", s.handleDeleteTunnel)

				r.Get("/packages", s.handleListPackages)
				r.Post("/packages/install", s.handleInstallPackages)
				r.Delete("/packages", s.handleDeletePackages)

				r.Get("/files", s.handleListFiles)
				r.Delete("/files", s.handleDeleteFile)
				r.Get("/files/stat", s.handleStatFile)
				r.Get("/files/content", s.handleGetFileContent)
				r.Put("/files/content", s.handlePutFileContent)
				r.Get("/files/download", s.handleDownloadFile)
				r.Post("/files/mkdir", s.handleMkdir)
				r.Post("/files/rename", s.handleRename)
				r.Post("/files/upload", s.handleUpload)
				r.Post("/files/archive", s.handleUploadArchive)
				r.Post("/files/extract", s.handleExtractArchive)
				r.Get("/files/archive", s.handleExportArchive)

				r.Post("/exec", s.handleExec)

				r.Post("/sessions", s.handleCreateSession)
				r.Get("/sessions", s.handleListSessions)
				r.Route("/sessions/{sessionID}", func(r chi.Router) {
					r.Get("/", s.handleGetSession)
					r.Delete("/", s.handleDeleteSession)
					r.Post("/input", s.handleSessionInput)
					r.Post("/signal", s.handleSessionSignal)
					r.Post("/resize", s.handleSessionResize)
					r.Get("/output", s.handleSessionOutput)
				})

				r.Post("/runs", s.handleCreateRun)
				r.Get("/runs", s.handleListRuns)
				r.Delete("/runs", s.handlePurgeRuns)
				r.Route("/runs/{runID}", func(r chi.Router) {
					r.Get("/", s.handleGetRun)
					r.Post("/stop", s.handleStopRun)
					r.Delete("/", s.handlePurgeRun)
					r.Get("/logs", s.handleGetRunLogs)
				})
			})
		})
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, model.New(404, model.CodeNotFound, "route not found"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, model.New(405, "validation", "method not allowed"))
	})

	return r
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	dataOK := s.dataDirWritable()
	nsjailOK := s.svc.Runner().Available()
	cgroupsOK := s.svc.Runner().CgroupsUsable()
	// The isolated network layer is reported truthfully: when the egress layer
	// is enabled but cannot serve, readiness is not claimed.
	netAvail, netReady, netReason := s.svc.NetworkStatus()
	tunEnabled, tunReady, tunReason := s.svc.TunnelsStatus()
	// Host-identity masking is reported truthfully too: when it is enabled but
	// the shim cannot be applied, readiness is not claimed, so an operator sees
	// a leaking deploy rather than a silent one.
	maskEnabled, maskReady, maskReason := s.svc.Runner().MaskStatus()
	// The provider-tool overlay is reported truthfully too: when it is enabled
	// but the curated trees could not be materialized, readiness is not claimed,
	// so an operator sees a deploy that would expose the platform's tooling
	// rather than a silent one.
	overlayEnabled, overlayReady, overlayNote := s.svc.Runner().OverlayStatus()
	// The profile registry is reported truthfully too: when it failed to load
	// (or the configured default profile is missing) the service cannot give a
	// workspace the allowlist it was promised, so readiness is not claimed.
	profileReady, profileNote := s.svc.Runner().ProfileStatus()
	// The egress-decoy layer is reported truthfully too: when it is enabled but
	// the CA or profile cannot be materialized, readiness is not claimed, so an
	// operator sees a deploy that would serve the real identity rather than a
	// silent one.
	decoyEnabled, decoyReady, decoyNote := s.svc.DecoyStatus()
	status := http.StatusOK
	if !dataOK || !nsjailOK || !cgroupsOK {
		status = http.StatusServiceUnavailable
	}
	if s.cfg.Network.Enabled && (!netAvail || !netReady) {
		status = http.StatusServiceUnavailable
	}
	if tunEnabled && !tunReady {
		status = http.StatusServiceUnavailable
	}
	if maskEnabled && !maskReady {
		status = http.StatusServiceUnavailable
	}
	if overlayEnabled && !overlayReady {
		status = http.StatusServiceUnavailable
	}
	if !profileReady {
		status = http.StatusServiceUnavailable
	}
	if decoyEnabled && !decoyReady {
		status = http.StatusServiceUnavailable
	}
	body := map[string]any{
		"status":   map[bool]string{true: "ready", false: "not_ready"}[status == http.StatusOK],
		"nsjail":   nsjailOK,
		"cgroups":  cgroupsOK,
		"data_dir": dataOK,
		"network": map[string]any{
			"enabled":   s.cfg.Network.Enabled,
			"available": netAvail,
			"ready":     netReady,
		},
		"tunnels": map[string]any{
			"enabled": tunEnabled,
			"ready":   tunReady,
		},
		"identity_masking": map[string]any{
			"enabled": maskEnabled,
			"ready":   maskReady,
		},
		"provider_tools_hidden": map[string]any{
			"enabled": overlayEnabled,
			"ready":   overlayReady,
		},
		"profiles": map[string]any{
			"ready":   profileReady,
			"default": s.cfg.Profiles.Default,
		},
		"egress_decoy": map[string]any{
			"enabled": decoyEnabled,
			"ready":   decoyReady,
		},
	}
	if netReason != "" {
		body["network"].(map[string]any)["reason"] = netReason
	}
	if tunReason != "" {
		body["tunnels"].(map[string]any)["reason"] = tunReason
	}
	if maskReason != "" {
		body["identity_masking"].(map[string]any)["reason"] = maskReason
	}
	if overlayNote != "" {
		body["provider_tools_hidden"].(map[string]any)["reason"] = overlayNote
	}
	if profileNote != "" {
		body["profiles"].(map[string]any)["reason"] = profileNote
	}
	if decoyNote != "" {
		body["egress_decoy"].(map[string]any)["reason"] = decoyNote
	}
	// Per-workspace resource visibility/enforcement: with it on, a workspace
	// sees its OWN disk allowance (df/stat -f/os.statvfs) and memory (free,
	// /proc/meminfo), enforced by a real per-workspace filesystem (kernel
	// boundary) and the cgroup. It is default ON. The synthetic /proc is
	// reported alongside so an operator can see exactly which files it carries.
	diskEnabled, diskMech, diskNote := s.svc.DiskStatus()
	body["workspace_limits"] = map[string]any{
		"enabled":    diskEnabled,
		"mechanism":  diskMech,
		"proc":       s.cfg.Disk.ProcEnabled,
		"proc_files": []string{"meminfo", "cpuinfo", "stat", "uptime", "loadavg", "version", "vmstat", "filesystems", "mounts", "self/mounts", "self/mountinfo", "self/limits"},
	}
	if diskNote != "" {
		body["workspace_limits"].(map[string]any)["reason"] = diskNote
	}
	writeJSON(w, status, body)
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	l := s.cfg.Limits
	m := s.cfg.MaxLimits
	tunEnabled, tunReady, tunReason := s.svc.TunnelsStatus()
	tunCap := map[string]any{
		"enabled":           tunEnabled,
		"ready":             tunReady,
		"max_per_workspace": s.cfg.Tunnels.MaxPerWorkspace,
		"url_timeout_sec":   s.cfg.Tunnels.URLTimeoutSec,
	}
	if tunReason != "" {
		tunCap["reason"] = tunReason
	}
	netAvail, netReady, netReason := s.svc.NetworkStatus()
	netCap := map[string]any{
		"enabled":   s.cfg.Network.Enabled,
		"available": netAvail,
		"ready":     netReady,
		"egress":    s.svc.EgressEnabled(),
	}
	if netReason != "" {
		netCap["reason"] = netReason
	}
	// Host-identity masking capability, reported truthfully: the syscall-
	// reachable host facts are masked for a dynamically-linked workload when
	// enabled+ready. The honest limits (static/musl/raw-syscall bypass) are
	// documented in docs/DECISIONS.md.
	maskEnabled, maskReady, maskReason := s.svc.Runner().MaskStatus()
	maskCap := map[string]any{
		"enabled":   maskEnabled,
		"ready":     maskReady,
		"mechanism": "ld.so.preload shim (uname/sysinfo/sysconf/sched_getaffinity/getauxval/dlsym, plus a filesystem interposer that hides /etc/ld.so.preload, the shim itself and the legacy engine dirs from the workspace)",
		"limits":    "a statically-linked binary, a musl runtime, a raw syscall(2), or a handle-scoped dlvsym(3) bypasses the shim; the shim and its preload list are invisible to the workspace (ADR 79), and only a raw syscall(2) additionally reaches /etc/ld.so.preload",
	}
	if maskReason != "" {
		maskCap["reason"] = maskReason
	}
	overlayEnabled, overlayReady, overlayNote := s.svc.Runner().OverlayStatus()
	overlayCap := map[string]any{
		"enabled":   overlayEnabled,
		"ready":     overlayReady,
		"mechanism": "profile-materialized read-only /usr (also bound at /bin, /sbin, /lib, /lib64), plus filtered /etc/{passwd,group,ssl/certs}",
		"limits":    "a workspace sees only the tools its profile names (an explicit allowlist) plus engine base commands, shared libraries and loaders; a profile entry naming the engine/platform is refused (fail loud), and it cannot change the provider the egress address is registered to",
	}
	if overlayNote != "" {
		overlayCap["reason"] = overlayNote
	}
	// The workspace profiles a client can choose from. Each entry lists the
	// commands and attached data/library paths, so a client can pick knowingly.
	profileDefault, profiles := s.svc.ProfileCatalogue()
	profileCap := map[string]any{
		"default":  profileDefault,
		"profiles": profiles,
	}
	if pr, note := s.svc.Runner().ProfileStatus(); !pr {
		profileCap["ready"] = false
		profileCap["reason"] = note
	} else {
		profileCap["ready"] = true
	}
	// Egress-decoy capability, reported truthfully: with it on, a workspace's
	// attempts to learn its own public IP are answered with a consistent
	// invented identity. It is a DECOY, not a security boundary; the honest
	// limits are enumerated in docs/EGRESS-DECOY.md and ADR 84.
	decoyEnabled, decoyReady, decoyNote := s.svc.DecoyStatus()
	decoyCap := map[string]any{
		"enabled":   decoyEnabled,
		"ready":     decoyReady,
		"mechanism": "in-namespace stub DNS resolver + stub HTTP/TLS responder, one consistent per-workspace decoy profile, with a generated CA trusted inside the workspace and in-namespace nft rules forcing DNS to the stub",
		"limits":    "a decoy layer, NOT a security boundary: a tenant's OWN endpoint (they read the peer address in their own logs) is unfixable; a hardcoded-IP endpoint outside the covered set, intermediate traceroute hops, and MTU/ICMP fingerprinting are out of scope; a knowledgeable tenant can recognise a documentation-range address",
	}
	if decoyNote != "" {
		decoyCap["reason"] = decoyNote
	}
	// Per-workspace resource visibility + enforcement. With it on (the
	// default), a workspace sees ITS OWN disk allowance and memory on every
	// channel, and cannot exceed the disk allowance (kernel boundary) or the
	// memory ceiling (cgroup). The mechanism is named truthfully.
	diskEnabled, diskMech, diskNote := s.svc.DiskStatus()
	diskCap := map[string]any{
		"enabled":   diskEnabled,
		"mechanism": diskMech,
	}
	if diskMech == "fuse" {
		diskCap["reporting"] = "real per-workspace ext4 filesystem image (fuse2fs), host-side mounted; df/stat -f/os.statvfs are the filesystem's own answers"
		diskCap["enforcement"] = "kernel filesystem boundary: a write past the allowance fails with ENOSPC, no overshoot"
		diskCap["limits"] = "one image file and one fuse2fs process per workspace; FUSE throughput overhead (measured ~0.4x native write, ~0.3x read)"
	} else if diskMech == "counter" {
		diskCap["reporting"] = "the API usage endpoint and the run watchdog"
		diskCap["enforcement"] = "engine used-bytes watchdog (real but SAMPLED; a running process can overshoot before the next poll)"
		diskCap["limits"] = "the sampled overshoot is stated in docs/DECISIONS.md; not equivalent to the kernel boundary"
	} else {
		diskCap["reporting"] = "none"
		diskCap["enforcement"] = "none"
		diskCap["limits"] = "disk.enabled=false: the allowance is neither reported as the workspace's own nor enforced"
	}
	if diskNote != "" {
		diskCap["reason"] = diskNote
	}
	// Per-workspace write-RATE ceiling: a SAMPLED cap on how fast a workspace's
	// disk may grow, enforced by stopping the offending run(s) after N
	// consecutive over-limit samples. Advertised truthfully: the switch, the N
	// rule, the server maximum, the distinct outcome code and the measured
	// overshoot bound. It is NEVER a kernel boundary (contrast the disk TOTAL);
	// see ADR 95.
	writeRateCap := map[string]any{
		"enabled":           s.svc.WriteRateEnabled(),
		"server_max_mb_s":   s.cfg.MaxLimits.WriteRateMBs,
		"default_mb_s":      s.cfg.Limits.WriteRateMBs,
		"samples":           s.svc.WriteRateSamples(),
		"outcome":           string(model.RunRateLimited),
		"refusal_code":      model.CodeWriteRateExceeded,
		"overshoot_formula": "N x interval_seconds x write_rate_mb_s (a SAMPLED ceiling, never a kernel boundary)",
		"mechanism":         "sample the workspace's real disk growth (one statfs on the metrics tick) and stop the run(s) after N consecutive over-limit samples; the engine's own write path (API upload/write, archive extraction) is refused with 429 write_rate_exceeded while over the ceiling",
		"limits":            "SAMPLED, not a kernel boundary: the worst-case overshoot is N x interval_seconds x write_rate_mb_s; a single burst above the ceiling does not kill a run; a workspace already at its disk ceiling is not a rate violation; usage that falls (a delete) resets the baseline. io.max cannot limit this here (measured) because the image is on ZFS and the bio is generated outside the cgroup.",
		"one_line_enable":   "disk: { write_rate_enabled: true }  # plus a per-workspace limits.write_rate_mb_s",
	}
	// The worst-case overshoot is a COMPUTED number for the configured default
	// ceiling, so the advertised bound is the real one and never a hand-waved
	// figure (ADR 95).
	if v := s.svc.WriteRateOvershootMB(s.cfg.Limits.WriteRateMBs); v > 0 {
		writeRateCap["default_overshoot_mb"] = v
	}
	// Workspace subscriptions: the engine owns the clock and enforcement, stores
	// an absolute expiry, and locks a lapsed term (distinct from suspension); the
	// way out is a renewal. Advertised truthfully: enabled is whether the reaper
	// runs; grace_period and the duration grammar are exposed so a client can
	// render a term without guessing. A workspace with no term is unlimited.
	maxDur, _ := s.cfg.Subscription.MaxDurationValue()
	graceDur, _ := s.cfg.Subscription.GracePeriodValue()
	subCap := map[string]any{
		"enabled":            s.cfg.Subscription.Enabled,
		"default_duration":   s.cfg.Subscription.DefaultDuration,
		"max_duration":       maxDur.String(),
		"grace_period":       graceDur.String(),
		"reap_interval":      s.cfg.Subscription.ReapInterval,
		"duration_grammar":   "s m h d w mo y (summed, e.g. 1d12h); mo=30d and y=365d are FIXED-LENGTH approximations, never calendar-exact",
		"expiry_semantics":   "absolute instants; extend adds while live, restarts from now when lapsed and unlocks; a live term is never shortened",
		"locked_status":      "locked (automatic, lapsed term) is distinct from suspended (manual); refused work is 403 subscription_expired, reads/usage/delete stay allowed",
		"summary_present_on": []string{"GET /v1/workspaces/{id}", "GET /v1/workspaces"},
	}
	// Native archive handling: extraction and export are performed IN THE
	// ENGINE (no shell tool), so the refusals are deterministic and identical on
	// every host. Advertised truthfully: enabled is the server switch, formats
	// is the content-detected allowlist, limits are the bounds actually in
	// force, and refusal_codes is the stable set a client can act on. RAR is
	// read-only (its encoder is proprietary); only ZIP is ever written.
	arc := s.cfg.Archives
	archivesCap := map[string]any{
		"enabled": arc.Enabled,
		"formats": arc.Formats,
		"read_only": []string{
			string(archive.FormatRAR),
		},
		"limits": map[string]any{
			"max_entries":  arc.MaxEntries,
			"max_total_mb": arc.MaxTotalMB,
			"max_file_mb":  arc.MaxFileMB,
			"max_ratio":    arc.MaxRatio,
			"upload_mb":    arc.UploadMB,
			"overwrite":    arc.Overwrite,
			"skip_junk":    arc.SkipJunk,
		},
		"routes": []string{
			"POST /v1/workspaces/{id}/files/archive",
			"POST /v1/workspaces/{id}/files/extract",
			"GET /v1/workspaces/{id}/files/archive",
		},
		"refusal_codes": []string{
			model.CodeUnsupportedArchive,
			model.CodeArchiveEncrypted,
			model.CodeArchiveMultivolume,
			model.CodeArchiveTooLarge,
			model.CodeArchiveTooManyEntries,
			model.CodeArchiveRatioExceeded,
			model.CodeArchiveUnsafeEntry,
			model.CodeArchiveEntryExists,
		},
		"semantics": "extraction validates every entry before writing (a refused archive leaves no partial tree); an entry that escapes the destination, is a symlink/hardlink, or sets a privileged bit is refused, and the bytes written are bounded by a streaming budget so a lying header cannot overshoot; overwrite:false (the default) refuses to clobber an existing file with 409 archive_entry_exists",
	}
	// Per-workspace resource stream: RAM + disk, pushed at a configurable
	// interval. Advertised truthfully: the effective (clamped) cadence, the
	// enforced floor and ceiling, whether the expensive cpu/pids extras are on,
	// and the subscriber ceiling. The RAM figure is the SUM over a workspace's
	// per-run and per-session cgroups; disk is one statfs (never a tree walk).
	metricsOn, metInterval, metMin, metMax, metCPU, metMaxSubs := s.svc.MetricsStatus()
	metricsCap := map[string]any{
		"enabled":          metricsOn,
		"interval_seconds": metInterval,
		"min_interval_sec": metMin,
		"max_interval_sec": metMax,
		"include_cpu":      metCPU,
		"max_subscribers":  metMaxSubs,
		"mechanism":        "one service-wide sampler goroutine (lazily started on the first subscriber, stopped on the last), one sample per tick fanned out to every subscriber; disk from a single statfs (O(1), never a tree walk); RAM summed over the workspace's per-run and per-session cgroups; idle workspaces cost zero cgroup reads",
		"limits":           "a client may not request a cadence below min_interval_sec (422); a workspace with no live cgroup reports RAM 0; each stream holds at most one pending frame (coalesce, never queue) and drops are counted",
		"routes": []string{
			"GET /v1/metrics",
			"GET /v1/metrics/stream",
			"GET /v1/workspaces/{id}/metrics",
			"GET /v1/workspaces/{id}/metrics/stream",
		},
	}
	// Native port/service discovery: ON demand, per workspace, answering which
	// ports are open and which process serves each one. Advertised truthfully:
	// the switch, the bounded-scan caps, whether unix sockets are reported and
	// the route. It is deliberately NOT part of the metrics stream (one table
	// read is measured at ~4.6 ms; see ADR 96). A port here is listening inside
	// the workspace's private namespace and is NOT thereby reachable from
	// outside.
	portsOn, portsMaxPids, portsMaxFDs, portsUnix := s.svc.PortsStatus()
	portsCap := map[string]any{
		"enabled":         portsOn,
		"max_pids":        portsMaxPids,
		"max_fds_per_pid": portsMaxFDs,
		"include_unix":    portsUnix,
		"mechanism":       "on-demand read of /proc/<pid>/net/{tcp,tcp6,udp,udp6,unix} for a pid inside the workspace's own namespace, with socket-inode -> owning-process attribution over the workspace's process registry; never cached and never on the metrics tick",
		"routes":          []string{"GET /v1/workspaces/{id}/ports"},
		"reachability":    "a reported port is listening inside the workspace's private network namespace; it is NOT reachable from outside unless a tunnel publishes it (limits.allow_tunnels AND tunnels.enabled)",
		"default_note":    "enabled by default: read-only introspection of the tenant's own workspace, which the tenant cannot see from inside (no /proc/net, no ss/netstat)",
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":       model.Version,
		"api_version":   model.APIVersion,
		"runtimes":      []string{"python3"},
		"profiles":      profileCap,
		"subscriptions": subCap,
		"archives":      archivesCap,
		"metrics":       metricsCap,
		"ports":         portsCap,
		"limits_defaults": map[string]any{
			"cpu_cores":            l.CPUCores,
			"memory_mb":            l.MemoryMB,
			"disk_mb":              l.DiskMB,
			"max_wall_time_sec":    l.MaxWallTimeSec,
			"max_runs":             l.MaxRuns,
			"allow_net":            l.AllowNet,
			"allow_unlimited_runs": l.AllowUnlimitedRuns,
			"allow_tunnels":        l.AllowTunnels,
			"write_rate_mb_s":      l.WriteRateMBs,
		},
		"limits_maxima": map[string]any{
			"cpu_cores":            m.CPUCores,
			"memory_mb":            m.MemoryMB,
			"disk_mb":              m.DiskMB,
			"max_wall_time_sec":    m.MaxWallTimeSec,
			"max_runs":             m.MaxRuns,
			"allow_net":            m.AllowNet,
			"allow_unlimited_runs": m.AllowUnlimitedRuns,
			"write_rate_mb_s":      m.WriteRateMBs,
		},
		"tunnels":               tunCap,
		"network":               netCap,
		"identity_masking":      maskCap,
		"provider_tools_hidden": overlayCap,
		"egress_decoy":          decoyCap,
		"workspace_limits":      diskCap,
		"write_rate":            writeRateCap,
		"interpreters":          model.SupportedInterpreters(),
		"runnable_extensions":   model.SupportedExtensions(),
	})
}

func (s *Server) dataDirWritable() bool {
	dir := s.svc.Store().DataDir()
	f, err := os.CreateTemp(dir, ".ready-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}
