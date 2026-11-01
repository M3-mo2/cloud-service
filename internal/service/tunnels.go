package service

import (
	"time"

	"sandboxd/internal/model"
)

// TunnelsForbiddenByServer reports that the server-level tunnels.enabled switch
// is false.
func TunnelsForbiddenByServer() error {
	return model.Wrap(422, model.CodeTunnelsDisabled,
		"tunnels are disabled by the server configuration",
		map[string]string{"config": "tunnels.enabled", "field": "allow_tunnels"})
}

// TunnelsForbiddenByWorkspace reports that the workspace has not opted in.
func TunnelsForbiddenByWorkspace() error {
	return model.Wrap(422, model.CodeTunnelsDisabled,
		"tunnels are not permitted for this workspace",
		map[string]string{"config": "limits.allow_tunnels", "field": "allow_tunnels"})
}

// TunnelsNeedEgress reports that a workspace cannot host a tunnel because it
// has no outbound internet (cloudflared must reach Cloudflare).
func TunnelsNeedEgress() error {
	return model.Wrap(422, model.CodeTunnelsDisabled,
		"tunnels require workspace egress (limits.allow_net)",
		map[string]string{"config": "limits.allow_net", "field": "allow_net"})
}

// TunnelsAllowed enforces the two-key rule (server switch AND workspace
// opt-in), plus the egress precondition cloudflared needs.
func (s *Service) TunnelsAllowed(w *model.Workspace) error {
	if !s.cfg.Tunnels.Enabled {
		return TunnelsForbiddenByServer()
	}
	if !w.Limits.AllowTunnels {
		return TunnelsForbiddenByWorkspace()
	}
	if !w.Limits.AllowNet {
		return TunnelsNeedEgress()
	}
	return nil
}

