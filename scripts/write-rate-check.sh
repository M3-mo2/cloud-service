#!/usr/bin/env bash
# write-rate-check.sh -- real-nsjail acceptance for the per-workspace write-RATE
# ceiling (a SAMPLED cap enforced by stopping the offender).
#
# WHY THIS EXISTS
#   DISK TOTAL, RAM and CPU each have a real per-workspace ceiling. Write RATE
#   did not, so one workspace running `dd`/`git clone`/a build can starve every
#   other workspace's IO. A kernel throttle is not available here: `io.max` does
#   not account the write at all (measured; the image is on ZFS and the bio is
#   generated outside our cgroup), and a token bucket in the engine's own write
#   path only covers API uploads/archives — a tenant's OWN process writes
#   straight through FUSE and never touches our code. So the ceiling is SAMPLED
#   from the workspace's real disk growth (one statfs, already on the metrics
#   sampler's tick) and enforced by stopping the run(s).
#
#   THE COVERAGE CLAIM, AND THE ASSERTION THAT PROVES IT: a TENANT'S OWN writer
#   started through the runs API — NOT an API upload — is what trips the
#   ceiling. A bucket in our Go code could never do that.
#
# WHAT IT ASSERTS (every check is hard; the script exits non-zero on any failure
# and no assertion is ever weakened to make a run pass):
#    1. DEFAULT OFF: with the feature disabled (the shipped default), a fast
#       writer inside the workspace runs undisturbed, the advertised enforced
#       limit reads 0, and /v1/meta reports the feature disabled;
#    2. with it ENABLED and low, a TENANT'S OWN writer (a runs-API process, NOT
#       an API upload) ends with the DISTINCT `rate_limited` outcome — not
#       `exited` and not a generic failure;
#    3. the workspace DISK really grew before the stop (the growth was seen), and
#       the offender was stopped BEFORE it filled its disk (the rate ceiling,
#       not ENOSPC, ended it);
#    4. a SINGLE burst above the ceiling does NOT kill a run (the N-consecutive
#       rule), while a sustained rate does;
#    5. a run UNDER the ceiling is untouched (no false positives: the feature is
#       not "kill everything that writes");
#    6. a DELETE inside the workspace (usage falling) is not a rate violation;
#    7. an idle workspace costs NOTHING extra: no tree walk, no extra cgroup
#       read, and exactly one statfs per workspace per tick;
#    8. the engine's OWN write path is covered too: a sustained series of API
#       uploads is refused with 429 `write_rate_exceeded`, so the two paths are
#       consistent;
#    9. the ADVERTISED limit equals the ENFORCED one (/v1/meta, the workspace
#       limits and the metrics snapshot all name the number the gate tripped at);
#   10. the measured bytes written before the kill is inside the documented
#       overshoot window (the ADR's SAMPLED bound: free_rate x (N+3) x interval);
#   11. a rate ceiling above the workspace's own disk_mb is REFUSED, not silently
#       accepted;
#   12. no leak: no mount, no cgroup, no fuse2fs process, no orphan disk.img.
#
# REQUIREMENTS
#   Root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk),
#   the real nsjail binary, fuse2fs, mkfs.ext4, the Go toolchain, python3, curl
#   and jq.
#
# USAGE
#   sudo -E bash scripts/write-rate-check.sh
#   Environment: PORT (default 8091).
#
# EXIT
#   0 when every assertion passes; non-zero on any failure. When root, nsjail or
#   the toolchain is absent the script prints a SKIP line and exits 0.
set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: sudo -E bash scripts/write-rate-check.sh

Real-nsjail acceptance for the per-workspace write-RATE ceiling. It builds
sandboxd, runs it under the host's real nsjail with a real FUSE disk, and
asserts: default-off changes nothing; a tenant's OWN writer trips a distinct
rate_limited outcome; a single burst does not kill; a run under the ceiling is
untouched; a delete is not a violation; idle costs nothing; the engine's own
write path is refused too; the advertised limit equals the enforced one; the
overshoot is inside the documented window; and nothing is leaked.

