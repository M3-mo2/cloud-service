# API guide

Human-friendly, self-sufficient reference for the sandboxd HTTP/JSON API
(v0.0.5). The machine-readable contract is
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
| `workspace_suspended` | 409 | Work refused because the workspace is suspended | `POST .../exec` or `POST .../runs` on a `suspended` workspace. |
| `validation` | 400 | Malformed or invalid request | Bad JSON, unknown JSON field, failing `validate` tag, missing query/body field, bad `stream`/`status`/`kind` filter, unknown route body. Also carries the **405** status for method-not-allowed. |
| `unprocessable` | 422 | Unsafe path or invalid limit/target | Path traversal / symlink escape, `cwd` escape, command not found, deleting the workspace root, file-run target that is a directory, a limit `<= 0` or **above the server maximum**. |
| `payload_too_large` | 413 | Body or file over a hard cap | JSON body > `json_body_mb`, upload > `upload_mb`, file write > `file_write_mb`. |
| `insufficient_storage` | 507 | Workspace disk quota exceeded | Write/upload/rename past `limits.disk_mb`. |
| `timeout` | 504 | Timeout | **Defined but never emitted** (see discrepancies). |
| `unavailable` | 503 | Sandbox backend unavailable | nsjail binary missing, or nsjail could not set up/launch the sandbox (`details.nsjail`). |
| `internal` | 500 | Unexpected server error | Bugs and unmapped filesystem conditions (e.g. listing a path that is a file). |

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
| `caps.file_read_mb` | 5 | `GET /files/content` | returns a **prefix** with `truncated:true` (not an error) |
| `caps.exec_stdout_kb` | 256 | Sync exec stdout | `truncated:true` |
| `caps.exec_stderr_kb` | 256 | Sync exec stderr | `truncated:true` |
| `caps.run_log_mb` | 10 | Each async run log stream | `truncated:true` on the run |

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
  "nsjail":bool,"cgroups":bool,"data_dir":bool}`.
- **Errors:** none; unsuitability is expressed by the 503 and the boolean fields.
- **Semantics:** `data_dir` is true when a temp file can be created under the
  data directory; `nsjail` when the nsjail binary is found; `cgroups` when the
  cgroup v2 hierarchy at `/sys/fs/cgroup` is writable by the process (requires a
  container started with `--cgroupns=host`). Any false ⇒ 503.

```sh
curl -s localhost:8080/readyz
# {"cgroups":true,"data_dir":true,"nsjail":true,"status":"ready"}
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
    "version": "0.0.5",
    "api_version": "v1",
    "runtimes": ["python3"],
    "limits_defaults": {
      "cpu_cores": 1, "memory_mb": 512, "disk_mb": 1024,
      "max_wall_time_sec": 300, "max_runs": 3, "allow_net": false
    },
    "limits_maxima": {
      "cpu_cores": 8, "memory_mb": 8192, "disk_mb": 51200,
      "max_wall_time_sec": 3600, "max_runs": 64, "allow_net": true
    }
  }
  ```
- **Errors:** 401 `unauthorized`.
- **Semantics:** `limits_defaults` is the value a workspace gets when `limits`
  is omitted at creation; it comes from the `limits:` config block.
  `limits_maxima` is the server ceiling (`max_limits:` config) enforced on
  creation and `PATCH`.

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
  | `allow_net` | `bool` | `allow_net` | `false` (no network) or `true` |

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

Stop every running/queued run of the workspace and leave it `active`.

- **Path parameter:** `workspaceID`.
- **Response 200:**

  ```json
  {"workspace": <workspace with status "active">,
   "stopped_run_ids": ["run_..."], "stopped_run_count": 1}
  ```

- **Errors:** 401, 403, 404.
- **Semantics:** this is the "stop its operations" control. It kills every
  tracked run (SIGTERM→SIGKILL process group) and reports what was stopped. The
  workspace keeps `status: active`, so new work is still accepted. **Idempotent
  and a no-op success** (`stopped_run_count: 0`) when nothing is running. It
  differs from `suspend` only in that it does **not** block future work.

```sh
curl -s -X POST -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/stop
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

List a directory.

- **Path parameter:** `workspaceID`.
- **Query parameters:**
  - `path` (string, default `/`) — directory path relative to the workspace
    root; a leading `/` is accepted and normalized.
- **Response 200:**

  ```json
  {"entries":[{"name":"main.py","path":"/main.py","type":"file",
               "size":12,"mod_time":"2026-01-02T15:04:05Z"}]}
  ```

  `type` is `file`, `dir`, or `symlink`. `path` is the absolute
  workspace-rooted path (always starts with `/`). `size` is the entry size in
  bytes; `mod_time` is RFC3339 UTC.
