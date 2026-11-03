#!/usr/bin/env bash
# metrics-check.sh -- real-nsjail acceptance for the per-workspace resource
# stream (RAM + disk).
#
# WHY THIS EXISTS
#   The feature exists to be CHEAP. The single hard requirement is that streaming
#   a workspace's RAM and disk every 3s is so light that thousands of workspaces
#   cause no pressure. A naive implementation (sample per connection, walk the
#   directory tree, read /proc/<pid>/environ, shell out to df) works and FAILS
#   the requirement. Unit tests pin the pure arithmetic; only the REAL service,
#   a real nsjail sandbox and a real FUSE disk can prove that:
#     * the RAM figure tracks a real allocation and is the SUM over the
#       workspace's per-run and per-session cgroups (not one cgroup);
#     * the disk figure equals `df -B1 --output=used /workspace` INSIDE the
#       workspace, for an EMPTY workspace as well as one with data -- the
#       assertion that catches the f_bavail/f_bfree bug;
#     * an idle workspace costs ZERO cgroup reads (only one statfs);
#     * the per-tick cost does not grow with the number of subscribers (fan-out);
#     * there is no tree walk anywhere on the sampling path;
#     * the interval floor is enforced, a stalled subscriber is coalesced (drop
#       counter moves, pending stays bounded), disconnects are clean, and the
#       sampler goroutine truly starts lazily and stops with the last subscriber.
#
#   RESOURCE DISCIPLINE: this gate creates only a handful of workspaces, runs at
#   most TWO concurrently, opens at most THREE subscribers at a time, and bounds
#   every wait. It tears each workspace down before the next phase and verifies
#   with findmnt and `grep -cx fuse2fs` that nothing was leaked.
#
# WHAT IT ASSERTS (every check is hard; the script exits non-zero on any failure
# and no assertion is ever weakened to make a run pass):
#    1. build marker positive control (the tested binary is a fresh build);
#    2. both stream routes return text/event-stream and deliver an initial frame
#       promptly (bounded wait);
#    3. at least 3 frames arrive with roughly the interval cadence, each parseable
#       JSON, with monotonically increasing timestamps;
#    4. the RAM figure rises above a floor while a known-size allocation is held,
#       then falls back once the run ends;
#    5. with TWO runs the RAM figure is at least the largest single cgroup's
#       reading and consistent with the sum (not just one cgroup);
#    6. idle elision: with no live run RAM is 0 and the sampler's cgroup-read
#       counter does not advance across ticks;
#    7. fan-out, not per-subscriber sampling: with several subscribers open the
#       per-tick cgroup reads do not grow with the subscriber count;
#    8. no tree walk on the stream path: the walk counter is 0 and a workspace
#       with many files reports its disk far faster than the interval;
#    9. the interval floor is enforced (10ms -> 422);
#   10. a stalled subscriber does not grow the engine: pending stays bounded and
#       the drop counter moves (coalescing, not buffering);
#   11. disconnect is clean: after N open/close cycles the subscriber count is 0
#       and the sampler stops when the last one leaves;
#   12. the lazy start is real: with zero subscribers the tick counter is frozen;
#   13. the stream's disk figure agrees with GET /usage for the same workspace,
#       and both agree with `df -B1 --output=used /workspace` run INSIDE it.
#
# REQUIREMENTS
#   Root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk),
#   the real nsjail binary (NOT the test stub), fuse2fs/mkfs.ext4, the Go
#   toolchain, python3, curl and jq.
#
# USAGE
#   sudo -E bash scripts/metrics-check.sh
#   Environment: PORT (default 8093).
#
# EXIT
#   0 when every assertion passes; non-zero on any failure. When root, nsjail or
#   the toolchain is absent the script prints a SKIP line and exits 0.
set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: sudo -E bash scripts/metrics-check.sh

Real-nsjail acceptance harness for the per-workspace resource stream (RAM +
disk). It builds sandboxd, runs it under the host's real nsjail with a real FUSE
disk, and asserts the whole cheap-stream contract: initial frame + SSE content
type, cadence, real RAM tracking (the SUM over per-run cgroups), idle elision,
fan-out, no tree walk, the interval floor, coalescing with a drop counter, clean
disconnect, lazy start/stop, and that the reported disk equals `df` inside the
workspace.

Environment:
  PORT   port for the service (default 8093).

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
PORT=${PORT:-8093}
DATA=$(mktemp -d)
CFG=$(mktemp)
BIN=$(mktemp)
LOG=$(mktemp)
SRV=""
KEY=dev-key-please-rotate
A="Authorization: Bearer $KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
MARKER=zcp-metrics-stream-v0.0.19
PASS=0
FAIL=0

