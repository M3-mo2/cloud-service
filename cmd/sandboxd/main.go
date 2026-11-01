// Command sandboxd runs the HTTP/JSON code-execution backend.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"sandboxd/internal/api"
	"sandboxd/internal/auth"
	"sandboxd/internal/config"
	"sandboxd/internal/executor"
	"sandboxd/internal/model"
	"sandboxd/internal/service"
	"sandboxd/internal/storage"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "path", *configPath, "error", err)
		os.Exit(1)
	}

	store, err := storage.New(cfg.Storage.DataDir)
	if err != nil {
		logger.Error("failed to initialize storage", "data_dir", cfg.Storage.DataDir, "error", err)
		os.Exit(1)
	}

	tmplPath := resolveTemplatePath(*configPath)
	runner := executor.NewNsjailRunner(cfg, tmplPath, store.TmpDir())

	svc := service.New(cfg, store, runner, logger)
	if n, err := svc.RecoverRuns(); err != nil {
		logger.Error("run recovery failed", "error", err)
	} else if n > 0 {
		logger.Info("recovered orphaned runs", "count", n)
	}
	if n, err := svc.RecoverSessions(); err != nil {
		logger.Error("session recovery failed", "error", err)
	} else if n > 0 {
		logger.Info("recovered orphaned sessions", "count", n)
	}
	authn := auth.New(cfg.Keys)
	server := api.NewServer(svc, authn, logger)

	httpServer := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           server.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if !runner.Available() {
		logger.Warn("nsjail backend unavailable; runs will return 503", "nsjail_path", cfg.Sandbox.NsjailPath)
	}

	logger.Info("starting sandboxd",
		"version", model.Version,
		"addr", cfg.Server.Addr,
		"data_dir", store.DataDir(),
		"nsjail", runner.Available(),
	)

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		logger.Error("server error", "error", err)
		os.Exit(1)
	case sig := <-stop:
		logger.Info("shutting down", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	if !svc.Shutdown(5 * time.Second) {
		logger.Warn("run drain deadline exceeded; some runs may not have terminated")
	}
	if !svc.ShutdownSessions(5 * time.Second) {
		logger.Warn("session drain deadline exceeded; some sessions may not have terminated")
	}
	logger.Info("stopped")
}

// resolveTemplatePath locates the nsjail template relative to CWD, then the
// config file's directory, falling back to the workspace-relative default.
func resolveTemplatePath(configPath string) string {
	candidates := []string{
		executor.ConfigTemplatePath("."),
	}
	if abs, err := filepath.Abs(configPath); err == nil {
		candidates = append(candidates, executor.ConfigTemplatePath(filepath.Dir(abs)))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}
