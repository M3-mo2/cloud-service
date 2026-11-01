# sandboxd specification (v0.0.5)

This document is the canonical specification for the sandboxd service. It is a
faithful copy of the project brief, retained in-repo so implementation and
review stay aligned.

---

You are implementing a Go service called "sandboxd" — a secure per-user code
execution backend exposed over HTTP/JSON.

## 0. GROUND RULES

- Language: Go 1.22 (the brief targeted 1.22; `go.mod` requires Go 1.23 because
  the mandated koanf YAML parser declares a `go 1.23` floor — see
  [DECISIONS.md](DECISIONS.md)). The service version string is `0.0.5`
  (`model.Version`), which is independent of the per-feature decision groups in
  DECISIONS.md.
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
- `GET /readyz` → 200 or 503 `{"status":"ready"|"not_ready","nsjail":bool,"cgroups":bool,"data_dir":bool}`
- `GET /v1/meta` → `{"version":"0.0.5","api_version":"v1","runtimes":["python3"],"limits_defaults":{...},"limits_maxima":{...}}`
- `GET /metrics` → Prometheus text

### 2.2 Workspaces

- `POST /v1/workspaces` body `{"name","limits?":{cpu_cores,memory_mb,disk_mb,max_wall_time_sec,max_runs,allow_net}}` → 201 `{workspace}`
- `GET /v1/workspaces` → `{"workspaces":[...]}` (owned by caller)
- `GET /v1/workspaces/{id}` → `{workspace}`
- `PATCH /v1/workspaces/{id}` body `{"name?","limits?":{...}}` → 200 `{workspace}`
  (runtime-mutable settings; see below)
- `DELETE /v1/workspaces/{id}` → 204 (stop all runs, rm -rf workspace dir)

Workspace JSON:

```json
{"id":"ws_...","owner_id":"user_...","name":"...","created_at":"...","status":"active","limits":{},"last_activity_at":"..."}
```

`status` is `active` or `suspended` (`deleted` exists in the type vocabulary but
is never persisted: a deleted workspace is removed outright). The status is
persisted and survives a restart.

**`PATCH` — runtime-mutable settings.** All fields are optional; an absent field
**or an explicit `null`** keeps the current value (a `null` never resets a field
to zero). Mutable: `name` (non-empty after trimming, `<= 128` chars) and every
`limits` field (`cpu_cores`, `memory_mb`, `disk_mb`, `max_wall_time_sec`,
`max_runs`, `allow_net`). `suspended` is **not** patchable — use the lifecycle
endpoints. A numeric limit `<= 0` or **above the server maximum** is rejected
with `422 unprocessable` (`details` carries `field`/`value`/`max`); it is never
silently clamped. Lowering a limit applies to the **next** run/exec; a run
already in flight finishes under its existing limits. The change is persisted
atomically and survives a restart.

**Lifecycle.**

- `POST /v1/workspaces/{id}/suspend` → 200
  `{"workspace","changed","stopped_run_ids","stopped_run_count"}`. Sets
  `status:"suspended"` and stops every running/queued run (same kill path as
  `stop`). While suspended, **work is refused**: `POST .../exec` and
  `POST .../runs` return **`409 workspace_suspended`**, as do the package
  management endpoints; reads (files, runs, logs) keep working. Idempotent
  (`changed:false` when already suspended). Survives restart.
- `POST /v1/workspaces/{id}/resume` → 200 `{"workspace","changed"}`. Sets
  `status:"active"`; idempotent; does not restart stopped runs.
- `POST /v1/workspaces/{id}/stop` → 200
  `{"workspace","stopped_run_ids","stopped_run_count"}`. Kills every tracked run
  but leaves the workspace `active` (accepting new work). Idempotent; a no-op
  success when nothing is running.

**Usage.** `GET /v1/workspaces/{id}/usage` → 200 with `workspace_id`, `status`,
`created_at`, `last_activity_at`, `disk_bytes`, `disk_bytes_complete`, `disk_mb`,
`disk_percent`, `runs_total`, `runs_by_status` (map status→count), `active_runs`
(queued+running), `max_runs`, and the effective `limits`. `disk_bytes` is a
**bounded walk** of the workspace `files/` tree (at most 100 000 directory
entries); if the bound is hit, `disk_bytes_complete` is `false` and
`disk_bytes` is a lower bound. `disk_percent` is
`disk_bytes / (disk_mb * 1024 * 1024) * 100`. `last_activity_at` is the most
recent run/mutation (run start/stop, purge, PATCH, suspend/resume/stop).

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
- `GET /v1/workspaces/{id}/runs?status=&kind=&limit=&offset=` →
  `{"runs":[...],"total":N,"limit":L,"offset":O}`