- **Errors:** 400 `validation` (n/a here), 401, 403, 404 `not_found` (directory
  does not exist), 422 `unprocessable` (unsafe path).
- **Semantics:** the `path` is sanitized with `cleanRelative` then `secureJoin`:
  `..` above the root and symlinks resolving outside the root are rejected with
  422. Listing a path that is a **file** returns **500** (see discrepancies).

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files?path=/"
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

Read a file.

- **Path parameter:** `workspaceID`.
- **Query parameters:**
  - `path` (string, **required**) — file path.
  - `encoding` (string, optional) — `text` (default) or `base64`; only the exact
    value `base64` is special, anything else is treated as text.
  - `meta` (string, optional) — the exact value `true` selects the JSON wrapper.
- **Response 200 (raw, default):** the file bytes with the detected
  `Content-Type`, `Content-Disposition: attachment; filename=<base>`,
  `Content-Length` and `X-Sandboxd-Truncated: true|false`.
- **Response 200 (`meta=true`):**

  ```json
  {"path":"/main.py","content":"print(\"hi\")\n","encoding":"text",
   "size":12,"sha256":"0ca9...","content_type":"text/x-python; charset=utf-8",
   "truncated":false}
  ```

  `content` is base64 when `encoding=base64`. `path` echoes the query string
  verbatim (not normalized). `size` is the real file size; `content` may be a
  prefix of it when `truncated` is true.
- **Errors:** 400 `validation` (path missing), 401, 403, 404 `not_found`,
  422 `unprocessable`.
- **Semantics:** at most `caps.file_read_mb` bytes are returned; a larger file
  yields a truncated prefix with `truncated:true` (no error). For a truncated
  read the `sha256` is over the **returned prefix**, not the whole file (see
  discrepancies). Reading a directory returns 500 (see discrepancies).

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/main.py"

curl -s -H "Authorization: Bearer $KEY" \
  "localhost:8080/v1/workspaces/$WS/files/content?path=/main.py&meta=true"
```

### PUT /v1/workspaces/{workspaceID}/files/content

Write (create or overwrite) a file.

- **Path parameter:** `workspaceID`.
- **Query parameters:** `path` (string, **required**).
- **Request body (`putFileRequest`):**

  | Field | Go type | JSON | Required | Validation | Default |
  |-------|---------|------|----------|------------|---------|
  | `content` | `string` | `content` | no | none — an omitted field writes an **empty** file | `""` |
  | `encoding` | `string` | `encoding` | no | `base64` decodes the content; anything else is literal text | `text` |

- **Response 204:** empty body.
- **Errors:** 400 `validation` (path missing, invalid JSON, invalid base64),
  401, 403, 413 `payload_too_large` (body > `json_body_mb`, or content >
  `file_write_mb`), 422 `unprocessable`, 507 `insufficient_storage` (quota).
- **Semantics:** missing parent directories are created. The write is accounted
  against `limits.disk_mb`. Writing over a path that is a directory returns 500
  (see discrepancies).

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

### POST /v1/workspaces/{workspaceID}/runs

Start an asynchronous run. Returns immediately; poll it via the run endpoints.

- **Path parameter:** `workspaceID`.
- **Request body (`createRunRequest`):**

  | Field | Go type | JSON | Required | Validation | Default |
  |-------|---------|------|----------|------------|---------|
  | `kind` | `string` | `kind` | yes | `required,oneof=file command` | — (400) |
  | `file` | `string` | `file` | for `kind=file` | must resolve inside the root and exist | — |
  | `args` | `[]string` | `args` | no | appended after the file for `kind=file` | — |
  | `argv` | `[]string` | `argv` | for `kind=command` | non-empty | — |
  | `env` | `map[string]string` | `env` | no | same allowlist as `exec` | none |
  | `timeout_sec` | `int` | `timeout_sec` | no | `<=0` or `>` workspace `max_wall_time_sec` → clamped to it | workspace `max_wall_time_sec` |

  **There is no `cwd` field.** Async runs execute with the sandbox CWD at the
  workspace root (`/workspace`). Supplying `cwd` is an unknown field and is
  rejected with 400 `validation`.

- **Response 202:**

  ```json
  {"run":{"id":"run_01J...","workspace_id":"ws_01J...","kind":"file",
    "argv":["python3","main.py"],"status":"queued","exit_code":null,
    "started_at":"2026-01-02T15:04:05Z","ended_at":"",
    "logs":{"stdout_bytes":0,"stderr_bytes":0,"truncated":false}}}
  ```
- **Errors:** 400 `validation` (bad `kind`, missing `file`/`argv`, bad JSON),
  401, 403, 404 `not_found` (`kind=file` target missing — or the workspace
  missing), 409 `workspace_suspended`, 422 `unprocessable` (`kind=file` target
  is a directory, or the path escapes the root).
- **Semantics:**
  - `kind=file` validates the target at request time and builds the argv
    `["python3", <file without leading slash>, ...args]`; the interpreter is
    **`python3`** and files are passed as arguments to it (any extension is
    accepted — Python decides). A missing file is 404, a directory is 422.
  - `kind=command` executes `argv` verbatim.
  - Old runs are pruned before each new run: the newest **100** runs per
    workspace are retained (`storage.Retention`); the oldest are deleted.
  - Each run's stdout/stderr are streamed to per-run log files, capped at
    `caps.run_log_mb` per stream.

```sh
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"file","file":"/main.py","timeout_sec":300}' \
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
| Network | `limits.allow_net` | false | true | clones the network namespace unless true |

