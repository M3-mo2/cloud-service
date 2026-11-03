# Client Integration Guide

**Audience:** the author of a frontend product — a Telegram bot, a web console, a
CLI — that authenticates its own users and calls this engine "on the user's
behalf" with a service API key. End users never touch these APIs directly.

**Scope:** this is a *usage contract*, not an endpoint index. It states, for each
thing a product must do, the calls to make in order and the exact response fields
to depend on. The per-endpoint reference is [`docs/API.md`](API.md); the OpenAPI
document is [`api/openapi.yaml`](../api/openapi.yaml); measured transcripts for
every flow below are in
[`docs/INTEGRATION-VERIFICATION.md`](INTEGRATION-VERIFICATION.md).

The engine version this document describes is **0.0.20** (`model.Version`). The
HTTP contract prefix is `/v1` (`model.APIVersion`).

---

## 1. Start by reading `/v1/meta` — never hardcode

Every `/v1` route requires a bearer key:

```http
GET /v1/meta HTTP/1.1
Authorization: Bearer <service-api-key>
```

`/healthz`, `/readyz` and the Prometheus `/metrics` route are unauthenticated.
`/v1/meta` is authenticated and is the engine's self-description. **Read it at
startup** and again whenever a request is refused with `422`/`409` because your
cached numbers were stale. Build every picker, slider and cap in your UI from
this response, not from constants in your code.

### The two rules that matter most

1. **`limits_defaults` is what a workspace gets when your request names no
   limits; `limits_maxima` is the maximum you MAY request.** A request above a
   maximum is refused with HTTP `422` and code `unprocessable` — it is never
   silently clamped. If your UI offers a value the engine rejects, users get a
   hard failure; derive the UI from these two objects.
2. **`runtimes`, `interpreters`, `runnable_extensions` and `profiles` decide what
   you may ask a workspace to execute.** A workspace can only run a tool its
   profile actually provides. `runnable_extensions` maps a file extension to the
   interpreter that runs it (`.py`→`python3`, `.js`/`.mjs`/`.cjs`→`node`,
   `.sh`/`.bash`→`bash`, `.pl`/`.pm`→`perl`), and `interpreters` is the closed set
   you may pass as an explicit `interpreter`. `profiles` lists each offered
   profile with the full list of `commands` it provides; a request that names a
   profile or a tool outside these is refused (`unknown_profile`), not switched
   to something else.

### Every top-level key, and what to do with it

| Key | Type | What the client does with it |
| --- | --- | --- |
| `version` | string | The engine build (`"0.0.20"`). Log it; use it for support. |
| `api_version` | string | The HTTP contract prefix (`"v1"`). Assert it matches your client. |
| `runtimes` | string[] | The runtimes available to `exec`/`runs` (currently `["python3"]`). Show only these. |
| `interpreters` | string[] | The closed allowlist for a run's explicit `interpreter` field (`bash`, `node`, `perl`, `python3`). |
| `runnable_extensions` | string[] | File extensions that map to an interpreter; a file's `runnable` field reports one of these. |
| `profiles` | object | `{default, ready, profiles:[{name, description, commands, paths, languages, provides, notes}]}`. The `default` is applied when a create omits `profile`. Offer only `profiles[].name`. |
| `limits_defaults` | object | The per-workspace limit defaults (see rule 1). |
| `limits_maxima` | object | The server maxima (see rule 1). A value above these is `422`. |
| `network` | object | `{enabled, available, ready, egress, reason?}`. Whether `allow_net` can give a workspace egress. If `enabled:false`, `allow_net` still creates a private empty namespace — no internet. |
| `ports` | object | `{enabled, include_unix, max_pids, max_fds_per_pid, max_pids, mechanism, routes, reachability, reads_total, default_note}`. Whether `GET /ports` is served. |
| `metrics` | object | `{enabled, interval_seconds, min_interval_sec, max_interval_sec, include_cpu, max_subscribers, mechanism, limits, routes}`. The stream cadence and its enforced floor. |
| `archives` | object | `{enabled, formats, read_only, limits:{max_entries,max_total_mb,max_file_mb,max_ratio,upload_mb,overwrite,skip_junk}, routes, refusal_codes, semantics}`. What you may upload/extract and the stable refusal codes. |
| `subscriptions` | object | `{enabled, default_duration, max_duration, grace_period, reap_interval, duration_grammar, expiry_semantics, locked_status, summary_present_on}`. The term grammar and what a lapsed term does. |
| `tunnels` | object | `{enabled, ready, max_per_workspace, url_timeout_sec, reason?}`. Whether `POST /tunnels` can publish a port. |
| `workspace_limits` | object | `{enabled, mechanism, reporting, enforcement, limits, reason?}`. How a workspace's disk is reported and enforced (`fuse` = a real per-workspace filesystem; `counter` = a sampled watchdog). |
| `write_rate` | object | `{enabled, server_max_mb_s, default_mb_s, samples, outcome, refusal_code, overshoot_formula, mechanism, limits, one_line_enable}`. The sampled write-rate ceiling (default off). |
| `egress_decoy` | object | `{enabled, ready, mechanism, limits, reason?}`. Whether a workspace's IP-discovery is answered with a decoy identity. A decoy, not a security boundary. |
| `identity_masking` | object | `{enabled, ready, mechanism, limits, reason?}`. Whether host identity is masked for a workspace. |
| `provider_tools_hidden` | object | `{enabled, ready, mechanism, limits, reason?}`. Whether the platform's own tooling is absent from a workspace. |

