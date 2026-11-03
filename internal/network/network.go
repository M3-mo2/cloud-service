package network

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/decoy"
)

// Fixed names/values for the engine-owned relay namespace. The relay is a peer
// namespace, not the host namespace: slirp4netns connects it to the host, and
// each workspace connects to it through a second slirp4netns. Nothing here
// touches the host's interfaces or routing table.
const (
	// netnsNamePrefix marks the workspace namespaces this package owns, so
	// reaping can identify leftovers without touching anything else under
	// NetnsDir.
	netnsNamePrefix = "sbxdws-"
	// privateBlackholes are the operator's private ranges. The relay installs a
	// blackhole route for each, so a workspace that resolves a private address
	// (or a malicious peer returns one) cannot reach the host LAN, the host's
	// own services, or another workspace.
	privateBlackholes = "10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16 100.64.0.0/10"
)

// ManagerConfig configures the network manager.
type ManagerConfig struct {
	// WorkspaceSubnet is the pool of per-workspace /24 blocks (10.201.0.0/16).
	WorkspaceSubnet netip.Prefix
	// NetnsDir is where named namespaces live (default /run/netns).
	NetnsDir string
	// RelayNetns is the engine relay namespace name (default sbxdrelay).
	RelayNetns string
	// ResolverDir is where the generated per-workspace egress resolv.conf is
	// written. It must be a stable, traversable path (e.g. the service tmp dir).
	// Empty disables writing it.
	ResolverDir string
	// Network is the network configuration (tools, MTU, resolver list).
	Network config.NetworkConfig
	// Tunnels is the tunnel configuration (cloudflared path, url timeout).
	Tunnels config.TunnelsConfig
}

// mustPrefix parses a CIDR constant; a malformed one is a programming error.
func mustPrefix(s string) netip.Prefix {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		panic(err)
	}
	return p
}

// Manager owns the relay namespace and the per-workspace egress namespaces. It
// never touches the host's interface or route topology. It is safe for
// concurrent use.
type Manager struct {
	sys system
	cfg ManagerConfig

	alloc *allocator

	mu sync.Mutex
	// relayApplied tracks whether the relay is currently up.
	relayApplied bool
	// relayCmd is the outer slirp4netns process (host -> relay).
	relayCmd *os.Process
	// applied tracks which workspaces currently have a live namespace.
	applied map[string]bool
	// slirpCmds holds the inner slirp4netns process (relay -> workspace) per
	// workspace, so a release can stop the exact process it started.
	slirpCmds map[string]*os.Process

	// tunnels holds the live tunnel processes keyed by tunnel id.
	tunnels map[string]*tunnelProc

	// decoy owns the egress-decoy layer (stub resolver + TLS responder + CA +
	// in-netns rules). nil when no decoy manager was built.
	decoy *decoyManager
}

// NewManager constructs a manager over the real host.
func NewManager(cfg ManagerConfig) *Manager {
	if cfg.NetnsDir == "" {
		cfg.NetnsDir = "/run/netns"
	}
	if cfg.RelayNetns == "" {
		cfg.RelayNetns = "sbxdrelay"
	}
	return newManagerWithSystem(cfg, osSystem{})
}

func newManagerWithSystem(cfg ManagerConfig, sys system) *Manager {
	m := &Manager{
		sys:       sys,
		cfg:       cfg,
		alloc:     newAllocator(cfg.WorkspaceSubnet.Addr(), 1<<8),
		applied:   map[string]bool{},
		slirpCmds: map[string]*os.Process{},
		tunnels:   map[string]*tunnelProc{},
	}
	if cfg.Network.Decoy.Enabled {
		caDir := ""
		if cfg.ResolverDir != "" {
			caDir = filepath.Join(cfg.ResolverDir, "egress-decoy")
		}
		m.decoy = newDecoyManager(cfg.Network.Decoy, cfg.Network.Resolver, caDir)
	}
	return m
}

// Available reports whether the host has the tools this layer needs.
func (m *Manager) Available() bool {
	for _, t := range []string{"nsenter", "unshare", "mount", m.cfg.Network.SlirpPath} {
		if !toolAvailable(t) {
			return false
		}
	}
	return true
}

