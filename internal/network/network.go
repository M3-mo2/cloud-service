package network

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"sandboxd/internal/config"
)

// Fixed names for the engine-owned hub namespace and the host<->hub link. The
// hub is a peer namespace, not the host namespace: the host sees only the
// host-side veth of this one link, and the workspace subnet is never present in
// the host routing table.
const (
	hubNetnsName = "sbxdhub"
	hubHostIface = "sbxdh0"
	hubIface     = "sbxdh1"
	// netnsNamePrefix marks the namespaces this package owns, so reaping can
	// identify leftovers without touching anything else under NetnsDir.
	netnsNamePrefix = "sbxdws-"
	// ifaceNamePrefix marks the per-workspace hub-side veths.
	ifaceNamePrefix = "sbxw"
	// guestIface is the interface name inside a workspace's own namespace. The
	// name is local to each namespace, so every workspace can use "eth0".
	guestIface = "eth0"
)

// Allocation is the private network assigned to one workspace.
type Allocation struct {
	WorkspaceID string
	// Index is the 1-based /30 block index inside the workspace subnet.
	Index uint32
	// HubAddr is the .1 address (the hub end of the workspace link).
	HubAddr netip.Addr
	// GuestAddr is the .2 address (the workspace's own address).
	GuestAddr netip.Addr
}

// ManagerConfig configures the network manager.
type ManagerConfig struct {
	// WorkspaceSubnet is the pool of per-workspace /30 blocks (10.200.0.0/16).
	WorkspaceSubnet netip.Prefix
	// HubSubnet is the /30 used for the host<->hub link; its .1 and .2 are the
	// host and hub addresses respectively.
	HubSubnet netip.Prefix
	// NetnsDir is where named namespaces live (default /run/netns).
	NetnsDir string
	// ResolverDir is where the generated egress resolv.conf is written. It must
	// be a stable, traversable path (e.g. the service tmp dir). Empty disables
	// writing it.
	ResolverDir string
	// Ports is the published-port configuration.
	Ports config.PortsConfig
	// Egress, when non-nil, enables the userspace egress proxy for workspaces
	// that opt in (allow_net). When nil, allow_net workspaces still get their
	// own isolated namespace but have no path to the outside: egress is simply
	// impossible, never a host-namespace share.
	Egress *EgressConfig
}

// DefaultManagerConfig returns the measured-safe defaults for this host.
func DefaultManagerConfig(ports config.PortsConfig) ManagerConfig {
	return ManagerConfig{
		WorkspaceSubnet: mustPrefix("10.200.0.0/16"),
		HubSubnet:       mustPrefix("10.199.0.0/30"),
		NetnsDir:        "/run/netns",
		Ports:           ports,
	}
}

func mustPrefix(s string) netip.Prefix {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		panic(err)
	}
	return p
}

// Manager owns the workspace netns/veth lifecycle, the egress proxy and the
// published-port forwarder. It is safe for concurrent use.
type Manager struct {
	sys system
	cfg ManagerConfig

	alloc *allocator

	mu sync.Mutex
	// applied tracks which workspaces currently have a live namespace/route.
	applied map[string]bool

	// egress is the userspace egress proxy (nil when disabled).
	egress *egressProxy
	// forwarders holds the live published-port listeners, keyed by port ID.
	forwarders map[string]*forwarder
}

// NewManager constructs a manager over the real host.
func NewManager(cfg ManagerConfig) *Manager {
	if cfg.NetnsDir == "" {
		cfg.NetnsDir = "/run/netns"
	}
	return newManagerWithSystem(cfg, osSystem{})
}

func newManagerWithSystem(cfg ManagerConfig, sys system) *Manager {
	return &Manager{
		sys:        sys,
		cfg:        cfg,
		alloc:      newAllocator(cfg.WorkspaceSubnet.Addr(), 1<<14),
		applied:    map[string]bool{},
		forwarders: map[string]*forwarder{},
	}
}

// PortsConfig returns the manager's published-port configuration.
func (m *Manager) PortsConfig() config.PortsConfig { return m.cfg.Ports }

// Available reports whether the host has the tools this layer needs.
func (m *Manager) Available() bool {
	for _, t := range []string{"ip", "nsenter", "unshare", "mount"} {
		if !toolAvailable(t) {
			return false
		}
	}
	return true
}