> **Observed top-level keys (v0.0.20):** `api_version`, `archives`,
> `egress_decoy`, `identity_masking`, `interpreters`, `limits_defaults`,
> `limits_maxima`, `metrics`, `network`, `ports`, `profiles`,
> `provider_tools_hidden`, `runnable_extensions`, `runtimes`, `subscriptions`,
> `tunnels`, `version`, `workspace_limits`, `write_rate`. Treat this list as
> informative: read whatever the running service returns.

---

## 2. The canonical lifecycle, end to end

One worked flow with real bodies. All requests carry
`Authorization: Bearer <key>`; JSON bodies also carry
`Content-Type: application/json`.

### 2.1 Create a workspace

```http
POST /v1/workspaces
{"name":"demo","profile":"python"}
```

`profile` is top-level (it selects the toolchain, orthogonal to limits). Omit it
for the server default. Response is `201`:

```json
{"workspace":{
  "id":"ws_01M6HJASCWM9Z1PF755GT89NK4",
  "owner_id":"user_dev","name":"demo","profile":"python",
  "created_at":"2026-11-03T20:41:39Z","status":"active",
  "limits":{"cpu_cores":1,"memory_mb":512,"disk_mb":256,"max_wall_time_sec":300,
            "max_runs":8,"allow_net":true,"allow_unlimited_runs":true,
            "allow_tunnels":false,"write_rate_mb_s":0},
  "last_activity_at":"2026-11-03T20:41:39Z",
  "summary":{"state":"unlimited","age_sec":0,"created_at":"...","runs":{"total":0,"active":0},
             "disk":{"used_bytes":0,"limit_bytes":268435456},"profile":"python"}
}}
```

**Depend on:** `workspace.id` (the handle for every later call), `status`, and
`summary.state` (one of `unlimited`, `active`, `expiring_soon`, `locked`).
`summary` is present on **every** workspace get and list item, so a dashboard
renders in one request.

### 2.2 Upload a project

Write files with `PUT /files/content` (see §3). A JSON body, not raw bytes:

```http
PUT /v1/workspaces/{id}/files/content?path=/main.py
{"content":"<base64>","encoding":"base64"}
```

### 2.3 Install dependencies

```http
POST /v1/workspaces/{id}/packages/install
{"packages":["six"]}
```

`200` with `{exit_code, output, stdout, timed_out, truncated, duration_ms,
target:"/opt/python/private"}`. **Read `exit_code`**: a non-zero code is a
failed install whose reason is in `output`. Installation runs `pip` inside the
workspace's own sandbox and requires `allow_net:true`; without it `pip` cannot
reach an index and `exit_code` is non-zero. List with `GET /packages`, remove one
with `DELETE /packages?name=six`, remove all with `DELETE /packages?all=true`.

### 2.4 Run the project

For a quick synchronous result use `exec` (§4.1); for a long-lived process use a
run (§4.2). See the dedicated section for the exact shapes.

### 2.5 Open an interactive terminal

Create a session (§5), send input, poll output with an offset, resize and signal,
then delete it.

### 2.6 Find the ports it listens on

```http
GET /v1/workspaces/{id}/ports
```

Reports the listening endpoints and their owning process (§7). These are inside
the workspace's own namespace; they are **not** published.

### 2.7 Suspend, resume, stop, delete

```http
POST   /v1/workspaces/{id}/suspend     # refuse new work, stop runs/sessions, keep data
POST   /v1/workspaces/{id}/resume      # back to active
POST   /v1/workspaces/{id}/stop        # kill everything, stay active
DELETE /v1/workspaces/{id}             # remove forever → 204
```

See §8 for the exact response fields and the differences.

---

## 3. Uploading and managing files

All file routes are under `/v1/workspaces/{workspaceId}/files*`. Paths are
workspace-relative; the workspace root is `/`. A sandboxed command sees the
workspace mounted at `/workspace`, so a file you wrote at `/src/main.py` is
`/workspace/src/main.py` to `exec`/`runs`.

| Route | Method | Purpose | Body / params |
| --- | --- | --- | --- |
| `/files` | GET | List a directory (paged) | `?path=/&offset=&limit=` |
| `/files` | DELETE | Delete a file | `?path=` |
| `/files/stat` | GET | File metadata | `?path=` |
| `/files/content` | GET | Read file | `?path=&encoding=&offset=&limit=&start_line=&end_line=&meta=` |
| `/files/content` | PUT | **Write file** | **JSON** `{"content":"...","encoding":"base64"}` + `?path=` |
| `/files/download` | GET | Download **raw bytes** | `?path=` |
| `/files/upload` | POST | Multipart upload | form `path` + `file` |
| `/files/mkdir` | POST | Create a directory | JSON `{"path":"/src"}` |
| `/files/rename` | POST | Move/rename | JSON `{"from":"/a","to":"/b"}` |
| `/files/archive` | POST | Upload-and-extract | multipart `archive` + `path`, `overwrite`, `dry_run` |
| `/files/archive` | GET | Export a directory as a zip | `?path=/` |
| `/files/extract` | POST | Extract a file already in the workspace | JSON `{"path":"/x.zip","dest":"/out","overwrite":false,"dry_run":false}` |

### 3.1 Read and write

