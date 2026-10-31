# sandboxd specification (v0.0.5)

This document is the canonical specification for the sandboxd service. It is a
faithful copy of the project brief, retained in-repo so implementation and
review stay aligned.

---

You are implementing a Go service called "sandboxd" — a secure per-user code
execution backend exposed over HTTP/JSON.

## 0. GROUND RULES

- Language: Go 1.22.
- OS: Ubuntu Noble (24.04), x86_64.
- Router: `github.com/go-chi/chi/v5`.
- Config: `github.com/knadh/koanf` (YAML).
- IDs: `github.com/oklog/ulid/v2` with prefixes `ws_` and `run_`.
- Validation: `github.com/go-playground/validator/v10`.
- Logging: `log/slog` (JSON).
- Metrics: `github.com/prometheus/client_golang`.
- Testing: stdlib `testing` + `httptest`.
- Lint: `golangci-lint` if available; otherwise `go vet ./...` must pass.
- Sandbox: `nsjail` (may or may not be installed on the host — the service must
  detect and report via `/readyz`, never crash).
- No external DB. Storage is filesystem JSON under `./data/` in v0.0.5.
- Everything must build with `go build ./...` and pass `go test ./...`.
- Generate OpenAPI 3.1 spec at `api/openapi.yaml` — it is the source of truth
  for handlers.

## 1. RESOLVED DECISIONS

See [DECISIONS.md](DECISIONS.md) for the resolved decisions with rationale.

## 2. FEATURE SPEC

Base path `/v1`. All endpoints require `Authorization: Bearer <api-key>`. All
request/response bodies are JSON unless stated. All timestamps RFC3339 UTC.

### 2.1 Meta

- `GET /healthz` → 200 `{"status":"ok"}`
- `GET /readyz` → 200 or 503 `{"status","nsjail":bool,"data_dir":bool}`
- `GET /v1/meta` → `{"version":"0.0.5","api_version":"v1","runtimes":["python3"],"limits_defaults":{...}}`
- `GET /metrics` → Prometheus text

### 2.2 Workspaces

- `POST /v1/workspaces` body `{"name","limits?":{cpu_cores,memory_mb,disk_mb,max_wall_time_sec,max_runs,allow_net}}` → 201 `{workspace}`
- `GET /v1/workspaces` → `{"workspaces":[...]}` (owned by caller)
- `GET /v1/workspaces/{id}` → `{workspace}`
- `DELETE /v1/workspaces/{id}` → 204 (stop all runs, rm -rf workspace dir)

Workspace JSON:

```json
{"id":"ws_...","owner_id":"user_...","name":"...","created_at":"...","status":"active","limits":{}}
```

### 2.3 Files (per workspace)

All paths relative to workspace root; leading `/` allowed and normalized. All
endpoints enforce path safety.

- `GET /v1/workspaces/{id}/files?path=/` → `{"entries":[{"name","path","type":"file|dir","size","mod_time"}]}`
- `GET /v1/workspaces/{id}/files/content?path=/main.py[&encoding=base64]` → file bytes (`Content-Type` detected, `Content-Disposition: attachment; filename=...`); JSON wrapper if `?meta=true`
- `PUT /v1/workspaces/{id}/files/content?path=/main.py` body `{"content":"...","encoding?":"text|base64"}` → 204
- `DELETE /v1/workspaces/{id}/files?path=/main.py` → 204
- `POST /v1/workspaces/{id}/files/mkdir` body `{"path":"/src"}` → 201
- `POST /v1/workspaces/{id}/files/rename` body `{"from":"/a","to":"/b"}` → 204
- `POST /v1/workspaces/{id}/files/upload` multipart `file` + `path` → 201

Enforce output/body caps (§1.8) and disk quota (§1.5).

### 2.4 Exec (sync)

- `POST /v1/workspaces/{id}/exec` body `{"cmd":["ls","-la"],"cwd":"/","env":{"K":"V"},"timeout_sec":30,"stdin":""}` → 200 `{"exit_code":0,"stdout":"...","stderr":"...","duration_ms":12,"truncated":false}`

Hard cap timeout (default 60 s). Captures capped at §1.8.

### 2.5 Runs (async)

- `POST /v1/workspaces/{id}/runs` body either:
  - `{"kind":"file","file":"/main.py","args":[...],"env":{...},"timeout_sec":300}`
  - `{"kind":"command","argv":["python3","main.py"],"env":{...},"timeout_sec":300}`
  → 202 `{run}`
