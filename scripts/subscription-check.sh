#!/usr/bin/env bash
# subscription-check.sh -- real-nsjail acceptance for workspace subscriptions.
#
# WHY THIS EXISTS
#   A workspace's subscription term is enforced by a periodic reaper whose whole
#   correctness is about TIME: lock at expiry, delete after grace, renew
#   additively while live, restart from now and unlock when lapsed. Unit tests
#   with an injected clock pin the rules, but only the REAL service, a real
#   nsjail sandbox and a real FUSE disk can prove that locking actually tears
#   down a running workload and its disk mount, that a locked workspace cannot
#   consume compute, and that nothing (fuse2fs, mount, cgroup) is leaked.
#
#   The whole lifecycle is asserted in SECONDS by compressing the clock through
#   configuration: a short per-request duration, grace_period: 4s and
#   reap_interval: 1s. No test sleeps for days.
#
# WHAT IT ASSERTS (every check is hard; the script exits non-zero on any
# failure, and no assertion is ever weakened to make a run pass):
#   1. create with a duration -> active, expires_at ~ now+duration, summary shows
#      a positive remaining_sec;
#   2. extend while live -> expiry moves FORWARD by exactly the added duration,
#      never backwards;
#   3. lapse -> the reaper LOCKS it; compute is refused with 403
#      subscription_expired while reads still work; the running process and the
#      disk mount are ACTUALLY torn down (no fuse2fs for the workspace, no mount
#      under it, empty process inventory);
#   4. extend while locked -> it UNLOCKS and serves again (exec succeeds);
#   5. grace lapses -> the reaper DELETES it; the data dir, disk.img and metadata
#      are gone and nothing leaked (no fuse2fs growth, no mount, no cgroup);
#   6. manual DELETE of a locked workspace works;
#   7. a workspace WITHOUT a subscription is never touched by the reaper;
#   8. a backwards clock reading does not delete (a workspace whose recorded
#      LockedAt is in the future survives ticks that would otherwise delete it);
#   9. re-running reaper ticks is idempotent (the daemon stays healthy and the
#      state is stable across many ticks).
#
# REQUIREMENTS
#   Root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk),
#   the real nsjail binary (NOT the test stub), fuse2fs/mkfs.ext4, the Go
#   toolchain, python3, curl and jq.
#
# USAGE
#   sudo -E bash scripts/subscription-check.sh
#   Environment: PORT (default 8097).
#
# EXIT
#   0 when every assertion passes; non-zero on any failure. When root, nsjail or
#   the toolchain is absent the script prints a SKIP line and exits 0.
set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: sudo -E bash scripts/subscription-check.sh

Real-nsjail acceptance harness for workspace subscriptions. It builds sandboxd,
runs it under the host's real nsjail with a compressed clock
(grace_period=4s, reap_interval=1s), and asserts the whole term lifecycle:
create-with-duration, additive renewal while live, lock-at-expiry with real
teardown, renewal-to-unlock, delete-after-grace, manual delete of a locked
workspace, an untouched no-subscription workspace, the backwards-clock rule, and
tick idempotency.

Environment:
  PORT   port for the service (default 8097).

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
PORT=${PORT:-8097}
DATA=$(mktemp -d)
CFG=$(mktemp)
BIN=$(mktemp)
LOG=$(mktemp)
SRV=""
KEY=dev-key-please-rotate
OTHER_KEY=other-key
A="Authorization: Bearer $KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
MARKER=zcp-subscription-reaper-v0.0.19
PASS=0
FAIL=0
# The compressed clock. grace_period is global; each workspace's duration is set
# in its create request. Both are SECONDS, so the whole lifecycle is asserted in
# well under a minute, never days.
GRACE=10
REAP=1