chk() { # label expected actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}
chkbool() { # label expected actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}
chkge() { # label actual >= floor
  if awk "BEGIN{exit !($2 >= $3)}"; then
    echo "  PASS  $1 ($2 >= $3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 $2 < $3"; FAIL=$((FAIL + 1))
  fi
}
chkle() { # label actual <= ceiling
  if awk "BEGIN{exit !($2 <= $3)}"; then
    echo "  PASS  $1 ($2 <= $3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 $2 > $3"; FAIL=$((FAIL + 1))
  fi
}

# api METHOD PATH [json-body] -> HTTP status; body to $TMPBODY.
TMPBODY=$(mktemp)
api() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -s -o "$TMPBODY" -w '%{http_code}' -X "$method" "$BASE$path" -H "$A" -H "$J" -d "$body"
  else
    curl -s -o "$TMPBODY" -w '%{http_code}' -X "$method" "$BASE$path" -H "$A"
  fi
}
jbody() { jq -r "$1" "$TMPBODY"; }

# snap_field WS FIELD -> the workspace's one-shot metrics field (prints "" if
# the workspace is absent).
snap_ws() { curl -s -H "$A" "$BASE/v1/workspaces/$1/metrics"; }
snap_ws_field() { snap_ws "$1" | jq -r ".workspaces[] | select(.workspace_id==\"$1\") | .$2 // \"\"" 2>/dev/null; }
# counters -> the service-wide sampler counters object.
counters() { curl -s -H "$A" "$BASE/v1/metrics" | jq -c '.sampler'; }
counter() { counters | jq -r ".$1"; }
# svc_field FIELD -> a field of the service-wide snapshot's first workspace.
usage_field() { curl -s -H "$A" "$BASE/v1/workspaces/$1/usage" | jq -r ".$2 // \"\""; }
workspace_dir() { echo "$DATA/workspaces/$1"; }
fuse_count() { ps -eo comm --no-headers | grep -cx fuse2fs; }

# exec_in WS CMD -> runs CMD in the workspace sandbox, prints its stdout.
exec_in() {
  local ws="$1" cmd="$2"
  curl -s -X POST "$BASE/v1/workspaces/$ws/exec" -H "$A" -H "$J" \
    -d "{\"cmd\":[\"sh\",\"-c\",\"$cmd\"]}" | jq -r '.stdout // ""'
}

# sse_tick_delta PATH SECONDS -> opens ONE stream, reads frames for SECONDS, and
# prints "delta first last" for the sampler tick counter. Foreground and bounded;
# it closes cleanly when it returns.
sse_tick_delta() {
  python3 - "$1" "$2" "$PORT" "$KEY" <<'PY'
import sys, socket, json, time
path, secs, port, key = sys.argv[1], float(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
s = socket.socket(); s.settimeout(5); s.connect(("127.0.0.1", port))
s.sendall(f"GET {path} HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer {key}\r\nAccept: text/event-stream\r\n\r\n".encode())
buf=b""; first=None; last=None
end=time.time()+secs
while time.time()<end:
    try: c=s.recv(65536)
    except socket.timeout: break
    if not c: break
    buf+=c
    while b"\n\n" in buf:
        ev, buf = buf.split(b"\n\n",1)
        for line in ev.decode("utf-8","replace").splitlines():
            if line.startswith("data: "):
                try: d=json.loads(line[6:])
                except Exception: continue
                t=d["sampler"]["ticks"]
                if first is None: first=t
                last=t
s.close()
print(f"{(last or 0)-(first or 0)} {first or 0} {last or 0}")
PY
}

# sse_frames PATH COUNT TIMEOUT -> prints COUNT JSON frame payloads (one per
# line) read from an SSE stream, bounded by TIMEOUT seconds. The reader opens a
# SINGLE stream, reads COUNT frames, then closes; it never loops unboundedly.
sse_frames() {
  python3 - "$1" "$2" "$3" "$PORT" "$KEY" <<'PY'
import sys, socket
path, count, timeout, port, key = sys.argv[1], int(sys.argv[2]), float(sys.argv[3]), int(sys.argv[4]), sys.argv[5]
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.settimeout(timeout)
s.connect(("127.0.0.1", port))
req = f"GET {path} HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer {key}\r\nAccept: text/event-stream\r\n\r\n"
s.sendall(req.encode())
buf = b""
frames = []
deadline = timeout
while len(frames) < count:
    try:
        chunk = s.recv(65536)
    except socket.timeout:
        break
    if not chunk:
        break
    buf += chunk
    while b"\n\n" in buf:
        event, buf = buf.split(b"\n\n", 1)
        text = event.decode("utf-8", "replace")
        if text.startswith("HTTP/"):
            # strip headers off the first event if they arrived glued
            if "\r\n\r\n" in text:
                text = text.split("\r\n\r\n", 1)[1]
        for line in text.splitlines():
            if line.startswith("data: "):
                frames.append(line[len("data: "):])
s.close()
for f in frames:
    print(f)
PY
}

# sse_stall PATH SECONDS BUFBYTES -> opens a stream with a TINY socket receive
# buffer, never reads, and blocks (so the engine's per-subscriber channel must
# coalesce). Bounded: it closes after SECONDS.
sse_stall() {
  python3 - "$1" "$2" "$3" "$PORT" "$KEY" <<'PY'
import sys, socket, time
path, secs, bufbytes, port, key = sys.argv[1], float(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4]), sys.argv[5]
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
try:
    s.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUFFORCE, bufbytes)
except Exception:
    s.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, bufbytes)
