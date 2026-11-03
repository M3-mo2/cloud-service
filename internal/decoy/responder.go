package decoy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Responder is the in-namespace stub HTTP/TLS server. It serves the decoy
// profile for every intercepted hostname, choosing a body shape that matches
// the service the tenant addressed (plain text, JSON, the Cloudflare trace) so
// the answer looks like it came from that service rather than from a proxy.
type Responder struct {
	cfg      Config
	Profile  Profile
	CA       *CA
	HTTPPort int
	TLS      bool
	// dot is the shared resolver used to answer a DNS-over-TLS frame that
	// arrives on the TLS port (the nft rules redirect port 853 there), so
	// `kdig +tls @1.1.1.1` gets the decoy too.
	dot *Resolver
}

// NewResponder builds a responder for one workspace. res, when non-nil, lets
// the TLS port answer DNS-over-TLS frames as well.
func NewResponder(cfg Config, ca *CA, workspaceID string, res *Resolver) *Responder {
	return &Responder{cfg: cfg, Profile: cfg.Derive(workspaceID), CA: ca, HTTPPort: cfg.HTTPPort, dot: res}
}

// Serve runs both the plain-HTTP and the TLS listener until ctx is cancelled.
func (r *Responder) Serve(ctx context.Context) error {
	httpsPort := r.cfg.HTTPSPort
	// Plain HTTP (a few services, and ip-api.com, are HTTP-only).
	httpLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(r.cfg.HTTPPort)))
	if err != nil {
		return fmt.Errorf("decoy responder: http bind: %w", err)
	}
	tlsLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(httpsPort)))
	if err != nil {
		httpLn.Close()
		return fmt.Errorf("decoy responder: https bind: %w", err)
	}
	defer httpLn.Close()
	defer tlsLn.Close()
	return r.serveBound(ctx, httpLn, tlsLn)
}

// serveBound runs the responder over already-bound listeners so the helper can
// bind synchronously and signal readiness first.
func (r *Responder) serveBound(ctx context.Context, httpLn, tlsLn net.Listener) error {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			host := strings.TrimSuffix(hello.ServerName, ".")
			if host == "" {
				// No SNI: a client addressed a bare resolver IP. Present a
				// certificate carrying every covered resolver IP as a SAN so
				// verification succeeds (never a warning) whichever one it used.
				return r.CA.LeafForIPs(wellKnownDoHIPList())
			}
			return r.CA.Leaf(host)
		},
	}
	// Bind the wrapped TLS listener up front so a client never races a nil.
	wrapped := tls.NewListener(tlsLn, tlsCfg)
	done := make(chan struct{}, 2)
	go func() { r.acceptLoop(ctx, httpLn, false); done <- struct{}{} }()
	go func() { r.acceptLoop(ctx, wrapped, true); done <- struct{}{} }()
	select {
	case <-ctx.Done():
		return nil
	case <-done:
		return nil
	}
}

func (r *Responder) acceptLoop(ctx context.Context, ln net.Listener, isTLS bool) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Serve closes the listener when ctx is cancelled, which makes
			// Accept return; a transient error is retried.
			if ctx.Err() != nil {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		go r.handle(conn, isTLS)
	}
}

func (r *Responder) handle(conn net.Conn, isTLS bool) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	host := ""
	if tc, ok := conn.(*tls.Conn); ok {
		if err := tc.Handshake(); err != nil {
			return
		}
		host = strings.TrimSuffix(tc.ConnectionState().ServerName, ".")
	}
	// Peek the first bytes to distinguish HTTP from DNS-over-TLS on the same
	// TLS port. A DNS-over-TLS frame starts with a 2-byte big-endian length
	// whose first byte is 0x00 for any query under 256 bytes; an HTTP request
	// starts with an ASCII method. This lets one listener answer both without a
	// second port, so `kdig +tls @1.1.1.1` gets the decoy too.
	var lead [1]byte
	n, err := conn.Read(lead[:])
	if err != nil || n == 0 {
		return
	}
	if looksLikeDNSFrame(lead[0]) {
		r.handleDoT(conn, lead[0])
		return
	}
	req, err := readRequestWithLead(conn, lead[0])
	if err != nil {
		return
	}
	if host == "" {
		host = hostOnly(req.Host)
	}
	status, ctype, body := r.Respond(req, host)
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, statusText(status), ctype, len(body), body)
	conn.Write([]byte(resp))
}