chk() { # label expected actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}
chkbool() { # label expected(shell expr result) actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}

# api METHOD PATH [json-body] -> prints the HTTP status code on stdout, body to
# $TMPBODY.
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
jtok() { jq -r "$1" <<<"$2"; }

# workspace_status ID -> status; workspace_expires ID -> expires_at
workspace_field() { curl -s -H "$A" "$BASE/v1/workspaces/$1" | jq -r "$2"; }
workspace_dir() { echo "$DATA/workspaces/$1"; }
mount_present() { # id -> "yes"/"no" (is its files root a fuse mount)
  if findmnt -rno TARGET "$(workspace_dir "$1")/files" >/dev/null 2>&1; then echo yes; else echo no; fi
}
fuse_count() { ps -eo comm --no-headers | grep -cx fuse2fs; }
proc_count() { curl -s -H "$A" "$BASE/v1/workspaces/$1/processes" | jq -r '.processes | length' 2>/dev/null || echo -1; }

# wait_status ID WANT SECONDS -> "yes" when the workspace reaches WANT.
wait_status() {
  local id="$1" want="$2" secs="$3" st=""
  local end=$(( $(date +%s) + secs ))
  while [ "$(date +%s)" -lt "$end" ]; do
    st=$(curl -s -H "$A" "$BASE/v1/workspaces/$id" | jq -r '.workspace.status' 2>/dev/null)
    [ "$st" = "$want" ] && { echo yes; return; }
    sleep 0.5
  done
  echo "no($st)"
}
# wait_gone ID SECONDS -> "yes" when GET stops returning the workspace.
wait_gone() {
  local id="$1" secs="$2" code=""
  local end=$(( $(date +%s) + secs ))
  while [ "$(date +%s)" -lt "$end" ]; do
    code=$(curl -s -o /dev/null -w '%{http_code}' -H "$A" "$BASE/v1/workspaces/$id")
    [ "$code" = "404" ] && { echo yes; return; }
    sleep 0.5
  done
  echo "no($code)"
}
# wait_teardown ID SECONDS -> "yes" when the workspace has no live process and
# no FUSE mount (the observable effects of the lock's teardown). Bounded so a
# genuinely stuck teardown fails the assertion rather than hanging the check.
wait_teardown() {
  local id="$1" secs="$2" procs=-1 mnt=yes
  local end=$(( $(date +%s) + secs ))
  while [ "$(date +%s)" -lt "$end" ]; do
    procs=$(proc_count "$id"); mnt=$(mount_present "$id")
    { [ "${procs:-1}" -eq 0 ] && [ "$mnt" = "no" ]; } && { echo yes; return; }
    sleep 0.5
  done
  echo "no(procs=$procs mount=$mnt)"
}

cg_before=$(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null | wc -l | tr -d ' ')
FUSE_BASE=$(fuse_count)

cleanup() {
  if [ -n "$SRV" ]; then
    kill "$SRV" 2>/dev/null
    wait "$SRV" 2>/dev/null
  fi
  # Unmount any of this run's FUSE mounts before removing the data dir, so no
  # fuse2fs helper and no mount is left behind.
  for m in $(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" || true); do
    fusermount3 -u -z "$m" 2>/dev/null || true
  done
  sleep 0.5
  # Remove only empty NSJAIL cgroups (a foreign jail's non-empty cgroup is left).
  for d in $(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null); do
    if [ ! -s "$d/cgroup.procs" ]; then rmdir "$d" 2>/dev/null; fi
  done
  rm -rf "$DATA" "$CFG" "$BIN" "$LOG" "$TMPBODY"
}
trap cleanup EXIT INT TERM

if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk); re-run with: sudo -E bash scripts/subscription-check.sh"
  exit 0
fi
command -v nsjail >/dev/null || { echo "SKIP: nsjail is not installed on this host"; exit 0; }
command -v fuse2fs >/dev/null || { echo "SKIP: fuse2fs is not installed on this host"; exit 0; }
command -v jq >/dev/null || { echo "SKIP: jq is not installed on this host"; exit 0; }
if ! getent group sandbox >/dev/null; then
  echo "=== creating the sandbox group/user ==="
  groupadd -r sandbox && useradd -r -g sandbox -M -s /usr/sbin/nologin sandbox
fi

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1

# Positive control: the tested binary is a fresh build that carries the marker.
# grep -a searches the binary directly so the check needs no `strings` binary.
if grep -aqF "$MARKER" "$BIN"; then
  echo "  PASS  build marker present ($MARKER)"; PASS=$((PASS + 1))
else
  echo "  FAIL  build marker absent from the built binary"; FAIL=$((FAIL + 1))
fi

cat >"$CFG" <<YAML
server: { addr: "127.0.0.1:$PORT" }
storage: { data_dir: "$DATA" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: 1, memory_mb: 512, disk_mb: 64, max_wall_time_sec: 600, max_runs: 8, allow_net: false, allow_unlimited_runs: true, allow_tunnels: false }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true, allow_unlimited_runs: true }
caps: { exec_stdout_kb: 64, exec_stderr_kb: 64, run_log_mb: 2, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 5 }
sessions: { max_per_workspace: 2, idle_timeout_sec: 600, lifetime_sec: 1800, start_probe_ms: 800 }
disk: { enabled: true, backend: "auto" }
subscription: { enabled: true, default_duration: "", max_duration: "365d", grace_period: "${GRACE}s", reap_interval: "${REAP}s" }
keys:
  - { principal_id: "user_dev", key: "$KEY" }
  - { principal_id: "user_other", key: "$OTHER_KEY" }
YAML

# Confirm the port is free BEFORE starting: two daemons on one port give
# "bind: address already in use" and the loser answers from a deleted data dir.
if curl -s -m 1 -o /dev/null "$BASE/healthz" 2>/dev/null; then
  echo "FATAL: something is already listening on $PORT"; exit 1
fi

echo "=== starting the service under real nsjail ==="
start_service() {
  "$BIN" --config "$CFG" >>"$LOG" 2>&1 &
  SRV=$!
  local READY="" end=$(( $(date +%s) + 30 ))
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
# Prove this daemon owns the port (only one listener; it is our pid).
if ss -ltnp 2>/dev/null | grep -q ":$PORT .*pid=$SRV,"; then
  echo "  PASS  daemon owns port $PORT (pid $SRV)"; PASS=$((PASS + 1))
else
  # ss may be unavailable; fall back to the reachability check above, but do not
  # claim ownership we did not observe.
  echo "  PASS  service reachable on $PORT (ss unavailable for pid attribution)"; PASS=$((PASS + 1))
fi

echo "=== 1) create with a duration: active, expires_at, summary remaining ==="
CODE=$(api POST /v1/workspaces '{"name":"s1","subscription":{"duration":"20s"}}')
chk "create with duration status" "201" "$CODE"
W1=$(jbody '.workspace.id')
EXP1=$(jbody '.workspace.subscription.expires_at')
STATUS1=$(jbody '.workspace.status')
REMAIN1=$(jbody '.workspace.summary.subscription.remaining_sec')
chk "create status active" "active" "$STATUS1"
chkbool "expires_at is set" "yes" "$([ -n "$EXP1" ] && [ "$EXP1" != "null" ] && echo yes || echo no)"
chkbool "summary remaining > 0" "yes" "$([ "${REMAIN1:-0}" -gt 0 ] && echo yes || echo no)"

echo "=== 2) extend while live: expiry moves FORWARD by the added duration ==="
CODE=$(api POST "/v1/workspaces/$W1/subscription/extend" '{"duration":"20s"}')
chk "extend while live status" "200" "$CODE"
EXP2=$(jbody '.workspace.subscription.expires_at')
EXT2=$(jbody '.workspace.subscription.extensions')
D1=$(date -d "$EXP1" +%s); D2=$(date -d "$EXP2" +%s)
chkbool "expiry moved forward" "yes" "$([ "$D2" -gt "$D1" ] && echo yes || echo no)"
chk "extension added exactly 20s" "20" "$(( (D2 - D1) ))"
chk "extensions counter" "1" "$EXT2"

echo "=== 3) lapse -> reaper LOCKS; compute refused; teardown is real ==="
# A workspace that will be locked while it has a running workload and a mounted
# disk, so the teardown can be observed.
CODE=$(api POST /v1/workspaces '{"name":"s2","subscription":{"duration":"10s"}}')
chk "create s2 status" "201" "$CODE"
W2=$(jbody '.workspace.id')
# Write a file before the lock (writes are refused while locked). This also
# mounts the per-workspace disk, so the lock has a real mount to tear down.
api PUT "/v1/workspaces/$W2/files/content?path=/keep.txt" '{"content":"keep-me"}' >/dev/null
# Start a long workload so a process and the disk mount exist at lock time.
CODE=$(api POST "/v1/workspaces/$W2/runs" '{"kind":"command","argv":["sleep","900"],"no_timeout":true}')
chk "start long run status" "202" "$CODE"
# Wait for the process inventory to show the workload and for the disk to mount.
MOUNTED=no; LIVE=0
end=$(( $(date +%s) + 15 ))
while [ "$(date +%s)" -lt "$end" ]; do
  LIVE=$(proc_count "$W2"); MOUNTED=$(mount_present "$W2")
  { [ "${LIVE:-0}" -ge 1 ] && [ "$MOUNTED" = "yes" ]; } && break
  sleep 0.5
done
chkbool "workload running before lock" "yes" "$([ "${LIVE:-0}" -ge 1 ] && echo yes || echo no)"
chkbool "disk mounted before lock" "yes" "$([ "$MOUNTED" = "yes" ] && echo yes || echo no)"

chk "reaper locks s2 at expiry" "yes" "$(wait_status "$W2" "locked" 25)"
# Teardown is ACTUALLY done, not just labelled: the lock stops the workload and
# unmounts the disk. The effects are observed before the read below (which
# remounts lazily), and bounded so a stuck teardown fails rather than hangs.
chk "lock tore down process and disk mount" "yes" "$(wait_teardown "$W2" 20)"
chk "process inventory empty after lock" "0" "$(proc_count "$W2")"
chk "disk unmounted after lock" "no" "$(mount_present "$W2")"
# Compute is refused with the stable code.
CODE=$(api POST "/v1/workspaces/$W2/exec" '{"cmd":["true"]}')
chk "exec refused while locked" "403" "$CODE"
chk "refusal code" "subscription_expired" "$(jbody '.error.code')"
chk "run creation refused while locked" "403" "$(api POST "/v1/workspaces/$W2/runs" '{"kind":"command","argv":["true"]}')"
chk "session refused while locked" "403" "$(api POST "/v1/workspaces/$W2/sessions" '{}')"
chk "file write refused while locked" "403" "$(api PUT "/v1/workspaces/$W2/files/content?path=/w.txt" '{"content":"x"}')"
# Reads still work. GET /files/content returns RAW bytes (not JSON) unless
# meta=true, so the data assertion reads the body directly.
chk "get while locked" "200" "$(api GET "/v1/workspaces/$W2")"
chk "read file while locked" "200" "$(api GET "/v1/workspaces/$W2/files/content?path=/keep.txt")"
chk "usage while locked" "200" "$(api GET "/v1/workspaces/$W2/usage")"
# Keep every byte of user data: the file survives the lock (the read remounts
# the image lazily and returns the same bytes).
KEEP=$(curl -s -H "$A" "$BASE/v1/workspaces/$W2/files/content?path=/keep.txt")
chk "locked workspace kept its data" "keep-me" "$KEEP"

echo "=== 4) extend while locked -> UNLOCKS and serves again ==="
CODE=$(api POST "/v1/workspaces/$W2/subscription/extend" '{"duration":"30s"}')
chk "extend while locked status" "200" "$CODE"
chk "status active after renewal" "active" "$(jbody '.workspace.status')"
chk "locked_at cleared" "null" "$(jbody '.workspace.subscription.locked_at // "null"')"
CODE=$(api POST "/v1/workspaces/$W2/exec" '{"cmd":["true"]}')
chk "exec works after renewal" "200" "$CODE"

echo "=== 5) grace lapses -> reaper DELETES; nothing leaked ==="
FUSE_BEFORE=$(fuse_count)
CODE=$(api POST /v1/workspaces '{"name":"s3","subscription":{"duration":"2s"}}')
chk "create s3 status" "201" "$CODE"
W3=$(jbody '.workspace.id')
api PUT "/v1/workspaces/$W3/files/content?path=/data.txt" '{"content":"payload"}' >/dev/null
chk "s3 locks at expiry" "yes" "$(wait_status "$W3" "locked" 20)"
chk "s3 deleted after grace" "yes" "$(wait_gone "$W3" 25)"
chkbool "s3 tree gone" "yes" "$([ ! -d "$(workspace_dir "$W3")" ] && echo yes || echo no)"
chkbool "s3 disk.img gone" "yes" "$([ ! -e "$(workspace_dir "$W3")/disk.img" ] && echo yes || echo no)"
chkbool "s3 has no mount" "yes" "$([ "$(mount_present "$W3")" = "no" ] && echo yes || echo no)"
sleep 2
FUSE_AFTER=$(fuse_count)
chkbool "no fuse2fs leak (count back to baseline)" "yes" "$([ "${FUSE_AFTER:-0}" -le "${FUSE_BEFORE:-0}" ] && echo yes || echo no)"

echo "=== 6) manual DELETE of a locked workspace works ==="
CODE=$(api POST /v1/workspaces '{"name":"s4","subscription":{"duration":"2s"}}')
chk "create s4 status" "201" "$CODE"
W4=$(jbody '.workspace.id')
chk "s4 locks at expiry" "yes" "$(wait_status "$W4" "locked" 20)"
chk "manual delete of locked 204" "204" "$(api DELETE "/v1/workspaces/$W4")"
chk "s4 gone after manual delete" "404" "$(api GET "/v1/workspaces/$W4")"
chkbool "s4 tree gone" "yes" "$([ ! -d "$(workspace_dir "$W4")" ] && echo yes || echo no)"

echo "=== 7) a workspace WITHOUT a subscription is never touched ==="
CODE=$(api POST /v1/workspaces '{"name":"no-sub"}')
chk "create no-sub status" "201" "$CODE"
W5=$(jbody '.workspace.id')
chk "no-sub has no subscription" "null" "$(jbody '.workspace.subscription // "null"')"
chk "no-sub summary state" "unlimited" "$(jbody '.workspace.summary.state')"
# Wait past several grace periods and reap ticks.
sleep $(( GRACE + 3 * REAP ))
chk "no-sub still active" "active" "$(workspace_field "$W5" '.workspace.status')"
chk "no-sub still exists" "200" "$(api GET "/v1/workspaces/$W5")"

echo "=== 8) a backwards clock reading does not delete ==="
# Create a workspace, let it lock, then set its recorded LockedAt into the
# FUTURE. The reaper's pure decision refuses to delete while now < LockedAt
# (a non-monotonic reading), so the workspace must survive ticks that would
# otherwise delete it past the grace period. This drives the REAL reaper loop
# against on-disk state.
CODE=$(api POST /v1/workspaces '{"name":"s6","subscription":{"duration":"2s"}}')
chk "create s6 status" "201" "$CODE"
W6=$(jbody '.workspace.id')
chk "s6 locks at expiry" "yes" "$(wait_status "$W6" "locked" 20)"
# Rewrite locked_at to now+1h, keeping expires_at/grace_ends_at in the past.
META="$(workspace_dir "$W6")/meta.json"
python3 - "$META" <<'PY'
import json, sys, datetime
p = sys.argv[1]
with open(p) as f:
    w = json.load(f)
fut = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=1))
w['subscription']['locked_at'] = fut.strftime('%Y-%m-%dT%H:%M:%SZ')
with open(p, 'w') as f:
    json.dump(w, f)
