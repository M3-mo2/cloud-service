package decoy

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
)

// MarkValue is the SO_MARK value the stub resolver stamps on its own upstream
// queries so the in-namespace nft rules (which redirect every other port-53
// query to the stub) let them through to the real resolver. It must match the
// nft rule's `meta mark 0x24 accept`.
const MarkValue = 0x24

// Resolver is the in-namespace stub DNS resolver. It answers the DNS-based
// "what is my IP" queries with the decoy profile and forwards every other name
// to the configured upstream resolvers, so ordinary resolution (pypi, npm,
// github) is untouched.
type Resolver struct {
	cfg      Config
	Profile  Profile
	Upstream []string
	Port     int
}

// NewResolver builds a resolver from a config and workspace id.
func NewResolver(cfg Config, workspaceID string, upstream []string) *Resolver {
	return &Resolver{
		cfg:      cfg,
		Profile:  cfg.Derive(workspaceID),
		Upstream: upstream,
		Port:     cfg.ResolverPort,
	}
}

// Serve runs the resolver until ctx is cancelled. It binds both UDP and TCP on
// 127.0.0.1:<port>.
func (r *Resolver) Serve(ctx context.Context) error {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: r.Port}
	uc, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("decoy resolver: udp bind: %w", err)
	}
	tc, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: r.Port})
	if err != nil {
		uc.Close()
		return fmt.Errorf("decoy resolver: tcp bind: %w", err)
	}
	defer uc.Close()
	defer tc.Close()
	return r.serveBound(ctx, uc, tc)
}

// serveBound runs the resolver over already-bound listeners so the helper can
// bind synchronously and signal readiness before any traffic arrives.
func (r *Resolver) serveBound(ctx context.Context, uc *net.UDPConn, tc *net.TCPListener) error {
	done := make(chan struct{}, 2)
	go func() { r.serveUDP(ctx, uc); done <- struct{}{} }()
	go func() { r.serveTCP(ctx, tc); done <- struct{}{} }()
	select {
	case <-ctx.Done():
		return nil
	case <-done:
		return nil
	}
}

func (r *Resolver) serveUDP(ctx context.Context, c *net.UDPConn) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		c.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		q := append([]byte(nil), buf[:n]...)
		resp := r.answer(q)
		c.WriteToUDP(resp, from)
	}
}

func (r *Resolver) serveTCP(ctx context.Context, l *net.TCPListener) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		l.SetDeadline(time.Now().Add(time.Second))
		conn, err := l.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		go r.handleTCPConn(conn)
	}
}

func (r *Resolver) handleTCPConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	var lenBuf [2]byte
	if _, err := readFull(conn, lenBuf[:]); err != nil {
		return
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n <= 0 || n > 4096 {
		return
	}
	msg := make([]byte, n)
	if _, err := readFull(conn, msg); err != nil {
		return
	}
	resp := r.answer(msg)
	out := make([]byte, 2+len(resp))
	binary.BigEndian.PutUint16(out, uint16(len(resp)))
	copy(out[2:], resp)
	conn.Write(out)
}

func readFull(c net.Conn, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := c.Read(b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// answer produces a DNS response for a raw query. An intercepted echo name (or
// a covered hostname, so the responder gets the traffic) is answered locally;
// everything else is forwarded upstream.
func (r *Resolver) answer(query []byte) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return r.formErr(query, dnsmessage.RCodeFormatError)
	}
	q, err := p.Question()
	if err != nil {
		return r.formErr(query, dnsmessage.RCodeFormatError)
	}
	name := strings.TrimSuffix(q.Name.String(), ".")
	lower := strings.ToLower(name)

	// DNS echo names: answer with the decoy directly.
	if rec, ok := IsDNSEcho(lower); ok {
		if rec == "txt" {
			return r.txtResponse(hdr, q, r.Profile)
		}
		return r.aResponse(hdr, q, r.Profile.IP.String())
	}

	// A covered HTTP-echo or DoH hostname: resolve it to the decoy address so
	// the TLS/HTTP responder receives the connection.
	if q.Type == dnsmessage.TypeA || q.Type == dnsmessage.TypeAAAA {
		if r.cfg.MatchesHost(lower) {
			if q.Type == dnsmessage.TypeAAAA {
				// No decoy IPv6: answer NOERROR with no records so a v6-preferring
				// client falls back to the A record rather than leaking.
				return r.emptyResponse(hdr, q)
			}
			return r.aResponse(hdr, q, r.Profile.IP.String())
		}
	}

	// PTR for the decoy address itself: answer the decoy PTR for consistency.
	if q.Type == dnsmessage.TypePTR {
		if rev, ok := ptrFor(r.Profile.IP.String()); ok && strings.EqualFold(name, rev) {
			return r.ptrResponse(hdr, q, r.Profile.PTR)
		}
	}

	return r.forward(query)
}

