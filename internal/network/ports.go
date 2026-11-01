package network

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

// PickHostPort returns a free host port inside the configured host_range that
// is not in used. It checks the actual bind state (net.Listen) rather than
// trusting a bookkeeping list, so a port held by a foreign process or a stale
// listener is never handed out.
func (m *Manager) PickHostPort(used map[int]bool) (int, error) {
	lo, hi, err := parseHostRange(m.cfg.Ports.HostRange)
	if err != nil {
		return 0, err
	}
	for p := lo; p <= hi; p++ {
		if used[p] {
			continue
		}
		if m.hostPortFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free host port in %s", m.cfg.Ports.HostRange)
}

// hostPortFree reports whether a port can be bound on the configured bind
// address right now.
func (m *Manager) hostPortFree(port int) bool {
	ln, err := net.Listen("tcp", m.cfg.Ports.BindAddr+":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// StartForwarder binds the host port and starts relaying to the workspace's
// own private address. targetAddr is the workspace's allocation address and
// port is the in-workspace port; the caller must have derived both from the
// workspace record, never from a request.
func (m *Manager) StartForwarder(portID string, hostPort int, targetAddr string, targetPort int) error {
	if targetAddr == "" {
		return fmt.Errorf("empty forward target")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.forwarders[portID]; exists {
		return nil
	}
	target := targetAddr + ":" + strconv.Itoa(targetPort)
	f, err := m.startForwarder(portID, hostPort, m.cfg.Ports.BindAddr, target)
	if err != nil {
		return err
	}
	m.forwarders[portID] = f
	return nil
}

// StopForwarder stops and removes a forwarder, freeing the host port.
func (m *Manager) StopForwarder(portID string) {
	m.mu.Lock()
	f, ok := m.forwarders[portID]
	if ok {
		delete(m.forwarders, portID)
	}
	m.mu.Unlock()
	if ok {
		f.stop()
	}
}

// ProbePort reports whether a workspace is currently listening on port, by
// dialing its private address from the hub namespace. It is the honest signal
// behind a published port's status.
func (m *Manager) ProbePort(wsID string, port int) bool {
	alloc, ok := m.alloc.get(wsID)
	if !ok {
		return false
	}
	target := alloc.GuestAddr.String() + ":" + strconv.Itoa(port)
	return m.probeTarget(m.netnsPath(hubNetnsName), target, m.cfg.Ports.ConnectTimeout())
}

// AllocationAddr returns a workspace's private address string, if allocated.
func (m *Manager) AllocationAddr(wsID string) (string, bool) {
	alloc, ok := m.alloc.get(wsID)
	if !ok {
		return "", false
	}
	return alloc.GuestAddr.String(), true
}

// PortsReady reports whether the published-port feature can actually serve:
// the layer is available, the host range parses and the bind address can be
// bound. It never claims ready when it is not.
func (m *Manager) PortsReady() (bool, string) {
	if !m.cfg.Ports.Enabled {
		return false, "ports.enabled is false"
	}
	if _, _, err := parseHostRange(m.cfg.Ports.HostRange); err != nil {
		return false, err.Error()
	}
	lo, _, _ := parseHostRange(m.cfg.Ports.HostRange)
	probe, err := net.Listen("tcp", m.cfg.Ports.BindAddr+":"+strconv.Itoa(lo))
	if err != nil {
		return false, "cannot bind " + m.cfg.Ports.BindAddr
	}
	probe.Close()
	if !m.Available() {
		return false, "missing tool " + m.toolHint()
	}
	return true, ""
}

// waitDrain waits up to d for a forwarder's in-flight connections to finish.
func (f *forwarder) waitDrain(d time.Duration) {
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}
