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

# wait_run_terminal WS RUN_ID [attempts] -- poll a run until it reaches a
# terminal status and print that status (or the last observed one on timeout).
wait_run_terminal() { # ws runID [attempts]
  st=""
  for _ in $(seq 1 "${3:-120}"); do
    st=$(curl -s -H "$K" "$B/v1/workspaces/$1/runs/$2" | jsonfield "['run']['status']")
    case "$st" in exited|killed|timeout|failed|interrupted) break;; esac
    sleep 0.1
  done
  printf '%s' "$st"
}

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
check "meta.version" "$(curl -s -H "$K" "$B/v1/meta" | jsonfield "['version']")" "0.0.16"

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

echo "### 7b. first-class file browsing and viewing"
# A listing path that is a file must fail loudly, never silently empty.
check "list.file_422" "$(httpcode -H "$K" "$B/v1/workspaces/$WS/files?path=/main.py")" "422"

# A browse fixture: one dir and two files with a known sorted order.
curl -s -o /dev/null -H "$K" -H "$JSON" -d '{"path":"/browse/zdir"}' "$B/v1/workspaces/$WS/files/mkdir"
curl -s -o /dev/null -X PUT -H "$K" -H "$JSON" -d '{"content":"bbb"}' "$B/v1/workspaces/$WS/files/content?path=/browse/b.txt"
curl -s -o /dev/null -X PUT -H "$K" -H "$JSON" -d '{"content":"a"}' "$B/v1/workspaces/$WS/files/content?path=/browse/a.txt"
BLIST=$(curl -s -H "$K" "$B/v1/workspaces/$WS/files?path=/browse")
# Directories first, then files, each byte-wise by name.
check "list.order" "$(printf '%s' "$BLIST" | python3 -c 'import json,sys;print(",".join(x["name"] for x in json.load(sys.stdin)["entries"]))')" "zdir,a.txt,b.txt"
check "list.first_type" "$(printf '%s' "$BLIST" | jsonfield "['entries'][0]['type']")" "dir"
check "list.total" "$(printf '%s' "$BLIST" | jsonfield "['total']")" "3"
check "list.count_eq_total" "$(printf '%s' "$BLIST" | jsonfield "['count']")" "3"
check "list.not_truncated" "$(printf '%s' "$BLIST" | jsonfield "['truncated']")" "False"
check "list.metadata" "$(printf '%s' "$BLIST" | python3 -c 'import json,sys;e=json.load(sys.stdin)["entries"];print(all(x["mode"] and x["mod_time"] and x["path"].startswith("/") for x in e))')" "True"
# Bounded paging signals the truncation and the full total.
check "list.page_count" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files?path=/browse&limit=1&offset=0" | jsonfield "['count']")" "1"
check "list.page_truncated" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files?path=/browse&limit=1&offset=0" | jsonfield "['truncated']")" "True"

# stat: metadata for one path, 404 when missing.
check "stat.file_type" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/stat?path=/main.py" | jsonfield "['type']")" "file"
check "stat.file_sha" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/stat?path=/main.py" | jsonfield "['sha256']")" "$(printf 'print("hi")\n' | sha256sum | cut -d' ' -f1)"
check "stat.file_size" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/stat?path=/main.py" | jsonfield "['size']")" "12"
check "stat.missing_404" "$(httpcode -H "$K" "$B/v1/workspaces/$WS/files/stat?path=/missing")" "404"

# PUT over a directory is 422, not 500.
check "put.over_dir_422" "$(httpcode -X PUT -H "$K" -H "$JSON" -d '{"content":"x"}' "$B/v1/workspaces/$WS/files/content?path=/browse/zdir")" "422"
# A body without content is a 4xx and writes nothing.
check "put.no_content_4xx" "$(httpcode -X PUT -H "$K" -H "$JSON" -d '{}' "$B/v1/workspaces/$WS/files/content?path=/nope.txt")" "400"
check "put.no_content_no_file" "$(httpcode -H "$K" "$B/v1/workspaces/$WS/files/stat?path=/nope.txt")" "404"
# Reading a directory as a file is 422, not 404/500.
check "read.dir_422" "$(httpcode -H "$K" "$B/v1/workspaces/$WS/files/content?path=/browse/zdir")" "422"

