# API guide

Human-friendly, self-sufficient reference for the sandboxd HTTP/JSON API
(v0.0.16). The machine-readable contract is
[api/openapi.yaml](../api/openapi.yaml); where the two disagree, **this document
follows the implemented behaviour** and the disagreement is recorded in
[Known discrepancies](#known-discrepancies) at the end.

## Overview

### Base URL

- Default listener: `http://127.0.0.1:8080` (`server.addr` in config, default
  `127.0.0.1:8080`). The service binds **loopback only** by default; expose it
  through a reverse proxy or change `server.addr` deliberately.
- The versioned router prefix is `/v1` and is part of every versioned path
  (declared in [`internal/api/router.go`](../internal/api/router.go) as
  `r.Route("/v1", ...)`). There is no way to address a workspace without it.
- Three probes are **unversioned**: `/healthz`, `/readyz`, `/metrics`.
- All request/response bodies are JSON unless stated. All timestamps are
  RFC3339 UTC.

### Authentication model

sandboxd is a **headless sandbox engine**, and these keys are **service
credentials held by an operator-owned frontend** (for example a separate
Telegram bot product), not user credentials. End users never see, hold or send
an API key; the frontend authenticates its own users and then calls sandboxd on
their behalf as a trusted backend client. Everything below therefore describes
the caller as a **service principal**, never an end user.

Every `/v1` endpoint requires a static bearer key:

```
Authorization: Bearer <api-key>
```

- Keys are configured statically under `keys:` as `{key, principal_id}` entries.
  Several keys may map to the same `principal_id`; a duplicate key keeps the
  first mapping. Comparison is constant-time per candidate key.
- The header is parsed case-insensitively on the `Bearer ` prefix; the token is
  trimmed. A missing header, an empty token, or a key that is not configured
  yields **`401 unauthorized`** (`WWW-Authenticate: Bearer`) before the handler
  runs. The unversioned probes are never authenticated.
- A valid key resolves to exactly one `principal_id`. Ownership is enforced per
  resource: on every workspace route the loaded workspace's `owner_id` must equal
  the caller's `principal_id`.
- Status codes for the ownership rule:
  - **missing / invalid key → `401 unauthorized`**;
  - **valid key, workspace owned by another principal → `403 forbidden`**;
  - **valid key, workspace does not exist → `404 not_found`** (the workspace is
    loaded before ownership is checked, so a non-existent ID is reported as
    not-found rather than forbidden);
  - `GET /v1/workspaces` lists only the caller's workspaces.

### Error model

All errors share one envelope:

```json
{"error":{"code":"unprocessable","message":"unsafe path: escapes workspace root"}}
```

| Field | Type | Notes |
|-------|------|-------|
| `error.code` | string | Machine-readable code (table below). |
| `error.message` | string | Human-readable description. |
| `error.details` | object (string→string), optional | Extra context; omitted when empty. Validation failures put field→tag pairs here; the 503 sandbox failure puts nsjail's diagnostic under `details.nsjail`; quota errors put `used_bytes`/`limit_bytes`. |

A hard cap breach or a malformed body is still reported through this envelope.

### Error codes

Every error code constant defined in
[`internal/model/errors.go`](../internal/model/errors.go), plus the one
additional code the router emits:

| Code | HTTP | Meaning | Triggered by |
|------|------|---------|--------------|
| `unauthorized` | 401 | Missing or invalid credentials | No/invalid `Authorization` header on a `/v1` route. |
| `forbidden` | 403 | Resource belongs to another principal | Valid key, `owner_id` mismatch. |
| `not_found` | 404 | Resource missing | Unknown workspace/run/file, unknown route. |
| `conflict` | 409 | Operation conflicts with current state | Purging run history (or deleting one run) while a run is still queued/running. |
| `tunnels_disabled` | 422 | Quick tunnels forbidden | `POST .../tunnels` when `tunnels.enabled` is false (`details.config:"tunnels.enabled"`), the workspace has not opted in (`limits.allow_tunnels`), or the workspace has no egress (`limits.allow_net`), which cloudflared needs. |
| `too_many_tunnels` | 422 | Per-workspace tunnel cap reached | `POST .../tunnels` when `tunnels.max_per_workspace` active tunnels already exist. |
| `workspace_suspended` | 409 | Work refused because the workspace is suspended | `POST .../exec`, `POST .../runs`, `POST .../sessions` or a session sub-resource on a `suspended` workspace. |
| `too_many_sessions` | 409 | Per-workspace concurrent-session cap reached | `POST .../sessions` when `sessions.max_per_workspace` is already in use. |
| `session_ended` | 409 | Operation on a terminal session | `input`/`signal`/`resize` (or `output` after `DELETE`) on a session that has ended; the workspace check wins while suspended. |
| `validation` | 400 | Malformed or invalid request | Bad JSON, unknown JSON field, failing `validate` tag, missing query/body field, bad `stream`/`status`/`kind` filter, unknown route body. Also carries the **405** status for method-not-allowed. |
| `unprocessable` | 422 | Unsafe path or invalid limit/target | Path traversal / symlink escape, `cwd` escape or a `cwd` that is missing / not a directory, command not found, deleting the workspace root, file-run target that is a directory or an unsupported extension with no interpreter, an interpreter outside the allowlist, `argv` supplied together with `kind:file`, a `no_timeout` request forbidden at the server or workspace level, a limit `<= 0` or **above the server maximum**, conflicting byte/line range parameters, a request `limit` above `file_read_mb`. |
| `not_directory` | 422 | Wrong node type for the operation | Listing a path that is a file, reading/writing/downloading a directory as a file, writing over a directory. The path exists, so this is never 404 (see discrepancies 9). |
| `payload_too_large` | 413 | Body or file over a hard cap | JSON body > `json_body_mb`, upload > `upload_mb`, file write > `file_write_mb`, a `GET /files/content` `limit` above `file_read_mb`. |
| `insufficient_storage` | 507 | Workspace disk quota exceeded | Write/upload/rename past `limits.disk_mb`. |
| `timeout` | 504 | Timeout | **Defined but never emitted** (see discrepancies). |
| `tunnel_url_unavailable` | 504 | No public URL was observed in time | `POST .../tunnels` when cloudflared did not print a `trycloudflare.com` URL within `tunnels.url_timeout_sec`, including when Cloudflare's rate-limited quick-tunnel provisioning returns `429` (cloudflared's own text is in `details.reason`). The engine never fabricates a URL. |
| `unavailable` | 503 | Sandbox backend unavailable | nsjail binary missing, or nsjail could not set up/launch the sandbox (`details.nsjail`); also when the network/tunnel layer is not configured. |
| `internal` | 500 | Unexpected server error | Bugs and genuinely unmapped conditions. |

Note the code is not always a 1:1 mirror of the status: a **405 method not
allowed** response carries `code:"validation"`.

### Content types and size caps

Requests are `application/json` except multipart upload. JSON responses are
`application/json; charset=utf-8`. File reads return the raw bytes with a
detected `Content-Type` (unless `meta=true`).

Hard caps, all from `internal/config` (never raisable per request):

| Config key | Default | Applies to | Effect when hit |
|------------|---------|------------|-----------------|
| `caps.json_body_mb` | 1 | Every JSON request body | `413 payload_too_large` |
| `caps.upload_mb` | 50 | `POST /files/upload` multipart body and file | `413 payload_too_large` |
| `caps.file_write_mb` | 10 | `PUT /files/content` payload | `413 payload_too_large` |
| `caps.file_read_mb` | 5 | `GET /files/content` inline reads | returns a **prefix** with `truncated:true` (not an error); a request `limit` above it is `413` |
| `caps.file_list_max` | 1000 | `GET /files` page size | returns a bounded page with `truncated:true` and the full `total` |
| `caps.exec_stdout_kb` | 256 | Sync exec stdout | `truncated:true` |
| `caps.exec_stderr_kb` | 256 | Sync exec stderr | `truncated:true` |
| `caps.run_log_mb` | 10 | Each async run log stream | `truncated:true` on the run |
| `sessions.output_kb` | 1024 | Session PTY output log | `output_truncated:true` on the session |
| `sessions.max_input_kb` | 64 | A single session input request | `413 payload_too_large` |

Because `PUT /files/content` always travels in a JSON body, `json_body_mb`
(1 MiB) is the effective ceiling for a single write; `file_write_mb` only bites
if the JSON cap is raised.

## Endpoint reference

Every route registered in `internal/api/router.go`, in router order.

### GET /healthz

Liveness probe. Unauthenticated.

- **Response 200:** `{"status":"ok"}`.
- **Errors:** none.
- **Semantics:** returns 200 whenever the process is alive; it says nothing about
  dependencies.

```sh
curl -s localhost:8080/healthz
# {"status":"ok"}
```

### GET /readyz

Readiness probe. Unauthenticated.

- **Response 200 (ready) / 503 (not ready):** `{"status":"ready"|"not_ready",
  "nsjail":bool,"cgroups":bool,"data_dir":bool,"network":{...},
  "tunnels":{...},"identity_masking":{"enabled":bool,"ready":bool,"reason":str}}`.
- **Errors:** none; unsuitability is expressed by the 503 and the boolean fields.
- **Semantics:** `data_dir` is true when a temp file can be created under the
  data directory; `nsjail` when the nsjail binary is found; `cgroups` when the
  cgroup v2 hierarchy at `/sys/fs/cgroup` is writable by the process (requires a
  container started with `--cgroupns=host`). Any false ⇒ 503. When
  `identity_masking.enabled` is true, `ready` reports whether the host-identity
  masking shim can be applied: enabled-but-not-ready makes the whole probe 503,
  so a deploy that would leak the host identity is never reported ready.

```sh
curl -s localhost:8080/readyz
# {"cgroups":true,"data_dir":true,"identity_masking":{"enabled":true,"ready":true},
#  "nsjail":true,"status":"ready"}
```

### GET /metrics

Prometheus text exposition. Unauthenticated.

- **Response 200:** Prometheus text (`text/plain`). Includes
  `sandboxd_http_requests_total{sandboxd_http_request_duration_seconds}`,
  `sandboxd_http_inflight_requests`, `sandboxd_runs_total`, plus Go/process
  collectors.
- **Errors:** none.

```sh
curl -s localhost:8080/metrics | head
```

### GET /v1/meta

Service metadata: version, API version, available runtimes, and the server's
default per-workspace limits.

- **Request:** none.
- **Response 200:**

  ```json
  {
    "version": "0.0.16",
    "api_version": "v1",
    "runtimes": ["python3"],
    "interpreters": ["bash", "node", "perl", "python3"],
    "runnable_extensions": [".bash", ".cjs", ".js", ".mjs", ".pl", ".pm", ".py", ".sh"],
    "limits_defaults": {
      "cpu_cores": 1, "memory_mb": 512, "disk_mb": 1024,
      "max_wall_time_sec": 300, "max_runs": 3, "allow_net": false,
      "allow_unlimited_runs": false, "allow_tunnels": false
    },
    "limits_maxima": {
      "cpu_cores": 8, "memory_mb": 8192, "disk_mb": 51200,
      "max_wall_time_sec": 3600, "max_runs": 64, "allow_net": true,
      "allow_unlimited_runs": false
    },
    "network": {
      "enabled": false, "available": true, "ready": true, "egress": false
    },
    "tunnels": {
      "enabled": false, "ready": false,
      "max_per_workspace": 2, "url_timeout_sec": 45, "reason": "tunnels.enabled is false"
    },
    "identity_masking": {
      "enabled": true, "ready": true,
      "mechanism": "ld.so.preload shim (uname/sysinfo/sysconf/sched_getaffinity/getauxval)",
      "limits": "a statically-linked binary, a musl runtime, or a raw syscall(2) bypasses the shim"
    }
  }
  ```