// aResponse builds an A-record response.
func (r *Resolver) aResponse(hdr dnsmessage.Header, q dnsmessage.Question, ip string) []byte {
	parsed := net.ParseIP(ip).To4()
	if parsed == nil {
		return r.emptyResponse(hdr, q)
	}
	var a [4]byte
	copy(a[:], parsed)
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: hdr.ID, Response: true, OpCode: hdr.OpCode,
		Authoritative: true, RecursionDesired: hdr.RecursionDesired, RecursionAvailable: true,
	})
	b.EnableCompression()
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 30}, dnsmessage.AResource{A: a})
	out, _ := b.Finish()
	return out
}

// txtResponse builds a TXT-record response (for o-o.myaddr.l.google.com and
// whoami.cloudflare) carrying the decoy address as the text.
func (r *Resolver) txtResponse(hdr dnsmessage.Header, q dnsmessage.Question, prof Profile) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: hdr.ID, Response: true, OpCode: hdr.OpCode,
		Authoritative: true, RecursionDesired: hdr.RecursionDesired, RecursionAvailable: true,
	})
	b.EnableCompression()
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	_ = b.TXTResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeTXT, Class: q.Class, TTL: 30}, dnsmessage.TXTResource{TXT: []string{prof.IP.String()}})
	out, _ := b.Finish()
	return out
}

// ptrResponse builds a PTR-record response.
func (r *Resolver) ptrResponse(hdr dnsmessage.Header, q dnsmessage.Question, ptr string) []byte {
	name, err := dnsmessage.NewName(strings.TrimSuffix(ptr, ".") + ".")
	if err != nil {
		return r.emptyResponse(hdr, q)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: hdr.ID, Response: true, OpCode: hdr.OpCode,
		Authoritative: true, RecursionDesired: hdr.RecursionDesired, RecursionAvailable: true,
	})
	b.EnableCompression()
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	_ = b.PTRResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET, TTL: 30}, dnsmessage.PTRResource{PTR: name})
	out, _ := b.Finish()
	return out
}

// emptyResponse builds a NOERROR/empty answer.
func (r *Resolver) emptyResponse(hdr dnsmessage.Header, q dnsmessage.Question) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: hdr.ID, Response: true, OpCode: hdr.OpCode,
		Authoritative: true, RecursionDesired: hdr.RecursionDesired, RecursionAvailable: true,
	})
	b.EnableCompression()
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	out, _ := b.Finish()
	return out
}

// formErr builds a minimal error response.
func (r *Resolver) formErr(query []byte, code dnsmessage.RCode) []byte {
	var id uint16
	if len(query) >= 2 {
		id = binary.BigEndian.Uint16(query[:2])
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true, RCode: code, RecursionAvailable: true})
	out, _ := b.Finish()
	return out
}

// forward relays a query to an upstream resolver over a socket marked with
// SO_MARK, so the nft redirect (which catches port 53) lets it out. It tries
// each upstream in turn.
func (r *Resolver) forward(query []byte) []byte {
	for _, up := range r.Upstream {
		if resp := r.forwardOne(query, up); resp != nil {
			return resp
		}
	}
	// No upstream answered: SERVFAIL (never the truth, never an address).
	return r.formErr(query, dnsmessage.RCodeServerFailure)
}

func (r *Resolver) forwardOne(query []byte, upstream string) []byte {
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(upstream, "53"))
	if err != nil {
		return nil
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil
	}
	defer conn.Close()
	// Stamp SO_MARK so the outbound query escapes the port-53 redirect.
	if raw, err := conn.SyscallConn(); err == nil {
		raw.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, MarkValue)
		})
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(query); err != nil {
		return nil
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return nil
	}
	return append([]byte(nil), buf[:n]...)
}

// ptrFor returns the reverse-DNS name (in-addr.arpa) for an IPv4 address.
func ptrFor(ip string) (string, bool) {
	p := net.ParseIP(ip).To4()
	if p == nil {
		return "", false
	}
	return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", p[3], p[2], p[1], p[0]), true
}