# Range read: exactly the requested slice with honest range/total_size.
RANGE=$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/content?path=/main.py&offset=0&limit=5&meta=true")
check "range.content" "$(printf '%s' "$RANGE" | jsonfield "['content']")" 'print'
check "range.length" "$(printf '%s' "$RANGE" | jsonfield "['range']['length']")" "5"
check "range.offset" "$(printf '%s' "$RANGE" | jsonfield "['range']['offset']")" "0"
check "range.total_size" "$(printf '%s' "$RANGE" | jsonfield "['total_size']")" "12"
check "range.whole_file" "$(printf '%s' "$RANGE" | jsonfield "['whole_file']")" "False"
check "range.truncated" "$(printf '%s' "$RANGE" | jsonfield "['truncated']")" "True"
# The partial hash is the hash of the returned slice, and differs from the full hash.
FULLHASH=$(printf 'print("hi")\n' | sha256sum | cut -d' ' -f1)
PARTIALHASH=$(printf '%s' "$RANGE" | jsonfield "['sha256']")
check "range.partial_hash" "$PARTIALHASH" "$(printf 'print' | sha256sum | cut -d' ' -f1)"
check "range.partial_ne_full" "$([ "$PARTIALHASH" = "$FULLHASH" ] && echo SAME || echo DIFF)" "DIFF"
# The negative case above is the point: a full read still equals the file hash.
check "range.full_eq_file" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/content?path=/main.py&meta=true" | jsonfield "['sha256']")" "$FULLHASH"
check "range.full_whole_file" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/content?path=/main.py&meta=true" | jsonfield "['whole_file']")" "True"

# Binary detection on a file with NUL bytes (base64 of 00 01 02).
curl -s -o /dev/null -X PUT -H "$K" -H "$JSON" -d '{"content":"AAEC","encoding":"base64"}' "$B/v1/workspaces/$WS/files/content?path=/bin.dat"
check "binary.flag" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/content?path=/bin.dat&meta=true" | jsonfield "['binary']")" "True"
check "binary.encoding" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/content?path=/bin.dat&meta=true" | jsonfield "['encoding']")" "base64"

# Line-range read on a multi-line file.
curl -s -o /dev/null -X PUT -H "$K" -H "$JSON" \
  -d '{"content":"one\ntwo\nthree\nfour\n"}' \
  "$B/v1/workspaces/$WS/files/content?path=/lines.txt"
LINES=$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/content?path=/lines.txt&start_line=2&end_line=3")
check "lines.content" "$(printf '%s' "$LINES" | jsonfield "['content']" | tr -d '\n')" "twothree"
check "lines.start_line" "$(printf '%s' "$LINES" | jsonfield "['start_line']")" "2"
check "lines.end_line" "$(printf '%s' "$LINES" | jsonfield "['end_line']")" "3"
check "lines.line_count" "$(printf '%s' "$LINES" | jsonfield "['line_count']")" "2"
check "lines.total_lines" "$(printf '%s' "$LINES" | jsonfield "['total_lines']")" "4"

# Download a file larger than caps.file_read_mb (default 5 MiB), complete.
# Upload it (the multipart cap is larger) rather than writing it through exec,
# which runs as the unprivileged sandbox user and may not own the workspace root.
BIGUP=$(mktemp)
python3 -c "open('$BIGUP','wb').write(b'X'*6291456)"
check "download.big_upload" "$(httpcode -H "$K" -F path=/big.bin -F file=@"$BIGUP" "$B/v1/workspaces/$WS/files/upload")" "201"
rm -f "$BIGUP"
check "download.big_code" "$(httpcode -H "$K" "$B/v1/workspaces/$WS/files/download?path=/big.bin")" "200"
check "download.big_len" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/download?path=/big.bin" | wc -c | tr -d ' ')" "6291456"
check "download.content_length" "$(curl -s -D - -o /dev/null -H "$K" "$B/v1/workspaces/$WS/files/download?path=/big.bin" | tr -d '\r' | awk 'tolower($1)=="content-length:"{print $2}')" "6291456"
check "download.dir_422" "$(httpcode -H "$K" "$B/v1/workspaces/$WS/files/download?path=/browse/zdir")" "422"

# The stat path reports the same size as the download.
check "download.stat_size" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/stat?path=/big.bin" | jsonfield "['size']")" "6291456"

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