- **Errors:** 401 `unauthorized`.
- **Semantics:** `limits_defaults` is the value a workspace gets when `limits`
  is omitted at creation; it comes from the `limits:` config block.
  `limits_maxima` is the server ceiling (`max_limits:` config) enforced on
  creation and `PATCH`. `interpreters` is the allowlist of interpreters a
  `kind:file` run may use (see [File execution and interpreters](#file-execution-and-interpreters));
  `runnable_extensions` is the file extensions those interpreters cover, which
  is exactly what a file's `runnable` field (from `stat`/listing) reports.
  `network` and `tunnels` report the egress/tunnel capabilities **truthfully**:
  `enabled` is the server switch, `ready` is whether the layer can actually
  serve, and `reason` explains a `ready:false`. `network.egress` is true only
  when the chained-slirp4netns egress fabric is configured on.
  `identity_masking` reports the host-identity masking capability:
  `enabled` is the server switch (`identity.enabled`, default true), `ready` is
  whether the shim can be applied, `reason` explains a not-ready state, and
  `mechanism`/`limits` state plainly how it works and what it does **not**
  cover (a statically-linked, musl or raw-`syscall(2)` binary bypasses it).

```sh
KEY=dev-key-please-rotate
curl -s -H "Authorization: Bearer $KEY" localhost:8080/v1/meta
```

### POST /v1/workspaces

Create a workspace owned by the authenticated principal.

- **Request body (`createWorkspaceRequest`):**

  | Field | Go type | JSON | Required | Validation | Default when omitted |
  |-------|---------|------|----------|------------|----------------------|
  | `name` | `string` | `name` | yes | `required,min=1,max=128` | — (400 if missing/empty/>128) |
  | `limits` | `*limitsJSON` | `limits` | no | none | server `limits` defaults |

  `limits` fields (all optional; a value of 0/absent falls back to the server
  default; only positive numeric values take effect):

  | Field | Go type | JSON | Notes |
  |-------|---------|------|-------|
  | `cpu_cores` | `float64` | `cpu_cores` | fractional allowed (e.g. `0.5`) |
  | `memory_mb` | `int` | `memory_mb` | |
  | `disk_mb` | `int` | `disk_mb` | disk quota |
  | `max_wall_time_sec` | `int` | `max_wall_time_sec` | async run ceiling |
  | `max_runs` | `int` | `max_runs` | maps to cgroup pids max = value × 64 |
  | `allow_net` | `bool` | `allow_net` | `false` (own empty namespace, no egress) or `true` (egress via chained slirp4netns) |
  | `allow_unlimited_runs` | `bool` | `allow_unlimited_runs` | workspace opt-in for wall-clock-unbounded runs; `false` by default; a `true` value is rejected with `422` unless the server `max_limits.allow_unlimited_runs` is also `true` |
  | `allow_tunnels` | `bool` | `allow_tunnels` | workspace opt-in for quick tunnels; `false` by default; a `true` value is rejected with `422` unless the server `tunnels.enabled` is also `true` |

  Unknown JSON fields are rejected (`400 validation`, `details.error`).
- **Response 201:**

  ```json
  {"workspace":{"id":"ws_01J...","owner_id":"user_dev","name":"demo",
    "created_at":"2026-01-02T15:04:05Z","status":"active",
    "limits":{"cpu_cores":1,"memory_mb":256,"disk_mb":1024,
              "max_wall_time_sec":300,"max_runs":3,"allow_net":false}}}
  ```
- **Errors:** 400 `validation` (bad JSON / failed tag), 401 `unauthorized`,
  413 `payload_too_large` (body > `json_body_mb`), 422 `unprocessable` (a limit
  above the server maximum).
- **Semantics:** `allow_net` is always taken from the request when a `limits`
  object is present (so `{"limits":{}}` sets `allow_net` to `false`), and only
  from the server default when `limits` is omitted entirely. Numeric limits are
  bounded by the server-level maxima (`max_limits:` in config; defaults
  `cpu_cores` 8, `memory_mb` 8192, `disk_mb` 51200, `max_wall_time_sec` 3600,
  `max_runs` 64); a request above a maximum is `422 unprocessable`. Limits can
  be changed later with `PATCH`.

```sh
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"demo","limits":{"memory_mb":256,"allow_net":false}}' \
  localhost:8080/v1/workspaces
```

### GET /v1/workspaces

List the caller's workspaces.

- **Response 200:** `{"workspaces":[ <workspace>, ... ]}`, newest first, with
  both `status:"active"` and `status:"suspended"` entries (only the internal
  `deleted` marker, which deletion never writes because it removes the tree, is
  filtered out).
- **Errors:** 401 `unauthorized`.
- **Semantics:** empty result is `{"workspaces":[]}` (never `null`). A
  suspended workspace is still listed; use its `status` field to tell.

```sh
curl -s -H "Authorization: Bearer $KEY" localhost:8080/v1/workspaces
```

### GET /v1/workspaces/{workspaceID}

Fetch one workspace.

- **Path parameter:** `workspaceID` — the `ws_<ULID>` id.
- **Response 200:** `{"workspace": <workspace>}`.
- **Errors:** 401 `unauthorized`, 403 `forbidden` (owned by another principal),
  404 `not_found`.

```sh
WS=ws_REPLACE_ME
curl -s -H "Authorization: Bearer $KEY" localhost:8080/v1/workspaces/$WS
```

### PATCH /v1/workspaces/{workspaceID}

Update a subset of a workspace's fields **without recreating it**. This is the
runtime-mutable settings endpoint.

- **Path parameter:** `workspaceID`.
- **Request body (`patchWorkspaceRequest`):** all fields optional; an absent
  field **or an explicit `null`** keeps the current value (a `null` never resets
  a field to zero):

  | Field | Go type | JSON | Notes |
  |-------|---------|------|-------|
  | `name` | `*string` | `name` | non-empty after trimming, `<= 128` chars; `400` otherwise |
  | `limits.cpu_cores` | `*float64` | `limits.cpu_cores` | must be `> 0` and `<= max_limits.cpu_cores` |
  | `limits.memory_mb` | `*int` | `limits.memory_mb` | must be `> 0` and `<= max_limits.memory_mb` |
  | `limits.disk_mb` | `*int` | `limits.disk_mb` | must be `> 0` and `<= max_limits.disk_mb` |
  | `limits.max_wall_time_sec` | `*int` | `limits.max_wall_time_sec` | must be `> 0` and `<= max_limits.max_wall_time_sec` |
  | `limits.max_runs` | `*int` | `limits.max_runs` | must be `> 0` and `<= max_limits.max_runs` |
  | `limits.allow_net` | `*bool` | `limits.allow_net` | `true`/`false` |
  | `limits.allow_unlimited_runs` | `*bool` | `limits.allow_unlimited_runs` | workspace opt-in for unbounded runs; `true` is rejected with `422` unless `max_limits.allow_unlimited_runs` is also `true` |

  `suspended` is **not** patchable; use the lifecycle endpoints below.

- **Response 200:** `{"workspace": <workspace>}`, the same shape as
  `GET /v1/workspaces/{workspaceID}`.
- **Errors:** 400 `validation` (bad JSON, empty/too-long name, unknown field),
  401, 403, 404, 422 `unprocessable` (a limit `<= 0`, or above the server
  maximum; `details` carries `field`, `value`, `max`).
- **Semantics:**
  - The update is a merge: only provided, non-null fields change. A numeric
    value that is positive but above the server maximum is **rejected with
    422**, never silently clamped (the maxima are documented in
    [Resource limits](#resource-limits)).
  - Lowering a limit applies to the **next** run/exec. A run already in flight
    is **not** retroactively killed — it finishes under its existing limits
    (see [DECISIONS.md](DECISIONS.md)).
  - The updated limits are persisted atomically and survive a service restart.

```sh
curl -s -X PATCH -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"limits":{"memory_mb":256}}' \
  localhost:8080/v1/workspaces/$WS
```

### DELETE /v1/workspaces/{workspaceID}

Delete a workspace.

- **Path parameter:** `workspaceID`.
- **Response 204:** empty body.
- **Errors:** 401 `unauthorized`, 403 `forbidden`, 404 `not_found`.
- **Semantics:** all in-flight runs are cancelled and killed (the service waits
  up to 5 s for them to drain), then the entire workspace tree — metadata, files
  and all run records/logs — is removed with `rm -rf`. Deletion is not soft.

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE \
  -H "Authorization: Bearer $KEY" localhost:8080/v1/workspaces/$WS
# 204
```

### POST /v1/workspaces/{workspaceID}/suspend

Suspend the workspace: new work is refused and running runs are stopped.

- **Path parameter:** `workspaceID`.
- **Response 200:**

  ```json
  {"workspace": <workspace with status "suspended">,
   "changed": true, "stopped_run_ids": ["run_..."], "stopped_run_count": 1}
  ```

- **Errors:** 401, 403, 404.
- **Semantics:**
  - Sets `status` to `suspended` and **stops every running/queued run** of the
    workspace (same kill path as `stop`). `stopped_run_ids` lists the runs that
    were signalled.
  - It also **ends every live terminal session** of the workspace (reason
    `workspace_suspended`); a suspended workspace must not keep any work
    running. The response reports runs only — session ids are not listed here.
  - While suspended, **every operation that starts work is refused**:
    `POST .../exec` and `POST .../runs` return **409 `workspace_suspended`**.
    Reads keep working — file listing/reading, run listing, run lookup and log
    reads are unaffected.
  - **Idempotent:** suspending an already-suspended workspace returns
    `changed: false` (it still stops anything running).
  - The suspended status is persisted and survives a restart; work stays
    refused afterwards.

```sh
curl -s -X POST -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/suspend
```

### POST /v1/workspaces/{workspaceID}/resume

Resume a suspended workspace; work is accepted again.

- **Path parameter:** `workspaceID`.
- **Response 200:** `{"workspace": <workspace with status "active">, "changed": true}`.
- **Errors:** 401, 403, 404.
- **Semantics:** sets `status` to `active`. Idempotent: resuming an
  already-active workspace returns `changed: false`. Does not restart anything
  that was stopped.

```sh
curl -s -X POST -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/resume
```

### POST /v1/workspaces/{workspaceID}/stop

Stop **everything** in the workspace — every running/queued run, every live
terminal session, and every detached process — and leave it `active`.

- **Path parameter:** `workspaceID`.
- **Response 200:**

  ```json
  {"workspace": <workspace with status "active">,
   "stopped_run_ids": ["run_..."], "stopped_run_count": 1,
   "stopped_session_ids": ["ses_..."], "stopped_session_count": 1,
   "stopped_process_ids": ["proc_..."], "stopped_process_count": 2}
  ```

- **Errors:** 401, 403, 404.
- **Semantics:** this is the "stop my thing" control. It kills every tracked run
  (SIGTERM→SIGKILL process group) **and** ends every live terminal session
  (reason `workspace_stopped`) **and** stops every `detached` process — a process
  started from a session that carries no run/session attribution (see
  [process inventory](#get-v1workspacesworkspaceidprocesses)); it reports what
  was stopped via all three counts and all three id lists. The workspace keeps
  `status: active`, so new work is still accepted. **Idempotent and a no-op
  success** (`*_count: 0`) when nothing is running. It differs from `suspend`
  only in that it does **not** block future work.
  - The existing `stopped_run_*` and `stopped_session_*` fields keep their exact
    shape (the frontend and `scripts/acceptance.sh` read them); the
    `stopped_process_*` fields are additive.
  - Before this change the endpoint stopped runs only and left sessions (and any
    process a session had started) running; it now stops both, and the detached
    processes too (see [DECISIONS.md](DECISIONS.md) #59 and #63, and
    [VERIFICATION.md §9f](VERIFICATION.md)).

```sh
curl -s -X POST -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/stop
```

### GET /v1/workspaces/{workspaceID}/processes

List **every live process** belonging to the workspace, including processes
started from a terminal session. This is the process inventory.

- **Path parameter:** `workspaceID`.
- **Response 200:**

  ```json
  {"processes":[
     {"id":"proc_0123...","pid":179173,"ppid":179172,
      "argv":["python3","bot.py"],"kind":"session",
      "source_id":"ses_01J...","started_at":"2026-01-02T15:04:05Z"},
     {"id":"proc_abcd...","pid":179200,"ppid":1,
      "argv":["sleep","600"],"kind":"detached",
      "source_id":"","started_at":"2026-01-02T15:04:06Z"}],
   "count":2}
  ```

  | Field | Meaning |
  |-------|---------|
  | `id` | Stable opaque id, a hash of `(workspace, pid, kernel start time)`. It is what the single-process stop accepts and stays stable for the process's life. |
  | `pid` | Host-side process id. |
  | `ppid` | Host-side parent pid. |
  | `argv` | The command line. |
  | `kind` | `run`, `session`, or `detached`. |
  | `source_id` | The run id / session id it belongs to; empty for `detached`. |
  | `started_at` | When the process appeared. |

- **Errors:** 401 `unauthorized`, 403 `forbidden`, 404 `not_found`.
- **Semantics (attribution is the point):** the inventory enumerates the members
  of each per-run / per-session **cgroup** and maps each host pid back to its
  owning run/session using a tag the engine injects into every sandbox
  (`SANDBOXD_WORKSPACE`/`SANDBOXD_KIND`/`SANDBOXD_SOURCE`). A process inside a
  workspace's sandbox that carries no source tag is reported as **`detached`** —
  it is **never hidden**; an inventory that silently omitted a process would be
  worse than one that reports it. A workload that called `os.setsid()` and left
  its process group is still found, because the cgroup (not the process group)
  is the primitive.
- **Isolation:** a workspace's inventory **never** lists another workspace's
  processes, and a foreign principal gets **403** (the existing ownership
  contract). An empty inventory is an honest `{"processes":[],"count":0}`, never
  an error and never `null`.

```sh
curl -s -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/processes
```

### POST /v1/workspaces/{workspaceID}/processes/{processID}/stop

Stop the **process tree** of one process, addressed by its stable `id` from the
inventory.

- **Path parameters:** `workspaceID`, `processID` (a `proc_<hex>` id).
- **Response 200:**

  ```json
  {"process_id":"proc_0123...","status":"stopped","stopped":true,
   "stopped_at":"2026-01-02T15:04:07Z"}
  ```

  `status` is `stopped` when a live tree was signalled, or `gone` when the
  process had already exited (idempotent) — in that case `stopped` is `false`.
- **Errors:** 401 `unauthorized`, 403 `forbidden` (foreign principal or a
  workspace the caller does not own), 404 `not_found` (an id that does not name
  a process in this workspace).
- **Semantics:** **killing does not rely on the process group.** The stop uses
  the process's sandbox **cgroup** (`cgroup.kill`, or an equivalent that signals
  every member), so a workload that called `os.setsid()` and left its process
  group is still stopped. The call is idempotent: a process that has already
  exited is reported `gone` rather than an error, and a repeated stop is a 404
  because the process is no longer in the inventory.

```sh
PID=$(curl -s -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/processes | \
  python3 -c 'import json,sys;print(json.load(sys.stdin)["processes"][0]["id"])')
curl -s -X POST -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/processes/$PID/stop
```

### GET /v1/workspaces/{workspaceID}/usage

Report the workspace's actual disk use, run counters and effective limits.

- **Path parameter:** `workspaceID`.
- **Response 200:**

  ```json
  {"workspace_id":"ws_...","status":"active",
   "created_at":"2026-01-02T15:04:05Z","last_activity_at":"2026-01-02T15:10:00Z",
   "disk_bytes":11,"disk_bytes_complete":true,"disk_mb":1024,"disk_percent":0.001,
   "runs_total":3,"runs_by_status":{"exited":2,"killed":1},
   "active_runs":0,"max_runs":3,
   "limits":{"cpu_cores":1,"memory_mb":512,"disk_mb":1024,
             "max_wall_time_sec":300,"max_runs":3,"allow_net":false}}
  ```

- **Errors:** 401, 403, 404.
- **Semantics:**
  - `disk_bytes` is the size of the workspace's `files/` tree, measured with a
    **bounded walk** (at most 100 000 directory entries). If the tree is larger,
    the walk stops early, `disk_bytes_complete` is `false`, and `disk_bytes` is a
    **lower bound**. This keeps the endpoint cheap (no unbounded recursive scan,
    no cache to go stale).
  - `disk_percent` is `disk_bytes / (disk_mb * 1024 * 1024) * 100`.
  - `runs_by_status` counts all retained runs; `runs_total` is their sum.
    `active_runs` counts `queued` + `running`; compare it with `max_runs`.
  - `last_activity_at` is the most recent run/mutation (run start/stop, file
    purge, PATCH, suspend/resume/stop); `created_at` is the creation time.

```sh
curl -s -H "Authorization: Bearer $KEY" localhost:8080/v1/workspaces/$WS/usage
```

### GET /v1/workspaces/{workspaceID}/packages

List what is installed in the workspace's **private** package tier, and report
whether the shared tier is available.

- **Path parameter:** `workspaceID`.
- **Response 200:**

  ```json
  {"packages":[{"name":"requests","version":"2.31.0"},
               {"name":"Flask","version":"3.0.0"}],
   "count":2,"truncated":false,
   "shared_enabled":true,"shared_available":true,
   "shared_path":"/opt/python/shared","private_path":"/opt/python/private"}
  ```

  `packages` is sorted by name and lists only the workspace's own private
  distributions (read from `*.dist-info`/`*.egg-info` metadata). `count` mirrors
  the array length. `truncated` is `true` when the enumeration hit its bound
  (**1000** distributions), in which case the list is a prefix.
  `shared_enabled` reflects config; `shared_available` is a live probe (the
  shared directory exists and is a readable directory).   `shared_path` is
  `""` when the shared tier is unavailable, `private_path` is always
  `/opt/python/private`.
- **Errors:** 401 `unauthorized`, 403 `forbidden`, 404 `not_found`, 409
  `workspace_suspended`.
- **Semantics:** the endpoint is a bounded read (`maxListedPackages` = 1000) so
  a workspace cannot make the service walk an unbounded directory. It never
  resolves the sandbox path on the host. All three package endpoints — unlike
  file/run reads — are refused with **409 `workspace_suspended`** while the
  workspace is suspended (package management is workspace *management*, not a
  passive read).

```sh
curl -s -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/packages
```

### POST /v1/workspaces/{workspaceID}/packages/install

Install one or more requirement specifiers into the workspace's **private**
tier, without a shell.

- **Path parameter:** `workspaceID`.
- **Request body (`installPackagesRequest`):**

  | Field | Go type | JSON | Required | Validation |
  |-------|---------|------|----------|------------|
  | `packages` | `[]string` | `packages` | yes | non-empty; each element must be a plausible requirement specifier (below) |

- **Response 200:**

  ```json
  {"exit_code":0,
   "output":"Collecting requests==2.31.0\nSuccessfully installed requests-2.31.0\n",
   "stdout":"... (alias of output) ...",
   "timed_out":false,"truncated":false,"duration_ms":4123,
   "target":"/opt/python/private"}
  ```

  `output` carries the combined stdout+stderr, capped at
  `packages.install_output_kb` (default 256 KiB); `truncated` is `true` when the
  cap was hit. `timed_out` is `true` when the wall-clock bound
  (`packages.install_timeout_sec`, default 120 s) was reached. `exit_code` is
  `pip`'s exit code (pipeline failures, including "no network", surface here,
  not as an HTTP error).
- **Errors:** 400 `validation` (bad JSON / unknown field), 401 `unauthorized`,
  403 `forbidden`, 404 `not_found`, 409 `workspace_suspended`, 422
  `unprocessable` (empty list, or a requirement that fails validation),
  413 `payload_too_large` (body > `json_body_mb`), 503 `unavailable` (nsjail
  missing or a sandbox launch failure).
- **Validation rules (each hostile input rejected with 422, never reaching a
  command line):** empty; whitespace-only; longer than 200 characters; first
  character not a letter or digit (rejects `-e`, `--upgrade`, `-r ...`,
  `../../etc/passwd`); any character outside `[A-Za-z0-9._+~*=<>,!\[\]-]`
  (rejects `;`, spaces, tabs, newlines, `/`, `\`, `|`, `&`, `$(`, backticks,
  quotes, parentheses …); unbalanced `[` / `]`. The accepted specifiers are
  also passed to `pip` as individual **argv** elements, never through a shell.
- **Semantics:** the install runs `python3 -m pip install --target
  /opt/python/private <reqs...>` **inside the workspace's own sandbox** (the
  same nsjail isolation as any run), so package build code executes as the
  unprivileged sandbox uid and cannot write outside the workspace or into the
  shared read-only tier. Installation needs network; a workspace with
  `allow_net:false` fails at the index fetch and reports it via `exit_code` and
  `output`.

```sh
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"packages":["requests==2.31.0"]}' \
  localhost:8080/v1/workspaces/$WS/packages/install
```

### DELETE /v1/workspaces/{workspaceID}/packages

Remove one package, or all packages, from the workspace's private tier.

- **Path parameter:** `workspaceID`.
- **Query parameters:**
  - `name` (string) — remove a single distribution by normalized name.
  - `all` (string) — the exact value `true` removes **every** entry in the
    private tier.
- **Response 204:** empty body, when `name` addressed one distribution.
- **Response 200 (`?all=true`):** `{"removed":3,"all":true}` — the number of
  top-level entries removed.
- **Errors:** 400 `validation` (neither `name` nor `all=true`; or both given),
  401, 403, 404 `not_found` (no such package), 409 `workspace_suspended`.
- **Semantics:**
  - `?name=` removes only the files that belong to that distribution (its
    `RECORD`, or the `egg-info` plus same-named top-level module), then its
    metadata directory. Matching is PEP 503 name normalization
    (`My_Package` ≡ `my-package`).
  - **Remove-all requires the explicit flag.** A bare `DELETE .../packages`
    with no parameters is a **400**, and `?name=` together with `?all=true` is
    also a **400**, so "remove everything" can never be a footgun.
  - Removal is refused while the workspace is suspended (`409`).

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/packages?name=requests"
# 204

curl -s -X DELETE -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/packages?all=true"
# {"all":true,"removed":2}
```

### POST /v1/workspaces/{workspaceID}/tunnels

Expose one workspace port to the public internet with a **quick Cloudflare
tunnel** (no account, no token). Returns the public URL captured from
cloudflared's own output.

- **Path parameter:** `workspaceID`.
- **Request body:** `{"port": 5000}` — the port the workspace's app listens on
  inside its own namespace. Required; must be `1024`–`65535` (privileged ports
  cannot be bound by the unprivileged sandbox uid, so they are refused).
- **Response 201:**

  ```json
  {"tunnel":{"id":"tun_01J...","workspace_id":"ws_01J...","port":5000,
             "url":"https://random-words-here.trycloudflare.com",
             "status":"active","created_at":"2026-01-02T15:04:05Z"}}
  ```
- **Errors:** 400 `validation` (missing/out-of-range/privileged `port`),
  401, 403 `forbidden` (foreign workspace), 404 `not_found` (no such workspace),
  409 `workspace_suspended`, 422 `tunnels_disabled` (server switch off, workspace
  not opted in, or no `allow_net`), 422 `too_many_tunnels` (per-workspace cap),
  503 `unavailable` (the tunnel/network layer is not configured),
  **504 `tunnel_url_unavailable`** (cloudflared did not print a URL within
  `tunnels.url_timeout_sec`).
- **Semantics:** the URL is **only ever the one cloudflared printed**, captured
  with a bounded wait; the engine never fabricates a URL. If none is observed
  within the timeout the create fails with the machine-readable 504 and leaves
  no record. The cloudflared process runs **inside the workspace's own
  namespace** and reaches Cloudflare through the workspace's egress; a tunnel
  therefore requires `limits.allow_net` as well as the two tunnel keys.

```sh
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"port":5000}' localhost:8080/v1/workspaces/$WS/tunnels
# {"tunnel":{"id":"tun_...","url":"https://....trycloudflare.com","status":"active", ...}}
```

The user's own simplest path is the same mechanism: with a shell inside the
workspace, `cloudflared tunnel --url http://localhost:5000` prints a working URL
(cloudflared is on `PATH` in the sandbox). See
[Quick tunnels and egress](#quick-tunnels-and-egress).

### GET /v1/workspaces/{workspaceID}/tunnels

List the workspace's tunnels.

- **Response 200:** `{"tunnels":[ <tunnel>, ... ]}`, newest first. Each
  tunnel's `status` is refreshed from the live cloudflared process: `active`
  while it is running, `stopped` once it is gone.
- **Errors:** 401, 403, 404.

### GET /v1/workspaces/{workspaceID}/tunnels/{tunnelID}

Fetch one tunnel.

- **Response 200:** `{"tunnel": <tunnel>}`.
- **Errors:** 401, 403, 404 `not_found` (unknown tunnel or workspace).

### DELETE /v1/workspaces/{workspaceID}/tunnels/{tunnelID}

Stop the tunnel's cloudflared process and remove the record. After it returns,
the public URL no longer serves.

- **Response 204:** empty body.
- **Errors:** 401, 403, 404 `not_found`.
- **Semantics:** idempotent in effect — the URL stops working; the record is
  removed, so a second `DELETE` is `404`. A tunnel also dies with its workspace
  (`stop`/`suspend`/`DELETE`) and with a service restart (orphaned cloudflared
  processes are reaped at startup).

### Two-tier Python packages

A workload running in a workspace can `import` packages from **two tiers**,
without exporting anything:

| Tier | Sandbox path | Host location | Writable | Visible to |
|------|--------------|---------------|----------|------------|
| **Shared** | `/opt/python/shared` | `packages.shared_dir` (e.g. `/opt/sandboxd/packages`) | **no** (`rw:false`) | every workspace |
| **Private** | `/opt/python/private` | `{data_dir}/workspaces/{ws_id}/packages` | yes (sandbox uid only) | one workspace |

The two paths are added to the interpreter's search path via the **`PYTHONPATH`**
variable, **shared first, private second**. This works identically for
`python3 -c 'import x'`, `python3 file.py` and `python3 -m module`; no
`.pth`, `sitecustomize.py` or interpreter mutation is involved (see
[DECISIONS.md](DECISIONS.md) §36). The ordering means a private package of the
same name shadows the shared one **only for that workspace** — the shared set
can never be shadowed across workspaces, because no other workspace can write
it.

**Precedence and isolation.**

- A package present in the **shared** tier is importable with **no installation
  step** in every workspace.
- A package installed in workspace A's **private** tier is **not** importable in
  workspace B: the private directory lives in A's own data tree and is owned by
  A's sandbox uid.
- If both tiers contain the same distribution, the private copy wins **for that
  workspace only**.

**Security guarantees.**

- The shared tier is bind-mounted **read-only** (`rw:false` in the nsjail
  config): a workspace cannot modify or poison it.
- The private tier is bind-mounted writable and is the workspace's own
  directory; its host path goes through the workspace path-safety helpers and is
  never the shared location. It is deleted with the workspace.
- A missing, empty or unreadable shared directory **never blocks a run**: the
  run degrades to the private tier and still works. `/readyz` is unaffected.
- Installation runs inside the workspace's sandbox as the unprivileged sandbox
  uid and cannot write to the shared tier. `pip` setup code therefore never runs
  as the orchestrator.

**Configuration** (`packages:` in config):

| Key | Default | Meaning |
|-----|---------|---------|
| `packages.shared_dir` | `""` | host directory bind-mounted read-only as the shared tier; empty disables the mount |
| `packages.shared_enabled` | `true` | toggle the shared tier |
| `packages.shared_requirements` | `""` | optional path to a requirements file documenting the image; informational only |
| `packages.install_timeout_sec` | `120` | pip install wall-clock bound |
| `packages.install_output_kb` | `256` | pip install captured-output cap |

**Worked example.**

```sh
KEY=dev-key-please-rotate
AUTH="Authorization: Bearer $KEY"
JSON="Content-Type: application/json"
BASE=http://127.0.0.1:8080

# workspace A (owner user_dev)
A=$(curl -s -H "$AUTH" -H "$JSON" -d '{"name":"a"}' \
      $BASE/v1/workspaces | python3 -c 'import json,sys;print(json.load(sys.stdin)["workspace"]["id"])')

# install a package privately (needs allow_net:true to reach an index)
curl -s -H "$AUTH" -H "$JSON" -d '{"packages":["six==1.16.0"]}' \
  $BASE/v1/workspaces/$A/packages/install
# {"exit_code":0,"output":"... Successfully installed six-1.16.0 ...","target":"/opt/python/private"}

# it imports with no further setup
curl -s -H "$AUTH" -H "$JSON" -d '{"cmd":["python3","-c","import six; print(six.__version__)"]}' \
  $BASE/v1/workspaces/$A/exec
# {"exit_code":0,"stdout":"1.16.0\n", ...}

# a second workspace does NOT see it (isolation)
B=$(curl -s -H "$AUTH" -H "$JSON" -d '{"name":"b"}' \
      $BASE/v1/workspaces | python3 -c 'import json,sys;print(json.load(sys.stdin)["workspace"]["id"])')
curl -s -H "$AUTH" -H "$JSON" -d '{"cmd":["python3","-c","import six"]}' \
  $BASE/v1/workspaces/$B/exec
# {"exit_code":1,"stderr":"ModuleNotFoundError: No module named 'six'\n", ...}

# a package already in the shared tier imports everywhere with no install step
curl -s -H "$AUTH" -H "$JSON" -d '{"cmd":["python3","-c","import requests; print(requests.__version__)"]}' \
  $BASE/v1/workspaces/$B/exec
# {"exit_code":0,"stdout":"2.31.0\n", ...}   (when requests is present in shared_dir)
```

### GET /v1/workspaces/{workspaceID}/files

List a directory as a bounded page of entries.

- **Path parameter:** `workspaceID`.
- **Query parameters:**
  - `path` (string, default `/`) — directory path relative to the workspace
    root; a leading `/` is accepted and normalized.
  - `limit` (integer, optional) — maximum entries to return; `<= 0` or absent
    uses `caps.file_list_max` (default 1000). A value above the cap is clamped by
    the cap.
  - `offset` (integer, optional) — number of sorted entries to skip. Must be
    `>= 0`.
- **Response 200:**

  ```json
  {"path":"/",
   "entries":[{"name":"src","path":"/src","type":"dir","size":0,"mode":"0755",
               "mod_time":"2026-01-02T15:04:05Z"},
              {"name":"main.py","path":"/main.py","type":"file","size":12,
               "mode":"0644","mod_time":"2026-01-02T15:04:05Z"}],
   "count":2,"total":2,"truncated":false}
  ```

  `type` is `file`, `dir`, or `symlink`. `path` is the absolute
  workspace-rooted path (always starts with `/`). `mode` is the octal permission
  string. `size` is the entry size in bytes; a directory reports `0`.
  `mod_time` is RFC3339 UTC. For a symlink, `target` is the raw link target and
  `target_inside` says whether it resolves inside the workspace. For a regular
  file, `runnable` is the interpreter that runs it by extension (e.g. `"python3"`
  for `main.py`), or absent when no interpreter is registered — a UI can offer
  "run this file" only where it is present (see
  [File execution and interpreters](#file-execution-and-interpreters)).
- **Ordering (stable, so a client can rely on it):** directories first, then
  files (symlinks count as files), each group sorted by name byte-wise.
- **Paging:** `count` is the number of entries in this page, `total` the full
  child count, and `truncated` is true when entries were omitted. Page by
  advancing `offset` by `count` until `truncated` is false.
- **Errors:** 400 `validation` (n/a here), 401, 403, 404 `not_found` (directory
  does not exist), 422 `unprocessable` (unsafe path, bad `limit`/`offset`),
  422 `not_directory` (the path is a **file**).
- **Semantics:** the `path` is sanitized with `cleanRelative` then `secureJoin`:
  `..` above the root and symlinks resolving outside the root are rejected with
  422. Listing a path that is a **file** fails loudly with `422 not_directory` —
  it is never a silently-empty `200`.

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files?path=/"

# page two of 100-entry pages
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files?path=/&limit=100&offset=100"
```

### GET /v1/workspaces/{workspaceID}/files/stat

Return metadata for a single path without reading its contents.

- **Path parameter:** `workspaceID`.
- **Query parameters:** `path` (string, **required**).
- **Response 200:**

  ```json
  {"name":"main.py","path":"/main.py","type":"file","size":12,"mode":"0644",
   "mod_time":"2026-01-02T15:04:05Z","content_type":"text/x-python; charset=utf-8",
   "sha256":"0ca9...","binary":false,"runnable":"python3"}
  ```

  For a regular file, `sha256` is the hash of the **whole** file,
  `content_type` is detected from the extension/content, `binary` uses the
  bounded detection rule (below), and `runnable` is the interpreter that runs
  the file by extension (absent when none is registered). For a directory,
  `size` is `0` and `content_type`/`sha256`/`runnable` are omitted. For a
  symlink, `type` is `symlink`, `target` is the raw link target and
  `target_inside` is present.
- **Errors:** 400 `validation` (path missing), 401, 403, 404 `not_found`,
  422 `unprocessable` (unsafe path).
- **Semantics:** a missing path is `404`; an existing file, directory or
  symlink is always `200`.

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files/stat?path=/main.py"
```

### DELETE /v1/workspaces/{workspaceID}/files

Delete a file or directory tree.

- **Path parameter:** `workspaceID`.
- **Query parameters:** `path` (string, **required**) — target path.
- **Response 204:** empty body.
- **Errors:** 400 `validation` (path missing), 401, 403, 404 `not_found`,
  422 `unprocessable` (unsafe path; deleting the workspace root `/` is also 422).
- **Semantics:** directories are removed recursively. Disk accounting is reduced
  by the removed subtree's size.

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE \
  -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files?path=/main.py"
```

### GET /v1/workspaces/{workspaceID}/files/content

Read a file, in whole or in part.

- **Path parameter:** `workspaceID`.
- **Query parameters:**
  - `path` (string, **required**) — file path.
  - `encoding` (string, optional) — `text` (default) or `base64`; only the exact
    value `base64` is special, anything else is treated as text. Binary content
    is always returned base64-encoded, whatever `encoding` says.
  - `meta` (string, optional) — the exact value `true` selects the JSON wrapper.
  - `offset` (integer, optional) — byte offset to start at (default 0). Must be
    `>= 0`.
  - `limit` (integer, optional) — maximum bytes to return. `<= 0` or absent
    reads up to `caps.file_read_mb`; a value above `caps.file_read_mb` is
    rejected with `413 payload_too_large`. This is the **maximum `limit`
    accepted**.
  - `start_line`, `end_line` (integers, optional) — 1-based inclusive line range.
    `end_line` absent or `0` means "to the end of the file".
- **Response 200 (raw, default):** the file bytes with the detected
  `Content-Type`, `Content-Disposition: attachment; filename=<base>`,
  `Content-Length` and the headers `X-Sandboxd-Truncated`,
  `X-Sandboxd-Whole-File`, `X-Sandboxd-Binary`, `X-Sandboxd-Total-Size`,
  `X-Sandboxd-Range-Offset`, `X-Sandboxd-Range-Length`. Use `meta=true` when a
  `sha256` is wanted.
- **Response 200 (`meta=true`, byte read):**

  ```json
  {"path":"/main.py","content":"print(\"hi\")\n","encoding":"text",
   "size":12,"total_size":12,"range":{"offset":0,"length":12},
   "whole_file":true,"sha256":"0ca9...",
   "content_type":"text/x-python; charset=utf-8",
   "truncated":false,"binary":false}
  ```

- **Response 200 (`meta=true`, line read):**

  ```json
  {"path":"/main.py","content":"print(\"hi\")\n","encoding":"text",
   "start_line":1,"end_line":1,"line_count":1,"total_lines":1,
   "sha256":"0ca9...","truncated":false,"binary":false}
  ```

- **Errors:** 400 `validation` (path missing), 401, 403, 404 `not_found`,
  413 `payload_too_large` (`limit` above `file_read_mb`), 422 `unprocessable`
  (unsafe path; bad `offset`/`limit`; both byte and line params; `end_line <
  start_line`), 422 `not_directory` (the path is a directory).
- **Precedence:** byte parameters (`offset`/`limit`) and line parameters
  (`start_line`/`end_line`) are **mutually exclusive**; supplying both is a
  `422`. With neither, the whole file is read up to `file_read_mb`.
- **Semantics (integrity):** for a byte read, `sha256` is the hash of **exactly
  the bytes returned in this response**, never of a larger slice. `range` reports
  the `offset`/`length` actually returned, `total_size` is the real file size,
  and `whole_file` is true only when the response contains the entire file. A
  client can therefore tell a whole-file hash from a partial one from the
  response alone: `whole_file:true` (or a `sha256` equal to the file's
  `sha256sum`) means the hash covers the whole file.
- **Semantics (truncation):** a file larger than `caps.file_read_mb` yields a
  capped prefix with `truncated:true`, the `range` actually returned and
  `total_size`; truncation is never silent. A request `limit` above the cap is a
  `413`, not a silent clamp.
- **Semantics (binary):** content is reported as `binary:true` (and forced to
  base64) when a bounded leading sample — at most 8192 bytes — contains a NUL
  byte or invalid UTF-8. Detection cannot inspect a file larger than the sample;
  binary data appearing only past the sample is reported as text.
- **Semantics (lines):** a line range returns the requested lines plus
  `line_count`/`total_lines`. `end_line` past the end returns the remaining
  lines. A final line without a trailing newline still counts. Reading is
  bounded: accumulated line output is capped at `file_read_mb` (then
  `truncated:true`) and a single line is capped at 1 MiB.

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/main.py"

curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/main.py&meta=true"

# the first 1 KiB
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/big&offset=0&limit=1024&meta=true"

# lines 10..20
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/main.py&start_line=10&end_line=20"
```

### GET /v1/workspaces/{workspaceID}/files/download

Stream the **entire** file for delivery to a user.

- **Path parameter:** `workspaceID`.
- **Query parameters:** `path` (string, **required**).
- **Response 200:** the whole file, with the detected `Content-Type`,
  `Content-Disposition: attachment; filename=<base>`, `Content-Length` when the
  size is known, and `X-Sandboxd-Truncated: false` / `X-Sandboxd-Whole-File:
  true`.
- **Errors:** 400 `validation` (path missing), 401, 403, 404 `not_found`,
  422 `unprocessable` (unsafe path), 422 `not_directory` (the path is a
  directory).
- **Semantics:** the response is streamed with constant memory (it is not
  buffered), and it is **exempt from `caps.file_read_mb`** — that cap governs
  inline viewing, so a file too large to read inline can still be downloaded.
  The workspace disk quota still governs storage. The practical limit is the
  workspace's `limits.disk_mb` (the largest file that can exist) and the
  response is bounded by the client's read; there is no server-side download
  size cap.

```sh
curl -s -H "Authorization: Bearer $KEY" -OJ \
  "localhost:8080/v1/workspaces/$WS/files/download?path=/big.bin"
```

### PUT /v1/workspaces/{workspaceID}/files/content

Write (create or overwrite) a file.

- **Path parameter:** `workspaceID`.
- **Query parameters:** `path` (string, **required**).
- **Request body (`putFileRequest`):**

  | Field | Go type | JSON | Required | Validation | Default |
  |-------|---------|------|----------|------------|---------|
  | `content` | `string` | `content` | **yes** | an omitted field is a `400 validation`; an explicit `""` writes a 0-byte file | — |
  | `encoding` | `string` | `encoding` | no | `base64` decodes the content; anything else is literal text | `text` |

- **Response 204:** empty body.
- **Errors:** 400 `validation` (path missing, invalid JSON, missing `content`,
  invalid base64), 401, 403, 413 `payload_too_large` (body > `json_body_mb`, or
  content > `file_write_mb`), 422 `unprocessable`, 422 `not_directory` (the path
  is an existing directory), 507 `insufficient_storage` (quota).
- **Semantics:** missing parent directories are created. The write is accounted
  against `limits.disk_mb`. Writing over a path that is a **directory** is a
  `422 not_directory`.

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X PUT \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"content":"print(\"hi\")\n"}' \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/main.py"

# binary write
curl -s -o /dev/null -w '%{http_code}\n' -X PUT \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"content":"AAECAw==","encoding":"base64"}' \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/data.bin"
```

### POST /v1/workspaces/{workspaceID}/files/mkdir

Create a directory (with parents, like `mkdir -p`).

- **Path parameter:** `workspaceID`.
- **Request body (`mkdirRequest`):** `path` (string, `required`).
- **Response 201:** `{"path":"<echoed value of path>"}` (echoes the request,
  not the normalized path).
- **Errors:** 400 `validation`, 401, 403, 422 `unprocessable`.
- **Semantics:** creating an existing directory succeeds.

```sh
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' -d '{"path":"/src"}' \
  localhost:8080/v1/workspaces/$WS/files/mkdir
# 201
```

### POST /v1/workspaces/{workspaceID}/files/rename

Rename or move a file or directory.

- **Path parameter:** `workspaceID`.
- **Request body (`renameRequest`):** `from` (`required`), `to` (`required`).
- **Response 204:** empty body.
- **Errors:** 400 `validation`, 401, 403, 404 `not_found` (source missing),
  422 `unprocessable` (either path unsafe, or either is the root).
- **Semantics:** the destination's parent directories are created. Renaming the
  workspace root is rejected with 422. `to` overwriting an existing file follows
  `os.Rename` semantics.

```sh
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"from":"/a.txt","to":"/src/a.txt"}' \
  localhost:8080/v1/workspaces/$WS/files/rename
```

### POST /v1/workspaces/{workspaceID}/files/upload

Upload a file with a multipart form.

- **Path parameter:** `workspaceID`.
- **Request:** `multipart/form-data` with:
  - `path` (string, **required**) — destination path.
  - `file` (binary, **required**) — the file part.
- **Response 201:** `{"path":"<path>","size":<bytes>,"name":"<client filename>"}`.
- **Errors:** 400 `validation` (not multipart, missing `path`, missing `file`),
  401, 403, 413 `payload_too_large` (body > `upload_mb`), 422 `unprocessable`,
  507 `insufficient_storage`.
- **Semantics:** the whole body is capped at `uploads_mb`; the client filename is
  informational only — the server writes to `path`.

```sh
curl -s -H "Authorization: Bearer $KEY" \
  -F path=/uploaded.py -F file=@./uploaded.py \
  localhost:8080/v1/workspaces/$WS/files/upload
# {"name":"uploaded.py","path":"/uploaded.py","size":18}
```

### POST /v1/workspaces/{workspaceID}/exec

Run one command **synchronously** and return its output inline.

- **Path parameter:** `workspaceID`.
- **Request body (`execRequest`):**

  | Field | Go type | JSON | Required | Validation | Default |
  |-------|---------|------|----------|------------|---------|
  | `cmd` | `[]string` | `cmd` | yes | `required,min=1` (argv) | — (400) |
  | `cwd` | `string` | `cwd` | no | resolved against the workspace root; must not escape | workspace root |
  | `env` | `map[string]string` | `env` | no | allowlist applied (below) | none |
  | `timeout_sec` | `int` | `timeout_sec` | no | clamped: `<=0` → 60, `>60` → 60 | 60 |
  | `stdin` | `string` | `stdin` | no | passed to the child's stdin | empty |

- **Response 200:**

  ```json
  {"exit_code":0,"stdout":"hi\n","stderr":"","duration_ms":37,"truncated":false}
  ```
- **Errors:** 400 `validation` (missing `cmd`, bad JSON), 401, 403, 409
  `workspace_suspended` (the workspace is suspended), 422 `unprocessable`
  (`cwd` escapes the root, or `cmd[0]` not found), 503 `unavailable` (nsjail
  missing or sandbox launch failure, with `details.nsjail`).
- **Semantics:**
  - **Wall-clock cap:** synchronous exec has a hard cap of **60 s**, the
    constant `execHardCapSec` in
    [`internal/service/exec.go`](../internal/service/exec.go). A requested
    `timeout_sec` is ignored above it and defaulted to it below. This is *not*
    the same as the workspace `max_wall_time_sec` (default 300), which bounds
    asynchronous runs only.
  - **`cwd` rule:** resolved by `sandboxCwd` in
    [`internal/executor/nsjail.go`](../internal/executor/nsjail.go). Empty ⇒
    `/workspace`. A relative path is resolved against the workspace root; a
    leading `/` is treated as workspace-rooted (it is stripped, **not** treated
    as the host filesystem root); a `..` that climbs above the root is rejected
    with 422. The sandbox sees the workspace at `/workspace`, so pass either
    `"/"`, `""`, or a workspace-relative path such as `"/src"` / `"src"`.
  - **`env` allowlist:** only `PYTHONPATH`, `PYTHONUNBUFFERED`,
    `PYTHONDONTWRITEBYTECODE`, `PYTHONHASHSEED`, `LANG`, `LC_ALL`, `LC_CTYPE`
    and `TZ` pass into the sandbox; every other key (including loader-injection
    names such as `LD_PRELOAD`) is silently dropped.
  - **`cmd[0]`** is resolved on the host `PATH` before nsjail is invoked (nsjail
    does no PATH lookup); an unresolvable command is 422. An absolute path or a
    path containing `/` is used as-is.
  - **Output caps:** stdout/stderr are each capped at `caps.exec_stdout_kb` /
    `exec_stderr_kb` (256 KiB); exceeding sets `truncated:true`. A timeout yields
    `exit_code:124`; a killed process yields `137`.
  - **File run vs command run:** sync `exec` always takes an explicit `cmd`;
    there is no implicit interpreter. To run a Python file, pass
    `["python3","main.py"]`.

```sh
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"cmd":["python3","main.py"],"cwd":"/","timeout_sec":30}' \
  localhost:8080/v1/workspaces/$WS/exec
# {"duration_ms":37,"exit_code":0,"stderr":"","stdout":"hi\n","truncated":false}
```

### Sessions (interactive terminals)

A **session** is a first-class, server-side interactive terminal. Unlike `exec`
(one-shot, 60 s cap) and `runs` (async, no input channel), a session hosts a real
PTY inside the workspace sandbox: a client can send input, read incremental
output, change the window size, send a signal to the running command, and close
it — all over the HTTP API. Session ids are `ses_<ULID>` and are unrelated to
`run_` ids.

Sessions are managed by the service, not re-implemented by each client. The
interactive shell runs under the **same nsjail sandbox** as any other workload:
same uid/gid, same seccomp policy, same mounts, no new privileges, and no host
filesystem access. Network is available only when the workspace sets
`allow_net:true`.

#### How output, input, signals and resize work

- **Output** reuses the run-log mechanism. The PTY relay helper copies the PTY's
  byte stream to its stdout; the service writes that to
  `{data}/workspaces/{ws}/sessions/{ses}/output.log`, capped at
  `sessions.output_kb`. Clients poll it exactly like a run log, with
  `offset`/`limit`, so a client can tail by passing the returned `next` back.
- **Input** is relayed as a line-delimited JSON control message to the helper's
  stdin. The client's `data` is written to the PTY as raw bytes and is **never**
  interpreted as a command: a payload containing `{"type":"close"}` arrives as
  literal text.
- **Signals** are delivered to the session's **foreground process group**, so
  `INT` interrupts the running command (Ctrl-C) **without ending the session**.
  This is the difference from `POST .../runs/{id}/stop`, which kills the whole
  run.
- **Resize** issues `TIOCSWINSZ` on the PTY, so `stty size` inside the shell
  reports the new size.

#### Session statuses

```
running ──┬──► exited     (shell exited on its own; exit_code recorded)
          ├──► killed     (DELETE, workspace delete, suspend, shutdown)
          ├──► timed_out  (idle timeout or absolute lifetime)
          └──► failed     (relay could not start: PTY/sandbox failure)

POST /sessions ──► failed   (the relay died at startup; create reports it)
```

`running, exited, killed, timed_out, failed` are the five values of `status`. A
session also carries a `reason` for its terminal transition (`shell_exited`,
`user_closed`, `idle_timeout`, `lifetime`, `workspace_deleted`,
`workspace_suspended`, `service_shutdown`, `interrupted`,
`sandbox_unavailable`). All terminal states are final.

A **`failed`** session could not start at all: the sandbox could not be launched
or the PTY relay could not allocate a terminal (typically a missing `/dev/ptmx`
/ devpts). It carries a **`diagnostic`** string with the actionable cause and
never leaves a Python traceback in the output stream. `StartSession` waits a
short, bounded window (`sessions.start_probe_ms`) for such an early relay death,
so the **create response itself** reports `failed` rather than a `201 … running`
that is already dead; if the relay dies after that window the record still
transitions to `failed` when the session goroutine reaps it.

#### Session limits and config (`sessions:` in config)

| Key | Default | Meaning |
|-----|---------|---------|
| `sessions.max_per_workspace` | 4 | Concurrent sessions per workspace; over it is `409 too_many_sessions`. |
| `sessions.idle_timeout_sec` | 900 | No activity for this long ends the session (`timed_out` / `idle_timeout`). Activity is any input, signal, resize or output. |
| `sessions.lifetime_sec` | 3600 | Absolute wall-clock ceiling (`timed_out` / `lifetime`), regardless of activity. A create's `timeout_sec` may shorten it, never lengthen it. |
| `sessions.output_kb` | 1024 | Cap on the persisted output log; past it bytes are dropped and `output_truncated` is set. |
| `sessions.max_input_kb` | 64 | Cap on a single input request's `data`. |
| `sessions.shell` | `/bin/sh` | Default shell (an absolute path in the sandbox image). |
| `sessions.kill_grace_sec` | 3 | SIGHUP→SIGKILL grace when a session is closed or times out. |
| `sessions.start_probe_ms` | 250 | How long create waits for a relay that dies at startup, so a failed start is reported as `failed` (not `running`). Capped at 2 s. |

The service starts with none of these set (all have defaults).

### POST /v1/workspaces/{workspaceID}/sessions

Start an interactive terminal session.

- **Path parameter:** `workspaceID`.
- **Request body (`createSessionRequest`):**

  | Field | Go type | JSON | Required | Validation | Default |
  |-------|---------|------|----------|------------|---------|
  | `shell` | `string` | `shell` | no | absolute path available in the image | `sessions.shell` |
  | `cwd` | `string` | `cwd` | no | resolved against the workspace root; must exist and be a directory; must not escape | workspace root |
  | `cols` | `int` | `cols` | no | `1..1000` | 80 |
  | `rows` | `int` | `rows` | no | `1..1000` | 24 |
  | `env` | `map[string]string` | `env` | no | same allowlist as `exec` | none |
  | `timeout_sec` | `int` | `timeout_sec` | no | clamped to `sessions.lifetime_sec` | `sessions.lifetime_sec` |

- **Response 201:**

  ```json
  {"session":{"id":"ses_01J...","workspace_id":"ws_01J...","shell":"/bin/sh",
    "cwd":"/","cols":80,"rows":24,"status":"running",
    "created_at":"2026-01-02T15:04:05Z","last_activity_at":"2026-01-02T15:04:05Z",
    "started_at":"2026-01-02T15:04:05Z","ended_at":"","exit_code":null,
    "output_bytes":0,"output_truncated":false}}
  ```

  If the relay cannot start (a PTY or sandbox failure), the same `201` carries a
  terminal session instead — no traceback in the output:

  ```json
  {"session":{"id":"ses_01J...","status":"failed","reason":"sandbox_unavailable",
    "ended_at":"2026-01-02T15:04:05Z",
    "diagnostic":"sandbox unavailable: PTY allocation failed: sandboxd-pty-error: out of pty devices",
    "output_bytes":0,"output_truncated":false}}
  ```
- **Errors:** 400 `validation`, 401 `unauthorized`, 403 `forbidden`, 404
  `not_found` (workspace), 409 `workspace_suspended`, **409
  `too_many_sessions`** (the per-workspace cap is reached), 413
  `payload_too_large`, 422 `unprocessable` (`cwd` escapes / missing / not a
  directory, `shell` not absolute, `cols`/`rows` out of range), 503
  `unavailable` (nsjail missing or a sandbox launch failure).
- **Semantics:** the shell runs as a session leader on a freshly allocated PTY,
  in the workspace's sandbox. A session is refused while the workspace is
  suspended (`409 workspace_suspended`), exactly like `exec` and `runs`.

```sh
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"shell":"/bin/sh","cols":80,"rows":24}' \
  localhost:8080/v1/workspaces/$WS/sessions
```

### GET /v1/workspaces/{workspaceID}/sessions

List the workspace's sessions, newest first, with the same filter/pagination
shape as the runs list.

- **Path parameter:** `workspaceID`.
- **Query parameters:**
  - `status` (string, repeatable or comma-separated) — one or more of
    `running,exited,killed,timed_out,failed`; an unknown value is `400`.
  - `limit` (int) — max sessions to return; `0`/absent means all remaining.
  - `offset` (int) — sessions to skip (default 0).
- **Response 200:** `{"sessions":[ <session>, ... ], "total": N, "limit": L, "offset": O}`.
  `total` counts matches before pagination.
- **Errors:** 400 `validation`, 401, 403.

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/sessions?status=running"
```

### GET /v1/workspaces/{workspaceID}/sessions/{sessionID}

Fetch one session (the same object returned everywhere).

- **Path parameters:** `workspaceID`, `sessionID` (`ses_<ULID>`).
- **Response 200:** `{"session": <session>}`.
- **Errors:** 401, 403, 404.

### POST /v1/workspaces/{workspaceID}/sessions/{sessionID}/input

Append raw bytes to the session's PTY.

- **Path parameters:** `workspaceID`, `sessionID`.
- **Request body:** `{"data":"<raw string>"}` — `data` is required and non-empty.
- **Response 204:** empty body.
- **Errors:** 400 `validation` (missing `data`), 401, 403, 404, 409
  `workspace_suspended` (workspace check wins), **409 `session_ended`** (the
  session is a terminal tombstone — see below), 413 `payload_too_large` (over
  `sessions.max_input_kb`).
- **Semantics:** the value is relayed to the helper, which writes it to the PTY
  verbatim. Input bytes are never interpreted as helper commands.

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"data":"make test\n"}' \
  localhost:8080/v1/workspaces/$WS/sessions/$SES/input
```

### POST /v1/workspaces/{workspaceID}/sessions/{sessionID}/signal

Deliver a signal to the session's **foreground process group**.

- **Path parameters:** `workspaceID`, `sessionID`.
- **Request body:** `{"signal":"INT"|"TERM"|"QUIT"|"HUP"}`.
- **Response 204:** empty body.
- **Errors:** 400 `validation`, 401, 403, 404, 409 `workspace_suspended`, 409
  `session_ended`, 422 `unprocessable` (a signal outside the allowed set).
- **Semantics:** a signal interrupts the running command **without ending the
  session**, so the same session can run another command afterwards. This is the
  key distinction from `POST .../runs/{id}/stop`, which terminates the run.

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"signal":"INT"}' \
  localhost:8080/v1/workspaces/$WS/sessions/$SES/signal
```

### POST /v1/workspaces/{workspaceID}/sessions/{sessionID}/resize

Change the PTY window size.

- **Path parameters:** `workspaceID`, `sessionID`.
- **Request body:** `{"cols":N,"rows":M}`.
- **Response 204:** empty body.
- **Errors:** 400 `validation`, 401, 403, 404, 409 `workspace_suspended`, 409
  `session_ended`, 422 `unprocessable` (`cols`/`rows` out of range).
- **Semantics:** issues `TIOCSWINSZ` on the PTY, so `stty size` inside the shell
  reports the new dimensions.

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"cols":120,"rows":40}' \
  localhost:8080/v1/workspaces/$WS/sessions/$SES/resize
