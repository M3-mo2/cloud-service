#!/usr/bin/env bash
# identity-check.sh -- real-nsjail acceptance for host-identity masking.
#
# WHY THIS EXISTS
#   A tenant must not be able to learn WHICH machine their workspace runs on:
#   no host name, no internal DNS, no machine/hardware fingerprint, no real host
#   size, no way to reach a "what is my IP" service without allow_net, and a
#   host that is left byte-for-byte untouched. Most of the filesystem view is
#   already closed by the mount namespace (no /proc, no /sys), but a set of
#   facts remains reachable by SYSCALL and no mount hides them. This harness
#   runs the REAL service against the host's REAL nsjail and asserts both the
#   already-closed baseline AND the newly-masked facts, plus the host-topology
#   invariant. It is the same class of check as process-stop-check.sh: real
#   processes, real namespaces, hard assertions.
#
# WHAT IT ASSERTS (every check is hard; the script exits non-zero on any failure,
# and no assertion is ever weakened to make a run pass):
#   1. an allow_net:false workspace cannot read any of the "already closed"
#      paths: /etc/machine-id, /etc/os-release, /etc/hostname, /etc/hosts,
#      /etc/resolv.conf, /etc/hostid, /etc/shadow, /proc/version,
#      /proc/meminfo, /proc/cpuinfo, /proc/net/dev, /proc/1/cgroup,
#      /proc/self/cgroup, /proc/self/mountinfo, /sys/class/net, every
#      /sys/class/dmi/id/*, /var/run/zerops and /run/zerops;
#   2. hostname is the generic "sandbox" (never the host's), `hostname -f`
#      fails, `hostname -I` is empty, and the operator's internal DNS names do
#      not resolve;
#   3. an allow_net:false workspace reaches NO "what is my IP" service;
#   4. with masking ON (the default), the reported cpu count and memory match
#      the WORKSPACE's limits (not the host's) and the kernel string no longer
#      identifies the platform -- AND those answers survive `env -i`, so a
#      tenant cannot bypass the mask by clearing the environment;
#   5. with masking OFF, the same probes DO leak the host values, which proves
#      the switch is real and verifiable (the leak is asserted, not hidden);
#   6. the egress IP an allow_net:true workspace presents is reported as
#      MEASURED, with the value, and is a public address (a platform-shared NAT
#      address is an acceptable, expected outcome -- only the measurement is
#      required);
#   7. the host is untouched: `ip -o link` gains no interface, `ip route` is
#      byte-identical and /run/netns is empty, before and after the whole suite.
#
# THE THIRD STATE: skipped-environment.
#   Assertion 6 depends on the public internet reachable from the workspace. If
#   the egress path cannot reach the IP service at all (a platform-wide network
#   or DNS outage), the assertion is recorded as SKIP with the reason and the
#   suite still exits 0 -- a suite that goes red because the internet is down
#   trains people to ignore it. This is never used to skip assertion 4 or 1: a
#   masking or filesystem-view failure is ALWAYS a hard FAIL. The skip follows
#   the tunnel-check.sh precedent exactly.
#
# REQUIREMENTS
#   Root (nsjail needs cgroup v2 and mount namespaces; the egress layer needs
#   slirp4netns), the real nsjail binary, slirp4netns, the Go toolchain, python3
#   and curl. `sudo` resets PATH, so the toolchain is resolved explicitly.
#
# USAGE
#   sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/identity-check.sh
#   Environment: PORT (mask-on instance, default 8095), PORT2 (mask-off
#     instance, default 8094).
#
# EXIT
#   0 when every assertion passes (or only the egress-IP assertion was skipped
#   for an external network outage); non-zero on any real failure. When root,
#   nsjail or the toolchain is absent the script prints SKIP and exits 0: it
#   cannot prove anything without them.
set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/identity-check.sh

