#!/usr/bin/env bash
# session-nsjail-check.sh -- real-nsjail acceptance for terminal sessions.
#
# WHY THIS EXISTS
#   Unit tests with a stub runner cannot catch a PTY that fails to allocate
#   inside the sandbox. A real bug shipped that way: sessions returned 201
#   Created and reported "running" while the PTY relay died with
#   "OSError: out of pty devices", because the jail's /dev had only
#   null/urandom/zero. This script runs the real service against the REAL nsjail
#   on the host and asserts a session actually gets a terminal. It is the check
#   that catches a dead PTY where unit tests stay green.
#
# WHAT IT ASSERTS (every check is hard; the script exits non-zero on any
# failure, and no assertion is ever weakened to make a run pass):
#   1. a real terminal: `tty` prints /dev/pts/N, `test -t 1` succeeds, and the
#      stream contains neither "out of pty devices" nor a traceback;
#   2. shell state survives between commands (`cd /tmp` then `pwd`);
#   3. an interrupt (signal INT to `sleep 30`) does not end the session;
#   4. `resize` reaches the PTY (`stty size` reports the new size);
#   5. a foreign principal (a second API key) is refused (403/404, never
#      success) on create, input, output and delete;
#   6. DELETE ends the session cleanly;
#   7. `POST .../input` on an ended session returns 409 with code
#      `session_ended` (the documented terminal-session error);
#   8. a FAILED PTY allocation surfaces as a session in `failed` state with
#      reason `sandbox_unavailable` and a diagnostic carrying the PTY error --
#      never a session reported running whose stream is a traceback.
#
# HOW CHECK 8 IS FORCED (it is honest about what it proves):
#   nsjail has no PTY support; the relay allocates the terminal with
#   pty.fork(), which needs the host's /dev/ptmx and devpts. A SECOND sandboxd
#   instance is started on PORT+1 whose nsjail template is a copy of
#   configs/nsjail_python.cfg.tmpl with its `{{- if .PTY }}` guard rewritten to
#   `{{- if false }}`, so that instance deliberately never binds /dev/pts and
#   /dev/ptmx into a session's jail. Inside that jail pty.fork() fails with
#   "out of pty devices", and the check proves the service turns that into a
#   failed session with a diagnostic. It is the real-nsjail analogue of the unit
#   test TestSessionPTYFailureIsLoud (which forces the same condition through a
#   fake runner error). It asserts the real failure path, not a mock.
#
# REQUIREMENTS
#   Root (nsjail needs cgroup v2 and mount namespaces), the real nsjail binary
#   (not the test stub), the Go toolchain, python3 and curl. `sudo` resets PATH,
#   so the toolchain is resolved explicitly below.
#
# USAGE
#   sudo -E bash scripts/session-nsjail-check.sh
#   Environment: PORT (default 18099); the forced-failure service uses PORT+1.
#
# EXIT
#   0 when every assertion passes; non-zero on any failure. When root, nsjail or
#   the toolchain is absent the script prints a SKIP line and exits 0: it cannot
#   prove anything without them.
set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: sudo -E bash scripts/session-nsjail-check.sh

Real-nsjail acceptance harness for terminal sessions. It builds sandboxd, runs
it under the host's real nsjail, and asserts a session gets a real PTY, keeps
state, survives SIGINT, honours resize, refuses a foreign principal, ends
cleanly, returns 409 session_ended afterwards, and surfaces a forced PTY
allocation failure as a failed session with a diagnostic.

Environment:
  PORT   port for the primary service (default 18099); the forced-failure
         service uses PORT+1.

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
PORT=${PORT:-18099}
PORT2=$((PORT + 1))
DATA=$(mktemp -d)
DATA2=$(mktemp -d)
CFG=$(mktemp)
CFG2=$(mktemp)
BIN=$(mktemp)
LOG=$(mktemp)
LOG2=$(mktemp)
TMPL_DIR2=$(mktemp -d)
mkdir -p "$TMPL_DIR2/configs"
TMPL2="$TMPL_DIR2/configs/nsjail_python.cfg.tmpl"
KEY=dev-key-please-rotate
OTHER_KEY=other-key
A="Authorization: Bearer $KEY"
B="Authorization: Bearer $OTHER_KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
BASE2="http://127.0.0.1:$PORT2"
SRV=""
SRV2=""
pass=0
fail=0