```

### GET /v1/workspaces/{workspaceID}/sessions/{sessionID}/output

Poll the session's PTY output by byte offset. Same semantics as the run-log
endpoint.

- **Path parameters:** `workspaceID`, `sessionID`.
- **Query parameters:**
  - `offset` (int64) — start byte offset; default and negative → 0; beyond the
    current size → clamped to the size.
  - `limit` (int64) — max bytes to return; default and `<=0` → 65536.
- **Response 200:**

  ```json
  {"data":"$ ","offset":0,"next":2,"eof":true,"truncated":false}
  ```

  `truncated` mirrors the session's `output_truncated` (the write cap was hit);
  for a long-running session it stays `false` until the output cap is reached.
- **Errors:** 400 `validation`, 401, 403, 404, **409 `session_ended`** (session
  ended by `DELETE`), 409 `workspace_suspended`.
- **Tailing recipe (identical to run logs):**
  1. `GET` with `offset=0`; write `data`; set `offset = next`.
  2. While `eof` is `false`, repeat step 1.
  3. If the session is still `running`, sleep briefly and repeat from step 1 (a
     live session may append between polls, flipping `eof` back to `false`).
  4. Stop when the session is terminal and a final poll returns `eof:true`.

```sh
OFFSET=0
while :; do
  CHUNK=$(curl -s -H "Authorization: Bearer $KEY" \
    "localhost:8080/v1/workspaces/$WS/sessions/$SES/output?offset=$OFFSET&limit=65536")
  printf '%s' "$CHUNK" | python3 -c 'import json,sys;d=json.load(sys.stdin);sys.stdout.write(d["data"])'
  OFFSET=$(printf '%s' "$CHUNK" | python3 -c 'import json,sys;print(json.load(sys.stdin)["next"])')
  EOF=$(printf '%s' "$CHUNK" | python3 -c 'import json,sys;print(json.load(sys.stdin)["eof"])')
  STATUS=$(curl -s -H "Authorization: Bearer $KEY" \
    "localhost:8080/v1/workspaces/$WS/sessions/$SES" | python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["status"])')
  [ "$EOF" = "True" ] && [ "$STATUS" != "running" ] && break
  sleep 1
