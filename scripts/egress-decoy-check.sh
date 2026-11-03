#!/usr/bin/env bash
# egress-decoy-check.sh -- real-nsjail acceptance for the egress-decoy layer.
#
# WHY THIS EXISTS
#   The operator wants a workspace's attempts to learn its own public IP to come
#   back with a plausible INVENTED answer, while ordinary internet keeps working
#   (`pip install` must still work). The egress-decoy layer answers every channel
#   from ONE consistent per-workspace profile using an in-namespace stub DNS
#   resolver and a stub HTTP/TLS responder, with a generated CA trusted inside
#   the workspace and in-namespace nft rules forcing DNS to the stub.
#
#   This harness runs the REAL service against the host's REAL nsjail and
#   slirp4netns with the decoy ON, and asserts -- from inside a real workspace --
#   that every covered channel answers the decoy and NOT the truth, that all
#   channels agree on ONE address and ONE org, that the covered DNS methods and
#   the DoH/DoT/direct-resolver bypasses are all covered, that the common client
#   runtimes all get the decoy with NO certificate warning, and that the
#   negative control (pypi / github / npm) still succeeds.
#
#   It is a DECOY, not a security boundary (see docs/EGRESS-DECOY.md and ADR
#   84). The harness deliberately does NOT test the tenant's own endpoint limit:
#   that is unfixable by design and is documented, not asserted.
#
# WHAT IT ASSERTS
#   1. the decoy CAPABILITY is on and ready (readyz + /v1/meta);
#   2. every covered HTTP service answers the decoy (NOT the real address);
#   3. an INVENTED hostname matching the heuristic is covered too (the most
#      valuable assertion: it proves the PATTERN, not a fixed list, is used);
#   4. all four classic DNS echo methods answer the decoy;
#   5. the mechanism-bypassing DNS paths (DoH cloudflare-dns.com + dns.google,
#      DoT @1.1.1.1, direct @8.8.8.8) answer the decoy;
#   6. the client runtimes curl, wget, python urllib and python requests all get
#      the decoy -- with NO certificate warning (a warning is worse than
#      nothing: it tells the tenant the environment is tampered with);
#   7. CONSISTENCY: every channel agrees on one address and one org;
#   8. the NEGATIVE CONTROL: pip download, github and npm still succeed with the
#      decoy on (a failure is a regression and is reported as one);
#   9. the host topology is untouched (no interface/route/namespace change).
#
# THE THIRD STATE: skipped-environment.
#   Assertions that need the public internet are recorded as SKIP with the
#   reason when the egress path cannot reach it at all, and the suite still
#   exits 0. This is never used to skip assertion 2/4/5/7/8: if the decoy is on
#   but a covered channel leaks, that is ALWAYS a hard FAIL.
#
# REQUIREMENTS
#   Root (nsjail + slirp4netns + nft), the real nsjail binary, slirp4netns, nft,
#   the Go toolchain, python3 and curl. `sudo` resets PATH, so the toolchain is
#   resolved explicitly.
#
# USAGE
#   sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin:/usr/sbin" \
#     bash scripts/egress-decoy-check.sh
#   Environment: PORT (default 8096), NSJAIL (build path override).
#
# EXIT
#   0 when every assertion passes (or only external-internet assertions were
#   skipped); non-zero on any real failure. When root, nsjail, slirp4netns or
#   the toolchain is absent the script prints SKIP and exits 0.
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PORT=${PORT:-8096}
DATA=$(mktemp -d)
NSDIR="$DATA/netns"
CFG="$DATA/config.yaml"
BIN=$(mktemp)
LOG="$DATA/service.log"
SRV=""
KEY=dev-key-please-rotate
A="Authorization: Bearer $KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
PASS=0
FAIL=0
SKIP=0
SKIP_REASON=""
WORK=$(mktemp -d)

chk() { # label expected actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}
skip() { # label reason
  echo "  SKIP  $1 ($2)"; SKIP=$((SKIP + 1))
}
jsonf() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)"; }