`PUT /files/content` takes a **JSON body** — the content is a string field, and
binary must be base64 with `"encoding":"base64"`. It writes and returns `204`
with no body:

```http
PUT /v1/workspaces/{id}/files/content?path=/main.py
{"content":"cHJpbnQoMSkK","encoding":"base64"}
```

`GET /files/content?path=/main.py&meta=true` returns the JSON wrapper
`{path, content, encoding, size, total_size, range, whole_file, sha256,
content_type, truncated, binary}`. Without `meta=true` it streams the raw file
bytes with `Content-Type` and `X-Sandboxd-*` headers.

`GET /files/download?path=/data.txt` **always returns raw bytes** (with
`Content-Disposition`) — it must not be parsed as JSON.

### 3.2 List, stat, mkdir, rename

A list entry is
`{name, path, type, size, mode, mod_time, target?, target_inside?, runnable?}`.
`type` is `file`/`dir`/`symlink`; `runnable` names the interpreter for a
recognised extension (e.g. `"python3"` for `.py`). `stat` adds `content_type`,
`sha256` and `binary`. `mkdir` returns `201 {"path":...}`; `rename` returns `204`;
`DELETE /files?path=` returns `204`.

### 3.3 Archives

`POST /files/archive` takes a multipart `archive` file and extracts it into
`path` (default `/`). `POST /files/extract` extracts a file already inside the
workspace. `GET /files/archive?path=/` streams a **zip** of a directory.

Advertised limits live in `/v1/meta.archives.limits`:
`max_entries`, `max_file_mb`, `max_total_mb`, `max_ratio` (uncompressed/compressed
bomb guard), `upload_mb` (multipart upload cap), `overwrite` (default `false` —
an existing file is not clobbered), `skip_junk` (macOS/Windows metadata is
skipped and reported `skipped:true`).

Supported formats are `zip` and `rar`, detected from **content magic bytes, never
the filename**. **RAR is read-only by design** — its encoder is proprietary, so
the engine extracts RAR4/RAR5 but only ever writes ZIP. A product must not offer
"create .rar". `read_only` in `/v1/meta.archives` names `["rar"]`.

Every refusal is one of the stable `refusal_codes` (see §9): `unsupported_archive`
(415), `archive_too_large`/`archive_too_many_entries`/`archive_ratio_exceeded`
(413), `archive_encrypted`/`archive_multivolume`/`archive_unsafe_entry` (422),
`archive_entry_exists` (409). Extraction validates every entry *before* writing,
so a refused archive leaves no partial tree.

---

## 4. Executing things — two ways, and when to use each

### 4.1 `exec` — one command, direct output, hard 60 s cap

```http
POST /v1/workspaces/{id}/exec
{"cmd":["python3","/workspace/src/main.py","a","b"],"cwd":"/","env":{"K":"v"},
 "stdin":"","timeout_sec":10}
```

`cmd` is an argv array. Response `200`:

```json
{"exit_code":0,"stdout":"hello from main.py\nargs: ['a', 'b']\n",
 "stderr":"","duration_ms":68,"truncated":false}
```

`timeout_sec` is honored but **hard-capped at 60 s**; a request above it is
clamped to 60. Use `exec` for a quick, bounded command whose output you show
directly. `cwd` is relative to the workspace root (`/` = the root). `stdin` is a
string written to the command's standard input.

### 4.2 `runs` — asynchronous, and its output is **not in the run object**

```http
POST /v1/workspaces/{id}/runs
{"kind":"file","file":"/src/main.py","args":["x","y"]}
```

Create returns **`202 Accepted`** with a `run` whose `status` is `queued`:

```json
{"run":{"id":"run_01M6H...","workspace_id":"ws_...","kind":"file",
  "argv":["python3","/workspace/src/main.py","x","y"],"status":"queued",
  "exit_code":null,"started_at":"...","ended_at":"","cwd":"/",
  "timeout_sec":300,"unlimited":false,"interpreter":"python3",
  "logs":{"stdout_bytes":0,"stderr_bytes":0,"truncated":false}}}
```

**There is no `stdout`/`stderr` field on a run.** A client that reads the run
body expecting `stdout` finds nothing and reports an empty result — this has
already happened downstream. The output comes from the logs endpoint, read **by
byte offset**:

```http
GET /v1/workspaces/{id}/runs/{runId}/logs?stream=stdout&offset=0&limit=65536
```

```json
{"data":"hello from main.py\nargs: ['x', 'y']\n","offset":0,"next":36,
 "eof":true,"truncated":false}
```

The fields:

- `data` — the chunk from `offset`, as text.
- `offset` — the byte offset this chunk started at (echoes what you asked).
- `next` — **the offset to ask for next**. Poll from here.
- `eof` — true when you have read everything written so far; the run may still be
  running, so also check `run.status`.
- `truncated` — true when the run's log cap (`caps.run_log_mb`) was hit; older
  bytes were dropped.

`stream` is `stdout` (default) or `stderr`.

A short polling loop, reading each stream to its end and stopping when the run is
terminal:

```python
def follow(base, ws, run, headers):
    out = ""
    offsets = {"stdout": 0, "stderr": 0}
    while True:
        for stream in ("stdout", "stderr"):
            r = get(f"{base}/v1/workspaces/{ws}/runs/{run}/logs",
                    params={"stream": stream, "offset": offsets[stream]},
                    headers=headers).json()
            out += r["data"]
            offsets[stream] = r["next"]
        status = get(f"{base}/v1/workspaces/{ws}/runs/{run}",
                     headers=headers).json()["run"]["status"]
        if status not in ("queued", "running"):
            break
    return out
```