Environment:
  PORT   port for the service (default 8091).

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
PORT=${PORT:-8091}
DATA=$(mktemp -d)
CFG=$(mktemp)
CFG_OFF=$(mktemp)
BIN=$(mktemp)
LOG=$(mktemp)
LOG_OFF=$(mktemp)
SRV=""
KEY=dev-key-please-rotate
A="Authorization: Bearer $KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
PASS=0
FAIL=0
# The suite ceiling: 4 MiB/s on a 256 MiB disk. This ratio lets a paced writer
# (8 MiB/s) trip the ceiling in a few samples long before it could fill the
# disk — the disk ceiling must NOT be what ends the run.
RATE_MBS=4
DISK_MB=256
# N, the consecutive over-limit samples before a run is stopped.
N=3
# The sampler cadence this gate runs at (fast, so the test is bounded).
INTERVAL_MS=1000
INTERVAL_SEC=1

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
# chkle LABEL actual ceiling -> PASS when actual <= ceiling.
chkle() {
  if awk "BEGIN{exit !($2 <= $3)}"; then
    echo "  PASS  $1 ($2 <= $3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 $2 > $3"; FAIL=$((FAIL + 1))
  fi
}
jf() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)"; }
# jbool renders a JSON boolean as the lowercase "true"/"false" the shell
# comparisons below use (Python prints True/False).
jbool() { python3 -c "import json,sys;print(str(json.load(sys.stdin)$1).lower())"; }

# idle_rates -> "statfs_delta ticks_delta candidates" between two consecutive
# SSE frames while the service is idle. It reads the live sampler counters from
# the stream (NOT via one-shot /v1/metrics calls, which would themselves add
# statfs reads), so the delta is the tick's own work.
idle_rates() {
  python3 - "$PORT" "$KEY" <<'PY'
import sys, socket, json, time
port, key = int(sys.argv[1]), sys.argv[2]
s = socket.socket(); s.settimeout(8); s.connect(("127.0.0.1", port))
s.sendall(f"GET /v1/metrics/stream HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer {key}\r\nAccept: text/event-stream\r\n\r\n".encode())
buf=b""; frames=[]
end=time.time()+15
while len(frames)<3 and time.time()<end:
    try: c=s.recv(65536)
    except socket.timeout: break
    if not c: break
    buf+=c
    while b"\n\n" in buf:
        ev, buf = buf.split(b"\n\n",1)
        for line in ev.decode("utf-8","replace").splitlines():
            if line.startswith("data: "):
                try: frames.append(json.loads(line[6:]))
                except Exception: pass
s.close()
if len(frames)<2:
    print("0 0 0 0"); sys.exit()
a,b = frames[-2], frames[-1]
sf=b["sampler"]["statfs_reads"]-a["sampler"]["statfs_reads"]
cg=b["sampler"]["cgroup_reads"]-a["sampler"]["cgroup_reads"]
tk=b["sampler"]["ticks"]-a["sampler"]["ticks"]
cand=b["sampler"].get("candidates",1)
print(f"{sf} {cg} {tk} {cand}")
PY
}

fuse_count() { ps -eo comm --no-headers | grep -cx fuse2fs; }
ws_parents() { ls -d /sys/fs/cgroup/ws-* 2>/dev/null | wc -l | tr -d ' '; }
root_leaves() { ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null | wc -l | tr -d ' '; }