- `GET /v1/workspaces/{id}/runs/{runId}` → `{run}`
- `DELETE /v1/workspaces/{id}/runs` → 200 `{"deleted":N}` (purge finished history)
- `DELETE /v1/workspaces/{id}/runs/{runId}` → 204 (delete one finished run)
- `POST /v1/workspaces/{id}/runs/{runId}/stop` → 202 (idempotent; SIGTERM→SIGKILL per §1.11)
- `GET /v1/workspaces/{id}/runs/{runId}/logs?stream=stdout|stderr&offset=0&limit=65536` → `{"data":"...","offset":0,"next":123,"eof":false,"truncated":false}`

### 2.5.2 Terminal sessions (interactive)

A **session** is a first-class, server-side interactive terminal running a real
PTY inside the workspace sandbox. It is distinct from `exec` (one-shot) and
`runs` (async, no input). Session ids are `ses_<ULID>`.

- `POST /v1/workspaces/{id}/sessions` body
  `{"shell":"/bin/sh"|null,"cwd":"..."|null,"cols":N,"rows":M,"env":{...}|null,"timeout_sec":N}`
  → 201 `{session}`
- `GET /v1/workspaces/{id}/sessions?status=&limit=&offset=` →
  `{"sessions":[...],"total":N,"limit":L,"offset":O}`
- `GET /v1/workspaces/{id}/sessions/{sessionID}` → `{session}`
- `POST /v1/workspaces/{id}/sessions/{sessionID}/input` body
  `{"data":"<raw string>"}` → 204
- `POST /v1/workspaces/{id}/sessions/{sessionID}/signal` body
  `{"signal":"INT"|"TERM"|"QUIT"|"HUP"}` → 204
- `POST /v1/workspaces/{id}/sessions/{sessionID}/resize` body
  `{"cols":N,"rows":M}` → 204
- `GET /v1/workspaces/{id}/sessions/{sessionID}/output?offset=&limit=` →
  `{"data":"...","offset":N,"next":N,"eof":bool,"truncated":bool}`
- `DELETE /v1/workspaces/{id}/sessions/{sessionID}` → 200 `{session,changed}`

Session JSON:

```json
{"id":"ses_...","workspace_id":"ws_...","shell":"/bin/sh","cwd":"/","cols":80,"rows":24,
 "status":"running|exited|killed|timed_out","created_at":"...","last_activity_at":"...",
 "started_at":"...","ended_at":"...","exit_code":null,"reason":"...",
 "output_bytes":0,"output_truncated":false}
```

**Lifecycle.** `running` → `exited` (shell exits), `killed` (DELETE /
workspace delete / suspend / service shutdown), or `timed_out` (idle timeout or
absolute lifetime). Each terminal transition carries a `reason`. All terminal
states are final.

**Limits.** A per-workspace concurrent-session cap (`sessions.max_per_workspace`,
default 4) returns **409 `too_many_sessions`**. An idle timeout
(`sessions.idle_timeout_sec`, default 900) and an absolute lifetime
(`sessions.lifetime_sec`, default 3600) both end the session with status
`timed_out`; the requested `timeout_sec` may shorten the lifetime, never lengthen
it. Sizes are bounded 1..1000. `cwd` is resolved with the workspace path-safety
helper and must exist.

**Signal vs run stop.** A session signal is delivered to the session's
**foreground process group**, so `INT` interrupts a running command without
ending the session; `POST .../runs/{id}/stop` instead terminates the run.

**Ended sessions.** `DELETE` is idempotent and retains the terminal record;
`input`/`signal`/`resize` (and `output` after `DELETE`) report **409
`session_ended`**. A session that ended on its own (shell exit or timeout) keeps
its `output` readable.