#### Run request fields

- `kind` — `"file"` or `"command"` (required).
- **`kind:"file"`** — set `file` to a workspace-relative path; the engine picks
  the interpreter from the extension (or `interpreter` if you pass one). Pass
  extra arguments in `args` (an array). Do **not** pass `argv` with `kind:"file"`
  (that is a `422`).
- **`kind:"command"`** — set `argv` to the full command array (required). `file`
  and `args` do not apply.
- `env` — `{NAME: value}` merged into the run's environment.
- `cwd` — working directory, relative to the workspace root.
- `interpreter` — one of `/v1/meta.interpreters`; overrides the extension guess.
- `timeout_sec` — a finite wall-clock limit, capped by the workspace's
  `max_wall_time_sec`.
- `no_timeout` — `true` requests an unbounded run. It is accepted **only when both
  the workspace's `limits.allow_unlimited_runs` and the server's
  `max_limits.allow_unlimited_runs` are true**; otherwise `422 unprocessable`. A
  run created with `no_timeout:true` reports `"unlimited":true` and
  `"timeout_sec":null`.

A run's terminal `status` is one of `exited`, `killed`, `timeout`, `failed`,
`interrupted`, `oom` (the memory ceiling ended it), `rate_limited` (the write-rate
ceiling ended it). Inspect `exit_code` and `logs`.

---

## 5. The interactive terminal — `sessions` is the supported way

The engine has a **native PTY session API**. Use it; do not build your own.

### 5.1 Create

```http
POST /v1/workspaces/{id}/sessions
{"shell":"/bin/sh","cwd":"/","cols":80,"rows":24,"env":{},"timeout_sec":0}
```

All fields optional. `cols`/`rows` default to 80×24 and are bounded to
`1..1000` (outside → `422 unprocessable`). Response is **`201`** with a
`session` object; note the identifier field name:

```json
{"session":{"id":"ses_01M6HJATZTFXTWXWBG8A5KWYS8",
  "workspace_id":"ws_...","shell":"/bin/sh","cwd":"","cols":80,"rows":24,
  "status":"running","created_at":"...","last_activity_at":"...",
  "started_at":"...","ended_at":"","exit_code":null,
  "output_bytes":0,"output_truncated":false}}
```

**Depend on `session.id`** (`ses_<ULID>`), `status`, and `reason`/`diagnostic`
when it ends. A session whose profile cannot host the PTY relay (no `python3`)
is refused at create with `422 session_needs_interpreter`. A failed launch is
reported as `status:"failed"` with a `diagnostic`, never as a running session
whose output is a traceback.

### 5.2 Input, output, resize, signal, delete

| Route | Method | Body / params | Success |
| --- | --- | --- | --- |
| `/sessions` | GET | `?status=&limit=&offset=` | `200` list |
| `/sessions/{sessionId}` | GET | — | `200 {"session":...}` |
| `/sessions/{sessionId}` | DELETE | — | `200 {"session":...,"changed":bool}` |
| `/sessions/{sessionId}/input` | POST | JSON **`{"data":"..."}`** | `204` |
| `/sessions/{sessionId}/output` | GET | `?offset=&limit=` | `200` chunk |
| `/sessions/{sessionId}/resize` | POST | JSON `{"cols":100,"rows":40}` | `204` |
| `/sessions/{sessionId}/signal` | POST | JSON `{"signal":"INT"}` | `204` |

The input field is named **`data`**; the identifier in the path is
`{sessionId}`. Signals are limited to `INT`, `TERM`, `QUIT`, `HUP` (others →
`422`). Output uses the **same offset protocol as run logs** — read `{data,
offset, next, eof, truncated}` and poll from `next`:

```http
POST /v1/workspaces/{id}/sessions/{sessionId}/input   {"data":"echo hi\r"}
GET  /v1/workspaces/{id}/sessions/{sessionId}/output?offset=0
```
```json
{"data":"$ echo hi\r\nhi\r\n$ ","offset":0,"next":18,"eof":true,"truncated":false}
```

### 5.3 Properties a client depends on

- It is a **real PTY**: full-screen programs, colours, `stty size`, job control
  and interrupts work.
- The shell keeps state between calls — `cd`, `export`, aliases survive — because
  it is one long-lived process.
- Output is **polled** with an offset, exactly like run logs.
- A session has a lifetime (`sessions.lifetime_sec`, default 3600 s) and an idle
  timeout (`sessions.idle_timeout_sec`, default 900 s). When it ends you must
  **create a new session**; sending input to an ended session returns
  `409 session_ended`. `DELETE` retains the record but makes further input/signal/
  resize return `409 session_ended`; its output remains readable until the
  workspace is deleted.
- Concurrency is capped at `sessions.max_per_workspace` (default 4); past it,
  create returns `409 too_many_sessions`.

### 5.4 Do NOT do this: a long-lived `run` reading commands from a FIFO

