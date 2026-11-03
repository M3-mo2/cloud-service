# API guide

Human-friendly, self-sufficient reference for the sandboxd HTTP/JSON API
(v0.0.18). The machine-readable contract is
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
| `not_found` | 404 | Resource missing | Unknown workspace/run/file, unknown route. Also `ports_disabled` when port discovery is switched off (`ports.enabled: false`), so the surface reads as absent. |
| `conflict` | 409 | Operation conflicts with current state | Purging run history (or deleting one run) while a run is still queued/running. |
| `tunnels_disabled` | 422 | Quick tunnels forbidden | `POST .../tunnels` when `tunnels.enabled` is false (`details.config:"tunnels.enabled"`), the workspace has not opted in (`limits.allow_tunnels`), or the workspace has no egress (`limits.allow_net`), which cloudflared needs. |
| `too_many_tunnels` | 422 | Per-workspace tunnel cap reached | `POST .../tunnels` when `tunnels.max_per_workspace` active tunnels already exist. |
| `unknown_profile` | 422 | A workspace create named a profile that is not offered | `POST /v1/workspaces` with `profile` set to a name not in `GET /v1/meta`'s `profiles` list. The name is preserved in `details.profile`; it is never silently replaced by the default. |
| `session_needs_interpreter` | 422 | A terminal session was requested for a profile without Python | `POST .../sessions` on a workspace whose profile does not provide `python3` (e.g. `minimal`). The PTY relay that backs a session is a Python program, so a shell-only profile cannot host one. |
| `workspace_suspended` | 409 | Work refused because the workspace is suspended | `POST .../exec`, `POST .../runs`, `POST .../sessions` or a session sub-resource on a `suspended` workspace (a manual operator action). |
| `subscription_expired` | 403 | Work refused because the subscription term lapsed and the workspace is `locked` | `POST .../exec`, `.../runs`, `.../sessions`, `.../tunnels`, `.../packages/install` and every **mutating** `files` operation (`PUT /files/content`, `DELETE /files`, `POST /files/mkdir`, `POST /files/rename`, `POST /files/upload`) on a `locked` workspace. Reads, `GET /usage`, the summary and `DELETE /v1/workspaces/{id}` stay allowed; renew with `POST .../subscription/extend`. |
| `subscription_invalid` | 422 | A subscription create/extend is malformed | A create/extend whose `duration` is zero, negative, unparseable or missing a unit (the grammar is `s m h d w mo y`, summed, e.g. `1d12h`; a bare integer is refused), whose `expires_at` is not in the future, that supplies both `duration` and `expires_at`, or that supplies neither. `details.field` names the offender. |
| `max_duration_exceeded` | 422 | A subscription would run past the server ceiling | A create/extend whose expiry would land more than `subscription.max_duration` (default `365d`) in the future. |
| `subscription_would_shorten` | 422 | An extension tried to shorten a live term | `POST .../subscription/extend` with an absolute `expires_at` earlier than the still-live `expires_at`. The engine never silently shortens a paid term; a duration extension is always additive while live. |
| `too_many_sessions` | 409 | Per-workspace concurrent-session cap reached | `POST .../sessions` when `sessions.max_per_workspace` is already in use. |
| `session_ended` | 409 | Operation on a terminal session | `input`/`signal`/`resize` (or `output` after `DELETE`) on a session that has ended; the workspace check wins while suspended. |
| `validation` | 400 | Malformed or invalid request | Bad JSON, unknown JSON field, failing `validate` tag, missing query/body field, bad `stream`/`status`/`kind` filter, unknown route body. Also carries the **405** status for method-not-allowed. |
| `unprocessable` | 422 | Unsafe path or invalid limit/target | Path traversal / symlink escape, `cwd` escape or a `cwd` that is missing / not a directory, command not found, deleting the workspace root, file-run target that is a directory or an unsupported extension with no interpreter, an interpreter outside the allowlist, `argv` supplied together with `kind:file`, a `no_timeout` request forbidden at the server or workspace level, a limit `<= 0` or **above the server maximum**, conflicting byte/line range parameters, a request `limit` above `file_read_mb`. |
| `not_directory` | 422 | Wrong node type for the operation | Listing a path that is a file, reading/writing/downloading a directory as a file, writing over a directory. The path exists, so this is never 404 (see discrepancies 9). |
| `payload_too_large` | 413 | Body or file over a hard cap | JSON body > `json_body_mb`, upload > `upload_mb`, file write > `file_write_mb`, a `GET /files/content` `limit` above `file_read_mb`. |
| `insufficient_storage` | 507 | Workspace disk quota exceeded | Write/upload/rename past `limits.disk_mb`. |
| `write_rate_exceeded` | 429 | The workspace's sustained write rate exceeds its ceiling | The engine's **own** write path (`PUT /files/content`, `POST /files/upload`, `POST /files/archive`, `POST /files/extract`) on a workspace whose disk has grown faster than `limits.write_rate_mb_s` for N consecutive samples, while `disk.write_rate_enabled` is true. Distinct from `insufficient_storage` (the disk is not full). The SAMPLED ceiling; a tenant's own process is stopped with the `rate_limited` run status instead (see [Write rate](#write-rate)). |
| `timeout` | 504 | Timeout | **Defined but never emitted** (see discrepancies). |
| `tunnel_url_unavailable` | 504 | No public URL was observed in time | `POST .../tunnels` when cloudflared did not print a `trycloudflare.com` URL within `tunnels.url_timeout_sec`, including when Cloudflare's rate-limited quick-tunnel provisioning returns `429` (cloudflared's own text is in `details.reason`). The engine never fabricates a URL. |
| `unavailable` | 503 | Sandbox backend unavailable | nsjail binary missing, or nsjail could not set up/launch the sandbox (`details.nsjail`); also when the network/tunnel layer is not configured. |
| `unsupported_archive` | 415 | The content matches no supported archive format | An archive request whose **content** (magic bytes) is not a known ZIP/RAR, or a detected format the `archives.formats` allowlist disables. `details.signature` names the leading bytes. The filename is never trusted. |
| `archive_encrypted` | 422 | The archive is encrypted | A ZIP (general-purpose flag bit 0/6) or a RAR archive/entry that needs a password. The engine holds no password, so it never emits possibly-garbage plaintext. |
| `archive_multivolume` | 422 | The archive is multi-volume | A RAR `partN.rar` that continues in another file. The engine extracts single-volume archives only; it refuses rather than truncating. |
| `archive_too_large` | 413 | A size cap would be exceeded | An archive whose total uncompressed bytes exceed `archives.max_total_mb`, or a single entry above `archives.max_file_mb`. Enforced by a streaming budget, so a lying header cannot overshoot. |
| `archive_too_many_entries` | 413 | Entry-count cap exceeded | An archive with more entries than `archives.max_entries`. |
| `archive_ratio_exceeded` | 413 | Compression-ratio cap exceeded | An entry whose uncompressed/compressed ratio exceeds `archives.max_ratio` (a zip-bomb guard). `details.entry` names it. |
| `archive_unsafe_entry` | 422 | An entry would escape the destination or is a link | An entry whose name is `..` traversal, absolute, a drive letter or a duplicate collision; a symlink or hardlink entry; or any entry the workspace path cleaner rejects. **Nothing is written.** `details.entry` names it. |
| `archive_entry_exists` | 409 | An entry would clobber an existing file | An archive request with `overwrite:false` (the default) where an entry's target already exists. The existing content is left intact; use `overwrite:true` to replace. `details.entry` names it. |
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
  "tunnels":{...},"identity_masking":{"enabled":bool,"ready":bool,"reason":str},
  "provider_tools_hidden":{"enabled":bool,"ready":bool,"reason":str},
  "workspace_limits":{"enabled":bool,"mechanism":str,"proc":bool,"proc_files":[str],"reason":str},
  "egress_decoy":{"enabled":bool,"ready":bool,"reason":str}}`.
- **Errors:** none; unsuitability is expressed by the 503 and the boolean fields.
- **Semantics:** `data_dir` is true when a temp file can be created under the
  data directory; `nsjail` when the nsjail binary is found; `cgroups` when the
  cgroup v2 hierarchy at `/sys/fs/cgroup` is writable by the process (requires a
  container started with `--cgroupns=host`). Any false ⇒ 503. When
  `identity_masking.enabled` is true, `ready` reports whether the host-identity
  masking shim can be applied: enabled-but-not-ready makes the whole probe 503,
  so a deploy that would leak the host identity is never reported ready. When
  `provider_tools_hidden.enabled` is true, `ready` reports whether the curated
  `/usr/local` and `/etc/ssl/certs` overlays could be materialized; here too,
  enabled-but-not-ready makes the probe 503, so a deploy that would expose the
  platform's own tooling is never reported ready. When `egress_decoy.enabled` is
  true, `ready` reports whether the decoy CA and profile materialized;
  enabled-but-not-ready makes the probe 503, so a deploy that would serve the
  real identity is never reported ready. See `docs/EGRESS-DECOY.md` and ADR 84.
  `workspace_limits` reports how a workspace's own disk allowance and memory are
  made visible and enforced: `enabled` is the `disk.enabled` switch, `mechanism`
  is the effective backend (`fuse`, `counter`, or `none`), `proc` is whether the
  curated per-workspace `/proc` is served and `proc_files` lists exactly what it
  carries. The two-workspace differentiation and the enforcement proof are in
  `docs/VERIFICATION.md` §14 and ADR 85/86.

```sh
curl -s localhost:8080/readyz
# {"cgroups":true,"data_dir":true,"identity_masking":{"enabled":true,"ready":true},
#  "nsjail":true,"provider_tools_hidden":{"enabled":true,"ready":true},"status":"ready"}
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
    "version": "0.0.20",
    "api_version": "v1",
    "runtimes": ["python3"],
    "subscriptions": {
      "enabled": true,
      "default_duration": "",
      "max_duration": "8760h0m0s",
      "grace_period": "168h0m0s",
      "reap_interval": "60s",
      "duration_grammar": "s m h d w mo y (summed, e.g. 1d12h); mo=30d and y=365d are FIXED-LENGTH approximations, never calendar-exact",
      "expiry_semantics": "absolute instants; extend adds while live, restarts from now when lapsed and unlocks; a live term is never shortened",
      "locked_status": "locked (automatic, lapsed term) is distinct from suspended (manual); refused work is 403 subscription_expired, reads/usage/delete stay allowed",
      "summary_present_on": ["GET /v1/workspaces/{id}", "GET /v1/workspaces"]
    },
    "archives": {
      "enabled": true,
      "formats": ["zip", "rar"],
      "read_only": ["rar"],
      "limits": {"max_entries": 20000, "max_total_mb": 2048, "max_file_mb": 512,
                 "max_ratio": 200, "upload_mb": 50, "overwrite": false, "skip_junk": true},
      "routes": ["POST /v1/workspaces/{id}/files/archive",
                 "POST /v1/workspaces/{id}/files/extract",
                 "GET /v1/workspaces/{id}/files/archive"],
      "refusal_codes": ["unsupported_archive", "archive_encrypted", "archive_multivolume",
                        "archive_too_large", "archive_too_many_entries",
                        "archive_ratio_exceeded", "archive_unsafe_entry", "archive_entry_exists"],
      "semantics": "extraction validates every entry before writing (a refused archive leaves no partial tree); an entry that escapes the destination, is a symlink/hardlink, or sets a privileged bit is refused, and the bytes written are bounded by a streaming budget so a lying header cannot overshoot; overwrite:false (the default) refuses to clobber an existing file with 409 archive_entry_exists"
    },
    "ports": {
      "enabled": true,
      "max_pids": 64,
      "max_fds_per_pid": 512,
      "include_unix": true,
      "mechanism": "on-demand read of /proc/<pid>/net/{tcp,tcp6,udp,udp6,unix} for a pid inside the workspace's own namespace, with socket-inode -> owning-process attribution over the workspace's process registry; never cached and never on the metrics tick",
      "routes": ["GET /v1/workspaces/{id}/ports"],
      "reachability": "a reported port is listening inside the workspace's private network namespace; it is NOT reachable from outside unless a tunnel publishes it (limits.allow_tunnels AND tunnels.enabled)",
      "default_note": "enabled by default: read-only introspection of the tenant's own workspace, which the tenant cannot see from inside (no /proc/net, no ss/netstat)",
      "reads_total": 0
    },
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
      "mechanism": "ld.so.preload shim (uname/sysinfo/sysconf/sched_getaffinity/getauxval/dlsym, plus a filesystem interposer that hides /etc/ld.so.preload, the shim itself and the legacy engine dirs from the workspace)",
      "limits": "a statically-linked binary, a musl runtime, a raw syscall(2), or a handle-scoped dlvsym(3) bypasses the shim; the shim and its preload list are invisible to the workspace (ADR 79), and only a raw syscall(2) additionally reaches /etc/ld.so.preload"
    },
    "provider_tools_hidden": {
      "enabled": true, "ready": true,
      "mechanism": "profile-materialized read-only /usr (also bound at /bin, /sbin, /lib, /lib64), plus filtered /etc/{passwd,group,ssl/certs}",
      "limits": "a workspace sees only the tools its profile names (an explicit allowlist) plus engine base commands, shared libraries and loaders; a profile entry naming the engine/platform is refused (fail loud), and it cannot change the provider the egress address is registered to"
    },
    "egress_decoy": {
      "enabled": false, "ready": false,
      "reason": "network.decoy.enabled is false",
      "mechanism": "in-namespace stub DNS resolver + stub HTTP/TLS responder, one consistent per-workspace decoy profile, with a generated CA trusted inside the workspace and in-namespace nft rules forcing DNS to the stub",
      "limits": "a decoy layer, NOT a security boundary: a tenant's OWN endpoint (they read the peer address in their own logs) is unfixable; a hardcoded-IP endpoint outside the covered set, intermediate traceroute hops, and MTU/ICMP fingerprinting are out of scope; a knowledgeable tenant can recognise a documentation-range address"
    },
    "workspace_limits": {
      "enabled": true, "mechanism": "fuse",
      "reporting": "real per-workspace ext4 filesystem image (fuse2fs), host-side mounted; df/stat -f/os.statvfs are the filesystem's own answers",
      "enforcement": "kernel filesystem boundary: a write past the allowance fails with ENOSPC, no overshoot",
      "limits": "one image file and one fuse2fs process per workspace; FUSE throughput overhead (measured ~0.4x native write, ~0.3x read)"
    },
    "profiles": {
      "ready": true,
      "default": "python",
      "profiles": [
        {"name":"minimal","description":"POSIX shell, coreutils and the common text/file utilities only. No language runtime.",
         "languages":[],"provides":["sh","bash","coreutils","findutils","grep","sed"],
         "commands":["bash","cat",", ..."],"paths":[],"notes":"shell-only profile. ..."},
        {"name":"python","description":"Python 3.12 with pip/venv, plus Node.js, Perl, git and common shell tools.",
         "languages":["python","node"],"provides":["python","pip","node","git","coreutils","shell"],
         "commands":["bash","cat",", ..."],"paths":["/usr/lib/python3.12",", ..."],"notes":"..."}
      ]
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
  cover (a statically-linked, musl, raw-`syscall(2)` or handle-scoped-`dlvsym(3)`
  binary bypasses it). `provider_tools_hidden` reports the provider-tool overlay
  (`identity.hide_provider_tools`, default true): `enabled` is the switch,
  `ready` whether the profile trees and `/etc/ssl/certs` were materialized, and
  `limits` states that a workspace sees only its profile's allowlist (an entry
  naming the engine/platform is refused, fail loud) but that it cannot change the
  provider the egress address is registered to (see
  [DECISIONS.md](DECISIONS.md) ADR 74/76/83).
  `egress_decoy` reports the egress-decoy capability (`network.decoy.enabled`,
  default false): `enabled` is the switch, `ready` whether the decoy CA and
  profile materialized, and `mechanism`/`limits` state that a workspace's
  IP-discovery is answered with one consistent invented identity and that it is a
  **decoy, not a security boundary** (a tenant's own endpoint, traceroute hops,
  and hardcoded-IP endpoints are out of scope). See
  [EGRESS-DECOY.md](EGRESS-DECOY.md) and [DECISIONS.md](DECISIONS.md) ADR 84.
  `workspace_limits` reports the per-workspace resource visibility and
  enforcement (`disk.enabled`, default true): `mechanism` is the effective disk
  backend (`fuse` = a real per-workspace ext4 filesystem, kernel boundary;
  `counter` = the engine's used-bytes watchdog, real but **sampled**; `none` =
  neither reported nor enforced), `reporting`/`enforcement`/`limits` state what
  each backend does and its cost, and `reason` explains a fallback. See
  [DECISIONS.md](DECISIONS.md) ADR 85/86 and `docs/VERIFICATION.md` §14.
  `profiles` reports the tool profiles a client can choose from: `default` is the
  profile a workspace gets when its create omits one, `ready` is whether the
  registry loaded and the default exists (`reason` explains a `ready:false`), and
  `profiles` lists each offered profile with `name`, `description`, `languages`,
  `provides`, the full `commands` allowlist, the attached data/library `paths`,
  and free-form `notes`. A profile is data (a YAML file under `configs/profiles/`
  embedded in the binary, overridable via `profiles.dir`): adding a language means
  adding a file, never editing the engine. See
  [Workspace profiles](#workspace-profiles).
  `subscriptions` reports the workspace-subscription capability:
  `enabled` is whether the reaper loop runs (`subscription.enabled`),
  `default_duration` is the term a create gets with no `subscription` (empty =
  **no expiry**, unlimited), `max_duration`/`grace_period`/`reap_interval` are the
  configured bounds, `duration_grammar` is the accepted grammar (and repeats that
  `mo`/`y` are fixed-length approximations), `expiry_semantics` states the
  additive-while-live / from-now-when-lapsed / never-shorten rules, and
  `locked_status` states that `locked` is automatic (distinct from `suspended`),
  that refused work is `403 subscription_expired`, and that reads/usage/delete
  stay allowed. See [Subscriptions](#subscriptions).
  `archives` reports the native-archive capability: `enabled` is the server
  switch, `formats` the content-detected allowlist, `read_only` the formats that
  may be read but never written (`rar`), `limits` the bounds actually in force
  (from the `archives:` config block), `routes` the three archive endpoints,
  `refusal_codes` the stable codes a client can act on, and `semantics` a plain
  statement of the validation-before-write and overwrite rules. See
  [Native archives](#native-archives).

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
  | `profile` | `string` | `profile` | no | must name a profile from `GET /v1/meta`'s `profiles` list | server `profiles.default` (`python`) |
  | `limits` | `*limitsJSON` | `limits` | no | none | server `limits` defaults |
  | `subscription` | `*subscriptionRequestJSON` | `subscription` | no | `duration` XOR `expires_at`, see below | server `subscription.default_duration`; empty means **no expiry** (unlimited) |

  `profile` is a **top-level field**, not under `limits`: the profile selects the
  workspace's toolchain, which is orthogonal to its resource limits. An unknown
  name is **`422 unknown_profile`** (it is never silently replaced by the
  default). The resolved name — including the default when `profile` is omitted
  — is stored on the record and returned by every workspace endpoint.

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
  | `write_rate_mb_s` | `int` | `write_rate_mb_s` | per-workspace write-RATE ceiling (MiB/s), `0` by default (no rate ceiling). A SAMPLED ceiling enforced by stopping a run whose sustained disk growth exceeds it; meaningful only when the server `disk.write_rate_enabled` is true. A value above the workspace's own `disk_mb` is rejected with `422` (a workspace cannot write its whole disk in under a second). See [Write rate](#write-rate). |

  Unknown JSON fields are rejected (`400 validation`, `details.error`).
- **Response 201:**

  ```json
  {"workspace":{"id":"ws_01J...","owner_id":"user_dev","name":"demo",
    "profile":"python",
    "created_at":"2026-01-02T15:04:05Z","status":"active",
    "limits":{"cpu_cores":1,"memory_mb":256,"disk_mb":1024,
              "max_wall_time_sec":300,"max_runs":3,"allow_net":false},
    "subscription":{"granted_at":"2026-01-02T15:04:05Z",
                    "expires_at":"2026-02-01T15:04:05Z",
                    "grace_ends_at":"2026-02-08T15:04:05Z","extensions":0},
    "summary":{"state":"active","age_sec":0,"created_at":"2026-01-02T15:04:05Z",
               "runs":{"total":0,"active":0},
               "disk":{"used_bytes":0,"limit_bytes":1073741824},
               "profile":"python",
               "subscription":{"state":"active","expires_at":"2026-02-01T15:04:05Z",
                               "remaining_sec":2592000,"extensions":0}}}}
  ```

  A workspace created **without** a `subscription` (and with
  `subscription.default_duration` empty) has no `subscription` object and its
  summary has `"state":"unlimited"` and no nested subscription. The `summary`
  object is the cheap derived view (see [Subscriptions](#subscriptions)).
- **Errors:** 400 `validation` (bad JSON / failed tag), 401 `unauthorized`,
  413 `payload_too_large` (body > `json_body_mb`), 422 `unprocessable` (a limit
  above the server maximum), 422 `unknown_profile` (`profile` not in the
  catalogue; `details.profile` carries the requested name), 422
  `subscription_invalid` / `max_duration_exceeded` (a malformed or too-long
  term; see [Subscriptions](#subscriptions)).
- **Semantics:** `allow_net` is always taken from the request when a `limits`
  object is present (so `{"limits":{}}` sets `allow_net` to `false`), and only
  from the server default when `limits` is omitted entirely. Numeric limits are
  bounded by the server-level maxima (`max_limits:` in config; defaults
  `cpu_cores` 8, `memory_mb` 8192, `disk_mb` 51200, `max_wall_time_sec` 3600,
  `max_runs` 64); a request above a maximum is `422 unprocessable`. Limits can
  be changed later with `PATCH`. The **profile is immutable**: it decides which
  tools exist in the workspace, so it is fixed at creation and `PATCH` never
  changes it (a profile change is a new workspace).

```sh
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"demo","limits":{"memory_mb":256,"allow_net":false}}' \
  localhost:8080/v1/workspaces
```

### GET /v1/workspaces

List the caller's workspaces.

- **Response 200:** `{"workspaces":[ <workspace>, ... ]}`, newest first, with
  `status:"active"`, `"suspended"` and `"locked"` entries (only the internal
  `deleted` marker, which deletion never writes because it removes the tree, is
  filtered out). Each item carries the cheap derived `summary` object, so a
  dashboard renders a list in **one** request.
- **Errors:** 401 `unauthorized`.
- **Semantics:** empty result is `{"workspaces":[]}` (never `null`). A
  suspended or locked workspace is still listed; use its `status` field to tell.
  `status:"locked"` is the derived status (the term lapsed), so it can appear
  before the reaper has physically torn the workspace down.

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

### POST /v1/workspaces/{workspaceID}/subscription/extend

Renew (extend) a workspace's subscription term. This is the renewal path.

- **Path parameter:** `workspaceID`.
- **Request body:** exactly one of `duration` or `expires_at` (the same grammar
  as create; a bare integer is refused).

  | Field | JSON | Notes |
  |-------|------|-------|
  | `duration` | `string` | e.g. `"30d"`, `"1d12h"`, `"1mo"`. Summed units `s m h d w mo y`; `mo`=30d and `y`=365d are **fixed-length approximations**, never calendar-exact. |
  | `expires_at` | `string` | an RFC3339 absolute instant (the client owns its calendar). Must be in the future. |

- **Response 200:** `{"workspace": <workspace>}`, the same shape as
  `GET /v1/workspaces/{workspaceID}`, with `subscription.expires_at` advanced and
  `subscription.extensions` incremented.
- **Errors:** 401, 403, 404, 422 `subscription_invalid` /
  `max_duration_exceeded` / `subscription_would_shorten` (`details` carries the
  offending field). Extending a deleted workspace is `404` and **never
  resurrects** it.
- **Semantics — stated and asserted:**
  - **While the term is still live:** the extension **ADDS** to the existing
    expiry (`expires_at += duration`), so a user who renews early keeps the whole
    paid term. An absolute `expires_at` **earlier** than the live one is refused
    (`422 subscription_would_shorten`); a live term is **never shortened**.
  - **After the term has lapsed (`locked`):** the extension starts from **now**
    and **unlocks** the workspace, restoring service (the next `exec`/run works).
  - The new expiry may not land further than `subscription.max_duration` from now
    (`422 max_duration_exceeded`).

```sh
# renew while live: the term grows by 30 days
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"duration":"30d"}' localhost:8080/v1/workspaces/$WS/subscription/extend
# renew with an absolute instant the billing product computed
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"expires_at":"2027-01-01T00:00:00Z"}' localhost:8080/v1/workspaces/$WS/subscription/extend
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
  (`WORKSPACE_ID`/`PROCESS_KIND`/`PROCESS_SOURCE`). A process inside a
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

### GET /v1/workspaces/{workspaceID}/ports

List the **ports the workspace's processes are listening on**, and which process
serves each one. This is what lets a client tell a user "your app is live on port
8080".

- **Path parameter:** `workspaceID`.
- **Response 200:**

  ```json
  {"workspace_id":"ws_01J...",
   "ports":[
     {"port":8080,"protocol":"tcp","address":"127.0.0.1","scope":"loopback",
      "state":"LISTEN","attributed":true,"pid":179173,"kind":"run",
      "label":"python3 -m http.server 8080","argv":["python3","-m","http.server","8080"],
      "owners":[179173],"inode":2704413670},
     {"port":5353,"protocol":"udp","address":"0.0.0.0","scope":"wildcard",
      "state":"","attributed":false}],
   "count":2,
   "unix":[{"path":"/run/app.sock","type":"stream","state":"connected",
            "attributed":true,"pid":179173,"kind":"run"}],
   "unix_count":1,
   "complete":true,"notes":[],"pids_scanned":1,"fds_scanned":12,
   "reachability":"ports are listening inside the workspace's private network namespace; this does not mean they are reachable from outside. Publishing a port requires a tunnel (limits.allow_tunnels and tunnels.enabled)."}
  ```

  | Field | Meaning |
  |-------|---------|
  | `port` | The listening port number. |
  | `protocol` | `tcp`, `tcp6`, `udp` or `udp6`. |
  | `address` | The bind address, decoded from the kernel's hex form (e.g. `127.0.0.1`, `0.0.0.0`, `::1`). |
  | `scope` | `loopback` (`127.0.0.0/8`, `::1`), `wildcard` (`0.0.0.0`, `::`), or `specific`. Decides whether the port is reachable from outside at all. |
  | `state` | `LISTEN` for a TCP listener. A **UDP** socket has **no** listen state, so its `state` is the **empty string** — the engine says so rather than inventing "LISTEN". |
  | `attributed` | `true` when the owning process was resolved. When `false`, the entry is still reported, never dropped and never guessed. |
  | `pid`, `kind`, `label`, `argv`, `owners` | The owning process: its host pid, its `run`/`session`/`detached` kind, a short human label (`argv[0]` basename + args, truncated), its argv, and every candidate pid holding the socket (when more than one). |
  | `inode` | The kernel socket inode, for correlation with a client's own diagnostics. |
  | `unix` | A **separate** array of unix-domain sockets. They are **never** mixed into `ports`, because a unix socket is not a reachable port. |
  | `complete` | `false` (with a `notes` entry) when the bounded scan hit a cap; the list is then a **partial** view, not a total one. |
  | `pids_scanned`, `fds_scanned` | The bounded work actually performed. |
  | `reachability` | The plain statement, always present, that these ports are **inside** the workspace and are **not** thereby reachable from outside. |

- **Errors:** 401 `unauthorized`, 403 `forbidden`, 404 `not_found`, 404
  `ports_disabled` (the feature is switched off with `ports.enabled: false`).
- **Semantics (how it is measured):** the engine reads
  `/proc/<pid>/net/{tcp,tcp6,udp,udp6,unix}` for a pid that is **already inside
  the workspace's own network namespace**, and attributes each socket to its
  owner by matching the socket inode against the workspace's process registry
  (the same registry `GET .../processes` serves, so the two cannot disagree).
  The read is **bounded** (`ports.max_pids`, `ports.max_fds_per_pid`) and
  **live** on every call — nothing is cached, so a port that appears after the
  first call is reported on the next, and one whose process exited disappears.
  The list is sorted deterministically by `(port, protocol)`.
- **Not reachability.** A port here is listening inside the workspace's private
  network namespace. It does **not** mean it is reachable from the internet:
  publishing a port is a separate feature (**tunnels**), which requires
  `limits.allow_tunnels` **and** `tunnels.enabled`. A client must never tell a
  user their app is public merely because it appears here.
- **Isolation:** the scan runs **only** over the workspace's own processes, so a
  port on the **host**, or another tenant's port, can never appear. A foreign
  principal gets **403**. An empty workspace is an honest
  `{"ports":[],"count":0,"complete":true}` with a 200, never an error.
- **Cost:** the read is **on-demand only** and is deliberately **not** part of
  the metrics stream (one table read is measured at ~4.6 ms).

```sh
curl -s -H "Authorization: Bearer $KEY" \
  localhost:8080/v1/workspaces/$WS/ports
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

### GET /v1/metrics

One-shot (non-streaming) snapshot of every workspace the principal owns. The
frame shape is identical to one SSE frame from `GET /v1/metrics/stream`, so a
polling client and a streaming client parse the same JSON. It is O(1) per
workspace: disk is a single `statfs`; RAM is one `memory.current` per **live**
leaf cgroup, plus (only while a run or session is live) one `memory.max` read of
the workspace's parent cgroup for the enforced ceiling. A workspace with no live
cgroup costs zero cgroup reads and reports RAM 0.

- **Response 200:**

  ```json
  {"timestamp":"2026-01-02T15:04:05.123456789Z","interval_seconds":3,
   "workspaces":[
     {"workspace_id":"ws_...",
      "ram_used_bytes":113246208,"ram_limit_bytes":167772160,
      "ram_percent":67.5,
      "ram_complete":true,
      "disk_used_bytes":90112,"disk_limit_bytes":67108864,
      "disk_percent":0.13,"disk_complete":true,
      "live_runs":2,"live_cgroups":2}],
   "sampler":{"ticks":42,"cgroup_reads":84,"statfs_reads":42,
              "tree_walk_reads":0,"subscribers":0,"running":0,
              "dropped_frames":0,"candidates":1}}
  ```

- **Errors:** 401. 404 when `metrics.enabled` is false.
- **Semantics:** `ram_used_bytes` is the **SUM** of `memory.current` over the
  workspace's live run **and** session cgroups — reading one cgroup would be a
  wrong number, not an approximation. `ram_limit_bytes` is the workspace's
  **ENFORCED ceiling**: the parent cgroup's `memory.max` (the workspace's
  `memory_mb` in bytes), read from the kernel while a run or session is live, and
  falling back to `memory_mb` when idle. It is the number the tenant is
  advertised **and** the number the kernel enforces, so `ram_used_bytes <=
  ram_limit_bytes` always holds and `ram_percent` never exceeds 100 — the older
  per-run model could report `ram_used > ram_limit` and a percentage above 100,
  which read as a broken contract (see [docs/DECISIONS.md](DECISIONS.md) ADR 94).
  `disk_used_bytes` is the filesystem's used bytes (`total − f_bfree`), which
  equals what `df` reports **inside** the workspace. `*_complete` is false when a
  figure is a lower bound (`ram_complete` on a failed cgroup read,
  `disk_complete` when the root is not a live mount). Percentages are 0 when the
  limit is not positive — a derived percentage the numbers cannot support is
  never published.

```sh
curl -s -H "Authorization: Bearer $KEY" localhost:8080/v1/metrics
```

### GET /v1/metrics/stream

Server-Sent Events stream of the same frames, one per tick. Scoped to every
workspace the principal owns.

- **Response 200:** `Content-Type: text/event-stream`, `Cache-Control: no-cache`.
  An initial `: connected` comment line is written and flushed so the client's
  parser sees bytes immediately; then one `data: <json>` frame per tick.
- **Query:** `interval` (optional) — a cadence override. It is **clamped to the
  configured floor and ceiling**; a value **below the floor is refused with
  `422`** (never silently raised). Accepts a Go duration (`1s`, `500ms`).
- **Errors:** 401, 422 (interval below the floor). 404 when disabled.
- **Semantics:** the stream is a thin consumer of the **one service-wide
  sampler**: the sampler starts on the first subscriber, stops on the last
  (a service nobody watches costs zero), and samples **once per tick** regardless
  of how many subscribers are connected. Each stream holds **at most one pending
  frame** — a subscriber that lags is **dropped**, never buffered — so a slow
  client cannot grow the engine's memory. See
  [docs/DECISIONS.md](DECISIONS.md) ADR 93.

```sh
curl -sN -H "Authorization: Bearer $KEY" "localhost:8080/v1/metrics/stream?interval=3s"
```

### GET /v1/workspaces/{workspaceID}/metrics

One-shot snapshot of a single workspace. Same frame shape as
`GET /v1/metrics`, scoped to one workspace the principal owns.

- **Path parameter:** `workspaceID`.
- **Response 200:** the same `{timestamp, interval_seconds, workspaces, sampler}`
  object with exactly one entry in `workspaces`.
- **Errors:** 401, 403, 404.

### GET /v1/workspaces/{workspaceID}/metrics/stream

SSE stream for a single workspace. Identical framing and rules to
`GET /v1/metrics/stream`, scoped to the one workspace.

- **Path parameter:** `workspaceID`.
- **Response 200:** `Content-Type: text/event-stream`.
- **Errors:** 401, 403, 404, 422.

```sh
curl -sN -H "Authorization: Bearer $KEY" "localhost:8080/v1/workspaces/$WS/metrics/stream"
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

The engine runs cloudflared from the host inside the workspace's namespace; with
the default `identity.hide_provider_tools` it is **not** on `PATH` inside the
workspace (the curated `/usr/local` does not carry it), so a tunnel is started
through this API. An operator who wants a raw `cloudflared` inside the workspace
can expose one via `identity.tools_dir`. See
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

### POST /v1/workspaces/{workspaceID}/files/archive

Upload an archive and extract it into the workspace in one request.

- **Path parameter:** `workspaceID`.
- **Request:** `multipart/form-data` with:
  - `archive` (binary, **required**) — the archive bytes. The format is detected
    from the **content** (magic bytes), never the filename.
  - `path` (string, optional) — destination directory; default `/`. It is
    resolved through the workspace path machinery and may not escape the root.
  - `overwrite` (bool, optional) — default is the server's `archives.overwrite`
    (`false`). With `false`, an entry that would clobber an existing file is
    refused.
  - `dry_run` (bool, optional) — default `false`. With `true` the archive is
    validated and the entries are reported, and **nothing is written**.
- **Response 200:** an `archiveResult`:

  ```json
  {
    "format": "zip",
    "destination": "/",
    "entries_total": 5,
    "entries_extracted": 4,
    "entries_skipped": 1,
    "entries_rejected": 0,
    "bytes_written": 51,
    "dry_run": false,
    "entries": [
      {"name":"src/main.py","path":"/src/main.py","type":"file","size":15,
       "mode":"-rw-r--r--","rejected":false,"skipped":false}
    ]
  }
  ```

  Each entry carries `name` (as stored in the archive), `path` (the resolved
  workspace path), `type` (`file`/`dir`/`symlink`/`hardlink`), `size`, `mode`
  (privileged bits already stripped), `rejected` and `skipped`. A rejected entry
  adds `reason` (a stable refusal code); a skipped one adds `skip_reason`.
- **Errors:** 400 `validation` (not multipart, missing `archive`), 401, 403, 403
  `subscription_expired` (locked workspace), 409 `archive_entry_exists` (a
  collision with `overwrite:false`), 413 `payload_too_large` (upload >
  `archives.upload_mb`) or `archive_too_large` / `archive_too_many_entries` /
  `archive_ratio_exceeded` (a cap), 415 `unsupported_archive`, 422
  `archive_unsafe_entry` / `archive_encrypted` / `archive_multivolume` /
  `unprocessable` (corrupt), 507 `insufficient_storage` (workspace disk).
- **Semantics:** extraction is performed **in the engine** (Go's
  `archive/zip` and `github.com/nwaples/rardecode/v2`); no host tool is invoked.
  The archive is untrusted input; see [Native archives](#native-archives) for the
  full safety model. The whole archive is validated **before** a byte is written,
  so a refusal leaves no partial tree. The archive-level caps are a ceiling; the
  workspace's own disk allowance is the real floor.

```sh
curl -s -H "Authorization: Bearer $KEY" \
  -F archive=@project.zip -F path=/dst \
  localhost:8080/v1/workspaces/$WS/files/archive
```

### POST /v1/workspaces/{workspaceID}/files/extract

Extract an archive that is **already in the workspace** (an upload, a downloaded
release, a clone artifact) into a destination directory, using the same engine.

- **Path parameter:** `workspaceID`.
- **Request body (`extractRequest`):**

  | Field | Go type | JSON | Required | Validation | Default |
  |-------|---------|------|----------|------------|---------|
  | `path` | `string` | `path` | yes | the source archive path | — (400) |
  | `dest` | `string` | `dest` | no | resolved; may not escape the root | workspace root |
  | `overwrite` | `bool` | `overwrite` | no | — | `archives.overwrite` (`false`) |
  | `dry_run` | `bool` | `dry_run` | no | — | `false` |

- **Response 200:** the same `archiveResult` as the upload-and-extract route.
- **Errors:** as above; the source file is read under the `archives.upload_mb`
  cap (a larger source is 413 `payload_too_large`), and a missing source is 404
  `not_found`.
- **Semantics:** identical safety model and refusal codes to the upload route;
  only the source differs (a workspace file instead of the request body).

```sh
curl -s -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"path":"/bundle.zip","dest":"/out"}' \
  localhost:8080/v1/workspaces/$WS/files/extract
```

### GET /v1/workspaces/{workspaceID}/files/archive

Stream a **zip** of a directory tree in the workspace ("download my project").

- **Path parameter:** `workspaceID`.
- **Query parameter:** `path` (string, optional) — the directory to export;
  default `/`. A missing path is 404 `not_found`; a non-directory is 422
  `not_directory`.
- **Response 200:** the zip bytes, with `Content-Type: application/zip` and a
  `Content-Disposition` attachment whose filename is derived from the directory
  (default `workspace.zip`).
- **Errors:** 401, 403, 403 `subscription_expired`, 404 `not_found`, 422
  `not_directory`. A failure **before** any byte is streamed is reported as a
  normal JSON error; a mid-stream failure can only truncate, which the client
  detects.
- **Semantics:** the export is built by the same engine package as extraction, so
  the two directions are symmetrical. The walk never follows a symlink and
  **skips** links and, when `archives.skip_junk` is on, macOS metadata junk, so a
  zip can never smuggle a link out and an import/export round-trips cleanly. The
  root itself is never emitted. Only **ZIP is written**; RAR is read-only (its
  encoder is proprietary).

```sh
curl -s -H "Authorization: Bearer $KEY" -o project.zip \
  "localhost:8080/v1/workspaces/$WS/files/archive?path=/project"
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
    the given statuses (`queued,running,exited,killed,timeout,failed,interrupted,oom`).
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
interrupted/oom runs).

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

## Workspace profiles

A workspace's toolchain is chosen by a **named profile**: an explicit,
reviewable **allowlist** of the tools (and the data/library trees those tools
need) the workspace is given. A workspace names the profile it wants at creation
(`POST /v1/workspaces {"name":"x","profile":"python"}`), the stored record says
which it got, and `GET /v1/meta` lists the offered profiles so a client can
choose.

This replaces the previous model, in which every workspace received a copy of
almost the whole host `/usr/bin` and `/usr/sbin` with only the engine/platform
names removed (a denylist). A profile is data, not a code path: adding a
language means adding a file under `configs/profiles/` (or a directory an
operator points `profiles.dir` at), never editing the engine.

- **A tool is not just its binary.** A profile has entry sets: `commands` names
  binaries, and `paths` names files, directories and whole trees a tool needs.
  The `python` profile attaches `/usr/lib/python3.12` (the whole stdlib — without
  it `import json` fails), `/usr/lib/node_modules/npm`, `/usr/lib/git-core`,
  the perl trees and more. The engine also always provides the shared libraries,
  the ELF loader and `/usr/lib/ssl` (TLS trust paths), so a dynamically-linked
  TLS client works.
- **Absence is enforced.** A tool not in the profile is **absent**: executing it
  inside a real workspace fails command-not-found (`422 unprocessable`). The
  engine's existing engine/platform name+content filter still applies on top of
  a profile; a profile entry naming the engine or platform is refused (fail
  loud), never silently shipped.
- **The default** is the server's `profiles.default` (`python`). A create that
  omits `profile` gets it, and the record reports the resolved name. An existing
  record with no stored profile (predating profiles) reads as the default.
- **Immutability.** A workspace's profile is fixed at creation and is **not**
  patchable: it decides which tools exist, so changing it would invalidate
  anything already installed or running. A different toolchain is a new
  workspace.
- **Terminal sessions need Python.** The PTY relay that backs a session is a
  Python program, so `POST .../sessions` on a profile without `python3` (e.g.
  `minimal`) is refused `422 session_needs_interpreter`.
- **Where `screen`/`tmux` work.** A PTY-based tool works only in a terminal
  **session** (which binds `/dev/pts` and `/dev/ptmx`), never in one-shot
  `exec`/`run` (which get only `null`/`urandom`/`zero`), where `openpty()` fails
  with `ENOENT` regardless of the profile. The `python` profile documents this
  rather than shipping a tool that mysteriously fails.

The built-in profiles are `python` (Python 3.12, pip/venv, Node.js, Perl, git and
common shell tools; it also provides a `python` alias of `python3`, which the
host lacks) and `minimal` (POSIX shell + coreutils only, no language runtime —
used by the test suite to prove the allowlist excludes things).

```sh
# list the profiles and the default
curl -s -H "Authorization: Bearer $KEY" localhost:8080/v1/meta | \
  python3 -c 'import json,sys;d=json.load(sys.stdin)["profiles"];print("default",d["default"]);[print(" -",p["name"],len(p["commands"]),"commands") for p in d["profiles"]]'

# create under a named profile
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"demo","profile":"python"}' localhost:8080/v1/workspaces
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

## Subscriptions

A workspace may carry a **subscription**: a running term with an **absolute
expiry instant**. This is an engine feature, not a billing feature: the engine
owns the **clock** and the **enforcement**, and stores absolute instants (never
a "days remaining" counter), while a separate product owns *why* a term is what
it is. Because every value is absolute, state is recomputed from stored times
after a restart and is never held in memory. A workspace with **no**
subscription never expires and is never touched by the reaper — that is the
default, so nothing changes for a workspace that does not ask.

### The duration grammar

A term is given as either a `duration` string or an absolute `expires_at`
(RFC3339). The duration grammar is one or more `<number><unit>` pairs summed
together:

| Unit | Meaning | Exactness |
|------|---------|-----------|
| `s` | seconds | exact |
| `m` | minutes | exact |
| `h` | hours | exact |
| `d` | days (24 h) | exact |
| `w` | weeks (7 d) | exact |
| `mo` | **30 days** | a **fixed-length approximation**, never a calendar month |
| `y` | **365 days** | a **fixed-length approximation**, never a calendar year |

Combinations are allowed: `1d12h`, `1w2d`, `12h30m`. A **bare integer is
refused** (`"30"` is ambiguous); a unit suffix is required. Zero, negative,
fractional and unparseable durations are `422 subscription_invalid`. A client
that needs calendar months must send an absolute `expires_at`.

### States and the summary

Every workspace carries a cheap **derived** `summary` (on the get **and** on
each list item), computed from stored fields with **no directory walk**:

| Field | Meaning |
|-------|---------|
| `summary.state` | `active`, `expiring_soon`, `locked` or `unlimited` |
| `summary.age_sec` / `created_at` | how long the workspace has existed |
| `summary.last_activity_at` | last run/mutation time |
| `summary.runs.total` / `active` | run counters |
| `summary.disk.used_bytes` / `limit_bytes` | disk (used is a cheap statvfs of the already-mounted filesystem) |
| `summary.profile` | the workspace's profile |
| `summary.subscription.state` | the term state: `active`, `expiring_soon`, `locked`, `unlimited` |
| `summary.subscription.expires_at` | the absolute expiry (absent when unlimited) |
| `summary.subscription.remaining_sec` | whole seconds until expiry, **clamped at 0, never negative** |
| `summary.subscription.grace_ends_at` | `expires_at + grace_period` (deletion instant) |
| `summary.subscription.extensions` | how many times the term was extended |
| `summary.subscription.locked_at` | when the reaper locked it (absent when never) |

The threshold for `expiring_soon` is `min(10% of the granted term, 24 h)`.
`remaining_sec` is rounded **up** to whole seconds, so a live term never reports
`0`: `remaining_sec` is `0` **exactly when** the term is `locked` (or, for a
workspace with no term, the summary carries no subscription object at all). This
makes `remaining_sec: 0` a hard signal of a lock. **The numbers agree with
enforcement**: if the summary says `remaining_sec` is `0` the workspace is
actually locked and refusing compute.

### Enforcement: lock, refuse, delete

When a term lapses the reaper **locks** the workspace. Locking stops using
resources, not merely labels the workspace: it stops running runs, kills
sessions and detached processes, deletes tunnels, and **unmounts the
per-workspace disk image** (releasing the `fuse2fs` process) while keeping every
byte of data.

- **Refused while locked** (`403 subscription_expired`): `exec`, `runs`,
  `sessions`, `tunnels`, `packages/install`, and every **mutating** `files`
  operation (`PUT /files/content`, `DELETE /files`, `POST /files/mkdir`,
  `POST /files/rename`, `POST /files/upload`).
- **Allowed while locked**: `GET /v1/workspaces/{id}` (and its summary),
  `GET /v1/workspaces`, `GET /usage`, `GET /files` and every file read
  (`stat`, `content`, `download`), and `DELETE /v1/workspaces/{id}` so it can be
  removed early. A locked workspace cannot consume compute.

After `expires_at + grace_period` (default `168h` = 7 days) the reaper
**deletes** the workspace: stop compute, unmount, then purge the files,
`disk.img` and the metadata. A manual `DELETE` works at any time, including while
locked.

### Configuration

| `subscription:` key | Default | Meaning |
|---------------------|---------|---------|
| `enabled` | `true` | the reaper switch. `false` = the loop does not run, so nothing is automatically locked or deleted. |
| `default_duration` | `""` | the term a create gets when it names none. **Empty means NO expiry** (unchanged behaviour). |
| `max_duration` | `365d` | server ceiling measured from now; beyond it is `422 max_duration_exceeded`. |
| `grace_period` | `168h` | how long a lapsed workspace stays locked before deletion. |
| `reap_interval` | `60s` | reaper tick interval. |

These defaults change the behaviour of **no** existing workspace: a workspace
without a subscription is never locked or deleted.

```sh
# create with a 30-day term
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"trial","subscription":{"duration":"30d"}}' \
  localhost:8080/v1/workspaces
# renew early (adds 30 days to the live term) or renew a locked workspace
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"duration":"30d"}' localhost:8080/v1/workspaces/$WS/subscription/extend
```

## Native archives

The engine reads and writes archives **itself** (Go's `archive/zip` and
`github.com/nwaples/rardecode/v2`); it never shells out to `unzip`, `tar` or
`unrar`. This host has no RAR tool at all, and a security boundary cannot live in
a host binary that may be absent or different. Because the behavior is in the
engine, every refusal is deterministic and identical on every host. The three
routes are `POST /files/archive` (upload-and-extract), `POST /files/extract`
(in-place) and `GET /files/archive` (export). The capability is advertised in
`GET /v1/meta` under `archives`. See [DECISIONS.md](DECISIONS.md) ADR 92.

**Formats.** `zip` and `rar` (both RAR4 and RAR5) are detected from the
**content's magic bytes**, never the filename — a `.zip` that is really a RAR
still works, and an unrecognized format is `415 unsupported_archive` naming the
detected signature. An operator can disable one via `archives.formats`.
**Extraction** reads ZIP and RAR; **export** writes only ZIP, because RAR's
encoder is proprietary, so **RAR is read-only by design**.

**Asymmetric direction.** You upload a zip or a rar and download a zip; you
cannot make the engine produce a rar. This is deliberate, not a gap.

### The safety model (archives are untrusted input)

- **Nothing outside the destination.** Every entry name is validated through the
  workspace's own path cleaner; a `..` that escapes, an absolute path or a drive
  letter is refused with `422 archive_unsafe_entry`. A symlink or hardlink entry
  is **refused**, never materialized — the engine never has to decide whether a
  link target is safe, because it never creates one.
- **No privileged bits.** `setuid`, `setgid` and sticky bits are stripped from
  every extracted mode; a file that asked for `4755` lands as `0755` (or a safe
  default when nothing usable remains).
- **Nothing is written until the whole archive is validated.** The header list is
  read and every entry is decided **before** the first byte is written, so a
  refused archive leaves **no** partial tree.
- **A streaming budget, not a header promise.** The bytes actually written are
  bounded by `archives.max_total_mb` and `archives.max_file_mb` (and a per-entry
  ratio guard), so a header that lies about its size cannot overshoot the cap.
- **A collision is refused, not overwritten.** The default (`overwrite:false`) is
  `409 archive_entry_exists` with the existing file's content preserved; a request
  can opt in to replacement with `overwrite:true`.

If a case cannot be made safe, it is **refused** with a stable code. A refused
extraction is always better than a compliant-looking escape.

### `dry_run`

With `dry_run:true` the archive is validated and the entries are reported
(`entries_total`, each entry's `rejected`/`skipped` and `reason`), and **nothing
is written**. A dry run does **not** fail on a rejected entry — previewing a
hostile archive is exactly its purpose — so it reports `entries_rejected` while
returning `200`.

### Configuration

| `archives:` key | Default | Meaning |
|-----------------|---------|---------|
| `enabled` | `true` | server switch; `false` refuses every archive route with `422 unprocessable`. |
| `max_entries` | `20000` | entry-count cap (`413 archive_too_many_entries`). |
| `max_total_mb` | `2048` | total uncompressed budget, streaming (`413 archive_too_large`). |
| `max_file_mb` | `512` | single-entry uncompressed cap (`413 archive_too_large`). |
| `max_ratio` | `200` | uncompressed/compressed ratio guard (`413 archive_ratio_exceeded`). |
| `upload_mb` | `50` | archive upload cap, separate from `caps.upload_mb` (`413 payload_too_large`). |
| `overwrite` | `false` | default collision policy (`409 archive_entry_exists`). |
| `skip_junk` | `true` | skip `__MACOSX/`, `.DS_Store`, `._*`, `Thumbs.db`, `desktop.ini`. |
| `formats` | `["zip","rar"]` | content-detected allowlist. |

```sh
# extract an uploaded project into /dst (default overwrite:false)
curl -s -H "Authorization: Bearer $KEY" -F archive=@project.zip -F path=/dst \
  localhost:8080/v1/workspaces/$WS/files/archive
# preview what an archive would do, writing nothing
curl -s -H "Authorization: Bearer $KEY" -F archive=@project.zip -F dry_run=true \
  localhost:8080/v1/workspaces/$WS/files/archive
# export a directory as a zip
curl -s -H "Authorization: Bearer $KEY" -o project.zip \
  "localhost:8080/v1/workspaces/$WS/files/archive?path=/project"
```

## Lifecycle and limits

### Workspace lifecycle

A workspace has a `status` of `active`, `suspended` or `locked`:

```
create ──► active ──suspend──► suspended
              ▲                   │
              └──── resume ───────┘
                (stop leaves active)

           active ──term lapses──► locked ──renew──► active
                                     │
                              grace ends (reaper) ──► deleted
```

`suspended` is a **manual, reversible-by-API** action (`suspend`/`resume`).
`locked` is the **automatic** consequence of a lapsed subscription term: the
reaper stops the workspace's compute and unmounts its disk (keeping every byte of
data), refuses work with `403 subscription_expired`, and the way out is a
**renewal** (`POST .../subscription/extend`), not `resume`. After the grace
period the reaper deletes it (or you delete it manually at any time). See
[Subscriptions](#subscriptions).

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
                     ├──► oom          (kernel OOM at the workspace ceiling; exit_code 137)
                     ├──► rate_limited (write rate over the ceiling; exit_code 137)
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
- `oom` — the kernel's OOM killer ended the run because it reached the
  workspace's **memory ceiling** (`memory_mb`). It is distinct from `exited`: a
  run killed at the ceiling never reads as a clean exit. It is distinct from
  `killed` because nothing was requested. The executor confirms it by reading the
  workspace parent cgroup's `oom_kill` counter across the run, so a bare signal
  is not misreported as an OOM. `exit_code` is 137.
- `failed` — the executor itself returned an error (e.g. nsjail missing or a
  launch failure); `exit_code` is null and the diagnostic is not exposed on the
  run object.
- `rate_limited` — the engine's **write-RATE** watchdog stopped the run because
  the workspace's disk grew faster than its `write_rate_mb_s` ceiling for N
  consecutive samples. It is distinct from `exited` (it was stopped), from
  `failed` (nothing was wrong with the program), from `killed` (nothing was
  requested) and from `oom` (the memory ceiling). It is only ever produced when
  `disk.write_rate_enabled` is true (see [Write rate](#write-rate)).
- `interrupted` — the process did not survive a service restart; startup
  recovery moves any still-`queued`/`running` run to this terminal state.
- All terminal states are final. A stop only changes a live run to `killed`.

### Resource limits

Per-workspace limits (`limits:` config; overridable at creation and via
`PATCH`):

| Limit | Config key | Default | Server maximum (`max_limits:`) | Effect |
|-------|------------|---------|-------------------------------|--------|
| CPU cores | `limits.cpu_cores` | 1 | 8 | cgroup `cpu.max` on the workspace parent |
| Memory | `limits.memory_mb` | 512 | 8192 | physical ceiling: cgroup `memory.max` on the workspace **parent** cgroup (bytes); `rlimit_as` is `memory_mb + headroom` (virtual backstop, ADR 82) |
| Disk | `limits.disk_mb` | 1024 | 51200 | per-workspace filesystem image + file-API quota; `RLIMIT_FSIZE` per file |
| Wall time | `limits.max_wall_time_sec` | 300 | 3600 | async run timeout (`--time_limit`) |
| Runs / procs | `limits.max_runs` | 3 | 64 | cgroup pids max = `max_runs * 64` on the workspace parent; also `RLIMIT_NPROC` |
| Network | `limits.allow_net` | false | true | private namespace connected to the internet via chained slirp4netns (never the host network) |
| Unbounded runs | `limits.allow_unlimited_runs` | false | false | workspace opt-in for `no_timeout:true`; **both** levels must be true |
| Tunnels | `limits.allow_tunnels` | false | server switch `tunnels.enabled` | workspace opt-in for quick tunnels; **both** this and `tunnels.enabled` must be true, and `allow_net` must also be true |
| Write rate | `limits.write_rate_mb_s` | 0 (off) | `max_limits.write_rate_mb_s` | per-workspace write-RATE ceiling (MiB/s); a **SAMPLED** cap enforced by stopping the offending run with the distinct `rate_limited` outcome. Meaningful only when `disk.write_rate_enabled` is true. **Both** the value and the server switch are required; a value above the workspace `disk_mb` is `422 unprocessable`. |

The memory, CPU and pids ceilings all live on **one cgroup per workspace** (a
parent cgroup named `ws-<id>`), so the advertised number is the number the kernel
enforces across every run and session concurrently (see
[docs/DECISIONS.md](DECISIONS.md) ADR 94).

The server maximum is a hard ceiling: a create or `PATCH` value above it is
rejected with `422 unprocessable` (never silently clamped). `max_limits:` is
loaded from config and always at least the defaults. `allow_unlimited_runs` and
`allow_tunnels` are the two non-numeric ceilings: each is a switch, and the
workspace value can only be `true` when the server switch is `true` (see
[Unbounded runs](#unbounded-runs) and [Quick tunnels](#quick-tunnels-and-egress)).

### Write rate

A workspace's write RATE can be capped so one workspace running `dd`/`git
clone`/a build cannot starve every other workspace's IO. Unlike the disk TOTAL,
RAM and CPU (which are kernel-enforced), this ceiling is **SAMPLED**: the
workspace's real disk growth is measured once per metrics tick (one `statfs`)
and a run whose **sustained** rate exceeds the ceiling is stopped with the
distinct `rate_limited` status.

- **Default OFF.** It changes nothing unless an operator turns it on:
  `disk: { write_rate_enabled: true }` plus a per-workspace
  `limits.write_rate_mb_s` (or the `limits.write_rate_mb_s` server default).
- **The ceiling** is `limits.write_rate_mb_s` (MiB/s), bounded by the server
  `max_limits.write_rate_mb_s`; a value above the workspace's own `disk_mb` is
  rejected with `422 unprocessable` (a workspace cannot write its whole disk in
  under a second). `0` means no rate ceiling.
- **Sustained, not instantaneous.** `N` (`disk.write_rate_samples`, default 3)
  **consecutive** over-limit samples are required; a single burst is not killed
  and any under-limit sample resets the counter.
- **A rate-kill is a distinct outcome**: the offending run ends `rate_limited`
  (never `exited`, never a generic failure). The engine's **own** write path
  (an API write/upload or an archive extraction) has no run to stop, so it is
  refused with `429 write_rate_exceeded` while the workspace is over its
  ceiling; both paths enforce the **same** sampled ceiling.
- **The overshoot is a number, not a kernel boundary.** With the shipped
  defaults (`N = 3`, interval `3 s`) a workspace can write up to
  `9 × write_rate_mb_s` MiB above the line before the kill. This is stated and
  measured (see [docs/DECISIONS.md](DECISIONS.md) ADR 95); it is never presented
  as equivalent to the disk filesystem boundary.
- **The advertised limit is the enforced one**: `GET /v1/meta` reports
  `write_rate` (enabled, samples, server max, the distinct outcome and refusal
  code), and the per-workspace metrics snapshot reports `write_rate_limit_mb_s`
  (0 when the feature is off).

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
  v0.0.18 (see [Quick tunnels and egress](#quick-tunnels-and-egress)).
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
# {"version":"0.0.18","api_version":"v1", ... }

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