echo "### 10b. runs: cwd, interpreter selection, runnable metadata (Deliverable A)"
# A file that prints its own cwd, run in a subdirectory, proves the real
# working directory (the sandbox sees the workspace at /workspace).
curl -s -o /dev/null -H "$K" -H "$JSON" -d '{"path":"/rundir"}' "$B/v1/workspaces/$WS/files/mkdir"
curl -s -o /dev/null -X PUT -H "$K" -H "$JSON" \
  -d '{"content":"import os\nprint(os.getcwd())\n"}' \
  "$B/v1/workspaces/$WS/files/content?path=/rundir/whereami.py"
CWD_RUN=$(curl -s -H "$K" -H "$JSON" -d '{"kind":"file","file":"/rundir/whereami.py","cwd":"/rundir"}' "$B/v1/workspaces/$WS/runs" | jsonfield "['run']['id']")
check "run.cwd_recorded" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$CWD_RUN" | jsonfield "['run']['cwd']")" "/rundir"
check "run.cwd_status" "$(wait_run_terminal "$WS" "$CWD_RUN")" "exited"
check "run.cwd_is_realdir" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$CWD_RUN/logs?stream=stdout&offset=0&limit=65536" | jsonfield "['data']" | tr -d '\n')" "/workspace/rundir"

# A cwd pointing at a missing path is 422, never 503 (defect N-1).
check "run.cwd_missing_422" "$(httpcode -H "$K" -H "$JSON" -d '{"kind":"file","file":"/rundir/whereami.py","cwd":"/nope"}' "$B/v1/workspaces/$WS/runs")" "422"
check "exec.cwd_missing_422" "$(httpcode -H "$K" -H "$JSON" -d '{"cmd":["pwd"],"cwd":"/nope"}' "$B/v1/workspaces/$WS/exec")" "422"

# Backward compatible: a .py file with no interpreter field still runs via python3.
PYRUN=$(curl -s -H "$K" -H "$JSON" -d '{"kind":"file","file":"/main.py"}' "$B/v1/workspaces/$WS/runs" | jsonfield "['run']['id']")
check "run.py_backcompat_interpreter" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$PYRUN" | jsonfield "['run']['interpreter']")" "python3"
check "run.py_backcompat_status" "$(wait_run_terminal "$WS" "$PYRUN")" "exited"

# runnable metadata on stat and listing, straight from the file extension.
check "stat.runnable_py" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/stat?path=/rundir/whereami.py" | jsonfield "['runnable']")" "python3"
check "stat.runnable_txt_none" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/files/stat?path=/lines.txt" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("runnable"))')" "None"

# A .sh file selects bash by extension with no interpreter field.
curl -s -o /dev/null -X PUT -H "$K" -H "$JSON" -d '{"content":"echo ext-sh\n"}' "$B/v1/workspaces/$WS/files/content?path=/ext.sh"
SHJSON=$(curl -s -H "$K" -H "$JSON" -d '{"kind":"file","file":"/ext.sh"}' "$B/v1/workspaces/$WS/runs")
SHRUN=$(printf '%s' "$SHJSON" | jsonfield "['run']['id']")
check "run.sh_by_extension" "$(printf '%s' "$SHJSON" | jsonfield "['run']['interpreter']")" "bash"
SHST=$(wait_run_terminal "$WS" "$SHRUN")
check "run.sh_by_extension_status" "$SHST" "exited"
check "run.sh_by_extension_out" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$SHRUN/logs?stream=stdout&offset=0&limit=65536" | jsonfield "['data']" | tr -d '\n')" "ext-sh"

# An explicit allowlisted interpreter runs a non-.py file.
curl -s -o /dev/null -X PUT -H "$K" -H "$JSON" -d '{"content":"echo sh-ok\n"}' "$B/v1/workspaces/$WS/files/content?path=/runme.txt"
ISEL=$(curl -s -H "$K" -H "$JSON" -d '{"kind":"file","file":"/runme.txt","interpreter":"bash"}' "$B/v1/workspaces/$WS/runs")
ISEL_RUN=$(printf '%s' "$ISEL" | jsonfield "['run']['id']")
check "run.interpreter_override" "$(printf '%s' "$ISEL" | jsonfield "['run']['interpreter']")" "bash"
ISELST=$(wait_run_terminal "$WS" "$ISEL_RUN")
check "run.interpreter_override_status" "$ISELST" "exited"
check "run.interpreter_override_out" "$(curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$ISEL_RUN/logs?stream=stdout&offset=0&limit=65536" | jsonfield "['data']" | tr -d '\n')" "sh-ok"