The server maximum is a hard ceiling: a create or `PATCH` value above it is
rejected with `422 unprocessable` (never silently clamped). `max_limits:` is
loaded from config and always at least the defaults.

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
- **Network:** unavailable by default. The network namespace is cloned unless the
  workspace sets `allow_net:true`; there is no host allowlist in v0.0.5.
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
# {"version":"0.0.5","api_version":"v1", ... }

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

1. **Path-parameter name.** The router uses `{workspaceID}` and `{runID}`;
   `openapi.yaml` names them `workspaceId` and `runId`. No client-visible effect
   (the value is positional), but the spec is inaccurate.
2. **`timeout` is still defined but never emitted.** It is a constant in
   `internal/model/errors.go` and appears in the OpenAPI `ErrorCode` enum, but no
   handler produces it. (`conflict` is now emitted on run purge; see the run
   endpoints.)
3. **`insufficient_storage` (507) is emitted but is not a Go constant.** The
   quota mapping in `internal/api/errors.go` uses the string literal
   `"insufficient_storage"`; the OpenAPI enum lists it, `errors.go` does not
   define it.
4. **405 is not `method_not_allowed`.** `MethodNotAllowed` returns HTTP 405 with
   `code:"validation"`. OpenAPI has no 405 response (and no `method_not_allowed`
   code).
5. **Unknown routes return a documented-shaped 404.** The router's `NotFound`
   returns `404 not_found` "route not found"; `openapi.yaml` does not describe the
   catch-all.
6. **Reading a file never returns 413.** `openapi.yaml` lists 413 for
   `GET .../files/content`, but the read path returns a truncated prefix with
   `truncated:true` instead of erroring at `file_read_mb`.
7. **`sha256` is over the returned prefix for truncated reads.** For files larger
   than `file_read_mb`, the `meta=true` wrapper hashes the truncated bytes, not
   the whole file.
8. **`PUT /files/content` does not require `content`.** `openapi.yaml` marks it
   required; the implementation has no `validate` tag, so `{}` writes a 0-byte
   file.
9. **Operations on the wrong node type return 500, not 4xx.** Listing a path that
   is a file, reading a directory, or writing over a directory map to
   `500 internal` (the internal `errNotDirectory` is not translated). OpenAPI
   implies 4xx.
10. **Async `cwd` is rejected, not accepted.** Runs execute at the workspace
    root; a `cwd` field on `POST /runs` is an unknown field → 400. Only sync
    `exec` accepts `cwd`.
11. **`failed` runs have `exit_code:null`.** No exit code is recorded when the
    executor errors; `openapi.yaml` permits `exit_code` null but the semantic
    (null on failure) is undocumented.
12. **Path parameters are named `{workspaceID}`/`{runID}`** in the router while
    `openapi.yaml` uses `workspaceId`/`runId`; no client-visible effect (the value
    is positional). Covered by discrepancy 1 above; listed here for the new
    endpoints too.
13. **`/readyz` shape is richer than SPEC.md.** SPEC.md §2.1 lists
    `{"status","nsjail","data_dir"}`; the implementation adds `cgroups` and uses
    `status` values `ready`/`not_ready`. `openapi.yaml` matches the
    implementation.
14. **`LogChunk.truncated` is always false.** The field is in the schema but is
    never set to true; log truncation is reported on the run's `logs.truncated`.

## Notes

- SSE log streaming is intentionally deferred to v0.1; poll with `offset`/`limit`.
- `stop` sends SIGTERM to the process group, waits `sandbox.kill_grace_sec`
  (default 3 s), then SIGKILL.
- Runs are retained per workspace (newest 100), pruned before each new run.
