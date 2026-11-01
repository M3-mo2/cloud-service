#!/usr/bin/env bash
# process-stop-check.sh -- real-nsjail acceptance for native process control.
#
# WHY THIS EXISTS
#   A process started inside a terminal session used to be invisible to the API
#   and unreachable: it could not be listed, counted or stopped, so "stop my
#   thing" (the single most common user action) did not work. Unit tests with a
#   stub runner cannot see that a jailed, setsid()-escaped process survives a
#   process-group kill; only the real nsjail + cgroup v2 can. This script runs
#   the real service against the host's REAL nsjail and asserts the whole
#   contract, including a forced daemon death and a restart.
#
# WHAT IT ASSERTS (every check is hard; the script exits non-zero on any failure,
# and no assertion is ever weakened to make a run pass):
#   1. a session-started detached process appears in GET .../processes with
#      kind and source_id set;
#   2. the implemented session-DELETE contract: DELETE ends the session and its
#      whole process tree, so the detached process it started is gone afterwards
#      (the contract is stated, then asserted exactly);
#   3. POST /workspaces/{id}/stop leaves ZERO processes for that workspace
#      (asserted with a bracketed pgrep and with the process inventory);
#   4. a setsid()-escaping workload is stopped by the single-process stop;
#   5. a process started in workspace A is NOT visible from workspace B (a
#      foreign principal is refused with 403);
#   6. the NSJAIL.* cgroup count does not grow across the run and returns to its
#      pre-run value;
#   7. after a forced daemon death (SIGKILL) and a restart, the orphan is reaped:
#      a second start must find no surviving workload and no leftover cgroup.
#
# REQUIREMENTS
#   Root (nsjail needs cgroup v2 and mount namespaces), the real nsjail binary
#   (NOT the test stub), the Go toolchain, python3 and curl. `sudo` resets PATH,
#   so the toolchain is resolved explicitly below.
#
# USAGE
#   sudo -E bash scripts/process-stop-check.sh
#   Environment: PORT (default 8099).
#
# EXIT
#   0 when every assertion passes; non-zero on any failure. When root, nsjail or
#   the toolchain is absent the script prints a SKIP line and exits 0: it cannot
#   prove anything without them.
set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: sudo -E bash scripts/process-stop-check.sh

Real-nsjail acceptance harness for native process inventory and stop. It builds
sandboxd, runs it under the host's real nsjail, and asserts that a session's
detached process is listed and stoppable, that a setsid()-escaping workload is
stopped by the single-process stop, that the workspace stop leaves zero
processes, that workspaces do not see each other's processes, that cgroups do not
accumulate, and that a restart reaps an orphan left by a forced daemon death.

Environment:
  PORT   port for the service (default 8099).

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
PORT=${PORT:-8099}
DATA=$(mktemp -d)
CFG=$(mktemp)
BIN=$(mktemp)
LOG=$(mktemp)
SRV=""
KEY=dev-key-please-rotate
OTHER_KEY=other-key
A="Authorization: Bearer $KEY"
B="Authorization: Bearer $OTHER_KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
PASS=0
FAIL=0
# Every marked workload is a python3 sleeper with a unique token as its last
# argv element, so the token really appears in the process's argv (a shell
# comment would be stripped). The token is what the bracketed pgrep and the
# inventory matcher look for; it never collides with this shell or the service.
NS="sandboxdpsc-$$"
PY="import time;time.sleep(900)"

chk() { # label expected actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}

jsonf() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)"; }

# count_ws_procs reads a workspace's process inventory and prints the entry
# count; a request error prints -1 so a caller can tell "no answer" from "zero".
count_ws_procs() { # key ws_id
  curl -s -H "$1" "$BASE/v1/workspaces/$2/processes" | python3 -c '
import json,sys
try:
    print(len(json.load(sys.stdin).get("processes",[])))
except Exception:
    print(-1)
'
}

# marked_proc prints "<id> <pid>" of a process in the workspace whose argv
# contains the token, preferring the python3 offender (not its sh wrapper), or an
# empty string. $3, when set to "pid", prints only the pid.
marked_proc() { # key ws_id token
  curl -s -H "$1" "$BASE/v1/workspaces/$2/processes" | python3 -c "
import json,sys
tok='$3'
try:
    procs=json.load(sys.stdin).get('processes',[])
except Exception:
    procs=[]
m=[p for p in procs if any(tok in a for a in p.get('argv',[]))]
# Prefer the python3 offender over the sh -c wrapper.
m.sort(key=lambda p: 0 if p.get('argv') and 'python3' in p['argv'][0] else 1)
print((m[0]['id']+' '+str(m[0]['pid'])) if m else '')
"
}