// looksLikeDNSFrame reports whether a leading byte begins a DNS-over-TLS frame
// (a 2-byte length prefix whose high byte is 0). HTTP methods all start with a
// letter (G, P, H, O, D, ...), never NUL.
func looksLikeDNSFrame(b byte) bool { return b == 0 }

// handleDoT reads one DNS-over-TLS frame (2-byte length + message), answers it
// with the same logic as the UDP/TCP resolver, and writes the response.
func (r *Responder) handleDoT(conn net.Conn, first byte) {
	var lenBuf [2]byte
	lenBuf[0] = first
	if _, err := readFull(conn, lenBuf[1:]); err != nil {
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
	if r.dot == nil {
		return
	}
	resp := r.dot.answer(msg)
	out := make([]byte, 2+len(resp))
	binary.BigEndian.PutUint16(out, uint16(len(resp)))
	copy(out[2:], resp)
	conn.Write(out)
}

// httpReq is the minimal request view the responder needs.
type httpReq struct {
	Method string
	Host   string
	Path   string
	Query  string
}

// readRequest reads an HTTP request head. A client keeps the connection open
// (it is waiting for the response), so the reader must stop at the end of the
// header block — reading to EOF would block until the deadline.
func readRequest(c net.Conn) (httpReq, error) {
	return readRequestWithLead(c, 0)
}

// readRequestWithLead reads a request whose first byte has already been consumed
// (lead != 0), so the DNS/HTTP discriminator does not lose it.
func readRequestWithLead(c net.Conn, lead byte) (httpReq, error) {
	var buf []byte
	if lead != 0 {
		buf = append(buf, lead)
	}
	tmp := make([]byte, 4096)
	for {
		if i := bytes.Index(buf, []byte("\r\n\r\n")); i >= 0 {
			buf = buf[:i]
			break
		}
		if len(buf) > (1 << 16) {
			break
		}
		n, err := c.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			continue
		}
		if err != nil {
			if len(buf) == 0 {
				return httpReq{}, err
			}
			break
		}
	}
	lines := strings.Split(string(buf), "\r\n")
	if len(lines) == 0 {
		return httpReq{}, fmt.Errorf("empty request")
	}
	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) < 2 {
		return httpReq{}, fmt.Errorf("bad request line")
	}
	req := httpReq{Method: parts[0], Path: parts[1]}
	if u, err := url.Parse(parts[1]); err == nil {
		req.Path = u.Path
		req.Query = u.RawQuery
	}
	for _, ln := range lines[1:] {
		if i := strings.Index(ln, ":"); i > 0 {
			k := strings.TrimSpace(ln[:i])
			if strings.EqualFold(k, "Host") {
				req.Host = strings.TrimSpace(ln[i+1:])
			}
		}
	}
	return req, nil
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 404:
		return "Not Found"
	default:
		return "OK"
	}
}

func hostOnly(h string) string {
	h = strings.TrimSpace(h)
	if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}