- `GET /v1/workspaces/{id}/runs` → `{"runs":[...]}`
- `GET /v1/workspaces/{id}/runs/{runId}` → `{run}`
- `POST /v1/workspaces/{id}/runs/{runId}/stop` → 202 (idempotent; SIGTERM→SIGKILL per §1.11)
- `GET /v1/workspaces/{id}/runs/{runId}/logs?stream=stdout|stderr&offset=0&limit=65536` → `{"data":"...","offset":0,"next":123,"eof":false,"truncated":false}`

Run JSON:

```json
{"id":"run_...","workspace_id":"ws_...","kind":"file|command","argv":[...],"status":"queued|running|exited|killed|timeout|failed","exit_code":0,"started_at":"...","ended_at":"...","logs":{"stdout_bytes":0,"stderr_bytes":0,"truncated":false}}
```

Run persistence layout: `data/workspaces/{ws_id}/runs/{run_id}/meta.json`,
`stdout.log`, `stderr.log`. Update `meta.json` on every state transition
(write-temp + rename for atomicity).

### 2.6 Auth

Middleware `auth.Bearer(cfg.Keys)` extracts `principal_id`. Every workspace
endpoint checks `workspace.owner_id == principal_id` → else 403 `forbidden`.

### 2.7 Audit log

Every request logged as JSON to slog: `{ts, principal_id, workspace_id?,
run_id?, method, path, status, duration_ms, error_code?}`.

## 3. SECURITY / NSJAIL

Implement `internal/executor/nsjail.go` that builds a per-run nsjail config
file (temp file under `data/tmp/`) and invokes `nsjail --config <file>`. The
config template `configs/nsjail_python.cfg.tmpl` must include:

- namespaces: pid, mount, ipc, uts, user, net (net only if `allow_net=true`)
- `--chroot` disabled; use `--bindmount_ro` for `/usr`, `/lib`, `/lib64`,
  `/bin`, `/etc/alternatives`, `/etc/ssl/certs`, and the Python runtime;
  `--bindmount` workspace at `/workspace`; `--cwd /workspace`
- cgroups: memory = `limits.memory_mb`, pids = `max_runs * 64`, cpu derived from `cpu_cores`
  (applied via cgroup v2; requires the container to run with `--cgroupns=host`)
- rlimits: nproc, fsize (disk_mb), as (memory_mb) — nsjail interprets `as`/`fsize` in **megabytes**
- `--time_limit` = timeout
- `--user sandbox --group sandbox` (create on host if missing; document in README)
- `--seccomp_string` baseline deny: `mount`, `umount2`, `ptrace`, `bpf`,
  `kexec_load`, `kexec_file_load`, `reboot`, `swapon`, `swapoff`, `pivot_root`,
  `setns`, `unshare`, `clone3` if problems — but allow `clone`/`clone3` with
  restrictions; default to documented safe list
- `--disable_proc` (or read-only `/proc`)
- `--env` filtered to an allowlist for user-provided env only
  (`PYTHONPATH`, `PYTHONUNBUFFERED`, `PYTHONDONTWRITEBYTECODE`, `PYTHONHASHSEED`,
  `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ`); loader-injection names such as
  `LD_PRELOAD`/`LD_LIBRARY_PATH`/`DYLD_*`/`GLIBC_TUNABLES` are rejected

If nsjail binary is absent, executor returns `ErrNsjailUnavailable` (mapped to
503 `unavailable`). `/readyz` reflects this. Tests must NOT require nsjail; use
build tag `//go:build integration` for tests that invoke real nsjail.

Process kill must kill the process group (nsjail + children) per §1.11.

## 4. LAYOUT

```
cmd/sandboxd/main.go
internal/api/
internal/service/
internal/executor/
internal/workspace/
internal/storage/
internal/auth/
internal/config/
internal/model/
configs/config.example.yaml
configs/nsjail_python.cfg.tmpl
api/openapi.yaml
docs/SPEC.md
docs/API.md
docs/DECISIONS.md
README.md
Makefile
Dockerfile
.gitignore
LICENSE
```

## 5–8. Workflow, execution order, acceptance, autonomy

See [DECISIONS.md](DECISIONS.md) and [VERIFICATION.md](VERIFICATION.md). The
milestone commit history mirrors the requested execution order.