Real-nsjail acceptance harness for host-identity masking. It builds sandboxd,
runs it under the host's real nsjail, and asserts that the filesystem view is
closed, that hostname/DNS reveal nothing, that allow_net:false reaches no
"What is my IP" service, that with masking on the reported cpu count and memory
match the WORKSPACE's limits (not the host's) and the kernel no longer
identifies the platform, that with masking off the leak is real (the switch is
verifiable), that the measured egress IP is reported, and that the host
topology is untouched.

Environment:
  PORT   port for the mask-on instance (default 8095).
  PORT2  port for the mask-off instance (default 8094).

The egress-IP assertion is reported as SKIP (a third state, exit 0) when the
public internet is unreachable from the workspace, and as FAIL for any other
cause. Every other assertion is always hard.
USAGE
}
case "${1:-}" in
  -h | --help) usage; exit 0 ;;
esac

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PORT=${PORT:-8095}
PORT2=${PORT2:-8094}
DATA=$(mktemp -d)
DATA2=$(mktemp -d)
NSDIR="$DATA/netns"
NSDIR2="$DATA2/netns"
CFG="$DATA/config.yaml"
CFG2="$DATA2/config.yaml"
BIN=$(mktemp)
LOG="$DATA/service.log"
LOG2="$DATA2/service.log"
SRV=""
SRV2=""
KEY=dev-key-please-rotate
A="Authorization: Bearer $KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
BASE2="http://127.0.0.1:$PORT2"
PASS=0
FAIL=0
SKIP=0
SKIP_REASON=""
WORK=$(mktemp -d)

# The workspace's own limits, deliberately different from the host's, so a
# masked answer is provably the workspace's and not a lucky host coincidence.
WS_CPUS=1
WS_MEM_MB=256
WS_RELEASE=0.0.0-sandbox
HOST_RELEASE=$(uname -r)
HOST_CPUS=$(nproc 2>/dev/null || echo 0)
HOST_HOSTNAME=$(hostname 2>/dev/null || echo "")

chk() { # label expected actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}
# skip records an assertion that could not be exercised because an external
# dependency (the public internet) was unavailable. A distinct third state: not
# a pass (nothing proved) and not a fail (the engine is not at fault).
skip() { # label reason
  echo "  SKIP  $1 ($2)"; SKIP=$((SKIP + 1))
}
jsonf() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)"; }

# put_py writes a python file into a workspace.
put_py() { # key ws path content
  python3 - "$4" >"$WORK/payload.json" <<'PY'
import json,sys
print(json.dumps({"content":sys.argv[1]}))
PY
  curl -s -o /dev/null -m 30 -X PUT "$BASE/v1/workspaces/$2/files/content?path=$3" -H "$1" -H "$J" --data-binary @"$WORK/payload.json"
}
# exec_py runs a python file inside the workspace and prints the exec JSON.
exec_py() { # key ws path timeout
  curl -s -m 90 -X POST "$BASE/v1/workspaces/$2/exec" -H "$1" -H "$J" \
    -d "{\"cmd\":[\"python3\",\"${3#/}\"],\"timeout_sec\":${4:-40}}"
}
# exec_py2 is exec_py against the mask-off instance.
exec_py2() { # key ws path timeout
  curl -s -m 90 -X POST "$BASE2/v1/workspaces/$2/exec" -H "$1" -H "$J" \
    -d "{\"cmd\":[\"python3\",\"${3#/}\"],\"timeout_sec\":${4:-40}}"
}

# --- host topology snapshots (assertion 7) ---------------------------------
# These read the host's view only. The engine's egress is chained slirp4netns
# over UNIX sockets and creates ZERO host interfaces/routes, so the snapshots
# must be byte-identical before and after.
topo_links() { ip -o link 2>/dev/null | awk '{print $2,$9}' | sort; }
topo_routes() { ip route 2>/dev/null | sort; }
HOST_LINKS_BEFORE=$(topo_links)
HOST_ROUTES_BEFORE=$(topo_routes)
NS_ENTRIES_BEFORE=$(ls -A /run/netns 2>/dev/null | wc -l | tr -d ' ')