**Config keys** (`sessions:`): `max_per_workspace`, `idle_timeout_sec`,
`lifetime_sec`, `output_kb`, `max_input_kb`, `shell`, `kill_grace_sec`. The
service starts with none of them set.

Run JSON:

```json
{"id":"run_...","workspace_id":"ws_...","kind":"file|command","argv":[...],"status":"queued|running|exited|killed|timeout|failed|interrupted","exit_code":0,"started_at":"...","ended_at":"...","logs":{"stdout_bytes":0,"stderr_bytes":0,"truncated":false}}
```

**`POST /exec` does not create a run record.** Only the asynchronous
`POST .../runs` persists a `Run`; `exec` returns its output inline and leaves no
`run_` id, no log files and no listing entry. A client that wants a pollable,
auditable record must use `runs`.

**Listing is filtered and paginated.** `GET .../runs` accepts `status`
(repeatable or comma-separated; each value must be one of
`queued,running,exited,killed,timeout,failed,interrupted`, else `400`), `kind`
(`file` or `command`, else `400`), `limit` (`0`/absent means all remaining) and
`offset` (default 0). It returns newest-first and includes `total` (the count of
runs matching the filter **before** pagination) so offset pagination is
deterministic.

**Purge.** `DELETE .../runs` removes only finished runs and returns
`{"deleted":N}`; if any run is still `queued`/`running` the whole purge
**refuses** with **`409 conflict`** (`details.run_id` names one such run) and
deletes nothing. `DELETE .../runs/{runId}` removes one run (204) or returns
`409 conflict` if that run is still live.

Run persistence layout: `data/workspaces/{ws_id}/runs/{run_id}/meta.json`,
`stdout.log`, `stderr.log`. Update `meta.json` on every state transition
(write-temp + rename for atomicity).

### 2.5.1 Python packages (two tiers)

Each workspace sees two package tiers, added to the interpreter search path via
`PYTHONPATH` (**shared first, private second**):

- **Shared, read-only** — host directory `packages.shared_dir`, bind-mounted
  `rw:false` at `/opt/python/shared`, visible to every workspace, never
  installable.
- **Private, writable** — `{data_dir}/workspaces/{ws_id}/packages`, bind-mounted
  `rw:true` at `/opt/python/private`, owned by the workspace's sandbox uid.

- `GET /v1/workspaces/{id}/packages` → 200
  `{"packages":[{"name","version"}],"count","truncated","shared_enabled","shared_available","shared_path","private_path"}`.
  Lists the private tier (read from `*.dist-info`/`*.egg-info`) sorted by name,
  bounded to 1000 distributions (`truncated:true` past that). `shared_path` is
  `""` when the shared tier is unavailable; `private_path` is always
  `/opt/python/private`.
- `POST /v1/workspaces/{id}/packages/install` body `{"packages":[...]}` → 200
  `{"exit_code","output","stdout","timed_out","truncated","duration_ms","target"}`.
  Runs `python3 -m pip install --target /opt/python/private <reqs...>` **inside
  the workspace's own sandbox**. Requirements are validated strictly (see below)
  and passed to `pip` as individual argv elements, never through a shell. Needs
  network: with `allow_net:false` the index fetch fails and is reported through
  `exit_code`/`output`, not as an HTTP error. Bounded by
  `packages.install_timeout_sec` (default 120 s) and
  `packages.install_output_kb` (default 256 KiB).
- `DELETE /v1/workspaces/{id}/packages?name=<dist>` → 204 (remove one, PEP 503
  name-normalized). `DELETE .../packages?all=true` → 200
  `{"removed":N,"all":true}` (remove all). A bare delete (neither, or both) is
  `400` so "remove everything" can never be a footgun.

**Requirement validation.** Each element is rejected with `422 unprocessable`
(and never reaches a command line) when it is empty, whitespace-only, longer than
200 characters, does not start with a letter or digit, contains a character
outside `[A-Za-z0-9._+~*=<>,!\[\]-]`, or has unbalanced brackets. An empty
`packages` list is also `422`.

**Suspension.** The three package endpoints are workspace *management*, so all
three are refused with **`409 workspace_suspended`** while the workspace is
suspended (this is the one exception to "reads keep working" — listing packages
is refused too).

### 2.6 Auth

Middleware `auth.Bearer(cfg.Keys)` extracts `principal_id`. Every workspace
endpoint checks `workspace.owner_id == principal_id` → else 403 `forbidden`.

