#!/usr/bin/env bash
# spread-check.sh -- real-nsjail acceptance for the per-WORKSPACE resource ceiling.
#
# WHY THIS EXISTS
#   The workspace's RAM/CPU/pids ceiling used to be PER RUN: nsjail created one
#   cgroup per launch at /sys/fs/cgroup/NSJAIL.<pid>, each with
#   memory.max = memory_mb, so a workspace with N concurrent runs could consume
#   N x memory_mb against an advertised memory_mb. Measured before the fix: one
#   workspace at memory_mb 128 with three runs produced THREE cgroups each with
#   memory.max 134217728 -- 384 MB advertised as 128 MB.
#
#   The fix gives each workspace ONE parent cgroup (ws-<ULID>) whose memory.max
#   is the advertised ceiling, and points nsjail at it via cgroupv2_mount so
#   every run's leaf cgroup is created INSIDE it. The parent enforces the SUM.
#
#   This gate proves the change, not just the new state: it runs THREE
#   concurrent allocations whose SUM exceeds the workspace's memory_mb, and a
#   single allocation of 2 x memory_mb, and asserts each is REFUSED (OOM-killed)
#   while the advertised number is the enforced number.
#
# WHAT IT ASSERTS (every check is hard; exits non-zero on any failure):
#   1. creating a workspace and running in it creates exactly ONE workspace
#      parent cgroup (ws-<id>) with the delegated controllers enabled;
#   2. a run's leaf cgroup appears INSIDE that parent, and ZERO NSJAIL.* exist
#      directly under /sys/fs/cgroup while runs are live;
#   3. THE CENTRAL ASSERTION: with THREE concurrent runs whose SUM exceeds the
#      workspace's memory_mb, the parent's memory.max == memory_mb, the API's
#      ram_limit_bytes == memory_mb, and the workload is REFUSED (OOM-killed,
#      run status `oom`) rather than succeeding -- a single run of 2 x
#      memory_mb is likewise refused;
#   4. ram_used <= ram_limit for a live workspace under load (the contract the
#      tenant sees; it used to read ram_percent > 100%);
#   5. a setsid()-escaped process is still killed when the workspace is stopped
#      (the kill path survived the layout change);
#   6. deleting the workspace removes its parent cgroup and leaves no NSJAIL.*
#      and no ws-* parent behind;
#   7. the disk budget is the workspace's OWN allowance: a write past disk_mb is
#      refused at the boundary (the mechanism stays pinned).
#
# REQUIREMENTS
#   Root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk),
#   the real nsjail binary, fuse2fs, jq, python3 and the Go toolchain.
#
# USAGE
#   sudo -E bash scripts/spread-check.sh
#   Environment: PORT (default 8094).
#
# EXIT
#   0 when every assertion passes; non-zero on any failure. When root, nsjail or
#   the toolchain is absent the script prints a SKIP line and exits 0.
set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: sudo -E bash scripts/spread-check.sh

Real-nsjail acceptance for the per-workspace resource ceiling: one parent cgroup
per workspace, nsjail pointed at it, the SUM across concurrent runs bounded by
the advertised memory_mb, ram_used <= ram_limit, the setsid kill path intact,
and the parent removed on delete.

Environment:
  PORT   port for the service (default 8094).

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
PORT=${PORT:-8094}
DATA=$(mktemp -d)
CFG=$(mktemp)
BIN=$(mktemp)
LOG=$(mktemp)
SRV=""
KEY=dev-key-please-rotate
A="Authorization: Bearer $KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
PASS=0
FAIL=0
# M is the workspace memory_mb this gate runs at.
M=128
M_BYTES=$((M * 1024 * 1024))
# A marker unique to this run so the bracketed pgrep never matches this shell.
NS="sandboxdspread-$$"

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

# Counts, in the CURRENT layout, the live nsjail leaf cgroups (nested under a
# workspace parent) and the workspace parents. Both are the honest leak check.
ws_parents() { ls -d /sys/fs/cgroup/ws-* 2>/dev/null | wc -l | tr -d ' '; }
root_leaves() { ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null | wc -l | tr -d ' '; }
nested_leaves_for() { ls -d "/sys/fs/cgroup/ws-$1"/NSJAIL.* 2>/dev/null | wc -l | tr -d ' '; }
fuse_count() { ps -eo comm --no-headers | grep -cx fuse2fs; }

