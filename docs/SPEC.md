# sandboxd specification (v0.0.20)

This document is the canonical specification for the sandboxd service. It is a
faithful copy of the project brief, retained in-repo so implementation and
review stay aligned.

---

You are implementing a Go service called "sandboxd" — a secure per-principal
code execution backend exposed over HTTP/JSON. ("Principal" is a static API-key
identity, not a user account: accounts, per-user quota, plans and rate limiting
belong to the separate frontend product, §2.6.)

## 0. GROUND RULES

- Language: Go 1.22 (the brief targeted 1.22; `go.mod` requires Go 1.23 because
  the mandated koanf YAML parser declares a `go 1.23` floor — see
  [DECISIONS.md](DECISIONS.md)). The service version string is `0.0.20`
  (`model.Version`): it is the highest feature group resolved in DECISIONS.md
  (which runs to v0.0.20), not the `/v1` API contract version. The two move
  independently — the API prefix changes only on a breaking contract change.
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
- No external DB. Storage is filesystem JSON under `./data/` in v0.0.20.
- The service version string (`model.Version`) tracks the highest feature group
  resolved in [DECISIONS.md](DECISIONS.md) (currently v0.0.20), **not** the `/v1`
  API contract version: the API prefix changes only on a breaking contract
  change, so the two are deliberately decoupled. The decision log's own
  per-group labels (v0.0.6 … v0.0.20) are the version history this number
  follows.
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
- `GET /readyz` → 200 or 503 `{"status":"ready"|"not_ready","nsjail":bool,"cgroups":bool,"data_dir":bool,"network":{...},"tunnels":{...},"identity_masking":{"enabled":bool,"ready":bool,"reason":str},"provider_tools_hidden":{"enabled":bool,"ready":bool,"reason":str},"workspace_limits":{"enabled":bool,"mechanism":str,"proc":bool,"proc_files":[str],"reason":str},"egress_decoy":{"enabled":bool,"ready":bool,"reason":str}}`
- `GET /v1/meta` → `{"version":"0.0.20","api_version":"v1","runtimes":["python3"],"limits_defaults":{...},"limits_maxima":{...},"network":{...},"tunnels":{...},"identity_masking":{"enabled":bool,"ready":bool,"mechanism":str,"limits":str},"provider_tools_hidden":{"enabled":bool,"ready":bool,"mechanism":str,"limits":str},"workspace_limits":{"enabled":bool,"mechanism":str,"reporting":str,"enforcement":str,"limits":str},"egress_decoy":{"enabled":bool,"ready":bool,"mechanism":str,"limits":str},"write_rate":{"enabled":bool,"server_max_mb_s":int,"default_mb_s":int,"samples":int,"outcome":str,"refusal_code":str,"mechanism":str,"limits":str},"archives":{"enabled":bool,"formats":[str],"read_only":[str],"limits":{...},"routes":[str],"refusal_codes":[str],"semantics":str},"ports":{"enabled":bool,"max_pids":int,"max_fds_per_pid":int,"include_unix":bool,"mechanism":str,"routes":[str],"reachability":str,"default_note":str,"reads_total":int}}`
- `GET /metrics` → Prometheus text

#### What a workspace can and cannot observe

This is a security property, stated plainly. A workspace runs inside an nsjail
sandbox with a private mount namespace, an unprivileged uid, its own network
namespace, and (by default) a host-identity masking shim. It **cannot**:

- read machine identity: `/etc/machine-id`, `/etc/hostname`, `/etc/hosts`,
  `/etc/os-release`, `/etc/hostid`, `/etc/shadow` are not present;
- read the HOST kernel's view: `/proc/net/dev`, `/proc/self/cgroup`,
  `/proc/self/mountinfo` are not present, and `/proc` is a speculative-free,
  CURATED per-workspace payload, not the real procfs (see below);
- read hardware identity: `/sys/class/net` and every `/sys/class/dmi/id/*` are
  not mounted;
- learn the host name (`socket.gethostname()` returns the generic `sandbox`);
- resolve the operator's internal DNS names, or obtain its address with
  `hostname -f` / `hostname -I`;
- reach any "what is my IP" service without `allow_net:true`;
- learn the platform's kernel build (`uname` reports the generic
  `0.0.0-sandbox`), the host's core count or total RAM (`sysconf`/`sysinfo`
  report the workspace's limits), or the base image's distro patch level
  (`gnu_get_libc_version`/`confstr` are trimmed to the upstream number);
- learn the operator's server FQDN or LAN address: the workspace's `/etc/hosts`
  is a generated file holding only generic localhost entries, never the host's;
- see any tool its **profile** does not name: a workspace's whole `/usr` is
  replaced with a tree holding only the tools its named profile allows (an
  explicit allowlist) plus the engine base, shared libraries and loaders, so the
  platform's tooling (`zcp`/`zsc`/`yq`), the compiler chain, `apt` and every
  other unlisted command are simply absent. `/etc/ssl/certs` is replaced with a
  copy that holds no provider-named certificate while normal TLS keeps working;
- step behind the masking shim through a handle-scoped `dlsym`: a
  `dlopen("libc.so.6")` + `dlsym` lookup is answered by the shim, so
  `ctypes.CDLL("libc.so.6").uname` returns the masked value.

