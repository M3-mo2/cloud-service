package decoy

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestProfileDeriveDeterministicAndDistinct(t *testing.T) {
	cfg := Defaults()
	a1 := cfg.Derive("ws-aaa")
	a2 := cfg.Derive("ws-aaa")
	b := cfg.Derive("ws-bbb")
	if a1.IP != a2.IP {
		t.Fatalf("same workspace id gave different addresses: %s vs %s", a1.IP, a2.IP)
	}
	if a1.IP == b.IP {
		t.Fatalf("two workspace ids collided on one address: %s", a1.IP)
	}
	if !cfg.AddressRange.Contains(a1.IP) {
		t.Fatalf("derived address %s escapes the configured range %s", a1.IP, cfg.AddressRange)
	}
	if a1.Org == "" || a1.ASN == "" || a1.PTR == "" {
		t.Fatalf("profile incomplete: %+v", a1)
	}
}

func TestHostPatternsCoverKnownAndInvented(t *testing.T) {
	cfg := Defaults()
	covered := []string{
		"api.ipify.org", "api64.ipify.org", "icanhazip.com", "ipv4.icanhazip.com",
		"ifconfig.me", "ipinfo.io", "ip-api.com", "ident.me", "checkip.amazonaws.com",
		"wtfismyip.com", "api.myip.com", "www.cloudflare.com", "httpbin.org",
		"cloudflare-dns.com", "dns.google", "1.1.1.1",
		// INVENTED names matching the heuristic — the most valuable assertion.
		"whats-my-public-ip.example.net",
		"getmyip.example.org",
		"myip.example.invalid",
		"icanhazip-like.example",
	}
	for _, h := range covered {
		if !cfg.MatchesHost(h) {
			t.Errorf("expected %q to be intercepted", h)
		}
	}
	notCovered := []string{"pypi.org", "registry.npmjs.org", "github.com", "example.com", "files.pythonhosted.org"}
	for _, h := range notCovered {
		if cfg.MatchesHost(h) {
			t.Errorf("must NOT intercept ordinary host %q", h)
		}
	}
}

func TestDNSEchoNames(t *testing.T) {
	cases := map[string]string{
		"myip.opendns.com":        "a",
		"whoami.akamai.net":       "a",
		"o-o.myaddr.l.google.com": "txt",
		"whoami.cloudflare":       "txt",
	}
	for n, want := range cases {
		got, ok := IsDNSEcho(n)
		if !ok || got != want {
			t.Errorf("IsDNSEcho(%q) = (%q,%v), want (%q,true)", n, got, ok, want)
		}
	}
	if _, ok := IsDNSEcho("pypi.org"); ok {
		t.Errorf("pypi.org must not be a DNS echo")
	}
}

func TestCAAndLeaf(t *testing.T) {
	ca, err := NewCA("Example Transit Networks")
	if err != nil {
		t.Fatal(err)
	}
	// Reload round-trips.
	ca2, err := LoadCA(ca.CertPEM, ca.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca2.Leaf("api.ipify.org")
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.Certificate) < 2 {
		t.Fatalf("leaf chain too short: %d", len(leaf.Certificate))
	}
	if !strings.HasSuffix(ca.FileName(), ".0") {
		t.Fatalf("CA filename %q is not an OpenSSL hashed name", ca.FileName())
	}
	if ca.CertPEM == nil || ca.KeyPEM == nil {
		t.Fatal("CA PEM material missing")
	}
}

func TestResolverAnswersDecoyForEchoAndForwardsOtherwise(t *testing.T) {
	cfg := Defaults()
	cfg.ResolverPort = 0 // bind a random port
	r := NewResolver(cfg, "ws-1", []string{"1.1.1.1"})
	// Build a query for myip.opendns.com A.
	q := buildQuery(t, "myip.opendns.com", dnsmessage.TypeA)
	resp := r.answer(q)
	ip := parseFirstA(t, resp)
	if ip != r.Profile.IP.String() {
		t.Fatalf("DNS echo answered %s, want decoy %s", ip, r.Profile.IP)
	}
	// TXT echo.
	qt := buildQuery(t, "o-o.myaddr.l.google.com", dnsmessage.TypeTXT)
	tresp := r.answer(qt)
	txt := parseFirstTXT(t, tresp)
	if txt != r.Profile.IP.String() {
		t.Fatalf("TXT echo answered %q, want %s", txt, r.Profile.IP)
	}
}