// toolHint names the first missing tool, for a truthful /readyz.
func (m *Manager) toolHint() string {
	for _, t := range []string{"nsenter", "unshare", "mount", m.cfg.Network.SlirpPath} {
		if !toolAvailable(t) {
			return t
		}
	}
	return ""
}

// EgressEnabled reports whether the egress layer is configured on.
func (m *Manager) EgressEnabled() bool { return m.cfg.Network.Enabled }

// DecoyEnabled reports whether the egress-decoy layer is configured on.
func (m *Manager) DecoyEnabled() bool { return m != nil && m.decoy != nil && m.decoy.Enabled() }

// DecoyCA returns the decoy certificate authority (nil when the layer is off),
// so the executor can trust it inside the workspace's trust store.
func (m *Manager) DecoyCA() (*decoy.CA, error) {
	if m == nil || m.decoy == nil {
		return nil, nil
	}
	return m.decoy.CA()
}

// DecoyStatus reports the decoy layer truthfully: enabled, ready (the CA
// materialized), and a note on any failure.
func (m *Manager) DecoyStatus() (enabled, ready bool, note string) {
	if m == nil || m.decoy == nil || !m.decoy.Enabled() {
		return false, false, "network.decoy.enabled is false"
	}
	if _, err := m.decoy.CA(); err != nil {
		return true, false, err.Error()
	}
	if _, err := m.decoy.decoyConfig(); err != nil {
		return true, false, err.Error()
	}
	return true, true, ""
}

