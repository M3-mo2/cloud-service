#!/usr/bin/env bash
# tunnel-check.sh -- real acceptance for per-workspace egress and quick tunnels.
#
# WHY THIS EXISTS
#   allow_net used to mean "share the host network namespace", which let a
#   workspace bind host ports and reach the operator's LAN. The replacement is a
#   private namespace per workspace, connected to the public internet ONLY
#   through chained slirp4netns instances over UNIX sockets (zero host
#   interfaces, zero host routes). This script runs the real service against the
#   host's REAL nsjail, slirp4netns and cloudflared and asserts the whole
#   contract, including that the host's interface/route topology is byte-for-byte
#   unchanged.
#
# WHAT IT ASSERTS (every check is hard; the script exits non-zero on any failure,
# and no assertion is ever weakened to make a run pass):
#   1. an allow_net:true workspace resolves DNS, fetches https://pypi.org/simple/
#      and installs a Python package;
#   2. the same workspace CANNOT reach 10.9.112.7:22, 10.9.112.1:53 or another
#      workspace's port;
#   3. an allow_net:false workspace reaches nothing;
#   4. POST .../tunnels {port:5000} returns a trycloudflare.com URL, and fetching
#      that URL from the host serves a page from an app started on 5000 inside
#      the workspace;
#   5. DELETE .../tunnels/{id} → 204 and the URL stops working;
#   6. 403/404/400 on the documented paths, and the documented 5xx
#      (tunnel_url_unavailable) when no URL is observed;
#   7. the tunnel dies with its workspace and the cloudflared process is gone;
#   8. zero host topology: ip -o link shows no new interface, ip route is
#      unchanged, and /run/netns is empty before and after.
#
# THE THIRD STATE: skipped-environment.
#   Checks 4, 5 and 7 depend on Cloudflare's quick-tunnel provisioning, which is
#   UNAUTHENTICATED and RATE LIMITED. When the shared quota is spent cloudflared
#   prints, verbatim, "quick tunnel provisioning failed with status 429" and
#   exits; the engine surfaces that honestly as 504 tunnel_url_unavailable with
#   cloudflared's own text in details.reason. The harness matches THAT message
#   (never a timeout) to tell "somebody else's quota ran out" apart from "our
#   engine is broken". On a match it retries with bounded backoff (TUNNEL_RETRIES
#   attempts, TUNNEL_BACKOFF seconds base) and, if the limit persists, records
#   the affected assertions as SKIP with the reason and still exits 0: a suite
#   that goes red because an external quota ran out trains people to ignore it.
#   Any OTHER tunnel failure stays a hard FAIL — the skip is never blanket. The
#   count of skipped checks and the reason appear in the one-line tally, e.g.
#   "24 passed / 0 failed / 3 skipped (cloudflare quick-tunnel rate limit)".
#
# REQUIREMENTS
#   Root (nsjail needs cgroup v2 and mount namespaces; slirp4netns must create
#   the engine namespaces), the real nsjail binary, slirp4netns, cloudflared, the
#   Go toolchain, python3 and curl. `sudo` resets PATH, so the toolchain is
#   resolved explicitly.
#
# USAGE
#   sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/tunnel-check.sh
#   Environment: PORT (default 8099), PORT2 (the no-URL instance, default 8098),
#     TUNNEL_RETRIES (default 3), TUNNEL_BACKOFF (default 5 seconds).
#
# EXIT
#   0 when every assertion passes, or when only tunnel-dependent assertions were
#   skipped for the external rate limit; non-zero on any real failure. When root,
#   nsjail, slirp4netns or cloudflared is absent the script prints SKIP and exits
#   0: it cannot prove anything without them.
set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/tunnel-check.sh

Real acceptance harness for per-workspace egress and quick Cloudflare tunnels.
It builds sandboxd, runs it under the host's real nsjail, and asserts the
egress/isolation contract, the tunnel API, and that host topology is unchanged.

Environment:
  PORT            port for the main service (default 8099).
  PORT2           port for the no-URL service instance (default 8098).
  TUNNEL_RETRIES  attempts for a tunnel create while rate limited (default 3).
  TUNNEL_BACKOFF  base seconds between retries; attempt N waits N*this (default 5).

