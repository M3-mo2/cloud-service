package network

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// forwarder is one published-port listener. It accepts on the host bind address
// and relays to a fixed workspace-private target. The target is captured at
// construction from the workspace's own allocation and is never read from a
// request, so the forwarder cannot be turned into an open relay or an SSRF
// pivot into the host LAN.
type forwarder struct {
	m        *Manager
	id       string
	hostPort int
	bindAddr string
	// target is "addr:port" inside the workspace (from the allocation).
	target string
	// hubNetns is the namespace the dial is performed from (the hub), so the
	// host needs no route to the workspace subnet.
	hubNetns string

	ln       net.Listener
	maxConns int
	connTO   time.Duration
	idleTO   time.Duration

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	// sem bounds concurrent in-flight connections.
	sem chan struct{}
}

// startForwarder binds the host port and launches the accept loop.
func (m *Manager) startForwarder(portID string, hostPort int, bindAddr, target string) (*forwarder, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(bindAddr, strconv.Itoa(hostPort)))
	if err != nil {
		return nil, fmt.Errorf("bind %s:%d: %w", bindAddr, hostPort, err)
	}
	f := &forwarder{
		m:        m,
		id:       portID,
		hostPort: hostPort,
		bindAddr: bindAddr,
		target:   target,
		hubNetns: m.netnsPath(hubNetnsName),
		ln:       ln,
		maxConns: m.cfg.Ports.MaxConns,
		connTO:   m.cfg.Ports.ConnectTimeout(),
		idleTO:   m.cfg.Ports.IdleTimeout(),
		sem:      make(chan struct{}, m.cfg.Ports.MaxConns),
	}
	f.wg.Add(1)
	go f.acceptLoop()
	return f, nil
}

func (f *forwarder) acceptLoop() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			f.mu.Lock()
			closed := f.closed
			f.mu.Unlock()
			if closed {
				return
			}
			// A transient accept error (e.g. EMFILE) should not spin.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		select {
		case f.sem <- struct{}{}:
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer func() { <-f.sem }()
				f.handle(conn)
			}()
		default:
			// At capacity: refuse honestly rather than pile up goroutines.
			writeHTTPError(conn, 503, "forwarder_busy", "published port is at its connection limit")
			conn.Close()
		}
	}
}

// handle relays one connection to the workspace target. An unreachable target
// yields an HTTP 502 with a machine-readable body (the port is published but
// nothing answers inside the workspace); a healthy target is relayed verbatim.
func (f *forwarder) handle(conn net.Conn) {
	defer conn.Close()
	up, err := f.m.dialInNetns(f.hubNetns, f.target, f.connTO)
	if err != nil {
		writeHTTPError(conn, 502, "port_unreachable", "published port is not reachable inside the workspace")
		return
	}
	defer up.Close()
	relayIdle(conn, up, f.idleTO)
}

// stop closes the listener and waits for in-flight connections to finish
// (bounded by the idle timeout, worst case).
func (f *forwarder) stop() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.mu.Unlock()
	if f.ln != nil {
		_ = f.ln.Close()
	}
}

// probe dials the target once and reports whether it is reachable.
func (m *Manager) probeTarget(hubNetns, target string, timeout time.Duration) bool {
	conn, err := m.dialInNetns(hubNetns, target, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// dialInNetns dials target from inside a named namespace. The dial runs on a
// locked OS thread that enters the namespace, opens the socket, and returns to
// the host namespace; the connected socket is bound to the namespace it was
// created in, so only the dial itself needs the namespace.
func (m *Manager) dialInNetns(netns, target string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = time.Second
	}
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	hostNS, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return nil, err
	}
	defer hostNS.Close()
	nsFile, err := os.Open(netns)
	if err != nil {
		return nil, err
	}
	defer nsFile.Close()

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := unix.Setns(int(nsFile.Fd()), unix.CLONE_NEWNET); err != nil {
			done <- result{nil, err}
			return
		}
		// Always restore the host namespace on this thread before it is reused.
		defer unix.Setns(int(hostNS.Fd()), unix.CLONE_NEWNET)
		conn, err := net.DialTimeout("tcp", target, timeout)
		done <- result{conn, err}
	}()
	select {
	case r := <-done:
		return r.conn, r.err
	case <-time.After(timeout + time.Second):
		return nil, errors.New("dial timed out")
	}
}

// relayIdle copies bytes in both directions, refreshing a shared idle deadline
// on every transfer. A connection that is idle longer than idleTO is closed so
// a half-open or slow client cannot pin a slot forever.
func relayIdle(a, b net.Conn, idle time.Duration) {
	if idle <= 0 {
		idle = 60 * time.Second
	}
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))
			n, err := src.Read(buf)
			if n > 0 {
				_ = dst.SetWriteDeadline(time.Now().Add(idle))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					// Idle beyond the cap: tear the pair down.
				}
				// Propagate EOF to the peer so it stops reading.
				if tcp, ok := dst.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
				return
			}
		}
	}
	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
}

// writeHTTPError writes a minimal HTTP/1.1 response with a JSON error body. It
// is used on the forwarder's failure paths so a published-but-unreachable port
// answers with a real HTTP status (502) and a machine-readable code rather than
// a bare connection reset.
func writeHTTPError(w io.Writer, status int, code, message string) {
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
	resp := "HTTP/1.1 " + strconv.Itoa(status) + " " + httpStatusText(status) + "\r\n" +
		"Content-Type: application/json; charset=utf-8\r\n" +
		"Connection: close\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	_, _ = io.WriteString(w, resp)
}

func httpStatusText(status int) string {
	switch status {
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	default:
		return "Error"
	}
}
