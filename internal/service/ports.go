package service

import (
	"sort"
	"time"

	"sandboxd/internal/executor"
	"sandboxd/internal/model"
	"sandboxd/internal/network"
)

// Network is the per-workspace networking and published-port surface the
// service depends on. *network.Manager implements it on the real host; a fake
// implements it in tests.
type Network interface {
	Available() bool
	EnsureHub() error
	EnsureWorkspace(wsID string) (*network.Allocation, error)
	ReleaseWorkspace(wsID string)
	HasWorkspace(wsID string) bool
	AllocationAddr(wsID string) (string, bool)
	ProbePort(wsID string, port int) bool
	PickHostPort(used map[int]bool) (int, error)
	StartForwarder(portID string, hostPort int, targetAddr string, targetPort int) error
	StopForwarder(portID string)
	PortsReady() (bool, string)
	Ready() (bool, string)
	EgressEnabled() bool
	NetnsPathFor(wsID string) string
	ResolverConfPath() string
	WriteResolverConf(alloc *network.Allocation) (string, error)
	EgressEnv(alloc *network.Allocation) map[string]string
	ReapOrphans(live map[string]bool) int
	Stop()
}

// SetNetwork attaches the networking implementation. It is called once at
// startup; a nil network leaves runs in a fresh empty namespace and disables
// published ports.
func (s *Service) SetNetwork(n Network) { s.net = n }

// NetworkEnabled reports whether a networking implementation is attached.
func (s *Service) NetworkEnabled() bool { return s.net != nil }

// EgressEnabled reports whether the userspace egress proxy is running.
func (s *Service) EgressEnabled() bool {
	return s.net != nil && s.net.EgressEnabled()
}

// PortsActive reports whether the published-port capability is usable: the
// server switch is on, a network is attached, and the layer reports ready.
func (s *Service) PortsActive() bool {
	if !s.cfg.Ports.Enabled || s.net == nil || !s.net.Available() {
		return false
	}
	ok, _ := s.net.PortsReady()
	return ok
}

// PortsStatus reports the capability and its readiness truthfully (never ready
// when it is not).
func (s *Service) PortsStatus() (enabled bool, ready bool, reason string) {
	enabled = s.cfg.Ports.Enabled
	if !enabled {
		return false, false, "ports.enabled is false"
	}
	if s.net == nil {
		return true, false, "network layer not configured"
	}
	if !s.net.Available() {
		return true, false, "network tools unavailable on this host"
	}
	ok, reason := s.net.PortsReady()
	return true, ok, reason
}

// NetworkStatus reports the isolated-network capability.
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

// PortsForbiddenByServer reports that the server-level ports.enabled switch is
// false.
func PortsForbiddenByServer() error {
	return model.Wrap(422, model.CodePortsDisabled,
		"published ports are disabled by the server configuration",
		map[string]string{"config": "ports.enabled", "field": "allow_ports"})
}

// PortsForbiddenByWorkspace reports that the workspace has not opted in.
func PortsForbiddenByWorkspace() error {
	return model.Wrap(422, model.CodePortsDisabled,
		"published ports are not permitted for this workspace",
		map[string]string{"config": "limits.allow_ports", "field": "allow_ports"})
}

// PortsAllowed enforces the two-key rule: the server switch AND the workspace
// opt-in must both be true.
func (s *Service) PortsAllowed(w *model.Workspace) error {
	if !s.cfg.Ports.Enabled {
		return PortsForbiddenByServer()
	}
	if !w.Limits.AllowPorts {
		return PortsForbiddenByWorkspace()
	}
	return nil
}

// PublishSpec is a publish request.
type PublishSpec struct {
	Port     int
	Protocol string
	Name     string
}