cg_before=$(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null | wc -l | tr -d ' ')

cleanup() {
  if [ -n "$SRV" ]; then
    kill "$SRV" 2>/dev/null
    wait "$SRV" 2>/dev/null
  fi
  # Kill only this run's marked workloads by exact pid (bracketed, never a bare
  # pattern). A foreign jail's process is never in this set, so another tenant's
  # work is never touched.
  for p in $(pgrep -f "[s]andboxdpsc-" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  sleep 0.5
  # Remove only cgroups that are now empty; a non-empty cgroup (a foreign jail
  # still running) is left alone.
  for d in $(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null); do
    if [ ! -s "$d/cgroup.procs" ]; then rmdir "$d" 2>/dev/null; fi
  done
  rm -rf "$DATA" "$CFG" "$BIN" "$LOG"
}
trap cleanup EXIT INT TERM

if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root (nsjail needs cgroup v2 and mount namespaces); re-run with: sudo -E bash scripts/process-stop-check.sh"
  exit 0
fi
command -v nsjail >/dev/null || { echo "SKIP: nsjail is not installed on this host"; exit 0; }
if ! getent group sandbox >/dev/null; then
  echo "=== creating the sandbox group/user ==="
  groupadd -r sandbox && useradd -r -g sandbox -M -s /usr/sbin/nologin sandbox
fi

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1

# Guard against silent rot: the tag mechanism and the new routes must exist in
# the source before the run starts.
[ -f "$ROOT/scripts/process-stop-check.sh" ] || { echo "FAIL  artifact: harness missing"; exit 1; }
grep -q "SANDBOXD_WORKSPACE" "$ROOT/internal/executor/process.go" ||
  { echo "FAIL  artifact: executor no longer injects the process tags"; exit 1; }
grep -q "TagEnv" "$ROOT/configs/nsjail_python.cfg.tmpl" ||
  { echo "FAIL  artifact: nsjail template no longer renders the process tags"; exit 1; }
grep -q "processes/{processID}/stop" "$ROOT/internal/api/router.go" ||
  { echo "FAIL  artifact: the single-process stop route is gone"; exit 1; }

echo "=== starting the service under real nsjail ==="
# The config trap: allow_unlimited_runs must appear in BOTH limits and
# max_limits. Written explicitly (never regex-patched from the example, which
# would produce a duplicate YAML key and read as a broken binary).
# Two principals are configured so the cross-owner check can expect 403 (with a
# single key it would be 401). data_dir is outside the repo. Port is 8099.
cat >"$CFG" <<YAML
server: { addr: "127.0.0.1:$PORT" }
storage: { data_dir: "$DATA" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: 1, memory_mb: 512, disk_mb: 256, max_wall_time_sec: 600, max_runs: 5, allow_net: true, allow_unlimited_runs: true }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true, allow_unlimited_runs: true }
caps: { exec_stdout_kb: 256, exec_stderr_kb: 256, run_log_mb: 5, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 5 }
sessions: { max_per_workspace: 4, idle_timeout_sec: 600, lifetime_sec: 1800, start_probe_ms: 800 }
keys:
  - { principal_id: "user_dev", key: "$KEY" }
  - { principal_id: "user_other", key: "$OTHER_KEY" }
YAML

start_service() {
  "$BIN" --config "$CFG" >>"$LOG" 2>&1 &
  SRV=$!
  READY=""
  for _ in $(seq 1 60); do
    READY=$(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)
    case "$READY" in *'"status":"ready"'*) return 0 ;; esac
    sleep 0.5
  done
  return 1
}

if ! start_service; then
  echo "  SERVICE NOT READY: $READY"; tail -20 "$LOG"; exit 1
fi
echo "  service ready: $READY"