// toolHint names the first missing tool, for a truthful /readyz.
func (m *Manager) toolHint() string {
	for _, t := range []string{"ip", "nsenter", "unshare", "mount"} {
		if !toolAvailable(t) {
			return t
		}
	}
	return ""
}

// EnsureHub creates the hub namespace and the host<->hub link if absent, and
// installs the host route to the workspace subnet via the hub. It also starts
// the egress proxy when configured. It is idempotent.
func (m *Manager) EnsureHub() error {
	if !m.Available() {
		return fmt.Errorf("network layer unavailable: missing tool %q", m.toolHint())
	}
	if _, err := os.Stat(m.netnsPath(hubNetnsName)); err != nil {
		if _, err := m.createNetns(hubNetnsName); err != nil {
			return fmt.Errorf("create hub namespace: %w", err)
		}
		// .1 is the host end, .2 is the hub end.
		hh := m.cfg.HubSubnet.Addr().Next() // .1
		hIn := hh.Next()                    // .2
		// Host end of the link.
		if err := m.sys.run("ip", "link", "add", hubHostIface, "type", "veth", "peer", "name", hubIface); err != nil {
			return fmt.Errorf("create hub link: %w", err)
		}
		if err := m.sys.run("ip", "addr", "add", hh.String()+"/30", "dev", hubHostIface); err != nil {
			return fmt.Errorf("address host hub end: %w", err)
		}
		if err := m.sys.run("ip", "link", "set", hubHostIface, "up"); err != nil {
			return err
		}
		if err := m.sys.run("ip", "link", "set", hubIface, "netns", m.netnsPath(hubNetnsName)); err != nil {
			return fmt.Errorf("move hub end into namespace: %w", err)
		}
		if err := m.inNetns(hubNetnsName, "ip", "link", "set", "lo", "up"); err != nil {
			return err
		}
		if err := m.inNetns(hubNetnsName, "ip", "addr", "add", hIn.String()+"/30", "dev", hubIface); err != nil {
			return err
		}
		if err := m.inNetns(hubNetnsName, "ip", "link", "set", hubIface, "up"); err != nil {
			return err
		}
		// The host reaches every workspace address through the hub. The hub
		// forwards between the host link and each workspace link (the hub has
		// ip_forward enabled).
		if err := m.sys.run("ip", "route", "add", m.cfg.WorkspaceSubnet.String(), "via", hIn.String(), "dev", hubHostIface); err != nil {
			return fmt.Errorf("route workspace subnet via hub: %w", err)
		}
	}
	if err := m.inNetns(hubNetnsName, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		// Enabling forwarding in the hub makes the hub route between its own
		// interfaces. It is best-effort: a host without sysctl in the sandbox
		// still serves direct on-link traffic.
		_ = m.inNetns(hubNetnsName, "sh", "-c", "echo 1 > /proc/sys/net/ipv4/ip_forward")
	}
	if m.cfg.Egress != nil && m.egress == nil {
		eg := newEgressProxy(m)
		if err := eg.start(); err != nil {
			return fmt.Errorf("start egress proxy: %w", err)
		}
		m.egress = eg
	}
	return nil
}