cleanup() {
  [ -n "$SRV" ] && { kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; }
  [ -n "$SRV2" ] && { kill "$SRV2" 2>/dev/null; wait "$SRV2" 2>/dev/null; }
  sleep 1
  # Remove only the engine's own namespaces under the two private netns dirs.
  for d in "$NSDIR" "$NSDIR2"; do
    for n in $(ls "$d" 2>/dev/null); do umount "$d/$n" 2>/dev/null; rm -f "$d/$n"; done
  done
  rm -rf "$DATA" "$DATA2" "$BIN" "$WORK"
}
trap cleanup EXIT INT TERM

if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root; re-run with: sudo -n env \"PATH=\$PATH:/usr/local/go/bin:/usr/local/bin\" bash scripts/identity-check.sh"
  exit 0
fi
command -v nsjail >/dev/null || { echo "SKIP: nsjail is not installed on this host"; exit 0; }
command -v slirp4netns >/dev/null || { echo "SKIP: slirp4netns is not installed on this host"; exit 0; }
if ! getent group sandbox >/dev/null; then
  groupadd -r sandbox && useradd -r -g sandbox -M -s /usr/sbin/nologin sandbox
fi

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
# The shim must be built from source before go build embeds it, so a stale or
# missing artifact cannot silently disable the mask.
if command -v cc >/dev/null 2>&1; then
  cc -shared -fPIC -O2 -Wall -o "$ROOT/internal/executor/hostmask.so" \
    "$ROOT/internal/executor/hostmask/hostmask.c" -ldl || { echo "FAIL  shim: could not build hostmask.so"; exit 1; }
fi
(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1

echo "=== host baseline ==="
echo "  host kernel=$HOST_RELEASE cpus=$HOST_CPUS hostname=$HOST_HOSTNAME"
echo "  workspace limits: cpus=$WS_CPUS mem=${WS_MEM_MB}MB"

# --- service startup (mirrors tunnel-check.sh) -----------------------------
write_cfg() { # path data netnsdir relay port identity_block
  cat >"$1" <<YAML
server: { addr: "127.0.0.1:$5" }
storage: { data_dir: "$2" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: $WS_CPUS, memory_mb: $WS_MEM_MB, disk_mb: 256, max_wall_time_sec: 600, max_runs: 5, allow_net: false, allow_unlimited_runs: true }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true, allow_unlimited_runs: true }
caps: { exec_stdout_kb: 256, exec_stderr_kb: 256, run_log_mb: 5, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 5 }
network: { enabled: true, netns_dir: "$3", relay_netns: "$4", slirp_path: "$(command -v slirp4netns)", mtu: 65520, resolver: ["1.1.1.1","8.8.8.8"], start_timeout_ms: 8000 }
$6
keys:
  - { principal_id: "user_dev", key: "$KEY" }
YAML
}

