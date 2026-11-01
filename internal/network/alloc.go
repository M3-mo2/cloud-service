package network

import (
	"fmt"
	"net/netip"
	"sync"
)

// allocator assigns each workspace exactly one private /30 subnet inside the
// service's workspace subnet. The host end of the workspace's veth is the .1
// address and the workspace's own address is .2. It is an in-memory index; the
// netns and veth are the durable record, and startup reaping rebuilds the
// index from a clean slate.
type allocator struct {
	mu     sync.Mutex
	byWS   map[string]*Allocation
	byAddr map[netip.Addr]*Allocation
	base   uint32
	max    uint32
	next   uint32
}

func newAllocator(base netip.Addr, max uint32) *allocator {
	return &allocator{
		byWS:   map[string]*Allocation{},
		byAddr: map[netip.Addr]*Allocation{},
		base:   addrToUint32(base),
		max:    max,
		next:   0,
	}
}

// reserve allocates the next free /30 for wsID. It returns the allocation with
// both addresses set. It fails when the pool is exhausted.
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
		addr := a.addrFor(idx)
		if _, taken := a.byAddr[addr]; taken {
			continue
		}
		// The workspace address is the .2 of the /30.
		alloc := &Allocation{
			WorkspaceID: wsID,
			Index:       idx + 1,
			HubAddr:     addr,
			GuestAddr:   addr.Next(),
		}
		a.byWS[wsID] = alloc
		a.byAddr[alloc.HubAddr] = alloc
		a.byAddr[alloc.GuestAddr] = alloc
		return alloc, nil
	}
	return nil, fmt.Errorf("workspace network pool exhausted (%d subnets)", a.max)
}

// addrFor returns the .1 (hub) address of the idx-th /30 block. The block's
// network address is base + idx*4; the hub takes .1 and the workspace .2.
func (a *allocator) addrFor(idx uint32) netip.Addr {
	return uint32ToAddr(a.base + (idx << 2) + 1)
}

// release frees the subnet held by wsID, if any.
func (a *allocator) release(wsID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	alloc, ok := a.byWS[wsID]
	if !ok {
		return
	}
	delete(a.byWS, wsID)
	delete(a.byAddr, alloc.HubAddr)
	delete(a.byAddr, alloc.GuestAddr)
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
	a.byAddr = map[netip.Addr]*Allocation{}
	a.next = 0
}

func addrToUint32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func uint32ToAddr(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