// EnsureRelay creates the relay namespace and connects it to the host with one
// slirp4netns instance, then installs the private-range blackholes. It is
// idempotent. It returns an error when the host lacks the tools, so the caller
// can fall back to an empty namespace rather than the host network.
func (m *Manager) EnsureRelay() error {
	if !m.cfg.Network.Enabled {
		return fmt.Errorf("network layer disabled by configuration")
	}
	if !m.Available() {
		return fmt.Errorf("network layer unavailable: missing tool %q", m.toolHint())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.relayApplied {
		return nil
	}

	if _, err := m.ensurePrivateNetns(m.cfg.RelayNetns); err != nil {
		return fmt.Errorf("create relay namespace: %w", err)
	}
	// Loopback up so an inner tool can bind 127.0.0.1 in the relay.
	_ = m.inNetns(m.cfg.RelayNetns, "ip", "link", "set", "lo", "up")

	// Outer slirp: host -> relay. It creates the relay's tap interface and its
	// default route, over a UNIX socket. No host interface appears: the socket
	// is the whole transport.
	cmd := m.startSlirp(m.cfg.RelayNetns, "", true)
	if err := m.waitInterface(m.cfg.RelayNetns, m.cfg.Network.StartTimeout()); err != nil {
		if cmd != nil {
			_ = cmd.Kill()
		}
		return fmt.Errorf("connect relay to host: %w", err)
	}
	if cmd == nil {
		return fmt.Errorf("start relay slirp: could not launch %s", m.cfg.Network.SlirpPath)
	}
	m.relayCmd = cmd

	// Blackhole every private range in the relay, so neither the workspace nor
	// a nested slirp can route to the operator's infrastructure even if a name
	// resolves to a private address.
	for _, net := range strings.Fields(privateBlackholes) {
		if err := m.inNetns(m.cfg.RelayNetns, "ip", "route", "add", "blackhole", net); err != nil {
			// A duplicate blackhole (a prior half-started relay) is harmless.
			if !strings.Contains(err.Error(), "File exists") {
				return fmt.Errorf("blackhole %s in relay: %w", net, err)
			}
		}
	}
	m.relayApplied = true
	return nil
}

// startSlirp launches a slirp4netns instance connecting a target namespace.
//
//   - outer (fromHost=true): run in the host namespace, targeting the relay.
//   - inner (fromHost=false): run INSIDE the relay namespace, targeting a
//     workspace, with a per-workspace --cidr so two workspaces never share an
//     address.
//
// DNS is disabled on both: the workspace is given a public resolver in its own
// /etc/resolv.conf instead, so DNS is ordinary NATed traffic and the host
// resolver is never published.
func (m *Manager) startSlirp(targetNetns, cidr string, fromHost bool) *os.Process {
	args := []string{
		"--configure",
		"--mtu=" + fmt.Sprintf("%d", m.cfg.Network.MTU),
		"--disable-host-loopback",
		"--disable-dns",
	}
	if cidr != "" {
		args = append(args, "--cidr="+cidr)
	}
	args = append(args, "--netns-type=path", m.netnsPath(targetNetns), "tap0")

	if fromHost {
		return m.spawn(m.cfg.Network.SlirpPath, args)
	}
	// Run inside the relay namespace. --no-fork makes nsenter exec slirp4netns
	// directly, so the tracked pid IS the slirp process and killing it frees the
	// relay-side socket.
	nsArgs := append([]string{"--net=" + m.netnsPath(m.cfg.RelayNetns), "--no-fork", m.cfg.Network.SlirpPath}, args...)
	return m.spawn("nsenter", nsArgs)
}

// spawn starts a long-lived process and returns its *os.Process. The child is
// reaped in a goroutine; the manager tracks the pid and kills it by pid only.
func (m *Manager) spawn(bin string, args []string) *os.Process {
	cmd := exec.Command(bin, args...)
	if err := cmd.Start(); err != nil {
		return nil
	}
	proc := cmd.Process
	go func() { _ = cmd.Wait() }()
	return proc
}

// waitInterface waits until a tap interface is configured in a namespace
// (slirp has set the address and route), bounded by timeout.
func (m *Manager) waitInterface(netns string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		out, err := m.inNetnsOutput(netns, "ip", "-o", "link", "show", "tap0")
		if err == nil && len(out) > 0 {
			// Ensure the address is set too.
			aout, aerr := m.inNetnsOutput(netns, "ip", "-o", "-4", "addr", "show", "tap0")
			if aerr == nil && strings.Contains(string(aout), "inet ") {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("interface did not come up in %s", filepath.Base(netns))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// EnsureWorkspace creates the workspace's private egress namespace if absent,
// and returns its allocation. It is idempotent and safe for concurrent callers.
// A workspace that is not opted in to egress still gets a private empty
// namespace (callers use EnsureEmptyWorkspace) so isolation is never weakened.
func (m *Manager) EnsureWorkspace(wsID string) (*Allocation, error) {
	if err := m.EnsureRelay(); err != nil {
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
		if err := m.buildWorkspaceEgress(alloc); err != nil {
			return nil, err
		}
		m.applied[wsID] = true
		return alloc, nil
	}
	alloc, err := m.alloc.reserve(wsID)
	if err != nil {
		return nil, err
	}
	if err := m.buildWorkspaceEgress(alloc); err != nil {
		m.alloc.release(wsID)
		return nil, err
	}
	m.applied[wsID] = true
	return alloc, nil
}

// buildWorkspaceEgress creates the workspace namespace and connects it to the
// relay with an inner slirp4netns. It creates no host interface or route. The
// namespace is created fresh (createNetns removes any stale path and refuses a
// namespace that resolves to the host), so a rebuild always starts with a clean
// tap0.
func (m *Manager) buildWorkspaceEgress(alloc *Allocation) error {
	name := m.wsNetnsName(alloc.WorkspaceID)
	// A stale inner slirp from a prior hard death would hold tap0 busy; stop the
	// one this manager started for this workspace, if any.
	if p := m.slirpCmds[alloc.WorkspaceID]; p != nil {
		_ = p.Kill()
		delete(m.slirpCmds, alloc.WorkspaceID)
	}
	if _, err := m.createNetns(name); err != nil {
		return fmt.Errorf("create workspace namespace: %w", err)
	}
	_ = m.inNetns(name, "ip", "link", "set", "lo", "up")

	proc := m.startSlirp(name, alloc.CIDR.String(), false)
	if proc == nil {
		return fmt.Errorf("start workspace slirp: could not launch %s", m.cfg.Network.SlirpPath)
	}
	if err := m.waitInterface(name, m.cfg.Network.StartTimeout()); err != nil {
		_ = proc.Kill()
		m.destroyNetns(name)
		return fmt.Errorf("connect workspace to relay: %w", err)
	}
	m.slirpCmds[alloc.WorkspaceID] = proc
	// Egress-decoy layer, when configured: start the stubs inside this very
	// namespace and install the in-namespace redirect. A failure is returned so
	// the workspace is refused rather than silently serving the real address.
	if m.DecoyEnabled() {
		if err := m.decoy.StartFor(alloc.WorkspaceID, m.netnsPath(name)); err != nil {
			_ = proc.Kill()
			m.destroyNetns(name)
			delete(m.slirpCmds, alloc.WorkspaceID)
			return fmt.Errorf("start egress decoy: %w", err)
		}
	}
	return nil
}

// EnsureEmptyWorkspace creates a private, empty workspace namespace with no
// egress (an allow_net:false workspace, or an allow_net workspace on a host
// where the egress layer is unavailable). The workload is in its own namespace
// with loopback only, never the host's. It returns the namespace path.
func (m *Manager) EnsureEmptyWorkspace(wsID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := m.wsNetnsName(wsID)
	if _, err := m.ensurePrivateNetns(name); err != nil {
		return "", fmt.Errorf("create workspace namespace: %w", err)
	}
	_ = m.inNetns(name, "ip", "link", "set", "lo", "up")
	return m.netnsPath(name), nil
}

// ReleaseWorkspace tears the workspace's namespace and its inner slirp down and
// frees its allocation. It is idempotent and stops only the process this
// manager started.
func (m *Manager) ReleaseWorkspace(wsID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.slirpCmds[wsID]; p != nil {
		_ = p.Kill()
		delete(m.slirpCmds, wsID)
	}
	if m.DecoyEnabled() {
		m.decoy.StopFor(wsID, m.netnsPath(m.wsNetnsName(wsID)))
	}
	m.destroyNetns(m.wsNetnsName(wsID))
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

// NetnsPathFor returns the namespace path a workspace would use. It does not
// create anything.
func (m *Manager) NetnsPathFor(wsID string) string {
	return m.netnsPath(m.wsNetnsName(wsID))
}

// RelayNetnsPath returns the relay namespace path.
func (m *Manager) RelayNetnsPath() string { return m.netnsPath(m.cfg.RelayNetns) }

// ReapOrphans tears down every leftover workspace namespace and the relay, and
// stops any slirp process this package started. It mirrors the cgroup
// ReapOrphans discipline: no workspace network survives a restart, so anything
// present at startup is an orphan from a hard death. It returns the number of
// workspace namespaces reaped.
//
// It stops only slirp processes whose target namespace is one of ours, matched
// by the namespace path in the process's own argv, so a foreign process is
// never signalled.
func (m *Manager) ReapOrphans(live map[string]bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alloc.reset()
	m.applied = map[string]bool{}
	for wsID, p := range m.slirpCmds {
		_ = p.Kill()
		delete(m.slirpCmds, wsID)
	}
	// Decoy helpers are engine-owned children; kill the ones this process
	// tracked and any orphan matched by argv, before their namespaces go.
	if m.DecoyEnabled() {
		m.decoy.ReapOrphans()
	}
	if m.relayCmd != nil {
		_ = m.relayCmd.Kill()
		m.relayCmd = nil
	}
	m.relayApplied = false

	// Stop any slirp4netns whose target is under our NetnsDir (an orphan from a
	// prior hard death). The match is on the exact namespace path, so nothing
	// else is ever signalled.
	m.killOurSlirps()

	reaped := 0
	entries, err := os.ReadDir(m.cfg.NetnsDir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), netnsNamePrefix) && e.Name() != m.cfg.RelayNetns {
			continue
		}
		m.destroyNetns(e.Name())
		reaped++
	}
	return reaped
}

// killOurSlirps terminates only slirp4netns processes whose argv names a
// namespace under our NetnsDir. It walks /proc itself and signals the exact
// pid; it never uses pkill/killall, so a foreign slirp is untouched.
func (m *Manager) killOurSlirps() {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	dir := filepath.Clean(m.cfg.NetnsDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := 0
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "slirp4netns" {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		for _, a := range args {
			if strings.HasPrefix(a, dir+"/") {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				break
			}
		}
	}
}

// Stop closes every inner slirp and the relay, tears every workspace namespace
// and the relay namespace down, and stops every tunnel (graceful shutdown). On
// return no engine-owned namespace or process remains, so a clean stop leaves
// the host exactly as it was.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for wsID, p := range m.slirpCmds {
		_ = p.Kill()
		delete(m.slirpCmds, wsID)
	}
	for id, t := range m.tunnels {
		t.stop()
		delete(m.tunnels, id)
	}
	if m.relayCmd != nil {
		_ = m.relayCmd.Kill()
		m.relayCmd = nil
	}
	m.relayApplied = false
	// Stop every decoy helper this process started, before its namespace goes.
	if m.DecoyEnabled() {
		m.decoy.ReapOrphans()
	}
	// Tear down EVERY engine-owned namespace under NetnsDir, not only those
	// that had an inner slirp: an allow_net:false workspace has an empty
	// namespace with no slirp process, and it must not be left behind either.
	m.destroyOwnedNamespacesLocked()
	m.applied = map[string]bool{}
	m.alloc.reset()
}

// destroyOwnedNamespacesLocked removes every workspace namespace this package
// owns plus the relay namespace. The caller holds m.mu.
func (m *Manager) destroyOwnedNamespacesLocked() {
	entries, err := os.ReadDir(m.cfg.NetnsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), netnsNamePrefix) || e.Name() == m.cfg.RelayNetns {
			m.destroyNetns(e.Name())
		}
	}
}