done
```

### DELETE /v1/workspaces/{workspaceID}/sessions/{sessionID}

End a session gracefully: SIGHUP the shell, then SIGKILL its process group after
`sessions.kill_grace_sec`.

- **Path parameters:** `workspaceID`, `sessionID`.
- **Response 200:**

  ```json
  {"session": <session with status "killed" and reason "user_closed">, "changed": true}
  ```

  `changed` is `true` when this call ended a live session and `false` on a
  repeat (the session was already terminal).
- **Errors:** 401, 403, 404 `not_found` (a session id that never existed).
- **Semantics:** idempotent. The terminal record and its output log are
  **retained** (so the session stays inspectable), and later `input`/`output`
  calls report **409 `session_ended`** rather than `not_found`. The record is
  removed with the workspace. **Ended-session rule:** input, signal, resize and
  output on a session ended by `DELETE` return `409 session_ended`; on a session
  that ended on its own (shell exit, idle/lifetime timeout) `output` stays
  readable while `input`/`signal`/`resize` return `409 session_ended`.

```sh
curl -s -X DELETE -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/sessions/$SES
# {"changed":true,"session":{"id":"ses_...","status":"killed","reason":"user_closed",...}}
```

#### Worked session example (create → input → output → signal → resize → delete)

```sh
KEY=dev-key-please-rotate
AUTH="Authorization: Bearer $KEY"
JSON="Content-Type: application/json"
BASE=http://127.0.0.1:8080