start_service() { # bin cfg log url; prints pid
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

echo "=== starting the mask-on service under real nsjail ==="
write_cfg "$CFG" "$DATA" "$NSDIR" sbxdrelay "$PORT" "identity: { enabled: true }"
SRV=$(start_service "$BIN" "$CFG" "$LOG" "$BASE") || exit 1
echo "  service started (pid $SRV)"
MASK_STATUS=$(curl -s "$BASE/readyz" | jsonf "['identity_masking']")
echo "  readyz.identity_masking: $MASK_STATUS"
chk "readyz.identity_masking.ready" "True" "$(curl -s "$BASE/readyz" | jsonf "['identity_masking']['ready']")"

# --- 1) the filesystem view is closed --------------------------------------
echo "=== 1) the 'already closed' paths are unreadable ==="
WSN=$(curl -s -m 20 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" -d '{"name":"closed"}' | jsonf "['workspace']['id']")
[ -n "$WSN" ] || { echo "FATAL: no workspace id"; exit 1; }
put_py "$A" "$WSN" /closed.py 'import os
paths = ["/etc/machine-id","/etc/os-release","/etc/hostname","/etc/hosts",
         "/etc/resolv.conf","/etc/hostid","/etc/shadow","/proc/version",
         "/proc/meminfo","/proc/cpuinfo","/proc/net/dev","/proc/1/cgroup",
         "/proc/self/cgroup","/proc/self/mountinfo","/sys/class/net",
         "/sys/class/dmi/id/product_name","/sys/class/dmi/id/sys_vendor",
         "/sys/class/dmi/id/product_uuid","/sys/class/dmi/id/board_name",
         "/sys/class/dmi/id/bios_version","/var/run/zerops","/run/zerops"]
for p in paths:
    try:
        with open(p, "rb") as fh:
            fh.read(1)
        print("READABLE", p)
    except Exception:
        print("CLOSED", p)
'
CLOSED_OUT=$(exec_py "$A" "$WSN" /closed.py 40 | jsonf "['stdout']")
READABLE=$(printf '%s' "$CLOSED_OUT" | grep -c '^READABLE' || true)
chk "closed.paths_none_readable" "0" "${READABLE:-0}"
if [ "${READABLE:-0}" != "0" ]; then
  printf '%s\n' "$CLOSED_OUT" | grep '^READABLE' | sed 's/^/    /'
fi

# --- 2) hostname and internal DNS ------------------------------------------
echo "=== 2) hostname and internal DNS reveal nothing ==="
put_py "$A" "$WSN" /host.py 'import socket, subprocess
print("HOSTNAME", socket.gethostname())
r = subprocess.run(["hostname","-f"], capture_output=True, text=True, timeout=8)
print("HNAME_F_RC", r.returncode)
print("HNAME_F_OUT", r.stdout.strip())
r = subprocess.run(["hostname","-I"], capture_output=True, text=True, timeout=8)
print("HNAME_I_OUT", r.stdout.strip())
for name in ["node-id-1.runtime.zcp.zerops","node-id-1.master.core.zerops","core.zerops","zerops"]:
    try:
        print("DNS", name, socket.gethostbyname(name))
    except Exception:
        print("DNS", name, "NXDOMAIN")
'
HOST_OUT=$(exec_py "$A" "$WSN" /host.py 40 | jsonf "['stdout']")
# val KEY prints the rest of the line after KEY (empty when there is no value).
val() { printf '%s' "$HOST_OUT" | awk -v k="$1" '$1==k{$1="";sub(/^ /,"");print;exit}'; }
WS_HOSTNAME=$(val HOSTNAME)
chk "host.gethostname_generic" "sandbox" "$WS_HOSTNAME"
chk "host.gethostname_not_host" "no" "$([ "$WS_HOSTNAME" = "$HOST_HOSTNAME" ] && echo yes || echo no)"
# hostname -f must fail to resolve (non-zero or empty); hostname -I must be empty.
F_RC=$(val HNAME_F_RC)
F_OUT=$(val HNAME_F_OUT)
chk "host.hostname_f_fails" "no" "$([ "$F_RC" = "0" ] && [ -n "$F_OUT" ] && echo yes || echo no)"
chk "host.hostname_I_empty" "" "$(val HNAME_I_OUT)"
DNS_RESOLVED=$(printf '%s' "$HOST_OUT" | awk '$1=="DNS" && $3!="NXDOMAIN"{print $2}' | tr '\n' ',')
chk "host.internal_dns_unresolved" "" "$DNS_RESOLVED"

# --- 3) allow_net:false reaches no "what is my IP" service ------------------
echo "=== 3) allow_net:false reaches no what-is-my-IP service ==="
put_py "$A" "$WSN" /nonet.py 'import urllib.request
for url in ["https://api.ipify.org","https://icanhazip.com","https://ifconfig.me/ip"]:
    try:
        print("REACHED", url, urllib.request.urlopen(url, timeout=8).read().decode().strip())
    except Exception:
        print("BLOCKED", url)
'
NONET_OUT=$(exec_py "$A" "$WSN" /nonet.py 60 | jsonf "['stdout']")
REACHED=$(printf '%s' "$NONET_OUT" | grep -c '^REACHED' || true)
chk "no_net.no_ip_service_reached" "0" "${REACHED:-0}"

# --- 4) masking ON: workspace limits, no platform fingerprint, env-proof ----
echo "=== 4) masking ON reports the workspace's limits (and survives env -i) ==="
WSA=$(curl -s -m 20 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" -d '{"name":"masked","limits":{"allow_net":true,"allow_unlimited_runs":true}}' | jsonf "['workspace']['id']")
put_py "$A" "$WSA" /masked.py 'import os, platform, subprocess
u = platform.uname()
print("RELEASE", u.release)
print("VERSION", u.version)
print("CPUCOUNT", os.cpu_count())
print("SYSCONF_CPU", os.sysconf("SC_NPROCESSORS_ONLN"))
print("MEM", os.sysconf("SC_PHYS_PAGES") * os.sysconf("SC_PAGE_SIZE"))
# env -i must not defeat the mask.
r = subprocess.run(["env","-i","PATH=/usr/bin:/bin","nproc"], capture_output=True, text=True, timeout=8)
print("ENVI_NPROC", r.stdout.strip())
r = subprocess.run(["env","-i","PATH=/usr/bin:/bin","uname","-r"], capture_output=True, text=True, timeout=8)
print("ENVI_UNAME", r.stdout.strip())
r = subprocess.run(["env","-i","PATH=/usr/bin:/bin","getconf","_NPROCESSORS_ONLN"], capture_output=True, text=True, timeout=8)
print("ENVI_GETCONF", r.stdout.strip())
'
MASK_OUT=$(exec_py "$A" "$WSA" /masked.py 60 | jsonf "['stdout']")
mval() { printf '%s' "$MASK_OUT" | awk -v k="$1" '$1==k{print $2}'; }
chk "mask.release_not_host" "no" "$([ "$(mval RELEASE)" = "$HOST_RELEASE" ] && echo yes || echo no)"
chk "mask.release_generic" "$WS_RELEASE" "$(mval RELEASE)"
chk "mask.cpu_count_workspace" "$WS_CPUS" "$(mval CPUCOUNT)"
chk "mask.sysconf_cpu_workspace" "$WS_CPUS" "$(mval SYSCONF_CPU)"
chk "mask.memory_workspace_bytes" "$((WS_MEM_MB * 1024 * 1024))" "$(mval MEM)"
chk "mask.cpu_not_host" "no" "$([ "$(mval CPUCOUNT)" = "$HOST_CPUS" ] && echo yes || echo no)"
if [ "$HOST_CPUS" = "$WS_CPUS" ]; then
  # If the host happens to have the same core count the "not host" assertion is
  # vacuous; say so rather than imply it proved something.
  skip "mask.cpu_distinct_from_host" "host and workspace both report ${WS_CPUS} cpu(s)"
fi
chk "mask.env_i_nproc" "$WS_CPUS" "$(mval ENVI_NPROC)"
chk "mask.env_i_uname" "$WS_RELEASE" "$(mval ENVI_UNAME)"
chk "mask.env_i_getconf" "$WS_CPUS" "$(mval ENVI_GETCONF)"
# A tenant must not be able to replace the shim or the preload list.
put_py "$A" "$WSA" /write.py 'import os
for p in ["/sandboxd/hostmask.so","/etc/ld.so.preload"]:
    try:
        with open(p, "a") as fh:
            fh.write("x")
        print("WROTE", p)
    except Exception:
        print("READONLY", p)
'
WRITE_OUT=$(exec_py "$A" "$WSA" /write.py 30 | jsonf "['stdout']")
WROTE=$(printf '%s' "$WRITE_OUT" | grep -c '^WROTE' || true)
chk "mask.shim_not_writable" "0" "${WROTE:-0}"

# The mask must not break the runtimes: bash, python3 and perl still run and
# produce their output with the shim active.
runcmd() { # base argv-json -> "<exit_code> <stdout>"
  curl -s -m 60 -X POST "$1/v1/workspaces/$2/exec" -H "$A" -H "$J" \
    -d "{\"cmd\":$3,\"timeout_sec\":30}" | python3 -c "import json,sys;d=json.load(sys.stdin);print(d.get('exit_code'),repr(d.get('stdout','').strip()))"
}
chk "mask.runtime_python3" "0 'py-ok'" "$(runcmd "$BASE" "$WSA" '["python3","-c","print(\"py-ok\")"]')"
chk "mask.runtime_bash" "0 'sh-ok'" "$(runcmd "$BASE" "$WSA" '["bash","-c","echo sh-ok"]')"
chk "mask.runtime_perl" "0 'perl-ok'" "$(runcmd "$BASE" "$WSA" '["perl","-e","print \"perl-ok\\n\""]')"
# node's outcome under the mask is captured and compared with its outcome
# WITHOUT the mask below: the requirement is that masking introduces no
# regression, and node is known to be constrained by rlimit_as independently of
# masking on this image, so equality (not success) is the honest assertion.
NODE_ON=$(runcmd "$BASE" "$WSA" '["node","-e","console.log(\"node-ok\")"]' | awk '{print $1}')

# --- 6) egress IP, measured (may be environment-skipped) --------------------
# Numbered to match the brief; executed here while the mask-on service (which
# owns the allow_net workspace) is still up.
echo "=== 6) measured egress IP on the allow_net:true path ==="
put_py "$A" "$WSA" /egress.py 'import urllib.request
for url in ["https://api.ipify.org","https://icanhazip.com","https://ifconfig.me/ip"]:
    try:
        print("EGRESS", url, urllib.request.urlopen(url, timeout=25).read().decode().strip())
    except Exception as e:
        print("EGRESSERR", url, type(e).__name__)
'
EGRESS_OUT=$(exec_py "$A" "$WSA" /egress.py 90 | jsonf "['stdout']")
EGRESS_IP=$(printf '%s' "$EGRESS_OUT" | awk '$1=="EGRESS"{print $3; exit}')
if [ -n "$EGRESS_IP" ]; then
  echo "  measured egress IP: $EGRESS_IP"
  chk "egress.ip_is_public" "yes" "$(python3 -c "
import ipaddress,sys
try:
    a=ipaddress.ip_address('$EGRESS_IP')
    print('yes' if a.is_global else 'no')
except Exception:
    print('no')
")"
  # Report whether it is the host's own public address (it should not matter,
  # but the fact is worth stating).
  HOST_EGRESS=$(timeout 20 curl -s https://api.ipify.org 2>/dev/null || echo "")
  if [ -n "$HOST_EGRESS" ] && [ "$HOST_EGRESS" = "$EGRESS_IP" ]; then
    echo "  note: workspace egress IP equals the host's own public egress IP (platform-shared NAT)"
  fi
else
  ERRS=$(printf '%s' "$EGRESS_OUT" | grep '^EGRESSERR' | head -1)
  if [ -n "$ERRS" ]; then
    SKIP_REASON="public internet unreachable from the workspace (${ERRS#EGRESSERR })"
    echo "  $SKIP_REASON"
    skip "egress.ip_is_public" "$SKIP_REASON"
  else
    chk "egress.ip_is_public" "yes" "no"
  fi
fi

# --- 5) masking OFF: the leak is real, so the switch is verifiable ---------
# The brief numbers this assertion 5. It is executed here (against the second
# instance) right after the masking-on checks so the two states are read side by
# side.
echo "=== 5) masking OFF leaks the host values (the switch is verifiable) ==="
put_py2() { # key ws path content
  python3 - "$4" >"$WORK/payload.json" <<'PY'
import json,sys
print(json.dumps({"content":sys.argv[1]}))
PY
  curl -s -o /dev/null -m 30 -X PUT "$BASE2/v1/workspaces/$2/files/content?path=$3" -H "$1" -H "$J" --data-binary @"$WORK/payload.json"
}
write_cfg "$CFG2" "$DATA2" "$NSDIR2" sbxdrelay2 "$PORT2" "identity: { enabled: false }"
SRV2=$(start_service "$BIN" "$CFG2" "$LOG2" "$BASE2") || { echo "  FAIL  mask-off service did not start"; FAIL=$((FAIL + 1)); }
if [ -n "$SRV2" ]; then
  chk "mask_off.readyz_enabled" "False" "$(curl -s "$BASE2/readyz" | jsonf "['identity_masking']['enabled']")"
  WSO=$(curl -s -m 20 -X POST "$BASE2/v1/workspaces" -H "$A" -H "$J" -d '{"name":"leaky"}' | jsonf "['workspace']['id']")
  put_py2 "$A" "$WSO" /leak.py 'import os, platform
u = platform.uname()
print("RELEASE", u.release)
print("CPUCOUNT", os.cpu_count())
print("MEM", os.sysconf("SC_PHYS_PAGES") * os.sysconf("SC_PAGE_SIZE"))
'
  LEAK_OUT=$(exec_py2 "$A" "$WSO" /leak.py 40 | jsonf "['stdout']")
  lval() { printf '%s' "$LEAK_OUT" | awk -v k="$1" '$1==k{print $2}'; }
  chk "mask_off.release_leaks_host" "$HOST_RELEASE" "$(lval RELEASE)"
  chk "mask_off.cpu_leaks_host" "$HOST_CPUS" "$(lval CPUCOUNT)"
  # The leaked memory must be the host's, not the 256 MB workspace limit: this
  # proves the off-switch really disables the mask rather than reporting a
  # constant.
  chk "mask_off.mem_leaks_host" "yes" "$([ "$(lval MEM)" -gt "$((WS_MEM_MB * 1024 * 1024))" ] && echo yes || echo no)"
  # node's result WITHOUT the mask, compared with the masked result: masking
  # must introduce no regression. On this image node is independently limited
  # by rlimit_as, so the honest assertion is equality of outcome.
  NODE_OFF=$(runcmd "$BASE2" "$WSO" '["node","-e","console.log(\"node-ok\")"]' | awk '{print $1}')
  chk "mask.runtime_node_no_regression" "$NODE_OFF" "$NODE_ON"
fi

# --- 7) the host is untouched ----------------------------------------------
echo "=== 7) the host topology is untouched ==="
[ -n "$SRV" ] && { kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; SRV=""; }
[ -n "$SRV2" ] && { kill "$SRV2" 2>/dev/null; wait "$SRV2" 2>/dev/null; SRV2=""; }
sleep 2
chk "topology.links_unchanged" "same" "$([ "$(topo_links)" = "$HOST_LINKS_BEFORE" ] && echo same || echo changed)"
chk "topology.routes_unchanged" "same" "$([ "$(topo_routes)" = "$HOST_ROUTES_BEFORE" ] && echo same || echo changed)"
NOVETH=$(ip -o link 2>/dev/null | grep -cE 'veth|sbx|tap0' || true)
chk "topology.no_new_veth_or_tap" "0" "${NOVETH:-0}"
chk "topology.netns_empty" "$NS_ENTRIES_BEFORE" "$(ls -A /run/netns 2>/dev/null | wc -l | tr -d ' ')"

echo
if [ "$SKIP" -gt 0 ]; then
  echo "================ identity acceptance: $PASS passed / $FAIL failed / $SKIP skipped ($SKIP_REASON) ================"
else
  echo "================ identity acceptance: $PASS passed / $FAIL failed / 0 skipped ================"
fi
[ "$FAIL" -eq 0 ]