Tunnel-dependent assertions are reported as SKIP (a third state, exit 0) when
Cloudflare's quick-tunnel quota is spent (429), and as FAIL for any other cause.
USAGE
}
case "${1:-}" in
  -h | --help) usage; exit 0 ;;
esac

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PORT=${PORT:-8099}
PORT2=${PORT2:-8098}
DATA=$(mktemp -d)
DATA2=$(mktemp -d)
NSDIR="$DATA/netns"
NSDIR2="$DATA2/netns"
CFG="$DATA/config.yaml"
CFG2="$DATA2/config.yaml"
BIN=$(mktemp)
LOG=$(mktemp)
LOG2=$(mktemp)
SRV=""
SRV2=""
KEY=dev-key-please-rotate
OTHER_KEY=other-key
A="Authorization: Bearer $KEY"
B="Authorization: Bearer $OTHER_KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
BASE2="http://127.0.0.1:$PORT2"
PASS=0
FAIL=0
SKIP=0
SKIP_REASON=""
CF_LIMITED=no
URL_OK=no
WORK=$(mktemp -d)
MARK="tunnelcheck-$$"

chk() { # label expected actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}
# skip records a tunnel-dependent assertion that COULD NOT be exercised because
# an external dependency (Cloudflare's quick-tunnel quota) was unavailable. It is
# a distinct third state: not a pass (nothing was proved) and not a fail (the
# engine is not at fault). The suite exits 0 on skips.
skip() { # label reason
  echo "  SKIP  $1 ($2)"; SKIP=$((SKIP + 1))
}
jsonf() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)"; }

# Cloudflare quick-tunnel provisioning is rate limited. cloudflared prints the
# exact line "quick tunnel provisioning failed with status 429" and exits; the
# engine surfaces that as 504 tunnel_url_unavailable with cloudflared's own
# output in details.reason. THAT message -- matched literally, never a timeout --
# is the signal that the external quota is spent, so the tunnel-dependent
# assertions become skipped-environment rather than failures. Any other 504
# reason (a bug, a missing binary, a crash) stays a hard failure; nothing is
# blanket-skipped.
CF_RATELIMIT_RE='provisioning failed with status 429'
is_rate_limited() { printf '%s' "$1" | grep -qE "$CF_RATELIMIT_RE"; }

TUNNEL_RETRIES=${TUNNEL_RETRIES:-3}
TUNNEL_BACKOFF=${TUNNEL_BACKOFF:-5}

# create_tunnel_retry POSTs a tunnel create for a workspace/port and retries ONLY
# while cloudflared reports the 429 rate limit, with bounded backoff. It sets
# TUN_BODY to the last response body and returns:
#   0 -> a tunnel with a usable trycloudflare URL was created
#   1 -> provisioning stayed rate limited (external; caller skips those checks)
#   2 -> the create failed for any other reason (caller must FAIL)
create_tunnel_retry() { # ws port
  local ws="$1" port="$2" attempt=0 body url reason wait
  while :; do
    attempt=$((attempt + 1))
    body=$(curl -s -m 150 -X POST "$BASE/v1/workspaces/$ws/tunnels" -H "$A" -H "$J" -d "{\"port\":$port}")
    TUN_BODY="$body"
    url=$(printf '%s' "$body" | jsonf "['tunnel']['url']" 2>/dev/null || echo "")
    case "$url" in https://*.trycloudflare.com) return 0 ;; esac
    reason=$(printf '%s' "$body" | jsonf "['error']['details'].get('reason','')" 2>/dev/null || echo "")
    if is_rate_limited "$reason"; then
      if [ "$attempt" -ge "$TUNNEL_RETRIES" ]; then
        echo "  cloudflare quick-tunnel rate limit persisted after $attempt attempt(s)"
        return 1
      fi
      wait=$((TUNNEL_BACKOFF * attempt))
      echo "  cloudflare 429 on attempt $attempt/$TUNNEL_RETRIES; retrying in ${wait}s"
      sleep "$wait"
      continue
    fi
    # Not the rate limit: a genuine failure the caller must surface.
    return 2
  done
}

# host topology snapshots
topo_links() { ip -o link | awk '{print $2,$9}' | sort; }
topo_routes() { ip route | sort; }
HOST_LINKS_BEFORE=$(topo_links)
HOST_ROUTES_BEFORE=$(topo_routes)

