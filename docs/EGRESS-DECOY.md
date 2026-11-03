# Egress decoy — make IP-discovery return a plausible invented answer

This document is the living reference for the **egress-decoy** layer: it
enumerates every way a tenant can learn the workspace's public IP (Part 1),
describes the interception design (Part 2), and states the honest limits. It is
meant to be extended: when a new discovery channel is found, add it here and,
where the design can cover it, in `internal/decoy`.

> **This is a decoy layer, not a security boundary.** It removes the problem for
> people who are "playing around", in the operator's words. It is defeated
> entirely by a tenant who controls their own endpoint (see §1.3). Read
> ADR 84 in `docs/DECISIONS.md` alongside this file.

## What the operator asked for

A workspace must not be able to learn the hosting platform's real public IP.
Normal internet must keep working — `pip install` must still work; an
internal-only workspace is explicitly **not** wanted. The workspace's attempts to
discover its own public IP must come back with a **plausible invented answer**,
looking like it came from the system itself.

It is config-gated and **default off** (`network.decoy.enabled: false`), so no
existing gate changes unless an operator turns it on. The one-line change:

```yaml
network: { decoy: { enabled: true } }
```

## Measured state today (inside a real `python`-profile workspace)

```
curl https://api.ipify.org        93.185.106.128              <- the real answer, unfiltered
curl https://icanhazip.com        93.185.106.128
curl https://ipinfo.io/org        AS43541 VSHosting s.r.o.    <- names the platform
curl cloudflare trace             ip=93.185.106.128 colo=PRG loc=CZ
hostname / uname -n               sandbox                     (already neutral — keep)
/etc/hostname                     absent                      (correct — keep)
/etc/hosts                        loopback only               (correct — keep)
hostname -f                       does not resolve            (correct — keep)

host: public IP 93.185.106.128 (platform-shared NAT); ONE public path only
RDAP for the /24 -> name "ZEROPS-IO", remark "Zerops.IO infrastructure"
rDNS of a neighbour -> gateway2.zerops.io    <- the platform name is in DNS itself
workspace namespace: slirp4netns, uids/network isolated, /proc NOT mounted
e2e: every workspace exits from the same NAT address (shared reputation)
```

---

# Part 1 — How a tenant learns its public IP (the living list)

The list below is grouped by mechanism. "(covered)" means the shipped design
answers it with the decoy; "(declared)" means it is out of scope and is
explicitly declared so, never silently half-fixed.

## 1.1 HTTP/HTTPS "what is my IP" services (covered)

There are **hundreds** of these. The canonical set is:

`api.ipify.org` · `api64.ipify.org` · `ipify.org` · `icanhazip.com` ·
`ipv4.icanhazip.com` · `ipv6.icanhazip.com` · `ifconfig.me` · `ifconfig.co` ·
`ifconfig.io` · `ifcfg.me` · `ipinfo.io` (and `/ip` `/json` `/org` `/country`
`/loc`) · `ip-api.com` (json/line/csv, plain HTTP only) · `ipapi.co` ·
`ipwho.is` · `ipwhois.app` · `ip.sb` · `api.ip.sb/ip` · `seeip.org` /
`ip.seeip.org` / `api.seeip.org/json` · `ident.me` · `v4.ident.me` ·
`ip4.ident.me` · `ipecho.net/plain` · `myexternalip.com` / `/raw` ·
`checkip.amazonaws.com` · `checkip.dyndns.org` / `checkip.dyndns.com` ·
`myip.dnsomatic.com` · `wtfismyip.com` (and `/text` `/json`) · `jsonip.com` ·
`api.myip.com` · `myip.com` · `myip.wtf` · `ipchicken.com` · `whatismyip.com` ·
`whatismyipaddress.com` · `whatsmyip.org` · `ip-lookup.net` ·
`api.hostip.info/get_json.php` · `ipgrab.io` · `geoplugin.net/json.gp` ·
`freegeoip.app` · `ipapi.is` · `ipquery.io` · `ip.me` · `curlmyip.org` ·
`ip4.me` · `ipv4.me` · `ipaddr.com` ·
`api.bigdatacloud.net/data/client-ip` · `ip4.whatismyip.akamai.com` ·
`httpbin.org/ip` · `httpbingo.org/ip` · `postman-echo.com/ip` · `myip.la` ·
`ip2location` / `ipstack` / `ipgeolocation.io` (need a key, same principle) ·
`www.cloudflare.com/cdn-cgi/trace` · `1.1.1.1/cdn-cgi/trace` · `cp.cloudflare.com`
· `dnsleaktest.com` · `browserleaks.com/ip` (browser-based).

