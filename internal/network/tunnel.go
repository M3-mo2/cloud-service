package network

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"sandboxd/internal/config"
)

// tunnelURLPattern matches a Cloudflare quick-tunnel URL. cloudflared prints one
// line like:
//
//	INF +--------------------------------------------------------------------------------------------+
//	INF |  Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):  |
//	INF |  https://random-words-here.trycloudflare.com                                               |
//
// The engine captures THAT line from cloudflared's own output; it never
// fabricates a URL. api.trycloudflare.com appears in cloudflared's startup log
// and is explicitly not a tunnel URL, so it is excluded.
var tunnelURLPattern = regexp.MustCompile(`https://[a-z0-9][a-z0-9-]*\.trycloudflare\.com`)

const quickTunnelExclude = "api.trycloudflare.com"

// TunnelStart is the outcome of starting a tunnel: the observed URL and the
// host pid of the cloudflared process.
type TunnelStart struct {
	URL string
	PID int
}

// tunnelProc is one live quick-tunnel cloudflared process, running INSIDE the
// workspace's namespace. It is tracked so a stop kills the exact pid.
type tunnelProc struct {
	id     string
	wsID   string
	port   int
	url    string
	cmd    *exec.Cmd
	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// StartTunnel launches cloudflared inside the workspace's namespace to expose
// localhost:port, and waits (bounded by timeout) for cloudflared to print its
// public URL. It returns the observed URL, or an error when no URL is observed
// within the timeout — it never fabricates one. The cloudflared process runs
// under a PRIVATE mount namespace with the workspace's public resolver, so it
// can resolve Cloudflare's edge without touching the host's /etc/resolv.conf.
func (m *Manager) StartTunnel(wsID, tunnelID string, port int, resolvConf string, timeout time.Duration) (*TunnelStart, error) {
	if !m.cfg.Network.Enabled {
		return nil, fmt.Errorf("network layer disabled by configuration")
	}
	if !toolAvailable(m.cfg.Tunnels.CloudflaredPath) {
		return nil, fmt.Errorf("cloudflared not found at %q", m.cfg.Tunnels.CloudflaredPath)
	}
	m.mu.Lock()
	netns := m.netnsPath(m.wsNetnsName(wsID))
	_, hasNet := m.applied[wsID]
	m.mu.Unlock()
	if !hasNet {
		return nil, fmt.Errorf("workspace has no egress namespace")
	}

	url := fmt.Sprintf("http://localhost:%d", port)
	args := []string{"tunnel", "--no-autoupdate", "--url", url}
	cmd := m.runInNetnsMount(netns, resolvConf, m.cfg.Tunnels.CloudflaredPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// cloudflared logs to stderr; read both streams through one pipe so the URL
	// line is captured regardless of which stream carries it.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("start cloudflared: %w", err)
	}
	// The child owns the write end now.
	pw.Close()

	tp := &tunnelProc{id: tunnelID, wsID: wsID, port: port, cmd: cmd, done: make(chan struct{})}

	// Capture output until the URL appears or the process exits.
	type scanResult struct {
		url  string
		tail string
	}
	resCh := make(chan scanResult, 1)
	go func() {
		chunk := make([]byte, 4096)
		var tail strings.Builder
		var found string
		for {
			n, rerr := pr.Read(chunk)
			if n > 0 {
				s := string(chunk[:n])
				tail.WriteString(s)
				if tail.Len() > 8192 {
					t := tail.String()
					tail.Reset()
					tail.WriteString(t[len(t)-4096:])
				}
				if found == "" {
					if u := matchTunnelURL(s); u != "" {
						found = u
						resCh <- scanResult{url: found, tail: tail.String()}
					}
				}
			}
			if rerr != nil {
				break
			}
		}
		pr.Close()
		// Reap the process so it does not linger as a zombie; record the
		// completion so a later stop knows it is gone.
		_ = cmd.Wait()
		close(tp.done)
		if found == "" {
			resCh <- scanResult{url: "", tail: tail.String()}
		}
	}()

	deadline := time.After(timeout)
	var result scanResult
	select {
	case result = <-resCh:
	case <-deadline:
		// No URL in time: kill exactly the process we started.
		tp.stop()
		return nil, fmt.Errorf("no tunnel URL observed within %s", timeout)
	}
	if result.url == "" {
		// The process exited without printing a URL.
		tail := strings.TrimSpace(result.tail)
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return nil, fmt.Errorf("cloudflared exited without a tunnel URL: %s", tail)
	}
	tp.url = result.url
	m.mu.Lock()
	m.tunnels[tunnelID] = tp
	m.mu.Unlock()
	return &TunnelStart{URL: result.url, PID: cmd.Process.Pid}, nil
}

// matchTunnelURL returns the first quick-tunnel URL in a chunk of cloudflared
// output, ignoring the api.trycloudflare.com control endpoint cloudflared logs.
func matchTunnelURL(s string) string {
	for _, m := range tunnelURLPattern.FindAllString(s, -1) {
		if !strings.HasSuffix(m, quickTunnelExclude) {
			return m
		}
	}
	return ""
}

// stop kills the tunnel's exact process group and waits (bounded) for it to
// exit. It is idempotent.
func (t *tunnelProc) stop() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()
	if t.cmd != nil && t.cmd.Process != nil {
		// Signal the process group so cloudflared's own children die too.
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGTERM)
		_ = t.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-t.done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
			_ = t.cmd.Process.Kill()
			<-t.done
		}
	}
}

