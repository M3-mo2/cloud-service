package api

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

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
				r.Get("/usage", s.handleUsage)

				r.Get("/processes", s.handleListProcesses)
				r.Post("/processes/{processID}/stop", s.handleStopProcess)

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
		"mechanism": "ld.so.preload shim (uname/sysinfo/sysconf/sched_getaffinity/getauxval/dlsym)",
		"limits":    "a statically-linked binary, a musl runtime, a raw syscall(2), or a handle-scoped dlvsym(3) bypasses the shim",
	}
	if maskReason != "" {
		maskCap["reason"] = maskReason
	}
	overlayEnabled, overlayReady, overlayNote := s.svc.Runner().OverlayStatus()
	overlayCap := map[string]any{
		"enabled":   overlayEnabled,
		"ready":     overlayReady,
		"mechanism": "curated read-only /usr/local and /etc/ssl/certs overlays",
		"limits":    "hides the host's own /usr/local and vendor-named certificates; it cannot change the provider the egress address is registered to",
	}
	if overlayNote != "" {
		overlayCap["reason"] = overlayNote
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     model.Version,
		"api_version": model.APIVersion,
		"runtimes":    []string{"python3"},
		"limits_defaults": map[string]any{
			"cpu_cores":            l.CPUCores,
			"memory_mb":            l.MemoryMB,
			"disk_mb":              l.DiskMB,
			"max_wall_time_sec":    l.MaxWallTimeSec,
			"max_runs":             l.MaxRuns,
			"allow_net":            l.AllowNet,
			"allow_unlimited_runs": l.AllowUnlimitedRuns,
			"allow_tunnels":        l.AllowTunnels,
		},
		"limits_maxima": map[string]any{
			"cpu_cores":            m.CPUCores,
			"memory_mb":            m.MemoryMB,
			"disk_mb":              m.DiskMB,
			"max_wall_time_sec":    m.MaxWallTimeSec,
			"max_runs":             m.MaxRuns,
			"allow_net":            m.AllowNet,
			"allow_unlimited_runs": m.AllowUnlimitedRuns,
		},
		"tunnels":               tunCap,
		"network":               netCap,
		"identity_masking":      maskCap,
		"provider_tools_hidden": overlayCap,
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