WS=$(curl -s -H "$AUTH" -H "$JSON" -d '{"name":"terminal"}' \
      $BASE/v1/workspaces | python3 -c 'import json,sys;print(json.load(sys.stdin)["workspace"]["id"])')

# 1. create
SES=$(curl -s -H "$AUTH" -H "$JSON" -d '{"cols":80,"rows":24}' \
      $BASE/v1/workspaces/$WS/sessions \
      | python3 -c 'import json,sys;print(json.load(sys.stdin)["session"]["id"])')

# 2. run a command through the PTY
curl -s -o /dev/null -H "$AUTH" -H "$JSON" \
  -d '{"data":"test -t 1 && echo REAL_TTY\n"}' \
  $BASE/v1/workspaces/$WS/sessions/$SES/input

# 3. read the output incrementally
OFFSET=0
while :; do
  C=$(curl -s -H "$AUTH" "$BASE/v1/workspaces/$WS/sessions/$SES/output?offset=$OFFSET")
  printf '%s' "$C" | python3 -c 'import json,sys;sys.stdout.write(json.load(sys.stdin)["data"])'
  OFFSET=$(printf '%s' "$C" | python3 -c 'import json,sys;print(json.load(sys.stdin)["next"])')
  [ "$(printf '%s' "$C" | python3 -c 'import json,sys;print(json.load(sys.stdin)["eof"])')" = "True" ] && break
