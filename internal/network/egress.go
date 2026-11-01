package network

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// EgressConfig configures the userspace egress proxy. No iptables/nft is
// available on the target host, so egress is delivered entirely in userspace:
// a workspace app is pointed at an HTTP proxy (through HTTP_PROXY/HTTPS_PROXY)
// and a DNS resolver, both of which run in the hub namespace but dial out from
// the host namespace. A workspace therefore never shares the host network
// namespace and still gains a controlled egress path.
type EgressConfig struct {
	// ProxyPort is the HTTP proxy port the hub listens on (in the hub ns).
	ProxyPort int
	// DNSPort is the DNS forwarder port (in the hub ns). It is 53 by default,
	// but the workspace's generated resolv.conf always names the exact port.
	DNSPort int
	// UpstreamDNS is the resolver the DNS forwarder queries from the host ns.
	// Empty means "read /etc/resolv.conf, else 1.1.1.1".
	UpstreamDNS []string
}

// egressProxy is the egress mechanism: an HTTP proxy and a DNS forwarder, both
// bound inside the hub namespace so every workspace reaches them at its own
// hub peer address.
type egressProxy struct {
	m   *Manager
	cfg EgressConfig

	httpLn net.Listener
	dnsLn  net.PacketConn

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newEgressProxy(m *Manager) *egressProxy {
	cfg := EgressConfig{}
	if m.cfg.Egress != nil {
		cfg = *m.cfg.Egress
	}
	if cfg.ProxyPort == 0 {
		cfg.ProxyPort = 3128
	}
	if cfg.DNSPort == 0 {
		cfg.DNSPort = 53
	}
	return &egressProxy{m: m, cfg: cfg}
}

// ProxyPort returns the HTTP proxy port.
func (e *egressProxy) ProxyPort() int { return e.cfg.ProxyPort }

// DNSPort returns the DNS forwarder port.
func (e *egressProxy) DNSPort() int { return e.cfg.DNSPort }

// start binds the proxy listeners inside the hub namespace.
func (e *egressProxy) start() error {
	ln, err := listenInNetns(e.m.netnsPath(hubNetnsName), "tcp", fmt.Sprintf("0.0.0.0:%d", e.cfg.ProxyPort))
	if err != nil {
		return fmt.Errorf("bind egress proxy: %w", err)
	}
	e.httpLn = ln
	pc, err := listenPacketInNetns(e.m.netnsPath(hubNetnsName), "udp", fmt.Sprintf("0.0.0.0:%d", e.cfg.DNSPort))
	if err != nil {
		ln.Close()
		return fmt.Errorf("bind egress dns: %w", err)
	}
	e.dnsLn = pc
	e.wg.Add(2)
	go e.acceptHTTP()
	go e.serveDNS()
	return nil
}

func (e *egressProxy) stop() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.mu.Unlock()
	if e.httpLn != nil {
		_ = e.httpLn.Close()
	}
	if e.dnsLn != nil {
		_ = e.dnsLn.Close()
	}
}

func (e *egressProxy) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