chk() {
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; pass=$((pass + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; fail=$((fail + 1))
  fi
}

# cleanup stops both services by their exact PID (never by a pattern, which can
# match the shell running it) and removes every temporary path the script made.
cleanup() {
  for pid in "$SRV" "$SRV2"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
  done
  for pid in "$SRV" "$SRV2"; do
    [ -n "$pid" ] && wait "$pid" 2>/dev/null
  done
  # Remove this run's engine-owned workspace parent cgroups (ws-*) and nested
  # NSJAIL.* leaves; a non-empty cgroup is left alone.
  for d in $(ls -d /sys/fs/cgroup/ws-*/NSJAIL.* 2>/dev/null); do
    [ -s "$d/cgroup.procs" ] || rmdir "$d" 2>/dev/null
  done
  for d in $(ls -d /sys/fs/cgroup/ws-* 2>/dev/null); do
    [ -s "$d/cgroup.procs" ] || rmdir "$d" 2>/dev/null
  done
  for d in $(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null); do
    [ -s "$d/cgroup.procs" ] || rmdir "$d" 2>/dev/null
  done
  rm -rf "$DATA" "$DATA2" "$CFG" "$CFG2" "$BIN" "$LOG" "$LOG2" "$TMPL_DIR2"
}
trap cleanup EXIT INT TERM

# The harness needs root: nsjail wants cgroup v2 and mount namespaces.
if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root (nsjail needs cgroup v2 and mount namespaces); re-run with: sudo -E bash scripts/session-nsjail-check.sh"
  exit 0
fi

command -v nsjail >/dev/null || { echo "SKIP: nsjail is not installed on this host"; exit 0; }

# The sandbox identity must exist for nsjail to drop privileges to it.
if ! getent group sandbox >/dev/null; then
  echo "=== creating the sandbox group/user ==="
  groupadd -r sandbox && useradd -r -g sandbox -M -s /usr/sbin/nologin sandbox
fi

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1

echo "=== starting the service under real nsjail ==="
cat >"$CFG" <<YAML
server: { addr: "127.0.0.1:$PORT" }
storage: { data_dir: "$DATA" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 3 }
limits: { cpu_cores: 1, memory_mb: 512, disk_mb: 256, max_wall_time_sec: 600, max_runs: 5, allow_net: true }
caps: { exec_stdout_kb: 256, exec_stderr_kb: 256, run_log_mb: 5, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 5 }
sessions: { max_per_workspace: 2, idle_timeout_sec: 600, lifetime_sec: 1800, start_probe_ms: 800 }
keys:
  - { principal_id: "user_dev", key: "$KEY" }
  - { principal_id: "user_other", key: "$OTHER_KEY" }
YAML

"$BIN" --config "$CFG" >"$LOG" 2>&1 &
SRV=$!
READY=""
for _ in $(seq 1 60); do
  READY=$(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)
  case "$READY" in *'"status":"ready"'*) break ;; esac
  sleep 0.5
done
case "$READY" in
  *'"status":"ready"'*) echo "  service ready: $READY" ;;
  *) echo "  SERVICE NOT READY: $READY"; tail -20 "$LOG"; exit 1 ;;
esac

WS=$(curl -s -m 15 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" -d '{"name":"sess"}' |
  python3 -c 'import json,sys;print(json.load(sys.stdin)["workspace"]["id"])')
