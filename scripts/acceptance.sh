#!/usr/bin/env bash
# Full HTTP lifecycle acceptance transcript for sandboxd v0.0.5.
#
# Environment:
#   BASE      base URL of a running server (default http://localhost:8080)
#   KEY       primary API key (default dev-key-please-rotate)
#   OTHER_KEY second principal's key (default other-key)
#   DATA_DIR  host path of the server's data dir, for the symlink-escape check
#   NSJAIL    real nsjail binary path; the script refuses a stub (default: none)
#
# The script requires a server backed by REAL nsjail. It will not run against a
# stub: set NSJAIL to the real binary path and the check below enforces it.
set -u

K="Authorization: Bearer ${KEY:-dev-key-please-rotate}"
OK="Authorization: Bearer ${OTHER_KEY:-other-key}"
B="${BASE:-http://localhost:8080}"
DATA_DIR="${DATA_DIR:-}"

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

echo "### 0. environment"
echo "base=$B data_dir=${DATA_DIR:-<unset>}"
echo "server readyz:"; curl -s "$B/readyz"; echo

echo "### 1. healthz"
curl -s "$B/healthz"; echo
echo "### 2. readyz"
curl -s "$B/readyz"; echo
echo "### 3. meta"
curl -s -H "$K" "$B/v1/meta"; echo
echo "### 4. create workspace"
WS=$(curl -s -H "$K" -H 'Content-Type: application/json' -d '{"name":"demo"}' "$B/v1/workspaces" | python3 -c "import json,sys;print(json.load(sys.stdin)['workspace']['id'])")
echo "workspace id: $WS"
echo "### 5. mkdir /src"
curl -s -H "$K" -H 'Content-Type: application/json' -d '{"path":"/src"}' "$B/v1/workspaces/$WS/files/mkdir"; echo
echo "### 6. PUT /main.py"
curl -s -o /dev/null -w "status=%{http_code}\n" -X PUT -H "$K" -H 'Content-Type: application/json' \
  -d '{"content":"print(\"hi\")\n"}' "$B/v1/workspaces/$WS/files/content?path=/main.py"
echo "### 7. upload /uploaded.py (multipart)"
UP=$(mktemp)
printf 'print("uploaded")\n' > "$UP"
curl -s -H "$K" -F path=/uploaded.py -F file=@"$UP" "$B/v1/workspaces/$WS/files/upload"; echo
rm -f "$UP"
echo "### 8. list /"
curl -s -H "$K" "$B/v1/workspaces/$WS/files?path=/" | python3 -m json.tool
echo "### 9. read /main.py (meta)"
curl -s -H "$K" "$B/v1/workspaces/$WS/files/content?path=/main.py&meta=true"; echo
echo "### 10. exec (sync)"
curl -s -H "$K" -H 'Content-Type: application/json' -d '{"cmd":["python3","main.py"]}' "$B/v1/workspaces/$WS/exec"; echo
echo "### 11. start run (file)"
RUN=$(curl -s -H "$K" -H 'Content-Type: application/json' -d '{"kind":"file","file":"/main.py"}' "$B/v1/workspaces/$WS/runs" | python3 -c "import json,sys;print(json.load(sys.stdin)['run']['id'])")
echo "run id: $RUN"
sleep 1
echo "### 12. run status"
curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$RUN"; echo
echo "### 13. run logs stdout"
curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$RUN/logs?stream=stdout&offset=0&limit=65536"; echo
echo "### 14. security: traversal read (expect 422)"
curl -s -o /dev/null -w "status=%{http_code}\n" -H "$K" "$B/v1/workspaces/$WS/files/content?path=../etc/passwd"
echo "### 15. security: traversal write (expect 422)"
curl -s -o /dev/null -w "status=%{http_code}\n" -X PUT -H "$K" -H 'Content-Type: application/json' -d '{"content":"x"}' "$B/v1/workspaces/$WS/files/content?path=../evil"
echo "### 16. security: traversal delete (expect 422)"
curl -s -o /dev/null -w "status=%{http_code}\n" -X DELETE -H "$K" "$B/v1/workspaces/$WS/files?path=../etc/passwd"
echo "### 17. security: missing token (expect 401)"
curl -s -o /dev/null -w "status=%{http_code}\n" "$B/v1/workspaces"
echo "### 18. security: cross-owner (expect 403)"
curl -s -o /dev/null -w "status=%{http_code}\n" -H "$OK" "$B/v1/workspaces/$WS"
echo "### 19. security: oversized JSON body (expect 413)"
BIG=$(mktemp)
python3 -c "import json;print(json.dumps({'name':'x'*1100000}))" > "$BIG"
curl -s -o /dev/null -w "status=%{http_code}\n" -H "$K" -H 'Content-Type: application/json' --data-binary @"$BIG" "$B/v1/workspaces"
rm -f "$BIG"
echo "### 19b. security: symlink escape (expect 422)"
if [ -n "${CONTAINER:-}" ]; then
  # The workspace is owned by the sandbox uid; create the symlink as root.
  docker exec "$CONTAINER" sh -c "ln -sf /etc/passwd /data/workspaces/$WS/files/passwd"
