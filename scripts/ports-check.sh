#!/usr/bin/env bash
# ports-check.sh -- real-nsjail acceptance for native port/service discovery.
#
# WHY THIS EXISTS
#   A tenant cannot answer "which ports is my workspace listening on?" itself:
#   inside a workspace the curated /proc carries NO /proc/net (ADR 86) and
#   neither `ss` nor `netstat` is installed, so a user's app can bind a port and
#   the user has no way to see it. The engine answers it with
#   GET /v1/workspaces/{id}/ports, which reads /proc/<pid>/net/* for a pid inside
#   the workspace's own network namespace and attributes each socket inode to the
#   owning process. Unit tests with a stub runner cannot prove the ISOLATION
#   property (a host port must never leak into a workspace's list); only the real
#   nsjail with real per-workspace namespaces can. This gate runs the real
#   service against the host's REAL nsjail and asserts the whole contract.
#
# WHAT IT ASSERTS (every check is hard; exits non-zero on any failure, and no
# assertion is ever weakened to make a run pass):
#   1. a TCP listener on 127.0.0.1 appears with the right port, protocol and
#      `loopback` scope;
#   2. a TCP listener on 0.0.0.0 appears with `wildcard` scope;
#   3. a UDP socket is reported and is NOT called LISTEN (UDP has no listen
#      state);
#   4. the owning process is resolved to the exact pid the test started, with a
#      label carrying its argv;
#   5. A PORT ON THE HOST IS NEVER REPORTED in a workspace's list -- the
#      isolation assertion, without which the endpoint could leak the host's and
#      other tenants' services;
#   6. two workspaces with different (and same-numbered) listeners each report
#      ONLY their own -- the cross-tenant assertion;
#   7. a workspace with nothing listening returns an EMPTY list and 200, not an
#      error;
#   8. a port that appears after the first call shows up on the next call (no
#      stale cache) and one whose process exited disappears;
#   9. `complete` is true in the normal case and the list is deterministically
#      sorted;
#  10. the endpoint is NOT part of the metrics stream: the metrics payload
#      carries no port data and the port-read counter does not move across a
#      tick (while it does move on a /ports request, so the test is not vacuous);
#  11. /v1/meta advertises the capability;
#  12. no leak: no mount, no cgroup, no netns, no fuse2fs, no orphan image
#      survives the gate.
#
# REQUIREMENTS
#   Root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk),
#   the real nsjail binary, the Go toolchain, python3, jq and curl. `sudo`
#   resets PATH, so the toolchain is resolved explicitly below.
#
# USAGE
#   sudo -E bash scripts/ports-check.sh
#   Environment: PORT (default 8092).
#
# EXIT
#   0 when every assertion passes; non-zero on any failure. When root, nsjail or
#   the toolchain is absent the script prints a SKIP line and exits 0.
set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: sudo -E bash scripts/ports-check.sh

Real-nsjail acceptance harness for native port/service discovery. It builds
sandboxd, runs it under the host's real nsjail, and asserts that a workspace's
listening ports are reported with their scope, protocol and owning process, that
a host port never leaks into a workspace's list, that two workspaces see only
their own ports, that an empty workspace is an honest empty 200, that the list
is live (not cached), that it is not part of the metrics stream, and that
nothing is leaked.

Environment:
  PORT   port for the service (default 8092).

Exit status is 0 only when every assertion passes (or inputs are absent, in
which case a SKIP line is printed first). It is non-zero on any failure.
USAGE
}
case "${1:-}" in
  -h | --help)
    usage
    exit 0
    ;;
esac

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PORT=${PORT:-8092}
DATA=$(mktemp -d)
CFG=$(mktemp)
BIN=$(mktemp)
LOG=$(mktemp)
SRV=""
HOSTPID=""
KEY=dev-key-please-rotate
OTHER_KEY=other-key
A="Authorization: Bearer $KEY"
B="Authorization: Bearer $OTHER_KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
PASS=0
FAIL=0
# Ports are chosen INSIDE the workspace's own private namespace, so they never
# collide with the host. They avoid the operator's ports (22/514/8080/8081/8099/
# 10303/65431/65436) on principle.
P_LOOP=18080   # 127.0.0.1  (A and B both use this number: per-namespace proof)
P_WILD=18081   # 0.0.0.0
P_UDP=18082    # 0.0.0.0 udp
P_LATE=18083   # appears only after the first read
# A unique token per run so the process inventory can be matched exactly and a
# bracketed pgrep never matches this shell.
NS="sandboxdports-$$"