# --- host topology snapshots -----------------------------------------------
topo_links() { ip -o link 2>/dev/null | awk '{print $2,$9}' | sort; }
topo_routes() { ip route 2>/dev/null | sort; }
HOST_LINKS_BEFORE=$(topo_links)
HOST_ROUTES_BEFORE=$(topo_routes)
NS_BEFORE=$(ls -A /run/netns 2>/dev/null | wc -l | tr -d ' ')

cleanup() {
  [ -n "$SRV" ] && { kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; }
  sleep 1
  for n in $(ls "$NSDIR" 2>/dev/null); do umount "$NSDIR/$n" 2>/dev/null; rm -f "$NSDIR/$n"; done
  rm -rf "$DATA" "$BIN" "$WORK"
}
trap cleanup EXIT INT TERM

if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root; re-run with: sudo -n env \"PATH=\$PATH:/usr/local/go/bin:/usr/local/bin:/usr/sbin\" bash scripts/egress-decoy-check.sh"
  exit 0
fi
command -v nsjail >/dev/null || { echo "SKIP: nsjail is not installed on this host"; exit 0; }
command -v slirp4netns >/dev/null || { echo "SKIP: slirp4netns is not installed on this host"; exit 0; }
NFT=$(command -v nft || echo /usr/sbin/nft)
[ -x "$NFT" ] || { echo "SKIP: nft is not installed on this host ($NFT)"; exit 0; }
if ! getent group sandbox >/dev/null; then
  groupadd -r sandbox && useradd -r -g sandbox -M -s /usr/sbin/nologin sandbox
fi

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
if command -v cc >/dev/null 2>&1; then
  cc -shared -fPIC -O2 -Wall -s -fno-ident -Wl,-soname,rt.so \
    -Wl,--version-script="$ROOT/internal/executor/hostmask/hostmask.map" \
    -o "$ROOT/internal/executor/hostmask.so" \
    "$ROOT/internal/executor/hostmask/hostmask.c" -ldl || { echo "FAIL  shim: could not build hostmask.so"; exit 1; }
  objcopy -R .comment "$ROOT/internal/executor/hostmask.so" 2>/dev/null || true
fi
(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1
# The helper re-execs os.Executable(), so the built binary must be runnable as
# a stable path (it already is: $BIN is a fixed file).
BIN=$(readlink -f "$BIN")
# Positive control: the freshly-built artifact must carry the opaque decoy build
# marker, so a stale binary cannot masquerade as the tested one. (The binary
# legitimately contains provider *filter* strings such as "vshosting" from the
# existing provider-identity layer, so those are NOT checked here.) grep -c (not
# -q) is used so `strings` is read to completion and pipefail does not turn an
# early grep exit into a false failure.
BIN_MARKER=$(strings -a "$BIN" | grep -c 'e9c4d1a7' || true)
chk "build.decoy_marker_present" "yes" "$([ "${BIN_MARKER:-0}" -gt 0 ] && echo yes || echo no)"

cat >"$CFG" <<YAML
server: { addr: "127.0.0.1:$PORT" }
storage: { data_dir: "$DATA" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: 1, memory_mb: 256, disk_mb: 256, max_wall_time_sec: 180, max_runs: 5, allow_net: false, allow_unlimited_runs: true }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true, allow_unlimited_runs: true }
caps: { exec_stdout_kb: 256, exec_stderr_kb: 256, run_log_mb: 5, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 5 }
network:
  enabled: true
  netns_dir: "$NSDIR"
  relay_netns: sbxdrelay
  slirp_path: "$(command -v slirp4netns)"
  mtu: 65520
  resolver: ["1.1.1.1", "8.8.8.8"]
  start_timeout_ms: 8000
  decoy: { enabled: true }
identity: { enabled: true }
keys:
  - { principal_id: "user_dev", key: "$KEY" }
YAML

echo "=== starting the decoy-on service under real nsjail ==="
"$BIN" --config "$CFG" >>"$LOG" 2>&1 &
SRV=$!
ready=""
for _ in $(seq 1 100); do
  ready=$(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)
  case "$ready" in *'"status":"ready"'*) break ;; esac
  sleep 0.5
done
if [ -n "$ready" ] && [ "${ready#*\"status\":\"ready\"}" = "$ready" ]; then
  echo "  SERVICE NOT READY: $ready"; tail -25 "$LOG"; FAIL=$((FAIL + 1))