### 2.7 Audit log

Every request logged as JSON to slog: `{ts, principal_id, workspace_id?,
run_id?, method, path, status, duration_ms, error_code?}`.

### 2.8 Error model

All errors share the envelope
`{"error":{"code","message","details?":"string→string"}}`. The complete code
vocabulary defined in `internal/model/errors.go` (every constant must appear in
the table) is:

| Code | HTTP | Meaning |
|------|------|---------|
| `unauthorized` | 401 | Missing/invalid credentials. |
| `forbidden` | 403 | Valid key, resource owned by another principal. |
| `not_found` | 404 | Unknown workspace/run/file/package or unknown route. |
| `conflict` | 409 | Purging run history (or deleting one run) while a run is still queued/running. |
| `workspace_suspended` | 409 | Work (exec/run/package management/session) requested on a suspended workspace. |
| `too_many_sessions` | 409 | Concurrent terminal sessions exceed `sessions.max_per_workspace`. |
| `session_ended` | 409 | Operation on a terminal session (input/signal/resize, or output after DELETE). |
| `validation` | 400 | Bad JSON, unknown field, failed `validate` tag, bad filter/query. Also carries **405** for method-not-allowed. |
| `unprocessable` | 422 | Unsafe path/symlink escape, `cwd` escape, command not found, a limit `<= 0` or above the server maximum, invalid package requirement. |
| `payload_too_large` | 413 | Body/file over a hard cap. |
| `insufficient_storage` | 507 | Workspace disk quota exceeded. **Emitted as a string literal in `internal/api/errors.go`; not defined as a constant in `internal/model/errors.go`.** |
| `timeout` | 504 | **Defined but never emitted** by any handler (retained in the vocabulary and the OpenAPI enum). |
| `unavailable` | 503 | nsjail missing, or nsjail could not set up/launch the sandbox. |
| `internal` | 500 | Unexpected server error; unmapped filesystem conditions (e.g. listing a path that is a file). |

The code is not always a 1:1 mirror of the status: a **405 method not allowed**
carries `code:"validation"`.

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
- DNS: mount `/etc/resolv.conf` and `/etc/hosts` read-only **only when
  `allow_net=true`**. With the host network namespace shared, name resolution
  otherwise fails with `Temporary failure in name resolution` (the resolver has
  no nameserver to query) even though TCP to a raw IP works — this is what made
  `pip install` impossible for an `allow_net` workspace. A workspace without
  `allow_net` must not see the host resolver at all.
- package tiers: when the shared tier is configured and available, bind-mount it
  read-only at `/opt/python/shared`; always bind-mount the workspace's private
  tier read-write at `/opt/python/private`; inject both (shared first) via
  `PYTHONPATH`
- `--env` filtered to an allowlist for user-provided env only
  (`PYTHONPATH`, `PYTHONUNBUFFERED`, `PYTHONDONTWRITEBYTECODE`, `PYTHONHASHSEED`,
  `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ`); loader-injection names such as
  `LD_PRELOAD`/`LD_LIBRARY_PATH`/`DYLD_*`/`GLIBC_TUNABLES` are rejected

If nsjail binary is absent, executor returns `ErrNsjailUnavailable` (mapped to
503 `unavailable`). `/readyz` reflects this. Tests must NOT require nsjail; use
build tag `//go:build integration` for tests that invoke real nsjail.

**Terminal sessions reuse the exact same sandbox.** nsjail has no PTY support
(no pty/tty option) and the image has no `socat`/`expect`, but its `python3` has
`pty`/`termios`. A session therefore runs a small stdlib-only **PTY relay
helper** as the entry command, with the helper materialized in the session's
private `/tmp` and bind-mounted read-only at `/run/sandboxd`. The helper
allocates the PTY, runs the shell as a session leader, and relays a
line-delimited JSON control protocol from its stdin; its stdout is the terminal
byte stream. The session runs with the same uid/gid, mounts and seccomp policy
as any workload — the sandbox is never weakened. Client `data` is written to the
PTY as raw bytes and is never interpreted as a helper command. See
[ARCHITECTURE.md](ARCHITECTURE.md) and [DECISIONS.md](DECISIONS.md).

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