chk() { # label expected actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}
chkbool() { # label yes/no actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}
jf() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)"; }

# ports_json KEY WS -> the full /ports JSON.
ports_json() { curl -s -H "$1" "$BASE/v1/workspaces/$2/ports"; }

# port_field KEY WS PORT PROTO FIELD -> a field of the matching endpoint, or "".
port_field() { # key ws port proto field
  ports_json "$1" "$2" | python3 -c "
import json,sys
try: ports=json.load(sys.stdin).get('ports',[])
except Exception: ports=[]
for e in ports:
    if e.get('port')==$3 and e.get('protocol')=='$4':
        v=e.get('$5','')
        if isinstance(v,bool): print('true' if v else 'false')
        else: print(v)
        break
"
}

# has_port KEY WS PORT PROTO -> yes/no.
has_port() { # key ws port proto
  v=$(port_field "$1" "$2" "$3" "$4" "port")
  [ "$v" = "$3" ] && echo yes || echo no
}

# proc_pid_by_token KEY WS TOKEN -> the host pid of the workspace process whose
# argv carries the token (prefers the python3 offender over any wrapper), or "".
proc_pid_by_token() { # key ws token
  curl -s -H "$1" "$BASE/v1/workspaces/$2/processes" | python3 -c "
import json,sys
tok='$3'
try: procs=json.load(sys.stdin).get('processes',[])
except Exception: procs=[]
m=[p for p in procs if any(tok in a for a in p.get('argv',[]))]
m.sort(key=lambda p: 0 if p.get('argv') and 'python3' in p['argv'][0] else 1)
print(m[0]['pid'] if m else '')
"
}

# run_token KEY WS ARGV_JSON -> starts an unbounded run.
run_token() { # key ws argvjson
  curl -s -o /dev/null -m 20 -X POST "$BASE/v1/workspaces/$2/runs" -H "$1" -H "$J" \
    -d "{\"kind\":\"command\",\"argv\":$3,\"no_timeout\":true}"
}

fuse_count() { ps -eo comm --no-headers | grep -cx fuse2fs; }
nsjail_leaves() {
  { ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null
    ls -d /sys/fs/cgroup/ws-*/NSJAIL.* 2>/dev/null
  } | wc -l | tr -d ' '
}
ws_parents() { ls -d /sys/fs/cgroup/ws-* 2>/dev/null | wc -l | tr -d ' '; }

# engine netns names live as bind mounts under NetnsDir (never `ip netns add`).
engine_netns() { ls /run/netns 2>/dev/null | grep -cE '^sbxdws-|^sbxdrelay$'; }

cg_before=$(nsjail_leaves)
FUSE_BASE=$(fuse_count)

# wait_gone PID -> "gone" once the pid no longer exists.
wait_gone() { # pid
  for _ in $(seq 1 40); do
    kill -0 "$1" 2>/dev/null || { echo gone; return; }
    sleep 0.25
  done
  echo alive
}

cleanup() {
  [ -n "$HOSTPID" ] && { kill "$HOSTPID" 2>/dev/null; wait "$HOSTPID" 2>/dev/null; HOSTPID=""; }
  if [ -n "$SRV" ]; then
    kill "$SRV" 2>/dev/null
    wait "$SRV" 2>/dev/null
  fi
  # Kill only this run's marked workloads by exact pid (bracketed).
  for p in $(pgrep -f "[s]andboxdports-" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  for m in $(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" || true); do
    fusermount3 -u -z "$m" 2>/dev/null || true
  done
  sleep 0.5
  for d in $(ls -d /sys/fs/cgroup/ws-*/NSJAIL.* 2>/dev/null); do
    [ ! -s "$d/cgroup.procs" ] && rmdir "$d" 2>/dev/null
  done
  for d in $(ls -d /sys/fs/cgroup/ws-* 2>/dev/null); do
    [ ! -s "$d/cgroup.procs" ] && rmdir "$d" 2>/dev/null
  done
  for d in $(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null); do
    [ ! -s "$d/cgroup.procs" ] && rmdir "$d" 2>/dev/null
  done
  # Tear down any engine netns bind mount left by this run.
  for n in $(ls /run/netns 2>/dev/null); do
    case "$n" in
      sbxdws-* | sbxdrelay)
        umount "/run/netns/$n" 2>/dev/null || true
        rm -f "/run/netns/$n" 2>/dev/null || true
        ;;
    esac
  done
  rm -rf "$DATA" "$CFG" "$BIN" "$LOG"
}
trap cleanup EXIT INT TERM

if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk); re-run with: sudo -E bash scripts/ports-check.sh"
  exit 0
fi
command -v nsjail >/dev/null || { echo "SKIP: nsjail is not installed on this host"; exit 0; }
command -v python3 >/dev/null || { echo "SKIP: python3 is not installed on this host"; exit 0; }
if ! getent group sandbox >/dev/null; then
  echo "=== creating the sandbox group/user ==="
  groupadd -r sandbox && useradd -r -g sandbox -M -s /usr/sbin/nologin sandbox
fi

# Guard against silent rot: the mechanism must exist in the source before the run
# starts, so a deleted implementation cannot pass by absence.
[ -f "$ROOT/internal/ports/parse.go" ] || { echo "FAIL  artifact: internal/ports/parse.go missing"; exit 1; }
grep -q "NetTablesForPids" "$ROOT/internal/executor/ports.go" ||
  { echo "FAIL  artifact: the /proc/<pid>/net reader is gone"; exit 1; }
grep -q 'r.Get("/ports"' "$ROOT/internal/api/router.go" ||
  { echo "FAIL  artifact: the /ports route is gone"; exit 1; }
grep -q '^ports:' "$ROOT/configs/config.example.yaml" ||
  { echo "FAIL  artifact: the ports config block is not in the example config"; exit 1; }

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
"$GO" build -o "$BIN" ./cmd/sandboxd 2>/dev/null || (cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1

echo "=== starting the service under real nsjail ==="
# network.enabled false: the port read needs no egress; every workspace still
# gets its OWN private, empty network namespace (never the host's), which is
# exactly what the isolation assertion exercises. metrics interval is 1s so the
# no-read-on-a-tick assertion is quick.
cat >"$CFG" <<YAML
server: { addr: "127.0.0.1:$PORT" }
storage: { data_dir: "$DATA" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: 1, memory_mb: 512, disk_mb: 64, max_wall_time_sec: 600, max_runs: 8, allow_net: false, allow_unlimited_runs: true }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true, allow_unlimited_runs: true }
caps: { exec_stdout_kb: 128, exec_stderr_kb: 128, run_log_mb: 5, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 5 }
disk: { enabled: true, backend: "auto" }
network: { enabled: false }
ports: { enabled: true }
metrics: { enabled: true, interval: "1s", min_interval: "1s", max_interval: "300s" }
keys:
  - { principal_id: "user_dev", key: "$KEY" }
  - { principal_id: "user_other", key: "$OTHER_KEY" }
YAML

if curl -s -m 1 -o /dev/null "$BASE/healthz" 2>/dev/null; then
  echo "FATAL: something is already listening on $PORT"; exit 1
fi

start_service() {
  "$BIN" --config "$CFG" >>"$LOG" 2>&1 &
  SRV=$!
  local READY="" end=$(( $(date +%s) + 60 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    READY=$(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)
    case "$READY" in *'"status":"ready"'*) return 0 ;; esac
    sleep 0.5
  done
  return 1
}
if ! start_service; then
  echo "  SERVICE NOT READY: $(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)"; tail -20 "$LOG"; exit 1
fi
echo "  service ready: $(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)"
# Prove the daemon we measure is the binary we built.
echo "  daemon exe hash: $(md5sum "/proc/$SRV/exe" 2>/dev/null | awk '{print $1}')  built: $(md5sum "$BIN" | awk '{print $1}')"

# ---- pick a free ephemeral high port for the HOST listener (and free it later) ----
HOST_PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
echo "=== host listener port: $HOST_PORT ==="
python3 -c "
import socket,time
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1)
s.bind(('127.0.0.1',$HOST_PORT)); s.listen(1); time.sleep(600)
" &
HOSTPID=$!
sleep 1
# Positive control: the host listener really is a listener on the host.
HOSTLISTEN=$(python3 -c "
import socket
s=socket.socket(); s.settimeout(0.5)
try:
    s.connect(('127.0.0.1',$HOST_PORT)); print('yes')
except Exception: print('no')
finally: s.close()
")
chkbool "host listener is really listening" "yes" "$HOSTLISTEN"

echo "=== creating workspaces A and B (different principals) ==="
WSA=$(curl -s -m 15 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" -d '{"name":"ports-a"}' | jf "['workspace']['id']")
WSB=$(curl -s -m 15 -X POST "$BASE/v1/workspaces" -H "$B" -H "$J" -d '{"name":"ports-b"}' | jf "['workspace']['id']")
[ -n "$WSA" ] && [ -n "$WSB" ] || { echo "FATAL: no workspace id"; exit 1; }
echo "  A=$WSA B=$WSB"

TCP_BIND='import socket,time
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind((ADDR,PORT)); s.listen(1); time.sleep(600)'
UDP_BIND='import socket,time
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
s.bind((ADDR,PORT)); time.sleep(600)'

# Start A's listeners. Each run's argv carries a unique token as its last
# element so the inventory match and the attribution assertion are exact.
TOK_LOOP="$NS-a-loop"
TOK_WILD="$NS-a-wild"
TOK_UDP="$NS-a-udp"
run_token "$A" "$WSA" "[\"python3\",\"-c\",\"import socket,time\ns=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1',$P_LOOP));s.listen(1);time.sleep(600)\",\"$TOK_LOOP\"]"
run_token "$A" "$WSA" "[\"python3\",\"-c\",\"import socket,time\ns=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('0.0.0.0',$P_WILD));s.listen(1);time.sleep(600)\",\"$TOK_WILD\"]"
run_token "$A" "$WSA" "[\"python3\",\"-c\",\"import socket,time\ns=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('0.0.0.0',$P_UDP));time.sleep(600)\",\"$TOK_UDP\"]"
# B binds the SAME loopback number, proving per-namespace isolation.
TOK_B="$NS-b-loop"
run_token "$B" "$WSB" "[\"python3\",\"-c\",\"import socket,time\ns=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1',$P_LOOP));s.listen(1);time.sleep(600)\",\"$TOK_B\"]"

# Wait for A's listeners to be visible.
LOOP_PID=""
for _ in $(seq 1 60); do
  LOOP_PID=$(proc_pid_by_token "$A" "$WSA" "$TOK_LOOP")
  [ -n "$LOOP_PID" ] && [ "$(has_port "$A" "$WSA" "$P_LOOP" tcp)" = yes ] && break
  sleep 0.5
done
[ -n "$LOOP_PID" ] || { echo "FATAL: A's loopback listener never appeared"; exit 1; }
echo "  A loopback listener pid=$LOOP_PID"

echo "=== 1) a loopback TCP listener with the right port/protocol/scope ==="
chk "A loopback protocol" "tcp" "$(port_field "$A" "$WSA" "$P_LOOP" tcp protocol)"
chk "A loopback scope" "loopback" "$(port_field "$A" "$WSA" "$P_LOOP" tcp scope)"
chk "A loopback address" "127.0.0.1" "$(port_field "$A" "$WSA" "$P_LOOP" tcp address)"
chk "A loopback state" "LISTEN" "$(port_field "$A" "$WSA" "$P_LOOP" tcp state)"

echo "=== 2) a wildcard TCP listener ==="
for _ in $(seq 1 40); do [ "$(has_port "$A" "$WSA" "$P_WILD" tcp)" = yes ] && break; sleep 0.5; done
chk "A wildcard port present" "yes" "$(has_port "$A" "$WSA" "$P_WILD" tcp)"
chk "A wildcard scope" "wildcard" "$(port_field "$A" "$WSA" "$P_WILD" tcp scope)"
chk "A wildcard address" "0.0.0.0" "$(port_field "$A" "$WSA" "$P_WILD" tcp address)"

echo "=== 3) a UDP socket, reported and NOT called LISTEN ==="
for _ in $(seq 1 40); do [ "$(has_port "$A" "$WSA" "$P_UDP" udp)" = yes ] && break; sleep 0.5; done
chk "A udp port present" "yes" "$(has_port "$A" "$WSA" "$P_UDP" udp)"
UDPS=$(port_field "$A" "$WSA" "$P_UDP" udp state)
chk "A udp is not LISTEN" "no" "$([ "$UDPS" = "LISTEN" ] && echo yes || echo no)"
chk "A udp state is the empty no-listen value" "" "$UDPS"

echo "=== 4) the owning process is resolved to the exact started pid ==="
chk "A loopback attributed" "true" "$(port_field "$A" "$WSA" "$P_LOOP" tcp attributed)"
chk "A loopback pid is the started pid" "$LOOP_PID" "$(port_field "$A" "$WSA" "$P_LOOP" tcp pid)"
chk "A loopback kind is run" "run" "$(port_field "$A" "$WSA" "$P_LOOP" tcp kind)"
# The endpoint's full argv must carry the unique token (exact attribution), and
# the human label (argv[0] basename + args, truncated) must be non-empty and
# name the interpreter.
chkbool "A loopback argv carries the run token" "yes" \
  "$(ports_json "$A" "$WSA" | grep -qF "$TOK_LOOP" && echo yes || echo no)"
LOOPLABEL=$(port_field "$A" "$WSA" "$P_LOOP" tcp label)
chkbool "A loopback label is a recognisable argv prefix" "yes" \
  "$(printf '%s' "$LOOPLABEL" | grep -q '^python3' && echo yes || echo no)"

echo "=== 5) ISOLATION: a HOST port is never reported in a workspace ==="
# The host listener on $HOST_PORT must not appear in A's or B's list. This is the
# assertion without which the endpoint could leak the host's and other tenants'
# services.
chk "host port not in A" "no" "$(has_port "$A" "$WSA" "$HOST_PORT" tcp)"
chk "host port not in B" "no" "$(has_port "$B" "$WSB" "$HOST_PORT" tcp)"
# And the whole list must not carry the host port as any field.
HOSTLEAK=$(ports_json "$A" "$WSA" | grep -cF "$HOST_PORT" || true)
chk "host port string absent from A's whole payload" "0" "$HOSTLEAK"

echo "=== 6) cross-tenant: two workspaces each report ONLY their own listener ==="
for _ in $(seq 1 40); do [ "$(has_port "$B" "$WSB" "$P_LOOP" tcp)" = yes ] && break; sleep 0.5; done
chk "B reports its own same-numbered listener" "yes" "$(has_port "$B" "$WSB" "$P_LOOP" tcp)"
B_PID=$(proc_pid_by_token "$B" "$WSB" "$TOK_B")
chk "B's 18080 is attributed to B's pid" "$B_PID" "$(port_field "$B" "$WSB" "$P_LOOP" tcp pid)"
chkbool "B's pid differs from A's pid" "yes" "$([ -n "$B_PID" ] && [ "$B_PID" != "$LOOP_PID" ] && echo yes || echo no)"
# A must NOT show B's wildcard/udp ports, and B must NOT show A's.
chk "B does not report A's wildcard port" "no" "$(has_port "$B" "$WSB" "$P_WILD" tcp)"
chk "B does not report A's udp port" "no" "$(has_port "$B" "$WSB" "$P_UDP" udp)"
chk "A does not report B's pid" "no" "$(ports_json "$A" "$WSA" | grep -qF "\"pid\":$B_PID" && echo yes || echo no)"

echo "=== 7) an empty workspace is an honest empty 200 ==="
WSC=$(curl -s -m 15 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" -d '{"name":"ports-empty"}' | jf "['workspace']['id']")
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "$A" "$BASE/v1/workspaces/$WSC/ports")
chk "empty workspace status" "200" "$CODE"
chk "empty workspace count" "0" "$(ports_json "$A" "$WSC" | jf "['count']")"
chk "empty workspace complete" "True" "$(ports_json "$A" "$WSC" | jf "['complete']")"

echo "=== 8) no stale cache: a new port appears; an exited port disappears ==="
# First read already happened above. Start a LATE listener and re-read.
TOK_LATE="$NS-a-late"
run_token "$A" "$WSA" "[\"python3\",\"-c\",\"import socket,time\ns=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1',$P_LATE));s.listen(1);time.sleep(600)\",\"$TOK_LATE\"]"
for _ in $(seq 1 40); do [ "$(has_port "$A" "$WSA" "$P_LATE" tcp)" = yes ] && break; sleep 0.5; done
chk "a port appearing after the first call is reported" "yes" "$(has_port "$A" "$WSA" "$P_LATE" tcp)"
# Now stop the process holding P_LOOP: its port must disappear.
CUR_PID=$(port_field "$A" "$WSA" "$P_LOOP" tcp pid)
if [ -n "$CUR_PID" ]; then
  # Find the process id (stable id) for that pid and stop its tree.
  PID_ID=$(curl -s -H "$A" "$BASE/v1/workspaces/$WSA/processes" | python3 -c "
import json,sys
try: p=[x for x in json.load(sys.stdin).get('processes',[]) if x.get('pid')==$CUR_PID]
except Exception: p=[]
print(p[0]['id'] if p else '')
")
  [ -n "$PID_ID" ] && curl -s -o /dev/null -m 20 -X POST "$BASE/v1/workspaces/$WSA/processes/$PID_ID/stop" -H "$A"
fi
GONE="no"
for _ in $(seq 1 40); do
  [ "$(has_port "$A" "$WSA" "$P_LOOP" tcp)" = no ] && { GONE="yes"; break; }
  sleep 0.5
done
chkbool "a port whose process exited disappears" "yes" "$GONE"
chk "the late port is still reported" "yes" "$(has_port "$A" "$WSA" "$P_LATE" tcp)"

echo "=== 9) complete is true and the list is deterministically sorted ==="
chk "A complete" "True" "$(ports_json "$A" "$WSA" | jf "['complete']")"
SORTED=$(ports_json "$A" "$WSA" | python3 -c "
import json,sys
try: ports=json.load(sys.stdin).get('ports',[])
except Exception: ports=[]
keys=[(e['port'],e['protocol']) for e in ports]
print('yes' if keys==sorted(keys) else 'no')
")
chk "A endpoints sorted by (port, protocol)" "yes" "$SORTED"
# Determinism: two consecutive reads give the identical order.
O1=$(ports_json "$A" "$WSA" | python3 -c "import json,sys;print([ (e['port'],e['protocol']) for e in json.load(sys.stdin).get('ports',[])])")
O2=$(ports_json "$A" "$WSA" | python3 -c "import json,sys;print([ (e['port'],e['protocol']) for e in json.load(sys.stdin).get('ports',[])])")
chk "two reads agree on order" "$O1" "$O2"

echo "=== 10) NOT part of the metrics stream; no port read on a tick ==="
# The metrics payload must carry no port data.
MSNAP=$(curl -s -H "$A" "$BASE/v1/workspaces/$WSA/metrics")
chkbool "metrics snapshot has RAM fields" "yes" "$(printf '%s' "$MSNAP" | grep -q 'ram_used_bytes' && echo yes || echo no)"
MPORT=$(printf '%s' "$MSNAP" | grep -cF "$P_LATE" || true)
chk "metrics payload carries no port number" "0" "$MPORT"
chkbool "metrics payload has no ports key" "yes" "$(printf '%s' "$MSNAP" | grep -q '"ports"' && echo no || echo yes)"
# The port-read counter must not move across metrics ticks, but must move on a
# /ports request (so the assertion is not vacuous).
RD0=$(curl -s -H "$A" "$BASE/v1/meta" | python3 -c "import json,sys;print(json.load(sys.stdin)['ports']['reads_total'])")
# Force a real subscription across at least two 1s ticks.
timeout 5 curl -s -N -H "$A" "$BASE/v1/metrics/stream" >/dev/null 2>&1 || true
RD1=$(curl -s -H "$A" "$BASE/v1/meta" | python3 -c "import json,sys;print(json.load(sys.stdin)['ports']['reads_total'])")
chk "no port read across metrics ticks" "$RD0" "$RD1"
# A /ports request DOES move the counter (proves the assertion is meaningful).
curl -s -o /dev/null -H "$A" "$BASE/v1/workspaces/$WSA/ports"
RD2=$(curl -s -H "$A" "$BASE/v1/meta" | python3 -c "import json,sys;print(json.load(sys.stdin)['ports']['reads_total'])")
chkbool "a /ports request moves the read counter" "yes" "$([ "$RD2" -gt "$RD1" ] && echo yes || echo no)"

echo "=== 11) /v1/meta advertises the capability ==="
META=$(curl -s -H "$A" "$BASE/v1/meta")
chk "meta ports enabled" "True" "$(printf '%s' "$META" | jf "['ports']['enabled']")"
chk "meta ports max_pids" "64" "$(printf '%s' "$META" | jf "['ports']['max_pids']")"
chkbool "meta ports carries a reachability note" "yes" \
  "$(printf '%s' "$META" | jf "['ports']['reachability']" | grep -qi 'not.*reachable\|reachable from outside' && echo yes || echo no)"

echo "=== 12) teardown and leak check ==="
# Terminate the host listener and free its port.
kill "$HOSTPID" 2>/dev/null; wait "$HOSTPID" 2>/dev/null; HOSTPID=""
HPFREE="no"
for _ in $(seq 1 20); do
  python3 -c "import socket;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
try:
    s.bind(('127.0.0.1',$HOST_PORT)); print('yes')
except OSError: print('no')
finally: s.close()" | grep -q yes && { HPFREE="yes"; break; }
  sleep 0.25
done
chkbool "the host port is free again" "yes" "$HPFREE"

# Delete every workspace, then stop the service gracefully.
for w in "$WSA" "$WSB" "$WSC"; do
  k="$A"; [ "$w" = "$WSB" ] && k="$B"
  curl -s -o /dev/null -m 40 -X DELETE "$BASE/v1/workspaces/$w" -H "$k" 2>/dev/null || true
done
sleep 2
kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; SRV=""
sleep 2

chk "no engine netns left" "0" "$(engine_netns)"
chk "no NSJAIL cgroup left" "0" "$(nsjail_leaves)"
chk "no workspace parent cgroup left" "0" "$(ws_parents)"
LEFT_MOUNTS=$(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" | wc -l | tr -d ' ')
chk "no mount left under the data dir" "0" "$LEFT_MOUNTS"
FUSE_NOW=$(fuse_count)
for _ in $(seq 1 20); do
  [ "${FUSE_NOW:-0}" -le "${FUSE_BASE:-0}" ] && break
  sleep 0.5; FUSE_NOW=$(fuse_count)
done
chkbool "no fuse2fs process left for this run (<= baseline)" "yes" \
  "$([ "${FUSE_NOW:-0}" -le "${FUSE_BASE:-0}" ] && echo yes || echo "no($FUSE_NOW>${FUSE_BASE})")"
chk "no marked workload process left" "0" "$(pgrep -fc "[s]andboxdports-" 2>/dev/null || true)"

echo
echo "================ ports acceptance: $PASS passed / $FAIL failed ================"
[ "$FAIL" -eq 0 ]
