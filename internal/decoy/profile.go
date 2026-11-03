// Package decoy implements the egress-decoy layer: an in-namespace stub
// resolver and a stub HTTP/TLS responder that answer a workspace's attempts to
// learn its own public IP with a plausible, CONSISTENT invented identity,
// while ordinary internet access (pypi, npm, github) is untouched.
//
// This is a decoy layer, NOT a security boundary. A tenant with their own
// endpoint defeats it entirely (they read the peer address in their own logs),
// and the honest limits are enumerated in docs/EGRESS-DECOY.md and
// docs/DECISIONS.md. It exists to remove the problem for people "playing
// around", in the operator's words.
package decoy

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// Profile is the single decoy identity every covered channel must serve. If
// ipify answers one address while ipinfo.io/org still names the real provider,
// the mismatch teaches the tenant more than the truth would, so the whole
// profile is derived once per workspace and reused verbatim.
type Profile struct {
	// IP is the invented public address, drawn from the configured range.
	IP netip.Addr
	// Org is the organisation text (e.g. a made-up hosting company).
	Org string
	// ASN is the autonomous-system text (e.g. "AS64500").
	ASN string
	// City / Country / Region complete the geolocation answer.
	City    string
	Country string
	Region  string
	// PTR is the reverse-DNS name reported for IP.
	PTR string
	// LatencyMS is a plausible round-trip for latency-reporting services.
	LatencyMS int
}

// Config is the service-level decoy configuration. It is DEFAULT OFF.
type Config struct {
	// Enabled turns the whole layer on. When false no stub runs, no nft rule
	// is installed, no CA is generated, and the workspace's resolv.conf names
	// the configured public resolvers as before.
	Enabled bool
	// AddressRange is the range decoy addresses are drawn from (a CIDR). The
	// documentation ranges 198.51.100.0/24, 203.0.113.0/24 and 192.0.2.0/24 are
	// the safe default; a knowledgeable tenant can recognise them, so an
	// operator may prefer a range he owns (see the ADR).
	AddressRange netip.Prefix
	// Org / ASN / City / Country / Region are the shared profile fields.
	Org     string
	ASN     string
	City    string
	Country string
	Region  string
	// RDNS is the reverse-DNS domain; the PTR becomes "<name>.<rdns>".
	RDNS string
	// ResolverPort, HTTPPort and HTTPSPort are the in-namespace ports the stub
	// servers bind; the in-netns nft rules redirect port 53 and the covered
	// address's 80/443 to them.
	ResolverPort int
	HTTPPort     int
	HTTPSPort    int
	// ExtraHosts are additional hostnames to intercept, matched exactly (or as
	// a suffix when prefixed with ".").
	ExtraHosts []string
	// ExtraPatterns are additional regular expressions (case-insensitive) a
	// hostname must match to be intercepted.
	ExtraPatterns []string
}

// Defaults returns the neutral decoy configuration (disabled).
func Defaults() Config {
	return Config{
		Enabled:      false,
		AddressRange: netip.MustParsePrefix("198.51.100.0/24"),
		Org:          "Example Transit Networks",
		ASN:          "AS64500",
		City:         "Frankfurt",
		Country:      "DE",
		Region:       "Hesse",
		RDNS:         "example-transit.net",
		ResolverPort: 15353,
		HTTPPort:     18080,
		HTTPSPort:    18443,
	}
}