// Respond chooses a body for the addressed service so the answer is consistent
// across every channel: the same IP, org, ASN, city, country and rDNS.
func (r *Responder) Respond(req httpReq, host string) (int, string, string) {
	p := r.Profile
	path := req.Path
	host = strings.ToLower(host)

	// DNS-over-HTTPS endpoints: answer a controlled JSON DNS reply so a DoH
	// client does not error and cannot learn the truth.
	if IsDoH(host) || isDoHPath(path) {
		return 200, "application/dns-json", r.dohJSON(req.Query)
	}

	switch {
	case strings.HasSuffix(host, "ipify.org"):
		if strings.Contains(path, "json") {
			return 200, "application/json", fmt.Sprintf(`{"ip":%q}`, p.IP.String())
		}
		return 200, "text/plain", p.IP.String() + "\n"

	case strings.HasSuffix(host, "icanhazip.com"):
		return 200, "text/plain", p.IP.String() + "\n"

	case strings.HasSuffix(host, "ifconfig.me"), strings.HasSuffix(host, "ifconfig.co"), strings.HasSuffix(host, "ifconfig.io"), strings.HasSuffix(host, "ifcfg.me"):
		switch {
		case strings.Contains(path, "json"):
			return 200, "application/json", r.jsonipStyle()
		case strings.Contains(path, "city"):
			return 200, "text/plain", p.City + "\n"
		case strings.Contains(path, "country"):
			return 200, "text/plain", p.Country + "\n"
		case strings.Contains(path, "ua"):
			return 200, "text/plain", "curl\n"
		default:
			return 200, "text/plain", p.IP.String() + "\n"
		}

	case strings.HasSuffix(host, "ipinfo.io"):
		switch {
		case strings.HasSuffix(path, "/org"):
			return 200, "text/plain", p.ASN + " " + p.Org + "\n"
		case strings.HasSuffix(path, "/country"):
			return 200, "text/plain", p.Country + "\n"
		case strings.HasSuffix(path, "/city"):
			return 200, "text/plain", p.City + "\n"
		case strings.HasSuffix(path, "/loc"):
			return 200, "text/plain", locOf(p) + "\n"
		case strings.Contains(path, "json") || path == "/" || path == "":
			return 200, "application/json", r.ipinfoJSON()
		default:
			return 200, "text/plain", p.IP.String() + "\n"
		}

	case strings.Contains(host, "ip-api.com"):
		if strings.Contains(path, "line") {
			return 200, "text/plain", fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s\n", p.IP, p.Country, p.Country, p.Region, p.City, "0", p.Org)
		}
		return 200, "application/json", fmt.Sprintf(`{"status":"success","country":"%s","countryCode":"%s","region":"%s","regionName":"%s","city":"%s","query":"%s","org":"%s","as":"%s"}`,
			countryName(p.Country), p.Country, regionCode(p), p.Region, p.City, p.IP, p.Org, p.ASN)

	case strings.HasSuffix(host, "ident.me"):
		return 200, "text/plain", p.IP.String() + "\n"

	case strings.HasSuffix(host, "checkip.amazonaws.com"), strings.HasSuffix(host, "checkip.dyndns.org"), strings.HasSuffix(host, "checkip.dyndns.com"):
		if strings.Contains(host, "amazonaws") {
			return 200, "text/plain", p.IP.String() + "\n"
		}
		return 200, "text/html", "<html><head><title>Current IP Check</title></head><body>Current IP Address: " + p.IP.String() + "</body></html>\n"

	case strings.HasSuffix(host, "wtfismyip.com"):
		if strings.Contains(path, "json") {
			return 200, "application/json", r.wtfJSON()
		}
		return 200, "text/plain", p.IP.String() + "\n"

	case strings.Contains(host, "myip.com"), strings.Contains(host, "api.myip.com"), strings.HasSuffix(host, "myip.la"), strings.HasSuffix(host, "myip.wtf"):
		return 200, "application/json", fmt.Sprintf(`{"ip":%q,"country":%q,"cc":%q}`, p.IP.String(), countryName(p.Country), p.Country)

	case strings.Contains(host, "jsonip.com"):
		return 200, "application/json", fmt.Sprintf(`{"ip":%q,"about":"/about","Pro!":true}`, p.IP.String())

	case strings.Contains(host, "seeip.org"):
		return 200, "application/json", fmt.Sprintf(`{"ip":%q,"continent_code":"EU","country_code":%q,"country":%q,"region":%q,"city":%q}`, p.IP.String(), p.Country, countryName(p.Country), p.Region, p.City)

	case strings.Contains(host, "geoplugin.net"):
		return 200, "application/json", fmt.Sprintf(`{"geoplugin_request":%q,"geoplugin_status":200,"geoplugin_city":%q,"geoplugin_region":%q,"geoplugin_countryCode":%q,"geoplugin_countryName":%q}`, p.IP.String(), p.City, p.Region, p.Country, countryName(p.Country))

	case strings.Contains(host, "ipwho.is"), strings.Contains(host, "ipwhois.app"):
		return 200, "application/json", r.ipwhoJSON()

	case strings.Contains(host, "ipapi.co"), strings.Contains(host, "ipapi.com"):
		return 200, "application/json", r.ipapiJSON()

	case strings.Contains(host, "bigdatacloud.net"):
		return 200, "application/json", fmt.Sprintf(`{"ipString":%q,"ipAddress":%q,"countryName":%q,"country":{"isoCode":%q},"locality":%q}`, p.IP.String(), p.IP.String(), countryName(p.Country), p.Country, p.City)

	case strings.Contains(host, "hostip.info"):
		return 200, "application/json", fmt.Sprintf(`{"ip":%q,"country_name":%q,"country_code":%q,"city":%q}`, p.IP.String(), countryName(p.Country), p.Country, p.City)

	case strings.Contains(host, "ipgrab.io"), strings.Contains(host, "ipquery.io"):
		return 200, "application/json", fmt.Sprintf(`{"ip":%q,"country":%q,"city":%q}`, p.IP.String(), p.Country, p.City)

	case strings.Contains(host, "ip.sb"), strings.Contains(host, "ip.me"), strings.Contains(host, "ipecho.net"), strings.Contains(host, "curlmyip.org"):
		return 200, "text/plain", p.IP.String() + "\n"

	case strings.Contains(host, "myexternalip.com"):
		return 200, "text/plain", p.IP.String() + "\n"

	case strings.Contains(host, "ipchicken.com"), strings.Contains(host, "whatismyip.com"), strings.Contains(host, "whatsmyip.org"), strings.Contains(host, "whatismyipaddress.com"), strings.Contains(host, "ip-lookup.net"), strings.Contains(host, "ipaddr.com"):
		return 200, "text/html", "<html><head><title>What Is My IP Address?</title></head><body><h1>" + p.IP.String() + "</h1><p>" + p.City + ", " + p.Country + " \u2014 " + p.Org + "</p></body></html>\n"

	case strings.Contains(host, "dnsleaktest.com"), strings.Contains(host, "browserleaks.com"):
		return 200, "text/html", "<html><head><title>IP</title></head><body>" + p.IP.String() + "</body></html>\n"

	case isCloudflareTrace(req, host):
		return 200, "text/plain", r.cloudflareTrace()

	case strings.Contains(host, "httpbin"), strings.Contains(host, "httpbingo"), strings.Contains(host, "postman-echo"):
		return 200, "application/json", fmt.Sprintf(`{"origin":%q}`, p.IP.String())

	default:
		// Any other shape-matched host (including an INVENTED service name that
		// matches the heuristic) gets the IP as text and a small JSON at /json.
		if strings.Contains(path, "json") {
			return 200, "application/json", fmt.Sprintf(`{"ip":%q,"ipAddress":%q,"query":%q,"origin":%q}`, p.IP.String(), p.IP.String(), p.IP.String(), p.IP.String())
		}
		return 200, "text/plain", p.IP.String() + "\n"
	}
}