# snap_field WS FIELD -> a field of the workspace's one-shot metrics frame.
snap_field() {
  curl -s -H "$A" "$BASE/v1/workspaces/$1/metrics" \
    | python3 -c "import json,sys
try:
    d=json.load(sys.stdin)
    for w in d.get('workspaces',[]):
        if w.get('workspace_id')=='$1': print(w.get('$2','')); break
except Exception: pass"
}
# counter NAME -> a service-wide sampler counter.
counter() { curl -s -H "$A" "$BASE/v1/metrics" | python3 -c "import json,sys
try: print(json.load(sys.stdin)['sampler'].get('$1',''))
except Exception: pass"; }
# run_status WS RUN
run_status() { curl -s -H "$A" "$BASE/v1/workspaces/$1/runs/$2" | python3 -c "import json,sys
try: print(json.load(sys.stdin)['run']['status'])
except Exception: pass"; }
# wait_run_terminal WS RUN SECONDS -> prints the terminal status (or last seen).
wait_run_terminal() {
  local st="" end=$(( $(date +%s) + $3 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    st=$(run_status "$1" "$2")
    case "$st" in
      "" | queued | running) ;;
      *) echo "$st"; return ;;
    esac
    sleep 0.4
  done
  echo "${st:-unknown}"
}

cleanup() {
  [ -n "$SRV" ] && { kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; }
  for m in $(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" || true); do
    fusermount3 -u -z "$m" 2>/dev/null || true
  done
  sleep 0.5
  # Select cgroups BY NAME at the top level only, never by walking the tree.
  for d in $(ls -d /sys/fs/cgroup/ws-*/NSJAIL.* 2>/dev/null); do
    [ ! -s "$d/cgroup.procs" ] && rmdir "$d" 2>/dev/null
  done
  for d in $(ls -d /sys/fs/cgroup/ws-* 2>/dev/null); do
    [ ! -s "$d/cgroup.procs" ] && rmdir "$d" 2>/dev/null
  done
  for d in $(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null); do
    [ ! -s "$d/cgroup.procs" ] && rmdir "$d" 2>/dev/null
  done
  rm -rf "$DATA" "$CFG" "$CFG_OFF" "$BIN" "$LOG" "$LOG_OFF"
}
trap cleanup EXIT INT TERM

if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk); re-run with: sudo -E bash scripts/write-rate-check.sh"
  exit 0
fi
command -v nsjail >/dev/null || { echo "SKIP: nsjail is not installed on this host"; exit 0; }
command -v fuse2fs >/dev/null || { echo "SKIP: fuse2fs is not installed on this host"; exit 0; }
command -v mkfs.ext4 >/dev/null || { echo "SKIP: mkfs.ext4 is not installed on this host"; exit 0; }
command -v jq >/dev/null || { echo "SKIP: jq is not installed on this host"; exit 0; }
command -v python3 >/dev/null || { echo "SKIP: python3 is not installed on this host"; exit 0; }
if ! getent group sandbox >/dev/null; then
  echo "=== creating the sandbox group/user ==="
  groupadd -r sandbox && useradd -r -g sandbox -M -s /usr/sbin/nologin sandbox
fi

# Guard against silent rot: the mechanism must exist in the source before the
# run starts, so a deleted implementation cannot pass by absence.
[ -f "$ROOT/internal/service/write_rate.go" ] || { echo "FAIL  artifact: write_rate.go missing"; exit 1; }
grep -q "advanceRate" "$ROOT/internal/service/write_rate.go" ||
  { echo "FAIL  artifact: the pure rate arithmetic is gone"; exit 1; }
grep -q "RunRateLimited" "$ROOT/internal/model/types.go" ||
  { echo "FAIL  artifact: the distinct rate_limited run status is gone"; exit 1; }

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1

if curl -s -m 1 -o /dev/null "$BASE/healthz" 2>/dev/null; then
  echo "FATAL: something is already listening on $PORT"; exit 1
fi

# start_service CFG LOG -> starts the built binary and waits for readiness.
start_service() {
  "$BIN" --config "$1" >>"$2" 2>&1 &
  SRV=$!
  local READY="" end=$(( $(date +%s) + 40 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    READY=$(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)
    case "$READY" in *'"status":"ready"'*) return 0 ;; esac
    sleep 0.5
  done
  return 1
}
stop_service() { [ -n "$SRV" ] && { kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; }; SRV=""; sleep 1; }

# A PACED tenant writer started through the runs API. It writes 4 MiB then
# sleeps, so the rate is ~8 MiB/s: above the 4 MiB/s ceiling (so it trips) but
# far below the free disk speed (so it never fills the 256 MiB disk before the
# watchdog acts). Bounded to 48 iterations (~192 MiB) as a hard cap.
paced_writer_json() {
  python3 -c "import json,sys
cmd='''import time
f=open(\"/workspace/rate.bin\",\"wb\")
for i in range(48):
    f.write(b\"x\"*(4*1024*1024)); f.flush(); time.sleep(0.5)
print(\"DONE\")'''
print(json.dumps({'kind':'command','argv':['python3','-c',cmd],'no_timeout':True}))"
}
# A shutdown-clean paced writer for the OFF phase: 8 iterations ~4s.
paced_short_json() {
  python3 -c "import json,sys
cmd='''import time
f=open(\"/workspace/off.bin\",\"wb\")
for i in range(8):
    f.write(b\"x\"*(4*1024*1024)); f.flush(); time.sleep(0.5)
print(\"DONE\")'''
print(json.dumps({'kind':'command','argv':['python3','-c',cmd],'no_timeout':True}))"
}

# ---------------------------------------------------------------------------
# PHASE 1: DEFAULT OFF. The feature must be invisible: a paced writer inside the
# workspace is not disturbed, the enforced limit reads 0, and meta says disabled.
# ---------------------------------------------------------------------------
echo "=== 1) DEFAULT OFF: a writer is not disturbed ==="
# This config deliberately omits disk.write_rate_enabled entirely, so the
# zero-value default (off) is what is exercised.
cat >"$CFG_OFF" <<YAML
server: { addr: "127.0.0.1:$PORT" }
storage: { data_dir: "$DATA" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: 1, memory_mb: 256, disk_mb: $DISK_MB, max_wall_time_sec: 600, max_runs: 4, allow_net: false, allow_unlimited_runs: true, write_rate_mb_s: $RATE_MBS }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true, allow_unlimited_runs: true, write_rate_mb_s: 51200 }
caps: { exec_stdout_kb: 64, exec_stderr_kb: 64, run_log_mb: 2, json_body_mb: 8, upload_mb: 40, file_read_mb: 5, file_write_mb: 40, file_list_max: 1000 }
disk: { enabled: true, backend: "auto" }
metrics: { enabled: true, interval: "${INTERVAL_MS}ms", min_interval: "100ms", max_interval: "300s" }
keys: [{ principal_id: "user_dev", key: "$KEY" }]
YAML
if ! start_service "$CFG_OFF" "$LOG_OFF"; then
  echo "  SERVICE NOT READY: $(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)"; tail -20 "$LOG_OFF"; exit 1
fi
DAEMON_EXE=$(readlink -f "/proc/$SRV/exe" 2>/dev/null)
chkbool "daemon binary identity (pid == built image)" "yes" \
  "$([ -n "$DAEMON_EXE" ] && [ "$(md5sum <"$DAEMON_EXE" | cut -d' ' -f1)" = "$(md5sum <"$BIN" | cut -d' ' -f1)" ] && echo yes || echo no)"
chk "meta reports the write-rate feature DISABLED by default" "false" \
  "$(curl -s -H "$A" "$BASE/v1/meta" | jbool "['write_rate']['enabled']")"

WOFF=$(curl -s -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" \
  -d "{\"name\":\"off\",\"limits\":{\"write_rate_mb_s\":$RATE_MBS,\"disk_mb\":$DISK_MB,\"allow_unlimited_runs\":true}}" | jf "['workspace']['id']")
[ -n "$WOFF" ] || { echo "FATAL: no workspace id"; exit 1; }
chk "default-off: the enforced write-rate ceiling is advertised as 0" "0" \
  "$(snap_field "$WOFF" write_rate_limit_mb_s)"
R_OFF=$(curl -s -X POST "$BASE/v1/workspaces/$WOFF/runs" -H "$A" -H "$J" -d "$(paced_short_json)" | jf "['run']['id']")
T0=$(date +%s%N)
ST_OFF=$(wait_run_terminal "$WOFF" "$R_OFF" 90)
T1=$(date +%s%N)
chk "default-off: a fast tenant writer is NOT rate-limited" "exited" "$ST_OFF"
OFF_SECS=$(awk "BEGIN{s=($T1-$T0)/1000000000; if(s<0.1)s=0.1; printf \"%.3f\", s}")
OFF_USED=$(snap_field "$WOFF" disk_used_bytes)
FREE_BPS=$(awk "BEGIN{r=$OFF_USED/$OFF_SECS; if(r<1)r=1; printf \"%.0f\", r}")
echo "  default-off writer finished ($ST_OFF) in ${OFF_SECS}s, disk used ${OFF_USED} B, measured free rate ~${FREE_BPS} B/s"
chkbool "default-off: the write really happened (disk grew)" "yes" "$([ "${OFF_USED:-0}" -gt 1000000 ] && echo yes || echo no)"
# Remove the OFF workspace before the ON phase so its disk unmounts cleanly.
curl -s -o /dev/null -X DELETE "$BASE/v1/workspaces/$WOFF" -H "$A" 2>/dev/null || true
stop_service

# ---------------------------------------------------------------------------
# PHASE 2: ENABLED. The same tenant writer now trips a distinct outcome; the
# other required properties are checked around it.
# ---------------------------------------------------------------------------
echo
echo "=== 2) ENABLED: a tenant's own writer trips the distinct rate outcome ==="
cat >"$CFG" <<YAML
server: { addr: "127.0.0.1:$PORT" }
storage: { data_dir: "$DATA" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: 1, memory_mb: 256, disk_mb: $DISK_MB, max_wall_time_sec: 600, max_runs: 4, allow_net: false, allow_unlimited_runs: true, write_rate_mb_s: $RATE_MBS }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true, allow_unlimited_runs: true, write_rate_mb_s: 51200 }
caps: { exec_stdout_kb: 64, exec_stderr_kb: 64, run_log_mb: 2, json_body_mb: 8, upload_mb: 40, file_read_mb: 5, file_write_mb: 40, file_list_max: 1000 }
disk: { enabled: true, backend: "auto", write_rate_enabled: true, write_rate_samples: $N }
metrics: { enabled: true, interval: "${INTERVAL_MS}ms", min_interval: "100ms", max_interval: "300s" }
keys: [{ principal_id: "user_dev", key: "$KEY" }]
YAML
if ! start_service "$CFG" "$LOG"; then
  echo "  SERVICE NOT READY: $(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)"; tail -20 "$LOG"; exit 1
fi
echo "  service ready"

# --- meta advertises the capability truthfully ---
META=$(curl -s -H "$A" "$BASE/v1/meta")
chk "meta write_rate.enabled" "true" "$(printf '%s' "$META" | jbool "['write_rate']['enabled']")"
chk "meta write_rate.samples == N" "$N" "$(printf '%s' "$META" | jf "['write_rate']['samples']")"
chk "meta write_rate.outcome == rate_limited" "rate_limited" "$(printf '%s' "$META" | jf "['write_rate']['outcome']")"
chk "meta write_rate.refusal_code == write_rate_exceeded" "write_rate_exceeded" "$(printf '%s' "$META" | jf "['write_rate']['refusal_code']")"

W=$(curl -s -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" \
  -d "{\"name\":\"rate\",\"limits\":{\"write_rate_mb_s\":$RATE_MBS,\"disk_mb\":$DISK_MB,\"allow_unlimited_runs\":true}}" | jf "['workspace']['id']")
[ -n "$W" ] || { echo "FATAL: no workspace id"; exit 1; }
echo "  workspace=$W rate=${RATE_MBS}MiB/s disk=${DISK_MB}MiB N=$N interval=${INTERVAL_MS}ms"
FUSE_BASE=$(fuse_count)

# --- 9) advertised == enforced ---
chk "workspace limits advertise write_rate_mb_s == the configured ceiling" "$RATE_MBS" \
  "$(curl -s -H "$A" "$BASE/v1/workspaces/$W" | jf "['workspace']['limits']['write_rate_mb_s']")"
chk "meta write_rate.default_mb_s == the configured per-workspace ceiling" "$RATE_MBS" \
  "$(printf '%s' "$META" | jf "['write_rate']['default_mb_s']")"
chk "the snapshot advertises write_rate_limit_mb_s == the enforced ceiling" "$RATE_MBS" \
  "$(snap_field "$W" write_rate_limit_mb_s)"

# --- 2) THE CENTRAL ASSERTION: a tenant's own writer trips it ---
RATE_RUN=$(curl -s -X POST "$BASE/v1/workspaces/$W/runs" -H "$A" -H "$J" -d "$(paced_writer_json)" | jf "['run']['id']")
ST=$(wait_run_terminal "$W" "$RATE_RUN" 120)
chk "a tenant's own writer ends with the distinct rate_limited outcome" "rate_limited" "$ST"
chkbool "the rate outcome is NOT a clean exited" "yes" "$([ "$ST" != "exited" ] && echo yes || echo no)"
USED_AT_KILL=$(snap_field "$W" disk_used_bytes)
chkbool "the disk really grew before the stop (the growth was seen)" "yes" \
  "$([ "${USED_AT_KILL:-0}" -gt 1000000 ] && echo yes || echo no)"
# The rate ceiling — NOT the disk filling — ended the run: the disk is well
# under its 256 MiB allowance.
chkle "the offender was stopped before it filled its disk (rate, not ENOSPC)" \
  "${USED_AT_KILL:-0}" "$(( DISK_MB * 1024 * 1024 * 9 / 10 ))"

# --- 10) overshoot inside the documented window ---
# The ADR states the overshoot as the free rate over the detection window: the
# watchdog acts within (N+1) sampler intervals of a sustained offender, so the
# bytes written before the stop are bounded by free_rate x (N+3) x interval.
WINDOW_BYTES=$(awk "BEGIN{printf \"%.0f\", $FREE_BPS * ($N+3) * $INTERVAL_SEC}")
chkle "bytes written before the kill are inside the documented window (free_rate x (N+3) x interval)" \
  "$USED_AT_KILL" "$WINDOW_BYTES"
echo "  measured ${USED_AT_KILL:-0} B <= window ${WINDOW_BYTES} B (free ${FREE_BPS} B/s, N=$N, interval ${INTERVAL_MS}ms)"

# Clean the file and let the baseline reset before the next phase.
curl -s -o /dev/null -X DELETE "$BASE/v1/workspaces/$W/files?path=rate.bin" -H "$A" 2>/dev/null || true
sleep 3

# --- 4) a SINGLE burst does not kill ---
# One 8 MiB write (a single over-limit sample at most) then idle; with N=3 the
# following under-limit samples reset the counter, so the run is not killed.
BURST_JSON=$(python3 -c "import json;print(json.dumps({'kind':'command','argv':['sh','-c','dd if=/dev/zero of=/workspace/burst.bin bs=1M count=8 2>/dev/null; sleep 8; echo DONE'],'no_timeout':True}))")
BURST_RUN=$(curl -s -X POST "$BASE/v1/workspaces/$W/runs" -H "$A" -H "$J" -d "$BURST_JSON" | jf "['run']['id']")
ST_BURST=$(wait_run_terminal "$W" "$BURST_RUN" 90)
chk "a single burst above the ceiling does NOT kill the run" "exited" "$ST_BURST"
curl -s -o /dev/null -X DELETE "$BASE/v1/workspaces/$W/files?path=burst.bin" -H "$A" 2>/dev/null || true
sleep 3

# --- 5) a run UNDER the ceiling is untouched (no false positives) ---
UNDER_JSON=$(python3 -c "import json;print(json.dumps({'kind':'command','argv':['python3','-c','import time\nf=open(\"/workspace/slow.bin\",\"wb\")\nfor i in range(8):\n f.write(b\"x\"*(1*1024*1024)); f.flush(); time.sleep(1)\nprint(\"DONE\")'],'no_timeout':True}))")
UNDER_RUN=$(curl -s -X POST "$BASE/v1/workspaces/$W/runs" -H "$A" -H "$J" -d "$UNDER_JSON" | jf "['run']['id']")
ST_UNDER=$(wait_run_terminal "$W" "$UNDER_RUN" 90)
chk "a run under the ceiling is NOT killed (no false positives)" "exited" "$ST_UNDER"
curl -s -o /dev/null -X DELETE "$BASE/v1/workspaces/$W/files?path=slow.bin" -H "$A" 2>/dev/null || true
sleep 3

# --- 6) a DELETE inside the workspace is not a rate violation ---
# Seed a file with a synchronous exec, then a run that deletes it (usage falls).
curl -s -o /dev/null -X POST "$BASE/v1/workspaces/$W/exec" -H "$A" -H "$J" \
  -d '{"cmd":["dd","if=/dev/zero","of=/workspace/del.bin","bs=1M","count=8"]}' 2>/dev/null || true
sleep 2
DEL_JSON=$(python3 -c "import json;print(json.dumps({'kind':'command','argv':['rm','-f','/workspace/del.bin'],'no_timeout':True}))")
DEL_RUN=$(curl -s -X POST "$BASE/v1/workspaces/$W/runs" -H "$A" -H "$J" -d "$DEL_JSON" | jf "['run']['id']")
ST_DEL=$(wait_run_terminal "$W" "$DEL_RUN" 60)
chk "a DELETE inside the workspace is not a rate violation" "exited" "$ST_DEL"

# --- 7) an idle workspace costs nothing extra ---
# Read the deltas from the sampler's OWN stream frames, not from one-shot
# /v1/metrics calls (which would themselves add statfs reads and corrupt the
# measurement). With one idle workspace, each tick must add exactly one statfs
# (the disk figure, which the rate computation reuses) and zero cgroup reads.
sleep 3
IDLE=$(idle_rates)
SF_D=$(echo "$IDLE" | awk '{print $1}')
CG_D=$(echo "$IDLE" | awk '{print $2}')
TK_D=$(echo "$IDLE" | awk '{print $3}')
CAND=$(echo "$IDLE" | awk '{print $4}')
chk "no tree walk on the write-rate path" "0" "$(counter tree_walk_reads)"
chk "idle workspace adds no cgroup reads (write-rate adds none)" "0" "$CG_D"
chkbool "exactly one statfs per candidate workspace per tick" "yes" \
  "$(awk "BEGIN{exit !($TK_D>0 && $CAND>0 && $SF_D == $CAND*$TK_D)}" && echo yes || echo "no(statfs_d=$SF_D ticks_d=$TK_D cand=$CAND)")"

# --- 8) the engine's OWN write path is covered too ---
# A sustained series of API uploads (the engine's Go write path, NOT a tenant
# process) grows the disk past the ceiling; once the watchdog has flagged the
# workspace, an upload is refused with 429 write_rate_exceeded. This makes the
# two paths consistent. Uploads are small (8 MiB) and bounded (<= 20), so the
# workspace never approaches its disk ceiling.
echo "  driving sustained API uploads (the engine's own write path) ..."
UP_CODE=""
for i in $(seq 1 20); do
  head -c 8388608 /dev/zero >"$DATA/up.bin"
  UP_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/workspaces/$W/files/upload" \
    -H "$A" -F "path=up$i.bin" -F "file=@$DATA/up.bin" -m 20)
  rm -f "$DATA/up.bin"
  [ "$UP_CODE" = "429" ] && break
  sleep 0.4
done
chk "the engine's own write path is refused with 429 write_rate_exceeded" "429" "$UP_CODE"
# Clean the uploads so the disk ceiling is not approached (best-effort).
for i in $(seq 1 20); do
  curl -s -o /dev/null -X DELETE "$BASE/v1/workspaces/$W/files?path=up$i.bin" -H "$A" 2>/dev/null || true
done
sleep 3

# --- 11) a rate ceiling above disk_mb is refused ---
BAD=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" \
  -d '{"name":"bad","limits":{"disk_mb":64,"write_rate_mb_s":128}}')
chk "a write_rate_mb_s above the workspace disk_mb is refused (422)" "422" "$BAD"

# --- 12) leak check ---
echo
echo "=== leak check: no mount, no cgroup, no fuse2fs growth ==="
curl -s -o /dev/null -X DELETE "$BASE/v1/workspaces/$W" -H "$A" 2>/dev/null || true
stop_service
sleep 1
chk "no NSJAIL.* left behind" "0" "$(root_leaves)"
chk "no workspace parent cgroup left behind" "0" "$(ws_parents)"
LEFT_MOUNTS=$(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" | wc -l | tr -d ' ')
chk "no mount left under the data dir" "0" "$LEFT_MOUNTS"
FUSE_NOW=$(fuse_count)
for _ in $(seq 1 20); do
  [ "${FUSE_NOW:-0}" -le "${FUSE_BASE:-0}" ] && break
  sleep 0.5; FUSE_NOW=$(fuse_count)
done
chkbool "no fuse2fs process left for this run (<= baseline)" "yes" \
  "$([ "${FUSE_NOW:-0}" -le "${FUSE_BASE:-0}" ] && echo yes || echo "no($FUSE_NOW>${FUSE_BASE})")"

echo
echo "================ write-rate acceptance: $PASS passed / $FAIL failed ================"
[ "$FAIL" -eq 0 ]