// hostPatterns are the hostname SHAPES that identify an IP-echo / "what is my
// IP" service. A pattern catches a service that did not exist when this list
// was written; a fixed service list does not, so both are implemented. The
// strong service tokens are matched anywhere in the hostname between non-
// alphanumeric boundaries, so an invented variant ("whats-my-public-ip.example")
// is caught as well as the canonical (`api.ipify.org`). The blocks are anchored
// tightly enough not to swallow ordinary hosts (pypi.org, github.com, ...).
var hostPatterns = []*regexp.Regexp{
	// Distinctive service names — matched as substrings with boundaries, so an
	// invented variant ("api64.ipify.org", "my-ipify.example") is caught too.
	regexp.MustCompile(`ipify`),
	regexp.MustCompile(`icanhazip`),
	regexp.MustCompile(`ipinfo`),
	regexp.MustCompile(`ip-?api`),
	regexp.MustCompile(`ipwho(is)?`),
	regexp.MustCompile(`ifconfig`),
	regexp.MustCompile(`ifcfg`),
	regexp.MustCompile(`ipecho`),
	regexp.MustCompile(`seeip`),
	regexp.MustCompile(`jsonip`),
	regexp.MustCompile(`myexternalip`),
	regexp.MustCompile(`checkip`),
	regexp.MustCompile(`whats?my-?ip`),
	regexp.MustCompile(`whatismyip`),
	regexp.MustCompile(`ipchicken`),
	regexp.MustCompile(`ipgrab`),
	regexp.MustCompile(`geoplugin`),
	regexp.MustCompile(`freegeoip`),
	regexp.MustCompile(`ipquery`),
	regexp.MustCompile(`ipaddr`),
	regexp.MustCompile(`hostip`),
	regexp.MustCompile(`ip-?lookup`),
	regexp.MustCompile(`dnsomatic`),
	regexp.MustCompile(`curlmyip`),
	// Generic descriptors that appear inside an invented name.
	regexp.MustCompile(`my-?ip`),
	regexp.MustCompile(`public-?ip`),
	regexp.MustCompile(`external-?ip`),
	regexp.MustCompile(`get-?my-?ip`),
	// Fixed hosts whose whole name is the service.
	regexp.MustCompile(`(^|\.)ip\.sb$`),
	regexp.MustCompile(`(^|\.)ip\.me$`),
	regexp.MustCompile(`(^|\.)ip4?\.me$`),
	regexp.MustCompile(`(^|\.)ipv4\.me$`),
	regexp.MustCompile(`(^|\.)ident\.me$`),
	regexp.MustCompile(`(^|\.)myip\.(com|la|wtf)$`),
	// Browser-based / diagnostics that reveal the same thing.
	regexp.MustCompile(`dnsleaktest`),
	regexp.MustCompile(`browserleaks`),
	// trace/echo endpoints whose hostname does not obviously say "ip".
	regexp.MustCompile(`(^|\.)cloudflare\.com$`),
	regexp.MustCompile(`httpbin`),
	regexp.MustCompile(`httpbingo`),
	regexp.MustCompile(`postman-echo`),
	regexp.MustCompile(`bigdatacloud`),
}