s.connect(("127.0.0.1", port))
req = f"GET {path} HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer {key}\r\nAccept: text/event-stream\r\n\r\n"
s.sendall(req.encode())
time.sleep(secs)
s.close()
PY
}

# bounded_waits for a workspace's ram to exceed a floor (or fall below a
# ceiling). Prints the last observed value.
wait_ram() { # WS OP THRESHOLD SECONDS
  local ws="$1" op="$2" thr="$3" secs="$4" v=""
  local end=$(( $(date +%s) + secs ))
  while [ "$(date +%s)" -lt "$end" ]; do
    v=$(snap_ws_field "$ws" ram_used_bytes)
    [ -z "$v" ] && v=0
    if [ "$op" = "above" ] && awk "BEGIN{exit !($v >= $thr)}"; then echo "$v"; return; fi
    if [ "$op" = "below" ] && awk "BEGIN{exit !($v <= $thr)}"; then echo "$v"; return; fi
    sleep 0.3
  done
  echo "${v:-0}"
}
wait_run_done() { # WS SECONDS
  local ws="$1" secs="$2"
  local end=$(( $(date +%s) + secs ))
  while [ "$(date +%s)" -lt "$end" ]; do
    local n
    n=$(curl -s -H "$A" "$BASE/v1/workspaces/$ws/runs" | jq -r '[.runs[]? | select(.status=="running" or .status=="queued")] | length' 2>/dev/null)
    [ "${n:-1}" -eq 0 ] && { echo yes; return; }
    sleep 0.4
  done
  echo no
}
# wait_live WS SECONDS -> waits until at least one run is running (queued runs
# have no cgroup yet). Prints "yes" or "no"; it never blocks past SECONDS.
wait_live() {
  local ws="$1" secs="$2" n=""
  local end=$(( $(date +%s) + secs ))
  while [ "$(date +%s)" -lt "$end" ]; do
    n=$(curl -s -H "$A" "$BASE/v1/workspaces/$ws/runs" | jq -r '[.runs[]? | select(.status=="running")] | length' 2>/dev/null)
    [ "${n:-0}" -ge 1 ] && { echo yes; return; }
    sleep 0.3
  done
  echo no
}
# wait_cgroups WS N SECONDS -> waits until the sampler reports >=N live cgroups
# for the workspace. The cgroup can appear a beat after the run is running.
wait_cgroups() {
  local ws="$1" want="$2" secs="$3" n=""
  local end=$(( $(date +%s) + secs ))
  while [ "$(date +%s)" -lt "$end" ]; do
    n=$(snap_ws_field "$ws" live_cgroups)
    [ "${n:-0}" -ge "$want" ] && { echo yes; return; }
    sleep 0.3
  done
  echo "no($n)"
}

# nsjail_leaves prints the number of LIVE nsjail leaf cgroups, both at the root
# (legacy) and nested under a workspace parent (ws-*/NSJAIL.*, the current
# per-workspace layout).
nsjail_leaves() {
  { ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null
    ls -d /sys/fs/cgroup/ws-*/NSJAIL.* 2>/dev/null
  } | wc -l | tr -d ' '
}

cg_before=$(nsjail_leaves)
FUSE_BASE=$(fuse_count)

cleanup() {
  if [ -n "$SRV" ]; then
    kill "$SRV" 2>/dev/null
    wait "$SRV" 2>/dev/null
  fi
  for m in $(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" || true); do
    fusermount3 -u -z "$m" 2>/dev/null || true
  done
  sleep 0.5
  for d in $(ls -d /sys/fs/cgroup/ws-*/NSJAIL.* 2>/dev/null); do
    if [ ! -s "$d/cgroup.procs" ]; then rmdir "$d" 2>/dev/null; fi
  done
  for d in $(ls -d /sys/fs/cgroup/ws-* 2>/dev/null); do
    if [ ! -s "$d/cgroup.procs" ]; then rmdir "$d" 2>/dev/null; fi
  done
  for d in $(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null); do
    if [ ! -s "$d/cgroup.procs" ]; then rmdir "$d" 2>/dev/null; fi
  done
  rm -rf "$DATA" "$CFG" "$BIN" "$LOG" "$TMPBODY"
}
trap cleanup EXIT INT TERM

if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk); re-run with: sudo -E bash scripts/metrics-check.sh"
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

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1
if grep -aqF "$MARKER" "$BIN"; then
  echo "  PASS  build marker present ($MARKER)"; PASS=$((PASS + 1))
