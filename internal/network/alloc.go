package network

import (
	"fmt"
	"net/netip"
	"sync"
)

// Allocation is the private address plan assigned to one allow_net workspace.
//
// The workspace's network namespace contains a single tap interface configured
// by slirp4netns from CIDR. The workspace takes .100 and its on-link gateway is
// .2; the CIDR is unique per workspace so two workspaces can never share an
// address even though each is in its own namespace.
type Allocation struct {
	WorkspaceID string
	// Index is the 1-based /24 block index inside the workspace pool.
	Index uint32
	// CIDR is the workspace's own /24 (its tap network).
	CIDR netip.Prefix
	// GuestAddr is the workspace's tap address (.100 of CIDR).
	GuestAddr netip.Addr
	// GatewayAddr is slirp's on-link gateway inside the workspace (.2 of CIDR).
	GatewayAddr netip.Addr
}

// allocator hands each allow_net workspace one /24 from the pool. It is an
// in-memory index; the durable record is the namespace under NetnsDir, and
// startup reaping rebuilds the index from a clean slate.
type allocator struct {
	mu      sync.Mutex
	byWS    map[string]*Allocation
	byIndex map[uint32]string
	base    netip.Addr
	max     uint32
	next    uint32
}

func newAllocator(base netip.Addr, max uint32) *allocator {
	return &allocator{
		byWS:    map[string]*Allocation{},
		byIndex: map[uint32]string{},
		base:    base,
		max:     max,
	}
}

// reserve allocates the next free /24 for wsID. Idempotent.
func (a *allocator) reserve(wsID string) (*Allocation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, ok := a.byWS[wsID]; ok {
		return existing, nil
	}
	for tries := uint32(0); tries < a.max; tries++ {
		idx := a.next
		a.next++
		if a.next >= a.max {
			a.next = 0
		}
		if idx >= a.max {
			continue
		}
		if _, taken := a.byIndex[idx]; taken {
			continue
		}
		cidr := a.cidrFor(idx)
		// .100 is the workspace address and .2 the gateway.
		ips := addrFor(cidr)
		alloc := &Allocation{
			WorkspaceID: wsID,
			Index:       idx + 1,
			CIDR:        cidr,
			GuestAddr:   ips.guest,
			GatewayAddr: ips.gateway,
		}
		a.byWS[wsID] = alloc
		a.byIndex[idx] = wsID
		return alloc, nil
	}
	return nil, fmt.Errorf("workspace network pool exhausted (%d subnets)", a.max)
}

// cidrFor returns the idx-th /24 block inside the base /16.
func (a *allocator) cidrFor(idx uint32) netip.Prefix {
	b := a.base.As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += idx << 8 // each block is a /24
	addr := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	return netip.PrefixFrom(addr, 24)
}

// release frees the /24 held by wsID, if any.
func (a *allocator) release(wsID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	alloc, ok := a.byWS[wsID]
	if !ok {
		return
	}
	delete(a.byWS, wsID)
	delete(a.byIndex, alloc.Index-1)
}

// get returns the allocation for wsID.
func (a *allocator) get(wsID string) (*Allocation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	alloc, ok := a.byWS[wsID]
	return alloc, ok
}

// reset clears the allocator (used at startup before reaping/rebuilding).
func (a *allocator) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.byWS = map[string]*Allocation{}
	a.byIndex = map[uint32]string{}
	a.next = 0
}

type addrPair struct {
	guest   netip.Addr
	gateway netip.Addr
}

// addrFor returns the .100 (workspace) and .2 (gateway) addresses of a /24.
func addrFor(p netip.Prefix) addrPair {
	base := p.Addr().As4()
	mk := func(last byte) netip.Addr {
		return netip.AddrFrom4([4]byte{base[0], base[1], base[2], last})
	}
	// slirp4netns places the gateway at .2 and the configured host address at
	// .100 (the same convention it uses for its default 10.0.2.0/24).
	return addrPair{guest: mk(100), gateway: mk(2)}
}