// dohHostPatterns are DNS-over-HTTPS endpoints. Intercepting them stops a
// tenant using DoH to bypass the stub; the responder answers a controlled DoH
// JSON so the client does not error.
var dohHostPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(^|\.)cloudflare-dns\.com$`),
	regexp.MustCompile(`(^|\.)mozilla\.cloudflare-dns\.com$`),
	regexp.MustCompile(`(^|\.)chrome\.cloudflare-dns\.com$`),
	regexp.MustCompile(`(^|\.)dns\.google(\.com)?$`),
	regexp.MustCompile(`(^|\.)doh\.opendns\.com$`),
	regexp.MustCompile(`(^|\.)dns\.quad9\.net$`),
	regexp.MustCompile(`(^|\.)doh\.cleanbrowsing\.org$`),
}

// dnsEchoNames are the classic DNS-based "what is my IP" queries. They return
// the address as seen by the resolver, so the stub answers them directly with
// the decoy (an A record for the A-query names, a TXT record for the TXT ones),
// instead of forwarding and leaking the truth.
var dnsEchoNames = map[string]string{
	"myip.opendns.com":        "a",
	"resolver1.opendns.com":   "a",
	"resolver2.opendns.com":   "a",
	"whoami.akamai.net":       "a",
	"whoami.ds.akahelp.net":   "a",
	"o-o.myaddr.l.google.com": "txt",
	"whoami.cloudflare":       "txt",
}

// wellKnownDoHIPs are resolver addresses whose DoH/DoT endpoints and Cloudflare
// trace are covered even when addressed as a bare IP (there is no name to match
// a pattern against). The in-netns nft rules cover these addresses' 443/853 so
// a client cannot bypass the stub by addressing a resolver directly.
var wellKnownDoHIPs = map[string]bool{
	"1.1.1.1": true, "1.0.0.1": true, "1.1.1.2": true, "1.1.1.3": true,
	"8.8.8.8": true, "8.8.4.4": true,
	"9.9.9.9": true, "149.112.112.112": true,
	"208.67.222.222": true, "208.67.220.220": true,
	"94.140.14.14": true, "94.140.15.15": true,
}

// MatchesHost reports whether host is covered by the built-in shape patterns or
// the configured extras. It is case-insensitive and strips a trailing dot. An
// IP literal is covered when it is a well-known resolver address.
func (c Config) MatchesHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return wellKnownDoHIPs[h]
	}
	for _, p := range hostPatterns {
		if p.MatchString(h) {
			return true
		}
	}
	for _, p := range dohHostPatterns {
		if p.MatchString(h) {
			return true
		}
	}
	for _, extra := range c.ExtraHosts {
		e := strings.ToLower(strings.TrimPrefix(extra, "."))
		if e == "" {
			continue
		}
		if strings.HasPrefix(extra, ".") {
			if h == e || strings.HasSuffix(h, "."+e) {
				return true
			}
			continue
		}
		if h == e {
			return true
		}
	}
	for _, pat := range c.ExtraPatterns {
		if re, err := regexp.Compile("(?i)" + pat); err == nil && re.MatchString(h) {
			return true
		}
	}
	return false
}

// IsDNSEcho reports whether the name is a DNS-based echo query and which record
// family answers it ("a" or "txt").
func IsDNSEcho(name string) (record string, ok bool) {
	r, ok := dnsEchoNames[strings.ToLower(strings.TrimSuffix(name, "."))]
	return r, ok
}

// IsDoH reports whether the host is a known DNS-over-HTTPS endpoint.
func IsDoH(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range dohHostPatterns {
		if p.MatchString(h) {
			return true
		}
	}
	return false
}

// PatternList returns the built-in shape patterns as strings, for docs and for
// the /v1/meta capability report.
func PatternList() []string {
	out := make([]string, 0, len(hostPatterns)+len(dohHostPatterns))
	for _, p := range hostPatterns {
		out = append(out, p.String())
	}
	for _, p := range dohHostPatterns {
		out = append(out, p.String())
	}
	sort.Strings(out)
	return out
}

// Derive returns the decoy profile for a workspace. It is deterministic in the
// workspace id, so a workspace always gets the same identity across restarts
// and across channels, while two workspaces get different addresses (so tenants
// comparing notes cannot simply agree on one value).
func (c Config) Derive(workspaceID string) Profile {
	rangePrefix := c.AddressRange
	if !rangePrefix.IsValid() {
		rangePrefix = netip.MustParsePrefix("198.51.100.0/24")
	}
	// The address range is finite; the number of usable addresses is
	// (2^bits - 2) for a /24 (network and broadcast). Draw a stable index.
	bits := rangePrefix.Bits()
	hostBits := 32 - bits
	var usable uint32 = 1 << hostBits
	if usable > 2 {
		usable -= 2
	}
	sum := sha256.Sum256([]byte("decoy-egress\x00" + workspaceID))
	idx := binary.BigEndian.Uint32(sum[:4]) % usable
	base := rangePrefix.Addr().As4()
	v := binary.BigEndian.Uint32(base[:]) + 1 + idx
	addr := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})

	// A stable short label for the PTR and to vary plausibility.
	label := fmt.Sprintf("%d-%s", idx+1, hex4(sum[4:6]))
	ptrSuffix := strings.TrimSuffix(c.RDNS, ".")
	ptr := label + "." + ptrSuffix

	// A plausible, stable, per-workspace latency.
	latency := 5 + int(binary.BigEndian.Uint16(sum[6:8])%90)

	return Profile{
		IP:        addr,
		Org:       c.Org,
		ASN:       c.ASN,
		City:      c.City,
		Country:   c.Country,
		Region:    c.Region,
		PTR:       ptr,
		LatencyMS: latency,
	}
}

func hex4(b []byte) string {
	const hexd = "0123456789abcdef"
	return string([]byte{
		hexd[b[0]>>4], hexd[b[0]&0xf],
		hexd[b[1]>>4], hexd[b[1]&0xf],
	})
}