done
# $ test -t 1 && echo REAL_TTY
# REAL_TTY

# 4. interrupt a long command (session survives)
curl -s -o /dev/null -H "$AUTH" -H "$JSON" -d '{"data":"sleep 30\n"}' \
  $BASE/v1/workspaces/$WS/sessions/$SES/input
curl -s -o /dev/null -H "$AUTH" -H "$JSON" -d '{"signal":"INT"}' \
  $BASE/v1/workspaces/$WS/sessions/$SES/signal

# 5. resize
curl -s -o /dev/null -H "$AUTH" -H "$JSON" -d '{"cols":120,"rows":40}' \
  $BASE/v1/workspaces/$WS/sessions/$SES/resize

# 6. close
curl -s -X DELETE -H "$AUTH" $BASE/v1/workspaces/$WS/sessions/$SES
# {"changed":true,"session":{"status":"killed","reason":"user_closed",...}}

# after DELETE, input reports the documented ended-session error
curl -s -H "$AUTH" -H "$JSON" -d '{"data":"x"}' \
  $BASE/v1/workspaces/$WS/sessions/$SES/input
# {"error":{"code":"session_ended","message":"session has ended","details":{"status":"killed"}}}
```

### POST /v1/workspaces/{workspaceID}/runs

Start an asynchronous run. Returns immediately; poll it via the run endpoints.

- **Path parameter:** `workspaceID`.
- **Request body (`createRunRequest`):**

  | Field | Go type | JSON | Required | Validation | Default |
  |-------|---------|------|----------|------------|---------|
  | `kind` | `string` | `kind` | yes | `required,oneof=file command` | — (400) |
  | `file` | `string` | `file` | for `kind=file` | must resolve inside the root and exist; its extension must map to an allowlisted interpreter (or `interpreter` must be given) | — |
  | `args` | `[]string` | `args` | no | appended after the file for `kind=file` | — |
  | `argv` | `[]string` | `argv` | for `kind=command` | non-empty; **must not** be combined with `kind=file` (422) | — |
  | `cwd` | `string` | `cwd` | no | working directory relative to the workspace root; must exist and be a directory; must not escape | workspace root |
  | `interpreter` | `string` | `interpreter` | no | `kind=file` only; must be in the allowlist (`interpreters` from `/v1/meta`) | extension-derived |
  | `env` | `map[string]string` | `env` | no | same allowlist as `exec` | none |
  | `timeout_sec` | `int` | `timeout_sec` | no | `<=0` or `>` workspace `max_wall_time_sec` → clamped to it; ignored when `no_timeout:true` | workspace `max_wall_time_sec` |
  | `no_timeout` | `bool` | `no_timeout` | no | requests a wall-clock-**unbounded** run; permitted only when both `max_limits.allow_unlimited_runs` and the workspace `limits.allow_unlimited_runs` are `true`, else 422 | `false` |

- **Response 202:**

  ```json
  {"run":{"id":"run_01J...","workspace_id":"ws_01J...","kind":"file",
    "argv":["python3","/workspace/main.py"],"status":"queued","exit_code":null,
    "cwd":"/","timeout_sec":300,"unlimited":false,"interpreter":"python3",
    "started_at":"2026-01-02T15:04:05Z","ended_at":"",
    "logs":{"stdout_bytes":0,"stderr_bytes":0,"truncated":false}}}
  ```

  `cwd` is always present (leading-slash normalized; `/` is the workspace root).
  `timeout_sec` is `null` **exactly when** `unlimited` is `true` (an unbounded
  run is never recorded with a fake finite number, so a reader can tell "no
  limit" from "an hour"). `interpreter` is present for a `kind=file` run and
  omitted otherwise.
- **Errors:** 400 `validation` (bad `kind`, missing `file`/`argv`, bad JSON),
  401, 403, 404 `not_found` (`kind=file` target missing — or the workspace
  missing), 409 `workspace_suspended`, 422 `unprocessable` (`kind=file` target
  is a directory, the path escapes the root, an unsupported extension with no
  `interpreter`, an `interpreter` outside the allowlist, `argv` combined with
  `kind=file`, a `cwd` that escapes / is missing / is not a directory, or a
  `no_timeout` request the server or workspace forbids).
- **Semantics:**
  - **`kind=file` selects an interpreter.** The interpreter is taken from the
    explicit `interpreter` field when present, otherwise from the file
    extension via the allowlist (`.py`→`python3`, `.js`/`.mjs`/`.cjs`→`node`,
    `.sh`/`.bash`→`bash`, `.pl`/`.pm`→`perl`). An unsupported extension with no
    override is `422` naming the supported set. The built argv is
    `[<interpreter>, /workspace/<file>, ...args]`: the file is passed as an
    **absolute sandbox path** so it is found regardless of `cwd`.
  - **`cwd` is a working directory, not a relative-path prefix** for `file`. It
    is resolved relative to the workspace root and validated to exist and be a
    directory at request time; a bad `cwd` is `422`, never a deferred 503. It
    applies to `kind=file` and `kind=command` alike.
  - **`argv` vs `kind=file`+`args` (explicit precedence):** a `kind=file` run
    builds its argv from the interpreter, the file and `args`; supplying `argv`
    as well is **rejected with 422** rather than silently preferred, so there is
    no ambiguous precedence. `kind=command` executes `argv` verbatim and ignores
    `file`/`args`.
  - **Unbounded runs.** `no_timeout:true` requests a run with **no wall-clock
    limit**: it runs until it exits or is stopped. It is distinct from
    `timeout_sec: 0`, which means "use the workspace default". It is refused
    with `422` unless **both** `max_limits.allow_unlimited_runs` (server) and
    the workspace's `limits.allow_unlimited_runs` are true; `details.config`
    names which level refused. The default is **deny**. An unbounded run still
    consumes a slot of `limits.max_runs` and its cgroup share — CPU, memory and
    pids ceilings still apply — so a runaway unlimited process can occupy the
    workspace's capacity indefinitely until someone stops it (see
    [DECISIONS.md](DECISIONS.md) #60 and
    [Unbounded runs](#unbounded-runs)).
  - `kind=command` executes `argv` verbatim.
  - Old runs are pruned before each new run: the newest **100** runs per
    workspace are retained (`storage.Retention`); the oldest are deleted.
  - Each run's stdout/stderr are streamed to per-run log files, capped at
    `caps.run_log_mb` per stream.

```sh
# a Python file in a subdirectory, run there
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"file","file":"/src/main.py","cwd":"/src"}' \
  localhost:8080/v1/workspaces/$WS/runs

# a shell file, interpreter chosen by extension
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"file","file":"/build.sh"}' \
  localhost:8080/v1/workspaces/$WS/runs

# a non-.py file with an explicit allowlisted interpreter
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"file","file":"/tool.txt","interpreter":"bash"}' \
  localhost:8080/v1/workspaces/$WS/runs

# an unbounded run (needs the server and workspace opt-in)
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"file","file":"/server.py","no_timeout":true}' \
  localhost:8080/v1/workspaces/$WS/runs

curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"command","argv":["python3","-c","print(1+1)"]}' \
  localhost:8080/v1/workspaces/$WS/runs
```

### GET /v1/workspaces/{workspaceID}/runs

List the workspace's runs, newest first, with filtering and pagination.

- **Path parameter:** `workspaceID`.
- **Query parameters:**
  - `status` (string, repeatable or comma-separated) — keep only runs in any of
    the given statuses (`queued,running,exited,killed,timeout,failed,interrupted`).
    An unknown status is `400`. Both `?status=exited,killed` and
    `?status=exited&status=killed` are accepted.
  - `kind` (`file` or `command`) — keep only runs of that kind.
  - `limit` (int) — maximum runs to return; `0`/absent means "all remaining".
  - `offset` (int) — number of matching runs to skip before returning (default 0).
- **Response 200:** `{"runs":[ <run>, ... ], "total": N, "limit": L, "offset": O}`.
  `total` is the number of runs matching the filter **before** pagination, so a
  client can page deterministically. `runs` is ordered newest first.
- **Errors:** 400 `validation` (unknown `status`/`kind`), 401, 403.
- **Semantics:** empty is `{"runs":[],"total":0}`. Pagination is applied after
  filtering; the ordering is stable (by start time, newest first).

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/runs?status=exited,killed&limit=20&offset=0"
```

### DELETE /v1/workspaces/{workspaceID}/runs

Purge the workspace's finished run history (killed/timed-out/failed/exited/
interrupted runs).

- **Path parameter:** `workspaceID`.
- **Response 200:** `{"deleted": N}` — the number of run records removed.
- **Errors:** 401, 403, **409 `conflict`** while any run is still
  `queued`/`running` (`details.run_id` names one such run), 404 (unknown
  workspace).
- **Semantics:** only finished runs are removed, and the whole purge **refuses**
  (nothing is deleted) if any run is still queued/running, so an in-flight run
  is never lost. Logs and metadata for the removed runs are deleted with them.

```sh
curl -s -X DELETE -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/runs
# {"deleted":3}
```

### GET /v1/workspaces/{workspaceID}/runs/{runID}

Fetch one run.

- **Path parameters:** `workspaceID`, `runID` (`run_<ULID>`).
- **Response 200:** `{"run": <run>}`.
- **Errors:** 401, 403, 404 `not_found`.
- **Semantics:** the `run` object is the same shape returned everywhere. While a
  run is `queued`/`running`, `exit_code` is `null` and `ended_at` is `""`; on a
  terminal transition `ended_at` is set. A run that ended `failed` also has
  `exit_code:null` (no exit code is recorded for executor errors).

```sh
RUN=run_REPLACE_ME
curl -s -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/runs/$RUN
```

### DELETE /v1/workspaces/{workspaceID}/runs/{runID}

Delete one finished run record (and its logs).

- **Path parameters:** `workspaceID`, `runID`.
- **Response 204:** empty body.
- **Errors:** 401, 403, 404 `not_found`, **409 `conflict`** if the run is still
  `queued`/`running`.
- **Semantics:** removes the run directory (`meta.json`, `stdout.log`,
  `stderr.log`). Refuses to delete a run that is still running.

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE \
  -H "Authorization: Bearer $KEY" localhost:8080/v1/workspaces/$WS/runs/$RUN
# 204
```

### POST /v1/workspaces/{workspaceID}/runs/{runID}/stop

Stop a run. Idempotent.

- **Path parameters:** `workspaceID`, `runID`.
- **Response 202:** `{"run": <run>}` with the resulting status.
- **Errors:** 401, 403, 404 `not_found` (unknown run).
- **Semantics:** if a live process was signalled, the status is `killed`; the
  stop cancels the run's context and sends `SIGTERM` to the process group, waits
  `sandbox.kill_grace_sec` (default 3 s), then `SIGKILL`. If the run had already
  finished (or an unknown-but-recorded run), the return carries its stored status
  and the call is a no-op. Calling stop on an unknown run id is 404.

```sh
curl -s -X POST -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/runs/$RUN/stop
```

### GET /v1/workspaces/{workspaceID}/runs/{runID}/logs

Poll a run log stream by byte offset.

- **Path parameters:** `workspaceID`, `runID`.
- **Query parameters:**
  - `stream` (string) — `stdout` (default) or `stderr`; anything else is 400.
  - `offset` (int64) — start byte offset; default and negative → 0; beyond the
    current size → clamped to the size.
  - `limit` (int64) — max bytes to return; default and `<=0` → 65536.
- **Response 200:**

  ```json
  {"data":"hi\n","offset":0,"next":3,"eof":true,"truncated":false}
  ```

  - `data` — the bytes from `[offset, offset+n)` decoded as a string.
  - `offset` — the effective start offset (after clamping).
  - `next` — `offset + len(data)`, the offset to pass on the next poll.
  - `eof` — true when `next` has reached the current end of the file.
  - `truncated` — currently **always false**; per-run log truncation is reported
    on the run object's `logs.truncated`, not here.
- **Errors:** 400 `validation` (bad `stream`), 401, 403, 404 `not_found`.
- **Streaming incrementally:** call with `offset=0`; while `eof` is false, keep
  calling with `offset=<next>` until `eof` is true. A live run may append more
  after `eof` was true, so a poller that wants the whole log should re-poll until
  the run reaches a terminal state and a final `eof:true` is observed.
- **Exact tailing recipe (poll with the returned offset):**
  1. Poll the run until `status` is terminal (`exited`, `killed`, `timeout`,
     `failed`, `interrupted`).
  2. Read `offset=0`; print `data`, set `offset = next`.
  3. While `eof` is `false`, repeat step 2.
  4. If the run is still not terminal, sleep briefly and repeat from step 2
     (a live run may append between polls, flipping `eof` back to `false`).

  Worked example against a running `long.py` that prints a line per second:

  ```sh
  RUN=run_REPLACE_ME
  OFFSET=0
  while :; do
    CHUNK=$(curl -s -H "Authorization: Bearer $KEY" \
      "localhost:8080/v1/workspaces/$WS/runs/$RUN/logs?stream=stdout&offset=$OFFSET&limit=65536")
    # {"data":"tick 0\ntick 1\n","offset":0,"next":14,"eof":false,"truncated":false}
    printf '%s' "$CHUNK" | python3 -c 'import json,sys; d=json.load(sys.stdin); import sys as s; s.stdout.write(d["data"])'
    OFFSET=$(printf '%s' "$CHUNK" | python3 -c 'import json,sys;print(json.load(sys.stdin)["next"])')
    EOF=$(printf '%s' "$CHUNK" | python3 -c 'import json,sys;print(json.load(sys.stdin)["eof"])')
    STATUS=$(curl -s -H "Authorization: Bearer $KEY" \
      "localhost:8080/v1/workspaces/$WS/runs/$RUN" | python3 -c 'import json,sys;print(json.load(sys.stdin)["run"]["status"])')
    [ "$EOF" = "True" ] && case "$STATUS" in exited|killed|timeout|failed|interrupted) break;; esac
    sleep 1
  done
  ```

  The first chunk above shows `offset:0` → `next:14`, so the next poll passes
  `offset=14`; once the run has ended and a final poll returns `eof:true` with
  `next` equal to the file size, the log is complete.

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/runs/$RUN/logs?stream=stdout&offset=0&limit=65536"
```