func TestResponderBodiesConsistent(t *testing.T) {
	cfg := Defaults()
	ca, err := NewCA(cfg.Org)
	if err != nil {
		t.Fatal(err)
	}
	r := NewResponder(cfg, ca, "ws-xyz")
	p := r.Profile
	cases := []struct {
		host, path, wantIP, wantOrg string
	}{
		{"api.ipify.org", "/", p.IP.String(), ""},
		{"icanhazip.com", "/", p.IP.String(), ""},
		{"ipinfo.io", "/org", "", p.Org},
		{"ifconfig.me", "/ip", p.IP.String(), ""},
		{"ident.me", "/", p.IP.String(), ""},
		{"checkip.amazonaws.com", "/", p.IP.String(), ""},
		{"wtfismyip.com", "/text", p.IP.String(), ""},
		{"api.myip.com", "/", p.IP.String(), ""},
		{"www.cloudflare.com", "/cdn-cgi/trace", p.IP.String(), ""},
		{"whats-my-public-ip.example.net", "/", p.IP.String(), ""},
	}
	for _, c := range cases {
		_, _, body := r.Respond(httpReq{Method: "GET", Host: c.host, Path: c.path}, c.host)
		if c.wantIP != "" && !strings.Contains(body, c.wantIP) {
			t.Errorf("%s%s body %q does not carry decoy IP %s", c.host, c.path, body, c.wantIP)
		}
		if c.wantOrg != "" && !strings.Contains(body, c.wantOrg) {
			t.Errorf("%s%s body %q does not carry decoy org %s", c.host, c.path, body, c.wantOrg)
		}
	}
	// ipinfo JSON must carry the ASN text too (consistency).
	_, _, body := r.Respond(httpReq{Host: "ipinfo.io", Path: "/json"}, "ipinfo.io")
	if !strings.Contains(body, p.ASN) {
		t.Errorf("ipinfo JSON %q lacks ASN %s", body, p.ASN)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Errorf("ipinfo JSON invalid: %v (%q)", err, body)
	}
}

func TestResponderCloudflareTraceFields(t *testing.T) {
	cfg := Defaults()
	ca, _ := NewCA(cfg.Org)
	r := NewResponder(cfg, ca, "ws-t")
	_, _, body := r.Respond(httpReq{Host: "www.cloudflare.com", Path: "/cdn-cgi/trace"}, "www.cloudflare.com")
	for _, needle := range []string{"ip=", "loc=", "colo=", "warp=", "h="} {
		if !strings.Contains(body, needle) {
			t.Errorf("cloudflare trace missing %q: %q", needle, body)
		}
	}
	if strings.Contains(body, "93.185.106") {
		t.Errorf("trace leaked a real platform address: %q", body)
	}
}

func TestResponderDoHJSON(t *testing.T) {
	cfg := Defaults()
	ca, _ := NewCA(cfg.Org)
	r := NewResponder(cfg, ca, "ws-doh")
	_, _, body := r.Respond(httpReq{Host: "cloudflare-dns.com", Path: "/dns-query", Query: "name=myip.opendns.com&type=A"}, "cloudflare-dns.com")
	var v struct {
		Answer []struct {
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("DoH JSON invalid: %v (%q)", err, body)
	}
	if len(v.Answer) == 0 || v.Answer[0].Data != r.Profile.IP.String() {
		t.Fatalf("DoH JSON did not answer decoy: %q", body)
	}
}

// --- helpers ---------------------------------------------------------------

func buildQuery(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	n, err := dnsmessage.NewName(strings.TrimSuffix(name, ".") + ".")
	if err != nil {
		t.Fatal(err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: n, Type: typ, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func parseFirstA(t *testing.T, msg []byte) string {
	t.Helper()
	var p dnsmessage.Parser
	if _, err := p.Start(msg); err != nil {
		t.Fatal(err)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatal(err)
	}
	a, err := p.Answer()
	if err != nil {
		t.Fatal(err)
	}
	ar, ok := a.Body.(*dnsmessage.AResource)
	if !ok {
		t.Fatalf("answer is %T, not A", a.Body)
	}
	return net.IP(ar.A[:]).String()
}

func parseFirstTXT(t *testing.T, msg []byte) string {
	t.Helper()
	var p dnsmessage.Parser
	if _, err := p.Start(msg); err != nil {
		t.Fatal(err)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatal(err)
	}
	a, err := p.Answer()
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := a.Body.(*dnsmessage.TXTResource)
	if !ok {
		t.Fatalf("answer is %T, not TXT", a.Body)
	}
	if len(tr.TXT) == 0 {
		t.Fatal("empty TXT")
	}
	return tr.TXT[0]
}

// TestResolverServeUDP is an end-to-end check over a real UDP socket.
func TestResolverServeUDP(t *testing.T) {
	cfg := Defaults()
	cfg.ResolverPort = freePort(t)
	r := NewResolver(cfg, "ws-served", []string{"127.0.0.1"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- r.Serve(ctx) }()
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: cfg.ResolverPort})
	if err != nil {
		t.Fatalf("dial resolver: %v", err)
	}
	defer conn.Close()
	// UDP dial succeeds even before the stub binds, so retry the exchange until
	// the server is up (a bound-but-empty port yields ICMP port-unreachable).
	var got string
	for i := 0; i < 50; i++ {
		conn.SetDeadline(time.Now().Add(time.Second))
		if _, err := conn.Write(buildQuery(t, "myip.opendns.com", dnsmessage.TypeA)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		got = parseFirstA(t, buf[:n])
		break
	}
	if got != r.Profile.IP.String() {
		t.Fatalf("served DNS echo %q, want %s", got, r.Profile.IP)
	}
	select {
	case err := <-errCh:
		t.Fatalf("resolver serve returned early: %v", err)
	default:
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