// isCloudflareTrace reports whether the request is the Cloudflare trace
// endpoint — which is served from ANY Cloudflare-fronted host, so it is
// identified by PATH, not by host.
func isCloudflareTrace(req httpReq, host string) bool {
	if req.Path == "/cdn-cgi/trace" {
		return true
	}
	return host == "cp.cloudflare.com"
}

func isDoHPath(path string) bool {
	return path == "/dns-query" || path == "/resolve"
}

// --- body builders ---

func (r *Responder) cloudflareTrace() string {
	p := r.Profile
	return strings.Join([]string{
		"fl=0f0",
		"h=" + p.IP.String(),
		"ip=" + p.IP.String(),
		"ts=" + strconv.FormatInt(time.Now().Unix(), 10),
		"visit_scheme=https",
		"uag=curl/8.5.0",
		"colo=" + coloFor(p),
		"sliver=none",
		"http=http/1.1",
		"loc=" + p.Country,
		"tls=TLSv1.3",
		"sni=plaintext",
		"warp=off",
		"gateway=off",
	}, "\n") + "\n"
}

func (r *Responder) ipinfoJSON() string {
	p := r.Profile
	return fmt.Sprintf(`{"ip":%q,"hostname":%q,"city":%q,"region":%q,"country":%q,"loc":%q,"org":%q,"postal":"10115","timezone":"Europe/Berlin","readme":"https://ipinfo.io/missingauth"}`,
		p.IP.String(), p.PTR, p.City, p.Region, p.Country, locOf(p), p.ASN+" "+p.Org)
}