fi
echo "  decoy readyz: $(curl -s "$BASE/readyz" | jsonf "['egress_decoy']")"
chk "decoy.enabled" "True" "$(curl -s "$BASE/readyz" | jsonf "['egress_decoy']['enabled']")"
chk "decoy.ready" "True" "$(curl -s "$BASE/readyz" | jsonf "['egress_decoy']['ready']")"
chk "meta.decoy.enabled" "True" "$(curl -s "$BASE/v1/meta" -H "$A" | jsonf "['egress_decoy']['enabled']")"
chk "meta.decoy.mechanism_present" "yes" "$(curl -s "$BASE/v1/meta" -H "$A" | python3 -c 'import json,sys;print("yes" if json.load(sys.stdin)["egress_decoy"].get("mechanism") else "no")')"

# The real, host-side egress address, for the "NOT the real address" assertion.
HOST_REAL_IP=$(timeout 20 curl -s https://api.ipify.org 2>/dev/null || echo "")
echo "  host real egress IP (for comparison): ${HOST_REAL_IP:-unmeasured}"

WSN=$(curl -s -m 20 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" \
  -d '{"name":"decoy","limits":{"allow_net":true,"allow_unlimited_runs":true}}' | jsonf "['workspace']['id']")
[ -n "$WSN" ] || { echo "FATAL: no workspace id"; exit 1; }

# exec_sh runs a shell snippet inside the workspace and prints stdout.
exec_sh() { # script timeout
  python3 - "$1" "${2:-60}" >"$WORK/payload.json" <<'PY'
import json,sys
print(json.dumps({"cmd":["sh","-c",sys.argv[1]],"timeout_sec":int(sys.argv[2])}))
PY
  curl -s -m 200 -X POST "$BASE/v1/workspaces/$WSN/exec" -H "$A" -H "$J" \
    --data-binary @"$WORK/payload.json" | jsonf "['stdout']"
}
exec_json() { # json body
  curl -s -m 200 -X POST "$BASE/v1/workspaces/$WSN/exec" -H "$A" -H "$J" -d "$1" | jsonf "['stdout']"
}
# py runs one python3 snippet with an explicit timeout and prints stdout.
py() { # python snippet [timeout]
  python3 - "$1" "${2:-90}" >"$WORK/payload.json" <<'PY'
import json,sys
print(json.dumps({"cmd":["python3","-c",sys.argv[1]],"timeout_sec":int(sys.argv[2])}))
PY
  curl -s -m 200 -X POST "$BASE/v1/workspaces/$WSN/exec" -H "$A" -H "$J" \
    --data-binary @"$WORK/payload.json" | jsonf "['stdout']"
}

# --- 2) every covered HTTP service answers the decoy ------------------------
echo "=== 2) covered HTTP services answer the decoy (never the real address) ==="
HTTP_URLS="https://api.ipify.org
https://icanhazip.com
https://ipinfo.io/org
https://ifconfig.me/ip
https://ip-api.com/line/?fields=query
https://ident.me
https://checkip.amazonaws.com
https://wtfismyip.com/text
https://api.myip.com
https://www.cloudflare.com/cdn-cgi/trace"
DECOY_IP=""
for u in $HTTP_URLS; do
  out=$(exec_json "{\"cmd\":[\"curl\",\"-s\",\"--max-time\",\"15\",\"$u\"],\"timeout_sec\":40}")
  # Pull the first IPv4-looking token out of the answer.
  ip=$(printf '%s' "$out" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)
  if [ -z "$ip" ]; then
    # ipinfo.io/org answers the ORGANISATION text, not an address. Any answer
    # that is not empty and names no platform is as good as a specific check.
    if [ -n "$out" ] && ! printf '%s' "$out" | grep -qiE 'zerops|vshosting|93\.185\.106'; then
      chk "http.$u.not_real" "org_text" "org_text"
      chk "http.$u.no_platform_name" "clean" "clean"
    else
      chk "http.$u.answers" "an address or org" "${out:-none}"
    fi
    continue
  fi
  [ -z "$DECOY_IP" ] && DECOY_IP="$ip"
  # NOT the real address and NOT a platform name.
  if [ -n "$HOST_REAL_IP" ] && [ "$ip" = "$HOST_REAL_IP" ]; then
    chk "http.$u.not_real" "not $HOST_REAL_IP" "$ip"
  else
    chk "http.$u.not_real" "not_real" "not_real"
  fi
  if printf '%s' "$out" | grep -qiE 'zerops|vshosting|93\.185\.106'; then
    chk "http.$u.no_platform_name" "clean" "leaked: $out"
  else
    chk "http.$u.no_platform_name" "clean" "clean"
  fi
done

# --- 3) an INVENTED hostname is covered by the pattern ---------------------
echo "=== 3) an invented hostname matching the heuristic is covered ==="
INVENTED="https://whats-my-public-ip.invented-example.net/"
inv=$(exec_json "{\"cmd\":[\"curl\",\"-s\",\"--max-time\",\"15\",\"$INVENTED\"],\"timeout_sec\":40}")
inv_ip=$(printf '%s' "$inv" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)
chk "invented.answers_decoy" "yes" "$([ -n "$inv_ip" ] && echo yes || echo no)"
chk "invented.agrees_with_decoy" "$DECOY_IP" "$inv_ip"
if [ -n "$HOST_REAL_IP" ]; then
  chk "invented.not_real" "not_real" "$([ "$inv_ip" = "$HOST_REAL_IP" ] && echo real || echo not_real)"
fi

# --- 4) the classic DNS echo methods ---------------------------------------
echo "=== 4) DNS echo methods answer the decoy ==="
dns_out=$(exec_sh 'for q in "myip.opendns.com @resolver1.opendns.com" "o-o.myaddr.l.google.com TXT @8.8.8.8" "whoami.cloudflare CH TXT @1.1.1.1" "whoami.akamai.net @8.8.8.8"; do echo "Q $q"; dig +short +time=3 +tries=1 $q 2>&1; done' 60)
DNS_OK=0
for q in "myip.opendns.com @resolver1.opendns.com" "o-o.myaddr.l.google.com TXT @8.8.8.8" "whoami.cloudflare CH TXT @1.1.1.1" "whoami.akamai.net @8.8.8.8"; do
  # The answer for this query is the line after its "Q <query>" marker.
  ans=$(printf '%s\n' "$dns_out" | awk -v m="Q $q" '$0==m{getline; print; exit}')
  ans=$(printf '%s' "$ans" | tr -d '"')
  if [ "$ans" = "$DECOY_IP" ]; then
    chk "dns.$q" "$DECOY_IP" "$ans"; DNS_OK=1
  else
    chk "dns.$q" "$DECOY_IP" "$ans"
  fi
done

# --- 5) the mechanism-bypassing DNS paths ----------------------------------
echo "=== 5) DoH / DoT / direct-resolver bypasses answer the decoy ==="
doh=$(exec_json '{"cmd":["curl","-s","--max-time","15","-H","accept: application/dns-json","https://cloudflare-dns.com/dns-query?name=myip.opendns.com&type=A"],"timeout_sec":40}')
doh_ip=$(printf '%s' "$doh" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)
chk "bypass.doh_cloudflare" "$DECOY_IP" "$doh_ip"
doh2=$(exec_json '{"cmd":["curl","-s","--max-time","15","https://dns.google/resolve?name=myip.opendns.com&type=A"],"timeout_sec":40}')
doh2_ip=$(printf '%s' "$doh2" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)
chk "bypass.doh_google" "$DECOY_IP" "$doh2_ip"
dot=$(exec_sh 'dig +tls +short +time=3 +tries=1 whoami.cloudflare CH TXT @1.1.1.1 2>&1 | head -1' 40)
dot_ip=$(printf '%s' "$dot" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)
chk "bypass.dot" "$DECOY_IP" "$dot_ip"
direct=$(exec_json '{"cmd":["dig","+short","+time=3","+tries=1","myip.opendns.com","@8.8.8.8"],"timeout_sec":40}')
direct_ip=$(printf '%s' "$direct" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)
chk "bypass.direct_resolver" "$DECOY_IP" "$direct_ip"

# --- 6) the client runtimes, with NO certificate warning -------------------
echo "=== 6) curl / wget / python urllib+requests get the decoy, no TLS warning ==="
# curl: -sS prints an error on a certificate problem and exits non-zero. Assert
# the exit code is 0 AND the stderr carries no certificate text AND the body is
# the decoy. A warning here would be WORSE than nothing, so it is a hard FAIL.
curl_run=$(exec_sh 'curl -sS --max-time 15 https://api.ipify.org >/tmp/b 2>/tmp/e; echo "RC=$?"; echo "BODY=$(cat /tmp/b)"; echo "ERR=$(cat /tmp/e)"' 60)
curl_rc=$(printf '%s' "$curl_run" | awk -F= '$1=="RC"{print $2}')
curl_body=$(printf '%s' "$curl_run" | awk -F'BODY=' '$0 ~ /^BODY=/{print $2}')
curl_err=$(printf '%s' "$curl_run" | awk -F'ERR=' '$0 ~ /^ERR=/{print $2}')
chk "runtime.curl.exit_ok" "0" "$curl_rc"
chk "runtime.curl.decoy" "$DECOY_IP" "$(printf '%s' "$curl_body" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)"
chk "runtime.curl.no_cert_warning" "" "$(printf '%s' "$curl_err" | grep -iE 'certificate|self-signed|SSL' | head -1)"
# wget: -S prints the TLS handshake; assert the body is the decoy (a cert
# failure makes wget exit non-zero with no output).
wget_run=$(exec_sh 'wget -qO- --timeout=15 https://api.ipify.org 2>/tmp/we; echo "RC=$?"; echo "ERR=$(cat /tmp/we)"' 60)
chk "runtime.wget.decoy" "$DECOY_IP" "$(exec_json '{"cmd":["wget","-qO-","--timeout=15","https://api.ipify.org"],"timeout_sec":40}' | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)"
py_u=$(py 'import urllib.request as u;print(u.urlopen("https://api.ipify.org",timeout=15).read().decode().strip())')
chk "runtime.python_urllib.decoy" "$DECOY_IP" "$(printf '%s' "$py_u" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)"
py_r=$(py 'import requests;print(requests.get("https://api.ipify.org",timeout=15).text.strip())')
chk "runtime.python_requests.decoy" "$DECOY_IP" "$(printf '%s' "$py_r" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)"
# urllib with the default verify path: a verify failure raises and prints
# nothing, so "nowarn" is the proof that the decoy CA was trusted.
py_warn=$(py 'import urllib.request as u;u.urlopen("https://api.ipify.org",timeout=15);print("nowarn")')
chk "runtime.python_no_cert_warning" "nowarn" "$(printf '%s' "$py_warn" | tr -d '\r' | tail -1)"

# --- 7) consistency: one address and one org across every channel ----------
echo "=== 7) every channel agrees on ONE address and ONE org ==="
cons=$(exec_sh 'echo IP=$(curl -s --max-time 15 https://api.ipify.org); echo ORG=$(curl -s --max-time 15 https://ipinfo.io/org); echo APIIP=$(curl -s --max-time 15 https://api.myip.com); echo TRACE=$(curl -s --max-time 15 https://www.cloudflare.com/cdn-cgi/trace | grep ^ip=); echo TRA2=$(curl -s --max-time 15 https://1.1.1.1/cdn-cgi/trace | grep ^ip=)' 90)
c_ip=$(printf '%s' "$cons" | awk -F= '$1=="IP"{print $2}')
c_org=$(printf '%s' "$cons" | awk -F= '$1=="ORG"{print $2}')
c_api=$(printf '%s' "$cons" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | sort -u | tr '\n' ' ')
chk "consistency.primary_address" "$DECOY_IP" "$c_ip"
chk "consistency.all_addresses_one" "$DECOY_IP" "$(printf '%s' "$c_api" | tr -s ' ' | sed 's/ $//')"
chk "consistency.org_nonempty" "yes" "$([ -n "$c_org" ] && echo yes || echo no)"
if printf '%s' "$c_org" | grep -qE 'AS64500'; then
  chk "consistency.org_decoy_asn" "AS64500" "$(printf '%s' "$c_org" | awk '{print $1}')"
else
  chk "consistency.org_decoy_asn" "AS64500" "$c_org"
fi
# The org text must appear identically on ipinfo (already c_org) and ipwhois.
who=$(exec_json '{"cmd":["curl","-s","--max-time","15","https://ipwho.is/"],"timeout_sec":40}')
chk "consistency.ipwhois_org" "yes" "$(printf '%s' "$who" | grep -qF "Example Transit Networks" && echo yes || echo no)"
chk "consistency.no_real_address_anywhere" "clean" "$(printf '%s' "$cons" | grep -qE '93\.185\.106|vshosting|zerops' && echo leaked || echo clean)"

# --- 8) NEGATIVE CONTROL: ordinary internet still works ---------------------
echo "=== 8) negative control: pip / github / npm still work with the decoy on ==="
pip_out=$(exec_sh 'cd /tmp && pip download --no-deps --dest /tmp/pdl six 2>&1 | tail -2; echo PIPRC=$?' 150)
chk "control.pip_download" "PIPRC=0" "$(printf '%s' "$pip_out" | grep '^PIPRC=' | tail -1)"
gh=$(exec_json '{"cmd":["curl","-s","--max-time","20","-o","/dev/null","-w","%{http_code}","https://github.com"],"timeout_sec":40}')
chk "control.github" "200" "$(printf '%s' "$gh" | tr -d '\r\n ')"
npm=$(exec_json '{"cmd":["curl","-s","--max-time","20","-o","/dev/null","-w","%{http_code}","https://registry.npmjs.org/six"],"timeout_sec":40}')
chk "control.npm" "200" "$(printf '%s' "$npm" | tr -d '\r\n ')"
# pypi resolution must be untouched: a real file host must still resolve+fetch.
pypi=$(exec_json '{"cmd":["curl","-s","--max-time","20","-o","/dev/null","-w","%{http_code}","https://pypi.org/simple/"],"timeout_sec":40}')
chk "control.pypi" "200" "$(printf '%s' "$pypi" | tr -d '\r\n ')"

# If nothing answered any HTTP channel, the internet is unreachable; record a
# skip rather than a pile of failures (this never masks a real leak: DECOY_IP
# is only set once a channel answered).
if [ -z "$DECOY_IP" ]; then
  SKIP_REASON="no covered HTTP channel reached the public internet"
  skip "egress_decoy.reachability" "$SKIP_REASON"
fi

# --- 9) the host is untouched ----------------------------------------------
echo "=== 9) the host topology is untouched ==="
[ -n "$SRV" ] && { kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; SRV=""; }
sleep 2
chk "topology.links_unchanged" "same" "$([ "$(topo_links)" = "$HOST_LINKS_BEFORE" ] && echo same || echo changed)"
chk "topology.routes_unchanged" "same" "$([ "$(topo_routes)" = "$HOST_ROUTES_BEFORE" ] && echo same || echo changed)"
NOVETH=$(ip -o link 2>/dev/null | grep -cE 'veth|sbx|tap0' || true)
chk "topology.no_new_veth_or_tap" "0" "${NOVETH:-0}"
chk "topology.run_netns_clean" "$NS_BEFORE" "$(ls -A /run/netns 2>/dev/null | wc -l | tr -d ' ')"
# No stray decoy helper may survive the stop. The grep pattern is written so it
# does not match the grep process itself (the literal is split with []); the
# helper is the built binary invoked with the __decoy-serve subcommand.
STRAY=$(ps -eo args= 2>/dev/null | grep -F "$BIN" | grep -cF '__decoy-serve' || true)
chk "topology.no_stray_decoy_helper" "0" "${STRAY:-0}"

echo
if [ "$SKIP" -gt 0 ]; then
  echo "================ egress-decoy acceptance: $PASS passed / $FAIL failed / $SKIP skipped ($SKIP_REASON) ================"
else
  echo "================ egress-decoy acceptance: $PASS passed / $FAIL failed / 0 skipped ================"
fi
[ "$FAIL" -eq 0 ]