echo "=== 1) a session-started detached process is listed with kind/source_id ==="
WSA=$(curl -s -m 15 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" -d '{"name":"proc-a"}' | jsonf "['workspace']['id']")
[ -n "$WSA" ] || { echo "FATAL: no workspace A id"; exit 1; }
SES=$(curl -s -m 30 -X POST "$BASE/v1/workspaces/$WSA/sessions" -H "$A" -H "$J" -d '{"cols":80,"rows":24}' | jsonf "['session']['id']")
echo "  workspace A=$WSA session=$SES"
sleep 2
TOK1="$NS-det"
# A detached python3 sleeper started inside the session; it inherits the session
# tag and its argv carries TOK1.
curl -s -o /dev/null -m 20 -H "$A" -H "$J" -d "{\"data\":\"python3 -c '$PY' $TOK1 &\\n\"}" \
  "$BASE/v1/workspaces/$WSA/sessions/$SES/input"
DET_ID=""
for _ in $(seq 1 40); do
  DET=$(marked_proc "$A" "$WSA" "$TOK1"); DET_ID=$(printf '%s' "$DET" | awk '{print $1}')
  [ -n "$DET_ID" ] && break
  sleep 0.5
done
chk "detached process listed" "yes" "$([ -n "$DET_ID" ] && echo yes || echo no)"
DET_JSON=$(curl -s -H "$A" "$BASE/v1/workspaces/$WSA/processes" | python3 -c "
import json,sys
tok='$TOK1'
d=json.load(sys.stdin)['processes']
m=[p for p in d if any(tok in a for a in p.get('argv',[]))]
print((m[0]['kind']+' '+m[0]['source_id']) if m else ' ')
")
chk "detached process kind" "session" "$(printf '%s' "$DET_JSON" | awk '{print $1}')"
chk "detached process source_id" "$SES" "$(printf '%s' "$DET_JSON" | awk '{print $2}')"

echo "=== 2) DELETE of the session ends its whole process tree (the contract) ==="
# Contract: DELETE ends the session AND its process tree -- the executor reaps the
# session cgroup on session end -- so a process the session started cannot
# outlive it. Asserted against the inventory and a bracketed pgrep.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 30 -X DELETE "$BASE/v1/workspaces/$WSA/sessions/$SES" -H "$A")
chk "session delete" "200" "$CODE"
sleep 3
LEFT=$(count_ws_procs "$A" "$WSA")
chk "session delete leaves zero processes" "0" "$LEFT"
SURV=$(pgrep -fc "[s]andboxdpsc-" 2>/dev/null || true)
chk "no surviving marked process (bracketed pgrep)" "0" "$([ -z "$SURV" ] && echo 0 || echo "$SURV")"

echo "=== 3) POST /workspaces/{id}/stop leaves zero processes for the workspace ==="
TOK2="$NS-run"
TOK3="$NS-ses"
curl -s -o /dev/null -m 20 -X POST "$BASE/v1/workspaces/$WSA/runs" -H "$A" -H "$J" \
  -d "{\"kind\":\"command\",\"argv\":[\"python3\",\"-c\",\"$PY\",\"$TOK2\"],\"no_timeout\":true}"
SES2=$(curl -s -m 30 -X POST "$BASE/v1/workspaces/$WSA/sessions" -H "$A" -H "$J" -d '{"cols":80,"rows":24}' | jsonf "['session']['id']")
sleep 1
curl -s -o /dev/null -m 20 -H "$A" -H "$J" -d "{\"data\":\"python3 -c '$PY' $TOK3 &\\n\"}" \
  "$BASE/v1/workspaces/$WSA/sessions/$SES2/input"
sleep 3
BEFORE=$(count_ws_procs "$A" "$WSA")
chk "workspace has live processes before stop" "yes" "$([ "$BEFORE" -ge 2 ] && echo yes || echo "no($BEFORE)")"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 40 -X POST "$BASE/v1/workspaces/$WSA/stop" -H "$A")
chk "workspace stop returned 200" "200" "$CODE"
sleep 3
LEFT=$(count_ws_procs "$A" "$WSA")
chk "workspace stop leaves zero processes" "0" "$LEFT"
SURV=$(pgrep -fc "[s]andboxdpsc-" 2>/dev/null || true)
chk "workspace stop: no marked survivor" "0" "$([ -z "$SURV" ] && echo 0 || echo "$SURV")"

echo "=== 4) a setsid()-escaping workload is stopped by the single-process stop ==="
TOK4="$NS-esc"
curl -s -o /dev/null -m 20 -X POST "$BASE/v1/workspaces/$WSA/runs" -H "$A" -H "$J" \
  -d "{\"kind\":\"command\",\"argv\":[\"sh\",\"-c\",\"setsid python3 -c '$PY' $TOK4 </dev/null >/dev/null 2>&1 & python3 -c '$PY' $NS-keep\"],\"no_timeout\":true}"
ESC_ID=""; ESC_PID=""
for _ in $(seq 1 40); do
  ESC=$(marked_proc "$A" "$WSA" "$TOK4")
  ESC_ID=$(printf '%s' "$ESC" | awk '{print $1}'); ESC_PID=$(printf '%s' "$ESC" | awk '{print $2}')
  [ -n "$ESC_ID" ] && break
  sleep 0.5
done
chk "setsid-escaped process listed" "yes" "$([ -n "$ESC_ID" ] && echo yes || echo no)"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 30 -X POST "$BASE/v1/workspaces/$WSA/processes/$ESC_ID/stop" -H "$A")
chk "single-process stop status" "200" "$CODE"
sleep 2
if [ -n "$ESC_PID" ]; then
  ALIVE=$(kill -0 "$ESC_PID" 2>/dev/null && echo alive || echo gone)