**The list is not the point; the pattern is.** A service whose *hostname* looks
like an IP-echo is the same threat:

```
my-?ip · ipify · ipinfo · ip-api · ipapi · ipwho(is)? · icanhazip · whatismyip ·
whatsmyip · checkip · seeip · ifconfig · ifcfg · ident.me · ipecho ·
external-?ip · public-?ip · myexternalip · jsonip · curlmyip · ipchicken ·
ipgrab · geoplugin · freegeoip · ipquery · ipaddr · hostip · ip-lookup ·
dnsomatic · ip.me · ip.sb · ip4.me · ipv4.me · myip.com/la/wtf ·
httpbin/httpbingo/postman-echo/bigdatacloud · cloudflare.com (trace)
```

A pattern catches services that **do not exist yet**; a fixed list does not.
The shipped design implements **both**: the hostname shape patterns above
(`internal/decoy/profile.go`), plus the well-known DoH resolver hostnames. The
probe battery asserts an **invented** hostname
(`whats-my-public-ip.invented-example.net`) is covered — the single most valuable
assertion, because it proves the pattern rather than a list.

## 1.2 DNS-based (covered)

The classic method, and the most often forgotten:

```
dig +short myip.opendns.com @resolver1.opendns.com
dig +short o-o.myaddr.l.google.com TXT @ns1.google.com
dig +short whoami.cloudflare CH TXT @1.1.1.1
dig +short whoami.akamai.net @ns1-1.akamaitechnologies.net
nslookup myip.opendns.com resolver1.opendns.com
```

These return the address **as seen by the resolver**, so they leak the same
thing. They are answerable at the DNS layer with a fake A/TXT record — see §2.1.

## 1.3 The tenant's own endpoint — UNFIXABLE, declared first

```
ssh user@their-server        -> their auth log records the peer address
curl https://their-server/   -> their access log records it
nc their-server 9999         -> their listener sees it
ping their-server            -> their tcpdump sees it
any protocol: SMTP, NTP, IRC, a torrent tracker, a game server
```

Anything that reaches a party the tenant controls reveals the address **to
them**. No host-side interception can change that: the address is decided by
whoever exits to the internet. **This is the fundamental limit and nothing in
this engine claims otherwise.** It is stated first in ADR 84.

## 1.4 Network path inspection (declared, partly)

```
traceroute 8.8.8.8   -> first hop and intermediate routers expose ISP/platform names
mtr 8.8.8.8          -> same, continuously
ping -t 1 / TTL      -> hop count only (weak but a hint)
```

Measured on this host: `93.185.106.1` reverse-resolves to `gw.vshosting.cz` and
neighbours are `gateway2.zerops.io`, so path inspection is a **real leak**.
Hiding intermediate hops without breaking normal routing is not possible with a
userspace decoy. **Declared out of scope** rather than half-fixed.

## 1.5 Bypassing local DNS (covered)

```
DoH : curl 'https://cloudflare-dns.com/dns-query?name=...' -H 'accept: application/dns-json'
      curl 'https://dns.google/resolve?name=...'
      curl -H 'content-type: application/dns-message' 'https://1.1.1.1/dns-query' --data-binary ...
DoT : kdig +tls @1.1.1.1  /  dig +tls
direct resolver by IP: dig @8.8.8.8 whoami.cloudflare CH TXT
```