# An unsupported extension with no interpreter is 422 naming the supported set.
UNSUP=$(curl -s -H "$K" -H "$JSON" -d '{"kind":"file","file":"/lines.txt"}' "$B/v1/workspaces/$WS/runs")
check "run.unsupported_ext_code" "$(printf '%s' "$UNSUP" | jsonfield "['error']['code']")" "unprocessable"
check_contains "run.unsupported_ext_names_set" "$(printf '%s' "$UNSUP" | jsonfield "['error']['details']['supported_extensions']")" ".py"

# argv + kind=file is an explicit conflict (422), never a silent precedence.
check "run.argv_file_conflict_422" "$(httpcode -H "$K" -H "$JSON" -d '{"kind":"file","file":"/main.py","argv":["python3","main.py"]}' "$B/v1/workspaces/$WS/runs")" "422"

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

echo "### 20b. unbounded runs: opt-in, honest record, real survival, zero survivors (Deliverable B)"
# A workspace with a small max_wall_time_sec (5 s) so survival past the ceiling
# is provable without an hour-long test. It opts in to unbounded runs.
WSUNL=$(curl -s -H "$K" -H "$JSON" -d '{"name":"unl","limits":{"max_wall_time_sec":5,"allow_unlimited_runs":true}}' "$B/v1/workspaces" | jsonfield "['workspace']['id']")
curl -s -o /dev/null -X PUT -H "$K" -H "$JSON" \
  -d '{"content":"import time\nprint(\"UNL_START\",flush=True)\ntime.sleep(30)\n"}' \
  "$B/v1/workspaces/$WSUNL/files/content?path=/long.py"

# Denied when the workspace config forbids it: the default workspace WS has
# allow_unlimited_runs=false, so no_timeout is refused 422 naming the key.
DEN=$(curl -s -H "$K" -H "$JSON" -d '{"kind":"file","file":"/long.py","no_timeout":true}' "$B/v1/workspaces/$WS/runs")
check "unlimited.denied_code" "$(printf '%s' "$DEN" | jsonfield "['error']['code']")" "unprocessable"
check "unlimited.denied_config" "$(printf '%s' "$DEN" | jsonfield "['error']['details']['config']")" "limits.allow_unlimited_runs"

# An allowed unbounded run records unlimited honestly: no fake finite number.
UNLJSON=$(curl -s -H "$K" -H "$JSON" -d '{"kind":"file","file":"/long.py","no_timeout":true}' "$B/v1/workspaces/$WSUNL/runs")
UNL=$(printf '%s' "$UNLJSON" | jsonfield "['run']['id']")
check "unlimited.recorded_flag" "$(printf '%s' "$UNLJSON" | jsonfield "['run']['unlimited']")" "True"
check "unlimited.recorded_timeout_null" "$(printf '%s' "$UNLJSON" | jsonfield "['run']['timeout_sec']")" "None"

# A finite run on the SAME workspace is bounded by max_wall_time_sec=5: a
# timeout_sec of 2 must end in status timeout, proving the ceiling is real and
# that `no_timeout` is what bypasses it.
FINJSON=$(curl -s -H "$K" -H "$JSON" -d '{"kind":"file","file":"/long.py","timeout_sec":2}' "$B/v1/workspaces/$WSUNL/runs")
FIN=$(printf '%s' "$FINJSON" | jsonfield "['run']['id']")
check "finite.recorded_timeout" "$(printf '%s' "$FINJSON" | jsonfield "['run']['timeout_sec']")" "2"
check "finite.times_out" "$(wait_run_terminal "$WSUNL" "$FIN" 80)" "timeout"

# The unbounded run is still alive well past the 5 s ceiling.
sleep 8
check "unlimited.survives_past_ceiling" "$(curl -s -H "$K" "$B/v1/workspaces/$WSUNL/runs/$UNL" | jsonfield "['run']['status']")" "running"