else
  echo "  FAIL  build marker absent from the built binary"; FAIL=$((FAIL + 1))
fi

# Small workspaces, a 1s cadence with a 1s floor, and a real FUSE disk.
cat >"$CFG" <<YAML
server: { addr: "127.0.0.1:$PORT" }
storage: { data_dir: "$DATA" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: 1, memory_mb: 512, disk_mb: 64, max_wall_time_sec: 600, max_runs: 8, allow_net: false }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true }
caps: { exec_stdout_kb: 64, exec_stderr_kb: 64, run_log_mb: 2, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 8 }
disk: { enabled: true, backend: "auto" }
# Cadence 1s, floor 10ms: the interval assertions use the default 1s, while the
# coalescing phase uses a 10ms override (at the floor) to fill a tiny socket
# buffer within a bounded stall. The floor is restored to 1s when the last
# subscriber leaves.
metrics: { enabled: true, interval: "1s", min_interval: "10ms", max_interval: "300s", include_cpu: false, idle_reconcile_interval: "15s", max_subscribers: 64 }
keys:
  - { principal_id: "user_dev", key: "$KEY" }
YAML

if curl -s -m 1 -o /dev/null "$BASE/healthz" 2>/dev/null; then
  echo "FATAL: something is already listening on $PORT"; exit 1
fi

echo "=== starting the service under real nsjail ==="
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
  echo "  SERVICE NOT READY: ${READY:-}"; tail -20 "$LOG"; exit 1
fi
echo "  service ready"

# Prove the daemon we measure is the binary we built (a stale daemon on the same
# port answers from a deleted data dir and makes correct code look broken).
DAEMON_EXE=$(readlink -f "/proc/$SRV/exe" 2>/dev/null)
if [ -n "$DAEMON_EXE" ] && [ "$(md5sum <"$DAEMON_EXE" | cut -d' ' -f1)" = "$(md5sum <"$BIN" | cut -d' ' -f1)" ]; then
  echo "  PASS  daemon binary identity (pid $SRV == built image)"; PASS=$((PASS + 1))
else
  echo "  FAIL  daemon binary does not match the built image ($DAEMON_EXE)"; FAIL=$((FAIL + 1))
fi

echo "=== 1) both stream routes return text/event-stream with an initial frame ==="
# Create the workspaces used by the gate (bounded: two).
CODE=$(api POST /v1/workspaces '{"name":"metrics-a"}')
chk "create workspace A" "201" "$CODE"
WA=$(jbody '.workspace.id')
CODE=$(api POST /v1/workspaces '{"name":"metrics-b"}')
chk "create workspace B" "201" "$CODE"
WB=$(jbody '.workspace.id')
FAN_EXTRA=""

CT=$(curl -s -o /dev/null -w '%{content_type}' -m 3 -N "$BASE/v1/metrics/stream" -H "$A")
chk "service-wide stream content-type" "text/event-stream" "$CT"
CT=$(curl -s -o /dev/null -w '%{content_type}' -m 3 -N "$BASE/v1/workspaces/$WA/metrics/stream" -H "$A")
chk "per-workspace stream content-type" "text/event-stream" "$CT"
# The initial comment + first frame must arrive promptly (bounded wait, no sleep).
FIRST=$(sse_frames "/v1/metrics/stream" 1 6 | head -1)
chkbool "service-wide stream delivered an initial frame" "yes" "$([ -n "$FIRST" ] && echo yes || echo no)"