// CreateTunnel starts a quick Cloudflare tunnel exposing a workspace port. It
// returns the tunnel record with the URL captured from cloudflared's own
// output. The record is persisted before cloudflared starts, so a crash leaves
// a reapable record rather than an untracked process.
func (s *Service) CreateTunnel(w *model.Workspace, port int) (*model.Tunnel, error) {
	if err := s.ensureActive(w); err != nil {
		return nil, err
	}
	if err := s.TunnelsAllowed(w); err != nil {
		return nil, err
	}
	if s.net == nil {
		return nil, model.New(503, model.CodeUnavailable, "network layer not configured")
	}
	if port < 1 || port > 65535 {
		return nil, model.Wrap(400, model.CodeValidation, "port out of range",
			map[string]string{"port": itoaInt(port), "range": "1-65535"})
	}
	// The workspace binds the port as the unprivileged sandbox uid, so a
	// privileged port can never answer; accepting the request would be a lie.
	if port < 1024 {
		return nil, model.Wrap(400, model.CodeValidation, "privileged ports cannot be tunnelled",
			map[string]string{"port": itoaInt(port), "min": "1024"})
	}

	existing, err := s.store.ListTunnels(w.ID)
	if err != nil {
		return nil, err
	}
	live := 0
	for _, t := range existing {
		if t.Status == model.TunnelActive {
			live++
		}
	}
	if live >= s.cfg.Tunnels.MaxPerWorkspace {
		return nil, model.Wrap(422, model.CodeTooManyTunnels, "workspace tunnel limit reached",
			map[string]string{"limit": "tunnels.max_per_workspace", "max": itoaInt(s.cfg.Tunnels.MaxPerWorkspace)})
	}

	// The workspace must have an egress namespace, and a resolver file for
	// cloudflared. A setup failure is refused, never silently downgraded.
	alloc, err := s.net.EnsureWorkspace(w.ID)
	if err != nil {
		return nil, model.New(503, model.CodeUnavailable, "workspace network unavailable: "+err.Error())
	}
	resolvConf, _ := s.net.WriteResolverConf(alloc)

	now := time.Now().UTC()
	tun := &model.Tunnel{
		ID:          model.NewTunnelID(),
		WorkspaceID: w.ID,
		OwnerID:     w.OwnerID,
		Port:        port,
		Status:      model.TunnelActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.store.CreateTunnel(tun); err != nil {
		return nil, err
	}

	start, err := s.net.StartTunnel(w.ID, tun.ID, port, resolvConf, s.cfg.Tunnels.URLTimeout())
	if err != nil {
		// No URL observed, or cloudflared could not start: remove the record so
		// a failed create leaves nothing behind, and report honestly.
		_ = s.store.DeleteTunnel(w.ID, tun.ID)
		if _, ready, reason := s.TunnelsStatus(); !ready {
			return nil, model.New(503, model.CodeUnavailable, "tunnel unavailable: "+reason)
		}
		return nil, model.Wrap(504, model.CodeTunnelURLUnavailable,
			"no public URL was observed from cloudflared within the timeout",
			map[string]string{"port": itoaInt(port), "reason": err.Error()})
	}
	tun.URL = start.URL
	tun.PID = start.PID
	if err := s.store.SaveTunnel(tun); err != nil {
		return nil, err
	}
	s.touchActivity(w.ID)
	return tun, nil
}

// ListTunnels returns a workspace's tunnels, refreshing each status from the
// live process so the response is honest.
func (s *Service) ListTunnels(w *model.Workspace) ([]*model.Tunnel, error) {
	tunnels, err := s.store.ListTunnels(w.ID)
	if err != nil {
		return nil, err
	}
	for _, t := range tunnels {
		s.refreshTunnelStatus(t)
	}
	return tunnels, nil
}

// GetTunnel returns one tunnel, refreshing its status.
func (s *Service) GetTunnel(w *model.Workspace, tunnelID string) (*model.Tunnel, error) {
	t, err := s.store.GetTunnel(w.ID, tunnelID)
	if err != nil {
		return nil, err
	}
	s.refreshTunnelStatus(t)
	return t, nil
}

// DeleteTunnel stops the tunnel's cloudflared process and removes the record.
// After it returns, the public URL no longer serves.
func (s *Service) DeleteTunnel(w *model.Workspace, tunnelID string) error {
	t, err := s.store.GetTunnel(w.ID, tunnelID)
	if err != nil {
		return err
	}
	if s.net != nil {
		s.net.StopTunnel(t.ID)
	}
	if err := s.store.DeleteTunnel(w.ID, tunnelID); err != nil {
		return err
	}
	s.touchActivity(w.ID)
	return nil
}

// ReleaseWorkspaceTunnels stops every cloudflared process of a workspace and
// removes its tunnel records. Called on stop, suspend and delete; idempotent.
func (s *Service) ReleaseWorkspaceTunnels(w *model.Workspace) {
	if w == nil {
		return
	}
	if s.net != nil {
		s.net.StopWorkspaceTunnels(w.ID)
	}
	tunnels, err := s.store.ListTunnels(w.ID)
	if err != nil {
		return
	}
	for _, t := range tunnels {
		_ = s.store.DeleteTunnel(w.ID, t.ID)
	}
}

// refreshTunnelStatus updates a tunnel's status from the live process: a tunnel
// whose cloudflared process is gone is `stopped`.
func (s *Service) refreshTunnelStatus(t *model.Tunnel) {
	if s.net == nil {
		return
	}
	live := s.net.TunnelRunning(t.ID)
	want := model.TunnelStopped
	if live {
		want = model.TunnelActive
	}
	if t.Status == want {
		return
	}
	t.Status = want
	t.UpdatedAt = time.Now().UTC()
	_ = s.store.SaveTunnel(t)
}

// ReapWorkspaceTunnels removes tunnel records and stops cloudflared processes
// that belong to no live workspace at startup. It returns the number of records
// reaped.
func (s *Service) ReapWorkspaceTunnels() int {
	if s.net != nil {
		s.net.ReapTunnels(nil)
	}
	tunnels, err := s.store.ListAllTunnels()
	if err != nil {
		return 0
	}
	reaped := 0
	for _, t := range tunnels {
		if _, err := s.store.GetWorkspace(t.WorkspaceID); err == nil {
			continue
		}
		_ = s.store.DeleteTunnel(t.WorkspaceID, t.ID)
		reaped++
	}
	return reaped
}