SES=$(curl -s -m 30 -X POST "$BASE/v1/workspaces/$WS/sessions" -H "$A" -H "$J" -d '{"cols":120,"rows":40}' |
  python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')
echo "  workspace=$WS session=$SES"
sleep 2

send() {
  curl -s -m 30 -X POST "$BASE/v1/workspaces/$WS/sessions/$SES/input" -H "$A" -H "$J" \
    -d "{\"data\":\"$1\"}" >/dev/null
  sleep "${2:-1.5}"
}
readout() {
  curl -s -m 30 "$BASE/v1/workspaces/$WS/sessions/$SES/output?offset=0&limit=65536" -H "$A" |
    python3 -c 'import json,sys;print(json.load(sys.stdin)["data"].replace(chr(13),""))'
}

echo "=== 1) the session must provide a REAL terminal ==="
send 'tty; test -t 1 && echo IS_TTY=yes || echo IS_TTY=no\n'
out=$(readout)
case "$out" in
  *"out of pty devices"*) chk "no PTY allocation failure" yes no ;;
  *) chk "no PTY allocation failure" yes yes ;;
esac
case "$out" in
  *"/dev/pts/"*) chk "tty is a pts device" yes yes ;;
  *) chk "tty is a pts device" yes "$out" ;;
esac
case "$out" in
  *"IS_TTY=yes"*) chk "stdout is a tty" yes yes ;;
  *) chk "stdout is a tty" yes "$out" ;;
esac
case "$out" in
  *Traceback*) chk "no traceback in the stream" yes no ;;
  *) chk "no traceback in the stream" yes yes ;;
esac

echo "=== 2) state survives between commands ==="
send 'cd /tmp\n' 1
send 'pwd\n' 1.5
out=$(readout)
case "$out" in
  *"/tmp"*) chk "cd then pwd" yes yes ;;
  *) chk "cd then pwd" yes "$out" ;;
esac

echo "=== 3) an interrupt must not end the session ==="
send 'sleep 30\n' 2
curl -s -m 20 -X POST "$BASE/v1/workspaces/$WS/sessions/$SES/signal" -H "$A" -H "$J" -d '{"signal":"INT"}' >/dev/null
sleep 2
st=$(curl -s "$BASE/v1/workspaces/$WS/sessions/$SES" -H "$A" |
  python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["status"])')
chk "session still running after SIGINT" running "$st"

echo "=== 4) resize reaches the PTY ==="
curl -s -m 20 -X POST "$BASE/v1/workspaces/$WS/sessions/$SES/resize" -H "$A" -H "$J" -d '{"cols":100,"rows":30}' >/dev/null
sleep 1
send 'stty size\n' 1.5
out=$(readout)
case "$out" in
  *"30 100"*) chk "stty size reflects the resize" yes yes ;;
  *) chk "stty size reflects the resize" yes "$out" ;;
esac

echo "=== 5) a foreign principal is refused on every session route ==="
# The second key (user_other) must never read or mutate user_dev's session; the
# refusal is 403 (owned-workspace check) or 404, never a success.
while IFS='|' read -r method path body; do
  [ -n "$method" ] || continue
  if [ -n "$body" ]; then
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 20 -X "$method" \
      "$BASE/v1/workspaces/$WS$path" -H "$B" -H "$J" -d "$body")
  else
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 20 -X "$method" \
      "$BASE/v1/workspaces/$WS$path" -H "$B")
  fi
  if [ "$code" = 403 ] || [ "$code" = 404 ]; then
    chk "foreign $method $path refused" refused refused
  else
    chk "foreign $method $path refused" refused "HTTP $code"
  fi
done <<EOF
POST|/sessions|{"cols":80,"rows":24}
POST|/sessions/$SES/input|{"data":"x"}
GET|/sessions/$SES/output?offset=0|
DELETE|/sessions/$SES|
EOF

echo "=== 6) cleanup ==="
code=$(curl -s -o /dev/null -w '%{http_code}' -m 20 -X DELETE "$BASE/v1/workspaces/$WS/sessions/$SES" -H "$A")
case "$code" in 200 | 204) chk "delete ends the session" yes yes ;; *) chk "delete ends the session" yes "$code" ;; esac

echo "=== 7) input on an ended session returns the documented 409 ==="
resp=$(curl -s -m 20 -w '\n%{http_code}' -X POST "$BASE/v1/workspaces/$WS/sessions/$SES/input" \
  -H "$A" -H "$J" -d '{"data":"x"}')