# put_py writes a python file into a workspace.
put_py() { # key ws path content
  python3 - "$4" >"$WORK/payload.json" <<'PY'
import json,sys
print(json.dumps({"content":sys.argv[1]}))
PY
  curl -s -o /dev/null -m 30 -X PUT "$BASE/v1/workspaces/$2/files/content?path=$3" -H "$1" -H "$J" --data-binary @"$WORK/payload.json"
}
# exec_py runs a python file inside a workspace and prints the exec JSON.
exec_py() { # key ws path timeout
  curl -s -m 90 -X POST "$BASE/v1/workspaces/$2/exec" -H "$1" -H "$J" \
    -d "{\"cmd\":[\"python3\",\"${3#/}\"],\"timeout_sec\":${4:-40}}"
}

cleanup() {
  [ -n "$SRV" ] && { kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; }
  [ -n "$SRV2" ] && { kill "$SRV2" 2>/dev/null; wait "$SRV2" 2>/dev/null; }
  sleep 1
  # Stop only the quick-tunnel cloudflared processes this harness started.
  for p in $(pgrep -f 'cloudflared tunnel --no-autoupdate --url' 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  # Remove the engine's own namespaces under the two private netns dirs.
  for d in "$NSDIR" "$NSDIR2"; do
    for n in $(ls "$d" 2>/dev/null); do umount "$d/$n" 2>/dev/null; rm -f "$d/$n"; done
  done
  # Remove this run's workspace parent cgroups (ws-*) and nested leaves; a
  # non-empty cgroup is left alone.
  for d in $(ls -d /sys/fs/cgroup/ws-*/NSJAIL.* 2>/dev/null); do
    [ -s "$d/cgroup.procs" ] || rmdir "$d" 2>/dev/null
  done
  for d in $(ls -d /sys/fs/cgroup/ws-* 2>/dev/null); do
    [ -s "$d/cgroup.procs" ] || rmdir "$d" 2>/dev/null
  done
  rm -rf "$DATA" "$DATA2" "$BIN" "$LOG" "$LOG2" "$WORK"
}
trap cleanup EXIT INT TERM

if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root; re-run with: sudo -n env \"PATH=\$PATH:/usr/local/go/bin:/usr/local/bin\" bash scripts/tunnel-check.sh"
  exit 0
fi
command -v nsjail >/dev/null || { echo "SKIP: nsjail is not installed on this host"; exit 0; }
command -v slirp4netns >/dev/null || { echo "SKIP: slirp4netns is not installed on this host"; exit 0; }
command -v cloudflared >/dev/null || { echo "SKIP: cloudflared is not installed on this host"; exit 0; }
if ! getent group sandbox >/dev/null; then
  groupadd -r sandbox && useradd -r -g sandbox -M -s /usr/sbin/nologin sandbox
fi

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1

echo "=== host topology before: $(ls -A "$NSDIR" 2>/dev/null | wc -l | tr -d ' ') netns entries ==="

start_service() { # bin cfg log url out; prints pid
  "$1" --config "$2" >>"$3" 2>&1 &
  local pid=$!
  local ready=""
  for _ in $(seq 1 80); do
    ready=$(curl -s -m 2 "$4/readyz" 2>/dev/null || true)
    case "$ready" in *'"status":"ready"'*) printf '%s' "$pid"; return 0 ;; esac
    sleep 0.5
  done
  echo "  SERVICE NOT READY on $4: $ready" >&2
  tail -25 "$3" >&2
  printf '%s' "$pid"
  return 1
}

write_cfg() { # path data netnsdir relay port url_timeout
  cat >"$1" <<YAML
server: { addr: "127.0.0.1:$5" }
storage: { data_dir: "$2" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: 1, memory_mb: 512, disk_mb: 256, max_wall_time_sec: 600, max_runs: 5, allow_net: false, allow_unlimited_runs: true, allow_tunnels: true }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true, allow_unlimited_runs: true, allow_tunnels: true }
caps: { exec_stdout_kb: 256, exec_stderr_kb: 256, run_log_mb: 5, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 5 }
network: { enabled: true, netns_dir: "$3", relay_netns: "$4", slirp_path: "$(command -v slirp4netns)", mtu: 65520, resolver: ["1.1.1.1","8.8.8.8"], start_timeout_ms: 8000 }
tunnels: { enabled: true, max_per_workspace: 2, url_timeout_sec: $6, cloudflared_path: "$(command -v cloudflared)" }
sessions: { max_per_workspace: 4, idle_timeout_sec: 600, lifetime_sec: 1800, start_probe_ms: 800 }
keys:
  - { principal_id: "user_dev", key: "$KEY" }
  - { principal_id: "user_other", key: "$OTHER_KEY" }