If a workspace can reach an outside resolver directly, local interception is
bypassed. The design forces DNS through the stub with in-namespace nft rules
(§2.3) and covers the well-known DoH/DoT endpoint addresses, so these paths are
answered with the decoy too.

## 1.6 Non-HTTP channels (declared)

`nc`/`socat` to any echo service · `ssh -v` to any host (banner + their log) ·
NTP against a tenant-controlled server · SMTP · IRC · a WebSocket from inside a
workspace · any outbound TLS to a tenant-controlled host. Same root cause as
§1.3: the peer observes the source address. **Declared out of scope.**

## 1.7 In-workspace signals that hint the platform (check these FIRST)

These are cheap and easy to miss; a platform name in `/etc/resolv.conf` or a
search domain would leak more cheaply than any command.

```
cat /etc/resolv.conf        -> names a platform resolver or a search domain?
hostname -d / dnsdomainname -> does either name the platform?
cat /etc/hosts              -> any platform hostnames?
ip route / netstat -rn      -> gateway address: neutral or a platform address?
ip addr                     -> is the workspace's own address platform-branded?
cat /etc/ssl/certs/*        -> any planted CA names platform-branded?
MTU / ICMP behaviour        -> fingerprinting beyond scope; noted as out of scope
/proc/net/*                 -> not mounted inside a workspace (verified)
```

**Measured baseline on this host** (all already correct, kept):

- `hostname` / `uname -n` → `sandbox`; `/etc/hostname` absent; `hostname -f`
  does not resolve.
- `/etc/hosts` → loopback only (the host's own `/etc/hosts` is never mounted).
- `/etc/resolv.conf` → a per-workspace file naming **public resolvers**, never
  the host's own resolver and never a search domain.
- `ip route` → the workspace's own slirp gateway (a synthetic `10.201.x.2`),
  never a platform address.
- `/etc/ssl/certs` → a filtered trust store with the provider's own certificates
  removed (ADR 74).

Nothing here needs fixing today. The decoy layer keeps the same discipline: the
stub resolver is at `127.0.0.1`, the redirect is by port not by resolver name,
and the decoy CA carries a neutral subject (the decoy org, never a platform
name), so `cat /etc/ssl/certs/*` reveals nothing about the mechanism.

---

# Part 2 — The design

The interception is **not** implemented by matching executed command names. A
command-name filter covers only the clients someone thought of, breaks on a
statically linked binary, and is trivially bypassed by:

```
python -c "import urllib.request; print(urllib.request.urlopen('https://api.ipify.org').read())"
```

Interception happens at the **network layer inside the workspace's own network
namespace**, so the client is irrelevant — which is also what makes the answer
look like it came from the system rather than from a patched library.

Everything is inside the workspace netns. Nothing on the host is touched: no
interface, no address, no route, no host nft rule.

## 2.1 In-namespace stub resolver + `/etc/resolv.conf`

`internal/decoy/resolver.go` binds UDP+TCP on `127.0.0.1:15353` inside the
workspace namespace. The workspace's `/etc/resolv.conf` (a bind-mounted file the
manager generates) names `127.0.0.1`. The resolver:

- answers the DNS echo names (`myip.opendns.com`, `whoami.akamai.net` → A;
  `o-o.myaddr.l.google.com`, `whoami.cloudflare` → TXT) with the decoy;
- answers a covered hostname (HTTP echo or DoH) with the decoy **A** record, so
  the TLS responder receives the connection;
- answers the decoy address's PTR with the decoy reverse name;
- **forwards everything else** (pypi, npm, github) to the configured public
  resolvers. Its upstream queries carry `SO_MARK = 0x24` so the port-53 redirect
  (§2.3) lets them out instead of re-capturing them.

This is what makes `dig myip.opendns.com` answerable with a fake record instead
of leaking the truth or returning NXDOMAIN.

## 2.2 Stub HTTP/TLS responder + a generated CA trusted inside the workspace