read -r ecode eerr < <(printf '%s' "$resp" | python3 -c '
import json,sys
raw=sys.stdin.read()
body,_,status=raw.rpartition("\n")
try:
    code=json.loads(body).get("error",{}).get("code","")
except Exception:
    code=""
print(status.strip() or "?", code)
')
chk "input on an ended session is 409" 409 "$ecode"
chk "ended-session error code" session_ended "$eerr"

echo "=== 8) a failed PTY allocation surfaces as a failed session ==="
# Force the failure by removing the PTY devpts/devptmx mounts from a COPY of the
# repo template (see the header). The second instance then runs a session whose
# jail cannot allocate a PTY.
sed 's/{{- if .PTY }}/{{- if false }}/' "$ROOT/configs/nsjail_python.cfg.tmpl" >"$TMPL2"
if ! grep -q '{{- if false }}' "$TMPL2"; then
  fail=$((fail + 1))
  echo "  FAIL  forced-failure template: the '{{- if .PTY }}' guard was not found in configs/nsjail_python.cfg.tmpl (template drifted; the forced-PTY-failure check is stale)"
else
  cat >"$CFG2" <<YAML
server: { addr: "127.0.0.1:$PORT2" }
storage: { data_dir: "$DATA2" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 3 }
limits: { cpu_cores: 1, memory_mb: 512, disk_mb: 256, max_wall_time_sec: 600, max_runs: 5, allow_net: true }
caps: { exec_stdout_kb: 256, exec_stderr_kb: 256, run_log_mb: 5, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 5 }
sessions: { max_per_workspace: 2, idle_timeout_sec: 600, lifetime_sec: 1800, start_probe_ms: 800 }
keys:
  - { principal_id: "user_dev", key: "$KEY" }
  - { principal_id: "user_other", key: "$OTHER_KEY" }
YAML
  # Run from TMPL_DIR2 so the service resolves the modified template relative to
  # its working directory (executor.resolveTemplatePath prefers ./configs/...).
  (cd "$TMPL_DIR2" && exec "$BIN" --config "$CFG2") >"$LOG2" 2>&1 &
  SRV2=$!
  READY2=""
  for _ in $(seq 1 60); do
    READY2=$(curl -s -m 2 "$BASE2/readyz" 2>/dev/null || true)
    case "$READY2" in *'"status":"ready"'*) break ;; esac
    sleep 0.5
  done
  case "$READY2" in
    *'"status":"ready"'*) ;;
    *) echo "  SERVICE2 NOT READY: $READY2"; tail -20 "$LOG2"; fail=$((fail + 1)) ;;
  esac

  WS2=$(curl -s -m 15 -X POST "$BASE2/v1/workspaces" -H "$A" -H "$J" -d '{"name":"ptynofail"}' |
    python3 -c 'import json,sys;print(json.load(sys.stdin)["workspace"]["id"])')
  CREATED=$(curl -s -m 30 -X POST "$BASE2/v1/workspaces/$WS2/sessions" -H "$A" -H "$J" -d '{"cols":80,"rows":24}')
  SSTATUS=$(printf '%s' "$CREATED" | python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["status"])')
  SREASON=$(printf '%s' "$CREATED" | python3 -c 'import json,sys;print(json.load(sys.stdin)["session"].get("reason") or "")')
  SDIAG=$(printf '%s' "$CREATED" | python3 -c 'import json,sys;print(json.load(sys.stdin)["session"].get("diagnostic") or "")')
  SES2=$(printf '%s' "$CREATED" | python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')
  GSTATUS=$(curl -s -m 20 "$BASE2/v1/workspaces/$WS2/sessions/$SES2" -H "$A" |
    python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["status"])')
  chk "forced PTY failure session status" failed "$SSTATUS"
  chk "forced PTY failure reason" sandbox_unavailable "$SREASON"
  case "$SDIAG" in
    *"out of pty devices"*) chk "forced PTY failure carries the PTY diagnostic" yes yes ;;
    *) chk "forced PTY failure carries the PTY diagnostic" yes "$SDIAG" ;;
  esac
  chk "forced PTY failure is terminal on GET" failed "$GSTATUS"
fi

echo
echo "================ session acceptance: $pass passed / $fail failed ================"
[ "$fail" -eq 0 ]