# Stopping it kills the whole process group and leaves zero survivors.
curl -s -X POST -H "$K" "$B/v1/workspaces/$WSUNL/runs/$UNL/stop" >/dev/null
sleep 1
check "unlimited.stop_status" "$(curl -s -H "$K" "$B/v1/workspaces/$WSUNL/runs/$UNL" | jsonfield "['run']['status']")" "killed"
if [ -n "${CONTAINER:-}" ]; then
  un=$(docker exec "$CONTAINER" pgrep -fc "long.py" || true)
else
  un=$(pgrep -fc "long.py" || true)
fi
check "unlimited.stop_no_survivors" "$([ -z "$un" ] && echo 0 || echo "$un")" "0"
curl -s -o /dev/null -X DELETE -H "$K" "$B/v1/workspaces/$WSUNL"

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

echo "### 21b. workspace stop stops runs AND sessions, leaving zero processes (Deliverable C)"
# The control-plane gap: POST /workspaces/{id}/stop must stop the workspace's
# runs *and* its live terminal sessions, and report both counts.
WSSTP=$(curl -s -H "$K" -H "$JSON" -d '{"name":"stopall"}' "$B/v1/workspaces" | jsonfield "['workspace']['id']")
curl -s -o /dev/null -X PUT -H "$K" -H "$JSON" \
  -d '{"content":"import time\nprint(\"STOP_START\",flush=True)\ntime.sleep(60)\n"}' \
  "$B/v1/workspaces/$WSSTP/files/content?path=/stopme.py"
STP_RUN=$(curl -s -H "$K" -H "$JSON" -d '{"kind":"file","file":"/stopme.py"}' "$B/v1/workspaces/$WSSTP/runs" | jsonfield "['run']['id']")
STP_SES=$(curl -s -H "$K" -H "$JSON" -d '{"cols":80,"rows":24}' "$B/v1/workspaces/$WSSTP/sessions" | jsonfield "['session']['id']")
# Give the session a command that is definitely alive so the process tree exists.
curl -s -o /dev/null -H "$K" -H "$JSON" -d '{"data":"sleep 600\n"}' "$B/v1/workspaces/$WSSTP/sessions/$STP_SES/input"
sleep 2
STOPJSON=$(curl -s -X POST -H "$K" "$B/v1/workspaces/$WSSTP/stop")
check "stop.runs_count" "$(printf '%s' "$STOPJSON" | jsonfield "['stopped_run_count']")" "1"
check "stop.sessions_count" "$(printf '%s' "$STOPJSON" | jsonfield "['stopped_session_count']")" "1"
check "stop.run_id_reported" "$(printf '%s' "$STOPJSON" | jsonfield "['stopped_run_ids'][0]")" "$STP_RUN"
check "stop.session_id_reported" "$(printf '%s' "$STOPJSON" | jsonfield "['stopped_session_ids'][0]")" "$STP_SES"
sleep 1
check "stop.run_status" "$(curl -s -H "$K" "$B/v1/workspaces/$WSSTP/runs/$STP_RUN" | jsonfield "['run']['status']")" "killed"
check "stop.session_status" "$(curl -s -H "$K" "$B/v1/workspaces/$WSSTP/sessions/$STP_SES" | jsonfield "['session']['status']")" "killed"
check "stop.session_reason" "$(curl -s -H "$K" "$B/v1/workspaces/$WSSTP/sessions/$STP_SES" | jsonfield "['session']['reason']")" "workspace_stopped"
# Zero surviving processes belonging to that workspace's tree (the run's
# interpreter and the session's shell both held stopme.py / sleep).
if [ -n "${CONTAINER:-}" ]; then
  sn=$(docker exec "$CONTAINER" pgrep -fc "stopme.py" || true)
else
  sn=$(pgrep -fc "stopme.py" || true)
fi
check "stop.zero_survivors" "$([ -z "$sn" ] && echo 0 || echo "$sn")" "0"
check "stop.workspace_active" "$(curl -s -H "$K" "$B/v1/workspaces/$WSSTP" | jsonfield "['workspace']['status']")" "active"
check "stop.delete_ws" "$(httpcode -X DELETE -H "$K" "$B/v1/workspaces/$WSSTP")" "204"

echo "### 22. delete workspace (expect 204) and get after delete (expect 404)"
check "delete.workspace" "$(httpcode -X DELETE -H "$K" "$B/v1/workspaces/$WS")" "204"
check "get.after_delete" "$(httpcode -H "$K" "$B/v1/workspaces/$WS")" "404"

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