`internal/decoy/responder.go` binds HTTP on `127.0.0.1:18080` and TLS on
`127.0.0.1:18443`. Because these services are almost all HTTPS, seamless
interception needs a **generated CA trusted inside the workspace**:

- `internal/decoy/ca.go` generates a CA whose subject is the decoy org (never a
  platform name), persisted under the engine tmp dir so it survives a restart.
- The executor's curated `/etc/ssl/certs` overlay has the decoy CA appended to
  its concatenated bundle (`ca-certificates.crt`) **and** installed under its
  OpenSSL hashed name (`internal/executor/provider.go:amendCertsWithDecoy`), so
  both `SSL_CERT_FILE`/`--cacert` and the default `capath` lookup find it.
  Python's `ssl` default paths (`/usr/lib/ssl/cert.pem`, `/etc/ssl/certs`) and
  python's `certifi` (which on Debian is a symlink to the same bundle) both read
  it. **No `SSL_CERT_FILE` needs to be injected into the workload** — the
  mounted trust store is enough, which is what keeps the answer looking native.
- On the TLS listener, `GetCertificate` mints a per-hostname certificate on
  demand (SAN = the requested name) so `curl`, `wget` and python all verify with
  **no certificate warning**. A client that sends no SNI (a bare resolver IP
  such as `https://1.1.1.1/`) gets a certificate carrying every covered
  resolver IP as a SAN, so that path also verifies.
- The same TLS port distinguishes a DNS-over-TLS frame (a 2-byte length prefix)
  from HTTP, so `dig +tls @1.1.1.1` is answered with the decoy too.

The trust-store amendment requires the curated `/etc/ssl/certs` overlay, i.e.
`identity.hide_provider_tools: true` (the default). If an operator disables
provider-tool hiding, the host trust store is mounted read-only and cannot be
amended; the decoy still answers every channel's **body**, but a TLS client
would see a certificate warning, so enabling the decoy in that state is
discouraged (the operator can still add the decoy CA to their base image).

The responder chooses a body shape that matches the addressed service — plain
text, `{"ip":...}`, `ipinfo.io`'s JSON/`/org`, the Cloudflare trace, `ip-api`'s
line format, a DoH JSON — so the answer looks like it came from that service.

**A warning is worse than nothing** because it tells the tenant the environment
is tampered with, so the probe battery asserts the absence of a certificate
warning for curl, wget and python, not merely that the decoy address is
returned.

## 2.3 In-namespace egress rules (nft)

`internal/decoy/nft.go` renders an nft program installed with
`nsenter --net=<ws> nft -f -`. It is confined to an engine-owned table
(`inet sbxd_decoy`), deleted on teardown, so teardown never removes a rule the
engine did not create:

```
table inet sbxd_decoy {
    chain out { type nat hook output priority dstnat; policy accept;
        meta mark 0x00000024 accept                          # the stub's own upstream
        udp dport 53 dnat ip to 127.0.0.1:15353              # DNS -> stub
        tcp dport 53 dnat ip to 127.0.0.1:15353
        ip daddr 1.1.1.1 tcp dport { 443, 853 } dnat ip to 127.0.0.1:18443
        ... (every well-known resolver address)              # DoH/DoT -> stub
        ip daddr <decoy-ip> tcp dport 443 dnat ip to 127.0.0.1:18443
        ip daddr <decoy-ip> tcp dport 80  dnat ip to 127.0.0.1:18080
    }
}
```

`dnat` (not `redirect`) targets the loopback address, which works on the output
hook for a locally-generated socket. The mark-accept rule is **first**, so the
stub resolver's forwarded upstream queries are not re-captured.

## 2.4 Consistency is the whole illusion

If `ipify` answers `198.51.100.7` while `ipinfo.io/org` still says
`AS43541 VSHosting`, the tenant learns more from the mismatch than from the
truth. Every channel serves **one profile** — same address, same org, same ASN,
same city, same country, same rDNS.

`internal/decoy/profile.go` derives the profile **per workspace** from the
workspace id (SHA-256), so a workspace always gets the same identity across
restarts and channels, while two tenants comparing notes see different
addresses. The whole profile lives in config with neutral defaults:

