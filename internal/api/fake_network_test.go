package api

import (
	"fmt"
	"net/netip"
	"sync"

	"sandboxd/internal/network"
)

// fakeNetwork is an in-memory service.Network for API/service tests. It models
// the isolated-network bookkeeping and the published-port forwarder without
// touching the host, so the HTTP contract (status codes, ownership, caps,
// honest status) can be asserted deterministically.
type fakeNetwork struct {
	mu sync.Mutex

	available bool
	ready     bool
	reason    string

	// enabled controls PortsReady (ports.enabled).
	enabled bool

	// reachable maps "wsID:port" -> true when the workspace "is listening".
	reachable map[string]bool

	// allocs maps wsID -> guest address.
	allocs map[string]string
	// nextIdx increments per workspace.
	nextIdx int

	// listeners maps portID -> bound host port.
	listeners map[string]int
	// usedHostPorts tracks every handed-out host port.
	usedHostPorts map[int]bool
	// hostRange is the allocation range.
	hostLo, hostHi int
	// startReachabilityError, when set, is used by EnsureWorkspace? (unused)
}

func newFakeNetwork() *fakeNetwork {
	return &fakeNetwork{
		available:     true,
		ready:         true,
		reachable:     map[string]bool{},
		allocs:        map[string]string{},
		listeners:     map[string]int{},
		usedHostPorts: map[int]bool{},
		hostLo:        32000,
		hostHi:        32999,
	}
}

func (f *fakeNetwork) Available() bool { return f.available }

func (f *fakeNetwork) EnsureHub() error { return nil }

func (f *fakeNetwork) EnsureWorkspace(wsID string) (*network.Allocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.allocs[wsID]; !ok {
		f.nextIdx++
		f.allocs[wsID] = fmt.Sprintf("10.200.%d.2", f.nextIdx)
	}
	return &network.Allocation{WorkspaceID: wsID, GuestAddr: fakeAddr(f.allocs[wsID])}, nil
}

func (f *fakeNetwork) ReleaseWorkspace(wsID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.allocs, wsID)
}

func (f *fakeNetwork) HasWorkspace(wsID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.allocs[wsID]
	return ok
}

func (f *fakeNetwork) AllocationAddr(wsID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.allocs[wsID]
	return a, ok
}

func (f *fakeNetwork) ProbePort(wsID string, port int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reachable[fmt.Sprintf("%s:%d", wsID, port)]
}

func (f *fakeNetwork) PickHostPort(used map[int]bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for p := f.hostLo; p <= f.hostHi; p++ {
		if used[p] || f.usedHostPorts[p] {
			continue
		}
		f.usedHostPorts[p] = true
		return p, nil
	}
	return 0, fmt.Errorf("no free host port")
}

func (f *fakeNetwork) StartForwarder(portID string, hostPort int, targetAddr string, targetPort int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listeners[portID] = hostPort
	return nil
}

func (f *fakeNetwork) StopForwarder(portID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.listeners, portID)
}

func (f *fakeNetwork) PortsReady() (bool, string) {
	if !f.enabled {
		return false, "ports.enabled is false"
	}
	return f.ready, f.reason
}

func (f *fakeNetwork) Ready() (bool, string) { return f.ready, f.reason }

func (f *fakeNetwork) EgressEnabled() bool { return false }

func (f *fakeNetwork) NetnsPathFor(wsID string) string {
	return "/run/netns/sbxdws-fake"
}

func (f *fakeNetwork) ResolverConfPath() string { return "" }

func (f *fakeNetwork) WriteResolverConf(alloc *network.Allocation) (string, error) {
	return "", fmt.Errorf("no egress in fake")
}

func (f *fakeNetwork) EgressEnv(alloc *network.Allocation) map[string]string { return nil }

func (f *fakeNetwork) ReapOrphans(live map[string]bool) int { return 0 }

func (f *fakeNetwork) Stop() {}

// listenerCount returns how many forwarders are live.
func (f *fakeNetwork) listenerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.listeners)
}

// setReachable marks a workspace port as listening.
func (f *fakeNetwork) setReachable(wsID string, port int, up bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reachable[fmt.Sprintf("%s:%d", wsID, port)] = up
}

func fakeAddr(s string) netip.Addr {
	a, _ := netip.ParseAddr(s)
	return a
}