// PublishPort maps an in-workspace port onto a fresh host port and starts the
// forwarder. It is owner-scoped (the caller passes an owned workspace). The
// forward target comes from the workspace's own allocation, never from the
// request.
func (s *Service) PublishPort(w *model.Workspace, spec PublishSpec) (*model.PublishedPort, error) {
	if err := s.ensureActive(w); err != nil {
		return nil, err
	}
	if err := s.PortsAllowed(w); err != nil {
		return nil, err
	}

	protocol := spec.Protocol
	if protocol == "" {
		protocol = "tcp"
	}
	if protocol != "tcp" {
		return nil, model.Wrap(400, model.CodeValidation, "unsupported protocol",
			map[string]string{"protocol": protocol, "supported": "tcp"})
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return nil, model.Wrap(400, model.CodeValidation, "port out of range",
			map[string]string{"port": itoaInt(spec.Port), "range": "1-65535"})
	}
	// Privileged ports (1-1023) are refused: the sandbox runs as an unprivileged
	// uid and can never bind them, so accepting the publish would be a lie.
	if spec.Port < 1024 {
		return nil, model.Wrap(400, model.CodeValidation, "privileged ports cannot be published",
			map[string]string{"port": itoaInt(spec.Port), "min": "1024"})
	}

	// Caps: per workspace, then per principal.
	existing, err := s.store.ListPorts(w.ID)
	if err != nil {
		return nil, err
	}
	if len(existing) >= s.cfg.Ports.MaxPerWorkspace {
		return nil, model.Wrap(422, model.CodeTooManyPorts, "workspace port limit reached",
			map[string]string{"limit": "ports.max_per_workspace", "max": itoaInt(s.cfg.Ports.MaxPerWorkspace)})
	}
	principalCount, err := s.principalPortCount(w.OwnerID, "")
	if err != nil {
		return nil, err
	}
	if principalCount >= s.cfg.Ports.MaxPerPrincipal {
		return nil, model.Wrap(422, model.CodeTooManyPorts, "principal port limit reached",
			map[string]string{"limit": "ports.max_per_principal", "max": itoaInt(s.cfg.Ports.MaxPerPrincipal)})
	}

	// Ensure the workspace has its own isolated network, and derive the forward
	// target from that allocation.
	alloc, err := s.EnsureWorkspaceNetwork(w.ID)
	if err != nil {
		return nil, err
	}
	targetAddr := alloc.GuestAddr.String()

	// Pick a free host port, excluding every already-published host port.
	used, err := s.usedHostPorts(w.ID)
	if err != nil {
		return nil, err
	}
	hostPort, err := s.net.PickHostPort(used)
	if err != nil {
		return nil, model.New(503, model.CodeUnavailable, "no free host port: "+err.Error())
	}

	now := time.Now().UTC()
	port := &model.PublishedPort{
		ID:             model.NewPortID(),
		WorkspaceID:    w.ID,
		OwnerID:        w.OwnerID,
		Port:           spec.Port,
		Protocol:       protocol,
		Name:           spec.Name,
		HostPort:       hostPort,
		Host:           s.cfg.Ports.BindAddr,
		Status:         model.PortUnreachable,
		AllocationAddr: targetAddr,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// Persist the record before binding the listener so a crash between the two
	// leaves a reapable record rather than an untracked listener.
	if err := s.store.CreatePort(port); err != nil {
		return nil, err
	}
	if err := s.net.StartForwarder(port.ID, hostPort, targetAddr, spec.Port); err != nil {
		_ = s.store.DeletePort(w.ID, port.ID)
		return nil, model.New(503, model.CodeUnavailable, "could not start forwarder: "+err.Error())
	}
	// Set the honest initial status from a live probe.
	port.Status = s.observePortStatus(w.ID, spec.Port)
	if err := s.store.SavePort(port); err != nil {
		return nil, err
	}
	s.touchActivity(w.ID)
	return port, nil
}

// ListPorts returns a workspace's published ports, refreshing each status from
// a live probe so the response is honest.
func (s *Service) ListPorts(w *model.Workspace) ([]*model.PublishedPort, error) {
	ports, err := s.store.ListPorts(w.ID)
	if err != nil {
		return nil, err
	}
	for _, p := range ports {
		s.refreshPortStatus(w.ID, p)
	}
	return ports, nil
}

// GetPort returns one published port, refreshing its status.
func (s *Service) GetPort(w *model.Workspace, portID string) (*model.PublishedPort, error) {
	p, err := s.store.GetPort(w.ID, portID)
	if err != nil {
		return nil, err
	}
	s.refreshPortStatus(w.ID, p)
	return p, nil
}

// UnpublishPort stops the forwarder and removes the record, freeing the host
// port.
func (s *Service) UnpublishPort(w *model.Workspace, portID string) error {
	p, err := s.store.GetPort(w.ID, portID)
	if err != nil {
		return err
	}
	if s.net != nil {
		s.net.StopForwarder(p.ID)
	}
	if err := s.store.DeletePort(w.ID, portID); err != nil {
		return err
	}
	s.touchActivity(w.ID)
	return nil
}

// ReleaseWorkspacePorts stops every forwarder of a workspace and removes its
// port records. It is called on stop, suspend and delete; it is idempotent and
// touches nothing on a workspace with no ports.
func (s *Service) ReleaseWorkspacePorts(w *model.Workspace) {
	if w == nil {
		return
	}
	ports, err := s.store.ListPorts(w.ID)
	if err != nil {
		return
	}
	for _, p := range ports {
		if s.net != nil {
			s.net.StopForwarder(p.ID)
		}
		_ = s.store.DeletePort(w.ID, p.ID)
	}
}

// EnsureWorkspaceNetwork creates (or returns) the workspace's isolated network
// allocation. When no network is attached it returns a nil allocation and nil
// error so runs still work in a fresh empty namespace.
func (s *Service) EnsureWorkspaceNetwork(wsID string) (*network.Allocation, error) {
	if s.net == nil || !s.net.Available() {
		return nil, nil
	}
	return s.net.EnsureWorkspace(wsID)
}

// ApplyNetwork decorates an executor request with the workspace's isolated
// network: the private namespace to join and, when egress is enabled for the
// workspace, the resolver file and proxy environment. It returns the request
// unchanged when no network is attached (nsjail then creates a fresh empty
// namespace). A failure to set up the workspace network is returned so the
// caller can refuse the run rather than fall back to sharing the host.
func (s *Service) ApplyNetwork(w *model.Workspace, req executor.Request) (executor.Request, error) {
	if s.net == nil || !s.net.Available() {
		// No usable network layer: leave NetnsPath empty so nsjail creates a
		// fresh, empty network namespace. This is never a host-network share,
		// so isolation is preserved even without the veth machinery.
		return req, nil
	}
	alloc, err := s.net.EnsureWorkspace(w.ID)
	if err != nil {
		return req, model.New(503, model.CodeUnavailable, "workspace network unavailable: "+err.Error())
	}
	req.NetnsPath = s.net.NetnsPathFor(w.ID)
	if w.Limits.AllowNet {
		// Egress is delivered by the userspace proxy; point the workspace at it.
		req.EgressEnv = envSlice(s.net.EgressEnv(alloc))
		if rc, err := s.net.WriteResolverConf(alloc); err == nil {
			req.ResolverConf = rc
		}
	}
	return req, nil
}

// envSlice converts a map to sorted KEY=VALUE entries for the nsjail template.
func envSlice(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// ReleaseWorkspaceNetwork tears down the workspace's namespace and veth.
func (s *Service) ReleaseWorkspaceNetwork(wsID string) {
	if s.net != nil {
		s.net.ReleaseWorkspace(wsID)
	}
}

// observePortStatus dials the workspace port once and maps the result onto the
// honest status vocabulary.
func (s *Service) observePortStatus(wsID string, port int) string {
	if s.net == nil {
		return model.PortUnreachable
	}
	if s.net.ProbePort(wsID, port) {
		return model.PortPublished
	}
	return model.PortUnreachable
}

// refreshPortStatus updates and persists a port's status from a live probe.
func (s *Service) refreshPortStatus(wsID string, p *model.PublishedPort) {
	st := s.observePortStatus(wsID, p.Port)
	if st == p.Status {
		return
	}
	p.Status = st
	p.UpdatedAt = time.Now().UTC()
	_ = s.store.SavePort(p)
}

// usedHostPorts returns the set of host ports already allocated across ALL
// workspaces, so a host port is never handed out twice (the per-principal
// isolation rests on each principal getting a distinct host port).
func (s *Service) usedHostPorts(_ string) (map[int]bool, error) {
	ports, err := s.store.ListAllPorts()
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, p := range ports {
		used[p.HostPort] = true
	}
	return used, nil
}

// principalPortCount counts published ports across every workspace of a
// principal. excludeID, when non-empty, is not counted.
func (s *Service) principalPortCount(principalID, excludeID string) (int, error) {
	workspaces, err := s.store.ListAllWorkspaces()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, w := range workspaces {
		if w.OwnerID != principalID {
			continue
		}
		ports, err := s.store.ListPorts(w.ID)
		if err != nil {
			continue
		}
		for _, p := range ports {
			if p.ID == excludeID {
				continue
			}
			count++
		}
	}
	return count, nil
}

// ReapWorkspacePorts reaps listeners and records that belong to no live
// workspace at startup. live is the set of workspace IDs currently present; a
// port whose workspace is missing is removed. It returns the number reaped.
func (s *Service) ReapWorkspacePorts() int {
	ports, err := s.store.ListAllPorts()
	if err != nil {
		return 0
	}
	reaped := 0
	for _, p := range ports {
		if _, err := s.store.GetWorkspace(p.WorkspaceID); err == nil {
			// Workspace still exists: keep the record; its listener is re-bound
			// lazily and duplicated listeners are prevented by the free-port
			// check on the next publish.
			continue
		}
		if s.net != nil {
			s.net.StopForwarder(p.ID)
		}
		_ = s.store.DeletePort(p.WorkspaceID, p.ID)
		reaped++
	}
	return reaped
}

// sortedPorts is a small helper for deterministic output.
func sortedPorts(ports []*model.PublishedPort) []*model.PublishedPort {
	sort.Slice(ports, func(i, j int) bool { return ports[i].CreatedAt.After(ports[j].CreatedAt) })
	return ports
}