| key | default |
|-----|---------|
| `address_range` | `198.51.100.0/24` (documentation range) |
| `org` | `Example Transit Networks` |
| `asn` | `AS64500` (private/documentation-ish ASN) |
| `city` / `country` / `region` | `Frankfurt` / `DE` / `Hesse` |
| `rdns` | `example-transit.net` |

**Trade-off (in ADR 84):** documentation ranges (`198.51.100.0/24`,
`203.0.113.0/24`, `192.0.2.0/24`) are real dotted quads that resolve to
nothing — the safe default — but a knowledgeable tenant can recognise them. An
operator may prefer a range he owns; `address_range` makes that a config change,
not a code change.

## 2.5 Lifecycle and cleanup

The helper is the same binary re-executed with the hidden subcommand
`__decoy-serve <spec>` under `nsenter --net=<ws> --no-fork`. The spec (which
carries the CA private key) is written `0600` under the engine tmp dir, never
inside a workspace. The manager binds the stub listeners **synchronously** and
the helper writes a ready file before the nft redirect is installed, so a run
started immediately after the workspace is created never sees a port with no
listener. On stop the helper process group is killed by exact pid, the nft table
is deleted inside the namespace, and the spec file is removed. A hard daemon
death is handled by `ReapOrphans` (matched by argv, never `pkill`/`killall`).

---

# Part 3 — Verification

`scripts/egress-decoy-check.sh` is the real-nsjail probe battery. It runs the
real service against the host's real nsjail and slirp4netns with the decoy **on**
and asserts, from inside a real workspace:

- **covered HTTP services** (`api.ipify.org`, `icanhazip.com`, `ipinfo.io/org`,
  `ifconfig.me`, `ip-api.com`, `ident.me`, `checkip.amazonaws.com`,
  `wtfismyip.com`, `api.myip.com`, the Cloudflare trace) answer the decoy and
  **not** the real address and **not** a platform name;
- an **invented hostname** matching the heuristic is covered and agrees with the
  decoy (the pattern, not a list);
- every **DNS method** (opendns, google TXT, cloudflare CH TXT, akamai) answers
  the decoy;
- the **bypasses** (DoH `cloudflare-dns.com` + `dns.google`, DoT `@1.1.1.1`,
  direct `@8.8.8.8`) answer the decoy;
- **curl, wget, python urllib and python requests** all get the decoy with **no
  certificate warning**;
- **consistency**: every channel agrees on one address and one org;
- the **negative control**: `pip download six`, `github`, `npm` and `pypi` still
  succeed with the decoy on (a failure is a regression and is reported as one);
- the **host topology is untouched** (no interface/route/namespace change, no
  stray helper).

The suite may report skipped-environment assertions only when the public
internet is unreachable from the workspace; a covered channel that leaks is
always a hard failure. See `docs/VERIFICATION.md` §9k for this session's numbers.

With the decoy **off** (the default) the layer does not run and the existing
`scripts/identity-check.sh` §6 reports the measured (real) egress address, as
before — no gate regresses.

# Known limits (restated)

1. **The tenant's own endpoint defeats it entirely** — the address is decided by
   whoever exits to the internet. Unfixable by design.
2. **Path inspection** (`traceroute`/`mtr`) exposes intermediate hops and
   platform names; not hidden, because doing so would break routing.
3. **A hardcoded-IP endpoint** not in the covered set (an echo service reachable
   only by a bare IP that is not a well-known resolver) is not intercepted.
4. **Novel services** are covered only if their hostname matches a shape pattern
   or is added to `extra_hosts`/`extra_patterns`. The pattern is deliberately
   broad, but it is not omniscient.
5. **MTU / ICMP / timing fingerprinting** is out of scope.
6. **A knowledgeable tenant can recognise a documentation-range address**; the
   operator can pick a range he owns.
7. The layer is **default off**; it does nothing until the operator enables it.