YAML
}

write_cfg "$CFG" "$DATA" "$NSDIR" sbxdrelay "$PORT" 90
echo "=== starting the service under real nsjail ==="
SRV=$(start_service "$BIN" "$CFG" "$LOG" "$BASE") || exit 1
echo "  service started (pid $SRV)"

# ---- 1) allow_net egress: DNS, pypi, package install -----------------------
echo "=== 1) allow_net workspace: DNS + pypi + package install ==="
WSA=$(curl -s -m 20 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" \
  -d '{"name":"egress","limits":{"allow_net":true,"allow_tunnels":true,"allow_unlimited_runs":true}}' | jsonf "['workspace']['id']")
[ -n "$WSA" ] || { echo "FATAL: no workspace A id"; exit 1; }
echo "  workspace A=$WSA"

put_py "$A" "$WSA" /net.py 'import socket, urllib.request
print("DNS", socket.gethostbyname("pypi.org"))
print("HTTP", urllib.request.urlopen("https://pypi.org/simple/", timeout=25).status)'
OUT=$(exec_py "$A" "$WSA" /net.py 50)
STDOUT=$(printf '%s' "$OUT" | jsonf "['stdout']")
chk "egress.dns_resolves" "yes" "$(printf '%s' "$STDOUT" | grep -q '^DNS ' && echo yes || echo no)"
chk "egress.pypi_http" "yes" "$(printf '%s' "$STDOUT" | grep -q 'HTTP 200' && echo yes || echo no)"
INST=$(curl -s -m 180 -X POST "$BASE/v1/workspaces/$WSA/packages/install" -H "$A" -H "$J" -d '{"packages":["six"]}')
chk "egress.package_installs" "0" "$(printf '%s' "$INST" | jsonf "['exit_code']")"

# ---- 2) isolation: cannot reach the host LAN or another workspace ----------
echo "=== 2) isolation: host LAN + another workspace unreachable ==="
put_py "$A" "$WSA" /lan.py 'import socket
for h, p in (("10.9.112.7", 22), ("10.9.112.1", 53), ("10.9.112.4", 443)):
    s = socket.socket(); s.settimeout(5)
    try:
        s.connect((h, p)); print("RESULT", h, p, "CONNECTED")
    except Exception:
        print("RESULT", h, p, "blocked")
    finally:
        s.close()'
LANSTDOUT=$(exec_py "$A" "$WSA" /lan.py 30 | jsonf "['stdout']")
chk "isolation.host_22_blocked" "blocked" "$(printf '%s' "$LANSTDOUT" | awk '$1=="RESULT" && $2=="10.9.112.7"{print $4}')"
chk "isolation.host_dns_blocked" "blocked" "$(printf '%s' "$LANSTDOUT" | awk '$1=="RESULT" && $2=="10.9.112.1"{print $4}')"
chk "isolation.control_443_blocked" "blocked" "$(printf '%s' "$LANSTDOUT" | awk '$1=="RESULT" && $2=="10.9.112.4"{print $4}')"

# A second allow_net workspace with its own server; wsA must not reach it.
WSB=$(curl -s -m 20 -X POST "$BASE/v1/workspaces" -H "$B" -H "$J" \
  -d '{"name":"other","limits":{"allow_net":true,"allow_tunnels":true,"allow_unlimited_runs":true}}' | jsonf "['workspace']['id']")
curl -s -o /dev/null -m 60 -X PUT "$BASE/v1/workspaces/$WSB/files/content?path=/x.txt" -H "$B" -H "$J" -d '{"content":"other\n"}'
curl -s -o /dev/null -m 30 -X POST "$BASE/v1/workspaces/$WSB/runs" -H "$B" -H "$J" \
  -d '{"kind":"command","argv":["python3","-m","http.server","6000","--bind","0.0.0.0"],"cwd":"/","no_timeout":true}'
sleep 3
PIDB=$(curl -s -H "$B" "$BASE/v1/workspaces/$WSB/processes" | python3 -c '
import json,sys
for p in json.load(sys.stdin).get("processes",[]):
    if any("http.server" in a for a in p.get("argv",[])):
        print(p["pid"]); break
')
WSB_ADDR=""
NSB=""
if [ -n "$PIDB" ]; then
  IDB=$(readlink "/proc/$PIDB/ns/net" 2>/dev/null | tr -dc '0-9')
  for f in "$NSDIR"/sbxdws-*; do
    [ -e "$f" ] || continue
    if [ "$(stat -c '%i' "$f" 2>/dev/null)" = "$IDB" ]; then NSB="$f"; break; fi
  done
  [ -n "$NSB" ] && WSB_ADDR=$(nsenter --net="$NSB" ip -o -4 addr show tap0 2>/dev/null | awk '{print $4}' | cut -d/ -f1)
fi
echo "  workspace B server ns=${NSB:-<not found>} addr=${WSB_ADDR:-<none>}"
if [ -n "$WSB_ADDR" ]; then
  put_py "$A" "$WSA" /cross.py "import socket
s = socket.socket(); s.settimeout(5)
try:
    s.connect((\"$WSB_ADDR\", 6000)); print(\"RESULT CONNECTED\")
except Exception:
    print(\"RESULT blocked\")
finally:
    s.close()"
  chk "isolation.other_workspace_blocked" "blocked" "$(exec_py "$A" "$WSA" /cross.py 20 | jsonf "['stdout']" | awk '{print $2}' | tr -d '\n')"
else
  chk "isolation.other_workspace_probed" "yes" "no"
fi

# ---- 3) allow_net:false reaches nothing ------------------------------------
echo "=== 3) allow_net:false workspace reaches nothing ==="
WSN=$(curl -s -m 20 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" -d '{"name":"nonet"}' | jsonf "['workspace']['id']")
put_py "$A" "$WSN" /off.py 'import socket
for h, p in (("10.9.112.7", 22), ("1.1.1.1", 80)):
    s = socket.socket(); s.settimeout(4)
    try:
        s.connect((h, p)); print("RESULT", h, "CONNECTED")
    except Exception:
        print("RESULT", h, "blocked")
    finally:
        s.close()'
OFFSTDOUT=$(exec_py "$A" "$WSN" /off.py 25 | jsonf "['stdout']")
chk "no_net.host_22_blocked" "blocked" "$(printf '%s' "$OFFSTDOUT" | awk '$1=="RESULT" && $2=="10.9.112.7"{print $3}')"
chk "no_net.public_ip_blocked" "blocked" "$(printf '%s' "$OFFSTDOUT" | awk '$1=="RESULT" && $2=="1.1.1.1"{print $3}')"

# ---- 4) tunnel: create, observe URL, fetch publicly ------------------------
echo "=== 4) create a tunnel and fetch it publicly ==="
curl -s -o /dev/null -m 30 -X PUT "$BASE/v1/workspaces/$WSA/files/content?path=/index.html" -H "$A" -H "$J" \
  -d "{\"content\":\"$MARK\n\"}"
curl -s -o /dev/null -m 30 -X POST "$BASE/v1/workspaces/$WSA/runs" -H "$A" -H "$J" \
  -d '{"kind":"command","argv":["python3","-m","http.server","5000","--bind","0.0.0.0"],"cwd":"/","no_timeout":true}'
sleep 3
INSIDE=$(curl -s -m 40 -X POST "$BASE/v1/workspaces/$WSA/exec" -H "$A" -H "$J" \
  -d '{"cmd":["python3","-c","import urllib.request;print(urllib.request.urlopen(\"http://127.0.0.1:5000/index.html\",timeout=5).read().decode().strip())"],"timeout_sec":15}' | jsonf "['stdout']" | tr -d '\n')
chk "tunnel.app_listens_inside" "$MARK" "$INSIDE"

CREATE_RC=0
create_tunnel_retry "$WSA" 5000 || CREATE_RC=$?
TID=""
URL=""
if [ "$CREATE_RC" = "0" ]; then
  TID=$(printf '%s' "$TUN_BODY" | jsonf "['tunnel']['id']" 2>/dev/null || echo "")
  URL=$(printf '%s' "$TUN_BODY" | jsonf "['tunnel']['url']" 2>/dev/null || echo "")
  case "$URL" in https://*.trycloudflare.com) URL_OK=yes ;; *) URL_OK=no ;; esac
  chk "tunnel.url_is_trycloudflare" "yes" "$URL_OK"
  echo "  url=$URL"
  PUBOK=no
  for _ in $(seq 1 40); do
    BODY=$(timeout 25 curl -s -L -m 20 "$URL/" 2>/dev/null)
    printf '%s' "$BODY" | grep -q "$MARK" && { PUBOK=yes; break; }
    sleep 3
  done
  chk "tunnel.public_fetch_serves_workspace" "yes" "$PUBOK"
elif [ "$CREATE_RC" = "1" ]; then
  # Cloudflare's quick-tunnel quota is spent. Nothing about the engine is
  # broken, so this is a skipped-environment state, not a failure.
  CF_LIMITED=yes
  SKIP_REASON="cloudflare quick-tunnel rate limit (HTTP 429 from provisioning)"
  echo "  $SKIP_REASON; tunnel-dependent assertions will be SKIPPED"
  skip "tunnel.url_is_trycloudflare" "$SKIP_REASON"
  skip "tunnel.public_fetch_serves_workspace" "$SKIP_REASON"
else
  # A create failure that is NOT the rate limit: surface it as a hard failure,
  # with the engine's own error, and still run the derived assertions.
  echo "  tunnel create failed (not a rate limit): $TUN_BODY" >&2
  chk "tunnel.url_is_trycloudflare" "yes" "no"
  chk "tunnel.public_fetch_serves_workspace" "yes" "no"
fi

# ---- 5) delete: 204 and the URL stops working ------------------------------
echo "=== 5) delete the tunnel; the URL stops working ==="
if [ "$CF_LIMITED" = "yes" ]; then
  skip "tunnel.delete_204" "$SKIP_REASON"
  skip "tunnel.url_dead_after_delete" "$SKIP_REASON"
else
  CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 30 -X DELETE "$BASE/v1/workspaces/$WSA/tunnels/$TID" -H "$A")
  chk "tunnel.delete_204" "204" "$CODE"
  sleep 3
  GONE=$(timeout 20 curl -s -o /dev/null -w '%{http_code}' -m 12 "$URL/" 2>/dev/null)
  # After delete the edge answers 530 (origin gone) or the connection fails; a
  # 200 serving our body would be a failure.
  chk "tunnel.url_dead_after_delete" "yes" "$([ "$GONE" != "200" ] && echo yes || echo no)"
fi

# ---- 6) error contracts + the documented 5xx -------------------------------
echo "=== 6) error contracts (403/404/400) and the documented 5xx ==="
chk "contract.foreign_create_403" "403" "$(curl -s -o /dev/null -w '%{http_code}' -m 20 -X POST "$BASE/v1/workspaces/$WSA/tunnels" -H "$B" -H "$J" -d '{"port":5000}')"
chk "contract.unknown_tunnel_404" "404" "$(curl -s -o /dev/null -w '%{http_code}' -m 20 "$BASE/v1/workspaces/$WSA/tunnels/tun_missing" -H "$A")"
chk "contract.bad_port_400" "400" "$(curl -s -o /dev/null -w '%{http_code}' -m 20 -X POST "$BASE/v1/workspaces/$WSA/tunnels" -H "$A" -H "$J" -d '{"port":80}')"
chk "contract.out_of_range_400" "400" "$(curl -s -o /dev/null -w '%{http_code}' -m 20 -X POST "$BASE/v1/workspaces/$WSA/tunnels" -H "$A" -H "$J" -d '{"port":70000}')"

# The documented 5xx: a second instance with url_timeout_sec=1 so cloudflared
# cannot possibly print a URL in time. The create must fail honestly.
write_cfg "$CFG2" "$DATA2" "$NSDIR2" sbxdrelay2 "$PORT2" 1
SRV2=$(start_service "$BIN" "$CFG2" "$LOG2" "$BASE2" 2>/dev/null)
if [ -n "$SRV2" ]; then
  WS2=$(curl -s -m 20 -X POST "$BASE2/v1/workspaces" -H "$A" -H "$J" \
    -d '{"name":"nourl","limits":{"allow_net":true,"allow_tunnels":true}}' | jsonf "['workspace']['id']")
  NOURL=$(curl -s -m 60 -X POST "$BASE2/v1/workspaces/$WS2/tunnels" -H "$A" -H "$J" -d '{"port":5000}')
  chk "contract.no_url_code" "tunnel_url_unavailable" "$(printf '%s' "$NOURL" | jsonf "['error']['code']" 2>/dev/null || echo none)"
  HTTP=$(curl -s -o /dev/null -w '%{http_code}' -m 60 -X POST "$BASE2/v1/workspaces/$WS2/tunnels" -H "$A" -H "$J" -d '{"port":5000}')
  chk "contract.no_url_status" "504" "$HTTP"
  kill "$SRV2" 2>/dev/null; wait "$SRV2" 2>/dev/null; SRV2=""
else
  echo "  FAIL  contract.no_url_instance did not start"; FAIL=$((FAIL + 1))
fi

# ---- 7) the tunnel dies with its workspace ---------------------------------
echo "=== 7) the tunnel dies with its workspace ==="
if [ "$CF_LIMITED" = "yes" ]; then
  # No cloudflared process can exist when provisioning is rate limited, so the
  # lifetime assertions cannot be exercised. Workspace delete is still asserted
  # (it must clean up regardless of tunnels).
  skip "lifetime.cf_running_before" "$SKIP_REASON"
  skip "lifetime.cf_gone_after_delete" "$SKIP_REASON"
else
  lifecycle_rc=0
  create_tunnel_retry "$WSA" 5000 || lifecycle_rc=$?
  if [ "$lifecycle_rc" = "1" ]; then
    CF_LIMITED=yes
    SKIP_REASON="cloudflare quick-tunnel rate limit (HTTP 429 from provisioning)"
    skip "lifetime.cf_running_before" "$SKIP_REASON"
    skip "lifetime.cf_gone_after_delete" "$SKIP_REASON"
  else
    CF_BEFORE=$(pgrep -fc 'cloudflared tunnel --no-autoupdate --url' 2>/dev/null || true)
    chk "lifetime.cf_running_before" "yes" "$([ -n "$CF_BEFORE" ] && [ "$CF_BEFORE" -ge 1 ] && echo yes || echo no)"
  fi
fi
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 40 -X DELETE "$BASE/v1/workspaces/$WSA" -H "$A")
chk "lifetime.ws_delete_204" "204" "$CODE"
if [ "$CF_LIMITED" != "yes" ]; then
  sleep 3
  CF_AFTER=$(pgrep -fc 'cloudflared tunnel --no-autoupdate --url' 2>/dev/null || true)
  chk "lifetime.cf_gone_after_delete" "0" "$([ -z "$CF_AFTER" ] && echo 0 || echo "$CF_AFTER")"
fi

# ---- 8) zero host topology -------------------------------------------------
echo "=== 8) zero host topology ==="
if [ -n "$SRV" ]; then kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; SRV=""; fi
sleep 2
noveth=$(ip -o link | grep -cE 'veth|sbx|tap0' || true)
# The engine must have removed its own namespaces on shutdown; the harness does
# NOT clean them up here, so a leak is caught rather than masked.
chk "topology.links_unchanged" "same" "$([ "$(topo_links)" = "$HOST_LINKS_BEFORE" ] && echo same || echo changed)"
chk "topology.routes_unchanged" "same" "$([ "$(topo_routes)" = "$HOST_ROUTES_BEFORE" ] && echo same || echo changed)"
chk "topology.no_new_veth_or_tap" "0" "${noveth:-0}"
chk "topology.netns_empty_after" "0" "$(ls -A "$NSDIR" 2>/dev/null | wc -l | tr -d ' ')"

echo
if [ "$SKIP" -gt 0 ]; then
  echo "================ tunnel acceptance: $PASS passed / $FAIL failed / $SKIP skipped ($SKIP_REASON) ================"
else
  echo "================ tunnel acceptance: $PASS passed / $FAIL failed / 0 skipped ================"
fi
# Exit 0 when nothing failed: skipped is an external-environment outcome, not a
# failure of the engine. A single non-rate-limit failure still exits non-zero.
[ "$FAIL" -eq 0 ]