## File execution and interpreters

A `kind:file` run no longer assumes Python. The interpreter is chosen by the
file's extension from an **allowlist**, or overridden by an explicit
`interpreter` field that must also be in the allowlist. The allowlist is exactly
what `interpreters`/`runnable_extensions` report on `GET /v1/meta`, and each
entry was measured executable **inside the real sandbox** before being listed
(see [VERIFICATION.md §9e](VERIFICATION.md)).

| Interpreter | Extensions |
|-------------|------------|
| `python3` | `.py` |
| `node` | `.js`, `.mjs`, `.cjs` |
| `bash` | `.sh`, `.bash` |
| `perl` | `.pl`, `.pm` |

- **Backward compatible:** `{"kind":"file","file":"/x.py"}` still runs `python3`
  with no `interpreter` field.
- **Explicit override:** `{"kind":"file","file":"/tool.txt","interpreter":"bash"}`
  runs a file whose extension is not registered. An interpreter outside the
  allowlist is `422` (never a free-form path — this field is not an
  arbitrary-execution primitive).
- **Unsupported extension, no override:** `422` naming the supported set
  (`details.supported_extensions`).
- **File path is absolute in the sandbox:** the built argv is
  `[<interpreter>, /workspace/<file>, ...args]`, so the file is found regardless
  of `cwd`.
- **Discoverability:** every file `stat`/listing entry carries
  `runnable: "<interpreter>"` when the extension maps to one, and omits it
  otherwise. A UI can offer "run this file" on exactly the files that support
  it, without guessing. The field is on both the listing and `stat` entry; a UI
  browsing a directory gets it for free, and `stat` gives it for a single known
  path.

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files/stat?path=/main.py" | \
  python3 -c 'import json,sys;print(json.load(sys.stdin)["runnable"])'
# python3
```

## Unbounded runs

A run may be requested with **no wall-clock limit**: it runs until it exits by
itself **or** the caller stops it.

- **Request it explicitly with `no_timeout: true`.** This is deliberately
  *distinct* from `timeout_sec: 0`, which means "use the workspace default", so
  a client cannot ask for unlimited by accident or by omission.
- **Gated twice, default deny.** An unbounded run is accepted only when **both**
  the server (`max_limits.allow_unlimited_runs`) and the workspace
  (`limits.allow_unlimited_runs`) permit it. Otherwise `422`, with
  `details.config` naming which level refused
  (`max_limits.allow_unlimited_runs` or `limits.allow_unlimited_runs`).
- **The record tells the truth.** An unbounded run reports `unlimited: true` and
  `timeout_sec: null`. It is **never** stored with a fake finite number like
  `3600`, so a reader can always tell "no limit" apart from "an hour".
- **Stopping works like any run.** `POST .../runs/{id}/stop` sends SIGTERM to
  the process group, waits `sandbox.kill_grace_sec`, then SIGKILL; the run ends
  `killed` and **zero** processes survive (verified in the acceptance suite past
  a deliberately small `max_wall_time_sec`).
- **The honest risk.** An unbounded run still consumes a slot of
  `limits.max_runs` and its cgroup share — CPU, memory and pids ceilings still
  apply — so a runaway unlimited process can occupy the workspace's capacity
  **indefinitely** until someone stops it. Unbounded means unbounded in time
  only, never in resources.

```sh
# enable it (operator): max_limits.allow_unlimited_runs: true
# opt a workspace in:
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"svc","limits":{"allow_unlimited_runs":true}}' \
  localhost:8080/v1/workspaces

# run without a time limit
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"file","file":"/server.py","no_timeout":true}' \
  localhost:8080/v1/workspaces/$WS/runs
# {"run":{...,"unlimited":true,"timeout_sec":null,...}}
```

## Quick tunnels and egress

The operator's core need: a workspace has **normal outbound internet** (so
`pip install` and `cloudflared` work inside it), and one command exposes one of
its ports **to the public internet**.

### Egress (`allow_net`) without host-network sharing

An `allow_net:true` workspace gets **its own network namespace**, connected to
the public internet only through **chained slirp4netns** instances:

```
workspace ns --slirp(UNIX socket)--> relay ns --slirp(UNIX socket)--> host
```

- The engine-owned **relay** namespace is connected to the host by one
  slirp4netns instance; each workspace is connected to the relay by a second.
- slirp4netns speaks to its parent over a **UNIX socket** and wires up a tap
  interface only inside the child namespace, so this creates **zero host
  interfaces, zero host routes and zero veth pairs**. The host's own topology is
  byte-for-byte unchanged.
- The relay installs a **blackhole route for every operator-private range**
  (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `169.254.0.0/16`,
  `100.64.0.0/10`), so a workspace reaches the **public internet and a public
  resolver** but **never the host LAN, the host's own services, or another
  workspace**.
- The workspace's `/etc/resolv.conf` names **public resolvers**
  (`network.resolver`, default `1.1.1.1`/`8.8.8.8`). The host's own resolver is
  deliberately never published.
- `allow_net:false` gets a private, **empty** namespace: no egress at all, and
  never the host network. Both states are measured by
  [`scripts/tunnel-check.sh`](../scripts/tunnel-check.sh).

> **Recorded hazard.** The abandoned first design created host `veth` pairs,
  addresses and routes, and took the target host off the network three times in
  one evening. It is deleted from this tree; see
  [DECISIONS.md](DECISIONS.md) (the host-topology ADR). No code path adds,
  removes or renames a host interface, address or route.

### Quick tunnels (one command → public URL)

`POST .../tunnels {port:5000}` starts `cloudflared tunnel --url
http://localhost:5000` **inside the workspace's namespace** and returns the
`https://<name>.trycloudflare.com` URL cloudflared prints. Quick tunnels need no
Cloudflare account and no token. The same binary is on `PATH` inside the sandbox,
so the user can also run `cloudflared tunnel --url http://localhost:5000`
themselves and get a working URL.

- **Gated twice, default deny.** `tunnels.enabled` (server) **and**
  `limits.allow_tunnels` (workspace) must both be true, **and** the workspace
  must have `allow_net` (cloudflared needs egress). Otherwise `422
  tunnels_disabled`, with `details.config` naming the refusing level.
- **Never a fabricated URL.** The URL is captured from cloudflared's own output
  with a bounded wait (`tunnels.url_timeout_sec`); if none is observed the
  create fails with **504 `tunnel_url_unavailable`** and leaves no record.