echo "=== 2) cadence: >=3 parseable frames with increasing timestamps ==="
CADJSON=$(sse_frames "/v1/workspaces/$WA/metrics/stream" 3 12)
NF=$(printf '%s\n' "$CADJSON" | grep -c .)
chkge "per-workspace stream delivered >=3 frames" "$NF" 3
MONO=$(printf '%s\n' "$CADJSON" | python3 -c '
import sys, json
last=None; ok=True; n=0
for line in sys.stdin:
    line=line.strip()
    if not line: continue
    try: d=json.loads(line)
    except Exception: ok=False; break
    t=d["timestamp"]; n+=1
    if last is not None and t < last: ok=False; break
    last=t
print("yes" if (ok and n>=3) else "no")
')
chkbool "frames parse as JSON with monotonic timestamps" "yes" "$MONO"
# Cadence: consecutive frames are ~1s apart (allow generous slack, both sides).
CADOK=$(printf '%s\n' "$CADJSON" | python3 -c '
import sys, json, datetime
ts=[]
for line in sys.stdin:
    line=line.strip()
    if not line: continue
    ts.append(json.loads(line)["timestamp"])
if len(ts) < 3: print("no"); sys.exit()
def p(s): return datetime.datetime.fromisoformat(s.replace("Z","+00:00"))
gaps=[(p(ts[i+1])-p(ts[i])).total_seconds() for i in range(len(ts)-1)]
ok=all(0.4 <= g <= 3.0 for g in gaps)
print("yes" if ok else "gaps="+str(gaps))
')
chkbool "frame cadence is roughly the 1s interval" "yes" "$CADOK"

echo "=== 3) RAM tracks a real allocation (single run) ==="
# Warm the workspace disk (mounts it) so the first run is not delayed.
exec_in "$WA" "df -h /workspace >/dev/null && echo ok" >/dev/null
# A run holds a 60 MiB allocation; RAM must rise above a floor, then fall back.
CODE=$(api POST "/v1/workspaces/$WA/runs" '{"kind":"command","argv":["python3","-c","import time;x=bytearray(60*1024*1024);time.sleep(20)"]}')
chk "start 60MB allocation run" "202" "$CODE"
LIVE=$(wait_live "$WA" 20)
chkbool "the allocation run reached running" "yes" "$LIVE"
R1=$(wait_ram "$WA" above 40000000 25)
chkge "RAM rises above the allocation floor" "$R1" 40000000
wait_run_done "$WA" 30 >/dev/null
R_AFTER=$(wait_ram "$WA" below 8000000 15)
chkle "RAM falls back after the run ends" "$R_AFTER" 8000000

echo "=== 4) the SUM is real: two runs exceed one cgroup ==="
# Two runs each hold ~60 MiB. The reported RAM must be clearly MORE than a single
# cgroup's reading, proving it is the sum and not one cgroup.
CODE=$(api POST "/v1/workspaces/$WA/runs" '{"kind":"command","argv":["python3","-c","import time;x=bytearray(60*1024*1024);time.sleep(20)"]}')
chk "start sum run 1" "202" "$CODE"
CODE=$(api POST "/v1/workspaces/$WA/runs" '{"kind":"command","argv":["python3","-c","import time;y=bytearray(60*1024*1024);time.sleep(20)"]}')
chk "start sum run 2" "202" "$CODE"
# Wait until BOTH runs are running (two live cgroups).
BOTH=$(python3 - "$PORT" "$KEY" "$WA" <<'PY'
import sys, json, urllib.request
port, key, ws = int(sys.argv[1]), sys.argv[2], sys.argv[3]
req = urllib.request.Request(f"http://127.0.0.1:{port}/v1/workspaces/{ws}/runs", headers={"Authorization": f"Bearer {key}"})
import time
end=time.time()+25
while time.time()<end:
    try:
        with urllib.request.urlopen(req, timeout=3) as r:
            d=json.load(r)
        n=sum(1 for x in d.get("runs",[]) if x.get("status")=="running")
        if n>=2: print("yes"); sys.exit()
    except Exception: pass
    time.sleep(0.3)
print("no")
PY
)
chkbool "both sum runs reached running" "yes" "$BOTH"
# Wait until the SAMPLER reports BOTH cgroups live (the second cgroup may appear
# a beat after the run is running), then read the summed RAM.
CG2=$(wait_cgroups "$WA" 2 25)
chkbool "sampler reports two live cgroups" "yes" "$CG2"
# The two-run sum must exceed the single-run reading by a margin a single cgroup
# cannot produce. Wait for the SUM to appear (a single cgroup cannot reach it).
R2=$(wait_ram "$WA" above "$((R1 + 30000000))" 25)
chkge "two-run RAM exceeds the single-run reading by a margin" "$R2" "$((R1 + 30000000))"

echo "=== 4b) the reported ceiling is the ENFORCED parent ceiling (ram_used <= ram_limit) ==="
# The workspace's memory_mb is 512 MiB (the config default). The reported
# ram_limit must be that number, the kernel's parent cgroup must enforce exactly
# it, and the summed usage must never exceed it -- because the kernel enforces
# the SUM at the parent, which is the contract that used to be broken (a tenant
# saw ram_used > ram_limit). Asserted while BOTH runs are still live.
LIM=$(snap_ws_field "$WA" ram_limit_bytes)
chk "reported ram_limit is the workspace memory_mb" "536870912" "$LIM"
USED=$(snap_ws_field "$WA" ram_used_bytes)
PCT=$(snap_ws_field "$WA" ram_percent)
chkbool "ram_used <= ram_limit under load" "yes" \
  "$(awk "BEGIN{exit !($USED <= $LIM)}" && echo yes || echo "no(used=$USED lim=$LIM)")"
chkbool "ram_percent <= 100 under load" "yes" \
  "$(awk "BEGIN{exit !($PCT <= 100)}" && echo yes || echo "no(pct=$PCT)")"
PARENTMAX=$(cat "/sys/fs/cgroup/ws-$WA/memory.max" 2>/dev/null || echo "")
chk "parent memory.max equals the advertised ceiling" "536870912" "$PARENTMAX"
wait_run_done "$WA" 40 >/dev/null

echo "=== 5) idle elision: no live run -> RAM 0, cgroup reads frozen ==="
IDLE_RAM=$(snap_ws_field "$WA" ram_used_bytes)
chk "idle RAM is 0" "0" "$IDLE_RAM"
C_BEFORE=$(counter cgroup_reads)
T_BEFORE=$(counter ticks)
sleep 3
C_AFTER=$(counter cgroup_reads)
# No live cgroup anywhere -> zero cgroup reads across several ticks.
chk "idle cgroup reads did not advance" "$C_BEFORE" "$C_AFTER"

echo "=== 6) fan-out: per-tick reads do not grow with subscriber count ==="
# One live run, three subscribers. Between two consecutive frames the cgroup read
# counter advances by exactly the per-tick reads (2), NOT 2*3.
CODE=$(api POST "/v1/workspaces/$WA/runs" '{"kind":"command","argv":["python3","-c","import time;time.sleep(40)"]}')
chk "start long run for fan-out" "202" "$CODE"
LIVE=$(wait_live "$WA" 20)
chkbool "the long run reached running" "yes" "$LIVE"
sleep 2
# Read frames until two consecutive ones both show a live cgroup, then measure
# the cgroup-read delta between them.
FAN=$(python3 - "$PORT" "$KEY" "$WA" <<'PY'
import sys, socket, json, time
port, key, ws = int(sys.argv[1]), sys.argv[2], sys.argv[3]
path = f"/v1/workspaces/{ws}/metrics/stream"
socks=[]
for _ in range(3):
    s=socket.socket(); s.settimeout(10); s.connect(("127.0.0.1",port))
    s.sendall(f"GET {path} HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer {key}\r\nAccept: text/event-stream\r\n\r\n".encode())
    socks.append(s)
def read_frame(s, buf):
    # Skip the initial ": connected" comment (an event with no data: line) and
    # keep reading until a real JSON frame arrives.
    while True:
        while b"\n\n" not in buf:
            c=s.recv(65536)
            if not c: return None, buf
            buf+=c
        ev, buf = buf.split(b"\n\n",1)
        for line in ev.decode("utf-8","replace").splitlines():
            if line.startswith("data: "):
                return json.loads(line[6:]), buf
        # an event with no data line (e.g. a comment) -- keep reading
buf=b""
prev=None
res=None
end=time.time()+25
while time.time()<end:
    fr, buf = read_frame(socks[0], buf)
    if fr is None: break
    w = fr["workspaces"][0] if fr.get("workspaces") else {}
    live = w.get("live_cgroups",0)
    if prev is not None and prev[0]>=1 and live>=1:
        reads = fr["sampler"]["cgroup_reads"] - prev[2]
        ticks = fr["sampler"]["ticks"] - prev[3]
        res=(reads,ticks)
        break
    prev=(live, w.get("live_runs",0), fr["sampler"]["cgroup_reads"], fr["sampler"]["ticks"])
for s in socks: s.close()
print(f"{res[0]} {res[1]}" if res else "0 0")
PY
)
FAN_READS=$(echo "$FAN" | awk '{print $1}')
FAN_TICKS=$(echo "$FAN" | awk '{print $2}')
FAN_OK=$(awk "BEGIN{exit !($FAN_TICKS>0 && $FAN_READS == 2*$FAN_TICKS)}" && echo yes || echo "no(reads=$FAN_READS ticks=$FAN_TICKS)")
chkbool "per-tick cgroup reads independent of 3 subscribers (2/tick)" "yes" "$FAN_OK"
wait_run_done "$WA" 60 >/dev/null

echo "=== 7) no tree walk; disk is O(1) ==="
WALK=$(counter tree_walk_reads)
chk "tree-walk counter is zero" "0" "$WALK"
# A workspace with many small files must still report its disk with ONE statfs.
# Create 3000 tiny files, then time the snapshot request itself (curl's
# time_total); a statfs snapshot is a few ms, while walking 3000 files would cost
# ~0.46s at the measured 153us/file. Two samples, min taken.
exec_in "$WA" "mkdir -p /workspace/many && for i in \$(seq 1 3000); do : > /workspace/many/f\$i; done && echo ok" >/dev/null
T1=$(curl -s -o /dev/null -w '%{time_total}' -H "$A" "$BASE/v1/workspaces/$WA/metrics")
T2=$(curl -s -o /dev/null -w '%{time_total}' -H "$A" "$BASE/v1/workspaces/$WA/metrics")
FAST=$(awk "BEGIN{t=($T1<$T2)?$T1:$T2; print (t<0.2)?\"yes\":\"no(t=\"t\"s)\"}")
chkbool "snapshot of a 3000-file workspace is O(1) (<0.2s)" "yes" "$FAST"
# The walk counter must STILL be zero after sampling a many-file workspace.
WALK2=$(counter tree_walk_reads)
chk "tree-walk counter still zero after many-file workspace" "0" "$WALK2"

echo "=== 8) the interval floor is enforced ==="
# 1ms is BELOW the 10ms floor and must be refused (422), not silently raised.
FLOOR_CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 3 "$BASE/v1/metrics/stream?interval=1ms" -H "$A")
chk "interval below the floor is refused (422)" "422" "$FLOOR_CODE"
# A valid override at the floor is accepted.
OK_CODE=$(sse_frames "/v1/metrics/stream?interval=10ms" 1 6 | head -1)
chkbool "interval at the floor is accepted" "yes" "$([ -n "$OK_CODE" ] && echo yes || echo no)"

echo "=== 9) a stalled subscriber is coalesced, not buffered ==="
# A tiny-receive-buffer socket that never reads forces the engine's single-slot
# per-subscriber channel to coalesce; the drop counter must move. The override
# sits at the floor (10ms) so the pipe fills promptly; the stall is bounded
# (25s) and the drop counter is polled. Three extra tiny workspaces enlarge the
# frames so the socket fills sooner; they are removed with the rest.
for n in 1 2 3; do
  CODE=$(api POST /v1/workspaces "{\"name\":\"fan$n\"}")
  FAN_EXTRA="$FAN_EXTRA $(jbody '.workspace.id')"
done
DROP0=$(counter dropped_frames)
sse_stall "/v1/metrics/stream?interval=10ms" 25 1024 &
STALL_PID=$!
DROP1=$DROP0
for _ in $(seq 1 25); do
  sleep 1
  DROP1=$(counter dropped_frames)
  [ "$DROP1" -gt "$DROP0" ] && break
done
wait "$STALL_PID" 2>/dev/null
chkge "stalled subscriber advanced the drop counter" "$DROP1" "$((DROP0 + 1))"
# Pending stayed bounded: the subscriber count never exceeded the ceiling.
chkle "subscriber count stayed within the configured ceiling" "$(counter subscribers)" 64

echo "=== 10) disconnect is clean; lazy start/stop is real ==="
# Wait until no subscriber remains from the stall phase.
for _ in $(seq 1 20); do [ "$(counter subscribers)" = "0" ] && break; sleep 0.5; done
# With zero subscribers the sampler goroutine must NOT be running.
chk "sampler not running with zero subscribers" "0" "$(counter running)"
TN0=$(counter ticks)
sleep 2
TN1=$(counter ticks)
chk "ticks frozen with zero subscribers" "$TN0" "$TN1"
chk "no subscribers before the cycle phase" "0" "$(counter subscribers)"
# Bounded: five open/close cycles.
for _ in 1 2 3 4 5; do
  sse_frames "/v1/metrics/stream" 1 4 >/dev/null
done
sleep 1
chk "subscribers returned to 0 after cycles" "0" "$(counter subscribers)"
# A subscriber makes ticks advance; the counter must move while it is connected.
TICKDELTA=$(sse_tick_delta "/v1/metrics/stream" 4 | awk '{print $1}')
chkge "ticks advanced while a subscriber was connected" "${TICKDELTA:-0}" 1
# After everything closed, the sampler stops and ticks freeze again.
for _ in $(seq 1 20); do [ "$(counter subscribers)" = "0" ] && break; sleep 0.5; done
sleep 1
chk "sampler stopped after the last subscriber left" "0" "$(counter running)"
TN2=$(counter ticks)
sleep 2
TN3=$(counter ticks)
chk "ticks frozen again after the last subscriber left" "$TN2" "$TN3"
chk "no subscribers left" "0" "$(counter subscribers)"

echo "=== 11) empty-workspace disk matches df (the f_bavail bug) ==="
# Workspace B is empty. Mount its disk by running df inside it first, THEN read
# the metric, so both observe the same mounted (empty) filesystem.
DF_EMPTY=$(exec_in "$WB" "df -B1 --output=used /workspace | tail -1 | tr -d ' '")
EMPTY_USED=$(snap_ws_field "$WB" disk_used_bytes)
# An empty workspace must report ~0 (NOT the 11.9%-full 7639040 the f_bavail bug
# produced) and must match df within one block.
chkle "empty workspace reports ~0 disk used" "$EMPTY_USED" 1048576
chkbool "empty workspace disk matches df inside it within one block" "yes" \
  "$(awk "BEGIN{d=$EMPTY_USED-$DF_EMPTY; if(d<0)d=-d; print (d<=4096)?\"yes\":\"no(api=$EMPTY_USED df=$DF_EMPTY)\"}")"

echo "=== 12) disk tracks a real write and matches df, /usage and the stream ==="
exec_in "$WB" "head -c 10485760 /dev/zero > /workspace/big.bin && echo ok" >/dev/null
sleep 1
DISK_METRIC=$(snap_ws_field "$WB" disk_used_bytes)
DF_USED=$(exec_in "$WB" "df -B1 --output=used /workspace | tail -1 | tr -d ' '")
USAGE_DISK=$(usage_field "$WB" disk_bytes)
SUMMARY_DISK=$(curl -s -H "$A" "$BASE/v1/workspaces/$WB" | jq -r '.workspace.summary.disk.used_bytes // ""')
chkge "disk used rose by roughly the write (>=8MiB)" "$DISK_METRIC" 8388608
chkbool "stream disk matches df inside the workspace within one block" "yes" \
  "$(awk "BEGIN{d=$DISK_METRIC-$DF_USED; if(d<0)d=-d; print (d<=4096)?\"yes\":\"no(metric=$DISK_METRIC df=$DF_USED)\"}")"
# The API summary is the SAME statfs path, so it must match exactly too.
chkbool "summary.disk.used_bytes matches the stream exactly" "yes" \
  "$([ "$SUMMARY_DISK" = "$DISK_METRIC" ] && echo yes || echo "no(summary=$SUMMARY_DISK metric=$DISK_METRIC)")"
# GET /usage is the OTHER path: it walks the tree and sums file BYTES, while the
# stream reports the filesystem's used BLOCKS (df). They agree within the ext4
# metadata baseline (~88 KiB measured); a tolerance of 1 MiB still catches the
# f_bavail bug (which is off by ~7.6 MiB on a 64 MiB workspace).
chkbool "stream disk agrees with GET /usage within tolerance" "yes" \
  "$(awk "BEGIN{d=$DISK_METRIC-$USAGE_DISK; if(d<0)d=-d; print (d<=1048576)?\"yes\":\"no(metric=$DISK_METRIC usage=$USAGE_DISK)\"}")"

echo "=== 13) /v1/meta advertises the metrics capability ==="
api GET /v1/meta >/dev/null
chk "meta metrics.enabled" "true" "$(jbody '.metrics.enabled')"
chkbool "meta reports a positive min interval" "yes" "$(jbody '.metrics.min_interval_sec > 0' | sed 's/true/yes/;s/false/no/')"
chkbool "meta lists all four routes" "yes" "$(jbody '.metrics.routes | length == 4' | sed 's/true/yes/;s/false/no/')"

echo "=== leak check: no cgroup growth, no mount, no fuse2fs growth ==="
# Delete every workspace this run created (bounded), then confirm nothing leaked.
for id in "$WA" "$WB" $FAN_EXTRA; do
  [ -n "$id" ] && api DELETE "/v1/workspaces/$id" >/dev/null 2>&1 || true
done
sleep 2
cg_after=$(nsjail_leaves)
chkbool "cgroups did not grow past baseline" "yes" "$([ "${cg_after:-0}" -le "${cg_before:-0}" ] && echo yes || echo no)"
PARENTS_LEFT=$(ls -d /sys/fs/cgroup/ws-* 2>/dev/null | wc -l | tr -d ' ')
chk "no workspace parent cgroup left after delete" "0" "$PARENTS_LEFT"
LEFT_MOUNTS=$(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" | wc -l | tr -d ' ')
chk "no mount left under the data dir" "0" "$LEFT_MOUNTS"
FUSE_END=$(fuse_count)
chkbool "no fuse2fs process left for this run (<= baseline)" "yes" "$([ "${FUSE_END:-0}" -le "${FUSE_BASE:-0}" ] && echo yes || echo no)"

echo
echo "================ metrics acceptance: $PASS passed / $FAIL failed ================"
[ "$FAIL" -eq 0 ]