func (r *Responder) jsonipStyle() string {
	p := r.Profile
	return fmt.Sprintf(`{"ip":%q,"about":"https://ifconfig.me","family":"IPv4"}`, p.IP.String())
}

func (r *Responder) wtfJSON() string {
	p := r.Profile
	return fmt.Sprintf(`{"ip":%q,"city":%q,"country":%q,"organization":%q,"asn":%q,"hostname":%q,"location":%q,"region":%q}`,
		p.IP.String(), p.City, p.Country, p.Org, p.ASN, p.PTR, locOf(p), p.Region)
}

func (r *Responder) ipwhoJSON() string {
	p := r.Profile
	return fmt.Sprintf(`{"ip":%q,"success":true,"type":"IPv4","continent":"Europe","continent_code":"EU","country":%q,"country_code":%q,"region":%q,"city":%q,"connection":{"asn":64500,"org":%q,"isp":%q,"domain":%q}}`,
		p.IP.String(), countryName(p.Country), p.Country, p.Region, p.City, p.Org, p.Org, strings.TrimPrefix(p.PTR, "1-"))
}

func (r *Responder) ipapiJSON() string {
	p := r.Profile
	return fmt.Sprintf(`{"ip":%q,"network":%q,"version":"IPv4","city":%q,"region":%q,"country":%q,"country_code":%q,"country_name":%q,"org":%q,"asn":%q}`,
		p.IP.String(), p.IP.String(), p.City, p.Region, p.Country, p.Country, countryName(p.Country), p.Org, p.ASN)
}

// dohJSON answers a DoH GET (?name=...&type=A|TXT) with a controlled DNS JSON.
func (r *Responder) dohJSON(query string) string {
	vals, _ := url.ParseQuery(query)
	name := strings.ToLower(strings.TrimSuffix(vals.Get("name"), "."))
	qtype := strings.ToUpper(vals.Get("type"))
	p := r.Profile
	type dnsAns struct {
		Name string `json:"name"`
		Type int    `json:"type"`
		TTL  int    `json:"TTL"`
		Data string `json:"data"`
	}
	resp := struct {
		Status int      `json:"Status"`
		TC     bool     `json:"TC"`
		RD     bool     `json:"RD"`
		RA     bool     `json:"RA"`
		AD     bool     `json:"AD"`
		CD     bool     `json:"CD"`
		Answer []dnsAns `json:"Answer"`
	}{Status: 0, RD: true, RA: true}

	if qtype == "TXT" {
		resp.Answer = []dnsAns{{Name: name + ".", Type: 16, TTL: 30, Data: `"` + p.IP.String() + `"`}}
	} else {
		// Any A query is answered with the decoy (an echo or a covered host).
		resp.Answer = []dnsAns{{Name: name + ".", Type: 1, TTL: 30, Data: p.IP.String()}}
	}
	out, _ := json.Marshal(resp)
	return string(out)
}

// locOf returns the "lat,lon" ipinfo reports.
func locOf(p Profile) string {
	return "50.1109,8.6821"
}

func regionCode(p Profile) string { return p.Region }

func coloFor(p Profile) string {
	// A stable, plausible Cloudflare colo for the decoy city.
	switch p.City {
	case "Frankfurt":
		return "FRA"
	case "Amsterdam":
		return "AMS"
	case "London":
		return "LHR"
	default:
		return "FRA"
	}
}

func countryName(cc string) string {
	switch strings.ToUpper(cc) {
	case "DE":
		return "Germany"
	case "NL":
		return "Netherlands"
	case "GB":
		return "United Kingdom"
	case "US":
		return "United States"
	case "FR":
		return "France"
	default:
		return cc
	}
}