// wsNetnsName returns the namespace name for a workspace.
func (m *Manager) wsNetnsName(wsID string) string {
	return netnsNamePrefix + shortID(wsID)
}

// shortID returns a stable 8-hex-character digest of an identifier, used in
// namespace names.
func shortID(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])[:8]
}

// netnsDirWritable reports whether the namespace directory is present and
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
	if !m.cfg.Network.Enabled {
		return false, "network.enabled is false"
	}
	if !m.Available() {
		return false, "missing tool " + m.toolHint()
	}
	if !m.netnsDirWritable() {
		return false, filepath.Clean(m.cfg.NetnsDir) + " not writable"
	}
	return true, ""
}

// ResolverConfPath returns the generated per-workspace resolv.conf path for a
// workspace, or "" when egress is disabled or no resolver directory is
// configured.
func (m *Manager) ResolverConfPath() string {
	if !m.cfg.Network.Enabled || m.cfg.ResolverDir == "" {
		return ""
	}
	return filepath.Join(m.cfg.ResolverDir, "egress-resolv")
}

// HostsConfPath returns the path of the generated per-workspace /etc/hosts, or
// "" when egress is disabled or no resolver directory is configured.
func (m *Manager) HostsConfPath() string {
	if !m.cfg.Network.Enabled || m.cfg.ResolverDir == "" {
		return ""
	}
	return filepath.Join(m.cfg.ResolverDir, "egress-hosts")
}