// EnsureWorkspace creates the workspace's private namespace and veth link if
// absent, returning its allocation. It is idempotent and safe for concurrent
// callers.
func (m *Manager) EnsureWorkspace(wsID string) (*Allocation, error) {
	if err := m.EnsureHub(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureWorkspaceLocked(wsID)
}

func (m *Manager) ensureWorkspaceLocked(wsID string) (*Allocation, error) {
	if alloc, ok := m.alloc.get(wsID); ok {
		if m.applied[wsID] {
			return alloc, nil
		}
		// Reserved but not applied: rebuild the link (e.g. after a reap).
		if err := m.buildWorkspaceLink(alloc); err != nil {
			return nil, err
		}
		m.applied[wsID] = true
		return alloc, nil
	}
	alloc, err := m.alloc.reserve(wsID)
	if err != nil {
		return nil, err
	}
	if err := m.buildWorkspaceLink(alloc); err != nil {
		m.alloc.release(wsID)
		return nil, err
	}
	m.applied[wsID] = true
	return alloc, nil
}

// buildWorkspaceLink creates the netns, the veth pair and the addresses.
func (m *Manager) buildWorkspaceLink(alloc *Allocation) error {
	name := m.wsNetnsName(alloc.WorkspaceID)
	if _, err := os.Stat(m.netnsPath(name)); err != nil {
		if _, err := m.createNetns(name); err != nil {
			return fmt.Errorf("create workspace namespace: %w", err)
		}
	}
	hubSide := ifaceNamePrefix + shortID(alloc.WorkspaceID)
	// Defensive cleanup of any stale link from a prior hard death (the hub
	// namespace may still hold a half-built interface).
	_ = m.inNetns(hubNetnsName, "ip", "link", "del", hubSide)
	// The peer must not be created with the name "eth0" in the HOST namespace:
	// that name is already taken by the host's own uplink, so `ip link add ...
	// peer name eth0` fails with "File exists". Create the peer under a unique
	// temporary name and rename it to eth0 *inside* the workspace namespace,
	// where "eth0" is free.
	peerSide := "sbxwp" + shortID(alloc.WorkspaceID)
	if err := m.sys.run("ip", "link", "add", hubSide, "type", "veth", "peer", "name", peerSide); err != nil {
		return fmt.Errorf("create workspace veth: %w", err)
	}
	// Hub end.
	if err := m.sys.run("ip", "link", "set", hubSide, "netns", m.netnsPath(hubNetnsName)); err != nil {
		return fmt.Errorf("move hub veth end: %w", err)
	}
	// Workspace end.
	if err := m.sys.run("ip", "link", "set", peerSide, "netns", m.netnsPath(name)); err != nil {
		return fmt.Errorf("move workspace veth end: %w", err)
	}
	// A stale eth0 in the namespace (from a prior half-built link) would make
	// the rename fail; remove it first, then rename the fresh peer to eth0.
	_ = m.inNetns(name, "ip", "link", "del", guestIface)
	if err := m.inNetns(name, "ip", "link", "set", peerSide, "name", guestIface); err != nil {
		return fmt.Errorf("rename workspace interface: %w", err)
	}
	// Address and bring up the hub end.
	if err := m.inNetns(hubNetnsName, "ip", "addr", "add", alloc.HubAddr.String()+"/30", "dev", hubSide); err != nil {
		return err
	}
	if err := m.inNetns(hubNetnsName, "ip", "link", "set", hubSide, "up"); err != nil {
		return err
	}
	// Workspace end: loopback up (so 127.0.0.1 works) and its own address.
	if err := m.inNetns(name, "ip", "link", "set", "lo", "up"); err != nil {
		return err
	}
	if err := m.inNetns(name, "ip", "addr", "add", alloc.GuestAddr.String()+"/30", "dev", guestIface); err != nil {
		return err
	}
	if err := m.inNetns(name, "ip", "link", "set", guestIface, "up"); err != nil {
		return err
	}
	// The workspace reaches the hub only. There is deliberately NO default
	// route, so the host LAN and every host port are unreachable. When egress
	// is enabled the only addition is a route to the hub peer for the proxy,
	// which is already on-link; the proxy is reached by address.
	return nil
}

// ReleaseWorkspace tears the workspace's link and namespace down and frees its
// allocation. It is idempotent.
func (m *Manager) ReleaseWorkspace(wsID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseWorkspaceLocked(wsID)
}

func (m *Manager) releaseWorkspaceLocked(wsID string) {
	name := m.wsNetnsName(wsID)
	hubSide := ifaceNamePrefix + shortID(wsID)
	// Deleting the hub-side veth deletes its peer (the workspace end) too.
	if err := m.sys.run("ip", "link", "del", hubSide); err != nil {
		// The peer may already be gone; try deleting inside the namespace.
		_ = m.inNetns(name, "ip", "link", "del", guestIface)
	}
	m.destroyNetns(name)
	delete(m.applied, wsID)
	m.alloc.release(wsID)
}

// HasWorkspace reports whether a workspace currently has a live namespace.
func (m *Manager) HasWorkspace(wsID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applied[wsID]
}

// AllocationOf returns a workspace's allocation, if it has one.
func (m *Manager) AllocationOf(wsID string) (*Allocation, bool) {
	return m.alloc.get(wsID)
}

// ReapOrphans tears down every leftover workspace namespace and veth that is
// not in live. It mirrors the cgroup ReapOrphans discipline: no workspace
// network survives a restart, so anything present at startup is an orphan from
// a hard death. It returns the number of namespaces reaped.
func (m *Manager) ReapOrphans(live map[string]bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Drop every in-memory reservation: the durable state is the host.
	m.alloc.reset()
	m.applied = map[string]bool{}

	// Remove workspace-side veths by walking the hub namespace's links.
	out, err := m.sys.output("nsenter", "--net="+m.netnsPath(hubNetnsName), "ip", "-o", "link", "show")
	var orphans []string
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			// "N: <name>: <flags>" -- the name is the second colon field.
			fields := strings.Split(line, ":")
			if len(fields) < 2 {
				continue
			}
			name := strings.TrimSpace(fields[1])
			if strings.HasPrefix(name, "@") {
				name = strings.TrimPrefix(name, "@")
			}
			if strings.HasPrefix(name, ifaceNamePrefix) {
				orphans = append(orphans, name)
			}
		}
	}
	for _, iface := range orphans {
		_ = m.sys.run("ip", "link", "del", iface)
	}

	// Remove namespaces we own.
	entries, err := os.ReadDir(m.cfg.NetnsDir)
	if err != nil {
		return len(orphans)
	}
	reaped := len(orphans)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), netnsNamePrefix) {
			continue
		}
		wsID := m.wsIDFromNetns(e.Name())
		if live != nil && live[wsID] {
			// A workspace that should be live is rebuilt by EnsureWorkspace;
			// its stale namespace is still removed here so the rebuild starts
			// clean.
		}
		m.destroyNetns(e.Name())
		reaped++
	}
	return reaped
}

