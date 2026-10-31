# API guide

Human-friendly guide to the sandboxd HTTP/JSON API (v0.0.5).
The machine-readable contract is [api/openapi.yaml](../api/openapi.yaml).

## Conventions

- Base path: `/v1` (meta probes are unversioned).
- Auth: `Authorization: Bearer <api-key>` on every `/v1` endpoint.
- Bodies: JSON unless noted. Timestamps are RFC3339 UTC.
- Errors use a consistent envelope:
  ```json
  {"error":{"code":"unprocessable","message":"unsafe path: escapes workspace root"}}
  ```

### Error codes

| code | HTTP | meaning |
|------|------|---------|
| `unauthorized` | 401 | missing/invalid key |
| `forbidden` | 403 | resource belongs to another principal |
| `not_found` | 404 | resource missing |
| `validation` | 400 | malformed or invalid request |
| `unprocessable` | 422 | unsafe path (traversal / symlink escape) |
| `payload_too_large` | 413 | body or file over cap |
| `insufficient_storage` | 507 | workspace disk quota exceeded |
| `unavailable` | 503 | nsjail missing |
| `internal` | 500 | unexpected server error |

## Meta

```sh
curl -s localhost:8080/healthz

curl -s localhost:8080/readyz
# {"status":"ready","nsjail":true,"cgroups":true,"data_dir":true}
# (503 when nsjail is absent or cgroup v2 is unusable -- the latter means the
#  container was not started with --cgroupns=host)

curl -s -H "Authorization: Bearer $KEY" localhost:8080/v1/meta
# {"version":"0.0.5","api_version":"v1","runtimes":["python3"],"limits_defaults":{...}}

curl -s localhost:8080/metrics | head
```

## Workspaces

```sh
KEY=dev-key-please-rotate

# Create
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"demo","limits":{"memory_mb":256,"allow_net":false}}' \
  localhost:8080/v1/workspaces
# => {"workspace":{"id":"ws_...","owner_id":"user_dev",...}}

WS=ws_REPLACE_ME

# List
curl -s -H "Authorization: Bearer $KEY" localhost:8080/v1/workspaces

# Get
curl -s -H "Authorization: Bearer $KEY" localhost:8080/v1/workspaces/$WS

# Delete (stops runs, removes the tree)
curl -s -X DELETE -H "Authorization: Bearer $KEY" localhost:8080/v1/workspaces/$WS
```

## Files

Paths are relative to the workspace root; a leading `/` is accepted.

```sh
# List a directory
curl -s -H "Authorization: Bearer $KEY" "localhost:8080/v1/workspaces/$WS/files?path=/"

# Create a directory
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"path":"/src"}' localhost:8080/v1/workspaces/$WS/files/mkdir

# Write a text file
curl -s -X PUT -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"content":"print(\"hi\")\n"}' \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/main.py"

# Write a binary file (base64)
curl -s -X PUT -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"content":"AAECAw==","encoding":"base64"}' \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/data.bin"

# Read (raw bytes, Content-Disposition set)
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/main.py"

# Read with JSON metadata wrapper
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/main.py&meta=true"
# {"path":"/main.py","content":"...","encoding":"text","size":12,"sha256":"...","truncated":false}

# Multipart upload
curl -s -H "Authorization: Bearer $KEY" -F path=/uploaded.py -F file=@./uploaded.py \
  localhost:8080/v1/workspaces/$WS/files/upload

# Rename / move
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"from":"/a.txt","to":"/src/a.txt"}' \
  localhost:8080/v1/workspaces/$WS/files/rename

# Delete
curl -s -X DELETE -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files?path=/main.py"
```

## Exec (synchronous)

```sh
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"cmd":["python3","main.py"],"cwd":"/","timeout_sec":30}' \
  localhost:8080/v1/workspaces/$WS/exec
# {"exit_code":0,"stdout":"hi\n","stderr":"","duration_ms":42,"truncated":false}
```

Hard cap: 60 s. stdout/stderr are each capped at 256 KB (`truncated:true` when hit).
`cwd` is relative to the workspace root (mounted at `/workspace`); it defaults to
the root and is rejected with 422 if it would escape. `env` is filtered through a
name allowlist (loader-injection names such as `LD_PRELOAD` are dropped).

If the sandbox cannot be set up (e.g. nsjail cannot apply cgroup v2 because the
container was not started with `--cgroupns=host`), exec returns
`503 unavailable` and the body's `details.nsjail` carries nsjail's diagnostic.

## Runs (asynchronous)

```sh
# Start a file run
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"file","file":"/main.py","timeout_sec":300}' \
  localhost:8080/v1/workspaces/$WS/runs

# Start a command run
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"command","argv":["python3","-c","print(1+1)"]}' \
  localhost:8080/v1/workspaces/$WS/runs

RUN=run_REPLACE_ME

# Poll status
curl -s -H "Authorization: Bearer $KEY" localhost:8080/v1/workspaces/$WS/runs/$RUN
# status progresses queued -> running -> exited|killed|timeout|failed|interrupted
# (interrupted = the run was still recorded as queued/running after a restart;
#  it is moved to interrupted on startup, since no process survives a restart)

# Poll logs (byte-offset based)
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/runs/$RUN/logs?stream=stdout&offset=0&limit=65536"
# {"data":"hi\n","offset":0,"next":3,"eof":true,"truncated":false}

# Stop
curl -s -X POST -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/runs/$RUN/stop
```

Each run keeps up to 10 MB per stream; the newest 100 runs per workspace are retained.

## Notes

- SSE log streaming is intentionally deferred to v0.1; poll with `offset`/`limit`.
- `stop` sends SIGTERM to the process group, waits 3 s, then SIGKILL.
