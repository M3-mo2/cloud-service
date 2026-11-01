#!/usr/bin/env bash
# End-to-end acceptance test for sandboxd.
#
# This is an ASSERTING test: every step compares observed output against the
# expected value and the script exits non-zero if any assertion fails. It is
# meant to run against a server backed by REAL nsjail inside a container started
# with --privileged (or --cap-add=SYS_ADMIN) and --cgroupns=host.
#
# Environment:
#   BASE      base URL of a running server (default http://localhost:8080)
#   KEY       primary API key (default dev-key-please-rotate)
#   OTHER_KEY second principal's key (default other-key)
#   DATA_DIR  host path of the server's data dir, for the symlink-escape check
#   CONTAINER name of a docker container to run the symlink-escape check within
#   NSJAIL    real nsjail binary path; the script refuses a stub (default: none)
#
# Exit code: 0 if all assertions pass, 1 otherwise.
set -u

K="Authorization: Bearer ${KEY:-dev-key-please-rotate}"
OK="Authorization: Bearer ${OTHER_KEY:-other-key}"
B="${BASE:-http://localhost:8080}"
DATA_DIR="${DATA_DIR:-}"
JSON="Content-Type: application/json"

fail=0
pass=0

# check LABEL ACTUAL EXPECTED
check() {
  if [ "$2" = "$3" ]; then
    pass=$((pass + 1))
    printf 'ok   %-28s %s\n' "$1" "$2"
  else
    fail=$((fail + 1))
    printf 'FAIL %-28s got %q want %q\n' "$1" "$2" "$3"
  fi
}

# check_contains LABEL HAYSTACK NEEDLE
check_contains() {
  case "$2" in
    *"$3"*) pass=$((pass + 1)); printf 'ok   %-28s contains %q\n' "$1" "$3" ;;
    *) fail=$((fail + 1)); printf 'FAIL %-28s missing %q in %q\n' "$1" "$3" "$2" ;;
  esac
}

jsonfield() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)"; }
httpcode() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

echo "### 0. environment"
echo "base=$B data_dir=${DATA_DIR:-<unset>}"

if [ -n "${NSJAIL:-}" ]; then
  if [ ! -x "$NSJAIL" ]; then
    echo "FATAL: NSJAIL=$NSJAIL is not an executable" >&2
    exit 2
  fi
  if head -c2 "$NSJAIL" 2>/dev/null | grep -q '#!'; then
    echo "FATAL: NSJAIL=$NSJAIL is a script (stub), not the real binary" >&2
    exit 2
  fi
fi

# Readiness must confirm real nsjail is present; a server without it cannot
# satisfy any isolation assertion below.
ready=$(curl -s "$B/readyz")
check "readyz.status" "$(printf '%s' "$ready" | jsonfield "['status']")" "ready"
check "readyz.nsjail" "$(printf '%s' "$ready" | jsonfield "['nsjail']")" "True"

echo "### 1. healthz"
check "healthz.status" "$(curl -s "$B/healthz" | jsonfield "['status']")" "ok"

echo "### 2. meta"
check "meta.version" "$(curl -s -H "$K" "$B/v1/meta" | jsonfield "['version']")" "0.0.5"

echo "### 3. create workspace"
WS=$(curl -s -H "$K" -H "$JSON" -d '{"name":"demo"}' "$B/v1/workspaces" | jsonfield "['workspace']['id']")
if [ -z "$WS" ] || [ "$WS" = "None" ]; then echo "FATAL: no workspace id" >&2; exit 1; fi
echo "workspace id: $WS"

echo "### 4. mkdir /src"
check "mkdir.src" "$(curl -s -o /dev/null -w '%{http_code}' -H "$K" -H "$JSON" -d '{"path":"/src"}' "$B/v1/workspaces/$WS/files/mkdir")" "201"

echo "### 5. PUT /main.py"
check "put.main.py" "$(httpcode -X PUT -H "$K" -H "$JSON" -d '{"content":"print(\"hi\")\n"}' "$B/v1/workspaces/$WS/files/content?path=/main.py")" "204"

echo "### 6. upload /uploaded.py (multipart)"
UP=$(mktemp)
printf 'print("uploaded")\n' > "$UP"
check "upload.status" "$(httpcode -H "$K" -F path=/uploaded.py -F file=@"$UP" "$B/v1/workspaces/$WS/files/upload")" "201"
rm -f "$UP"

echo "### 7. read /main.py (meta)"
check "read.main.py" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/content?path=/main.py&meta=true" | jsonfield "['sha256']")" "$(printf 'print("hi")\n' | sha256sum | cut -d' ' -f1)"

echo "### 8. exec (sync) -- a successful execution"
EXEC=$(curl -s -H "$K" -H "$JSON" -d '{"cmd":["python3","main.py"]}' "$B/v1/workspaces/$WS/exec")
check "exec.exit_code" "$(printf '%s' "$EXEC" | jsonfield "['exit_code']")" "0"
check "exec.stdout" "$(printf '%s' "$EXEC" | jsonfield "['stdout']" | tr -d '\n')" "hi"