// Stop closes the egress proxy and every forwarder (graceful shutdown).
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.egress != nil {
		m.egress.stop()
		m.egress = nil
	}
	for id, f := range m.forwarders {
		f.stop()
		delete(m.forwarders, id)
	}
}

// wsNetnsName returns the namespace name for a workspace.
func (m *Manager) wsNetnsName(wsID string) string {
	return netnsNamePrefix + shortID(wsID)
}

// NetnsPathFor returns the namespace path a workspace would use. It does not
// create anything.
func (m *Manager) NetnsPathFor(wsID string) string {
	return m.netnsPath(m.wsNetnsName(wsID))
}

// HubNetnsPath returns the hub namespace path.
func (m *Manager) HubNetnsPath() string { return m.netnsPath(hubNetnsName) }

// EgressEnabled reports whether the userspace egress proxy is running.
func (m *Manager) EgressEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.egress != nil
}

// wsIDFromNetns reverses wsNetnsName as far as possible (the hash is one-way,
// so the manager keeps a lookup for live IDs and rebuilds it on demand).
func (m *Manager) wsIDFromNetns(netnsName string) string {
	return "" // namespace names are hashes; reaping is by prefix, not by name
}

// shortID returns a stable 8-hex-character digest of an identifier, used in
// interface and namespace names (Linux interface names are capped at 15 chars).
func shortID(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])[:8]
}

// hostPortRange parses "lo-hi" into inclusive bounds.
func parseHostRange(s string) (int, int, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("host_range %q must be \"lo-hi\"", s)
	}
	var lo, hi int
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &lo); err != nil {
		return 0, 0, fmt.Errorf("host_range low: %w", err)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &hi); err != nil {
		return 0, 0, fmt.Errorf("host_range high: %w", err)
	}
	if lo < 1 || hi > 65535 || lo > hi {
		return 0, 0, fmt.Errorf("host_range %q out of bounds", s)
	}
	return lo, hi, nil
}

// netnsDirExists reports whether the namespace directory is present and
// writable, for a truthful readiness report.
func (m *Manager) netnsDirWritable() bool {
	if err := os.MkdirAll(m.cfg.NetnsDir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(m.cfg.NetnsDir, ".probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}

// Ready reports whether the network layer can be used, plus a reason.
func (m *Manager) Ready() (bool, string) {
	if !m.Available() {
		return false, "missing tool " + m.toolHint()
	}
	if !m.netnsDirWritable() {
		return false, filepath.Clean(m.cfg.NetnsDir) + " not writable"
	}
	if _, _, err := parseHostRange(m.cfg.Ports.HostRange); err != nil {
		return false, err.Error()
	}
	return true, ""
}