Do **not** implement a terminal by creating a long-lived `run` whose script reads
commands out of a FIFO in the workspace and evaluates them in the same process.
This is a **fact, not a preference**: that approach needs tools — `mkfifo`,
`flock`, `base64`, `timeout` — to be present **inside the workspace**, and a
workspace only gets the tools its profile names (the profile is an explicit
allowlist). The approach therefore **breaks silently whenever the profile
changes**: on a profile that does not name those four tools, the FIFO is never
created and the client reports an empty or hung terminal with no error. A
hand-rolled PTY emulation over a run also **loses job control and terminal
sizing** (`stty size` is wrong, `Ctrl-C` does not reach the foreground process
group). The native `sessions` API exists precisely so a client does not have to
reproduce a PTY; use it.

---

## 6. Watching resource usage

Two routes per scope: a one-shot snapshot and an SSE stream.

| Route | Method | Scope |
| --- | --- | --- |
| `/v1/metrics` | GET | every workspace the principal owns |
| `/v1/metrics/stream` | GET | the same, as `text/event-stream` |
| `/v1/workspaces/{id}/metrics` | GET | one workspace |
| `/v1/workspaces/{id}/metrics/stream` | GET | one workspace, SSE |

A snapshot frame:

```json
{"timestamp":"2026-11-03T20:49:20Z","interval_seconds":3,
 "workspaces":[{"workspace_id":"ws_...","ram_used_bytes":17870848,
   "ram_limit_bytes":536870912,"ram_percent":3.32,"ram_complete":true,
   "disk_used_bytes":491520,"disk_limit_bytes":268435456,"disk_percent":0.18,
   "disk_complete":true,"write_rate_limit_mb_s":0,"live_runs":2,"live_cgroups":2}],
 "sampler":{"ticks":3,"cgroup_reads":9,"statfs_reads":3,"tree_walk_reads":0,
   "subscribers":1,"running":1,"dropped_frames":0,"candidates":1}}
```