It **can**: observe its OWN cpu, memory and disk limits as configured (`nproc`,
`free -h`, `/proc/meminfo`, `df -h`, `stat -f` and `os.statvfs()` all report the
workspace's own values, because `/proc` is a curated per-workspace payload and
`/workspace` is a real per-workspace filesystem sized to `disk_mb` — see
[DECISIONS.md](DECISIONS.md) #85/#86), run code, read and
write its own workspace, and — with `allow_net:true` — reach the public internet,
presenting a **platform-shared NAT address** (measured `93.185.106.128` this
session); that address is the platform's egress and names the provider in its
registration (`AS43541 VSHosting`, `ZEROPS-IO`), which is structural on this
platform (see [DECISIONS.md](DECISIONS.md) #76). It can also reach the
**inherent** masking limits — a statically-linked binary, a musl runtime, a raw
`syscall(2)`, or a handle-scoped `dlvsym(3)` (documented in
[DECISIONS.md](DECISIONS.md) #72 and reported by `/v1/meta`'s
`identity_masking.limits`); those are stated, not hidden, and a raw `syscall(2)`
is asserted as a known result by `scripts/identity-check.sh`.

### 2.2 Workspaces

- `POST /v1/workspaces` body `{"name","limits?":{cpu_cores,memory_mb,disk_mb,max_wall_time_sec,max_runs,allow_net}}` → 201 `{workspace}`
- `GET /v1/workspaces` → `{"workspaces":[...]}` (owned by caller)
- `GET /v1/workspaces/{id}` → `{workspace}`
- `PATCH /v1/workspaces/{id}` body `{"name?","limits?":{...}}` → 200 `{workspace}`
  (runtime-mutable settings; see below)
- `DELETE /v1/workspaces/{id}` → 204 (stop all runs, rm -rf workspace dir)

Workspace JSON:

```json
{"id":"ws_...","owner_id":"user_...","name":"...","created_at":"...","status":"active","limits":{},"last_activity_at":"...","subscription":{},"summary":{}}
```

`limits` is `{cpu_cores, memory_mb, disk_mb, max_wall_time_sec, max_runs,
allow_net, allow_unlimited_runs, allow_tunnels, write_rate_mb_s}`;
`allow_unlimited_runs` and `allow_tunnels` are boolean switches (default false)
that must each be true (along with the matching server switch —
`max_limits.allow_unlimited_runs` and `tunnels.enabled` respectively) before a
workspace may request `no_timeout:true` runs or quick tunnels. `write_rate_mb_s`
(default 0 = no ceiling) is a per-workspace write-RATE ceiling in MiB/s; it is a
**SAMPLED** cap (not a kernel boundary), enforced only when the server
`disk.write_rate_enabled` is true, and a value above the workspace's own
`disk_mb` is refused with 422 (see §2.2a and [DECISIONS.md](DECISIONS.md) ADR 95).

`status` is `active`, `suspended` or `locked` (`deleted` exists in the type
vocabulary but is never persisted: a deleted workspace is removed outright). The
status is persisted and survives a restart. `locked` is the **derived** status of
a lapsed subscription term (see below): it is the automatic consequence of the
term ending, distinct from `suspended` (a manual operator action), and the way
out is a **renewal**, not `resume`.

**Subscriptions (a running term).** A workspace may carry a `subscription`: a
running term with an **absolute expiry instant**. This is an engine feature, not
a billing feature — the engine owns the clock and the enforcement and stores
absolute instants (never a "days remaining" counter), so state is recomputed from
stored times after a restart. A workspace with **no** subscription never expires
and is never touched by the reaper; that is the default.

- Create with a term: `POST /v1/workspaces` body
  `{"subscription":{"duration":"30d"}}` **or**
  `{"subscription":{"expires_at":"2027-01-01T00:00:00Z"}}`. The duration grammar
  is summed `<number><unit>` pairs, units `s m h d w mo y`; `d`=24h, `w`=7d,
  **`mo`=30d** and **`y`=365d** are fixed-length approximations, never
  calendar-exact. A bare integer is refused. Zero/negative/unparseable durations
  and non-future `expires_at` are `422 subscription_invalid`.
- Extend (renew): `POST /v1/workspaces/{id}/subscription/extend` with the same
  body. While the term is **live** the extension **ADDS** to the expiry (an early
  renewer keeps the whole paid term; an absolute `expires_at` earlier than the
  live one is refused, and a live term is never shortened). After it has
  **lapsed** the extension starts from **now** and **unlocks** the workspace,
  restoring service. A server `max_duration` ceiling is enforced. Extending a
  deleted workspace is `404` and never resurrects it.
- Locking (automatic, at `expires_at`): the reaper stops running runs, kills
  sessions and detached processes, deletes tunnels and **unmounts the disk
  image** (releasing `fuse2fs`) while keeping every byte of data. Refused while
  locked (`403 subscription_expired`): `exec`, `runs`, `sessions`, `tunnels`,
  `packages/install` and every mutating `files` op. Allowed while locked:
  `GET /v1/workspaces/{id}` and its `summary`, `GET /v1/workspaces`, `GET /usage`,
  file reads, and `DELETE`.
- Deletion (automatic, at `expires_at + grace_period`): unmount, then purge the
  files, `disk.img` and metadata. A manual `DELETE` works at any time, including
  while locked.
- Config: `subscription.enabled` (default true), `default_duration` (**empty =
  no expiry**), `max_duration` (`365d`), `grace_period` (`168h`), `reap_interval`
  (`60s`). The defaults change the behaviour of no existing workspace.
- A **derived `summary`** is present on `GET /v1/workspaces/{id}` and on every
  item of `GET /v1/workspaces`, computed from stored fields with no directory
  walk: `state` (`active`/`expiring_soon`/`locked`/`unlimited`, threshold
  `min(10% of the term, 24h)`), `age_sec`/`created_at`, `last_activity_at`,
  `runs{total,active}`, `disk{used_bytes,limit_bytes}`, `profile`, and
 `subscription{state,expires_at,remaining_sec,grace_ends_at,extensions,locked_at}`.
 `remaining_sec` is clamped at 0 and rounded up, so it is `0` **exactly when**
 the term is `locked` (a live term never reports 0; an unlimited workspace has no
 subscription object). The numbers agree with enforcement: `remaining_sec: 0`
 means the workspace really is locked and refusing compute.

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
  `{"workspace","stopped_run_ids","stopped_run_count","stopped_session_ids","stopped_session_count","stopped_process_ids","stopped_process_count"}`.
  Stops **everything**: every tracked run, every live terminal session (reason
  `workspace_stopped`), and every `detached` process (a process started from a
  session that carries no run/session attribution) — but leaves the workspace
  `active` (accepting new work). Idempotent; a no-op success when nothing is
  running. The original `stopped_run_*`/`stopped_session_*` fields keep their
  shape; the `stopped_process_*` fields are additive. This is the
  workspace-level **stop-everything** control (see §2.9).

**Usage.** `GET /v1/workspaces/{id}/usage` → 200 with `workspace_id`, `status`,
`created_at`, `last_activity_at`, `disk_bytes`, `disk_bytes_complete`, `disk_mb`,
`disk_percent`, `runs_total`, `runs_by_status` (map status→count), `active_runs`
(queued+running), `max_runs`, and the effective `limits`. `disk_bytes` is a
**bounded walk** of the workspace `files/` tree (at most 100 000 directory
entries); if the bound is hit, `disk_bytes_complete` is `false` and
`disk_bytes` is a lower bound. `disk_percent` is
`disk_bytes / (disk_mb * 1024 * 1024) * 100`. `last_activity_at` is the most
recent run/mutation (run start/stop, purge, PATCH, suspend/resume/stop).

**Process inventory and stop.** A workspace exposes every live process its
sandbox is running, including processes started from a session:

- `GET /v1/workspaces/{id}/processes` → 200
  `{"processes":[{"id","pid","ppid","argv","kind","source_id","started_at"}],"count"}`.
  `kind` is `run`, `session`, or `detached`; `source_id` is the run/session id
  it belongs to (empty for `detached`). `id` is a stable opaque id
  (`proc_<hex>`, a hash of workspace + pid + kernel start time). Attribution
  uses each run's/session's **cgroup** plus a tag injected into every sandbox; a
  process inside the workspace with no source tag is reported `detached`, never
  hidden. An empty inventory is `{"processes":[],"count":0}`, never an error, and
  a workspace's inventory never includes another workspace's processes.
- `POST /v1/workspaces/{id}/processes/{processID}/stop` → 200
  `{"process_id","status","stopped","stopped_at"}` where `status` is `stopped`
  or `gone` (idempotent). It stops the process **tree** using the process's
  cgroup — so a workload that called `os.setsid()` and left its process group is
  still stopped — and returns `404` for an id that does not name a process in
  this workspace, `403` across principals/workspaces.

**Port/service discovery.** A workspace exposes the ports its processes are
listening on, and which process serves each one:

- `GET /v1/workspaces/{id}/ports` → 200
  `{"workspace_id","ports":[{...}],"count","unix":[{...}],"unix_count",
  "complete","notes","pids_scanned","fds_scanned","reachability"}`. Each port
  entry carries `port`, `protocol` (`tcp`/`tcp6`/`udp`/`udp6`), `address`, a
  derived `scope` (`loopback` for `127.0.0.1`/`::1`, `wildcard` for
  `0.0.0.0`/`::`, else `specific`), `state` (`LISTEN` for TCP; a UDP socket has
  **no** listen state, so its `state` is empty rather than an invented value),
  and — when resolvable — the owning `pid`, `kind` and a short `label` (the
  `argv[0]` basename plus args, truncated), plus `argv` and `owners`. An entry
  whose owner cannot be resolved is still reported with `attributed:false`,
  never dropped and never guessed. The list is sorted deterministically by
  `(port, protocol)` and is **live** on every call (no cache); a port that
  appears after the first call is reported on the next, and one whose process
  exited disappears. Unix-domain sockets are reported in the **separate**
  `unix` array and never mixed into the port list, because a unix socket is not
  a reachable port. `complete` is `false` (with a `notes` entry) when a bounded
  scan cap was hit, so a partial list is never shown as total.
- **A reported port is listening inside the workspace's private network
  namespace; it does NOT mean it is reachable from outside.** Publishing a port
  to the internet is a separate feature (tunnels, which require
  `limits.allow_tunnels` **and** `tunnels.enabled`), so a client must never tell
  a user their app is public merely because it appears here. Every response
  carries that statement in `reachability`.
- An empty workspace is `{"ports":[],"count":0,"complete":true}` with a 200,
  never an error; a foreign principal gets **403**; the route reads as **404
  `ports_disabled`** when `ports.enabled` is false. This endpoint is
  **on-demand only** and is deliberately **not** part of the metrics stream.

**No leaks.** When a run or session ends (any reason) the engine removes the
cgroup it caused nsjail to create, so `NSJAIL.*` cgroups do not accumulate. On
**startup**, alongside marking previously-live runs `interrupted` / sessions
`killed`, the engine **reaps orphaned workloads and their cgroups** left by a
hard (SIGKILL) daemon death, so an orphan does not keep running for the life of
its `time_limit` (or forever when unbounded). A graceful `SIGTERM` shutdown
already leaves nothing.

**Resource stream (RAM + disk).** A workspace's RAM and disk are pushed every
`metrics.interval` (default **3s**), cheaply enough that thousands of workspaces
cause no pressure. Four routes:

- `GET /v1/metrics` and `GET /v1/workspaces/{id}/metrics` — one-shot snapshots
  (every workspace the principal owns, or one workspace).
- `GET /v1/metrics/stream` and `GET /v1/workspaces/{id}/metrics/stream` — SSE
  (`text/event-stream`), one JSON frame per tick, flushed after every frame.

Each frame reports, per workspace: `ram_used_bytes`, `ram_limit_bytes`,
`ram_percent`, `ram_complete`, `disk_used_bytes`, `disk_limit_bytes`,
`disk_percent`, `disk_complete`, `live_runs`, `live_cgroups` (and
`cpu_usage_usec`/`pids_current` when `include_cpu` is on). Together with a
`timestamp`, `interval_seconds`, and the `sampler` counters (`ticks`,
`cgroup_reads`, `statfs_reads`, `tree_walk_reads`, `subscribers`, `running`,
`dropped_frames`, `candidates`).

The design is fixed by measurement (see [docs/DECISIONS.md](DECISIONS.md)
ADR 93 and ADR 94): disk comes from a single **`statfs`** (O(1), ~1.7 µs), never
a tree walk (153 µs/file); RAM is the **SUM** over the workspace's per-run
**and** per-session leaf cgroups, but every leaf lives inside ONE
**per-workspace parent** cgroup whose `memory.max` is the ENFORCED ceiling (so
`ram_limit_bytes` is the parent's `memory.max` and `ram_used_bytes <=
ram_limit_bytes` always holds); **one** service-wide sampler starts on the first
subscriber and stops on the last; sampling happens **once per tick** and is
fanned out, so cost is independent of the subscriber count; an idle workspace (no
live cgroup) costs **zero** cgroup reads and reports RAM 0; each stream holds at
most **one pending frame** (coalesce, never queue; drops are counted); and
`?interval=` is clamped to a configured **floor** (below it → `422`). The whole
feature is gated on `metrics.enabled` and advertised in `GET /v1/meta`.

### 2.2a Write-rate ceiling (SAMPLED, default off)

A workspace's write RATE can be capped so one workspace running `dd`/`git
clone`/a build cannot starve every other workspace's IO. Unlike the disk TOTAL,
RAM and CPU — which are kernel-enforced (the per-workspace filesystem and the
per-workspace cgroup) — this ceiling is **SAMPLED**: the workspace's real disk
growth is measured once per metrics tick (one `statfs`) and a run whose
**sustained** rate exceeds the ceiling is stopped with the distinct
`rate_limited` status. `io.max` cannot limit this on the host (the image is on
ZFS and the bio is generated outside the cgroup), and a byte-bucket in the
engine's own write path would cover only API uploads, not a tenant's own
process — hence the sampled growth, which sees every byte.

- **DEFAULT OFF.** Enable with `disk: { write_rate_enabled: true }` plus a
  per-workspace `limits.write_rate_mb_s` (or the server default). `0` = no
  ceiling. A value above the workspace's own `disk_mb` is `422` (a workspace
  cannot write its whole disk in under a second). The feature rides the metrics
  sampler and needs `metrics.enabled`.
- **Sustained, not instantaneous.** `disk.write_rate_samples` (default 3)
  **consecutive** over-limit samples are required; a single burst is not killed
  and any under-limit sample resets the counter.
- **A rate-kill is distinct**: a tenant's own process ends `rate_limited`; the
  engine's own write path (an API write/upload or an archive extraction) has no
  run to stop, so it is refused `429 write_rate_exceeded`. Both paths enforce the
  same sampled ceiling.
- **The overshoot is a number, not a kernel boundary.** For the shipped defaults
  (`N = 3`, interval `3 s`) a workspace can write up to `9 × write_rate_mb_s`
  MiB above the line before the kill. See [DECISIONS.md](DECISIONS.md) ADR 95.

### 2.3 Files (per workspace)

All paths relative to workspace root; leading `/` allowed and normalized. All
endpoints enforce path safety. Browsing and viewing are first-class:

- `GET /v1/workspaces/{id}/files?path=/?limit=&offset=` → `{"path","entries":[{"name","path","type":"file|dir|symlink","size","mode","mod_time","target?","target_inside?","runnable?"}],"count","total","truncated"}`. Directories first, then files, each byte-wise by name; a bounded page (`caps.file_list_max`, default 1000) with `count`/`total`/`truncated`. Listing a file is `422 not_directory`, never an empty `200`. A regular file whose extension maps to an interpreter carries `runnable: "<interpreter>"`.
- `GET /v1/workspaces/{id}/files/stat?path=/main.py` → one entry's metadata (`type`, `size`, `mode`, `mod_time`, and for a file `content_type`/`sha256`/`binary`/`runnable`); `404` when missing.
- `GET /v1/workspaces/{id}/files/content?path=/main.py[&encoding=base64][&meta=true][&offset=&limit=][&start_line=&end_line=]` → file bytes (`Content-Type` detected, `Content-Disposition: attachment; filename=...`); JSON wrapper if `?meta=true`. Byte range (`offset`/`limit`, `limit` ≤ `file_read_mb`) and line range (`start_line`/`end_line`, 1-based inclusive) are mutually exclusive. `sha256` hashes exactly the returned bytes; `range`/`total_size`/`whole_file` make partial delivery explicit; binary content is detected (NUL/invalid UTF-8 in a bounded sample) and reported.
- `GET /v1/workspaces/{id}/files/download?path=/big.bin` → streams the entire file (constant memory), exempt from `file_read_mb`, with `Content-Type`/`Content-Length`/`Content-Disposition`; a directory is `422`.
- `PUT /v1/workspaces/{id}/files/content?path=/main.py` body `{"content":"...","encoding?":"text|base64"}` → 204 (missing `content` → 400; writing over a directory → 422 `not_directory`)
- `DELETE /v1/workspaces/{id}/files?path=/main.py` → 204
- `POST /v1/workspaces/{id}/files/mkdir` body `{"path":"/src"}` → 201
- `POST /v1/workspaces/{id}/files/rename` body `{"from":"/a","to":"/b"}` → 204
- `POST /v1/workspaces/{id}/files/upload` multipart `file` + `path` → 201
- `POST /v1/workspaces/{id}/files/archive` multipart `archive` + `path?` +
  `overwrite?` + `dry_run?` → 200 `archiveResult` (upload-and-extract one step)
- `POST /v1/workspaces/{id}/files/extract` body
  `{"path":"/bundle.zip","dest":"/out","overwrite?":false,"dry_run?":false}` → 200
  `archiveResult` (extract an archive already in the workspace)
- `GET /v1/workspaces/{id}/files/archive?path=/project` → `application/zip`
  (export a directory tree; only ZIP is written, RAR is read-only)

Enforce output/body caps (§1.8) and disk quota (§1.5). Operating on the wrong
node type (a file where a directory is required, or vice versa) is a `422`
`not_directory`; it is never `404` when the path exists and never `500`.

### 2.3.1 Native archives (upload-and-extract, in-place extract, export)

Archives are handled **in the engine** (Go `archive/zip` and
`github.com/nwaples/rardecode/v2`); no host `unzip`/`tar`/`unrar` is ever
invoked, so every refusal is deterministic and identical on every host. Formats
are detected from **content** (magic bytes), never the filename. ZIP and RAR4/5
are read; only ZIP is written (**RAR is read-only** — its encoder is
proprietary). Capability is advertised in `GET /v1/meta` under `archives`.

- `POST /v1/workspaces/{id}/files/archive` multipart `archive` (required) +
  `path` (dest, default `/`) + `overwrite` + `dry_run` → 200 `archiveResult`
  `{"format","destination","entries_total","entries_extracted","entries_skipped","entries_rejected","bytes_written","dry_run","entries":[{"name","path","type","size","mode","rejected","reason?","skipped","skip_reason?"}]}`
- `POST /v1/workspaces/{id}/files/extract` body `{"path","dest?","overwrite?","dry_run?"}` → 200 `archiveResult` (source is a workspace file, read under `archives.upload_mb`).
- `GET /v1/workspaces/{id}/files/archive?path=/dir` → the zip bytes
  (`Content-Type: application/zip`); a non-directory is `422 not_directory`.

**Safety (the archive is untrusted input).** Every entry name goes through the
workspace path cleaner; a traversal, absolute name, drive letter or intra-archive
collision is refused with `422 archive_unsafe_entry`, and a symlink/hardlink
entry is refused rather than materialized. Privileged mode bits
(`setuid`/`setgid`/sticky) are stripped (`4755`→`0755`). The **whole** archive is
validated before any byte is written, so a refusal leaves **no** partial tree;
the bytes actually written are bounded by a **streaming** budget, so a lying
header cannot overshoot `max_total_mb`/`max_file_mb`. A collision with
`overwrite:false` (the default) is `409 archive_entry_exists` and the existing
content is preserved. `dry_run:true` validates, reports and writes nothing (and
does not fail on a rejected entry).

**Stable refusal codes.** `415 unsupported_archive` (unknown content or a format
disabled by `archives.formats`), `422 archive_encrypted`, `422
archive_multivolume`, `413 archive_too_large`, `413 archive_too_many_entries`,
`413 archive_ratio_exceeded`, `422 archive_unsafe_entry`, `409
archive_entry_exists`. See [DECISIONS.md](DECISIONS.md) ADR 92.

### 2.4 Exec (sync)

- `POST /v1/workspaces/{id}/exec` body `{"cmd":["ls","-la"],"cwd":"/","env":{"K":"V"},"timeout_sec":30,"stdin":""}` → 200 `{"exit_code":0,"stdout":"...","stderr":"...","duration_ms":12,"truncated":false}`

Hard cap timeout (default 60 s). Captures capped at §1.8.

### 2.5 Runs (async)

- `POST /v1/workspaces/{id}/runs` body either:
  - `{"kind":"file","file":"/main.py","args":[...],"cwd":"/src","interpreter":"python3","env":{...},"timeout_sec":300,"no_timeout":false}`
  - `{"kind":"command","argv":["python3","main.py"],"cwd":"/src","env":{...},"timeout_sec":300,"no_timeout":false}`
  → 202 `{run}`
- `GET /v1/workspaces/{id}/runs?status=&kind=&limit=&offset=` →
  `{"runs":[...],"total":N,"limit":L,"offset":O}`
- `GET /v1/workspaces/{id}/runs/{runId}` → `{run}`
- `DELETE /v1/workspaces/{id}/runs` → 200 `{"deleted":N}` (purge finished history)
- `DELETE /v1/workspaces/{id}/runs/{runId}` → 204 (delete one finished run)
- `POST /v1/workspaces/{id}/runs/{runId}/stop` → 202 (idempotent; SIGTERM→SIGKILL per §1.11)
- `GET /v1/workspaces/{id}/runs/{runId}/logs?stream=stdout|stderr&offset=0&limit=65536` → `{"data":"...","offset":0,"next":123,"eof":false,"truncated":false}`

**`cwd` (working directory).** Both kinds accept `cwd`, relative to the workspace
root (leading `/` normalized), defaulting to the workspace root. It is validated
at request time to exist and be a directory, else **`422`**; a bad `cwd` is
never a deferred `503` (defect N-1). `cwd` is a *working directory*, not a
relative-path prefix for `file`: the target file is always resolved against the
workspace root and passed to the interpreter as an absolute sandbox path
(`/workspace/<file>`).

**Interpreter selection.** `kind=file` is no longer hardcoded to Python. The
interpreter is chosen by file extension from an allowlist (`.py`→`python3`,
`.js`/`.mjs`/`.cjs`→`node`, `.sh`/`.bash`→`bash`, `.pl`/`.pm`→`perl`), or by an
explicit `interpreter` field that must itself be in the allowlist. An
unsupported extension with no override is **`422`** naming the supported set; an
interpreter outside the allowlist is **`422`**. The allowlist and the covered
extensions are reported on `/v1/meta` (`interpreters`, `runnable_extensions`).
The built argv is `[<interpreter>, /workspace/<file>, ...args]`.

**Discoverability.** A file's `stat`/listing entry carries
`runnable: "<interpreter>"` when its extension maps to one (absent otherwise), so
a UI can offer "run this file" without guessing.

**Precedence.** `argv` (raw command) and `kind=file` are mutually exclusive:
supplying both is **`422`** (never a silent choice). `kind=file` builds argv from
the interpreter, file and `args`; `kind=command` executes `argv` verbatim.

**Unbounded runs.** `no_timeout:true` requests a run with **no wall-clock
limit** (runs until it exits or is stopped), distinct from `timeout_sec:0`
("use the workspace default"). It is gated twice — server
`max_limits.allow_unlimited_runs` **and** workspace `limits.allow_unlimited_runs`
must both be true — else **`422`** naming the level that refused. Default
**deny**. The run record reports `unlimited:true` and `timeout_sec:null`; it is
never stored with a fake finite number. An unbounded run still holds a slot of
`max_runs` and its cgroup share, so a runaway process can occupy the workspace's
capacity indefinitely until stopped (see §2.9 and DECISIONS.md #60).

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
 "status":"running|exited|killed|timed_out|failed","created_at":"...","last_activity_at":"...",
 "started_at":"...","ended_at":"...","exit_code":null,"reason":"...",
 "diagnostic":"... (only for failed)","output_bytes":0,"output_truncated":false}
```

**Lifecycle.** `running` → `exited` (shell exits), `killed` (DELETE /
workspace delete / suspend / service shutdown), `timed_out` (idle timeout or
absolute lifetime), or `failed` (the relay could not start — a PTY or sandbox
launch failure). Each terminal transition carries a `reason`; a `failed` session
also carries an actionable `diagnostic` and never leaves a traceback in its
output. All terminal states are final.

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
`lifetime_sec`, `output_kb`, `max_input_kb`, `shell`, `kill_grace_sec`,
`start_probe_ms`. The service starts with none of them set.

Run JSON:

```json
{"id":"run_...","workspace_id":"ws_...","kind":"file|command","argv":[...],"cwd":"/","status":"queued|running|exited|killed|timeout|failed|interrupted|oom|rate_limited","exit_code":0,"timeout_sec":300,"unlimited":false,"interpreter":"python3","started_at":"...","ended_at":"...","logs":{"stdout_bytes":0,"stderr_bytes":0,"truncated":false}}
```

`cwd` is always present. `timeout_sec` is `null` exactly when `unlimited` is
`true`. `interpreter` is present for a `kind=file` run and omitted otherwise.
`status: "oom"` marks a run the kernel's OOM killer ended at the workspace's
memory ceiling — distinct from a clean `exited` and from an explicit `killed`.
`status: "rate_limited"` marks a run the engine's write-RATE watchdog stopped
because the workspace's sustained disk growth exceeded its `write_rate_mb_s`
ceiling (see §2.2a) — distinct from `exited`, `failed`, `killed` and `oom`.

**`POST /exec` does not create a run record.** Only the asynchronous
`POST .../runs` persists a `Run`; `exec` returns its output inline and leaves no
`run_` id, no log files and no listing entry. A client that wants a pollable,
auditable record must use `runs`.

**Listing is filtered and paginated.** `GET .../runs` accepts `status`
(repeatable or comma-separated; each value must be one of
`queued,running,exited,killed,timeout,failed,interrupted,oom,rate_limited`, else `400`), `kind`
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

### 2.5.2 Egress and quick tunnels

**Egress (`allow_net`).** An `allow_net:true` workspace gets **its own network
namespace**, connected to the public internet only through **chained slirp4netns**
instances: `workspace ns --slirp(UNIX)--> relay ns --slirp(UNIX)--> host`. The
engine-owned relay namespace carries a **blackhole route for every
operator-private range** (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
`169.254.0.0/16`, `100.64.0.0/10`), so a workspace reaches the public internet
and a public resolver but **never the host LAN, the host's own services, or
another workspace**. It creates **zero host interfaces, routes or veth pairs**;
the host's topology is unchanged. `allow_net:false` gets a private, empty
namespace (no egress at all). `allow_net:true` is never a host-network share.
See [DECISIONS.md](DECISIONS.md) for the recorded host-topology hazard this
replaces.

**Quick tunnels.** `cloudflared` (quick tunnels only: no account, no token) is on
`PATH` inside the sandbox:

- `POST /v1/workspaces/{id}/tunnels` body `{"port":5000}` → `201
  {tunnel:{id,workspace_id,port,url,status,created_at}}` where `url` is the
  `https://<name>.trycloudflare.com` address **captured from cloudflared's own
  output** with a bounded wait. The engine never fabricates a URL: if none is
  observed within `tunnels.url_timeout_sec` the create fails with **`504
  tunnel_url_unavailable`** and leaves no record. Quick tunnels are
  **unauthenticated and rate limited** by Cloudflare; repeated creation in a
  short window can return `429`, reported as the same `504
  tunnel_url_unavailable` (cloudflared's own text in `details.reason`). The
  quick-tunnel **URL is ephemeral** — it changes on every start, so nothing may
  cache it; a named tunnel is the eventual answer and is out of scope. See
  [DECISIONS.md](DECISIONS.md) #70.
- `GET /v1/workspaces/{id}/tunnels` → list (status refreshed from the live
  process); `GET /v1/workspaces/{id}/tunnels/{tunnelID}` → one;
  `DELETE /v1/workspaces/{id}/tunnels/{tunnelID}` → `204`, and the public URL
  stops working.
- Honest failure contract: unknown id → `404`; foreign principal → `403`; bad
  body / port out of range (`<1`, `>65535`) or a privileged port (`<1024`) →
  `400`; server/workspace switch off or no `allow_net` → `422 tunnels_disabled`;
  per-workspace cap → `422 too_many_tunnels`; no URL in time → `504
  tunnel_url_unavailable`.
- A tunnel **dies with its workspace** (`stop`/`suspend`/`DELETE`) and startup
  reaps orphaned cloudflared processes. The engine runs cloudflared from the
  host inside the workspace's network namespace; with the default
  `identity.hide_provider_tools` it is **not** on `PATH` inside the workspace, so
  a tunnel is started through the API (a tenant needing a raw cloudflared can be
  given it by an operator via `identity.tools_dir`).

**Config.** `network: {enabled, netns_dir, relay_netns, slirp_path, mtu,
resolver, start_timeout_ms, egress_proxy}` and `tunnels: {enabled,
max_per_workspace, url_timeout_sec, cloudflared_path}`, plus
`limits.allow_tunnels` (the per-workspace opt-in). Both blocks are
**default-deny**: `enabled` defaults to `false`, and a workspace can opt in to
tunnels only when the server switch is on
and it has `allow_net`.

### 2.6 Auth

Middleware `auth.Bearer(cfg.Keys)` extracts `principal_id`. Every workspace
endpoint checks `workspace.owner_id == principal_id` → else 403 `forbidden`.

#### 2.6.1 Scope boundary (non-goals)

sandboxd is a **headless sandbox engine**; the bearer keys are **service
credentials held by an operator-owned frontend** (for example a Telegram bot
product, built separately and not specified here), and end users never see,
hold, or send them. The auth implementation matches that scope, and the
following are **explicitly non-goals of this repo** — enforcement belongs to the
consuming frontend:

- **Static config keys only** — `keys:` in `config.yaml`, each `{key,
  principal_id}`; no dynamic key store or DB-backed auth.
- **Identity is only a `principal_id` string** — there is no user entity; a
  workspace records `owner_id` and nothing else about a person.
- **No self-sign-up** — the service cannot create accounts, users or keys.
- **No per-key quota or expiry** — a key never expires and carries no per-key
  request/usage ceiling (`max_limits` is per-*workspace*, not per-principal).
- **No rate limiting** — nothing throttles a valid key.
- **No billing** — the service meters nothing and charges no one.

Accounts, sign-up, per-user quota, plans, rate limiting, billing and credential
issuance are owned by the frontend, which calls this service as a trusted
backend client. See [DECISIONS.md](DECISIONS.md) #54 and
[ARCHITECTURE.md](ARCHITECTURE.md) §1 "Who consumes sandboxd".

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
| `tunnels_disabled` | 422 | Tunnels forbidden: server switch off, workspace not opted in, or no workspace egress. |
| `too_many_tunnels` | 422 | Concurrent tunnels exceed `tunnels.max_per_workspace`. |
| `workspace_suspended` | 409 | Work (exec/run/package management/session) requested on a suspended workspace. |
| `too_many_sessions` | 409 | Concurrent terminal sessions exceed `sessions.max_per_workspace`. |
| `session_ended` | 409 | Operation on a terminal session (input/signal/resize, or output after DELETE). |
| `validation` | 400 | Bad JSON, unknown field, failed `validate` tag, bad filter/query. Also carries **405** for method-not-allowed. |
| `unprocessable` | 422 | Unsafe path/symlink escape, `cwd` escape, command not found, a limit `<= 0` or above the server maximum, invalid package requirement, conflicting byte/line range params, a `limit` above `file_read_mb`. |
| `not_directory` | 422 | Wrong node type: listing a file, reading/downloading a directory as a file, writing over a directory. The path exists, so never `404`. |
| `payload_too_large` | 413 | Body/file over a hard cap (including a `GET .../files/content` `limit` above `file_read_mb`). |
| `insufficient_storage` | 507 | Workspace disk quota exceeded. **Emitted as a string literal in `internal/api/errors.go`; not defined as a constant in `internal/model/errors.go`.** |
| `timeout` | 504 | **Defined but never emitted** by any handler (retained in the vocabulary and the OpenAPI enum). |
| `tunnel_url_unavailable` | 504 | Cloudflared did not print a public URL within `tunnels.url_timeout_sec`; the create fails and leaves no record. |
| `unavailable` | 503 | nsjail missing, or nsjail could not set up/launch the sandbox; or the network/tunnel layer is not configured. |
| `unsupported_archive` | 415 | Archive content matches no supported format (or a format disabled by `archives.formats`). |
| `archive_encrypted` | 422 | Encrypted ZIP or RAR (no password is available). |
| `archive_multivolume` | 422 | Multi-volume RAR (`partN.rar`). |
| `archive_too_large` | 413 | Total or single-entry uncompressed size over `archives.max_total_mb`/`max_file_mb`. |
| `archive_too_many_entries` | 413 | More entries than `archives.max_entries`. |
| `archive_ratio_exceeded` | 413 | Entry uncompressed/compressed ratio over `archives.max_ratio`. |
| `archive_unsafe_entry` | 422 | Entry escapes the destination (traversal/absolute/drive/collision) or is a symlink/hardlink. |
| `archive_entry_exists` | 409 | Entry would clobber an existing file and `overwrite` is false. |
| `internal` | 500 | Unexpected server error. |

The code is not always a 1:1 mirror of the status: a **405 method not allowed**
carries `code:"validation"`.

### 2.9 Named long-lived projects/services — NOT IMPLEMENTED (supervised services)

The engine has **no notion of a named, long-lived "project" or "service"** in
the *supervised* sense: there is no way to declare a persistent process with a
**desired state**, no **restart-on-crash**, no **autostart**, and no
**reconcile loop**. A frontend that wants a "project that stays up" must
implement that by re-issuing runs/sessions (and retrying on crash) on its side.

What **does** exist natively now (and did not before) is a **workspace-level
stop-everything**:

- `POST /v1/workspaces/{id}/stop` stops **everything in the workspace** — every
  run, every live session, and every **detached** process (a process started
  from a session that carries no run/session attribution). A caller can therefore
  reliably stop the dev server / bot / worker they launched in their own
  terminal, which was previously invisible to and unreachable from the API.
- `GET /v1/workspaces/{id}/processes` lists every live process of the workspace
  so a caller can see (and count) what is running before stopping it, and
  `POST /v1/workspaces/{id}/processes/{processID}/stop` stops one process tree
  (cgroup-scoped, so a `setsid()`-escaping workload is caught too).

What is **still not implemented** (the honest non-goal):

- A **run** or a **session** is one process tree / one shell. It runs until it
  exits, is stopped, or times out; it is **not restarted**, and there is no
  record of a *desired* state to reconcile against.
- A **workspace** has no "start my service" state: `suspend`/`stop` stop
  *current* work (and `stop` now reaches detached processes too), but they do not
  define a service to bring back up.
- There is **no supervisor**: nothing re-launches a crashed workload, nothing
  keeps a workload running across a service restart (a restart moves any
  queued/running run to `interrupted`, any running session to `killed`, and
  **reaps any orphaned workload and cgroup left by a hard daemon death** — but
  does not restart it), and there is no autostart on workspace resume.

Stated precisely: **stop-everything for a workspace exists natively now;
supervision (desired-state restart/autostart) remains a documented non-goal for
this version** (v0.0.20). Where such a capability would belong if built — an
engine-level supervisor versus the frontend restarting runs — is recorded in
[DECISIONS.md](DECISIONS.md) #61. **It is deliberately not implemented in this
version.**

## 3. SECURITY / NSJAIL

Implement `internal/executor/nsjail.go` that builds a per-run nsjail config
file (temp file under `data/tmp/`) and invokes `nsjail --config <file>`. The
config template `configs/nsjail_python.cfg.tmpl` must include:

- namespaces: pid, mount, ipc, uts, user, net. The network namespace is **never**
  the host's. With no engine-managed namespace nsjail creates a fresh, empty one;
  with one, the runner enters the workspace's own namespace and renders
  `clone_newnet:false`.
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
- DNS: mount `/etc/resolv.conf` naming **public resolvers**, and a **generated**
  `/etc/hosts` holding only generic localhost entries, read-only **only when
  `allow_net=true`**. The workspace reaches those resolvers through its
  chained-slirp4netns egress; the host's own resolver is deliberately never
  published to a workspace. The host's own `/etc/hosts` is **never** mounted
  (it names the operator's server FQDN and LAN address). A workspace without
  `allow_net` must not see any resolver or hosts file at all.
- host-identity masking (`identity.enabled`, default on): bind-mount a compiled
  preload shim read-only at the opaque `/run/.rt.so` and a read-only
  `/etc/ld.so.preload` naming it, and inject the workspace's own
  `cpu_cores`/`memory_mb` (as the engine-neutral `LSM_SYS_*` variables) so the
  shim answers `uname`, `sysinfo`, `sysconf`,
  `sched_getaffinity`, `getauxval` and the libc version strings with the
  workspace's values instead of the host's. The same shim interposes the
  filesystem entry points so **the mechanism itself is invisible** to the tenant:
  `/etc/ld.so.preload`, the shim's own `/run/.rt.so` and the legacy `/sandboxd`,
  `/run/sandboxd` all report `ENOENT` to `ls`/`cat`/`stat`/`test`/`find` and
  Python's `os.stat`/`os.listdir`, while the loader (which reads the preload list
  before any shim is mapped) still reads it and `env -i` stays masked. The mounts
  are `mandatory:true`: a mask that cannot be applied aborts the run rather than
  leaking. See [DECISIONS.md](DECISIONS.md) #71–#73, #79.
- **profile curation** (`identity.hide_provider_tools`, default on): the host's
  `/usr` is replaced inside the workspace with a tree **materialized from the
  workspace's named profile** — an explicit, reviewable **allowlist**. It holds
  only the commands the profile names plus engine base commands, the shared
  libraries, the ELF loader and `/usr/lib/ssl`, and the data/library trees the
  profile attaches (e.g. `/usr/lib/python3.12` for python, `/usr/lib/git-core`
  for git). `python3`, `node`, `bash`, `perl` and `git` therefore work only when
  the profile provides them, and `apt`, the `gcc`/`binutils` chain, `/usr/include`,
  `/usr/libexec` and the platform's own `/usr/local` tooling do not exist at all.
  `/etc/{passwd,group,ssl/certs}` are filtered of every entry naming the provider
  (`zerops`, `vshosting`, `zabbly`). The engine's engine-named filter (`nsjail`,
  `slirp4netns`, `sandboxd`, `hostmask`) still applies on top of a profile; a
  profile entry naming the engine or platform is **refused** (fail loud). This
  removes the engine's own runner, its helper binaries, the engine-named
  environment, the provider account and the provider-named systemd units while
  giving a tenant exactly the toolchain its profile promises. See
  [DECISIONS.md](DECISIONS.md) #74, #77, #83.
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
helper** as the entry command, with the helper source passed to the interpreter
**inline** (`python3 -c "<source>"`) so it never exists as a file inside the
sandbox (see [DECISIONS.md](DECISIONS.md) #78). The helper
allocates the PTY, runs the shell as a session leader, and relays a
line-delimited JSON control protocol from its stdin; its stdout is the terminal
byte stream. `pty.fork()` opens `/dev/ptmx` and needs devpts, so a **PTY
session** additionally bind-mounts the host's `/dev/pts` and `/dev/ptmx`
read-only (only for a session; one-shot `exec` does not); a private devpts
instance does not work because `openpty()` opens `/dev/ptmx`. A terminal is a
device, not a filesystem or process view, and the session otherwise runs with the
same uid/gid, mounts and seccomp policy as any workload — the sandbox is never
weakened. Client `data` is written to the PTY as raw bytes and is never
interpreted as a helper command. If the PTY cannot be allocated the session is
recorded `failed` with a `diagnostic`, never `running` with a traceback. See
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