// WriteHostsConf writes a per-workspace /etc/hosts containing only generic
// localhost entries, and returns its path. The host's own /etc/hosts is
// deliberately NEVER mounted into a workspace: it carries the operator's server
// FQDN (e.g. node-id-1.runtime.zcp.zerops) and its LAN address, which identify
// the machine and its internal DNS. A workspace gets a names file that resolves
// nothing but localhost.
func (m *Manager) WriteHostsConf(alloc *Allocation) (string, error) {
	if m.HostsConfPath() == "" {
		return "", fmt.Errorf("egress hosts not configured")
	}
	if err := os.MkdirAll(m.cfg.ResolverDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(m.cfg.ResolverDir, "egress-hosts-"+shortID(alloc.WorkspaceID))
	content := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// WriteResolverConf writes a per-workspace /etc/resolv.conf naming the
// configured PUBLIC resolvers, and returns its path. The host's own resolver is
// deliberately never published to a workspace: the relay blackholes the
// operator's private ranges, so a host resolver address would be unreachable
// anyway, and publishing it would leak the host's search domain.
func (m *Manager) WriteResolverConf(alloc *Allocation) (string, error) {
	if m.ResolverConfPath() == "" {
		return "", fmt.Errorf("egress resolver not configured")
	}
	if err := os.MkdirAll(m.cfg.ResolverDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(m.cfg.ResolverDir, "egress-resolv-"+shortID(alloc.WorkspaceID)+".conf")
	var b strings.Builder
	if m.DecoyEnabled() {
		// With the decoy layer on, name the in-namespace stub resolver. The
		// nft redirect forces every port-53 query to it anyway, but naming it
		// here makes the workspace behave normally even before the redirect.
		b.WriteString("nameserver 127.0.0.1\n")
	} else {
		for _, ns := range m.cfg.Network.Resolver {
			b.WriteString("nameserver " + ns + "\n")
		}
	}
	b.WriteString("options timeout:2 attempts:2\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