- `ram_limit_bytes` is the **enforced** ceiling (the workspace's `memory_mb`), and
  `ram_used_bytes` is the sum over the workspace's live run and session cgroups,
  so `ram_percent` never exceeds 100.
- `*_complete` false means that figure is a lower bound (a cgroup or mount read
  did not succeed), not a measurement.
- **A workspace with no live workload reports `ram_used_bytes: 0` — that is a
  real state, not an error.**
- The SSE stream sends one `data:` frame per tick. Cadence comes from
  `/v1/meta.metrics.interval_seconds`; `/v1/meta.metrics.min_interval_sec` is an
  **enforced floor**. A stream request with `?interval=` below the floor is
  refused with `422 unprocessable` (never silently raised). `max_interval_sec`
  is the ceiling. `include_cpu` adds `cpu_usage_usec`/`pids_current`;
  `max_subscribers` caps concurrent streams (past it, `503 unavailable`).
- `/v1/meta.metrics` also carries `mechanism` and `limits`: disk is a single
  `statfs` (never a tree walk), RAM is the sum over per-run/per-session cgroups,
  the sampler is lazy (cost is zero with no subscriber), and each stream holds at
  most one pending frame (coalesced, `dropped_frames` counts drops).

---

## 7. Finding what a workspace is listening on

```http
GET /v1/workspaces/{id}/ports
```

```json
{"workspace_id":"ws_...","count":1,"complete":true,"notes":[],
 "pids_scanned":2,"fds_scanned":8,
 "ports":[{"port":18090,"protocol":"tcp","address":"0.0.0.0","scope":"wildcard",
   "state":"LISTEN","attributed":true,"pid":890648,"kind":"run",
   "label":"python3 -c import socket,time ...","argv":["/bin/python3","-c","..."],
   "owners":[890648],"inode":2726317911}],
 "unix":[{"path":"/tmp/demo.sock","type":"stream","state":"unconnected",
   "attributed":true,"pid":890654,"kind":"run","label":"python3 -c ...",
   "inode":2726357326}],
 "unix_count":1,
 "reachability":"ports are listening inside the workspace's private network namespace; ..."}
```

Per-entry fields:

- `port`, and `protocol` — `tcp`, `tcp6`, `udp` or `udp6` (`ipv6:true` marks the
  v6 variants).
- `address`, and `scope` — `loopback` (`127.0.0.1`/`::1`), `wildcard`
  (`0.0.0.0`/`::`), or `specific` (a concrete interface address).
- `state` — `LISTEN` for TCP. **A UDP socket has no `LISTEN` state**, so `state`
  is the empty string for UDP.
- `attributed` — whether the owning process was resolved. **A socket with no
  discoverable owner has `attributed:false`** and is still reported (never
  dropped, never guessed); `pid`/`kind`/`label`/`argv`/`owners` are then absent.
- `pid`, `kind` (`run`/`session`/`exec`/`detached`), `label` (a short
  `argv[0]`-basename + args string), `argv`, `owners` (all holders), `inode`.

**Unix sockets come back in a separate `unix` section** (with `unix_count`),
never mixed into `ports`, because a unix socket is not a reachable port.

`complete` is `false` when the bounded scan hit a cap (`notes` names the bound);
`pids_scanned`/`fds_scanned` report the bounded work done.

> **These ports are inside the workspace's own network namespace. They are NOT
> published to the internet.** Every response carries a `reachability` note
> saying so. To make a port public, create a tunnel (`POST /tunnels`, §8.5), which
> requires both `limits.allow_tunnels` and `tunnels.enabled`. Never tell a user
> their app is public merely because it appears in `/ports`.

---

## 8. Lifecycle and time

### 8.1 `stop`, `suspend`, `resume`, `DELETE`

```http
POST /v1/workspaces/{id}/stop      # kill all runs, sessions and detached processes; stay active
POST /v1/workspaces/{id}/suspend   # set suspended, stop runs/sessions/tunnels; keep data
POST /v1/workspaces/{id}/resume    # back to active
DELETE /v1/workspaces/{id}         # remove forever → 204
```

`stop` returns `200` with `{workspace, stopped_run_ids, stopped_run_count,
stopped_session_ids, stopped_session_count, stopped_process_ids,
stopped_process_count}` and leaves `status:"active"`. `suspend` returns `200`
with `{workspace, changed, stopped_run_ids, stopped_run_count}`; a suspended
workspace **refuses all work** with `409 workspace_suspended` but stays readable
and can be resumed. `resume` returns `200` with `{workspace, changed}`. All three
are idempotent (`changed:false` when nothing changed).

### 8.2 Delete

`DELETE` returns `204`, tears the workspace down (stop compute, unmount its disk,
remove metadata and network) and is the only way to reclaim everything. A later
`GET` returns `404`.

### 8.3 Subscriptions: the term grammar

A workspace's term is granted at create with `subscription` or extended later:

```http
POST /v1/workspaces                       {"name":"x","subscription":{"duration":"1d12h"}}
POST /v1/workspaces/{id}/subscription/extend   {"duration":"2m"}
POST /v1/workspaces/{id}/subscription/extend   {"expires_at":"2026-12-31T00:00:00Z"}
```

`duration` and `expires_at` are mutually exclusive. The duration grammar is
`<number><unit>` pairs summed, units `s m h d w mo y`, e.g. `30d`, `12h30m`,
`1d12h`. A bare integer (`"30"`) is refused. **`mo` = 30 days and `y` = 365 days
are fixed-length approximations, never calendar-exact** — if a product needs a
real calendar month, send an absolute `expires_at` instead.

### 8.4 Expiry semantics — what actually happens

The engine stores an **absolute expiry instant** (`expires_at`) and derives the
state from it; the client owns its own calendar. Extending while the term is
**live** *adds* to the expiry (an early renewer keeps the whole paid term), and an
`expires_at` earlier than the current one is refused with
`422 subscription_would_shorten`. Extending after it lapsed **starts from now and
unlocks** the workspace. A create/extend beyond `subscriptions.max_duration` is
refused with `422 max_duration_exceeded`.

When the term elapses, the reaper:

1. **Locks** the workspace (`status:"locked"`, distinct from `suspended`),
   stopping runs, sessions and detached processes, releasing tunnels, and
   unmounting the disk — **every byte of user data is kept**. While locked, work
   is refused with `403 subscription_expired`, but **reads, the summary, usage,
   file reads and manual `DELETE` stay allowed**.
2. After the grace period (`subscriptions.grace_period`, default 168 h) it
   **deletes** the workspace entirely.

The workspace also reads as `locked` before the reaper has run, because the state
is derived: any request after `expires_at` sees it locked. `summary.subscription`
carries `{state, expires_at, remaining_sec, grace_ends_at, extensions,
locked_at}`; `remaining_sec` is clamped at ≥ 0 and rounded up, so `0` means
locked. A workspace created with no term is `state:"unlimited"` and never expires.
Default behaviour (no `subscription` and empty `default_duration`) is **no
expiry**.

### 8.5 Tunnels (optional, gated)

```http
POST /v1/workspaces/{id}/tunnels   {"port":18090}
```

Returns `201` with `{tunnel:{id, workspace_id, port, url, status, created_at}}`.
The `url` is a real `https://<name>.trycloudflare.com` address captured from
`cloudflared`; the engine never fabricates one — if none is observed within
`tunnels.url_timeout_sec` the create fails with `504 tunnel_url_unavailable`
(including Cloudflare rate-limiting, `429`). A tunnel requires **both**
`limits.allow_tunnels` and `tunnels.enabled` (and `allow_net`); otherwise
`422 tunnels_disabled`. List with `GET /tunnels`, get one with
`GET /tunnels/{tunnelId}`, remove with `DELETE /tunnels/{tunnelId}` (`204`). The
URL is **ephemeral** — re-fetch it; never cache it.

---

## 9. Failures and readiness — the table error handling is built from

### 9.1 Liveness and readiness

```http
GET /healthz   → 200 {"status":"ok"}                     # the process is up
GET /readyz    → 200 {"status":"ready", ...} | 503 {"status":"not_ready", ...}
```

`/readyz` reports the engine's capabilities truthfully: `nsjail`, `cgroups`,
`data_dir`, `network`, `tunnels`, `identity_masking`, `provider_tools_hidden`,
`profiles`, `egress_decoy`, `workspace_limits`, each with `enabled`/`ready` and a
`reason` when not. Use it as a liveness/readiness probe.

**A readiness check and a workspace's ability to run are not the same
question.** `/readyz` reports the engine's capabilities; an individual run can
still be refused with **`503` and code `unavailable`, carrying a message that
names the real cause** (for example `"sandbox unavailable: nsjail could not set up
or launch the sandbox"`, with a `nsjail` detail, or
`"host-identity masking unavailable: ..."`). **Surface that message to the
operator/user; do not replace it with your own generic "engine not ready" text,
or the real reason is lost.**

### 9.2 Error envelope

Every error is:

```json
{"error":{"code":"...","message":"...","details":{"field":"..."}}}
```

`message` is human-readable and safe to log; `code` is the stable machine value;
`details` is optional and names the offending field/value.

### 9.3 The codes

| Status | Code | Meaning | What to tell the user | Retry? |
| --- | --- | --- | --- | --- |
| 400 | `validation` | The request body/params are malformed or missing a required field. | "Something in the request was invalid." Log `details`. | No — fix the request. |
| 401 | `unauthorized` | Missing or invalid API key. | (Internal.) Your service key is wrong. | No — fix the key. |
| 403 | `forbidden` | The principal does not own the workspace. | "That resource is not yours." | No. |
| 403 | `subscription_expired` | The workspace's term lapsed and it is locked. | "Your plan expired; renew to continue." | After renewal (`subscription/extend`). |
| 404 | `not_found` | No such workspace/run/session/file/route. Reads as absent. | "That item no longer exists." | No. |
| 404 | `ports_disabled` | Port discovery is switched off. | Hide the feature. | No. |
| 405 | `validation` | Method not allowed on a known route. | (Internal.) | No — fix the client. |
| 409 | `conflict` | A conflicting state exists (generic). | "That is not possible right now." | Sometimes. |
| 409 | `workspace_suspended` | Work on a suspended workspace. | "This workspace is paused." | After `resume`. |
| 409 | `too_many_sessions` | The per-workspace session cap is reached. | "Close a terminal first." | After closing one. |
| 409 | `session_ended` | Input/signal/resize to an ended session. | "That terminal has closed." | No — create a new session. |
| 409 | `archive_entry_exists` | Extraction would overwrite; `overwrite:false`. | "A file already exists; overwrite?" | With `overwrite:true`. |
| 413 | `payload_too_large` | Body/upload exceeds a cap. | "That file is too large." | No — shrink it. |
| 413 | `archive_too_large` | Uncompressed size exceeds `max_total_mb`. | "Archive is too large to extract." | No. |
| 413 | `archive_too_many_entries` | More entries than `max_entries`. | Same. | No. |
| 413 | `archive_ratio_exceeded` | Compression ratio over `max_ratio` (bomb guard). | Same. | No. |
| 415 | `unsupported_archive` | Content is not a supported archive format. | "That is not a zip or rar." | No. |
| 422 | `unprocessable` | A well-formed request the engine refuses (limits above maxima, bad range, unbounded runs not permitted, signals, etc.). | Read `message`/`details`. | No — fix the request. |
| 422 | `unknown_profile` | The named profile is not offered. | "Pick one of the offered profiles." | No. |
| 422 | `not_directory` | A path is not a directory (or vice versa). | Read `message`. | No. |
| 422 | `session_needs_interpreter` | The profile cannot host the PTY relay. | "Terminals need the python profile." | No. |
| 422 | `subscription_invalid` | Unparseable/zero/negative duration, non-future `expires_at`, or both/neither given. | Read `message`/`details`. | No — fix the term. |
| 422 | `max_duration_exceeded` | The term would exceed `subscriptions.max_duration`. | "That term is too long." | No. |
| 422 | `subscription_would_shorten` | An `expires_at` earlier than a live term. | "Use `duration` to add time." | No. |
| 422 | `tunnels_disabled` | Tunnels not permitted (workspace or server switch off; or no `allow_net`). | "Tunnels are off for this workspace." | No. |
| 422 | `too_many_tunnels` | Would exceed `tunnels.max_per_workspace`. | "Delete a tunnel first." | After deleting one. |
| 422 | `archive_encrypted` | The archive needs a password. | "That archive is encrypted." | No. |
| 422 | `archive_multivolume` | A multi-volume RAR (`partN.rar`). | "Multi-part archives are not supported." | No. |
| 422 | `archive_unsafe_entry` | An entry escapes the destination or is an unsafe link/bits. | "That archive contains unsafe entries." | No. |
| 429 | `write_rate_exceeded` | The engine's own write path would exceed the workspace's write-rate ceiling. | "Slow down; retry shortly." | Yes, after a pause. |
| 500 | `internal` | An unexpected server error. | "Something went wrong; try again." | Maybe. |
| 503 | `unavailable` | The sandbox could not start, nsjail/masking is unavailable, or too many metrics subscribers. **`message` names the cause.** | Show the engine's `message`. | Yes (transient). |
| 504 | `tunnel_url_unavailable` | cloudflared did not produce a URL within the timeout (includes Cloudflare `429`). | "Could not publish; try again." | Yes. |
| 507 | `insufficient_storage` | A write exceeded the workspace's disk allowance. | "The workspace is out of disk." | After deleting files / raising `disk_mb`. |

The `503 unavailable` row is the sharpest edge: the run's own refusal carries the
real reason, and a client must **surface that message** rather than substitute a
generic one.

---

## 10. What a user can and cannot see

A short, factual section for writing correct UI copy. The full reasoning is in
[`docs/DECISIONS.md`](DECISIONS.md), [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) and
[`docs/EGRESS-DECOY.md`](EGRESS-DECOY.md).

- **The workspace sees its own disk and its own memory and CPU, and the reported
  number is the enforced number.** `df`/`stat -f`/`os.statvfs()` report the
  workspace's own allowance, and a write past it fails with `ENOSPC` at the
  boundary (when `workspace_limits.mechanism` is `fuse`). `free`/`/proc/meminfo`
  report its own memory. What a user reads is what the kernel enforces.
- **The platform's identity is masked.** The hostname, kernel release/version,
  volume identity and network identity are not the host's; `uname`/`sysconf`/
  `sysinfo` answer with the workspace's own limits. The honest limits (a
  statically-linked, musl, raw-syscall or handle-scoped-`dlvsym` binary can bypass
  the shim) are documented — it is defense-in-depth, not a security boundary.