- **Unauthenticated and rate limited.** Quick tunnels use Cloudflare's
  account-less path, which is **shared and rate limited**: creating several in a
  short window can make provisioning return `429`, surfaced as the same `504
  tunnel_url_unavailable` (with cloudflared's own text in `details.reason`). This
  is a limit of the external service, not a bug — a deployment that expects many
  users creating tunnels must account for it; an account-scoped **named tunnel**
  is the eventual answer and is **out of scope** here. See
  [DECISIONS.md](DECISIONS.md) #70.
- **The URL is ephemeral.** A quick-tunnel URL changes on every start, so nothing
  (client, cache or UI) may store or rely on it; the only valid URL is the one
  just observed from cloudflared's output. `scripts/tunnel-check.sh` reports the
  tunnel-dependent assertions as **skipped-environment** (not failed) when the
  quota is spent, so a green run is never faked and a quota outage never trains
  people to ignore the suite.
- **Tunnels die with their workspace.** `stop`, `suspend` and `DELETE` stop the
  cloudflared process and drop the records; a restart reaps orphaned cloudflared
  processes.

```sh
# enable (operator): network.enabled: true and tunnels.enabled: true
# opt in and expose a port:
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"web","limits":{"allow_net":true,"allow_tunnels":true}}' \
  localhost:8080/v1/workspaces

# run the app, then expose it
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"command","argv":["python3","-m","http.server","5000","--bind","0.0.0.0"],"no_timeout":true}' \
  localhost:8080/v1/workspaces/$WS/runs
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"port":5000}' localhost:8080/v1/workspaces/$WS/tunnels
# {"tunnel":{"url":"https://....trycloudflare.com", ...}}
# fetch that URL from anywhere; DELETE the tunnel to stop it.
```

### Configuration

| Block | Key | Default | Meaning |
|-------|-----|---------|---------|
| `network:` | `enabled` | `false` | server switch for the egress fabric |
| `network:` | `netns_dir` | `/run/netns` | where the engine's namespaces are bind-mounted |
| `network:` | `relay_netns` | `sbxdrelay` | name of the shared relay namespace |
| `network:` | `slirp_path` | `slirp4netns` | the slirp4netns binary |
| `network:` | `mtu` | `65520` | passed to each slirp instance |
| `network:` | `resolver` | `["1.1.1.1","8.8.8.8"]` | public resolvers for `/etc/resolv.conf` |
| `network:` | `start_timeout_ms` | `5000` | bound for a slirp interface to come up |
| `tunnels:` | `enabled` | `false` | server switch for quick tunnels |
| `tunnels:` | `max_per_workspace` | `2` | concurrent tunnels per workspace |
| `tunnels:` | `url_timeout_sec` | `45` | bounded wait for the URL |
| `tunnels:` | `cloudflared_path` | `cloudflared` | the cloudflared binary |
| `identity:` | `enabled` | `true` | host-identity masking switch (opt-**out**, not opt-in) |
| `identity:` | `kernel_release` | `0.0.0-sandbox` | the generic `uname.release` a workspace is told |
| `identity:` | `kernel_version` | `#1 SMP` | the generic `uname.version` a workspace is told |
| `identity:` | `shim_path` | `""` | optional override of the compiled shim; empty uses the embedded copy |

## Lifecycle and limits

### Workspace lifecycle

A workspace has a `status` of `active` or `suspended`:

```
create ──► active ──suspend──► suspended
              ▲                   │
              └──── resume ───────┘
                (stop leaves active)
```

- **Create** — `POST /v1/workspaces`. The caller may override, at creation time,
  `limits.cpu_cores`, `memory_mb`, `disk_mb`, `max_wall_time_sec`, `max_runs`
  (positive values only, each `<=` its server maximum) and `allow_net`.
  Everything else — the id (`ws_<ULID>`), `owner_id` (from the key),
  `created_at`, `status` — is fixed by the server. Limits come from the
  `limits:` config block when omitted.
- **Inspect** — `GET /v1/workspaces/{workspaceID}`.
- **List** — `GET /v1/workspaces` (caller's own, newest first).
- **Update** — `PATCH /v1/workspaces/{workspaceID}` changes `name` and/or a
  subset of `limits` at runtime; other fields are preserved.
- **Suspend / resume / stop** — `POST .../suspend`, `.../resume`, `.../stop`. All
  idempotent. `suspend` refuses new work (409 `workspace_suspended`) and stops
  running runs; `resume` accepts work again; `stop` kills running runs but leaves
  the workspace `active`.
- **Usage** — `GET /v1/workspaces/{workspaceID}/usage`.
- **Delete** — `DELETE /v1/workspaces/{workspaceID}`: stops and kills all runs,
  then removes the whole `{data_dir}/workspaces/{id}/` tree.

Both the status and the limits are persisted and survive a restart.

### Run lifecycle

Runs move through these states (the vocabulary in `internal/model/types.go`):

```
queued ──► running ──┬──► exited       (process returned; exit_code recorded)
                     ├──► timeout      (wall-clock limit; exit_code 124)
                     ├──► killed       (stop / workspace delete; exit_code 137)
                     └──► failed       (executor error; exit_code null)
queued | running ────────► interrupted  (recovered after a restart)
```

- `queued` — record created, goroutine not yet started. `GET` may briefly show
  this.
- `running` — the sandbox process is live; `started_at` is refreshed.
- `exited` — finished normally; `exit_code` is the process's code. Distinguish
  **success** by `status=="exited" && exit_code==0`.
- `timeout` — the wall-clock limit elapsed (async limit is the workspace
  `max_wall_time_sec`); `exit_code` is 124.
- `killed` — an explicit stop or a workspace delete cancelled it (`exit_code`
  137). This is "terminated on request", distinct from a crash.
- `failed` — the executor itself returned an error (e.g. nsjail missing or a
  launch failure); `exit_code` is null and the diagnostic is not exposed on the
  run object.
- `interrupted` — the process did not survive a service restart; startup
  recovery moves any still-`queued`/`running` run to this terminal state.
- All terminal states are final. A stop only changes a live run to `killed`.

### Resource limits

Per-workspace limits (`limits:` config; overridable at creation and via
`PATCH`):

| Limit | Config key | Default | Server maximum (`max_limits:`) | Effect |
|-------|------------|---------|-------------------------------|--------|
| CPU cores | `limits.cpu_cores` | 1 | 8 | cgroup CPU throttling (`cgroup_cpu_ms_per_sec`) |
| Memory | `limits.memory_mb` | 512 | 8192 | `RLIMIT_AS` and `cgroup_mem_max` (MB / bytes respectively) |
| Disk | `limits.disk_mb` | 1024 | 51200 | file-API quota; `RLIMIT_FSIZE` per file |
| Wall time | `limits.max_wall_time_sec` | 300 | 3600 | async run timeout (`--time_limit`) |
| Runs / procs | `limits.max_runs` | 3 | 64 | cgroup pids max = `max_runs * 64`; also `RLIMIT_NPROC` |
| Network | `limits.allow_net` | false | true | private namespace connected to the internet via chained slirp4netns (never the host network) |
| Unbounded runs | `limits.allow_unlimited_runs` | false | false | workspace opt-in for `no_timeout:true`; **both** levels must be true |
| Tunnels | `limits.allow_tunnels` | false | server switch `tunnels.enabled` | workspace opt-in for quick tunnels; **both** this and `tunnels.enabled` must be true, and `allow_net` must also be true |

The server maximum is a hard ceiling: a create or `PATCH` value above it is
rejected with `422 unprocessable` (never silently clamped). `max_limits:` is
loaded from config and always at least the defaults. `allow_unlimited_runs` and
`allow_tunnels` are the two non-numeric ceilings: each is a switch, and the
workspace value can only be `true` when the server switch is `true` (see
[Unbounded runs](#unbounded-runs) and [Quick tunnels](#quick-tunnels-and-egress)).

Process-level hard caps (`caps:` config; **not** overridable per request):
`exec_stdout_kb` 256, `exec_stderr_kb` 256, `run_log_mb` 10, `json_body_mb` 1,
`upload_mb` 50, `file_read_mb` 5, `file_write_mb` 10.

Additional bounds: `sandbox.kill_grace_sec` (default 3) is the SIGTERM→SIGKILL
grace; run retention is 100 runs per workspace. Disk accounting is done on the
file API hot path and reconciled with a `du` walk on each workspace load, so
out-of-band changes cannot drift the quota.

### Sandbox semantics the caller can observe

- **UID/GID:** the workload runs as the configured `sandbox.user`/`sandbox.group`
  (fallback `nobody`, uid/gid 65534). When sandboxd runs as root it maps
  `inside sandbox ↔ outside sandbox` and chowns the workspace tree to that
  identity; when it runs unprivileged it maps `inside sandbox ↔ outside
  orchestrator` and leaves ownership alone.
- **Workspace mount:** the workspace directory is bind-mounted read-write at
  **`/workspace`**, which is the sandbox CWD. The host path is
  `{data_dir}/workspaces/{ws_id}/files`.
- **Private `/tmp`:** each run gets a fresh, private, writable `/tmp`; it is
  never the host `/tmp`.
- **Read-only host bind mounts:** `/usr`, `/lib`, `/lib64`, `/bin`, `/sbin`, and
  `/etc/{alternatives,ssl/certs,ld.so.cache,nsswitch.conf,passwd,group}`, plus
  `/dev/{urandom,null,zero}`. `/etc/shadow`, the host `/proc`, and other tenants'
  data are not mounted.
- **Network:** a private network namespace always. With `allow_net:false` it is
  empty (no egress). With `allow_net:true` it is connected to the public internet
  through chained slirp4netns over UNIX sockets — the host network namespace is
  never shared, and the host LAN is blackholed. There is no host allowlist in
  v0.0.16 (see [Quick tunnels and egress](#quick-tunnels-and-egress)).
- **Syscalls:** a default-allow seccomp policy KILLs a denylist of dangerous
  syscalls (`mount`, `ptrace`, `bpf`, `kexec_*`, `reboot`, `setns`, `unshare`,
  module loading, …). `cgroup v2` ceilings require the container to run with
  `--cgroupns=host`; without it nsjail aborts and the call returns
  `503 unavailable` (never a silent exit).

## Worked end-to-end example

A complete, copy-pasteable transcript. It assumes a locally running instance
with the example config (key `dev-key-please-rotate`) and the Python runtime
available to the sandbox. `BASE` and `KEY` are the only variables you set.

```sh
BASE=http://127.0.0.1:8080
KEY=dev-key-please-rotate
AUTH="Authorization: Bearer $KEY"
JSON="Content-Type: application/json"
```

### 1. Check the service and create a workspace

```sh
curl -s $BASE/healthz
# {"status":"ok"}

curl -s $BASE/readyz
# {"cgroups":true,"data_dir":true,"nsjail":true,"status":"ready"}

curl -s -H "$AUTH" $BASE/v1/meta
# {"version":"0.0.16","api_version":"v1", ... }

WS=$(curl -s -H "$AUTH" -H "$JSON" \
  -d '{"name":"demo","limits":{"memory_mb":256}}' \
  $BASE/v1/workspaces | python3 -c 'import json,sys;print(json.load(sys.stdin)["workspace"]["id"])')
echo "$WS"
```

### 2. Write a file and run it synchronously

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X PUT -H "$AUTH" -H "$JSON" \
  -d '{"content":"print(\"hello from sandboxd\")\n"}' \
  "$BASE/v1/workspaces/$WS/files/content?path=/main.py"
# 204

curl -s -H "$AUTH" $BASE/v1/workspaces/$WS/files?path=/
curl -s -H "$AUTH" -H "$JSON" \
  -d '{"cmd":["python3","main.py"],"cwd":"/","timeout_sec":30}' \
  $BASE/v1/workspaces/$WS/exec
# {"duration_ms":37,"exit_code":0,"stderr":"","stdout":"hello from sandboxd\n","truncated":false}
```

### 3. Start a long run asynchronously and poll it

```sh
curl -s -o /dev/null -X PUT -H "$AUTH" -H "$JSON" \
  -d '{"content":"import time,sys\nfor i in range(30):\n    print(\"tick\",i,flush=True)\n    time.sleep(1)\n"}' \
  "$BASE/v1/workspaces/$WS/files/content?path=/long.py"

RUN=$(curl -s -H "$AUTH" -H "$JSON" \
  -d '{"kind":"file","file":"/long.py","timeout_sec":300}' \
  $BASE/v1/workspaces/$WS/runs | python3 -c 'import json,sys;print(json.load(sys.stdin)["run"]["id"])')
echo "$RUN"

for i in $(seq 1 10); do
  curl -s -H "$AUTH" $BASE/v1/workspaces/$WS/runs/$RUN \
    | python3 -c 'import json,sys;print("status:",json.load(sys.stdin)["run"]["status"])'
  sleep 1
done
```

### 4. Fetch logs incrementally

```sh
OFFSET=0
while :; do
  CHUNK=$(curl -s -H "$AUTH" \
    "$BASE/v1/workspaces/$WS/runs/$RUN/logs?stream=stdout&offset=$OFFSET&limit=64")
  echo "$CHUNK"
  DATA=$(printf '%s' "$CHUNK" | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"],end="")')
  OFFSET=$(printf '%s' "$CHUNK" | python3 -c 'import json,sys;print(json.load(sys.stdin)["next"])')
  EOF=$(printf '%s' "$CHUNK" | python3 -c 'import json,sys;print(json.load(sys.stdin)["eof"])')
  [ "$EOF" = "True" ] && break
done
```

### 5. Stop the run

```sh
curl -s -X POST -H "$AUTH" $BASE/v1/workspaces/$WS/runs/$RUN/stop
# {"run":{...,"status":"killed",...}}
```

### 6. Delete the workspace

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H "$AUTH" \
  $BASE/v1/workspaces/$WS
# 204

curl -s -o /dev/null -w '%{http_code}\n' -H "$AUTH" $BASE/v1/workspaces/$WS
# 404
```

## Known discrepancies

Places where the implemented behaviour differs from `api/openapi.yaml` (and in
some cases from `docs/SPEC.md`). The service is the source of truth; the spec
should be updated.

Each item was re-checked against the tree for this revision; items marked
**Verified** were confirmed by the grep/handler named in the item. Items 1–18
were written before the egress/tunnel work; items 19–21 cover the new
`/v1/workspaces/{id}/tunnels…` routes.

1. **Path-parameter name.** The router uses `{workspaceID}` and `{runID}`;
   `openapi.yaml` names them `workspaceId` and `runId`. No client-visible effect
   (the value is positional), but the spec is inaccurate. **Verified** — see also
   discrepancies 12, 16 and 19.
2. **Verified — `timeout` is still defined but never emitted.** It is a constant
   in `internal/model/errors.go` (`CodeTimeout`) and appears in the OpenAPI
   `ErrorCode` enum, but no handler produces it: `grep -rn CodeTimeout internal/`
   matches only its definition. (`conflict` is now emitted on run purge; see the
   run endpoints.)
3. **Verified — `insufficient_storage` (507) is emitted but is not a Go
   constant.** The quota mapping in `internal/api/errors.go:59` uses the string
   literal `"insufficient_storage"`; the OpenAPI enum lists it, `errors.go` does
   not define it (`grep -rn insufficient_storage internal/` matches only that
   literal).
4. **Verified — 405 is not `method_not_allowed`.** `MethodNotAllowed`
   (`internal/api/router.go:120`) returns HTTP 405 with `code:"validation"`.
   OpenAPI has no 405 response and no `method_not_allowed` code (`grep -n 405
   api/openapi.yaml` matches nothing).
5. **Verified — unknown routes return a documented-shaped 404.** The router's
   `NotFound` (`internal/api/router.go:117`) returns `404 not_found` "route not
   found"; `openapi.yaml` does not describe the catch-all.
6. **Resolved — reading a file can now return 413.** **Verified** —
   `internal/service/files.go:67` rejects a request `limit` above
   `caps.file_read_mb` with `413 payload_too_large`; the default read still
   returns a capped prefix with `truncated:true`.
7. **Resolved — `sha256` is over exactly the returned bytes.** **Verified** —
   `internal/workspace/fs.go:467` hashes `buf` (the returned slice) and
   `internal/api/files.go:176-178` reports `range`/`total_size`/`whole_file`
   alongside it.
8. **Resolved — `PUT /files/content` requires `content`.** **Verified** —
   `putFileRequest.Content` is a `*string` (`internal/api/files.go:276`), so an
   omitted field is `400 validation`; an explicit `""` writes a 0-byte file.
9. **Resolved — wrong-node-type operations return 422, not 500/404.**
   **Verified** — `internal/api/errors.go:84-89` maps both the is-a-directory
   and not-a-directory sentinels to `422 not_directory`, a code defined in
   `internal/model/errors.go:16`.
10. **Resolved — async `cwd` is now accepted.** **Verified** —
    `internal/service/runs.go:93` (`resolveRunCwd`) validates `cwd` for both
    kinds and rejects a missing/escaping/non-directory path with `422`
    (`runs.go:110`).
11. **Verified — `failed` runs have `exit_code:null`.** In
    `internal/service/runs.go:321-322` an executor error sets `RunFailed` and
    leaves `EndedAt`/`ExitCode` unset; `openapi.yaml` permits `exit_code` null
    but the semantic (null on failure) is undocumented.
12. **Path parameters are named `{workspaceID}`, `{runID}`, `{sessionID}`,
    `{processID}` and `{tunnelID}`** in the router while `openapi.yaml` uses the
    lower-case `workspaceId`/`runId`/`sessionId`/`processId`/`tunnelId`; no
    client-visible effect (the value is positional). Covered by discrepancy 1
    above; listed here for the new endpoints too.
13. **`/readyz` shape is richer than SPEC.md.** **Verified** — SPEC.md §2.1
    (`docs/SPEC.md:56`) lists `{"status","nsjail","cgroups","data_dir"}`, while
    the implementation (`internal/api/router.go:149-169`) adds `network` and
    `tunnels` sub-objects (each with `enabled`/`ready` and an optional `reason`).
    `status` is `ready`/`not_ready`. `openapi.yaml` matches the implementation.
14. **Verified — `LogChunk.truncated` is always false.** `internal/service/runs.go:637`
    returns `Truncated: false` unconditionally; the field is in the schema but
    never set true; run-log truncation is reported on the run's
    `logs.truncated` (`runLogsJSON.Truncated`).
15. **Session `DELETE` returns 200, not 204.** Unlike `DELETE .../runs/{id}`
    (204), `DELETE .../sessions/{id}` returns 200 with a `{session, changed}`
    body (`internal/api/sessions.go:311`) so the idempotency signal and the
    terminal state are observable. It retains the terminal record, so a repeat
    DELETE reports `changed:false` rather than 404. **Verified — and now
    documented**: `openapi.yaml` describes the 200 (`SessionAction` with
    `changed`), so this is no longer a spec gap, only a divergence from the
    run-delete convention.
16. **Session path parameter is `sessionId` in OpenAPI, `sessionID` in the
    router; process is `processId`/`processID` like-wise** (same class as
    discrepancy 1). The value is positional, so there is no client-visible
    effect.
17. **`GET .../sessions/{id}/output` returns 409 `session_ended` after DELETE.**
    **Verified** — `internal/service/sessions.go:617-619` returns the tombstone
    error when status is `killed` with reason `user_closed`; a session that ended
    on its own keeps its output readable. This is the one place the session
    output differs from the run log (which stays readable).
18. **Resolved — a bad `cwd` is 422, not 503 (defect N-1).** **Verified** —
    `executor.ErrInvalidCwd` is mapped to `422 unprocessable` in
    `internal/api/errors.go:78-80`. A genuine sandbox setup failure is still
    `503`. See [VERIFICATION.md §9d](VERIFICATION.md).
19. **Tunnel path parameter is `tunnelId` in OpenAPI, `tunnelID` in the router**
    (`internal/api/router.go:74-75`; `openapi.yaml:1108`). Same class as
    discrepancies 1, 12 and 16 — the value is positional, so there is no
    client-visible effect. **Verified.**
20. **`POST .../tunnels` declares its `504` inline, not through a shared
    response ref.** The endpoint lists the 504 with an inline `ErrorEnvelope`
    (`openapi.yaml:406-410`) rather than a `$ref`, while its other 4xx/5xx use
    the shared refs. The `tunnel_url_unavailable` code is in the `ErrorCode`
    enum (`openapi.yaml:1192`). Worth noting because this 504 is the
    *rate-limit* path (`429` from Cloudflare, surfaced honestly), not a server
    fault. **Verified** against `internal/model/errors.go:58`
    (`CodeTunnelURLUnavailable`) and the `docs/API.md` endpoint section.
21. **`POST .../tunnels` maps a not-opted-in / server-switch-off refusal to `422
    tunnels_disabled` (not `403`), but enumerates it only in the `ErrorCode`
    enum, not next to the route.** The route's 422 uses the generic
    `Unprocessable` response ref (`openapi.yaml:404`), so the specific
    `tunnels_disabled`/`too_many_tunnels` codes are not named on the operation
    even though they are in the enum. **Verified** against
    `internal/service/tunnels.go:34-45` (`TunnelsAllowed`) and
    `internal/service/tunnels.go:82-84`.

## Notes

- SSE log streaming is intentionally deferred to v0.1; poll with `offset`/`limit`.
- `stop` sends SIGTERM to the process group, waits `sandbox.kill_grace_sec`
  (default 3 s), then SIGKILL; unbounded runs stop through the identical path.
- Runs are retained per workspace (newest 100), pruned before each new run.
- There is **no notion of a named long-lived "project"/"service"** (no
  restart-on-crash, no autostart, no lifecycle beyond a single run or session).
  That is a documented non-goal for this version; see
  [SPEC.md §2.9](SPEC.md) and [DECISIONS.md](DECISIONS.md) #61.
