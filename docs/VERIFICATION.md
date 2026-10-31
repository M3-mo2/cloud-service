# Verification

Results for sandboxd v0.0.5 against the acceptance criteria in the spec.
Environment: Ubuntu Noble x86_64, Go toolchain (see `go version`), no real
`nsjail` binary installed on the host.

Because real nsjail could not be installed here (no root access, and the host
root filesystem was full), the HTTP lifecycle transcript below was produced with
the test-only stub at [`scripts/nsjail-stub.py`](../scripts/nsjail-stub.py).
The stub parses the exact nsjail config file that `internal/executor` renders
and then `exec`s the requested binary without isolation. The service itself has
no knowledge of the stub: it only runs whatever `sandbox.nsjail_path` points at.
Real-nsjail behavior is covered by tests behind the `integration` build tag.

## 1. Build and tests

```
$ go build ./...          # exit 0
$ go vet ./...            # exit 0
$ gofmt -l .              # empty
$ go test ./... -race     # exit 0
ok  	sandboxd/internal/api
ok  	sandboxd/internal/auth
ok  	sandboxd/internal/config
ok  	sandboxd/internal/executor
ok  	sandboxd/internal/model
ok  	sandboxd/internal/service
ok  	sandboxd/internal/storage
ok  	sandboxd/internal/workspace
```

`golangci-lint` is not installed on this host, so `go vet ./...` is the lint
gate (both the Makefile and this document use `go vet`).

Test functions: 110 (108 unit + 2 `integration`-tagged).

## 2. Probes

- `GET /healthz` → 200 `{"status":"ok"}` always while the process is alive.
- `GET /readyz` → 200 when the data directory is writable **and** nsjail is
  found; 503 `{"status":"not_ready","nsjail":false,"data_dir":true}` otherwise.
  Verified by `TestReadyzReportsNotReadyWithoutNsjail` and
  `TestReadyzReadyWithNsjail`.

## 3. Full lifecycle transcript

Reproduce with `bash scripts/acceptance.sh` against a running server.

```
### 1. healthz
{"status":"ok"}

### 2. readyz
{"data_dir":true,"nsjail":true,"status":"ready"}

### 3. meta
{"api_version":"v1","limits_defaults":{"allow_net":false,"cpu_cores":1,"disk_mb":1024,"max_runs":3,"max_wall_time_sec":300,"memory_mb":512},"runtimes":["python3"],"version":"0.0.5"}

### 4. create workspace
workspace id: ws_01M67M3GS6DZB3TE23KJ7XN7TX
### 5. mkdir /src
{"path":"/src"}

### 6. PUT /main.py
status=204
### 7. upload /uploaded.py (multipart)
{"name":"uploaded.py","path":"/uploaded.py","size":18}

### 8. list /
{
    "entries": [
        {"name": "main.py", "path": "/main.py", "type": "file", "size": 12, "mod_time": "2026-10-31T00:00:14Z"},
        {"name": "src", "path": "/src", "type": "dir", "size": 40, "mod_time": "2026-10-31T00:00:14Z"},
        {"name": "uploaded.py", "path": "/uploaded.py", "type": "file", "size": 18, "mod_time": "2026-10-31T00:00:14Z"}
    ]
}
### 9. read /main.py (meta)
{"content":"print(\"hi\")\n","content_type":"text/x-python; charset=utf-8","encoding":"text","path":"/main.py","sha256":"0ca9091eb4e31fb1ab24c8c5de92a08e4e5f402919f82ea3ca784f38534f03f3","size":12,"truncated":false}

### 10. exec (sync)
{"duration_ms":57,"exit_code":0,"stderr":"","stdout":"hi\n","truncated":false}

### 11. start run (file)
run id: run_01M67M3H0H1J32C4X7N52QAR88
### 12. run status
{"run":{"id":"run_01M67M3H0H1J32C4X7N52QAR88","workspace_id":"ws_01M67M3GS6DZB3TE23KJ7XN7TX","kind":"file","argv":["python3","main.py"],"status":"exited","exit_code":0,"started_at":"2026-10-31T00:00:14Z","ended_at":"2026-10-31T00:00:14Z","logs":{"stdout_bytes":3,"stderr_bytes":0,"truncated":false}}}

### 13. run logs stdout
{"data":"hi\n","eof":true,"next":3,"offset":0,"truncated":false}

### 14. security: traversal read (expect 422)
status=422
### 15. security: traversal write (expect 422)
status=422
### 16. security: traversal delete (expect 422)
status=422
### 17. security: missing token (expect 401)
status=401
### 18. security: cross-owner (expect 403)
status=403
### 19. security: oversized JSON body (expect 413)
status=413
### 19b. security: symlink escape (expect 422)
status=422
### 20. stop long run kills process group
processes before stop:
103284 python3 long.py
run status after stop:
killed
processes after stop (expect none):
(none)
### 21. delete workspace (expect 204)
status=204
### 22. get after delete (expect 404)
status=404
```

The run's stdout contained `hi`, the status reached `exited` with exit code 0,
polling the logs returned `hi\n`, a second (long) run was stopped and reached
`killed`, and deleting the workspace removed its directory tree.

## 4. Security rejections

All verified by `TestPathTraversalRejections`, `TestSymlinkEscapeRejected`,
`TestCrossOwnerFileAccessForbidden`, `TestOversizedJSONBody`, and the transcript
above:

| case | result |
|------|--------|
| `path=../etc/passwd` on read | 422 `unprocessable` |
| `path=../evil` on write | 422 `unprocessable` |
| `path=../etc/passwd` on delete | 422 `unprocessable` |
| symlink inside workspace → `/etc/passwd` | 422 `unprocessable` |
| missing/bad token | 401 `unauthorized` |
| cross-owner access | 403 `forbidden` |
| oversized (>1 MB) JSON body | 413 `payload_too_large` |

## 5. Process-group kill

`exec.Cmd` starts with `SysProcAttr{Setpgid: true}`. `stop` and timeout send
`SIGTERM` to `-pgid`, wait the configured grace (3 s), then `SIGKILL` to
`-pgid`. `TestKillProcessGroupReal` starts a real `sleep` in its own group,
kills it, and asserts a signalled exit. The transcript (step 20) shows
`python3 long.py` present before stop and absent after:

```
processes before stop:
103284 python3 long.py
run status after stop:
killed
processes after stop (expect none):
(none)
```

Reproduce the process check manually:

```sh
pgrep -af 'long.py|sandboxd/.*nsjail|nsjail'
```

## 6. Decisions

`docs/DECISIONS.md` contains all 17 resolved decisions, each with a one-line
rationale, plus implementation defaults.

## 7. OpenAPI

`api/openapi.yaml` validates as OpenAPI 3.1:

```
$ npx @redocly/cli lint api/openapi.yaml
Woohoo! Your API description is valid. 🎉
You have 9 warnings.          # operation-4xx-response on unauthenticated probes
```

All paths, methods, request/response schemas, and error codes are described.

## 8. Git

`git log --oneline` shows the milestone commits; `git status` is clean.

## Known limitations

- SSE log streaming is deferred to v0.1; logs are polled with `offset`/`limit`.
- Multi-node storage is deferred; storage is local filesystem JSON.
- DB-backed/dynamic authentication is deferred; keys are static in the config.
- Per-UID-per-workspace isolation is deferred to v0.1 (v0.0.5 uses a shared
  `sandbox` UID/GID with per-run user namespaces and cgroups).
- Disk quota is advisory to the sandboxed process (enforced on the file API
  path and via rlimit/`du` rechecks), not by the kernel per process tree.