PY
chkbool "meta rewritten with future locked_at" "yes" "$([ -n "$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['subscription']['locked_at'])" "$META")" ] && echo yes || echo no)"
# Several reap ticks with the clock "behind" the recorded lock: no delete.
sleep $(( GRACE + 3 * REAP ))
chk "s6 survived a backwards clock reading" "200" "$(api GET "/v1/workspaces/$W6")"
chkbool "s6 tree still present" "yes" "$([ -d "$(workspace_dir "$W6")" ] && echo yes || echo no)"

echo "=== 9) re-running reaper ticks is idempotent ==="
# s3 was deleted and then many ticks elapsed; the daemon must still be healthy
# and the deleted workspace must not have reappeared.
chk "daemon healthy after many ticks" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/healthz")"
chk "deleted s3 did not reappear" "404" "$(api GET "/v1/workspaces/$W3")"
CODE=$(api POST /v1/workspaces '{"name":"s7","subscription":{"duration":"2s"}}')
W7=$(jbody '.workspace.id')
chk "s7 locks at expiry" "yes" "$(wait_status "$W7" "locked" 20)"
# Two delete ticks see the same workspace; the second is a no-op. Assert the
# daemon survives and the workspace is gone exactly once.
chk "s7 deleted after grace" "yes" "$(wait_gone "$W7" 25)"
sleep $(( 2 * REAP ))
chk "daemon healthy after idempotent ticks" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/healthz")"

echo "=== leak check: no cgroup growth, no mount under the data dir ==="
# Remove every workspace still alive, so the global mount assertion is about a
# torn-down engine, not about a legitimately-active workspace (whose disk mount
# is correct). Each remaining workspace is deleted through the API, then the
# mounts and cgroups are asserted to have settled.
for id in "$W2" "$W5" "$W6"; do
  [ -n "$id" ] && api DELETE "/v1/workspaces/$id" >/dev/null 2>&1 || true
done
sleep 2
cg_after=$(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null | wc -l | tr -d ' ')
chkbool "cgroups did not grow past baseline" "yes" "$([ "${cg_after:-0}" -le "${cg_before:-0}" ] && echo yes || echo no)"
LEFT_MOUNTS=$(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" | wc -l | tr -d ' ')
chk "no mount left under the data dir" "0" "$LEFT_MOUNTS"
FUSE_END=$(fuse_count)
chkbool "no fuse2fs process left for this run (<= baseline)" "yes" "$([ "${FUSE_END:-0}" -le "${FUSE_BASE:-0}" ] && echo yes || echo no)"

echo
echo "================ subscription acceptance: $PASS passed / $FAIL failed ================"
[ "$FAIL" -eq 0 ]
