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
				r.Delete("/", s.handleDeleteWorkspace)

				r.Get("/files", s.handleListFiles)
				r.Delete("/files", s.handleDeleteFile)
				r.Get("/files/content", s.handleGetFileContent)
				r.Put("/files/content", s.handlePutFileContent)
				r.Post("/files/mkdir", s.handleMkdir)
				r.Post("/files/rename", s.handleRename)
				r.Post("/files/upload", s.handleUpload)

				r.Post("/exec", s.handleExec)

				r.Post("/runs", s.handleCreateRun)
				r.Get("/runs", s.handleListRuns)
				r.Route("/runs/{runID}", func(r chi.Router) {
					r.Get("/", s.handleGetRun)
					r.Post("/stop", s.handleStopRun)
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
	status := http.StatusOK
	if !dataOK || !nsjailOK || !cgroupsOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"status":   map[bool]string{true: "ready", false: "not_ready"}[status == http.StatusOK],
		"nsjail":   nsjailOK,
		"cgroups":  cgroupsOK,
		"data_dir": dataOK,
	})
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	l := s.cfg.Limits
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     model.Version,
		"api_version": model.APIVersion,
		"runtimes":    []string{"python3"},
		"limits_defaults": map[string]any{
			"cpu_cores":         l.CPUCores,
			"memory_mb":         l.MemoryMB,
			"disk_mb":           l.DiskMB,
			"max_wall_time_sec": l.MaxWallTimeSec,
			"max_runs":          l.MaxRuns,
			"allow_net":         l.AllowNet,
		},
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
