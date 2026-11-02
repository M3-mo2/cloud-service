package api

import (
	"fmt"
	"net/netip"
	"sync"
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/network"
)

// fakeNetwork is an in-memory service.Network for API/service tests. It models
// the isolated-egress bookkeeping and the quick-tunnel lifecycle without
// touching the host, so the HTTP contract (status codes, ownership, caps,
// honest URL/status) can be asserted deterministically.
type fakeNetwork struct {
	mu sync.Mutex

	available bool
	ready     bool
	reason    string

	// egress controls EgressEnabled (network.enabled).
	egress bool
	// tunnelsEnabled / tunnelsReady control the tunnel capability.
	tunnelsEnabled bool
	tunnelsReady   bool
	tunnelsReason  string

	// url, when non-empty, is the URL StartTunnel returns; tunnelErr forces a
	// failure instead.
	url       string
	tunnelErr error

	// allocs maps wsID -> guest address.
	allocs map[string]string
	// nextIdx increments per workspace.
	nextIdx int

	// running holds the live tunnels keyed by tunnel id -> workspace id.
	running map[string]string
}

func newFakeNetwork() *fakeNetwork {
	return &fakeNetwork{
		available:      true,
		ready:          true,
		egress:         true,
		tunnelsEnabled: true,
		tunnelsReady:   true,
		url:            "https://fake-tunnel.trycloudflare.com",
		allocs:         map[string]string{},
		running:        map[string]string{},
	}
}

func (f *fakeNetwork) Available() bool       { return f.available }
func (f *fakeNetwork) Ready() (bool, string) { return f.ready, f.reason }
func (f *fakeNetwork) EgressEnabled() bool   { return f.egress }

func (f *fakeNetwork) EnsureWorkspace(wsID string) (*network.Allocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.allocs[wsID]; !ok {
		f.nextIdx++
		f.allocs[wsID] = fmt.Sprintf("10.201.%d.100", f.nextIdx)
	}
	return &network.Allocation{WorkspaceID: wsID, GuestAddr: fakeAddr(f.allocs[wsID])}, nil
}

func (f *fakeNetwork) EnsureEmptyWorkspace(wsID string) (string, error) {
	return "/run/netns/sbxdws-" + wsID, nil
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

func (f *fakeNetwork) NetnsPathFor(wsID string) string {
	return "/run/netns/sbxdws-" + wsID
}

func (f *fakeNetwork) WriteResolverConf(alloc *network.Allocation) (string, error) {
	return "/data/tmp/egress-resolv-" + alloc.WorkspaceID + ".conf", nil
}

func (f *fakeNetwork) WriteHostsConf(alloc *network.Allocation) (string, error) {
	return "/data/tmp/egress-hosts-" + alloc.WorkspaceID, nil
}

func (f *fakeNetwork) ReapOrphans(live map[string]bool) int { return 0 }

func (f *fakeNetwork) Stop() {}

func (f *fakeNetwork) StartTunnel(wsID, tunnelID string, port int, resolvConf string, timeout time.Duration) (*network.TunnelStart, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tunnelErr != nil {
		return nil, f.tunnelErr
	}
	f.running[tunnelID] = wsID
	return &network.TunnelStart{URL: f.url, PID: 4242}, nil
}

func (f *fakeNetwork) StopTunnel(tunnelID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.running[tunnelID]
	delete(f.running, tunnelID)
	return ok
}

func (f *fakeNetwork) StopWorkspaceTunnels(wsID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, owner := range f.running {
		if owner == wsID {
			delete(f.running, id)
		}
	}
}

func (f *fakeNetwork) TunnelRunning(tunnelID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.running[tunnelID]
	return ok
}

func (f *fakeNetwork) ReapTunnels(live map[string]bool) int { return 0 }

func (f *fakeNetwork) TunnelsEnabled() bool { return f.tunnelsEnabled }

func (f *fakeNetwork) TunnelsReady() (bool, string) {
	if !f.tunnelsEnabled {
		return false, "tunnels.enabled is false"
	}
	return f.tunnelsReady, f.tunnelsReason
}

func (f *fakeNetwork) TunnelsConfig() config.TunnelsConfig {
	return config.TunnelsConfig{Enabled: f.tunnelsEnabled, MaxPerWorkspace: 2, URLTimeoutSec: 1}
}

// tunnelCount returns how many tunnels are live.
func (f *fakeNetwork) tunnelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.running)
}

func fakeAddr(s string) netip.Addr {
	a, _ := netip.ParseAddr(s)
	return a
}
