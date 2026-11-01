package service

import (
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/executor"
	"sandboxd/internal/model"
	"sandboxd/internal/network"
)

// Network is the per-workspace networking (egress) and quick-tunnel surface the
// service depends on. *network.Manager implements it on the real host; a fake
// implements it in tests.
type Network interface {
	Available() bool
	Ready() (bool, string)
	EgressEnabled() bool

	// EnsureWorkspace creates (or returns) a workspace's private egress
	// namespace with outbound internet through chained slirp4netns.
	EnsureWorkspace(wsID string) (*network.Allocation, error)
	// EnsureEmptyWorkspace creates a private, empty namespace (no egress) for a
	// workspace that is not opted in, or when the egress layer is unavailable.
	EnsureEmptyWorkspace(wsID string) (string, error)
	ReleaseWorkspace(wsID string)
	HasWorkspace(wsID string) bool
	NetnsPathFor(wsID string) string
	WriteResolverConf(alloc *network.Allocation) (string, error)
	ReapOrphans(live map[string]bool) int
	Stop()

	// Tunnels.
	StartTunnel(wsID, tunnelID string, port int, resolvConf string, timeout time.Duration) (*network.TunnelStart, error)
	StopTunnel(tunnelID string) bool
	StopWorkspaceTunnels(wsID string)
	TunnelRunning(tunnelID string) bool
	ReapTunnels(live map[string]bool) int
	TunnelsEnabled() bool
	TunnelsReady() (bool, string)
	TunnelsConfig() config.TunnelsConfig
}

// SetNetwork attaches the networking implementation. It is called once at
// startup; a nil network leaves runs in a fresh empty namespace and disables
// tunnels.
func (s *Service) SetNetwork(n Network) { s.net = n }

// NetworkEnabled reports whether a networking implementation is attached.
func (s *Service) NetworkEnabled() bool { return s.net != nil }

// EgressEnabled reports whether the userspace egress fabric is configured on.
func (s *Service) EgressEnabled() bool {
	return s.net != nil && s.net.EgressEnabled()
}

// NetworkStatus reports the isolated-egress capability truthfully.
func (s *Service) NetworkStatus() (available bool, ready bool, reason string) {
	if s.net == nil {
		return false, false, "network layer not configured"
	}
	if !s.net.Available() {
		return false, false, "network tools unavailable on this host"
	}
	ok, reason := s.net.Ready()
	return true, ok, reason
}

// TunnelsStatus reports the quick-tunnel capability truthfully.
func (s *Service) TunnelsStatus() (enabled bool, ready bool, reason string) {
	enabled = s.cfg.Tunnels.Enabled
	if !enabled {
		return false, false, "tunnels.enabled is false"
	}
	if s.net == nil {
		return true, false, "network layer not configured"
	}
	ok, reason := s.net.TunnelsReady()
	return true, ok, reason
}

// ApplyNetwork decorates an executor request with the workspace's isolated
// network: the private namespace to join and, when the workspace is opted in to
// egress, the public-resolver file. It returns the request unchanged when no
// usable network layer is attached (nsjail then creates a fresh empty
// namespace, which is never a host-network share). A failure to set up an
// opted-in workspace's egress is returned so the caller can refuse the run
// rather than silently run without the network the workspace asked for.
func (s *Service) ApplyNetwork(w *model.Workspace, req executor.Request) (executor.Request, error) {
	if s.net == nil || !s.net.Available() {
		// No usable network layer: leave NetnsPath empty so nsjail creates a
		// fresh, empty network namespace. This is never a host-network share,
		// so isolation is preserved even without a managed namespace.
		return req, nil
	}
	if w.Limits.AllowNet && s.net.EgressEnabled() {
		alloc, err := s.net.EnsureWorkspace(w.ID)
		if err != nil {
			return req, model.New(503, model.CodeUnavailable, "workspace network unavailable: "+err.Error())
		}
		req.NetnsPath = s.net.NetnsPathFor(w.ID)
		if rc, err := s.net.WriteResolverConf(alloc); err == nil {
			req.ResolverConf = rc
		}
		return req, nil
	}
	// Not opted in (or egress disabled): a private, empty namespace. The
	// workload can reach nothing, and never the host.
	if path, err := s.net.EnsureEmptyWorkspace(w.ID); err == nil {
		req.NetnsPath = path
	}
	return req, nil
}

// EnsureWorkspaceNetwork prepares the workspace's network namespace before a
// run is recorded, so a setup failure refuses the request. It returns nil when
// no usable network layer is attached.
func (s *Service) EnsureWorkspaceNetwork(w *model.Workspace) error {
	if s.net == nil || !s.net.Available() {
		return nil
	}
	if w.Limits.AllowNet && s.net.EgressEnabled() {
		if _, err := s.net.EnsureWorkspace(w.ID); err != nil {
			return model.New(503, model.CodeUnavailable, "workspace network unavailable: "+err.Error())
		}
		return nil
	}
	if _, err := s.net.EnsureEmptyWorkspace(w.ID); err != nil {
		return model.New(503, model.CodeUnavailable, "workspace network unavailable: "+err.Error())
	}
	return nil
}

// ReleaseWorkspaceNetwork tears down the workspace's namespace and egress.
func (s *Service) ReleaseWorkspaceNetwork(wsID string) {
	if s.net != nil {
		s.net.ReleaseWorkspace(wsID)
	}
}
