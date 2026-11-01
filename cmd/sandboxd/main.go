// Command sandboxd runs the HTTP/JSON code-execution backend.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"net/netip"
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
	"sandboxd/internal/network"
	"sandboxd/internal/service"
	"sandboxd/internal/storage"
)

// mustPrefix parses a CIDR the service relies on; a malformed constant is a
// programming error and panics at startup rather than degrading silently.
func mustPrefix(s string) netip.Prefix {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		panic("invalid network prefix " + s + ": " + err.Error())
	}
	return p
}

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
	// Attach the isolated-network + tunnel layer. Egress is delivered by
	// chained slirp4netns (never host interface/route changes); quick tunnels
	// expose a workspace port with cloudflared inside the workspace namespace.
	// When the host lacks the tools the service still starts (runs get a fresh
	// empty network namespace) but /readyz reports the network unavailable and
	// egress/tunnels stay unusable.
	netMgr := network.NewManager(network.ManagerConfig{
		WorkspaceSubnet: mustPrefix("10.201.0.0/16"),
		NetnsDir:        cfg.Network.NetnsDir,
		RelayNetns:      cfg.Network.RelayNetns,
		ResolverDir:     store.TmpDir(),
		Network:         cfg.Network,
		Tunnels:         cfg.Tunnels,
	})
	if cfg.Network.Enabled {
		if err := netMgr.EnsureRelay(); err != nil {
			logger.Error("network relay setup failed; allow_net workspaces will have no egress", "error", err)
		}
	}
	svc.SetNetwork(netMgr)
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
	// A run or session never survives a daemon restart, so any sandbox cgroup
	// still present at startup is an orphan from a hard (SIGKILL) death: its
	// workload keeps running (for the life of its time_limit, or forever when
	// unbounded) and its NSJAIL.<pid> cgroup directory accumulates. Reap both
	// before serving, so the "on the way in" half of the no-leak guarantee holds.
	if n := svc.ReapOrphans(); n > 0 {
		logger.Info("reaped orphaned sandbox cgroups", "count", n)
	}
	// The same discipline for the network: no workspace namespace, relay or
	// slirp process survives a restart, so anything present on the way in is
	// reaped.
	if n := netMgr.ReapOrphans(nil); n > 0 {
		logger.Info("reaped orphaned workspace networks", "count", n)
	}
	if n := svc.ReapWorkspaceTunnels(); n > 0 {
		logger.Info("reaped orphaned tunnels", "count", n)
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