# oomcount -> the workspace parent's cumulative OOM count. It reads
# memory.events (descendant-inclusive: a parent has no processes of its own) and
# sums oom_kill and oom_group_kill, because with memory.oom.group=1 the group
# kill is recorded as oom_group_kill (measured).
oomcount() {
  local f="/sys/fs/cgroup/ws-$1/memory.events"
  [ -r "$f" ] || { echo 0; return; }
  awk '/^oom_kill /{a=$2} /^oom_group_kill /{g=$2} END{print a+g+0}' "$f"
}

# snap_ws_field WS FIELD -> a field of the workspace's one-shot metrics frame.
snap_ws_field() {
  curl -s -H "$A" "$BASE/v1/workspaces/$1/metrics" \
    | python3 -c "import json,sys
try:
    d=json.load(sys.stdin)
    for w in d.get('workspaces',[]):
        if w.get('workspace_id')=='$1': print(w.get('$2','')); break
except Exception: pass"
}

# run_status WS RUN -> the run's status (or "").
run_status() { curl -s -H "$A" "$BASE/v1/workspaces/$1/runs/$2" | python3 -c "import json,sys
try: print(json.load(sys.stdin)['run']['status'])
except Exception: pass"; }

# all_run_statuses WS -> every run status, one per line.
all_run_statuses() { curl -s -H "$A" "$BASE/v1/workspaces/$1/runs" | python3 -c "import json,sys
try:
    for r in json.load(sys.stdin).get('runs',[]): print(r['status'])
except Exception: pass"; }

cleanup() {
  if [ -n "$SRV" ]; then
    kill "$SRV" 2>/dev/null
    wait "$SRV" 2>/dev/null
  fi
  # Unmount any of this run's FUSE mounts before removing the data dir.
  for m in $(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" || true); do
    fusermount3 -u -z "$m" 2>/dev/null || true
  done
  # Kill only this run's marked workloads by exact pid (bracketed).
  for p in $(pgrep -f "[s]andboxdspread-" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  sleep 0.5
  # Remove only EMPTY cgroups: nested leaves first, then empty parents, then any
  # root-level leaf. A non-empty cgroup (a foreign jail) is left alone.
  for d in $(ls -d /sys/fs/cgroup/ws-*/NSJAIL.* 2>/dev/null); do
    if [ ! -s "$d/cgroup.procs" ]; then rmdir "$d" 2>/dev/null; fi
  done
  for d in $(ls -d /sys/fs/cgroup/ws-* 2>/dev/null); do
    if [ ! -s "$d/cgroup.procs" ]; then rmdir "$d" 2>/dev/null; fi
  done
  for d in $(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null); do
    if [ ! -s "$d/cgroup.procs" ]; then rmdir "$d" 2>/dev/null; fi
  done
  rm -rf "$DATA" "$CFG" "$BIN" "$LOG"
}
trap cleanup EXIT INT TERM

if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk); re-run with: sudo -E bash scripts/spread-check.sh"
  exit 0
fi
command -v nsjail >/dev/null || { echo "SKIP: nsjail is not installed on this host"; exit 0; }
command -v fuse2fs >/dev/null || { echo "SKIP: fuse2fs is not installed on this host"; exit 0; }
command -v jq >/dev/null || { echo "SKIP: jq is not installed on this host"; exit 0; }
command -v python3 >/dev/null || { echo "SKIP: python3 is not installed on this host"; exit 0; }
if ! getent group sandbox >/dev/null; then
  echo "=== creating the sandbox group/user ==="
  groupadd -r sandbox && useradd -r -g sandbox -M -s /usr/sbin/nologin sandbox
fi

# Guard against silent rot: the mechanism must exist in the source before the
# run starts, so a deleted implementation cannot pass by absence.
[ -f "$ROOT/internal/executor/cgroup.go" ] || { echo "FAIL  artifact: cgroup.go missing"; exit 1; }
grep -q "WorkspaceCgroupPrefix" "$ROOT/internal/executor/cgroup.go" ||
  { echo "FAIL  artifact: the workspace parent cgroup is gone"; exit 1; }
grep -q "cgroupv2_mount" "$ROOT/configs/nsjail_python.cfg.tmpl" ||
  { echo "FAIL  artifact: nsjail is no longer pointed at the workspace parent"; exit 1; }

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1

echo "=== starting the service under real nsjail ==="
# One small workspace ceiling (128 MiB), a small disk, and enough runs to hold
# three concurrent allocations.
cat >"$CFG" <<YAML
server: { addr: "127.0.0.1:$PORT" }
storage: { data_dir: "$DATA" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: 1, memory_mb: $M, disk_mb: 64, max_wall_time_sec: 600, max_runs: 8, allow_net: false, allow_unlimited_runs: true }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true, allow_unlimited_runs: true }
caps: { exec_stdout_kb: 64, exec_stderr_kb: 64, run_log_mb: 2, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 8 }
disk: { enabled: true, backend: "auto" }
keys:
  - { principal_id: "user_dev", key: "$KEY" }
YAML

if curl -s -m 1 -o /dev/null "$BASE/healthz" 2>/dev/null; then
  echo "FATAL: something is already listening on $PORT"; exit 1
fi

start_service() {
  "$BIN" --config "$CFG" >>"$LOG" 2>&1 &
  SRV=$!
  local READY="" end=$(( $(date +%s) + 40 ))
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

W=$(curl -s -m 15 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" -d "{\"name\":\"spread\",\"limits\":{\"memory_mb\":$M,\"allow_unlimited_runs\":true}}" | jf "['workspace']['id']")
[ -n "$W" ] || { echo "FATAL: no workspace id"; exit 1; }
echo "  workspace=$W memory_mb=$M"
FUSE_BASE=$(fuse_count)

# ---- 1) exactly one workspace parent cgroup, with controllers delegated ----
echo "=== 1) one parent cgroup per workspace, controllers delegated ==="
# Provisioning happens on the first run/exec; kick a trivial exec first.
curl -s -o /dev/null -m 30 -X POST "$BASE/v1/workspaces/$W/exec" -H "$A" -H "$J" -d '{"cmd":["true"]}'
PFOUND="no"
for _ in $(seq 1 20); do
  [ -d "/sys/fs/cgroup/ws-$W" ] && { PFOUND="yes"; break; }; sleep 0.3
done
chkbool "workspace parent cgroup created" "yes" "$PFOUND"
# Exactly one parent for this workspace.
ONE=$(ls -d "/sys/fs/cgroup/ws-$W" 2>/dev/null | wc -l | tr -d ' ')
chk "exactly one parent cgroup for the workspace" "1" "$ONE"
CONT=$(cat "/sys/fs/cgroup/ws-$W/cgroup.subtree_control" 2>/dev/null | tr -s ' ' | sed 's/^ //')
chk "parent delegates cpu/memory/pids" "cpu memory pids" "$CONT"
chk "parent enforces the advertised memory_mb" "$M_BYTES" "$(cat "/sys/fs/cgroup/ws-$W/memory.max" 2>/dev/null)"
chk "parent gives the workspace no swap" "0" "$(cat "/sys/fs/cgroup/ws-$W/memory.swap.max" 2>/dev/null)"
chk "parent kills the whole group on OOM" "1" "$(cat "/sys/fs/cgroup/ws-$W/memory.oom.group" 2>/dev/null)"

# ---- 2) the leaf is nested; zero leaves at the root ----
echo "=== 2) run leaf cgroup is nested inside the parent; zero at the root ==="
R0=$(curl -s -m 20 -X POST "$BASE/v1/workspaces/$W/runs" -H "$A" -H "$J" \
  -d "{\"kind\":\"command\",\"argv\":[\"python3\",\"-c\",\"import time;time.sleep(25)\",\"$NS-hold\"],\"no_timeout\":true}" | jf "['run']['id']")
NEST="no"
for _ in $(seq 1 40); do
  n=$(nested_leaves_for "$W")
  [ "${n:-0}" -ge 1 ] && { NEST="yes"; break; }
  sleep 0.3
done
chkbool "run leaf cgroup nested inside the parent" "yes" "$NEST"
chk "zero NSJAIL.* directly under /sys/fs/cgroup while live" "0" "$(root_leaves)"
curl -s -o /dev/null -m 30 -X POST "$BASE/v1/workspaces/$W/runs/$R0/stop" -H "$A" 2>/dev/null || true
sleep 1

# ---- 3) THE CENTRAL ASSERTION: the SUM ceiling is enforced ----
echo "=== 3) central: SUM across concurrent runs is capped at memory_mb ==="
# One run allocating 2 x M must be REFUSED (OOM-killed), not succeed. The
# allocation is touched page by page so it counts against memory.max.
R2M=$(curl -s -m 20 -X POST "$BASE/v1/workspaces/$W/runs" -H "$A" -H "$J" \
  -d "{\"kind\":\"command\",\"argv\":[\"python3\",\"-c\",\"x=bytearray($((M*2))*1024*1024);[x.__setitem__(i,1) for i in range(0,len(x),4096)];print('ALLOC_OK')\"],\"no_timeout\":true}" | jf "['run']['id']")
ST=""
for _ in $(seq 1 120); do
  ST=$(run_status "$W" "$R2M")
  case "$ST" in exited|killed|timeout|failed|interrupted|oom) break;; esac
  sleep 0.5
done
# A process holding 2 x M must NOT read as a clean success.
chk "single 2 x memory_mb allocation is refused (status != exited)" "yes" \
  "$([ "$ST" != "exited" ] && echo yes || echo "no($ST)")"
chk "single 2 x memory_mb allocation reads as oom" "oom" "$ST"
OOM1=$(oomcount "$W")
chkbool "parent recorded an oom_kill (the ceiling fired)" "yes" "$([ "${OOM1:-0}" -ge 1 ] && echo yes || echo "no($OOM1)")"

# THREE concurrent runs, each of 0.9 x M: the SUM ~2.7 x M exceeds M, so the
# workspace must be killed despite each run fitting alone. Before the fix the
# three runs would each have had their own M ceiling and all three would succeed.
# Each run SLEEPS first and allocates second, so all three leaves are live (and
# observable) before the ceiling can fire.
echo "  launching three concurrent ~0.9M allocations (SUM exceeds M) ..."
RIDS=""
for i in 1 2 3; do
  rid=$(curl -s -m 20 -X POST "$BASE/v1/workspaces/$W/runs" -H "$A" -H "$J" \
    -d "{\"kind\":\"command\",\"argv\":[\"python3\",\"-c\",\"import time;time.sleep(4);x=bytearray($((M*9/10))*1024*1024);[x.__setitem__(q,1) for q in range(0,len(x),4096)];print('ALLOC_OK');time.sleep(25)\",\"$NS-sum$i\"],\"no_timeout\":true}" | jf "['run']['id']")
  RIDS="$RIDS $rid"
  sleep 0.2
done
# Observe the leaves while all three runs are alive (before they allocate).
PEAK=0
LIVE_SEEN=0
for _ in $(seq 1 60); do
  cur=$(cat "/sys/fs/cgroup/ws-$W/memory.current" 2>/dev/null || echo 0)
  [ "${cur:-0}" -gt "$PEAK" ] && PEAK=$cur
  n=$(nested_leaves_for "$W")
  [ "${n:-0}" -ge 2 ] && LIVE_SEEN=1
  # Stop early once the ceiling has fired (the parent recorded an OOM).
  now=$(oomcount "$W")
  [ "${now:-0}" -gt "${OOM1:-0}" ] && break
  sleep 0.3
done
chk "parent memory.max == the workspace's memory_mb" "$M_BYTES" "$(cat "/sys/fs/cgroup/ws-$W/memory.max" 2>/dev/null)"
chk "API ram_limit_bytes == the workspace's memory_mb" "$M_BYTES" "$(snap_ws_field "$W" ram_limit_bytes)"
USED=$(snap_ws_field "$W" ram_used_bytes); USED=${USED:-0}
LIM=$(snap_ws_field "$W" ram_limit_bytes); LIM=${LIM:-$M_BYTES}
chkbool "ram_used <= ram_limit under load" "yes" \
  "$(awk "BEGIN{exit !($USED <= $LIM)}" && echo yes || echo "no(used=$USED lim=$LIM)")"
PCT=$(snap_ws_field "$W" ram_percent); PCT=${PCT:-0}
chkbool "ram_percent <= 100 under load" "yes" \
  "$(awk "BEGIN{exit !($PCT <= 100)}" && echo yes || echo "no(pct=$PCT)")"
OOM2=$(oomcount "$W")
chkbool "the SUM ceiling fired an oom_kill" "yes" "$([ "${OOM2:-0}" -gt "${OOM1:-0}" ] && echo yes || echo "no($OOM1->$OOM2)")"
# At least one of the concurrent runs must read as oom (not a clean exit).
for _ in $(seq 1 60); do
  STATUSES=$(all_run_statuses "$W")
  case "$STATUSES" in *oom*) break;; esac
  sleep 0.5
done
chkbool "a concurrent run reads as oom (not exited)" "yes" \
  "$(printf '%s\n' "$STATUSES" | grep -qx oom && echo yes || echo no)"
# This is the assertion that PROVES THE CHANGE: at least two runs really were
# live at once (the SUM scenario happened) AND the ceiling fired. Under the old
# per-run layout each run had its own M ceiling, so three runs x <M would all
# have succeeded and nothing would have been killed.
chkbool "two or more runs were concurrently live (the SUM scenario happened)" "yes" \
  "$([ "${LIVE_SEEN:-0}" = 1 ] && echo yes || echo no)"
# The parent's peak memory.current must stay near the ceiling, not at 3 x M. A
# small tolerance absorbs the transient overage a cgroup can show before reclaim
# completes; the old per-run layout would have reached ~2.7 x M.
chkbool "peak summed usage stayed near the ceiling (not ~3 x M)" "yes" \
  "$(awk "BEGIN{exit !(${PEAK:-0} <= $((M_BYTES * 11 / 10)))}" && echo yes || echo "no(peak=$PEAK lim=$M_BYTES)")"
curl -s -o /dev/null -m 40 -X POST "$BASE/v1/workspaces/$W/stop" -H "$A"
sleep 2

# ---- 4) ram_used <= ram_limit (also proven above under load) ----
echo "=== 4) the tenant's contract: ram_used <= ram_limit ==="
R3=$(curl -s -m 20 -X POST "$BASE/v1/workspaces/$W/runs" -H "$A" -H "$J" \
  -d "{\"kind\":\"command\",\"argv\":[\"python3\",\"-c\",\"import time;x=bytearray(40*1024*1024);time.sleep(15)\",\"$NS-contract\"],\"no_timeout\":true}" | jf "['run']['id']")
for _ in $(seq 1 40); do
  U=$(snap_ws_field "$W" ram_used_bytes)
  [ -n "$U" ] && [ "${U:-0}" -gt 0 ] && break
  sleep 0.5
done
U=$(snap_ws_field "$W" ram_used_bytes); U=${U:-0}
L=$(snap_ws_field "$W" ram_limit_bytes); L=${L:-$M_BYTES}
chkbool "ram_used <= ram_limit for a live workspace" "yes" \
  "$(awk "BEGIN{exit !($U <= $L)}" && echo yes || echo "no(used=$U lim=$L)")"
chkbool "ram_limit_bytes is still the enforced M" "yes" \
  "$([ "$L" = "$M_BYTES" ] && echo yes || echo "no($L)")"
curl -s -o /dev/null -m 40 -X POST "$BASE/v1/workspaces/$W/stop" -H "$A"
sleep 2

# ---- 4b) the disk budget is the workspace's OWN allowance ----
echo "=== 4b) the disk budget is the workspace's own allowance ==="
# The workspace is 64 MiB. A write that would need more is refused at the
# boundary (ENOSPC / a non-zero exit), and the workspace's own df agrees with
# the engine's reported disk limit. This pins the disk mechanism (already
# correct) so it cannot silently regress.
DF_LIMIT=$(curl -s -X POST "$BASE/v1/workspaces/$W/exec" -H "$A" -H "$J" \
  --data-binary '{"cmd":["sh","-c","df -B1 --output=size /workspace | tail -1"]}' | jf "['stdout']" | tr -d ' \n')
chk "workspace df size equals disk_mb" "$((64 * 1024 * 1024))" "$DF_LIMIT"
# Write 100 MiB: must fail (the allowance is 64 MiB).
OVER=$(curl -s -X POST "$BASE/v1/workspaces/$W/exec" -H "$A" -H "$J" \
  --data-binary '{"cmd":["sh","-c","dd if=/dev/zero of=/workspace/over.bin bs=1M count=100 2>&1; echo RC=$?"]}' | jf "['stdout']")
chkbool "a write past disk_mb is refused (No space left / non-zero)" "yes" \
  "$(printf '%s' "$OVER" | grep -qiE 'No space left|RC=[1-9]' && echo yes || echo "no($OVER)")"
# The engine's reported disk limit matches the allowance.
DL=$(snap_ws_field "$W" disk_limit_bytes)
chk "reported disk_limit_bytes == disk_mb" "$((64 * 1024 * 1024))" "$DL"
curl -s -X POST "$BASE/v1/workspaces/$W/exec" -H "$A" -H "$J" \
  --data-binary '{"cmd":["sh","-c","rm -f /workspace/over.bin"]}' >/dev/null 2>&1 || true

# ---- 5) a setsid()-escaped process is still killed on stop ----
echo "=== 5) setsid()-escaped process is still killed (the kill path survived) ==="
TOK="$NS-esc"
curl -s -o /dev/null -m 20 -X POST "$BASE/v1/workspaces/$W/runs" -H "$A" -H "$J" \
  -d "{\"kind\":\"command\",\"argv\":[\"sh\",\"-c\",\"setsid python3 -c 'import time;time.sleep(300)' $TOK </dev/null >/dev/null 2>&1 & python3 -c 'import time;time.sleep(300)' $NS-keep\"],\"no_timeout\":true}"
ESC_PID=""
for _ in $(seq 1 40); do
  ESC_PID=$(curl -s -H "$A" "$BASE/v1/workspaces/$W/processes" | python3 -c "
import json,sys
tok='$TOK'
try:
    for p in json.load(sys.stdin).get('processes',[]):
        if any(tok in a for a in p.get('argv',[])): print(p['pid']); break
except Exception: pass")
  [ -n "$ESC_PID" ] && break
  sleep 0.5
done
chkbool "setsid-escaped process listed" "yes" "$([ -n "$ESC_PID" ] && echo yes || echo no)"
curl -s -o /dev/null -m 40 -X POST "$BASE/v1/workspaces/$W/stop" -H "$A"
sleep 3
if [ -n "$ESC_PID" ]; then
  ALIVE=$(kill -0 "$ESC_PID" 2>/dev/null && echo alive || echo gone)
else
  ALIVE="gone"
fi
chk "setsid-escaped process killed by the workspace stop" "gone" "$ALIVE"

# ---- 6) delete removes the parent cgroup and leaves nothing behind ----
echo "=== 6) delete removes the parent cgroup and leaves nothing behind ==="
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 40 -X DELETE "$BASE/v1/workspaces/$W" -H "$A")
chk "workspace delete" "204" "$CODE"
GONE="no"
for _ in $(seq 1 20); do
  [ ! -d "/sys/fs/cgroup/ws-$W" ] && { GONE="yes"; break; }; sleep 0.3
done
chkbool "workspace parent cgroup removed on delete" "yes" "$GONE"
chk "no NSJAIL.* left behind" "0" "$(root_leaves)"
chk "no workspace parent cgroup left behind" "0" "$(ws_parents)"

echo
echo "=== leak check: no mount, no fuse2fs growth ==="
LEFT_MOUNTS=$(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" | wc -l | tr -d ' ')
chk "no mount left under the data dir" "0" "$LEFT_MOUNTS"
# The workspace disk unmounts on delete; the fuse2fs helper exits as the mount
# goes away. Wait (bounded) for it to settle back to the baseline.
FUSE_NOW=$(fuse_count)
for _ in $(seq 1 20); do
  [ "${FUSE_NOW:-0}" -le "${FUSE_BASE:-0}" ] && break
  sleep 0.5; FUSE_NOW=$(fuse_count)
done
chkbool "no fuse2fs process left for this run (<= baseline)" "yes" \
  "$([ "${FUSE_NOW:-0}" -le "${FUSE_BASE:-0}" ] && echo yes || echo "no($FUSE_NOW>${FUSE_BASE})")"

echo
echo "================ spread acceptance: $PASS passed / $FAIL failed ================"
[ "$FAIL" -eq 0 ]