echo "### 9. cwd is honored"
check "exec.cwd" "$(printf '%s' "$(curl -s -H "$K" -H "$JSON" -d '{"cmd":["pwd"],"cwd":"/src"}' "$B/v1/workspaces/$WS/exec")" | jsonfield "['stdout']" | tr -d '\n')" "/workspace/src"

echo "### 10. start run (file)"
RUN=$(curl -s -H "$K" -H "$JSON" -d '{"kind":"file","file":"/main.py"}' "$B/v1/workspaces/$WS/runs" | jsonfield "['run']['id']")
echo "run id: $RUN"
for _ in $(seq 1 50); do
  st=$(curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$RUN" | jsonfield "['run']['status']")
  [ "$st" = "exited" ] && break
  sleep 0.1
done
check "run.status" "$st" "exited"
check "run.logs.stdout" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$RUN/logs?stream=stdout&offset=0&limit=65536" | jsonfield "['data']" | tr -d '\n')" "hi"

echo "### 11. security: traversal rejected (expect 422)"
check "traversal.read" "$(httpcode -H "$K" "$B/v1/workspaces/$WS/files/content?path=../etc/passwd")" "422"
check "traversal.write" "$(httpcode -X PUT -H "$K" -H "$JSON" -d '{"content":"x"}' "$B/v1/workspaces/$WS/files/content?path=../evil")" "422"
check "traversal.delete" "$(httpcode -X DELETE -H "$K" "$B/v1/workspaces/$WS/files?path=../etc/passwd")" "422"

echo "### 12. security: auth and ownership"
check "auth.missing" "$(httpcode "$B/v1/workspaces")" "401"
check "auth.cross_owner" "$(httpcode -H "$OK" "$B/v1/workspaces/$WS")" "403"

echo "### 13. security: oversized JSON body (expect 413)"
BIG=$(mktemp)
python3 -c "import json;print(json.dumps({'name':'x'*1100000}))" > "$BIG"
check "body.too_large" "$(httpcode -H "$K" -H "$JSON" --data-binary @"$BIG" "$B/v1/workspaces")" "413"
rm -f "$BIG"

echo "### 14. security: symlink escape (expect 422)"
if [ -n "${CONTAINER:-}" ]; then
  docker exec "$CONTAINER" sh -c "ln -sf /etc/passwd /data/workspaces/$WS/files/passwd"
  check "symlink.escape" "$(httpcode -H "$K" "$B/v1/workspaces/$WS/files/content?path=/passwd")" "422"
  docker exec "$CONTAINER" sh -c "rm -f /data/workspaces/$WS/files/passwd"
elif [ -n "$DATA_DIR" ]; then
  ln -sf /etc/passwd "$DATA_DIR/workspaces/$WS/files/passwd"
  check "symlink.escape" "$(httpcode -H "$K" "$B/v1/workspaces/$WS/files/content?path=/passwd")" "422"
  rm -f "$DATA_DIR/workspaces/$WS/files/passwd"
else
  echo "skip symlink.escape (set DATA_DIR or CONTAINER)"
fi

# Helper: write a python file into the workspace and exec it, returning JSON.
write_and_exec() { # path  source  timeout
  curl -s -o /dev/null -X PUT -H "$K" -H "$JSON" \
    -d "$(printf '%s' "$2" | python3 -c 'import json,sys;print(json.dumps({"content":sys.stdin.read()}))')" \
    "$B/v1/workspaces/$WS/files/content?path=$1"
  curl -s -H "$K" -H "$JSON" -d "{\"cmd\":[\"python3\",\"${1#/}\"],\"timeout_sec\":$3}" "$B/v1/workspaces/$WS/exec"
}

echo "### 15. isolation: network egress blocked"
OUT=$(write_and_exec /net.py 'import socket
try:
    s=socket.create_connection(("1.1.1.1",80),3)
    print("NET_OPEN")
except Exception as e:
    print("NET_BLOCKED:",type(e).__name__)
' 10)
check_contains "net.blocked" "$(printf '%s' "$OUT" | jsonfield "['stdout']")" "NET_BLOCKED"
check "net.exit_code" "$(printf '%s' "$OUT" | jsonfield "['exit_code']")" "0"

echo "### 16. isolation: /etc/shadow not reachable"
check "shadow.denied" "$(write_and_exec /shadow.py 'try:
    print(open("/etc/shadow").read()[:10])
except Exception as e:
    print("SHADOW_DENIED:",type(e).__name__)
' 10 | jsonfield "['stdout']" | tr -d '\n')" "SHADOW_DENIED: FileNotFoundError"

echo "### 17. isolation: mount(2)/ptrace killed by seccomp (SIGSYS=159)"
check "seccomp.mount" "$(write_and_exec /mount.py 'import ctypes
libc=ctypes.CDLL("libc.so.6",use_errno=True)
libc.mount(b"none",b"/tmp",b"tmpfs",0,None)
print("MOUNT_RC_AFTER")
' 10 | jsonfield "['exit_code']")" "159"

echo "### 18. isolation: fork bomb limited by cgroup pids.max"
check_contains "cgroup.fork_limit" "$(write_and_exec /fork.py 'import os
n=0
try:
    for _ in range(500):
        pid=os.fork()
        if pid==0:
            os._exit(0)
        n+=1
except Exception as e:
    print("FORK_LIMITED:",type(e).__name__)
print("FORKED",n)
' 20 | jsonfield "['stdout']")" "FORK_LIMITED"

echo "### 19. resource ceiling: memory limit enforced (F-1)"
# With a 512 MiB RLIMIT_AS, allocating ~1 GiB must fail rather than succeed.
check_contains "rlimit.memory" "$(write_and_exec /mem.py 'try:
    b=bytearray(1024*1024*1024)
    print("ALLOC_OK")
except MemoryError:
    print("MEMORY_DENIED")
' 20 | jsonfield "['stdout']")" "MEMORY_DENIED"

echo "### 20. stop long run kills process group"
write_and_exec /long.py 'import time
print("start", flush=True)
for i in range(60):
    time.sleep(1)
' 60 >/dev/null
LRUN=$(curl -s -H "$K" -H "$JSON" -d '{"kind":"file","file":"/long.py"}' "$B/v1/workspaces/$WS/runs" | jsonfield "['run']['id']")
sleep 1
curl -s -X POST -H "$K" "$B/v1/workspaces/$WS/runs/$LRUN/stop" >/dev/null
sleep 1
check "stop.run_status" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$LRUN" | jsonfield "['run']['status']")" "killed"
if [ -n "${CONTAINER:-}" ]; then
  n=$(docker exec "$CONTAINER" pgrep -fc "long.py" || true)
else
  n=$(pgrep -fc "long.py" || true)
fi
check "stop.no_survivors" "$([ -z "$n" ] && echo 0 || echo "$n")" "0"

echo "### 21. terminal session: a real PTY inside the sandbox"
# Create a session, run `tty` and `test -t 1`, and read the output back through
# the byte-offset reader. This is the real-nsjail fix guard: without the host
# /dev/pts + /dev/ptmx mounts the session would fail with "out of pty devices"
# (now surfaced as status=failed with a diagnostic, not a running traceback).
SES_JSON=$(curl -s -H "$K" -H "$JSON" -d '{"cols":80,"rows":24}' "$B/v1/workspaces/$WS/sessions")
SES=$(printf '%s' "$SES_JSON" | jsonfield "['session']['id']")
SES_STATUS=$(printf '%s' "$SES_JSON" | jsonfield "['session']['status']")
if [ "$SES_STATUS" = "failed" ]; then
  check "session.start" "$(printf '%s' "$SES_JSON" | jsonfield "['session']['diagnostic']")" "<none>"
else
  check "session.start_status" "$SES_STATUS" "running"
fi
curl -s -o /dev/null -H "$K" -H "$JSON" \
  -d '{"data":"tty; test -t 1 && echo TTY_OK; echo /dev/pts\n"}' \
  "$B/v1/workspaces/$WS/sessions/$SES/input"
SES_OUT=""
for _ in $(seq 1 50); do
  SES_OUT=$(curl -s -H "$K" "$B/v1/workspaces/$WS/sessions/$SES/output?offset=0&limit=65536" | jsonfield "['data']")
  case "$SES_OUT" in *TTY_OK*|*"out of pty devices"*|*Traceback*) break;; esac
  sleep 0.1
done
check_contains "session.tty_device" "$SES_OUT" "/dev/pts/"
check_contains "session.is_a_tty" "$SES_OUT" "TTY_OK"
case "$SES_OUT" in
  *"out of pty devices"*|*Traceback*)
    fail=$((fail + 1)); printf 'FAIL %-28s session output shows a PTY failure\n' "session.no_pty_error" ;;
  *)
    pass=$((pass + 1)); printf 'ok   %-28s no PTY error / traceback\n' "session.no_pty_error" ;;
esac
# The output is readable from a later offset (same reader as run logs).
NEXT=$(curl -s -H "$K" "$B/v1/workspaces/$WS/sessions/$SES/output?offset=0&limit=65536" | jsonfield "['next']")
check_contains "session.offset_suffix" \
  "$(curl -s -H "$K" "$B/v1/workspaces/$WS/sessions/$SES/output?offset=$((NEXT-4))&limit=65536" | jsonfield "['data']")" \
  "$(printf '%s' "$SES_OUT" | tail -c4)"
curl -s -o /dev/null -X DELETE -H "$K" "$B/v1/workspaces/$WS/sessions/$SES"
check "session.delete" "$(httpcode -X DELETE -H "$K" "$B/v1/workspaces/$WS/sessions/$SES")" "200"

echo "### 22. delete workspace (expect 204) and get after delete (expect 404)"
check "delete.workspace" "$(httpcode -X DELETE -H "$K" "$B/v1/workspaces/$WS")" "204"
check "get.after_delete" "$(httpcode -H "$K" "$B/v1/workspaces/$WS")" "404"

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