func (e *egressProxy) acceptHTTP() {
	defer e.wg.Done()
	for {
		conn, err := e.httpLn.Accept()
		if err != nil {
			if e.isClosed() {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.handleHTTP(conn)
		}()
	}
}

// handleHTTP serves one proxy connection. It supports CONNECT (used for HTTPS
// and generic TCP tunnels) and absolute-URI HTTP requests. Both dial from the
// host namespace, so the workspace gains exactly the one connection it asked
// for and nothing more.
func (e *egressProxy) handleHTTP(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method == http.MethodConnect {
		e.handleConnect(conn, req.Host)
		return
	}
	e.handleForward(conn, br, req)
}

func (e *egressProxy) handleConnect(client net.Conn, hostport string) {
	if !strings.Contains(hostport, ":") {
		hostport += ":443"
	}
	up, err := net.DialTimeout("tcp", hostport, e.dialTimeout())
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer up.Close()
	_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	relayIdle(client, up, 5*time.Minute)
}

func (e *egressProxy) handleForward(client net.Conn, br *bufio.Reader, req *http.Request) {
	// A forward-proxy request carries an absolute URI: GET http://host/path.
	if !req.URL.IsAbs() {
		_, _ = io.WriteString(client, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
		return
	}
	hostport := req.URL.Host
	if !strings.Contains(hostport, ":") {
		hostport += ":80"
	}
	up, err := net.DialTimeout("tcp", hostport, e.dialTimeout())
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer up.Close()

	// Rewrite the request into origin-form and forward it plus any body.
	outReq := req.Clone(req.Context())
	outReq.RequestURI = ""
	outReq.URL = &url.URL{Path: req.URL.Path, RawQuery: req.URL.RawQuery}
	outReq.Header.Del("Proxy-Connection")
	outReq.Header.Del("Proxy-Authorization")
	outReq.Header.Set("Connection", "close")
	if err := outReq.Write(up); err != nil {
		return
	}
	// Relay the response back verbatim.
	relayIdle(client, up, 5*time.Minute)
}

func (e *egressProxy) dialTimeout() time.Duration {
	if e.m.cfg.Ports.ConnectTimeout() > 0 {
		return e.m.cfg.Ports.ConnectTimeout()
	}
	return time.Second
}

// serveDNS forwards UDP DNS queries to the upstream resolver from the host
// namespace. It is deliberately minimal: a single request/response per packet.
func (e *egressProxy) serveDNS() {
	defer e.wg.Done()
	upstreams := e.upstreams()
	if len(upstreams) == 0 {
		return
	}
	buf := make([]byte, 4096)
	for {
		n, addr, err := e.dnsLn.ReadFrom(buf)
		if err != nil {
			if e.isClosed() {
				return
			}
			continue
		}
		query := append([]byte(nil), buf[:n]...)
		go e.forwardDNS(query, addr, upstreams)
	}
}

func (e *egressProxy) upstreams() []string {
	if len(e.cfg.UpstreamDNS) > 0 {
		return e.cfg.UpstreamDNS
	}
	b, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		var out []string
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if ns, ok := strings.CutPrefix(line, "nameserver"); ok {
				ns = strings.TrimSpace(ns)
				if ns != "" {
					out = append(out, net.JoinHostPort(ns, "53"))
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"1.1.1.1:53"}
}

func (e *egressProxy) forwardDNS(query []byte, from net.Addr, upstreams []string) {
	for _, up := range upstreams {
		c, err := dialUDPHost(up, 2*time.Second)
		if err != nil {
			continue
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write(query); err != nil {
			c.Close()
			continue
		}
		resp := make([]byte, 4096)
		n, err := c.Read(resp)
		c.Close()
		if err != nil {
			continue
		}
		_, _ = e.dnsLn.WriteTo(resp[:n], from)
		return
	}
	// All upstreams failed: return SERVFAIL (QR=1, RCODE=2) echoing the query.
	if len(query) >= 12 {
		fail := append([]byte(nil), query...)
		fail[2] = fail[2] | 0x80 // QR = response
		fail[3] = fail[3]&0xf0 | 0x02
		_, _ = e.dnsLn.WriteTo(fail, from)
	}
}

// dialUDPHost opens a UDP socket in the host namespace explicitly. The DNS
// server's serve loop runs on the host namespace already, but making the host
// namespace explicit here keeps the "egress dials from the host, never from a
// workspace" invariant true even if the caller's thread is reused.
func dialUDPHost(addr string, timeout time.Duration) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		hostNS, err := os.Open("/proc/self/ns/net")
		if err != nil {
			done <- result{nil, err}
			return
		}
		defer hostNS.Close()
		// Explicitly (re)enter the host namespace.
		if err := unix.Setns(int(hostNS.Fd()), unix.CLONE_NEWNET); err != nil {
			done <- result{nil, err}
			return
		}
		conn, err := net.DialTimeout("udp", addr, timeout)
		done <- result{conn, err}
	}()
	r := <-done
	return r.conn, r.err
}

// listenInNetns creates a TCP listener inside a named namespace. The socket is
// created on a locked thread that enters the namespace and returns; the
// listener stays bound to that namespace.
func listenInNetns(netns, netw, addr string) (net.Listener, error) {
	type result struct {
		ln  net.Listener
		err error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		hostNS, err := os.Open("/proc/self/ns/net")
		if err != nil {
			done <- result{nil, err}
			return
		}
		defer hostNS.Close()
		nsFile, err := os.Open(netns)
		if err != nil {
			done <- result{nil, err}
			return
		}
		defer nsFile.Close()
		if err := unix.Setns(int(nsFile.Fd()), unix.CLONE_NEWNET); err != nil {
			done <- result{nil, err}
			return
		}
		defer unix.Setns(int(hostNS.Fd()), unix.CLONE_NEWNET)
		ln, err := net.Listen(netw, addr)
		done <- result{ln, err}
	}()
	r := <-done
	return r.ln, r.err
}

// listenPacketInNetns is the UDP counterpart of listenInNetns.
func listenPacketInNetns(netns, network, addr string) (net.PacketConn, error) {
	type result struct {
		pc  net.PacketConn
		err error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		hostNS, err := os.Open("/proc/self/ns/net")
		if err != nil {
			done <- result{nil, err}
			return
		}
		defer hostNS.Close()
		nsFile, err := os.Open(netns)
		if err != nil {
			done <- result{nil, err}
			return
		}
		defer nsFile.Close()
		if err := unix.Setns(int(nsFile.Fd()), unix.CLONE_NEWNET); err != nil {
			done <- result{nil, err}
			return
		}
		defer unix.Setns(int(hostNS.Fd()), unix.CLONE_NEWNET)
		pc, err := net.ListenPacket(network, addr)
		done <- result{pc, err}
	}()
	r := <-done
	return r.pc, r.err
}

// EgressEnv returns the proxy environment a workspace with egress enabled must
// receive, given its allocation. It is hostname-free: the workspace reaches the
// hub at its own link peer address (HubAddr).
func (m *Manager) EgressEnv(alloc *Allocation) map[string]string {
	if m.egress == nil {
		return nil
	}
	proxy := fmt.Sprintf("http://%s:%d", alloc.HubAddr.String(), m.egress.ProxyPort())
	return map[string]string{
		"HTTP_PROXY":  proxy,
		"HTTPS_PROXY": proxy,
		"http_proxy":  proxy,
		"https_proxy": proxy,
		"NO_PROXY":    "localhost,127.0.0.1",
		"no_proxy":    "localhost,127.0.0.1",
	}
}

// EgressResolver returns the DNS nameserver address a workspace with egress
// enabled must be configured with (its hub link peer, where the DNS forwarder
// listens).
func (m *Manager) EgressResolver(alloc *Allocation) string {
	return alloc.HubAddr.String()
}

// ResolverConfPath returns the generated per-service resolv.conf path, or ""
// when egress is disabled or no resolver directory is configured.
func (m *Manager) ResolverConfPath() string {
	m.mu.Lock()
	on := m.egress != nil
	m.mu.Unlock()
	if !on || m.cfg.ResolverDir == "" {
		return ""
	}
	return filepath.Join(m.cfg.ResolverDir, "egress-resolv.conf")
}

// WriteResolverConf writes the shared egress resolv.conf once. All workspaces
// with egress reach the DNS forwarder at their own hub peer address, so a
// single generic file is insufficient (the address differs per workspace);
// callers therefore write a per-workspace file. This helper writes the file for
// one allocation.
func (m *Manager) WriteResolverConf(alloc *Allocation) (string, error) {
	if m.ResolverConfPath() == "" {
		return "", fmt.Errorf("egress resolver not configured")
	}
	if err := os.MkdirAll(m.cfg.ResolverDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(m.cfg.ResolverDir, "egress-resolv-"+shortID(alloc.WorkspaceID)+".conf")
	body := "nameserver " + alloc.HubAddr.String() + "\noptions timeout:2 attempts:2\noptions ndots:1\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// dnsQueryID is a tiny helper used by tests to build a DNS query header.
func dnsQueryID(id uint16) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:2], id)
	return b
}