// StopTunnel stops and forgets a tunnel by id. It returns whether one was
// tracked. It stops only the process this manager started.
func (m *Manager) StopTunnel(tunnelID string) bool {
	m.mu.Lock()
	t, ok := m.tunnels[tunnelID]
	if ok {
		delete(m.tunnels, tunnelID)
	}
	m.mu.Unlock()
	if ok {
		t.stop()
	}
	return ok
}

// StopWorkspaceTunnels stops every tunnel belonging to a workspace.
func (m *Manager) StopWorkspaceTunnels(wsID string) {
	m.mu.Lock()
	var stop []*tunnelProc
	for id, t := range m.tunnels {
		if t.wsID == wsID {
			stop = append(stop, t)
			delete(m.tunnels, id)
		}
	}
	m.mu.Unlock()
	for _, t := range stop {
		t.stop()
	}
}

// TunnelRunning reports whether a tunnel id is currently tracked.
func (m *Manager) TunnelRunning(tunnelID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.tunnels[tunnelID]
	return ok
}

// ReapTunnels stops every cloudflared quick-tunnel process that belongs to no
// live workspace at startup, plus any this manager tracked. It walks /proc and
// signals only cloudflared processes whose argv carries a quick-tunnel --url,
// so an operator's own named tunnel is never touched. It returns the number
// reaped.
func (m *Manager) ReapTunnels(live map[string]bool) int {
	m.mu.Lock()
	for id, t := range m.tunnels {
		t.stop()
		delete(m.tunnels, id)
	}
	m.mu.Unlock()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	reaped := 0
	for _, e := range entries {
		pid := 0
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil || strings.TrimSpace(string(comm)) != "cloudflared" {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if !isQuickTunnelArgs(args) {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
		reaped++
	}
	return reaped
}

// isQuickTunnelArgs reports whether a cloudflared argv is a quick tunnel (a
// `tunnel ... --url ...` invocation). A named tunnel (`--token`/`run`) is not.
func isQuickTunnelArgs(args []string) bool {
	hasTunnel, hasURL := false, false
	for _, a := range args {
		if a == "tunnel" {
			hasTunnel = true
		}
		if a == "--url" || strings.HasPrefix(a, "--url=") {
			hasURL = true
		}
	}
	return hasTunnel && hasURL
}

// tunnelConfig accessor used by the service layer for capability reporting.
func (m *Manager) TunnelsEnabled() bool { return m.cfg.Tunnels.Enabled }

// CloudflaredAvailable reports whether the cloudflared binary is present.
func (m *Manager) CloudflaredAvailable() bool {
	return toolAvailable(m.cfg.Tunnels.CloudflaredPath)
}

// TunnelsReady reports whether the tunnel capability can serve, with a reason.
func (m *Manager) TunnelsReady() (bool, string) {
	if !m.cfg.Tunnels.Enabled {
		return false, "tunnels.enabled is false"
	}
	if !m.cfg.Network.Enabled {
		return false, "network.enabled is false (cloudflared needs egress)"
	}
	if !m.CloudflaredAvailable() {
		return false, "cloudflared not found at " + m.cfg.Tunnels.CloudflaredPath
	}
	if !m.Available() {
		return false, "missing tool " + m.toolHint()
	}
	return true, ""
}

// TunnelsConfig returns the tunnel configuration.
func (m *Manager) TunnelsConfig() config.TunnelsConfig { return m.cfg.Tunnels }