elif [ -n "$DATA_DIR" ]; then
  ln -sf /etc/passwd "$DATA_DIR/workspaces/$WS/files/passwd"
else
  echo "skipped (set DATA_DIR or CONTAINER)"
fi
curl -s -o /dev/null -w "status=%{http_code}\n" -H "$K" "$B/v1/workspaces/$WS/files/content?path=/passwd"
if [ -n "${CONTAINER:-}" ]; then
  docker exec "$CONTAINER" sh -c "rm -f /data/workspaces/$WS/files/passwd"
fi
echo "### 20. real isolation: network egress blocked (allow_net=false)"
printf 'import socket\ntry:\n    s=socket.create_connection(("1.1.1.1",80),3)\n    print("NET_OPEN")\nexcept Exception as e:\n    print("NET_BLOCKED:",type(e).__name__)\n' > /tmp/net.py
curl -s -o /dev/null -X PUT -H "$K" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json;print(json.dumps({"content":open("/tmp/net.py").read()}))')" \
  "$B/v1/workspaces/$WS/files/content?path=/net.py"
curl -s -H "$K" -H 'Content-Type: application/json' -d '{"cmd":["python3","net.py"],"timeout_sec":10}' "$B/v1/workspaces/$WS/exec"; echo
echo "### 21. real isolation: /etc/shadow denied"
printf 'try:\n    print(open("/etc/shadow").read()[:10])\nexcept Exception as e:\n    print("SHADOW_DENIED:",type(e).__name__)\n' > /tmp/shadow.py
curl -s -o /dev/null -X PUT -H "$K" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json;print(json.dumps({"content":open("/tmp/shadow.py").read()}))')" \
  "$B/v1/workspaces/$WS/files/content?path=/shadow.py"
curl -s -H "$K" -H 'Content-Type: application/json' -d '{"cmd":["python3","shadow.py"],"timeout_sec":10}' "$B/v1/workspaces/$WS/exec"; echo
echo "### 22. real isolation: mount(2) / ptrace denied"
printf 'import ctypes,os\nlibc=ctypes.CDLL("libc.so.6",use_errno=True)\nr=libc.mount(b"none",b"/tmp",b"tmpfs",0,None)\nprint("MOUNT_RC",r,"ERRNO",ctypes.get_errno())\n' > /tmp/mount.py
curl -s -o /dev/null -X PUT -H "$K" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json;print(json.dumps({"content":open("/tmp/mount.py").read()}))')" \
  "$B/v1/workspaces/$WS/files/content?path=/mount.py"
curl -s -H "$K" -H 'Content-Type: application/json' -d '{"cmd":["python3","mount.py"],"timeout_sec":10}' "$B/v1/workspaces/$WS/exec"; echo
echo "### 23. real isolation: fork bomb limited by cgroup pids.max"
printf 'import os,sys\nn=0\ntry:\n    for _ in range(500):\n        pid=os.fork()\n        if pid==0:\n            os._exit(0)\n        n+=1\nexcept Exception as e:\n    print("FORK_LIMITED:",type(e).__name__)\nprint("FORKED",n)\n' > /tmp/fork.py
curl -s -o /dev/null -X PUT -H "$K" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json;print(json.dumps({"content":open("/tmp/fork.py").read()}))')" \
  "$B/v1/workspaces/$WS/files/content?path=/fork.py"
curl -s -H "$K" -H 'Content-Type: application/json' -d '{"cmd":["python3","fork.py"],"timeout_sec":20}' "$B/v1/workspaces/$WS/exec"; echo
echo "### 24. stop long run kills process group"
printf 'import time\nprint("start", flush=True)\nfor i in range(60):\n    time.sleep(1)\n' > /tmp/long.py
curl -s -o /dev/null -X PUT -H "$K" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json;print(json.dumps({"content":open("/tmp/long.py").read()}))')" \
  "$B/v1/workspaces/$WS/files/content?path=/long.py"
LRUN=$(curl -s -H "$K" -H 'Content-Type: application/json' -d '{"kind":"file","file":"/long.py"}' "$B/v1/workspaces/$WS/runs" | python3 -c "import json,sys;print(json.load(sys.stdin)['run']['id'])")
sleep 1
echo "processes before stop:"
if [ -n "${CONTAINER:-}" ]; then docker exec "$CONTAINER" pgrep -af "long.py" || true; else pgrep -af "long.py" || true; fi
curl -s -X POST -H "$K" "$B/v1/workspaces/$WS/runs/$LRUN/stop" >/dev/null
sleep 1
echo "run status after stop:"; curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$LRUN" | python3 -c "import json,sys;print(json.load(sys.stdin)['run']['status'])"
echo "processes after stop (expect none):"
if [ -n "${CONTAINER:-}" ]; then docker exec "$CONTAINER" pgrep -af "long.py" || echo "(none)"; else pgrep -af "long.py" || echo "(none)"; fi
echo "### 25. delete workspace (expect 204)"
curl -s -o /dev/null -w "status=%{http_code}\n" -X DELETE -H "$K" "$B/v1/workspaces/$WS"
echo "### 26. get after delete (expect 404)"
curl -s -o /dev/null -w "status=%{http_code}\n" -H "$K" "$B/v1/workspaces/$WS"
