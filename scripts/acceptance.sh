#!/usr/bin/env bash
# Full HTTP lifecycle acceptance transcript for sandboxd v0.0.5.
# Requires the server to be running with config at $CONFIG (default /tmp/sbxrun/config.yaml).
set -u
K="Authorization: Bearer dev-key"
OK="Authorization: Bearer other-key"
B="${BASE:-localhost:18080}"

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
printf 'print("uploaded")\n' > /tmp/sbxrun/uploaded.py
curl -s -H "$K" -F path=/uploaded.py -F file=@/tmp/sbxrun/uploaded.py "$B/v1/workspaces/$WS/files/upload"; echo
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
python3 -c "import json;print(json.dumps({'name':'x'*1100000}))" > /tmp/sbxrun/big.json
curl -s -o /dev/null -w "status=%{http_code}\n" -H "$K" -H 'Content-Type: application/json' --data-binary @/tmp/sbxrun/big.json "$B/v1/workspaces"
echo "### 19b. security: symlink escape (expect 422)"
ln -sf /etc/passwd /tmp/sbxrun/data/workspaces/$WS/files/passwd
curl -s -o /dev/null -w "status=%{http_code}\n" -H "$K" "$B/v1/workspaces/$WS/files/content?path=/passwd"
echo "### 20. stop long run kills process group"
printf 'import time\nprint("start", flush=True)\nfor i in range(60):\n    time.sleep(1)\n' > /tmp/sbxrun/long.py
curl -s -o /dev/null -X PUT -H "$K" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json;print(json.dumps({"content":open("/tmp/sbxrun/long.py").read()}))')" \
  "$B/v1/workspaces/$WS/files/content?path=/long.py"
LRUN=$(curl -s -H "$K" -H 'Content-Type: application/json' -d '{"kind":"file","file":"/long.py"}' "$B/v1/workspaces/$WS/runs" | python3 -c "import json,sys;print(json.load(sys.stdin)['run']['id'])")
sleep 1
echo "processes before stop:"; pgrep -af "long.py" || true
curl -s -X POST -H "$K" "$B/v1/workspaces/$WS/runs/$LRUN/stop" >/dev/null
sleep 1
echo "run status after stop:"; curl -s -H "$K" "$B/v1/workspaces/$WS/runs/$LRUN" | python3 -c "import json,sys;print(json.load(sys.stdin)['run']['status'])"
echo "processes after stop (expect none):"; pgrep -af "long.py" || echo "(none)"
echo "### 21. delete workspace (expect 204)"
curl -s -o /dev/null -w "status=%{http_code}\n" -X DELETE -H "$K" "$B/v1/workspaces/$WS"
echo "### 22. get after delete (expect 404)"
curl -s -o /dev/null -w "status=%{http_code}\n" -H "$K" "$B/v1/workspaces/$WS"