else
  ALIVE="gone"
fi
chk "setsid-escaped process stopped" "gone" "$ALIVE"
# The whole run tree is gone (the cgroup kill also stopped the sibling).
curl -s -o /dev/null -m 40 -X POST "$BASE/v1/workspaces/$WSA/stop" -H "$A"
sleep 2
chk "workspace empty after the single stop + cleanup" "0" "$(count_ws_procs "$A" "$WSA")"

echo "=== 5) isolation: workspace A's processes are not visible from workspace B ==="
WSB=$(curl -s -m 15 -X POST "$BASE/v1/workspaces" -H "$B" -H "$J" -d '{"name":"proc-b"}' | jsonf "['workspace']['id']")
chk "foreign inventory refused" "403" "$(curl -s -o /dev/null -w '%{http_code}' -H "$B" "$BASE/v1/workspaces/$WSA/processes")"
chk "foreign process stop refused" "403" "$(curl -s -o /dev/null -w '%{http_code}' -m 20 -X POST "$BASE/v1/workspaces/$WSA/processes/does-not-exist/stop" -H "$B")"
TOK5="$NS-iso"
curl -s -o /dev/null -m 20 -X POST "$BASE/v1/workspaces/$WSA/runs" -H "$A" -H "$J" \
  -d "{\"kind\":\"command\",\"argv\":[\"python3\",\"-c\",\"$PY\",\"$TOK5\"],\"no_timeout\":true}"
sleep 3
AW=$(count_ws_procs "$A" "$WSA")
BW=$(count_ws_procs "$B" "$WSB")
chk "workspace A sees its own process" "yes" "$([ "$AW" -ge 1 ] && echo yes || echo "no($AW)")"
chk "workspace B inventory empty" "0" "$BW"
curl -s -o /dev/null -m 40 -X POST "$BASE/v1/workspaces/$WSA/stop" -H "$A"
sleep 2

echo "=== 6) NSJAIL cgroup count returns to its pre-run value ==="
sleep 1
cg_after=$(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null | wc -l | tr -d ' ')
chk "cgroups did not grow past baseline" "no" "$([ "$cg_after" -gt "$cg_before" ] && echo yes || echo no)"
chk "cgroups back to pre-run value" "$cg_before" "$cg_after"

echo "=== 7) a forced daemon death + restart reaps the orphan ==="
WS7=$(curl -s -m 15 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" -d '{"name":"orphan"}' | jsonf "['workspace']['id']")
TOK7="$NS-orphan"
curl -s -o /dev/null -m 20 -X POST "$BASE/v1/workspaces/$WS7/runs" -H "$A" -H "$J" \
  -d "{\"kind\":\"command\",\"argv\":[\"python3\",\"-c\",\"$PY\",\"$TOK7\"],\"no_timeout\":true}"
sleep 3
ORPH=$(pgrep -fc "[s]andboxdpsc-" 2>/dev/null || true)
chk "marked workload alive before the kill" "yes" "$([ -n "$ORPH" ] && [ "$ORPH" -ge 1 ] && echo yes || echo "no($ORPH)")"

kill -9 "$SRV" 2>/dev/null
wait "$SRV" 2>/dev/null
SRV=""
sleep 2
NSPRE=$(pgrep -xc nsjail 2>/dev/null || true)
chk "nsjail survived the hard daemon death" "1" "$([ -z "$NSPRE" ] && echo 0 || echo "$NSPRE")"

echo "  restarting the daemon (the orphan should be reaped on the way in) ..."
if ! start_service; then
  echo "  RESTART NOT READY: $READY"; tail -20 "$LOG"; exit 1
fi
sleep 3
NSLEFT=$(pgrep -xc nsjail 2>/dev/null || true)
chk "nsjail reaped on restart" "0" "$([ -z "$NSLEFT" ] && echo 0 || echo "$NSLEFT")"
ORPH2=$(pgrep -fc "[s]andboxdpsc-" 2>/dev/null || true)
chk "orphan workload reaped on restart" "0" "$([ -z "$ORPH2" ] && echo 0 || echo "$ORPH2")"
cg_final=$(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null | wc -l | tr -d ' ')
chk "orphan cgroup removed on restart" "$cg_before" "$cg_final"

echo
echo "================ process-stop acceptance: $PASS passed / $FAIL failed ================"
[ "$FAIL" -eq 0 ]