- **Whether a workspace reaches the internet depends on its `allow_net` limit.**
  With `allow_net:false` (the default) the workspace has a private empty network
  namespace. With `allow_net:true` and `network.enabled`, it gets isolated egress
  through chained slirp4netns — it can reach the public internet but not the host
  LAN, the host's services, or another workspace.
- **When egress is on, the public address a workspace presents is the platform's**
  (the hosting provider's shared NAT), not an address belonging to the user.
  `egress_decoy` is a **decoy layer, not a security boundary**: it answers a
  workspace's attempts to learn its own IP with a consistent invented identity,
  but a tenant who controls their own endpoint reads the real peer address in
  their own logs, and hardcoded-IP endpoints, traceroute hops and MTU/ICMP
  fingerprinting are out of scope. Do not describe it to users as anonymity or a
  VPN.

---

## Appendix A — route index (every documented path)

For completeness, every path in `api/openapi.yaml`. Methods follow the table
above and [`docs/API.md`](API.md).

- `/healthz` (`GET`)
- `/readyz` (`GET`)
- `/metrics` (`GET`, Prometheus)
- `/v1/meta` (`GET`)
- `/v1/metrics` (`GET`)
- `/v1/metrics/stream` (`GET`, SSE)
- `/v1/workspaces` (`GET`, `POST`)
- `/v1/workspaces/{workspaceId}` (`GET`, `PATCH`, `DELETE`)
- `/v1/workspaces/{workspaceId}/suspend` (`POST`)
- `/v1/workspaces/{workspaceId}/resume` (`POST`)
- `/v1/workspaces/{workspaceId}/stop` (`POST`)
- `/v1/workspaces/{workspaceId}/subscription/extend` (`POST`)
- `/v1/workspaces/{workspaceId}/usage` (`GET`)
- `/v1/workspaces/{workspaceId}/metrics` (`GET`)
- `/v1/workspaces/{workspaceId}/metrics/stream` (`GET`, SSE)
- `/v1/workspaces/{workspaceId}/processes` (`GET`)
- `/v1/workspaces/{workspaceId}/processes/{processId}/stop` (`POST`)
- `/v1/workspaces/{workspaceId}/ports` (`GET`)
- `/v1/workspaces/{workspaceId}/packages` (`GET`, `DELETE`)
- `/v1/workspaces/{workspaceId}/packages/install` (`POST`)
- `/v1/workspaces/{workspaceId}/tunnels` (`GET`, `POST`)
- `/v1/workspaces/{workspaceId}/tunnels/{tunnelId}` (`GET`, `DELETE`)
- `/v1/workspaces/{workspaceId}/files` (`GET`, `DELETE`)
- `/v1/workspaces/{workspaceId}/files/stat` (`GET`)
- `/v1/workspaces/{workspaceId}/files/content` (`GET`, `PUT`)
- `/v1/workspaces/{workspaceId}/files/download` (`GET`)
- `/v1/workspaces/{workspaceId}/files/mkdir` (`POST`)
- `/v1/workspaces/{workspaceId}/files/rename` (`POST`)
- `/v1/workspaces/{workspaceId}/files/upload` (`POST`)
- `/v1/workspaces/{workspaceId}/files/archive` (`GET`, `POST`)
- `/v1/workspaces/{workspaceId}/files/extract` (`POST`)
- `/v1/workspaces/{workspaceId}/exec` (`POST`)
- `/v1/workspaces/{workspaceId}/runs` (`GET`, `POST`, `DELETE`)
- `/v1/workspaces/{workspaceId}/runs/{runId}` (`GET`, `DELETE`)
- `/v1/workspaces/{workspaceId}/runs/{runId}/stop` (`POST`)
- `/v1/workspaces/{workspaceId}/runs/{runId}/logs` (`GET`)
- `/v1/workspaces/{workspaceId}/sessions` (`GET`, `POST`)
- `/v1/workspaces/{workspaceId}/sessions/{sessionId}` (`GET`, `DELETE`)
- `/v1/workspaces/{workspaceId}/sessions/{sessionId}/input` (`POST`)
- `/v1/workspaces/{workspaceId}/sessions/{sessionId}/signal` (`POST`)
- `/v1/workspaces/{workspaceId}/sessions/{sessionId}/resize` (`POST`)
- `/v1/workspaces/{workspaceId}/sessions/{sessionId}/output` (`GET`)

Supporting routes used across the flows and not in the list above:
`/v1/workspaces/{workspaceId}/processes` (list) and
`/v1/workspaces/{workspaceId}/runs/{runId}/stop`, `/runs/{runId}/logs` (above),
and the `DELETE /runs` / `DELETE /runs/{runId}` purge routes.

## Appendix B — version

This guide documents engine version **0.0.20** (`model.Version`); the HTTP
contract prefix is `v1` (`model.APIVersion`).
