# Decisions

All resolved decisions for sandboxd, grouped by the feature version in which
they landed (v0.0.5, then v0.0.6 … v0.0.16 below). Each decision is fixed for its
group; deferred work is noted explicitly. Do not re-litigate these without a new
version. The service version string (`model.Version`) tracks the highest group
implemented, currently **v0.0.16** (#71–#73); the HTTP contract stays `/v1`
(see #17).

1. **Isolation model**: per-run user namespaces + shared `sandbox` UID/GID + per-run cgroups.
   _Rationale_: cheap, portable isolation that works without a per-user UID allocation scheme.
   Per-UID-per-workspace deferred to v0.1.
2. **Network**: disabled by default; workspace field `allow_net: bool` (default false).
   _Rationale_: least privilege by default; no host allowlist exists in v0.0.5.
   **Extended in v0.0.14 (#64):** `allow_net:true` no longer shares the host network
   namespace. It gives the workspace its own namespace with egress through chained
   slirp4netns, isolated from the host LAN.
3. **Log retention**: keep the last 100 runs per workspace, pruned on new run start.
   _Rationale_: bounds disk growth without a background GC.
4. **Exec vs Runs**: `exec` (sync) and `runs` (async) are separate endpoints but share one executor.
   _Rationale_: avoids duplicated process logic while presenting distinct UX.
5. **Disk quota**: tracked by bytes written via file APIs and rejected when exceeding `disk_mb`,
   plus a periodic `du` re-check on workspace load.
   _Rationale_: cheap accounting on the hot path with a reconciling audit on load.
6. **Auth**: static API keys from `config.yaml`; each key maps to one `principal_id`.
   _Rationale_: trivial to operate in v0.0.5; model keys as `{key, principal_id}` entries, 1:N allowed.
7. **Default limits per workspace**: cpu_cores=1, memory_mb=512, disk_mb=1024,
   max_wall_time_sec=300, max_runs=3.
   _Rationale_: conservative defaults safe for a small shared host.
8. **Output caps**: exec stdout/stderr 256 KB each; run logs 10 MB per stream; JSON body 1 MB;
   uploads 50 MB; file read 5 MB; file write 10 MB.
   _Rationale_: caps memory and storage while remaining generous for typical scripts.
9. **Truncation flag**: `truncated: true` when any cap was hit.
   _Rationale_: lets clients distinguish silent truncation from complete output.
10. **Path safety**: `filepath.Clean` + `filepath.Rel` + `os.Lstat` on each component; reject `..`
    escapes and symlinks pointing outside the workspace root.
    _Rationale_: defense in depth against traversal and symlink escape.
11. **Process kill**: `exec.Cmd` starts with `SysProcAttr{Setpgid: true}`; on stop/timeout send
    SIGTERM to `-pgid`, wait 3 s grace, then SIGKILL to `-pgid`.
    _Rationale_: guarantees the whole process tree dies, not just the direct child.
12. **Errors**: consistent JSON envelope `{"error":{"code","message","details"}}` with codes in the spec.
    _Rationale_: machine-readable errors for all clients.
13. **IDs**: `run_id` / `ws_id` are ULIDs with prefixes `run_` / `ws_`.
    _Rationale_: sortable, collision-resistant, visually distinguishable identifiers.
14. **Streaming**: SSE for logs is v0.1; v0.0.5 polls logs with `offset`/`limit`.
    _Rationale_: keeps v0.0.5 dependency-free; polling is sufficient for CLI clients.
15. **Binary safety**: file reads/writes support `?encoding=base64`; text is the default.
    _Rationale_: JSON transport needs an explicit binary encoding.
16. **Health probes**: `GET /healthz` returns 200 while the process is alive; `GET /readyz` returns
    200 only if the data dir is writable AND the nsjail binary is found, else 503 with a reason.
    _Rationale_: separates liveness from readiness for orchestrators.
17. **Version**: the service version string is `0.0.16`, the highest feature group
    resolved in this file.
    _Rationale_: single source of truth (`model.Version`) for the implementation
    version. It is deliberately **decoupled from the `/v1` API contract version**
    (`model.APIVersion`), which changes only on a breaking contract change: the
    service has gained eleven feature groups (v0.0.6 … v0.0.16) without one, so
    tying the two numbers together would force a spurious `/v2`. The initial
    `0.0.5` string predated those groups and was stale; it now tracks them.
18. **cgroup v2 is a hard requirement; `--cgroupns=host` is mandatory.** nsjail applies memory,
    pids and cpu ceilings via cgroup v2, which requires write access to the host/root cgroup
    hierarchy. A container started without `--cgroupns=host` cannot apply them, so nsjail aborts
    before `execve`. `/readyz` reports `cgroups`, and a failed launch is returned as
    `503 unavailable` with nsjail's own diagnostic — never a silent non-zero exit.
    _Rationale_: the resource model treats cgroup ceilings as active; making the requirement
    explicit and observable is safer than silently running without ceilings.
19. **rlimit units**: nsjail's `rlimit_as` and `rlimit_fsize` are **megabytes** (verified against the
    installed build), so the rendered config passes MB values, never bytes.
    _Rationale_: passing bytes multiplied every ceiling by ~10^6, effectively disabling them.
    **Corrected by ADR 82:** `rlimit_fsize` is `disk_mb`, but `rlimit_as` is **not** `memory_mb` — it
    is `memory_mb + headroom` (floored at 2048 MB), because a managed runtime's virtual reservations
    count against `RLIMIT_AS`. The physical ceiling is `cgroup_mem_max = memory_mb` (bytes).
20. **User environment is allowlisted.** Only `PYTHONPATH`, `PYTHONUNBUFFERED`,
    `PYTHONDONTWRITEBYTECODE`, `PYTHONHASHSEED`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ` pass through.
    Loader-injection variables (`LD_*`, `DYLD_*`, `GLIBC_TUNABLES`) are rejected by name.
    _Rationale_: a preloaded object could alter the interpreter the service chose to run, breaking
    the trust model that the jail contains a known entry point.
21. **Sandbox launch failures are explicit.** A non-zero nsjail exit that carries a fatal/error
    diagnostic on its log fd is classified as `ErrSandboxUnavailable` and surfaced as
    `503 unavailable` (sync) or a `failed` run with the diagnostic in the log (async).
    _Rationale_: an empty `stdout` exit 255 is indistinguishable from a blocked syscall unless the
    failure is reported.
22. **cwd is honored and validated** relative to the workspace root (mounted at `/workspace`); a
    `cwd` that would escape the root is rejected with `422`.
    _Rationale_: a documented contract field must have an effect and must not weaken path safety.
23. **Run recovery on startup.** Runs left `queued`/`running` by a prior process are moved to the
    terminal `interrupted` state; shutdown stops all runs and drains under a bounded deadline.
    _Rationale_: a run record must always reach a terminal state a polling client can observe.
24. **`kind=file` targets are validated at request time** via `workspace.Resolve` (`422` unsafe,
    `404` missing); the target must exist before the run is accepted.
    _Rationale_: deferring an invalid request to execution time returns success for a request that
    cannot succeed.

## v0.0.6 — workspace control plane

25. **Runtime-mutable limits via `PATCH`.** `PATCH /v1/workspaces/{id}` merges a subset of fields;
    an absent field or an explicit `null` keeps the current value (a `null` never resets a limit to
    zero) — implemented with pointer fields so omission and zero are distinguishable.
    _Rationale_: a workspace should not have to be recreated to change one setting, and a partial
    update must never silently clobber untouched fields.
26. **Server-level maxima are hard, not soft.** `max_limits:` in config defines a ceiling
    (defaults: cpu 8, memory 8192 MB, disk 51200 MB, wall 3600 s, runs 64). A create or PATCH value
    above a maximum is **rejected with `422`**, never silently clamped.
    _Rationale_: silently clamping hides a client mistake and makes a write non-idempotent from the
    caller's point of view; an explicit error is honest.
27. **Lowering a limit affects the next run/exec.** A run already in flight finishes under the
    limits it started with and is **not** retroactively killed.
    _Rationale_: killing live work as a side effect of a config change is surprising; operators who
    want an immediate stop have the workspace `stop` control.
28. **Workspace status is `active` | `suspended` and is enforced.** `exec` and run creation on a
    suspended workspace return **`409 workspace_suspended`**; reads (files, runs, logs) keep working.
    _Rationale_: a new code is needed because the condition is a policy refusal on an existing,
    owned resource — none of the existing codes (`conflict`, `forbidden`, `unprocessable`) express it
    without overloading a meaning.
29. **`suspend` stops the workspace's running runs; `stop` does not suspend.** Both are idempotent
    and report what changed (`changed`, `stopped_run_ids`). `suspend` = stop work *and* refuse new
    work; `stop` = stop current work but keep accepting.
    _Rationale_: the two controls differ only in whether new work is accepted; giving each a single
    clear purpose is easier to reason about than a flag on one endpoint.
30. **Run listing is filterable and paginated with a total.** `GET .../runs` accepts `status`
    (repeatable or comma-separated), `kind`, `limit`, `offset`, returns newest-first, and includes
    `total` so a client can page without guessing.
    _Rationale_: a flat list is unusable once a workspace has many runs; the total makes offset
    pagination deterministic.
31. **Purging run history refuses while a run is live.** `DELETE .../runs` (and `DELETE
    .../runs/{id}`) delete only finished runs and return **`409`** with nothing deleted if any run
    is queued/running.
    _Rationale_: never lose the logs of an in-flight run; an all-or-nothing purge is predictable.
32. **Usage disk size is a bounded walk, not a cached value.** `GET .../usage` walks at most
    100 000 directory entries; a larger tree yields a lower bound with `disk_bytes_complete: false`.
    _Rationale_: keeps the endpoint cheap on a large workspace and avoids a cache whose staleness
    window would itself need explaining; a documented lower bound is more useful than an unbounded
    scan.
33. **`last_activity_at` tracks mutations.** Workspace metadata records the most recent run start or
    mutation (PATCH, suspend/resume/stop, purge); `usage` exposes it alongside `created_at`.
    _Rationale_: an operator needs to see whether a workspace is still in use.
34. **Stopping an already-finished run reports its status, never errors.** `POST .../runs/{id}/stop`
    on a terminal run returns `202` with the stored status.
    _Rationale_: stop is documented as idempotent; a confusing failure on a race (the run ended
    between the client's check and the stop) would be worse than a clear no-op.

## v0.0.7 — two-tier Python packages

35. **Two package tiers, separate trust levels.** Python packages come from a **shared read-only
    tier** (one host directory installed once, visible to every workspace, bind-mounted `rw:false`)
    and a **private per-workspace tier** (a directory inside the workspace's own data dir,
    bind-mounted `rw:true`, writable by that workspace's sandbox uid only). _Rationale_: heavy/common
    dependencies stay on the image and are not duplicated per workspace, while a workspace can still
    `pip install` at runtime without being able to poison the shared set for anyone else.
36. **`sys.path` mechanism: an injected `PYTHONPATH`.** The two sandbox mount points
    (`/opt/python/shared`, `/opt/python/private`) are added through the already-allowlisted
    `PYTHONPATH` variable, with the **shared path first**. _Rationale_: it needs no interpreter
    mutation, works identically for `python3 -c`, `python3 file.py` and `python3 -m module`, and the
    ordering is explicit. Keeping the shared path first means a private package of the same name
    shadows the shared one **only for its own workspace** (private dirs are per workspace), which is
    the intended, safe override; the shared set can never be shadowed across workspaces because no
    other workspace can write it. A `.pth`/`sitecustomize.py` would require writing into the
    interpreter's site-packages (which we mount read-only) and copying would duplicate the set.
37. **The shared tier is bind-mounted read-only; the private tier is writable and sandbox-owned.**
    The nsjail config uses `rw:false` for the shared mount and `rw:true` for the private mount. When
    the orchestrator is privileged the private directory is chowned to the sandbox uid (the shared
    directory is deliberately never touched). _Rationale_: `rw:false` is the kernel-enforced
    guarantee that no workspace can modify or poison the shared set; ownership gives the workload
    write access to its own tier only, keeping the existing `sandbox` identity-drop model intact.
38. **A missing, empty or unreadable shared directory degrades to private-only.** The tier is probed
    on every run, and a workspace run/exec with no usable shared dir mounts only the private tier and
    starts normally. _Rationale_: the shared set is an optimization, not a dependency; a
    misconfigured image must not take the whole service down.
39. **Requirement specifiers are strictly validated.** `POST /packages/install` rejects anything that
    is empty, longer than 200 characters, does not start with an alphanumeric, or contains a
    character outside `[A-Za-z0-9._+~*=<>,!\[\]-]` (with balanced brackets) — with **422**. The
    values are also passed to `pip` as individual argv elements, never through a shell.
    _Rationale_: defense in depth against command-line injection (`-e`, `--upgrade`,
    `pkg; rm -rf /`, `../../etc/passwd`, ...): an allowlist plus no-shell execution means hostile
    input can never reach a command line.
40. **Installation runs inside the workspace's own sandbox.** `pip install --target
    /opt/python/private` executes under nsjail as the unprivileged sandbox uid with the shared tier
    read-only, so package setup code cannot run as the orchestrator or write outside the workspace.
    Wall-clock is bounded by `packages.install_timeout_sec` (default 120 s) and captured output by
    `packages.install_output_kb` (default 256 KiB). _Rationale_: installing is running untrusted
    package build code; it must be contained by the same isolation as any other workload.
41. **Removing all private packages requires an explicit `?all=true`.** `DELETE .../packages` with
    `?name=<pkg>` removes one distribution (using its `RECORD` where available); no parameter is a
    **400**, and `?name=` together with `?all=true` is a **400**. _Rationale_: an empty-name delete
    must never silently wipe a workspace's packages.

## v0.0.8 — resolver mounts and verification scope

42. **The resolver is mounted only when `allow_net` is set.** `/etc/resolv.conf` and `/etc/hosts`
    are bind-mounted read-only into the jail **iff** the workspace has `allow_net:true`; a
    workspace without network gets neither file. _Rationale_: sharing the host network namespace
    (`clone_newnet:false`) without a `resolv.conf` made `getaddrinfo` fail with
    `Temporary failure in name resolution` even though raw-IP TCP worked, which broke `pip install`
    and any hostname-using workload; conversely, leaking the host resolver into a no-network jail
    would publish the host's search domain and nameservers to a workload with no network to use
    them. The alternative — mounting the resolver unconditionally — was rejected because it widens
    what a `allow_net:false` workspace can observe for no benefit.
    **Superseded in part by #64 (v0.0.14):** the mechanism that made name resolution fail was the
    host-network share, which no longer exists; the resolver mount is retained, but it now names
    **public resolvers** reached through the chained-slirp4netns egress, never the host's.
43. **Sandbox behaviour that needs privileges is tested against the rendered config.** Where the
    host cannot run a real nsjail (no `nsjail` binary / unprivileged orchestrator), the assertions
    are made on the **generated nsjail configuration** — e.g.
    `TestRenderConfigMountsResolverWhenNetAllowed` /
    `TestRenderConfigOmitsResolverWhenNetDenied` assert the exact `mount { src, dst, is_bind, rw }`
    blocks and `clone_newnet`, and the package-mount tests assert the shared tier renders `rw:false`
    and the private tier `rw:true`. _Rationale_: the mount/rlimit/uidmap behaviour *is* the rendered
    text, so asserting on it is a faithful test of the decision that needs no kernel privileges;
    every claim that genuinely needs a live jail (actual `EROFS`, actual name resolution, actual
    namespace isolation) is labelled **[requires privileged container]** in
    [VERIFICATION.md](VERIFICATION.md) rather than implied to have run. The rejected alternative —
    skipping the assertions entirely when nsjail is absent — would leave the resolver and tier
    decisions untested on exactly the hosts most likely to regress them.

## v0.0.9 — first-class terminal sessions

44. **A stdlib-only Python PTY relay helper, not an alternative.** nsjail has
    **no PTY/tty option at all** and cannot allocate a controlling terminal; the
    sandbox image has **no `socat`** and **no `expect`**. Its `python3` does have
    `pty`, `termios` and `fcntl`, and `pty.fork()` gives a child a real
    `/dev/pts/N` (measured). A ~200-line stdlib-only script is therefore the
    smallest portable mechanism that allocates a real PTY. The rejected
    alternatives — `socat`/`expect` (absent), a C helper (a toolchain and a new
    artifact for no gain), or running the shell without a PTY (defeats the whole
    feature: no job control, no `isatty`, no interactive programs) — are all
    worse. The helper is embedded in the binary and materialized per session,
    so the image needs nothing beyond `python3`.
45. **The helper is mounted read-only; sessions reuse the whole sandbox.** The
    helper lives in the session's private `/tmp` and is bind-mounted `rw:false`
    at `/run/sandboxd`. A read-only mount means the interactive shell cannot
    rewrite the code that allocates its terminal, and the helper runs with the
    **same uid/gid, mounts and seccomp policy as any workload** — no sandbox
    control is relaxed. The isolated private `/tmp` is why `/run/sandboxd` is
    never confused with the workspace. _Rationale_: a terminal must not be a
    weaker path into the host than a run.
46. **A stdin pipe, not nsjail's `--pass_fd`.** nsjail does have `--pass_fd`,
    `--skip_setsid` and `--forward_signals`, and a session could in principle
    pass a PTY master/worker fd across the jail boundary. That would put a host
    PTY fd into the sandboxed process and make the service responsible for PTY
    lifecycle outside the jail. Instead the helper allocates the PTY *inside* the
    sandbox and the service keeps the child's stdin as an ordinary pipe it owns
    (`LiveProcess.Write`). _Rationale_: stdin needs no new nsjail feature, keeps
    the terminal entirely inside the isolation boundary, and matches the
    existing "keep a live `*exec.Cmd` per process" machinery.
47. **Signals are delivered to the foreground process group, not the run.**
    `POST .../sessions/{id}/signal` relays `INT`/`TERM`/`QUIT`/`HUP` to the PTY's
    foreground process group, so Ctrl-C interrupts the running command **without
    ending the session**; the same session then runs another command. This is
    deliberately different from `POST .../runs/{id}/stop`, which cancels the
    context and kills the whole process group. _Rationale_: an interactive
    terminal's core affordance is interrupting one command and continuing; a
    control that ended the session on every signal would not be a terminal.
48. **Idle and lifetime are separate, both configurable.** `sessions.
    idle_timeout_sec` (default **900**) ends a session after no input, signal,
    resize or output; `sessions.lifetime_sec` (default **3600**) is an absolute
    ceiling regardless of activity. Both reach status `timed_out`, with reason
    `idle_timeout` / `lifetime`. A create request's `timeout_sec` may **shorten**
    the lifetime but never lengthen it past the configured maximum.
    _Rationale_: an interactive shell can legitimately sit idle (a person
    thinking), so an idle timeout alone would be wrong; a lifetime alone would
    let an abandoned session hold resources forever. Two clearly-labelled
    reasons make both observable.
49. **Restart marks orphaned sessions honestly; DELETE retains the record.**
    No sandbox process survives a service restart, so at startup every session
    persisted `running` is moved to `killed` with reason `interrupted` (never
    left looking live). Service shutdown and workspace delete kill live sessions
    (reasons `service_shutdown`, `workspace_deleted`). `DELETE` is idempotent and
    **retains** the terminal record and its output log: a later
    `input`/`signal`/`resize` (or `output` after DELETE) reports **409
    `session_ended`** rather than a plain `not_found`, and the record is removed
    with the workspace. _Rationale_: a client that raced a session ending
    deserves a precise, documented answer; and pretending a crashed session is
    still running is a lie the run model already refuses to tell
    (`interrupted`).
50. **Session output reuses the run-log mechanism.** The helper's stdout is the
    terminal byte stream, written through the **same** `cappedWriter` to
    `sessions/{id}/output.log` and read by `GET .../output` with the identical
    `offset`/`next`/`eof`/`truncated` semantics as `GET .../runs/{id}/logs`.
    _Rationale_: a second streaming mechanism (SSE, websockets, a ring buffer)
    would duplicate the run-log contract and its tail recipe; reusing it means a
    client's existing poll loop works against a session unchanged.
51. **The control protocol is injection-proof by construction.** Client `data`
    is relayed as the JSON string value of an `input` message and written to the
    PTY as literal bytes; it is never re-parsed. A payload containing
    `{"type":"close"}` therefore arrives as terminal text (verified by
    `TestSessionHostileInput` and `TestPTYHelperInjectionProof`), and the helper
    injects no JSON/sentinels into the output stream, so no framing noise ever
    reaches the client. _Rationale_: the input channel and the control channel
    share one pipe; the only safe design is that the client's bytes can never be
    promoted into a command.
52. **Mount the host `/dev/pts` and `/dev/ptmx`, read-only, only for a PTY
    session.** nsjail has no PTY support of its own, so the relay helper
    allocates the terminal with `pty.fork()` — which opens **`/dev/ptmx`** and
    needs a usable **devpts**. The one-shot `/dev` (null/urandom/zero only)
    leaves both absent, and the helper dies with `OSError: out of pty devices`.
    A **private** devpts instance (`fstype: devpts`,
    `newinstance,ptmxmode=0666,mode=0620`) was measured **not** to help: it makes
    `/dev/pts/ptmx` appear, but `openpty()` opens `/dev/ptmx`, which that
    configuration does not create, so it still fails — now with ENOENT. Binding
    the **host's** `/dev/pts` **and** `/dev/ptmx` is the measured minimum that
    makes `pty.fork()` succeed inside the jail; both are read-only binds of
    device nodes, added **only when `Request.PTY` is set** so ordinary `exec`
    keeps the narrower `/dev` (a one-shot process needs no terminal). _Rationale_:
    the sandbox now sees the host's pty pool — **a terminal, not a filesystem or
    process view** — which is exactly the capability a terminal requires and
    nothing more; uid/gid, seccomp, mount and namespace isolation are otherwise
    unchanged. See the measured before/after in
    [VERIFICATION.md](VERIFICATION.md) §9b.
53. **A PTY (or sandbox-launch) failure is a `failed` session with a
    diagnostic, never a `running` one.** The relay writes `sandboxd-pty-error:`
    to stderr and exits non-zero when it cannot allocate a terminal; the executor
    recognises that marker and returns a `SandboxError`, and the service records
    the session as `failed` / `sandbox_unavailable` with the actionable
    `diagnostic` (never a Python traceback in the output). `StartSession` also
    waits a short, bounded window (`sessions.start_probe_ms`, default 250 ms,
    capped at 2 s) for a relay that dies immediately, so even the **create
    response** is honest rather than a `201 … running` that is already dead.
    _Rationale_: the original bug was silent at the API level — a client only
    learned of the failure by reading the output stream. The same treatment
    covers any immediate relay death (e.g. a missing interpreter), because the
    helper's non-zero exit arrives within the probe window.

## v0.0.10 — product boundary (engine vs frontend)

54. **sandboxd is a headless sandbox engine; a separate frontend product owns the users.**
    _Context_: sandboxd authenticates with static bearer keys (`internal/auth/auth.go`:
    `{key, principal_id}`, constant-time compare, no expiry, no quota, no per-key limits);
    there is no rate limiting anywhere (`grep -rniE "ratelimit|limiter|throttle" internal/ cmd/`
    → no matches), no per-principal/per-plan quota (`internal/config/` has only `max_limits`,
    per-workspace request ceilings), and no user entity (`internal/` has only `owner_id`/
    `principal_id`). The consumers were previously described only loosely, so a reader could
    conclude an end user holds a key.
    _Decision_: the service is a pure sandbox engine and its clients are **trusted backend
    services**. The bearer keys are **service credentials held by an operator-owned frontend**
    (for example a **Telegram bot product**, built in its own repo — its internals are **not
    decided in this document**); end users never see, hold, or send them. The frontend owns
    accounts, authentication, plans, quotas, rate limiting, billing and credential issuance,
    and calls sandboxd on the user's behalf. End-user accounts, sign-up, per-user quota, plans,
    rate limiting, billing and credential issuance are **out of scope for this repo**.
    _Consequences_:
    - **The service must not be exposed directly to end users.** With no authenticating frontend
      in front of it, anyone who can reach the listener and holds a key has a workspace.
    - **Abuse/quota policy is the frontend's responsibility**, so the operator must treat the
      frontend as the trust boundary. Stated honestly: **a leaked service key is not
      rate-limited or quota-limited by this service by design today** — `internal/auth` only maps
      a key to a `principal_id` and never throttles; the per-workspace `max_limits` are per-
      workspace ceilings, not per-principal quotas.
    - **Blast radius of a leaked key is host-level resource consumption.** A leaked key can
      create workspaces and run workloads up to the per-workspace `max_limits` (and the host's
      own capacity) with no per-key bound to stop it; the sandbox confines *what* each run may do
      (no network by default, rlimit/cgroup ceilings) but does not bound *how much* an attacker
      with a valid key may start. Key rotation and abuse control therefore live in the frontend.
    _Rationale_: naming the boundary explicitly keeps the two products from depending on each
    other's internals, and makes the security consequence of a leaked key a documented property
    rather than a surprise. The alternative — implying each end user holds a key — would misstate
    the trust model and invite exposing the engine directly.

## v0.0.11 — first-class file browsing and viewing

55. **Wrong-node-type file operations are `422 not_directory`, never `404` or `500`.**
    _Context_: listing a path that is a file returned `200 {"entries":[]}`; reading a directory
    as a file returned `404`; writing over a directory returned `500`. The internal
    `errNotDirectory` was never translated by `mapError`.
    _Decision_: a new error code `not_directory` (HTTP 422) is emitted whenever the path exists
    but is the wrong kind of node — listing a file, reading/downloading a directory as a file, or
    writing over a directory. Sealed variants `errNotDirectory` / `errIsDirectory` are translated
    in `internal/api/errors.go`. A missing path stays `404`.
    _Rationale_: the path exists, so `404` ("not found") is a lie and `500` blames the server for
    a client mistake; `422` names the real cause. A dedicated code lets a file-browser client
    distinguish "no such path" from "you asked a file to behave like a directory".

56. **A directory listing is a bounded, metadata-rich, deterministically ordered page.**
    _Context_: a viewer needs name/type/size/mode/mtime per row without a second call, and a
    directory can hold very many entries.
    _Decision_: `GET /files` returns `{path, entries, count, total, truncated}` where each entry
    carries `name`, `path`, `type` (`file`/`dir`/`symlink`), `size` (0 for a directory), `mode`
    (octal), `mod_time` (RFC3339) and, for a symlink, `target` plus `target_inside`. Ordering is
    directories first, then files, each byte-wise by name. The page is bounded by
    `caps.file_list_max` (default 1000) with `count`/`total`/`truncated` and `limit`/`offset`
    paging.
    _Rationale_: one call renders a browser row set; a documented total and a stable order make
    paging reliable; a bounded page keeps a pathological directory from forcing an unbounded
    response.

57. **`sha256` hashes exactly the returned bytes; partial delivery is explicit.**
    _Context_: the read path hashed the truncated prefix, so the checksum verified a file the
    client never received, and a client could not tell a whole-file hash from a partial one.
    _Decision_: the read response reports `sha256` over exactly the bytes in the response, plus
    `range {offset, length}`, `total_size`, and `whole_file` (true only when the response is the
    entire file). `offset`/`limit` select a byte range; `start_line`/`end_line` select a line
    range; the two are mutually exclusive (`422` otherwise); a `limit` above `file_read_mb` is
    `413`. A separate `GET /files/download` streams the entire file (constant memory, exempt from
    `file_read_mb`, still bounded by `disk_mb`). Binary content is detected from a bounded
    8192-byte sample (NUL or invalid UTF-8) and reported as `binary`, forced to base64 so it is
    never mangled into invalid JSON.
    _Rationale_: an integrity field that cannot be trusted to describe the payload is worse than
    none; making the range and the whole-file flag explicit lets a client verify what it got and
    resume paging. The bounded sample keeps detection cheap, and its limit is documented.

58. **`PUT /files/content` requires `content`; an explicit empty string writes a 0-byte file.**
    _Context_: a body without `content` wrote a 0-byte file with `204`, so a client bug silently
    destroyed a file.
    _Decision_: `putFileRequest.Content` is a `*string`; absent → `400 validation`, explicit `""`
    → a valid 0-byte write.
    _Rationale_: "I forgot the field" and "I intend an empty file" are different requests and must
    not be indistinguishable.

## v0.0.12 — native file execution, unbounded runs, workspace stop

59. **A workspace `stop` stops runs *and* sessions; `suspend` stops both too.**
    _Context_: `Service.StopWorkspace` called `StopAllRuns` only and left live
    terminal sessions running, so "stop the workspace's operations" did not
    actually stop the workspace. `SuspendWorkspace` already called
    `StopWorkspaceSessions`.
    _Decision_: `POST /workspaces/{id}/stop` stops every run **and** every live
    session (reason `workspace_stopped`), returning `stopped_run_ids` /
    `stopped_run_count` **and** `stopped_session_ids` / `stopped_session_count`.
    `suspend` already stopped both (reason `workspace_suspended`). The existing
    response fields keep working, so a client depending on them is unaffected.
    _Rationale_: a single control that claims to stop a workspace's work must not
    leave half of it running; surfacing both counts lets a client report exactly
    what it stopped. Verified by the acceptance suite (`stop.*` checks) which
    leaves zero processes of the workspace's tree. **Extended in v0.0.13** (#63):
    `stop` now also stops **detached** processes (a process started from a session
    that carries no run/session attribution), which were previously unreachable.

60. **Unbounded runs are opt-in twice, default deny, and recorded honestly.**
    _Context_: the product owner wants a user's file to run until the user stops
    it, not to a wall-clock limit; the engine capped everything at
    `max_wall_time_sec` (default 300, hard max 3600).
    _Decision_: a run may be requested with `no_timeout: true` — deliberately
    **distinct** from `timeout_sec: 0` ("use the workspace default") so unlimited
    cannot be asked for by omission. It is accepted only when **both**
    `max_limits.allow_unlimited_runs` (server) and the workspace
    `limits.allow_unlimited_runs` are true; otherwise `422`, with
    `details.config` naming the refusing level. Default is **false** at both
    levels. The run record reports `unlimited: true` and `timeout_sec: null`; it
    is never stored with a fake finite number, so a reader can tell "no limit"
    from "an hour". The executor renders nsjail `time_limit: 0` (verified to mean
    "no limit") and layers no timeout context; only an explicit stop cancels it.
    _Rationale_: an unbounded run is a real capacity risk, so it must be an
    explicit, doubly-consented act, not a default. Two levels let an operator
    forbid it globally while a workspace otherwise would opt in. Recording the
    truth (null, not 3600) is what the owner asked for ("do not store a fake
    number"). The honest risk — an unbounded run still holds a `max_runs` slot
    and its cgroup share, so a runaway process can occupy the workspace's
    capacity indefinitely until stopped — is stated in the docs. The kill path
    is proven, not assumed: stopping an unbounded run leaves zero surviving
    processes and status `killed`.

61. **A named long-lived project/service is a documented non-goal for this
    version.** _Context_: the engine has no supervisor, no restart-on-crash, no
    autostart, and no lifecycle beyond a single run or a single session; a leaked
    expectation of a "project that stays up" would be a gap discovered in
    production. _Decision_: this is written down as **not implemented** in
    [SPEC.md §2.9](SPEC.md), and this entry records where it would belong if
    built. _Rationale_: the two candidate homes are (a) an **engine-level
    supervisor** — a durable "service" record with a desired-state/reconcile
    loop, restart policy and autostart-on-resume, which fits the engine because
    only it can see process death and hold the cgroup/sandbox lease — versus (b)
    the **frontend** restarting runs/sessions itself — which needs no engine
    change but re-implements liveness, retry, and crash detection behind the
    engine's back, and cannot observe a crash the engine does not report. The
    engine-level supervisor is the more faithful place (the engine already owns
    `RecoverRuns`/`RecoverSessions`), but it is a new subsystem with its own
    decisions (restart backoff, crash-loop caps, desired-state storage), so it is
    deliberately deferred. Stated honestly: **neither exists today.** **Nuance
    added in v0.0.13** (#63): a workspace-level **stop-everything** *does* now
    exist natively (see SPEC.md §2.2/§2.9); only the *supervised* named service
    (desired-state, restart-on-crash, autostart) remains the non-goal.

62. **A run's `cwd` is validated at request time; a bad `cwd` is 422, not 503
    (defect N-1).** _Context_: `cwd` was not carried on a bad path, so a missing
    `cwd` reached nsjail and its `chdir` failure surfaced as `503 "sandbox
    unavailable"`, falsely implying the isolation layer was broken.
    _Decision_: `cwd` (both kinds) is resolved against the workspace root and
    checked to exist and be a directory before the run is accepted; a bad `cwd`
    is `422`. In the executor, an nsjail exit-255 diagnostic that indicates a
    failed `chdir` is classified as `ErrInvalidCwd` (→ `422`) rather than
    `ErrSandboxUnavailable` (→ `503`); a genuine setup failure stays `503`.
    _Rationale_: `cwd` is caller-controlled, so a typo must read as a bad request
    and never as a broken sandbox; the distinction is exactly the difference
    between "fix your request" and "page an operator".

## v0.0.13 — native process control (enumerate and stop anything in a workspace)

63. **A workspace can enumerate and stop every process it is running — including
    one started from a session — using the per-jail cgroup, not the process
    group.**
    _Context_ (measured against the live service): a process started inside a
    session (`nohup python3 probe.py &`) was invisible to and unreachable from the
    API. `DELETE` of the session returned `200` and the process survived; `POST
    /workspaces/{id}/stop` also left it alive; it appeared in **no** API surface
    (`GET .../runs` → `0`, `GET .../usage` → `{"active_runs":0}`, and
    `grep -c processes api/openapi.yaml` → `0`), so there was **no API that could
    stop it at all**. Yet it belongs to a workspace: it is a child of that
    workspace's sandbox and its cgroup (`/sys/fs/cgroup/NSJAIL.<pid>`) contains
    it. The single most common "stop my thing" action — stopping the dev server /
    bot / worker launched in one's own terminal — was therefore impossible.
    _Decision_: (a) every sandbox is tagged with the workspace and (for a run or
    session) its kind/source via environment variables
    (`SANDBOXD_WORKSPACE`/`SANDBOXD_KIND`/`SANDBOXD_SOURCE`), which every
    descendant inherits; (b) `GET /v1/workspaces/{id}/processes` enumerates the
    members of each `NSJAIL.*` cgroup and maps each host pid back to its owning
    workspace/run/session by that tag, reporting an unattributable member honestly
    as `detached` (never hidden); (c) `POST /v1/workspaces/{id}/processes/{id}/stop`
    and the extended `POST /v1/workspaces/{id}/stop` kill through the **cgroup**
    (`cgroup.kill`), so a process that called `setsid()` and left its process
    group is still caught; (d) when a run/session ends the engine **removes its
    cgroup**, and on **startup** it **reaps orphaned workloads and cgroups** left
    by a hard `SIGKILL` daemon death.
    _Rationale_: **you cannot stop what you cannot see**, so enumeration and kill
    share one primitive. The process group is insufficient by construction — a
    workload that calls `setsid()`/`setpgid()` escapes `kill(-pgid)` but cannot
    escape its cgroup — so `pgid`-only killing (decision #11) is kept as a
    fallback but is not the mechanism for enumeration or for a guaranteed stop.
    The `id` is a hash of `(workspace, pid, kernel start time)` because a bare host
    pid is not stable across a restart (pids are recycled); the hash distinguishes
    two processes that reuse a pid and stays stable for the process's life. The
    rejected alternative — killing by `pgid` and accepting that a `setsid()`
    workload survives — would report success while a process lived, which the
    house rule forbids ("a stop that reports success while a process survives is
    worse than a `500`"). The tag is an environment variable rather than a mount or
    a name because it is inherited by every descendant with no extra plumbing.
    Stated honestly: sandboxed code can re-write its own environment before
    exec'ing a child, so the tag is not cryptographically bound — **but** a
    workspace id (`ws_<ULID>`) is opaque and unguessable from inside a sandbox
    (there is no `/proc` of the host, no workspace enumeration, and the id is not
    derived from anything the workload can see), so a workload cannot learn
    another principal's workspace id to mis-tag itself, and the **stop is
    cgroup-scoped** so a kill can only ever affect the jail the addressed process
    is actually in. Cross-*principal* steering is therefore infeasible; within one
    principal's own workspaces there is no privilege boundary to cross. Verified
    end-to-end under real nsjail by `scripts/process-stop-check.sh` (25
    assertions), which includes a `setsid()`-escaping workload stopped by the
    single-process stop, zero survivors after a workspace stop, a cgroup count that
    returns to baseline, and an orphan reaped by a second start after a forced
    daemon death.

## v0.0.14 — per-workspace egress and quick public tunnels

64. **The host-topology design is ABANDONED and deleted; egress is delivered by
    chained slirp4netns.**
    _Context_: `allow_net:true` used to render `clone_newnet:false`, putting the
    workload **inside the host's own network namespace**, so it could bind host
    ports (including `22`/`8080`/`8081`) and reach the operator's LAN
    (`10.9.112.0/22`). An earlier "per-workspace networking" attempt tried to fix
    this by creating host `veth` pairs, assigning addresses and adding routes
    (`ip link add … type veth peer`, `ip addr add 10.199.0.1/30 dev …`, `ip route
    add 10.200.0.0/16 via …`). **That attempt took the target host off the network
    three times in one evening (18:24Z, 20:54Z, 21:42Z), each time needing an
    out-of-band restart**, and even had a before/after guard that did not
    prevent it. It is **deleted from this tree** (`internal/network/{network,egress,
    forwarder,ports}.go` veth/hub code and the published-ports
    `internal/api/ports.go` + `internal/service/ports.go` are removed).
    _Decision_: an `allow_net:true` workspace gets its **own** network namespace
    connected to the public internet only through **chained slirp4netns**:
    `workspace ns --slirp(UNIX socket)--> relay ns --slirp(UNIX socket)--> host`.
    slirp4netns speaks to its parent over a **UNIX socket** and wires a tap
    interface only inside the child namespace, so there are **zero host
    interfaces, zero host routes and zero veth pairs** — the measured proof is
    `scripts/tunnel-check.sh` §8 (host `ip -o link`/`ip route` identical before and
    after). The engine-owned **relay** namespace carries a blackhole route for
    every operator-private range (`10.0.0.0/8`, `172.16.0.0/12`,
    `192.168.0.0/16`, `169.254.0.0/16`, `100.64.0.0/10`), so a workspace reaches
    the public internet and a public resolver but **never the host LAN, the host's
    own services, or another workspace**. `allow_net:false` gets a private, empty
    namespace. **The rule is absolute: no `ip link add/del`, `ip addr add`, `ip
    route add`, `ip netns add` or interface rename on the host, ever.**
    _Rejected alternatives_: (a) `slirp4netns` alone without the relay — measured
    to NAT the guest through the **host's** routing table, so the guest reached
    `10.9.112.7:22`, `10.9.112.1:53` and `10.9.112.4:443` (the operator's own
    infrastructure); `--disable-host-loopback` does not fix a routed LAN. (b)
    iptables/nft NAT — **not installed** on the target host. (c) any host
    veth/bridge/macvlan — the three-outage hazard above.

65. **`createNetns` must never bind the host namespace.**
    _Context_: a first version of the namespace creator read
    `/proc/<pid>/ns/net` immediately after `Start()`, **before the child had
    exec'd `unshare`**; in that window the child was still in the host namespace,
    so the bind put the **host** netns at `/run/netns/sbxdrelay` and the "relay"
    connected to the host network (measured: the host grew a `tap0` and a default
    route). This is the same `$!`-is-`sudo`-not-`unshare` trap the earlier
    experiments documented.
    _Decision_: `createNetns` waits until the child's namespace **identity**
    differs from the host's before binding, then verifies the **bound**
    namespace's identity and refuses a namespace that resolves to the host.
    Identity is `stat` dev:inode (a bind-mounted nsfs file is **not** a symlink, so
    `readlink` on it fails — an earlier check used `readlink` and always "saw" the
    host). `ensurePrivateNetns` replaces a stale path that resolves to the host;
    `EnsureRelay` uses it. A namespace that is not distinct from the host is never
    returned.
    _Rationale_: a host-netns bind is exactly the catastrophic failure the whole
    design exists to prevent, so it is defended structurally, not by a diff.

66. **Quick Cloudflare tunnels, and the URL is only ever an observed one.**
    _Context_: the operator wants one command to expose a workspace port publicly:
    "لوكل هوست بورت ٥٠٠٠ بأمر واحد يطلع للعام".
    _Decision_: `POST /v1/workspaces/{id}/tunnels {port:5000}` runs
    `cloudflared tunnel --url http://localhost:5000` **inside the workspace's own
    namespace** and returns the `https://<name>.trycloudflare.com` URL captured
    from **cloudflared's own output** with a bounded wait
    (`tunnels.url_timeout_sec`). If no URL is observed the create fails with **504
    `tunnel_url_unavailable`** and leaves no record — **the engine never
    fabricates a URL**. The user's own simplest path works too: cloudflared is on
    `PATH` in the sandbox (the sandbox mounts `/usr`), so a shell inside the
    workspace can run `cloudflared tunnel --url http://localhost:5000` directly.
    _Rationale_: quick tunnels need **no account and no token**, so the feature
    needs no Cloudflare credential at all; a fabricated URL would be a lie the
    house rule forbids. **Quick tunnels only** — no named tunnel, no account, no
    `GIT_TOKEN`-style secret.

67. **Tunnels are default-deny (two keys) and die with their workspace.**
    _Decision_: a tunnel is created only when `tunnels.enabled` (server) **and**
    `limits.allow_tunnels` (workspace) are true, **and** the workspace has
    `allow_net` (cloudflared must reach Cloudflare); otherwise `422
    tunnels_disabled` with `details.config` naming the refusing level. This mirrors
    the `allow_unlimited_runs` two-key pattern exactly. A tunnel dies with its
    workspace (`stop`/`suspend`/`DELETE`), and **startup reaps orphaned
    cloudflared processes**, matched by `comm == cloudflared` plus a quick-tunnel
    argv (`tunnel … --url …`) so an operator's own named tunnel is never touched.
    _Rationale_: exposing a port to the public internet is a real capacity and
    abuse risk and must be a doubly-consented, default-deny act; and a public URL
    that outlives its workspace is a leak.

68. **The egress layer and the relay are default-deny and platform-honest.**
    _Decision_: `network.enabled` (default false) gates the whole egress fabric.
    When it is off, or when `nsenter`/`unshare`/`mount`/`slirp4netns` is missing,
    an `allow_net:true` workspace gets a private **empty** namespace (no egress) —
    never the host network. `/readyz` and `/v1/meta` report `network` and
    `tunnels` with `enabled`/`ready`/`reason` truthfully; `PATCH` rejects an
    opt-in the server forbids with `422` (the F-22 discipline). A slirp interface
    that does not come up within the bound aborts the setup with a reason rather
    than hanging.
    _Rationale_: an operator must be able to see, from `/readyz`, whether egress
    and tunnels can actually serve; and a missing tool must degrade to **less**
    capability, never to a host-network share.

69. **The privileged-port rule applies to tunnels.**
    _Decision_: a tunnel for a port below `1024` is refused with `400`, and a port
    outside `1–65535` with `400`. _Rationale_: the sandbox runs as an
    unprivileged uid and can never bind a privileged port, so accepting the
    request would return a URL that can never answer; refusing it honestly is the
    only truthful option.

## v0.0.15 — deterministic tunnel acceptance and the quick-tunnel operational limit

70. **Quick tunnels are unauthenticated and rate limited; the engine reports that
    honestly and a rate-limited suite run is skipped, not failed.**
    _Context_: quick tunnels need no Cloudflare account and no token, which is
    exactly why the feature needs no credential (decision #66) — but the same
    no-account path is **shared and rate limited by Cloudflare**. Creating
    several in a short window makes `cloudflared` print `quick tunnel
    provisioning failed with status 429` and exit. This is a limit of the
    **external service**, not of the engine: the engine already surfaces it as
    the documented `504 tunnel_url_unavailable` (with cloudflared's own text in
    `details.reason`) and **never fabricates a URL**. `scripts/tunnel-check.sh` is
    not hermetic for that reason — the runs themselves could exhaust the quota,
    and a suite that goes red because somebody else's quota ran out trains people
    to ignore it, which is worse than not having the suite.
    _Decision_: the harness detects the rate limit precisely — it matches
    cloudflared's literal `provisioning failed with status 429` in
    `details.reason`, **never a timeout** — retries the create with bounded
    backoff (`TUNNEL_RETRIES` attempts, `TUNNEL_BACKOFF` seconds base), and, if
    the limit persists, reports exactly the tunnel-dependent assertions
    (`tunnel.url_is_trycloudflare`, `tunnel.public_fetch_serves_workspace`,
    `tunnel.delete_204`, `tunnel.url_dead_after_delete`, `lifetime.cf_running_before`,
    `lifetime.cf_gone_after_delete`) as a distinct **skipped-environment** third
    state, prints the reason, and exits **0**. Any **other** tunnel failure — a
    crash, a missing binary, a wrong status — stays a hard **FAIL**; the skip is
    never blanket. The tally separates the three states
    (`21 passed / 0 failed / 6 skipped (cloudflare quick-tunnel rate limit)`),
    and the documented `504` + `tunnel_url_unavailable` path is **always**
    asserted, including while rate limited.
    _Operational limit_: quick tunnels are **unauthenticated and rate limited**;
    repeated creation in a short window can return `429`; a production deployment
    that expects many users creating tunnels must account for this — **a named
    tunnel with a Cloudflare account is the eventual answer and is explicitly out
    of scope**. **The quick-tunnel URL is ephemeral**: it changes on every start,
    so nothing (client, cache, UI) may store or rely on it; the only valid URL is
    the one just observed from cloudflared's own output.

## v0.0.16 — host identity: a workspace must not learn which machine it runs on

### ADR 71 — Threat model: what a tenant must and must not be able to observe

**Asset.** The identity of the machine a workspace runs on, and any fact that
fingers the operator's infrastructure: the public/egress IP, the host name, the
operator's internal DNS names, machine identity, hardware fingerprints, and the
**real** size of the host (cores, RAM).

**Adversary.** A tenant who controls the bytes a workload executes: arbitrary
`python3`/`node`/`bash`/`perl` code inside the workspace, including a terminal
session. They are assumed to be motivated and competent, and to be able to read
anything the kernel exposes to their process, in any language or via libc/raw
syscalls.

**Why it matters here.** `sandboxd` is a multi-tenant code-execution backend.
If a tenant can measure the host's real 64 cores and ~540 GB while their
workspace is capped at 1 CPU / 256 MB, they learn the operator is running a
large shared box and can size a resource-exhaustion attack, correlate
cross-tenant activity by the machine fingerprint, and use the kernel build
string (`7.1.8-zabbly+`, `#ubuntu24.04 …`) and the host FQDN to identify the
platform and its provider. That is inference the operator did not consent to
disclose.

**Boundary of this ADR.** The guarantee is **isolation of inference about the
host**, not secrecy of the code the tenant themselves runs. Everything the
tenant uploads is theirs; the property is only that the *machine* around it is
not fingerprinted.

### ADR 72 — What is already closed, what is structurally closable, and what is not

**Closed by the mount namespace (filesystem view), measured and asserted by
`scripts/identity-check.sh` assertion 1:** `/etc/machine-id`, `/etc/os-release`,
`/etc/hostname`, `/etc/hosts`, `/etc/resolv.conf`, `/etc/hostid`, `/etc/shadow`,
`/proc/version`, `/proc/meminfo`, `/proc/cpuinfo`, `/proc/net/dev`,
`/proc/1/cgroup`, `/proc/self/cgroup`, `/proc/self/mountinfo`, `/sys/class/net`,
every `/sys/class/dmi/id/*`, `/var/run/zerops` and `/run/zerops` are unreadable
(`ENOENT`), because nsjail mounts no `/proc` and no `/sys` (`mount_proc: false`)
and only the specific `/etc` files it needs. `socket.gethostname()` returns the
generic `sandbox`, `hostname -f` fails, `hostname -I` is empty, and the
operator's internal DNS names do not resolve. With `allow_net:false` every
"what is my IP" service fails at the network layer. **This baseline is preserved
and asserted; it is not re-derived here.**

**Closable, and now closed, by a preload shim (syscall view).** A mount hides a
*file*, never a *syscall*. These facts stay reachable after `/proc` and `/sys`
are gone:

| Fact | Reachable by | Leaks |
|------|--------------|-------|
| kernel release/version | `uname(2)`, `platform.uname()` | platform + build date |
| host core count | `sysconf(_SC_NPROCESSORS_*)`, `os.cpu_count()`, `nproc`, `sched_getaffinity(2)` | real machine size |
| host total/available RAM | `sysconf(_SC_PHYS_PAGES)`, `sysinfo(2)` | real machine size |
| base-image patch level | `gnu_get_libc_version(3)`, `confstr(3)`, `platform.libc_ver()` | distro → provider inference |
| CPU capability bits | `getauxval(AT_HWCAP)` | host CPU fingerprint |

**Approach evaluated and adopted: an `LD_PRELOAD` shim named in a read-only
`/etc/ld.so.preload`.** The shim (`internal/executor/hostmask/hostmask.c` →
`hostmask.so`) interposes `uname`, `sysinfo`, `sysconf`, `sched_getaffinity`,
`getauxval`, `gnu_get_libc_version` and `confstr`, and answers with the
**workspace's** limits. It is **stripped and has its `.comment` section removed
at build time** (`-s -fno-ident` plus `objcopy -R .comment`, which is required
because the `.comment` compiler line survives `strip`), so `strings` over it
yields no engine name, no compiler version and no source path. Its in-jail
mount point is opaque (`/run/.rt.so`) and its configuration variables are
engine-neutral (`LSM_SYS_CPUS`, `LSM_SYS_MEM`, `LSM_SYS_REL`, `LSM_SYS_VER`),
because a tenant can `strings` the shim and read its own environment: an
engine-named variable would announce the engine and the fact that a mask is
active. Three properties make it robust against the obvious evasions, all
**measured**:

- **`/etc/ld.so.preload` is read by the dynamic loader for every
  dynamically-linked exec, independent of the environment.** A tenant cannot
  defeat it with `env -u LD_PRELOAD` or `env -i`: `env -i PATH=… nproc` and
  `env -i PATH=… uname -r` still return the masked values
  (`scripts/identity-check.sh` assertion 4). This is the reason `LD_PRELOAD`
  alone (an environment variable, trivially cleared) was **rejected**. The
  compiled-in fallbacks (1 cpu, 256 MB, `0.0.0-sandbox`) keep the `env -i`
  answer generic.
- **Both the shim and the preload list are bind-mounted `rw:false` and the
  mounts are `mandatory:true`.** A tenant cannot replace the shim, and if either
  mount cannot be applied nsjail aborts the run rather than starting a sandbox
  that would leak (`identity-check.sh` asserts the shim and preload files are not
  writable; the Go test asserts the mount is read-only and the render refuses
  with `ErrMaskUnavailable` when the shim cannot be materialized).
- **The shim is not the only layer.** The curated trees in ADR 74 remove every
  engine *name* a tenant could otherwise read (the runner, its helpers, the
  engine-named environment, the relay source). The shim only supplies the
  syscall answers.

**Classification: closed, inherent, and not-yet-closed — kept separate on
purpose.** An earlier version of this ADR filed *every* `dlsym`/`dlopen` route
as "not closable by this mechanism". That was wrong: a **handle-scoped `dlsym`**
of an interposed symbol is a gap in the shim, not a property of preload
interposition, and it is now **closed**. The three classes are kept separate so
the next reader can tell a shut gap from a permanent limit from an open task:

**A. Closed (gaps in the shim/engine, now shut).**
`dlopen("libc.so.6")` returns a SPECIFIC object handle, and `dlsym(handle, …)`
on it searches only libc's local scope — deliberately skipping a preload shim.
Measured before the fix, `ctypes.CDLL("libc.so.6", use_errno=True).uname(buf)`
returned the **real** `7.1.8-zabbly+` **with no raw syscall at all**. The shim
now interposes `dlsym` itself and answers a request for a masked symbol, from
**any** handle, with the shim's own function — the same value the `RTLD_DEFAULT`
path already returned. Measured closed (`scripts/identity-check.sh` assertion 8,
`docs/VERIFICATION.md`): `CDLL("libc.so.6")`, `CDLL("/lib/…/libc.so.6")`,
`libc.dlopen` + `dlsym`, `dlmopen(LM_ID_NEWLM)`, and `dlsym(RTLD_NEXT, "uname")`
all return the masked value.

**Residual inside this class: a handle-scoped `dlvsym(3)`.** `dlvsym(handle,
"uname", "GLIBC_2.2.5")` still reaches the real value. The shim does **not**
interpose `dlvsym`: on this glibc, defining both `dlsym` and `dlvsym` makes a
versioned reference (`dlsym@GLIBC_2.2.5`) bind back to the shim's own unversioned
definition and the dynamic loader's bootstrap then recurses at process start —
measured, reproducible; interposing `dlsym` alone is stable and closes the
measured one-liner. `dlvsym` requires compiled code or a FFI that exposes it
(CPython's `ctypes` does not; `ctypes.CDLL` is `dlsym`-based), so the practical
one-liner is closed; the residual is documented rather than hidden.

**B. Inherently bypassable (properties of the mechanism / the kernel ABI).**

1. **A statically-linked binary** does not run the dynamic loader at all, so it
   is not intercepted and sees the real host values.
2. **A non-glibc runtime (musl, e.g. an Alpine-built binary)** is loaded by
   musl's own loader, which does not read `/etc/ld.so.preload`.
3. **A direct `syscall(2)`** with a raw `uname`/`sysinfo` completely bypasses
   libc and every interposer by construction.

**C. Closed (the mechanism is now INVISIBLE to the tenant; see ADR 79).** An
earlier version of this ADR filed the visible shim + `/etc/ld.so.preload` as an
unavoidable open gap: the loader must read the file at the tenant's own uid, so
`cat /etc/ld.so.preload` appeared to reveal a preload object at an opaque path.
**That classification was wrong and is corrected here.** The shim is already
LD_PRELOADed into every process the tenant runs, so it is in a position to
answer the filesystem questions the tenant would use to discover it
(`open`/`stat`/`access`/`readdir`/`readlink`/`realpath`). It now interposes
those entry points (with glibc-versioned exports) and answers `ENOENT` for
`/etc/ld.so.preload`, its own in-jail path and the engine's legacy directories,
so `ls`, `cat`, `stat`, `test`, `find` and Python's `os.stat`/`os.listdir` all
report the path missing. The hiding is **scoped to processes that already have
the shim loaded**: the dynamic loader reads `/etc/ld.so.preload` *before* any
preload object is mapped, so it is never intersected, and a real exec is still
masked (`env -i uname -r` and `env -u LSM_SYS_* uname -r` both report the masked
values — measured, ADR 79). The mechanism and the exact glibc-version trap are
in ADR 79; the shim's own strings are obfuscated so `strings` over it yields
none of the masked kernel values or variable names.

The zero-artifact goal (no shim file at all, nothing for the loader to read) is
a **separate, still-open** question; the two candidate mechanisms were measured
and neither holds on this host within bounded effort (ADR 79 §"zero-artifact").
That is now a *de-risked* open item: the mechanism being invisible is what the
operator's bar required, and it is met.

The masking layer is therefore **defense-in-depth, not a security boundary**. Its
limits — now split into "closed" and "inherent" — are reported truthfully in
`/readyz` and `/v1/meta` (`identity_masking.limits` says *"a statically-linked
binary, a musl runtime, a raw syscall(2), or a handle-scoped dlvsym(3) bypasses
the shim"*) and printed in the config example.

**The raw-syscall result is a KNOWN, asserted outcome, not a surprise.**
`libc.syscall(63, buf)` (raw `uname`) returns the real `7.1.8-zabbly+`, and
`libc.syscall(99, sysinfo_buf)` returns the host's real `totalram` and `procs`.
`scripts/identity-check.sh` **asserts both as expected** (assertion 9): a tenant
who takes this route is *expected* to see the host values, because no glibc-level
shim can sit under the kernel ABI. **What a tenant actually learns and can do:**
they learn the host's real kernel string, that the box is large (≈540 GB / many
cores) and shared, and the current process count — enough to *fingerprint the
machine and size an attack*, but **not** to read another workspace, escape the
namespace, or reach the host (the mount/pid/net/cgroup namespaces still hold);
the raw syscall is an oracle, not a door. Because the outcome is asserted, the
next reader cannot mistake it for a regression.

**Rejected alternatives.** (a) **`LD_PRELOAD` via the env allowlist** — an
environment variable a tenant clears with `env -i`; measured to be defeated by
that alone. (b) **`seccomp` returning `ENOSYS` for `uname`/`sysinfo`** — breaks
every legitimate runtime (`glibc` calls `uname` during startup; `sysinfo` is used
by many tools), so it fails the "must not break python3/node/bash/perl"
requirement. (c) **A fake `/proc` (`mount_proc` with synthesized `meminfo`,
`cpuinfo`, `version`)** — `uname`/`sysconf`/`sysinfo` do not read `/proc` at
all, so it closes none of the syscall-reachable gaps; it was already decided
that `/proc` is simply not mounted. (d) **A kernel-level fix (namespaces, a
patched kernel)** — out of scope: the operator does not control this host's
kernel (`7.1.8-zabbly+`), and the brief forbids host changes.

### ADR 73 — The masking switch is opt-OUT, truthful, and fail-loud

_Decision_: masking is a server switch `identity.enabled` (default **true**;
`identity-check.sh` and the config example both default it on). It is
opt-**out**, not opt-in, because it is the isolation guarantee, not a feature.
When enabled but not ready, the engine **refuses** the run
(`ErrMaskUnavailable` → `503 unavailable`) rather than launch a sandbox that
would leak — a missing mask is a loud failure, never a silent one — and
`/readyz` reports `503` with `identity_masking.enabled=true, ready=false,
reason=…`. `/v1/meta` reports `identity_masking {enabled, ready, mechanism,
limits}` on every call, so a client can see both the capability and its honest
limits. Turning the switch off is a diagnostic state that deliberately leaks,
and that leak is **asserted** by `identity-check.sh` assertion 5 (the host
kernel and core count are observed) so the switch is verifiable rather than
decorative. The masking answers come from the workspace's own `cpu_cores` and
`memory_mb`, with a floor of one cpu and one MB, so a zeroed limit never renders
a nonsense answer. The kernel strings default to `0.0.0-sandbox` / `#1 SMP` and
are configurable (`identity.kernel_release`/`kernel_version`) precisely so an
operator can pick their own generic value; the engine never substitutes the
host's.

**Measured on the target host this session** (see
[docs/VERIFICATION.md](VERIFICATION.md) §10): with masking on, a workspace
capped at 1 CPU / 256 MB reports `uname.release=0.0.0-sandbox`,
`os.cpu_count()=1`, `sysconf(_SC_PHYS_PAGES)·4096=268435456`, and `env -i nproc=1`;
with masking off it reports `7.1.8-zabbly+`, `2` and `540675186688` — the host.

### ADR 74 — The hosting platform's own tooling is not visible inside a workspace

**Problem.** The sandbox bind-mounts the host's whole `/usr` (`src: "/usr"`,
`dst: "/usr"`, `rw: false`) so a workload gets a normal toolchain. That also
brought in `/usr/local`, which on this platform carries the **operator's own
control binaries** and vendor material. Measured inside a real workspace with
`identity.enabled=true`:

```
$ command -v zcp      → /usr/local/bin/zcp
$ ls /usr/local/bin/  → cloudflared nsjail yq zcp zsc
$ strings /usr/local/bin/zcp | grep -i zerops
    ZEROPS_Y
    ub.com/zeropsio          ← the vendor's GitHub org
```

A full audit (not just `zcp`) found the vendor-named material under the mounted
trees: `/usr/local/bin/{zcp,zsc,yq}` (zsc additionally embeds the vendor's
database templates, `/opt/zerops/bin`, `ZeropsPassword`, and internal
`*.zerops` service names), `/usr/local/sbin/includes/basic_inc`
(`bubenik@vshosting.cz`), the vendor CA sources under
`/usr/local/share/ca-certificates/` (`O = Zerops Infra`, `CN = Zerops Infra Root
CA`), and the Go toolchain at `/usr/local/go` (a large host-tree mount).

**Decision — curate every tree the host mounts.** A new default-on switch
`identity.hide_provider_tools`. The sandbox mounts the host's `/usr`, `/lib`,
`/etc/passwd` and `/etc/group` so a workload gets a normal toolchain and a
working runtime; each of those also carries engine and platform material. When
the switch is on, every such tree is **replaced inside the workspace with a
curated, read-only copy** that drops the engine/platform entries and reproduces
the rest:

- **`/usr/local`** is replaced by an operator-providable curated directory
  (`identity.tools_dir`); empty (the default) sees **no** host `/usr/local`
  material.
- **`/usr/bin`, `/usr/sbin`** are curated copies (`identity.tools_bin_dir`/
  `tools_sbin_dir`; default the host trees). An entry is dropped when its
  **name** is `nsjail`, `slirp4netns`, `runcon`, `setpriv`, `unshare`, `nsenter`
  or `chroot`, or names the provider/engine; or when a regular file's **content**
  contains a provider or engine-distinctive string (`zerops`, `vshosting`,
  `zabbly`, `sandboxd`, `nsjail`, `slirp4netns`). Everything else — `python3`,
  `node`, `bash`, `perl`, `git`, `curl`, the whole distro toolchain — is
  reproduced, so a workspace keeps its toolchain with no engine name reachable.
- **`/usr/lib`** (mounted at **both** `/usr/lib` and `/lib`; the sandbox's `/lib`
  is its own directory, not the host symlink) is curated the same way. This is
  what removes the host tooling under `/usr/lib/{code-server,node_modules}` and
  the provider-named systemd units (`/usr/lib/systemd/system/zerops-*.service`).
- **`/usr/share/doc` and `/usr/share/man`** are curated (this removes the
  `slirp4netns` page).
- **`/etc/passwd` and `/etc/group`** are filtered copies: an entry naming the
  provider or engine is dropped, and a surviving group's **member list** is
  filtered too (the host's `sudo`/`www-data` groups list the operator account
  `zerops`), so `getent passwd`/`id` name no provider.
- **`/etc/ssl/certs`** is rebuilt from the host's own trust store with every
  certificate that **names the provider** dropped — from the per-certificate
  files and from the concatenated `ca-certificates.crt` bundle. The provider's
  name lives in the **decoded DER** subject/issuer, not the base64 PEM text, so
  the filter decodes each certificate before testing (a base64 substring test
  finds nothing; measured). Generic roots are untouched, so TLS keeps working.

The word `hostmask` is a **name-only** token, not a content token: it is a
generic networking term (the inverse of a netmask) used by Python's `ipaddress`
module and node's `netmask` package, so dropping every file that merely
*contains* it would break the Python and Node standard libraries. A file
*NAMED* `hostmask` is still dropped. This is measured: `ipaddress.ip_network(
"10.0.0.0/8").hostmask` works inside the curated tree.

All these mounts are `mandatory: true`: if a curated tree cannot be mounted the
run **aborts** (`503 sandbox_unavailable`) rather than expose the host's
directory — the same fail-loud contract as the masking shim. The overlays are
materialized by the same one-time `masker.prepare()` pass into the service tmp
dir, owned by the orchestrator; a workspace cannot write them, and they are
bind-mounted read-only. On the target host the copies are filesystem clones
(reflinks; measured), so curating ~2.5 GB of host trees costs ~10 s once and
almost no disk.

**Why not "hide the strings by stripping binaries"?** The binary is the
operator's; modifying it would break their tooling and signatures. The workspace
does not need it, so the correct fix is not to mount it.

**The audit is asserted, not assumed.** `scripts/identity-check.sh` assertion 8
runs a `strings` audit over **every** mounted binary path (not just checks that
`zcp` is absent), and the new zero-signal group (assertion 10) audits every
reachable file for a whole-word engine or provider name, asserts no path is
named `hostmask`, asserts no engine tool name resolves, no engine-named
environment is present, no provider account is in `/etc/passwd`/`/etc/group`,
and no relay source is readable — while asserting `python3`/`node`/`bash`/
`perl`/`git` still run. This is the lesson from the brief: `zcp`/`zsc` were not
the whole list, and the engine's *own* runner (`nsjail`) and helpers were the
first leak.

**Honest limit.** The curated trees still contain third-party distro and
operator tooling that is not this provider's (`code-server`, `agent-browser`,
the Go-less toolchain): those are generic, not platform-identifying, and are
kept because a workspace legitimately uses them. It hides *engine and provider
names*, not every byte of a large distro. It does not touch the egress address
(ADR 76).

### ADR 75 — The `dlopen`-scoped `dlsym` bypass is closed, not inherent

**Problem.** ADR 72's original text classified the `syscall(2)` family
(including `dlsym`-after-`dlopen`) as one blob of inherent holes. Measurement
showed at least one member was a **shim gap**: `ctypes.CDLL("libc.so.6")` then
`.uname(buf)` returned the real host release with **no raw syscall**.

**Decision.** Interpose `dlsym` in the shim. A handle-scoped lookup of a masked
symbol is answered with the shim's own function for any handle; all other lookups
pass through. Measured closed: `CDLL("libc.so.6")`, `CDLL(abs path)`,
`libc.dlopen`+`dlsym`, `dlmopen(LM_ID_NEWLM)`, `dlsym(RTLD_NEXT,"uname")`,
`dlsym(RTLD_DEFAULT,"uname")`. **Not** interposed: `dlvsym` — on this glibc,
defining both makes the loader bootstrap recurse (a versioned reference binds to
the unversioned definition), measured. The residual `dlvsym(handle, …)` route is
documented and asserted as the known remaining gap (assertion 8). The full
classification is in the corrected ADR 72.

**Consequence for the raw-syscall path.** `syscall(63)`/`syscall(99)` remain
inherent. They are now asserted as an **expected, documented** result
(`identity-check.sh` assertion 9), with the one-line reality written down in ADR
72: a tenant learns the real kernel string, that the host is large/shared, and
the process count — an oracle for fingerprinting and attack-sizing, not a route
to another workspace or the host.

### ADR 76 — The egress address names the provider; an egress proxy is opt-in

**Problem.** A workspace with `allow_net` reaches the internet through the
platform's shared NAT. That address names the provider in several independent
ways: `ipinfo.io` reports `org: "AS43541 VSHosting s.r.o."` (Prague, CZ); RDAP
at RIPE reports the block's registered name `ZEROPS-IO`; `ipwho.is` reports
Czechia/Prague; the Cloudflare trace says `colo=PRG loc=CZ`; and the reverse DNS
of the neighbouring address `93.185.106.1` is `gw.vshosting.cz`. The address is
`93.185.106.128`, which is **also the host's own egress**.

**Measured this session (the NEEDS-CONFIRMATION item).** No egress address
outside the vendor's registered ranges is obtainable on this platform: the host
has exactly one public path (the `10.9.112.0/22` private interface NATed to
`93.185.106.128`; the `/32` addresses on `lo` are private), there is **no** proxy
configured or available (`env` carries no `*_proxy`), and `93.185.106.0/24` is
registered to `ZEROPS-IO` (`MNT-VSHOSTING`, `VS2641-RIPE`). Changing the source
address is therefore **structurally out of reach** from inside this platform.

**Decision.** Record the fact honestly and add **one opt-in knob**, default
**off**: `network.egress_proxy` (an optional outbound HTTP/SOCKS proxy a
workspace's egress is routed through). It defaults off because, when on, it
**changes the outbound address and therefore the operator's abuse/reputation
profile** — that is the operator's call, not a default the engine applies. With
it off (the default) the workspace uses the platform NAT and the provider is
named, exactly as measured; the operator is told this plainly. The knob exists so
an operator who *does* run a proxy can point at it; the engine never invents one.
A rented/proxied egress is the only path to a differently-registered address on
this platform, and it is the operator's to choose.

**What a tenant can infer, and what the operator can change.** A tenant can
learn the provider (AS/org/registry name/country) and the shared-NAT address
from a "what is my IP" call; they cannot reach the host LAN or another workspace
through it (the relay blackholes every private range). The operator can change
nothing about the address on this platform except by introducing an external
proxy — which is why the knob is off by default and documented as a reputation
decision.

## v0.0.17 — zero signal: remove every engine/platform artifact a tenant can read

### ADR 77 — The zero-artifact goal: what Stage 1 removed, and why the shim is not (yet) gone

**The bar.** The operator's bar is **zero signal**, not "hard to find" and not
"cannot be replaced": a file that exists to hide the host must not be visible to
the tenant at all. A tenant is *obviously* in a sandbox (they upload a project
and run it) — that is not the concern. The concern is any artifact that names
the engine, the platform, or the masking mechanism.

**Stage 1 — every name removed (implemented, asserted).** Measured before this
iteration, a plain `ls` inside a real workspace showed the engine by name. All
of the following were removed and are asserted by `scripts/identity-check.sh`:

| Artifact (measured present) | Fix |
|---|---|
| `/usr/bin/nsjail`, `/usr/bin/slirp4netns`, `runcon`, `setpriv`, `unshare`, `nsenter`, `chroot` | curated `/usr/bin` + `/usr/sbin` drop them by name (ADR 74) |
| `/sandboxd/` at the filesystem root holding `hostmask.so` (0644, `strings` → `hostmask.c`, `0.0.0-sandbox`, five `SANDBOXD_MASK_*`, GCC line) | curated trees + shim stripped, renamed, moved to `/run/.rt.so` (ADR 72) |
| `/etc/ld.so.preload` naming `/sandboxd/hostmask.so` | preload now names `/run/.rt.so`; the string carries no engine name |
| `SANDBOXD_KIND`, `SANDBOXD_WORKSPACE`, four `SANDBOXD_MASK_*` in the environment | renamed to `PROCESS_KIND`/`WORKSPACE_ID`/`PROCESS_SOURCE` and `LSM_SYS_{CPUS,MEM,REL,VER}` |
| `/run/sandboxd/pty_relay.py` (10316 bytes, the engine's source, readable) | the relay is passed inline (`python3 -c "<source>"`); no file exists |
| provider account `zerops` in `/etc/passwd`, `/etc/group` (and group member lists) | `/etc/passwd`+`/etc/group` filtered (ADR 74) |
| provider-named systemd units and host tooling under `/usr/lib` | `/usr/lib` curated (also mounted at `/lib`) (ADR 74) |

The measured result: inside a 1-CPU/256-MB workspace, `ls /` shows no
`sandboxd`; `/etc/ld.so.preload` is `/run/.rt.so`; `strings` over every
reachable file contains **no** whole-word `sandboxd`, `nsjail`, `slirp4netns`,
`zerops`, `vshosting` or `zabbly`; no path is named `hostmask`; no engine tool
name resolves; `env` carries **no** engine-named variable; `getent passwd`/`id`
name no provider; `python3`/`bash`/`perl`/`git` still run. The one allowance is
that the **generic networking word** `hostmask` survives inside Python's
`ipaddress` module and node's `netmask` package — it is the inverse of a
netmask, not this engine's name, and dropping it would break those standard
libraries (measured: `ipaddress.ip_network("10.0.0.0/8").hostmask` works).

**Stage 2 — honest verdict (superseded in part by ADR 79).** This ADR
originally concluded that the shim "still exists as a *readable* file
referenced by `/etc/ld.so.preload`", and treated that visibility as unavoidable.
**ADR 79 disproves the "unavoidable" part**: the shim's filesystem interposers
now make `/etc/ld.so.preload` and the shim itself invisible to the tenant while
the loader still reads the preload list. What remains here is the narrower
zero-artifact question — can the shim file stop existing *at all*? Two mechanisms
could achieve that, and **both were measured; neither holds on this host within
bounded effort**:

- **(A) Replace the workspace's libc view.** If it worked, the masking *is* the
  library the tenant already has, with no extra file and no preload entry, and
  `ctypes.CDLL("libc.so.6").uname()` would be masked by construction. Prototyped
  and measured: a `libc.so.6` that merely interposes `uname`/`sysconf`/`sysinfo`
  and forwards via `dlopen`+`dlsym` **aborts every dynamically-linked binary** —
  the loader reports `no version information available` and
  `undefined symbol: __libc_start_main, version GLIBC_2.34`. A correct
  replacement must export the **entire** glibc symbol set *with version
  information* (a `.map` version script per glibc build) and forward each
  versioned symbol, or it breaks the runtime. That is a per-glibc-version
  project whose failure mode is a broken sandbox; shipping an untested prototype
  would trade a small leak for a large outage, which the brief forbids.
- **(B) Move interception to seccomp `user_notif`.** The kernel advertises
  `user_notif` in `/proc/sys/kernel/seccomp/actions_avail` (measured). **The
  nsjail build the service ships (3.4, the Dockerfile pin) has no
  `--seccomp_unotify` flag** (`unrecognized option`, measured) and **`secdump`
  is not installed**. A newer nsjail **does** carry the feature — **ADR 80**
  settles this: building nsjail `master` from source yields a binary that
  supports `--seccomp_unotify` and starts an unotify loop on this kernel — but
  `master` is an unreleased branch, and user_notif is a supervisor *tracing*
  feature (it logs filesystem/network operations to a report), not a drop-in
  interception hook that returns alternate values: a full replacement of the
  masking shim still needs a supervisor owning the listener fd and answering
  each notification. The flag is therefore recorded as *reachable-but-not-yet-a-
  replacement* rather than adopted, and 3.4 remains the shipped pin. The
  original measurement that this build's kafel binding maps `USER_NOTIF` to
  `ENOSYS` rather than `SECCOMP_RET_USER_NOTIF` stands for 3.4.

**The result.** The **visibility** gap this ADR first recorded is now **closed**
(ADR 79): the shim's filesystem interposers make `/etc/ld.so.preload` and the
shim itself invisible to every tool a tenant would use, while the loader still
reads the preload list and `env -i` remains masked. What remains open is the
narrower **zero-artifact** goal (no shim file at all); the reasons are recorded
with the exact measurement, and the harness reports the two candidate mechanisms
as `skip` (third state), never as a pass. What *is* asserted and unchanged: the
shim cannot be tampered with (read-only mounts; tampering attempts all fail with
`Read-only file system`), it survives `env -i` (compiled-in generic fallbacks),
and a raw `syscall(2)` from a static binary remains the documented permanent
limit (asserted as the real value, not pretended closed).

### ADR 78 — The PTY relay is passed inline, never materialized

**Problem.** The terminal-session relay was written to the session's `/tmp` as
`pty_relay.py` and bind-mounted read-only at `/run/sandboxd`, so it existed as
the engine's own source code (10316 bytes, readable) at a path that named the
engine, inside every session.

**Decision.** The relay source is passed to the interpreter inline
(`python3 -c "<source>" <config-json>`) as the session's entry command. There is
no helper file, no fixture directory, and no engine-named path in the sandbox.
The relay still runs with the session's own uid, mounts and seccomp policy — the
sandbox is **not** weakened for sessions — and the JSON configuration still
travels as an argument (not an environment variable), so no engine-named
variable is introduced. `internal/executor/live.go` builds the argv; the
`HelperDir` template plumbing and the `/run/sandboxd` mount are gone.

**Asserted.** `TestRenderSessionConfigHelperNoFile` proves the rendered session
config contains `-c`, no `/run/sandboxd`, and no `pty_relay` name;
`identity-check.sh` assertion 10 asserts no relay file is readable inside a
session-capable workspace.

## The masking mechanism is invisible inside a workspace (v0.0.17 line)

### ADR 79 — The masking shim hides itself and its preload list from the tenant

**Problem.** The operator found the masking mechanism by browsing inside his own
workspace: `ls /` → `/sandboxd` → `hostmask.so`, and `cat /etc/ld.so.preload`
named the shim's path. Two real gaps remained after the `v0.0.17` cleanup:

1. `/etc/ld.so.preload` was a **readable file naming the shim's path**, and the
   shim at that path was **readable** (`cat`/`strings` worked). Anyone who
   thought to look learned both that a shim existed and where it lived.
2. The shim's own strings still contained the masked kernel values
   (`0.0.0-sandbox`, `#1 SMP`) and the configuration variable names
   (`LSM_SYS_*`), so `strings` over it was a roadmap.

The operator's bar is **silence**: nothing named, nothing browsable, nothing
readable — not "protected, therefore fine".

**Decision — the shim hides the filesystem paths that reveal it.** The shim is
already `LD_PRELOAD`ed into every process the tenant runs, so it is in a position
to answer the filesystem questions the tenant would use to discover it. It now
interposes `open`/`open64`/`openat`/`openat64`, `stat`/`lstat`/`fstatat`/`statx`
and the `*64`/`__x*` aliases, `access`/`faccessat`/`eaccess`/`euidaccess`,
`readlink`, `readdir`/`readdir64`/`readdir_r` and `realpath`/
`canonicalize_file_name`, and answers **`ENOENT`** (and drops the directory
entry) for a configured list of paths:

| Hidden path | Why |
|---|---|
| `/etc/ld.so.preload` | the list that NAMES the shim; `cat` would reveal the mechanism |
| `/run/.rt.so` | the shim's own in-jail path |
| `/sandboxd` | the legacy engine directory at the filesystem root |
| `/run/sandboxd` | the legacy engine directory under `/run` |

The list is a **single explicit table** in `hostmask.c` (auditable), plus up to
four optional operator paths via the generic `HIDE_PATH`/`HIDE_PATH2..4`
variables.

**The two non-obvious things that made it work (both measured).**

1. **glibc VERSIONS its symbols, and an unversioned preload definition does not
   satisfy a versioned reference.** Merely interposing `stat`/`fstatat` hid
   `ls`, `cat` and `find` immediately, while `python`'s `os.stat()` and `bash`'s
   `test -f` sailed through — because they import `fstatat64@GLIBC_2.33` and
   `stat64@GLIBC_2.33`, not the plain names. The fix is `.symver` aliases plus a
   linker version script (`internal/executor/hostmask/hostmask.map`), emitting
   each interposer under the version its callers reference:
   `GLIBC_2.2.5` for `open/open64/access/euidaccess/readlink/readdir/readdir64/
   readdir_r/__xstat/__lxstat/canonicalize_file_name`; `GLIBC_2.3` `realpath`;
   `GLIBC_2.4` `openat/openat64/faccessat/eaccess/__fxstatat/readlinkat`;
   `GLIBC_2.28` `statx`; `GLIBC_2.33` `stat/stat64/lstat/lstat64/fstat/fstat64/
   fstatat/fstatat64`. The syscall-mask exports (`uname`/`sysconf`/`sysinfo`/…)
   reference
   the ancient `GLIBC_2.2.5` versions and are deliberately left unversioned.
   A measured surprise inside this work: coreutils `test -r` references
   **`euidaccess@GLIBC_2.2.5`** (not `eaccess@GLIBC_2.4`), so `test -e`/`-f`
   were hidden while `test -r` was not until `euidaccess` was bound too.
   The version script also makes the linker emit an auto `SONAME` equal to the
   output file name; `-Wl,-soname,rt.so` overrides it so the engine name does not
   leak into the dynamic string table (measured: without it, `strings` showed
   `hostmask.so`).

2. **`readlink(1) -f` and `realpath(1)` canonicalize component by component and
   pass only the LEAF**, so matching the full path alone misses them. The matcher
   therefore matches the path **and** the final component. A first pass concluded
   from this that they "never stat the leaf" and could only echo their argument,
   and left them alone — measured **wrong** in the follow-up: the coreutils
   canonicalizer *does* call `readlink(2)` on the leaf, and its behaviour depends
   on the errno. See **ADR 81**: the canonicalization family now answers `EACCES`,
   not `ENOENT`, which makes it abort instead of echoing.

**The loader-scope trap — solved, and the reason it works at all.** The dynamic
loader must read `/etc/ld.so.preload` for the mask to work. The hiding is scoped
to processes that **already have the shim mapped**: the loader reads the preload
list *before* any preload object is loaded, so it is never intersected by the
interposer. Only a process that already runs this shim answers `ENOENT` for the
file. Measured on this host: inside a workspace, `env -i uname -r`,
`env -u LSM_SYS_REL -u LSM_SYS_VER uname -r`, and Python's `platform.uname()`
all still report the masked `0.0.0-sandbox`, while `cat /etc/ld.so.preload`,
`ls -la /etc/ld.so.preload`, `test -e /etc/ld.so.preload`, Python's
`os.path.exists`/`os.listdir`, and `readdir` over `/etc` all report it missing.
There is therefore **no** trade-off: the tenant cannot see the mechanism and the
loader still reads it.

**Strings are no longer self-announcing.** The masked kernel values
(`0.0.0-sandbox`, `#1 SMP`), the variable names (`LSM_SYS_CPUS/MEM/REL/VER/NODE`)
and the hidden paths are stored **XORed** and decoded at runtime, so `strings`
over the shim yields none of them. An opaque random marker (`9f3a71c2`) is kept as
a live literal as the **positive control** for "verify the binary you are
testing": a stale `.so` cannot masquerade as the tested one. Measured: `strings`
over the shipped `.so` matches none of `sandboxd|hostmask|ld.so.preload|LSM_SYS|
SANDBOXD|0.0.0-sandbox|#1 SMP` and contains the marker.

**The documented limits are unchanged and still asserted.** A statically-linked
binary, a musl runtime, or a raw `syscall(2)` bypasses the shim; a raw
`newfstatat(262)`/`stat(4)` still reaches the real `/etc/ld.so.preload` (measured,
and asserted in `identity-check.sh` group 10c as the **known** result, never as a
pass or a failure). `readlink -f`/`realpath` echo the literal input, asserted as
a recorded non-leaking limit.

**Where this leaves ADR 72.** ADR 72's class C ("not yet closed — the visible
shim") is **corrected**: the visibility is closed by this ADR (the prototype
disproves the "unavoidable" classification, and a wrong "inherent limit" is worse
than an open gap). The narrower zero-artifact goal (no shim file at all) stays
open and is de-risked.

### ADR 80 — nsjail: 3.4 vs the unreleased `master` with `--seccomp_unotify`

**Problem.** A measured discrepancy: this host's *service-configured*
`/usr/local/bin/nsjail` is **902 672 bytes** and does **not** support
`--seccomp_unotify` (`unrecognized option`), while a **1 469 432-byte** nsjail
supports it. Which should the deployment ship?

**Measured this session, with evidence.**

- `/usr/local/bin/nsjail` (902 672 bytes) is **byte-identical**
  (`sha256 9b9fb1df…`) to a from-source build of the pinned release tag **3.4**,
  the Dockerfile's `NSJAIL_VERSION`. `--seccomp_unotify` is absent from 3.4.
- A from-source build of nsjail **`master`** (HEAD `d8415bc`, `3.6-122-gd8415bc`,
  2026-10-28) is **1 469 432 bytes** and supports `--seccomp_unotify`. The
  feature landed *after* the last tag (3.6, 2026-03-18); it is **not in any
  release**.
- On this kernel (`/proc/sys/kernel/seccomp/actions_avail` advertises
  `user_notif`), the `master` build **actually runs** the feature: a jailed
  process with `seccomp_unotify: true` logged `Started unotify loop` and wrote a
  report entry. So the flag is genuinely reachable here.

**Decision: keep 3.4 as the shipped pin; do not switch the deployment to
`master`.** Rationale, in order of weight:

1. `--seccomp_unotify` in nsjail is a **tracing** feature (a supervisor logs the
   filesystem/network operations the sandbox performs to a report file), **not a
   value-interception hook**. It cannot, by itself, return alternate values for
   `uname`/`sysconf`/`stat`; retiring the shim would still require a supervisor
   that owns the listener fd and answers each notification — a new component, not
   a config flag.
2. Pulling an **unreleased `master`** into a security-critical sandbox runner
   trades a stable pin for unreviewed future commits, which is the wrong default
   for the isolation boundary. 3.4 is a tagged release.
3. Switching does not remove the mask or the shim, so it buys nothing today.

**Wiring.** No config change: `sandbox.nsjail_path` already points at the
binary, and the shipped default remains `nsjail` (PATH) which resolves to
`/usr/local/bin/nsjail` (3.4). The finding is recorded so a future iteration
that builds a user_notif supervisor can bump `NSJAIL_VERSION` to a release tag
that carries the feature. The Dockerfile pin and this decision stay in sync;
`identity-check.sh` group 10b records the measurement as a `skip` with the
correct reason (3.4 lacks the flag; master has it but is unreleased and is a
tracer, not a hook).

### ADR 81 — the canonicalization family answers EACCES, not ENOENT

**Problem.** After ADR 79 the hiding worked for every *existence* probe (`ls`,
`cat`, `stat`, `test -f/-r/-e`, `find`, `os.stat`, `os.path.exists`, `open`,
`os.listdir`, `glob`), but `readlink -f /etc/ld.so.preload` and
`realpath /etc/ld.so.preload` still printed the hidden path. The follow-up
measurement also showed the interposers added in the hiding pass were **not**
what broke `node` (see ADR 82), so the two findings are independent.

**Measured (strace, coreutils 9.4, inside a real workspace).** The canonicalizer
never `stat`s the leaf; it calls `readlink(2)` on each component:

```
readlink("/etc", ...)                = -1 EINVAL   (not a symlink)
readlink("/etc/ld.so.preload", ...)  = -1 ENOENT
  -> stdout "/etc/ld.so.preload"        exit 0    (the literal ARGUMENT, echoed)
readlink("/etc/ld.so.preload", ...)  = -1 EACCES
  -> stderr "realpath: /etc/ld.so.preload: Permission denied"  exit 1, no stdout
```

An **`ENOENT` from the canonicalizer means "this component does not exist yet"**,
so coreutils keeps the literal it was given and prints it — an `ENOENT` *cannot*
hide the path from `readlink -f`/`realpath`. `EACCES` is the only errno that
makes them abort, and it is the natural answer for a path whose components exist
but whose leaf is withheld.

**Decision.** Split the filesystem interposers into two error modes:

- **Existence family** (`open`/`openat`/`stat`/`lstat`/`fstatat`/`statx`/`access`/
  `faccessat`/`eaccess`/`euidaccess`/`readdir`/`readdir64`/`readdir_r`) keeps
  **`ENOENT`**: human tools and Python report the path as simply absent.
- **Canonicalization family** (`readlink`/`readlinkat`/`realpath`/
  `canonicalize_file_name`) answers **`EACCES`**: coreutils aborts instead of
  echoing. `readlinkat@GLIBC_2.4` is now interposed too (it was a gap: the libc
  `readlinkat` returned the real target).

**Measured after the change** (inside a real workspace, all four hidden paths):
`readlink -f`/`readlink -e`/`realpath`/`realpath -e` return rc=1 with **no
stdout**; `ls`/`cat`/`stat`/`test -f` still report the path as absent; a
non-hidden path and a real symlink resolve normally.

**Documented residual (not closed).** Python's `os.path.realpath()` swallows the
`OSError` and returns its own argument, so it still echoes the tenant's literal
input. That reveals nothing the tenant did not type, and `os.path.exists()`
correctly reports `False`; it is asserted as the known outcome (a *literal* echo
count), never as a pass or a failure. A raw `syscall(2)` bypasses the interposer
like every other one. `identity-check.sh` group 10c guards both: the
canonicalization family must **never** echo, and the `os.path.realpath` echo is
counted.

### ADR 82 — `rlimit_as` is not `memory_mb`: a managed runtime reserves virtual space

**Problem (a regression report).** After ADR 79, `node -e 'console.log(42)'`
inside a workspace died with `Fatal process out of memory:
SegmentedTable::InitializeTable (subspace allocation)`, exit 133, while
`node -v` printed `v24.22.2`. The report named the newly interposed filesystem
symbols as the prime suspect.

**Measured — the interposers are not the cause.** The crash reproduces with the
shim **not loaded at all**:

```
$ (ulimit -v 262144; node -e 'console.log(42)')
# Fatal process out of memory: SegmentedTable::InitializeTable ...
```

and identically with the **previous, syscall-only** shim, and inside a real
sandbox with host-identity masking **off** (`identity.enabled: false`). Node's
virtual-address threshold is the *same* (1121 MB) with and without the shim.
The cause is `rlimit_as`: nsjail renders `rlimit_as = memory_mb`, and the pinned
node/V8 build reserves ~1.1 GiB of **virtual** address space before `main` — a
512 MiB `PROT_NONE` `CodeRange` `mmap` plus heap reservations:

```
$ (ulimit -v 1048576; strace -e mmap node -e '1')
mmap(0xea5000, 536870912, PROT_NONE, MAP_PRIVATE|MAP_ANONYMOUS|MAP_NORESERVE, -1, 0)
  = -1 ENOMEM
# Fatal process out of memory: Failed to reserve virtual memory for CodeRange
```

`RLIMIT_AS` counts every reservation, including `PROT_NONE` ones the process
never commits, so `rlimit_as = memory_mb` aborts a managed runtime on startup.
Node's own `--jitless` lowers the reservation (~768 MB) but is a behavioural
change with a large throughput cost, not an acceptable default.

**Decision — separate the physical ceiling from the virtual backstop.**

- The **physical** memory ceiling stays `cgroup_mem_max = memory_mb` bytes; a
  process that commits past it is OOM-killed there.
- `rlimit_as` becomes a **generous virtual backstop**:
  `rlimit_as = memory_mb + 1024`, floored at `2048` MB
  (`addressSpaceMB()` in `internal/executor/nsjail.go`). The workspace's own
  `memory_mb` is still the physical limit a tenant is charged against; the
  headroom only covers the address space a runtime reserves but does not commit.

**Measured after the change.** `node -e 'console.log(42)'` → `42` at the default
512 MB and at 256 MB, mask on and off; `python3`/`bash`/`perl`/`git` unaffected;
the mask still reports `0.0.0-sandbox`, `1` cpu. `python3` touching 4 GiB is
killed by the cgroup (exit 137) rather than raising `MemoryError` — the physical
ceiling is now the cgroup, and `acceptance.sh` #19 is updated to assert that
(exactly one of `exit 137` or `MEMORY_DENIED`), which still catches the unit bug
(a 512 TiB `rlimit_as` would let 4 GiB succeed). `identity-check.sh` gains a
**runtime group** that asserts `node`/`python3`/`bash`/`perl`/`git` succeed
under the mask, so a runtime regression can never pass silently again.

**Cost.** `rlimit_as` no longer bounds a single process's virtual address space
to `memory_mb`; the cgroup does. This is the correct model: `memory_mb` is a
committed-memory budget, and a virtual reservation is not committed memory.

## v0.0.18 — workspace profiles: an explicit allowlist replaces the denylist

### ADR 83 — named profiles with an explicit allowlist; entry sets; per-profile materialization

**Problem.** Every workspace got a **denylist** over the host's whole tool
trees: the engine copied almost all of `/usr/bin` (measured 982 entries at the
time; 973 on this host) and `/usr/sbin` (194; 193 here) and then removed only the
entries whose name or content named the engine/platform. A tenant therefore got
`apt`, `add-apt-repository`, the full `gcc`/`binutils` compiler chain, and the
operator's own tooling whether they wanted them or not. There was no way to give
a workspace a *smaller*, reviewable set of tools, and no way to add a language
without editing the engine's filter.

**Decision — an explicit, data-driven allowlist.** A workspace's toolchain is a
**named profile**: a YAML file (`configs/profiles/*.yaml`, embedded in the
binary, overridable via `profiles.dir`) that lists the tools the workspace gets.
Creating a workspace names the profile it wants (`POST /v1/workspaces
{"name":"x","profile":"python"}`), the record echoes the resolved profile
(including the server default when none is named), and `GET /v1/meta` lists the
offered profiles. An unknown name is `422 unknown_profile`, never a silent
fallback. The profile is **immutable** — it decides which tools exist, so
changing it would invalidate anything already installed or running.

**Entry sets, not just command names.** A tool is not just its binary. The
schema has three entry sets:

- `commands` — bare names resolved on the host PATH and placed in the
  workspace's `/usr/bin`. A same-directory symlink target (`python3` →
  `python3.12`) is materialized as its real bytes so the name works standalone.
- `aliases` — extra names created as symlinks to a *listed* command (never a new
  host binary). This is how the operator's `python` is provided, since the host
  has only `python3`.
- `paths` — absolute files, directories or whole trees reproduced at the same
  path, because a tool needs its data/library trees: `/usr/lib/python3.12` (the
  whole stdlib — without it `import json` fails), `/usr/lib/node_modules/npm`,
  `/usr/lib/git-core`, `/usr/lib/x86_64-linux-gnu/perl-base`, `/usr/share/perl`,
  `/etc/ssh/ssh_config`. The engine additionally always provides the shared
  libraries, the ELF loader and `/usr/lib/ssl` (the OpenSSL trust paths: without
  it a TLS client cannot verify a certificate even with `/etc/ssl/certs`
  mounted — measured, `urlopen("https://pypi.org/…")` failed with "unable to get
  local issuer certificate").

Every tool is verified by **executing it inside a real workspace**
(`scripts/identity-check.sh` §11), never by checking that a file exists.

**Denylist filter kept on top; fail loud.** The engine's existing engine/platform
name+content filter still applies to each profile entry. A profile entry whose
name or content names the engine or platform is **refused** (the profile fails
to load / the materialization returns an error), rather than silently shipped.

**Per-profile, not per-workspace, materialization.** A profile's tree does not
depend on the workspace, so it is materialized **once per service per profile**
and shared read-only by every workspace that names it; two `python` workspaces do
not duplicate the whole stdlib. This follows the existing pattern (the curated
overlays were already materialized once per service start, and the two package
tiers are shared/private). The trade-off: a profile's tree is fixed for the
service's lifetime, so changing a profile file requires a restart (acceptable:
profiles ship in the image, and a change is an operator action).

**What the profile can and cannot promise.**

- It **can** guarantee that a tool it does not name is *absent*: the whole
  `/usr` is the profile tree, so un-allowlisted material (`apt`, the compiler
  chain, `/usr/include`, `/usr/libexec`, the platform's `/usr/local`) is not
  mounted at all, not merely filtered. `runcon`/`setpriv`/`unshare`/`nsenter`/
  `chroot` are absent unless a profile names them (none do).
- It **cannot** promise that a named tool is safe, nor hide the egress provider:
  a named tool runs with the sandbox's identity and seccomp policy, and an
  `allow_net` workspace still presents the platform's shared NAT address (ADR
  76). A profile is an allowlist of *tools*, not a new security boundary.
- A **session** needs a profile that provides `python3` (the PTY relay is a
  Python program): a session on `minimal` is refused `422
  session_needs_interpreter`, an honest, actionable limitation rather than a
  mysterious nsjail error.
- `screen`/`tmux` work **only in a session** (which binds `/dev/pts` and
  `/dev/ptmx`), never in one-shot `exec`/`run` (which get only
  `null`/`urandom`/`zero`, so `openpty()` fails with `ENOENT`) — documented in
  the `python` profile rather than shipping a tool that mysteriously fails.

**Shipped profiles.** `python` (the operator's: Python 3.12, pip/venv, Node.js,
Perl, git and common shell tools, plus the `python` alias) and `minimal` (POSIX
shell + coreutils only, no language runtime). The `minimal` profile exists so
`scripts/identity-check.sh` can prove the allowlist really excludes things:
under it, `python3`/`python`/`node`/`git`/`gcc`/`curl` are absent by execution.

**Measured (this session, real nsjail).** `python` workspace: `python -c` →
`py-ok`, `python3 -c 'import json,csv,sqlite3,ssl,ctypes,urllib,…'` → `stdlib-ok`,
`pip --version` → `pip 24.0`, `node -e 'console.log(6*7)'` → `42`, `git --version`
→ `git version 2.43.0`, `perl -e` → `perl-ok`, `awk` → `awk-ok`; `gcc`/`apt` →
`422` command-not-found. `minimal` workspace: `sh`/`bash`/coreutils run;
`python3`/`node`/`git`/`gcc`/`curl` all fail command-not-found. A session works on
`python` (`IS_TTY`, `/dev/pts/N`, `stty size` → `24 80`). See
[VERIFICATION.md](VERIFICATION.md).

### ADR 84 — the egress decoy: a plausible invented public IP, not a security boundary

**Problem.** The workspace's public IP (`93.185.106.128`, a platform-shared NAT)
and its registration name the hosting platform: `curl https://ipinfo.io/org` →
`AS43541 VSHosting s.r.o.`, the RDAP record for the `/24` reads "ZEROPS-IO", and
a neighbour's rDNS is `gateway2.zerops.io`. A tenant who is "playing around"
learns which platform they are on in one command.

**This is the FIRST and MOST IMPORTANT sentence: the egress decoy is a decoy
layer, not a security boundary.** It removes the problem for casual discovery.
It is defeated by a tenant who controls their own endpoint, because the address
is decided by whoever exits to the internet, and no host-side interception can
change that. It is stated here first so no later sentence implies a stronger
guarantee. Stated plainly and without line breaks: this is a decoy layer, not a security boundary.

The specific limits, in order of importance:

1. **A tenant's own endpoint defeats it entirely.** `ssh user@their-server`,
   `curl https://their-server/`, `nc their-server 9999`, `ping their-server`,
   SMTP/NTP/IRC/a game server — any protocol that reaches a party the tenant
   controls reveals the peer address **to them**. Unfixable. Declared, not
   half-fixed.
2. **Path inspection (`traceroute`/`mtr`) exposes intermediate hops** and the
   platform's own router names; hiding them would break normal routing, so it is
   declared out of scope.
3. A **hardcoded-IP endpoint** not in the covered set, and **novel services**
   whose hostname matches no shape pattern, are not intercepted.
4. **MTU/ICMP/timing fingerprinting** is out of scope.
5. A knowledgeable tenant can **recognise a documentation-range address**.

**Decision — intercept at the network layer inside the workspace netns.** A
command-name filter covers only the clients someone thought of, breaks on a
statically linked binary, and is trivially bypassed by
`python -c "import urllib.request; …"`. Four cooperating pieces, all inside the
workspace's own network namespace, none on the host:

- **A stub resolver** at `127.0.0.1` (the workspace's `/etc/resolv.conf` names
  it). It answers the DNS echo names (A and TXT) and covered hostnames with the
  decoy and forwards everything else to the configured public resolvers, so
  `pip`/`npm`/`github` resolution is untouched. Its upstream queries carry
  `SO_MARK = 0x24` so the port-53 redirect passes them through.
- **A stub HTTP/TLS responder** and a **generated CA trusted inside the
  workspace** (appended to the curated `/etc/ssl/certs` bundle and to its
  OpenSSL-hashed name). Per-hostname certificates are minted on demand so TLS
  interception is seamless; a no-SNI client (a bare resolver IP) gets a
  certificate carrying the covered resolver IPs as SANs. The same port answers
  DNS-over-TLS, so `dig +tls @1.1.1.1` is covered. **A certificate warning is
  worse than nothing** (it tells the tenant the environment is tampered with),
  so the acceptance suite asserts its absence for curl, wget and python.
- **In-namespace nft rules** forcing port 53 to the stub and covering the
  well-known DoH/DoT resolver addresses, in an engine-owned table that is
  deleted on teardown. The rules never touch the host.
- **Consistency is the whole illusion.** Every channel serves ONE profile: same
  address, org, ASN, city, country, rDNS. A mismatch teaches the tenant more
  than the truth would. The profile is derived per workspace from the workspace
  id, so two tenants comparing notes see different addresses.

**Configuration, default OFF.** `network.decoy.enabled: false` is the default, so
no existing gate regresses. The whole profile is configurable (`address_range`,
`org`, `asn`, `city`, `country`, `region`, `rdns`, ports, extra hosts/patterns).
The safe default address range is a **documentation range** (`198.51.100.0/24`):
a real dotted quad that resolves to nothing, with a private-ish ASN text
(`AS64500`). The trade-off is recorded above (limit 5): a knowledgeable tenant
can recognise it, so an operator may prefer a range he owns — a config change,
not a code change.

**Proof.** `scripts/egress-decoy-check.sh` runs the real service against real
nsjail with the decoy ON and asserts every covered channel (HTTP services, an
invented hostname, all four DNS methods, DoH/DoT/direct resolver, all client
runtimes, and cross-channel consistency) answers the decoy and not the truth,
and that `pip download`/github/npm/pypi still succeed. Measured this session:
`57 passed / 0 failed / 0 skipped`. See [EGRESS-DECOY.md](EGRESS-DECOY.md) for
the full Part 1 enumeration and Part 2 design.

## Per-workspace limits: what is reported equals what is enforced

The operator's product requirement: a tenant runs `df -h` and sees **their own**
disk allowance, and `free -h` and sees **their own** RAM, and cannot exceed
either. The bar is not "print a plausible number": a number that is reported but
not enforced is a lie. The previous behaviour — a tenant seeing the platform's
100G ZFS volume and its `zfs` type — was itself a leak of the host filesystem
identity. Both switches ship **ON**.

### ADR 85 — the disk allowance is a real per-workspace filesystem (kernel boundary), with a sampled counter as the fallback

**The two mechanisms, named plainly.** There are exactly two enforcement
mechanisms for `disk_mb`, and they are not equivalent:

- **KERNEL-ENFORCED — the filesystem boundary (default; `disk.backend: auto` →
  `fuse`).** Each workspace's `/workspace` IS a real per-workspace ext4
  filesystem sized to its allowance, mounted host-side with `fuse2fs`. A write
  past the allowance is refused by the kernel at the crossing write with
  `ENOSPC`; there is **no overshoot** — the reported total is the enforced total,
  because they are the same filesystem.
- **SAMPLED — the used-bytes counter watchdog (fallback; `disk.backend:
  counter`, or when FUSE is unavailable).** A watchdog polls every
  `disk.counter_poll_ms` and cancels a run once the directory exceeds the
  allowance. It does NOT refuse the crossing write; it notices afterwards and
  kills the offender. The overshoot is real and **measured on this host at
  48 MiB – 1.2 GiB on a 64 MiB allowance** (six runs; see below). It is never
  presented as equivalent to the kernel boundary.

Which mechanism is live is published truthfully in
`/readyz.workspace_limits.mechanism` and `/v1/meta.workspace_limits.mechanism`
(`fuse`, `counter`, or `none`).

**Problem.** `disk_mb` was enforced only as `rlimit_fsize` (which caps **one
file**, not the workspace directory) plus a `du` recheck on the API file path.
Measured: N files of `disk_mb` each were allowed, `dd 2000MB` inside a workspace
wrote 1.0G then failed with "File size limit exceeded", and `df` reported the
platform's 100G volume throughout. Nothing observed the workspace directory as a
whole, and `df`/`stat -f`/`os.statvfs()` reported the host filesystem.

**Closed by measurement (do not re-litigate).** No loop devices (creating
`/dev/loopN` is `Operation not permitted`); no ZFS datasets (`zpool list` = no
pools; the container sees the mountpoint but cannot reach the pool); ext4
project quota is irrelevant because the filesystem is ZFS. The host DOES support
FUSE (`/dev/fuse` present, `fuse` in `/proc/filesystems`, `fusermount3`
installed), and a per-workspace ext4 image mounted with `fuse2fs` works.

**Decision — a real per-workspace filesystem (default).** Each workspace's
`files` directory is the mount point of an ext4 image file
(`{data}/workspaces/{id}/disk.img`) sized to its `disk_mb`, mounted **host-side
by the orchestrator** with `fuse2fs -o allow_other,fakeroot,uid=<sandbox>,gid=<sandbox>`.
The mount is host-side deliberately: the workspace only ever sees the mounted
result, so it needs neither `/dev/fuse` nor the fuse tools inside the jail, and
no device node is created (measured: creating one is `Operation not permitted`).
`/etc/fuse.conf` carries `user_allow_other` so `allow_other` is accepted.

**Why this is the strongest mechanism.** The mounted filesystem IS the
allowance, so:

- `df`, `stat -f` (including its `Type:` label), `os.statvfs()` and
  `os.statvfs().f_blocks*f_frsize` are all correct **for free** — no syscall
  interception, no shim. The `statvfs` family is never faked.
- Enforcement is a **kernel filesystem boundary at the boundary**: a write past
  the allowance fails with `ENOSPC` and **no overshoot**. Measured end-to-end:
  `dd 300MB` into a 256MB workspace copied 239075328 bytes and was refused the
  rest; `python f.write(300MiB)` raised `OSError: [Errno 28] No space left on
  device`; 10×5MiB files land only up to the TOTAL (which `rlimit_fsize` cannot
  do). The boundary refuses the write rather than sampling afterwards.

**Correction (this revision) — which `df` invocations are covered.** The claim
"`df` is correct for free" was too broad. Coreutils `df` builds its filesystem
table from **`/proc/self/mountinfo`**, and the curated `/proc` (ADR 86) initially
provided only `/proc/mounts`. Measured inside a real workspace before the fix:

- **`df --output=size <path>`, `df -B1 <path>`, `df --output=fstype <path>`,
  `stat -f`, `os.statvfs()`** — **worked**, because with an explicit path `df`
  `statvfs`es the path directly and does not need the mount table.
- **bare `df -h`, `df -h <path>`, bare `df`** — **BROKEN**: `df: cannot read table
  of mounted file systems: No such file or directory`, no row. These read
  `/proc/self/mountinfo`, which did not exist.

That split is exactly why the gate passed while the operator's literal command
(`df -h`) was broken: the gate tested only the path/`--output=` forms. Fixed by
generating `/proc/self/mountinfo` from the SAME mount list `/proc/mounts` is
built from (see ADR 86). After the fix, bare `df -h` prints the workspace row
with its own 256M, and `df -h /`/bare `df` exit 0. `df` needs **only mountinfo**;
it does not read `/etc/mtab` (measured: `strace` shows coreutils `df` opening
`/proc/self/mountinfo` alone), which is provided for conventional tools.

**Correction (this revision) — the provisioning flake, root cause and fix.** The
identity gate intermittently failed with grouped empty results, including disk
provisioning returning HTTP 503. Root cause: `fitImage` measured the ext4 total
with a transient **read-write** mount; `fuse2fs` commits the superblock
asynchronously, so unmount-then-`resize2fs` raced the final write and
`resize2fs` intermittently refused with `Please run 'e2fsck -f ... first`
(measured standalone: **48/60 with the rw probe, 0/60 with a read-only probe**).
A failed fit left the workspace permanently unprovisionable, so every exec
returned 503. Fixes: measure with a **read-only** mount (it only `statvfs`es, so
it never dirties the image), and self-heal a dirty image in `resize2fs` with
`e2fsck -p -f` + one retry (verified against a deliberately dirtied image). The
harness was also made honest: it surfaces an API error body instead of yielding
an empty `got=`, and polls the REAL materialization condition before a group
runs (bounded, no blind retry). See VERIFICATION.md §14c.2.


**Exact sizing.** ext4's own metadata means the usable `statvfs` total is
slightly below the image size, so `fitImage` sizes the filesystem so that the
mounted total equals `disk_mb` **exactly**: it resizes with `resize2fs` to a
target block count, measures the resulting `statvfs` via a transient mount, and
corrects (`target += desired − measured`). It converges in ≤4 iterations for
8 MiB…1 GiB (measured). So a 256MB workspace reports exactly 268435456 bytes on
every channel.

**The measured trade-off (stated, not hidden).** One image file and one
`fuse2fs` process per workspace (the helper daemonizes and outlives the
orchestrator, so it is reaped at startup and unmounted at shutdown). FUSE
throughput overhead, measured this session: write ~280–340 MB/s through FUSE vs
~1.3 GB/s native (≈0.25x), read ~1.1 GB/s through FUSE vs ~3.6 GB/s native
(≈0.3x). At the operator's expected workspace count this is acceptable; the
per-workspace image is sparse (a 1 GiB image is a few MiB on disk until
written). The image is created from any pre-existing workspace content (staged
aside on first mount and copied in), so an upgrade loses nothing.

**The fallback (real but SAMPLED).** When `/dev/fuse` or the tools are
unavailable, or `disk.backend=counter`, the engine's used-bytes counter is
promoted to an authority over **running** processes: a watchdog polls each
active run's workspace usage every `disk.counter_poll_ms` and cancels a run that
exceeds its allowance. This is REAL but not instantaneous — a process already
writing can overshoot between two polls. The overshoot is bounded by
`write_rate × counter_poll_ms`, and it is **measured**, not merely bounded:
with `disk_mb=64`, `counter_poll_ms=500` and a writer creating 4 MiB files (so
`rlimit_fsize`, which bounds ONE file, never binds), six runs on this host
overshot by **48 MiB, 52 MiB, 72 MiB, 172 MiB, 360 MiB and 1224 MiB** before the
watchdog cancelled them — the FUSE-less local filesystem is fast enough that the
overshoot is the writer's throughput over one poll, up to ~20× the allowance,
and the cancelled run keeps the bytes it already wrote. The kernel-boundary
backend, by contrast, refuses the crossing write outright and never overshoots.
**The fallback is never presented as equivalent to the kernel boundary**, and
`/v1/meta.workspace_limits.mechanism` names which one is in use (`fuse` or
`counter`). If neither can run (`disk.enabled=false`, `disk.backend=none`) the
allowance is neither reported as the workspace's own nor enforced, and that is
stated.

**`rlimit_fsize` stays consistent.** It remains `disk_mb` per file (a coarse
per-file backstop); the filesystem now bounds the directory as a whole, so the
two agree rather than conflict.

### ADR 86 — a curated per-workspace /proc, so free/vmstat/df report the workspace

**Problem.** `mount_proc` is deliberately `false`: the real procfs leaks host
process, network and kernel-identity information. But `free -h` was absent from
the profile, `/proc/meminfo` and `/proc/stat` did not exist, and a tool that
needed them failed.

**Decision — a curated, per-workspace /proc payload, read-only.** The engine
generates a directory from the workspace's OWN limits and bind-mounts it
read-only over `/proc` (a curated directory, not the real procfs; `mount_proc`
stays false). It carries exactly: `meminfo` (MemTotal = the memory ceiling,
SwapTotal 0), `cpuinfo` (exactly `cpu_cores` processor entries), `stat`,
`uptime`, `loadavg`, `version` (the same kernel string the shim's `uname`
returns), `vmstat`, `filesystems`, the mount table in BOTH shapes —
`mounts`/`self/mounts` (the `/proc/mounts` form) and **`self/mountinfo`** (the
real procfs `mountinfo` form coreutils `df` reads) — plus `tab` bound over
`/etc/mtab`, and `self/limits`. Both mount shapes are rendered from the SAME
`procMount` list so they can never disagree; the source token is the neutral
`/dev/workspace`, so no host path or `zfs` type is named. **`/etc/mtab` is not
required by `df`** (it opens `/proc/self/mountinfo` alone, measured) but is
provided because the conventional `/etc/mtab` view exists on a normal host.

**Why it is gated on masking.** The curated `/proc` and the LD_PRELOAD shim are
the SAME channel viewed two ways: the shim answers `uname`/`sysconf`/`sysinfo`
with the workspace's values, and the payload answers `meminfo`/`cpuinfo`/
`version` with the SAME values. With masking OFF (a diagnostic state) neither is
mounted, so the channels can never disagree. Every value agrees with the API
record and with `os.sysconf`/`os.cpu_count`/`os.statvfs`; **two differently
limited workspaces differ on every channel**, which is asserted explicitly
because identical numbers across different limits is the hardcoded-value
signature.

**Config, default ON.** `disk.enabled: true`, `disk.backend: auto` (fuse when
available, else counter), `disk.proc_enabled: true`. `disk.enabled=false` is the
off switch and is reported truthfully in `/readyz.workspace_limits` and
`/v1/meta.workspace_limits` (the honest limits, and the exact `/proc` file list,
are published there).

**`top`/`ps` are deliberately excluded** from the profile: they enumerate
`/proc/<pid>/*` for every process, and the curated `/proc` carries no
per-process entries (the real procfs would leak host process information). They
would print a confusing "Error, do this: mount -t proc proc /proc" rather than a
truthful view, so the profile omits them and its `notes` says why.

**Proof.** `scripts/identity-check.sh` §1b/§1c assert the curated `/proc`
reports only the workspace's own facts (host memory asserted ABSENT), that
`/proc/self/mountinfo` and `/etc/mtab` are present and agree with `/proc/mounts`,
that bare `df -h`, `df -h /workspace`, `df -h /` and bare `df` all exit 0 and
show the workspace its own size, and that `df`/`stat -f`/`os.statvfs` agree with
the allowance while a write past it fails `ENOSPC`; measured `131 passed / 0
failed / 2 skipped` (from 113/0/2), green five consecutive runs.

## v0.0.19 — workspace subscriptions (a term) and a fast workspace summary

Two features, designed as separate code: a **subscription** (a running term with
an absolute expiry, enforced by a reaper) and a **workspace summary** (a cheap
derived view on the get, the list and `/v1/meta`). Both are config-gated, and
**their defaults change the behaviour of no existing workspace or gate**.

### ADR 87 — the engine owns the clock, not the billing; absolute instants, no counters

**Decision.** A workspace carries an optional `model.Subscription` holding
*absolute instants*: `GrantedAt`, `ExpiresAt`, `RenewedAt`, `LockedAt`,
`GraceEndsAt`, and an `Extensions` count. A **nil subscription (or a zero
`ExpiresAt`) means NO expiry** and is the default. The set of automatic
behaviours is decided by a **pure function** of `(now, subscription, config)` in
`internal/subscription`, unit-testable without a daemon, and a single periodic
reaper (copied from the shape of `disk_watchdog.go`, the only existing ticker — a
goroutine with one `time.NewTicker` and one done channel) applies it.

**Why.** This service is a **pure sandbox engine**. Accounts, billing, plans and
quotas live in a separate Telegram-bot product that calls this API, so the engine
must not implement billing or money. It owns the **clock** and the
**enforcement**; the client owns *why* a term is what it is. Therefore the engine
stores an **absolute expiry instant**, never a "days remaining" counter: a
renewal is the client sending a new/extended expiry. This keeps billing out of
the engine and makes restarts trivially correct — state is recomputed from
stored absolute times and never from in-memory counters. The reaper holds **no**
term state of its own.

**Config-gated, defaults change nothing.** The `subscription:` block has
`enabled` (default `true`), `default_duration` (**empty = no expiry**),
`max_duration` (`365d`), `grace_period` (`168h`) and `reap_interval` (`60s`). A
workspace created without a subscription (and with `default_duration` empty) has
no `ExpiresAt`, is never selected by the reaper, and behaves exactly as before.
With `enabled: false` the loop does not run at all, so nothing is automatically
locked or deleted; a per-request term is still accepted and still reads `locked`
once lapsed (reads never mutate, and the derived status is a pure function of the
stored instants), but no resource teardown happens.

**Proof.** Unit tests in `internal/subscription` and
`internal/service/subscription_test.go` inject a mutable clock and assert the
whole lifecycle without sleeping; `scripts/subscription-check.sh` drives the REAL
service and nsjail with a compressed clock (`grace_period` seconds,
`reap_interval` seconds) and asserts the same rules end to end.

### ADR 88 — the duration grammar, and `mo`/`y` are fixed-length approximations

**Decision.** A term is given as **either** a `duration` string (the engine
computes `expires_at = now + duration`) **or** an absolute RFC3339 `expires_at`
(the client owns its calendar). The duration grammar is one or more
`<number><unit>` pairs summed together, units `s m h d w mo y`, where `d`=24h,
`w`=7d, **`mo`=30d** and **`y`=365d**. A **bare integer is refused** (it is
ambiguous: seconds? minutes?) — a unit suffix is required. Zero, negative,
fractional and unparseable durations are `422 subscription_invalid`. Go's
`time.ParseDuration` only reaches hours, so this is a small dedicated parser
(`internal/subscription.ParseDuration`) with an explicit ceiling (100 years) so a
typo cannot overflow `time.Duration`.

**Why.** `mo` and `y` are **fixed-length approximations, never calendar-exact**:
30 days is not a month and 365 days is not a year. This is documented here and in
the API/config/ADR commentary, and the parser's `mo`/`y` handling is asserted in
unit tests. A client that needs a calendar month or year must send an absolute
`expires_at`; the engine never pretends to compute a calendar.

### ADR 89 — `locked` is not `suspended`; the exact allow/refuse split

**Decision.** A distinct `locked` status is added. It is **not** the same as the
existing `suspended`: `suspended` is a **manual, reversible-by-API** action
(`POST .../suspend` / `.../resume`, `409 workspace_suspended`); `locked` is the
**automatic** consequence of a lapsed term and the way out is a **renewal**
(`POST .../subscription/extend`), not `resume`. The refused code is a distinct
`403 subscription_expired` so a client can tell the two causes apart. `active`
and `deleted` stay. The derived status is a pure function of the stored instants
(`now >= expires_at`, or a recorded `LockedAt`), so a lapsed workspace reads
`locked` even before the reaper has physically torn it down; the reaper skips a
`suspended` workspace (a manual operator action is left alone), and the persisted
status is never overridden for `suspended`/`deleted`.

**Locking actively stops using resources, not merely labels.** On locking the
reaper stops running runs, kills sessions and detached processes, deletes the
workspace's tunnels, and **unmounts the per-workspace disk image** (releasing the
`fuse2fs` process) while keeping every byte of user data.

**The exact split, stated and asserted:**

- **Refused while locked** (`403 subscription_expired`): `exec`, `runs`,
  `sessions`, `tunnels`, `packages/install`, and every **mutating** `files`
  operation (`PUT /files/content`, `DELETE /files`, `POST /files/mkdir`,
  `POST /files/rename`, `POST /files/upload`). A locked workspace cannot consume
  compute — asserted: a running workload is stopped and the process inventory
  drains to zero.
- **Allowed while locked**: `GET /v1/workspaces/{id}` and its summary,
  `GET /v1/workspaces` (list), `GET /usage`, `GET /files` and every file read
  (`stat`, `content`, `download`), and `DELETE /v1/workspaces/{id}` so it can be
  removed early.

**Enforcement is API-side, not merely reaper-side.** `ensureActive` refuses on
the *derived* status, so the lock is enforced from the stored instants even if
the reaper has not yet run or is disabled. The mutating file methods gain the
same gate. The summary and the gate read the same clock and the same pure
function, so if the summary says `remaining_sec: 0` with `state: locked` the
workspace really is refusing compute (asserted).

### ADR 90 — extension semantics: additive while live, from-now when lapsed, never shorten

**Decision.** `POST /v1/workspaces/{id}/subscription/extend` applies a renewal:

- **While the term is still live:** the extension **ADDS** to the existing
  expiry (`ExpiresAt += duration`), so a user who renews early is not penalised.
  An absolute `expires_at` **earlier** than the live one is refused
  (`422 subscription_would_shorten`); a live term is **never shortened** (silently
  shortening a paid term is the worst option, so the engine refuses rather than
  clamps).
- **After the term has lapsed (`locked`):** the extension starts from **now** and
  **unlocks** the workspace, restoring service. This is the renewal flow and the
  most important case; it is asserted end to end (exec works after).
- A server-side ceiling (`subscription.max_duration`, measured from now) is
  enforced: beyond it is `422 max_duration_exceeded`.
- Extending a `deleted` workspace is `404` and **never resurrects** a purged one
  (asserted: the metadata does not come back).

### ADR 91 — the reaper: pure decision, injected clock, idempotent, crash-safe, backwards-clock rule

**Decision.** The reaper's decision is a **pure function** `Decide(now, sub,
grace)` returning `none` / `lock` / `delete`:

```
no expiry                          -> none   (unlimited)
now <  ExpiresAt                   -> none   (live)
ExpiresAt <= now < GraceEndsAt     -> lock
now >= GraceEndsAt                 -> delete
```

The loop is a single goroutine with one ticker (copied from `disk_watchdog.go`),
started from `main` beside it, gated by `subscription.enabled`. The clock is
**injected** (`func() time.Time`, the `now` field), so tests are deterministic
and never sleep for days, and the acceptance harness drives a compressed clock
through config.

**Idempotent and crash-safe.** Lock and delete are safe to re-run after a crash
mid-operation: an already-unmounted workspace is a no-op
(`fusermount -u` on a non-mount returns nil), an already-gone `disk.img` is a
no-op, and a delete of an already-gone tree is handled. Verified by re-running a
tick twice (asserted) and by a `LockedAt` guard that keeps a second lock from
overwriting the first. Tear-down order is **stop compute → unmount → (for delete)
purge**, and nothing leaks: no `fuse2fs`, no mount, no cgroup, no netns, no orphan
`disk.img` (asserted in the gate). One workspace's failure is logged and the loop
continues to the others.

**Two observable phases, and the latency between them (measured).** The status a
client reads flips to `locked` **the instant the stored `ExpiresAt` passes** -- it
is derived from the stored instants, so a summary can never claim time remains
while enforcement disagrees -- but the **resource teardown** (stopping the
workload, unmounting the disk, releasing the `fuse2fs` helper, clearing the
cgroup/netns) happens on the **next reaper tick**. Measured on this host with
`reap_interval: 1s`: 0.26s and 1.06s between reading `locked` and the disk
actually being released. **With the default `reap_interval` of 60s that window is
therefore up to ~60s.** This is a *release* delay, not a hole: work is refused
from the first instant the term lapses (`403 subscription_expired`), so a lapsed
workspace cannot execute or consume anything in that window; only the un-mounting
trails. An operator who wants the trailing window tighter lowers `reap_interval`;
the measured trade-off is one tick of scheduling versus mount churn.

**The backwards-clock rule.** A backwards wall-clock jump must not cause a
delete. Every comparison is against a **stored absolute instant**, never an
in-memory countdown, so a backwards jump can only *delay* an action. To make that
explicit and robust, a `delete` is **refused while `now` is before the recorded
`LockedAt`** (a non-monotonic reading): the workspace stays locked (service is
already stopped) and is deleted on a later, monotonic tick. Asserted in the pure
function (boundary: at exactly `LockedAt` a delete is allowed again) and against
the real reaper (a workspace whose `LockedAt` is rewritten into the future
survives ticks).

### ADR 92 — native archives: in-engine extraction, the streaming budget, and the refusal codes

**Decision.** Archive upload/extract, in-place extract and directory export are
performed **in the engine** (`internal/archive`), never by shelling out to
`unzip`, `tar`, `unrar` or any host tool. Extraction reads **ZIP** and **RAR**
(RAR4 and RAR5); export writes **ZIP only**.

**Why in-engine, not a host tool (measured).** This host has **no RAR tool at
all** — `unrar` is not installed and no equivalent is present — so a shell-out
design would simply fail for RAR. The one candidate that IS sometimes available,
`unrar-free`, **cannot read RAR5**, the format modern `rar` produces by default;
depending on it would silently cap the feature at an obsolete format. More
fundamentally, the archive is **untrusted input** and the trust boundary must
live in the engine: a host binary that may be absent, a different version, or
absent from a future host is not a boundary this service can guarantee. In the
engine the behavior is identical on every host and every refusal is
deterministic.

**The dependency choice: `github.com/nwaples/rardecode/v2`.** It is **pure Go,
MIT-licensed, and requires no cgo**, so it compiles into the single static
binary with no extra host requirement; it reads both RAR4 (a.k.a. RAR 3.x) and
RAR5 and reports encryption, multi-volume and link entries through typed
errors/fields. **RAR is read-only by design**: RAR's encoder is proprietary and
no permissively-licensed RAR writer exists, so the engine never writes RAR and
never claims to. Export produces a ZIP; a caller who uploaded a RAR downloads a
ZIP. This asymmetry is deliberate and documented in the API.

**Format detection is from content, never the filename.** The leading magic
bytes decide: ZIP local/empty/spanned headers, or the shared `Rar!\x1a\x07`
prefix for RAR4 (`...\x07\x00`) and RAR5 (`...\x07\x01\x00`). A `.zip` that is
really a RAR still works, and an unrecognized format is refused naming the
detected signature. The filename is never trusted.

**Never trust the archive's headers.** An archive is an attacker-controlled
stream of claims. Two rules follow, and both are load-bearing:

1. **Validate everything before writing a byte.** The full header list is read
   and every entry decided (path cleaning, kind, caps) *before* the first byte
   is written, so a refused archive leaves **no partial tree**.
2. **Budget the bytes actually written, not the bytes promised.** The declared
   uncompressed sizes are only a cheap pre-check; the real guard is a **streaming
   budget** (`copyBudgeted`) that counts what it writes against `max_total_mb`
   and `max_file_mb` and stops at the cap. A header that lies about its size
   therefore cannot overshoot — the on-disk total can never exceed the budget.
   (This is also why a RAR header's `UnKnownSize` is handled by counting writes
   rather than trusting the field.)

**Name safety.** Every entry name is put through the workspace's **own** cleaner
(`workspace.CleanRelative`), never a second validator, so the archive path model
and the API path model cannot drift. A name that cleans to an escaping path is
refused, and an **absolute** name or a **drive letter** is refused **explicitly**
even though the cleaner would strip a leading slash — the archive must not look
like it accepted an absolute path. Two entries that normalize to the same path
(a collision) are refused rather than last-wins: refusing is safer and the
workspace filesystem is case-sensitive.

**Symlinks and hardlinks are refused, never materialized.** The engine does not
try to decide whether a link target is safe; it simply never creates one. This
closes the canonical "a symlink entry whose target is `..`, followed by an entry
that writes *through* it" escape outright. Devices, FIFOs and sockets are refused
with the same code.

**Privileged mode bits are stripped.** `SanitizeMode` takes the permission bits
only, which by definition drops `setuid`, `setgid` and sticky; an archive that
asks for `4755` lands as `0755`, and an entry with no usable permission bits gets
a safe default. No archive content can cause a privileged file.

**Collision policy.** The default is `overwrite:false`:
an entry that would clobber an existing file is refused with **`409
archive_entry_exists`** and the existing file's **content is preserved** —
asserted. A request opts in to replacement with `overwrite:true` (or the server
default `archives.overwrite`). Refusing is the safe default; replacing is an
explicit choice.

**`dry_run` semantics.** With `dry_run:true` the archive is validated and the
entries are reported (`entries_total`, each entry's `rejected`/`skipped` and
`reason`), and **nothing is written** (`bytes_written:0`). A dry run **does not
fail on a rejected entry**: previewing a hostile archive is exactly its purpose,
so it reports `entries_rejected` and still returns `200`. A real extraction, by
contrast, refuses the **whole** archive if any entry is rejected.

**Export safety (the other direction).** `WriteZip` walks with `Lstat` semantics
(it never follows a symlink) and **skips** links (and `skip_junk` noise), so a
zip can never smuggle a link out of the workspace; the root itself is never
emitted. The export is built by the same package as extraction, so "upload a
project" and "download my project" are symmetrical.

**The stable refusal codes** (one of these for every archive refusal, so a
client can act and the safety rules stay inspectable):

| Code | HTTP | Condition |
|------|------|-----------|
| `unsupported_archive` | 415 | content matches no supported format, or the detected format is disabled by `archives.formats` |
| `archive_encrypted` | 422 | ZIP (flag bit 0/6) or RAR archive/entry needs a password |
| `archive_multivolume` | 422 | RAR `partN.rar` continues in another file |
| `archive_too_large` | 413 | total uncompressed over `max_total_mb`, or one entry over `max_file_mb` |
| `archive_too_many_entries` | 413 | more entries than `max_entries` |
| `archive_ratio_exceeded` | 413 | an entry's uncompressed/compressed ratio over `max_ratio` |
| `archive_unsafe_entry` | 422 | traversal / absolute / drive letter / collision, or a symlink/hardlink entry |
| `archive_entry_exists` | 409 | entry would clobber an existing file and `overwrite` is false |

A corrupt or truncated archive whose content WAS recognized is reported as `422
unprocessable` ("corrupt or truncated archive"), distinct from a format mismatch;
the daemon is unaffected (an ordinary returned error). If a case cannot be made
safe, it is refused with one of these codes — a refused extraction is always
better than a compliant-looking escape.

**The `rar3-*` naming trap.** The RAR4 fixtures under
`internal/archive/testdata/rar4/` were generated from files named `rar3-*`,
because **RAR3.x and RAR4 are the same format generation** — "RAR 3.0" through
"RAR 4.x" share one container format, and RAR5 is the next generation. The
directory is `rar4/` (the generation) while the source files are named `rar3-*`
(a legacy label); this is not a mismatch and not a mistake. `rar4/` and `rar5/`
are the two distinct readers exercised.

**Proof.** Pure unit tests in `internal/archive` (23 test functions) cover
detection, the plan, the budget boundaries, mode stripping, the overwrite policy
and the RAR refusals; `internal/api/archive_test.go` covers the three endpoints;
and `scripts/archive-check.sh` drives the REAL service and nsjail against the
COMMITTED fixtures and asserts the whole boundary, including that nothing is
written outside the destination, disk usage does not grow on a bomb, an
encrypted/multi-volume archive is refused, random/empty/truncated input is
refused with the daemon still alive, and an export is a valid zip.

### The fast workspace summary (Feature 2)

**Decision.** A `WorkspaceSummary` is computed from stored fields and **cheap**
syscalls only — no directory-tree walk, no heavy work — and is exposed inside
`GET /v1/workspaces/{id}`, as a `summary` field on **every** item of
`GET /v1/workspaces` (so a dashboard renders a list in one request), and
advertised in `GET /v1/meta` under `subscriptions`. It carries `state`
(`active`/`expiring_soon`/`locked`/`unlimited`, threshold
`min(10% of the term, 24h)`), `age_sec`/`created_at`, `last_activity_at`,
`runs{total,active}`, `disk{used_bytes,limit_bytes}` and `profile`. `disk.used`
is a single `statvfs` of the already-mounted filesystem (0 when not mounted — it
never walks). `remaining_sec` is clamped at 0 and is **never negative**; `state`
is 0 exactly when the term is locked (rounded up, so a live term never reports 0). The
numbers agree with enforcement because both read the same pure function.

**Status derivation.** The API renders the **derived** status (`DeriveStatus`),
so a lapsed-but-not-yet-reaped workspace already reads `locked` and cannot
contradict enforcement; the persisted `Status` is only changed by the reaper (to
`locked`) and by a renewal (back to `active`, clearing `LockedAt`). A record
edited so that it has an expiry but no `GraceEndsAt` still decides correctly:
`Decide` derives the grace end from `ExpiresAt + grace` when the stored value is
zero.

### ADR 93 — the per-workspace resource stream: one lazy service-wide sampler, statfs disk, summed cgroups

**Decision.** A workspace's RAM and disk are pushed every 3s (configurable) over
SSE, served by a single service-wide sampler in `internal/metrics`. The design is
chosen **from measurement**, not intuition, because the operator's hard
requirement is that it be so cheap that thousands of workspaces cause no
pressure: an implementation that samples per connection, or walks a directory
tree, is refused even though it would "work". Four routes are shipped —
`GET /v1/metrics`, `GET /v1/metrics/stream`,
`GET /v1/workspaces/{id}/metrics`, `GET /v1/workspaces/{id}/metrics/stream` —
gated on `metrics.enabled` and advertised in `GET /v1/meta`.

**The disk figure is `statfs`, never a tree walk (measured).** `statfs` is
O(1) and costs **1.7–1.9 µs**; the directory walk `Service.Usage()` performs
costs **153 µs per file** (82× worse per file) and is O(files) — a 17908-file
tree took **2742 ms**. At a 3s cadence the disk figure therefore **must** come
from a single `statfs` (`workspace.MountedDiskUsed`). `Service.Usage()`'s walk is
never called from the sampling path; a `tree_walk_reads` counter is exposed and
is always 0 as a contract guard against a future regression.

**A workspace's RAM is the SUM over its cgroups — runs AND sessions (measured).**
The cgroup is **per run and per session, not per workspace**. Two runs in one
workspace (limit `memory_mb: 160`) produced two `NSJAIL.<pid>` cgroups, each with
`memory.max = 167772160` (160 MB), so `sum(current) = 113 MB` and
`sum(max) = 320 MB` against a 160 MB workspace limit. Nothing in the tree gives a
workspace's RAM as one number, so **summing is the only correct answer**; reading
one cgroup and calling it "the workspace's RAM" is a wrong number, not an
approximation. Sessions likewise get their own cgroup (measured: creating a
session added `NSJAIL.<pid>`), so the sum covers run cgroups and session cgroups.

**`memory.max` is a per-RUN ceiling — a documented property, not a fix.** Because
there is one cgroup per run, a workspace with N runs can consume **N ×
`memory_mb`** (with the default `max_runs: 20`, that is 20× the advertised
figure). This was a pre-existing property of the engine's enforcement model and
was **deliberately not changed here**: this ADR (the stream) reports the summed
truth and recorded the per-run behaviour honestly. **ADR 94 then changed the
enforcement model itself** — one parent cgroup per workspace, limits on the
parent, nsjail pointed at it — so `ram_cgroup_limit_bytes` (the per-run ceiling
sum, which could exceed the advertised limit) was **removed** and
`ram_limit_bytes` became the ENFORCED parent ceiling. The stream's design in this
ADR is unchanged; only the enforcement model and that one contradictory field
are superseded by ADR 94.

**The disk-reporting bug this task fixed (measured).** `MountedDiskUsed`
computed `used = total − f_bavail`. `f_bavail` is the space available to a
**non-root** writer, which on ext4 excludes the filesystem's **reserved-block
margin** (mkfs.ext4's `-m 5` default), so the figure did not match the tenant's
own `df`: an **empty** 64 MB workspace was reported as **7639040 bytes
(11.9 % full)** while `df -B1` inside it said **90112**. `total − f_bfree`
matches `df` exactly (90112 empty, 10579968 after 10 MB). `MountedDiskUsed` now
uses `f_bfree`, still one `statfs` call. The operator's rule is that **the number
reported must equal the number the tenant sees**, so the gate asserts the
reported disk equals `df -B1 --output=used /workspace` run **inside** the
workspace within one block, for an **empty** workspace as well as a written one.
`df` shows `size=64M` but only ~`57M` *available* because ext4 reserves ~5 % for
root; the engine reports **used** (`total − f_bfree`), so the reservation is a
deliberate filesystem property the reporting does not blur.

**One lazy service-wide sampler (the architecture).** A single goroutine owns the
ticker in `internal/metrics.Sampler`. It is **not** started at boot: it starts
when the first subscriber registers and stops when the last one leaves, so a
service nobody watches costs **exactly zero**. `Ticks` and `Running` are exposed,
so the gate proves the lazy start (frozen with zero subscribers) and clean stop.
One ticker, one cadence, for every subscriber.

**Sample once, fan out.** Per tick the sampler builds **one** snapshot covering
every candidate workspace, encodes one frame per scope, and delivers it to every
subscriber. Sampling cost is therefore O(workspaces with something live),
**independent of the subscriber count**: the gate opens three subscribers and
asserts the per-tick cgroup reads are exactly 2 (one live cgroup × two files),
not 2 × 3.

**Idle elision.** A workspace with **no live cgroup** reports RAM 0 and costs
**zero file reads**; only its disk is read, and that is one `statfs`. Measured
against the sampler's own counters. The candidate set comes from the executor's
**existing** run→cgroup tracking (`RunCgroupDirs`, joined to the service run
table) and the service's live sessions — **never** a `/sys/fs/cgroup` scan per
tick. Only the workspace inventory (owner, limits) is refreshed, on a **slower
reconcile cadence** (`idle_reconcile_interval`, default 15s); create/patch/delete
also update it immediately so a new workspace is visible on the next tick. A run
whose cgroup was not resolved at launch (the bounded fork-race window can expire
on a loaded host) is re-resolved lazily from the nsjail pid the runner already
tracks — the same registry and resolver, not a second one — so a live run is
never reported as RAM 0.

**Read the minimum.** By default only `memory.current` + `memory.max` are read
per live cgroup. `cpu.stat` (31 µs) and `pids.current` (25.4 µs) are the most
expensive files and are **opt-in** behind `metrics.include_cpu`; tripling the
reads for fields nobody asked for is exactly the cost this task exists to avoid.
cgroup files need root (the service is root); **no `sudo` on the sampling path**
— measured, `sudo -n cat` is ~9 ms versus ~23 µs direct, a 400× penalty that
alone would break the cheap requirement. Nothing on the sampling path reads
`/proc/<pid>/environ` (~9 ms per call) or shells out.

**The honest cost model (measured).**

```
per live cgroup, per tick (2 files)      ≈ 46 µs
per workspace, per tick (1 statfs)       ≈  1.7 µs
1000 workspaces x 1 live run:  1000 statfs + 1000 cgroups x 46µs  ≈  48 ms/tick  ≈ 1.6% of one core
10000 workspaces x 1 live run:                                    ≈ 480 ms/tick ≈ 16%  of one core
```

The stream stays cheap because idle workspaces are nearly free. A deployment
with 10k simultaneously-busy workspaces is a CPU question about cadence, which is
why the cadence is configurable and **clamped**.

**Coalescing, never a queue.** Each stream holds **at most one pending frame**. A
subscriber is a single-slot channel; if the client has not read the previous
frame the sampler **drops** the new one (it does not buffer), so a slow client
can never grow unbounded engine memory. Drops are counted per subscriber and
globally and exposed (`dropped_frames`); the gate stalls a reader with a tiny
socket buffer and asserts the counter moves. A subscriber is removed on **every**
exit path (a `defer`, so a panic path is covered), which is what stops the
sampler when the last one leaves — the failure that would otherwise turn "costs
nothing when idle" into "costs something forever".

**The interval floor is enforced.** Default `3s`; `min_interval` (default `1s`)
is a hard **floor**: a client requesting a faster cadence is **refused with 422**
(never silently raised — silently ignoring a requested cadence is worse than
refusing it), and a value above `max_interval` (default `300s`) is clamped down.
A per-request `?interval=` override adjusts the one service-wide cadence and is
clamped to the floor; the default is restored when the last subscriber leaves.

**Proof.** Pure unit tests in `internal/metrics` (interval clamp, percentage
arithmetic including a zero limit, cgroup parsing, the sum across cgroups, idle
elision, fan-out, coalescing, lazy start/stop, scope isolation) plus unit tests
for the `f_bfree` fix; and `scripts/metrics-check.sh` drives the REAL service,
real nsjail and a real FUSE disk and asserts the whole cheap-stream contract —
including the `df`-inside-the-workspace assertion that caught the `f_bavail`
bug, the SUM assertion (two runs exceed one cgroup), idle elision, fan-out, no
tree walk, the floor, coalescing, clean disconnect, and a full leak check.

### ADR 94 — one cgroup per WORKSPACE: the advertised ceiling is the enforced ceiling

**Decision.** A workspace's memory/CPU/pids ceiling is enforced by **ONE parent
cgroup per workspace**, owned by the engine, whose `memory.max` is the
workspace's advertised `memory_mb`. nsjail is pointed at that parent via
`cgroupv2_mount`, so every run's and session's leaf cgroup is created **inside**
it and the parent bounds the SUM across concurrent runs. This makes the RAM half
behave like the DISK half, which already gives each workspace its own
kernel-enforced allowance (ADR 85): a workspace is a server-shaped thing with its
own quota, and no process inside it can get around it.

**The measured finding that decided this (do not re-derive).** The ceiling was
**per run**. `cgroup_mem_max: {{ .MemoryBytes }}` goes into the nsjail config and
nsjail created one cgroup per launch at `/sys/fs/cgroup/NSJAIL.<pid>`, directly
under the root. Measured: one workspace at `memory_mb: 128` with **three** runs
produced **three** cgroups, EACH with `memory.max = 134217728`, so the workspace
could consume 384 MB against an advertised 128 MB, and `live_cgroups` equalled
the run count. `ADR 93` reported this honestly; this ADR fixes the enforcement.

**Moving an existing cgroup is EPERM (measured) — do not retry it.** Creating a
parent, delegating controllers, and `mv`-ing an existing `NSJAIL.<pid>` into it
fails with `mv: cannot move … : Operation not permitted` and `os.rename`
`errno=1` (EPERM). A cgroup cannot be reparented after creation, so the parent
must exist **before** nsjail launches and nsjail must be **told** to use it.
That is the whole reason the mechanism is `cgroupv2_mount` rather than a
move/hook.

**`cgroupv2_mount` is the mechanism and it works (measured).** With a parent
cgroup created and `+memory +cpu +pids` delegated into its
`cgroup.subtree_control`, and nsjail's `cgroupv2_mount` set to that parent, the
parent's `memory.current` summed the child (measured 87363584 for the child's
memory) and **zero** `NSJAIL.*` were left directly under `/sys/fs/cgroup`; the
leaf appeared at `<parent>/NSJAIL.<leader>`. No nsjail upgrade and no kernel
feature is needed.

**Why nsjail must create the leaf, and why the per-run limits stay equal to the
workspace ceiling (measured).** nsjail creates its leaf cgroup only when at
least one per-run cgroup limit is set. Measured: with
`cgroup_mem_max=0 cgroup_pids_max=0 cgroup_cpu_ms_per_sec=0` nsjail created
**no** leaf cgroup at all (`leaf=NONE`, parent `memory.current=0`); with any one
set it does. Setting the per-run `memory_mem_max` to 0 "unlimited" would
therefore cost the engine its cgroup-scoped kill/inventory handle — the exact
handle the kill path depends on. The per-run limits are therefore left EQUAL to
the workspace's values (`cgroup_mem_max = memory_mb`, `cgroup_pids_max =
max_runs*64`, `cgroup_cpu_ms_per_sec = cpu_cores`); the **parent** still enforces
the SUM, so one run may use up to `memory_mb` and N runs share the same
`memory_mb` — they no longer each get their own. (Measured with the intended
config: two concurrent 100 MB allocations against a 128 MB parent drove the
parent's `oom_kill` counter and both runs exited 137.)

**What is enforced where.**

| Level | File | Meaning |
|-------|------|---------|
| Workspace parent `ws-<id>` | `memory.max = memory_mb` | the **advertised** physical ceiling, enforced as the **SUM** |
| | `memory.swap.max = 0` | no swap — the ceiling is a real physical ceiling, not a soft one |
| | `memory.oom.group = 1` | an OOM kills the whole workspace group, not one arbitrary process |
| | `pids.max = max_runs*64` | the workspace-wide process budget |
| | `cpu.max = <cpu_cores*1000000> 1000000` | the workspace-wide CPU quota |
| | `cgroup.subtree_control = +memory +cpu +pids` | the delegated controllers (a parent with delegation but no enabled child controllers silently enforces nothing) |
| Run/session leaf `<parent>/NSJAIL.<pid>` | same values as the parent | keeps the leaf created so the kill/inventory handle exists; a single run is limited alone while the SUM is limited by the parent |

**Reporting the real ceiling (the contract the tenant sees).** `ram_used_bytes`
remains the SUM over the live run AND session leaf cgroups, but `ram_limit_bytes`
is now the **parent cgroup's `memory.max`** (read from the kernel while a run or
session is live, falling back to `memory_mb` when idle). Because the kernel
enforces the SUM at the parent, `ram_used_bytes <= ram_limit_bytes` always holds
and `ram_percent` never exceeds 100. The **contradictory second limit field
`ram_cgroup_limit_bytes` was removed**: it was the SUM of the per-run
`memory.max` values, which could exceed the workspace allowance (measured 320 MB
against a 160 MB workspace), and a tenant reading `ram_percent: 106.3` with
`ram_used 142 MB > ram_limit 128 MB` read it as a broken contract. One limit, the
enforced one, is the honest surface.

**What happens at the ceiling (measured, and made observable).** With
`memory.max` and `memory.swap.max=0` set on the parent and `memory.oom.group=1`,
a workload that commits past the ceiling is killed by the kernel's OOM killer,
which sends SIGKILL to the workspace's whole process group. Measured: a single
200 MB allocation in a 128 MB workspace exited nsjail with code **137** and the
parent's `memory.events` recorded `oom_kill 2`, `oom_group_kill 1`, `max 37`.
`memory.oom.group=1` is worth setting: without it an OOM kills one process inside
the group (possibly a helper), whereas with it the whole run dies as one unit
that the engine can attribute. **An OOM at the ceiling must not read as a clean
`exited`**: since a `137` is also the exit an ordinary SIGKILL produces, the
executor reads the workspace parent's cumulative `oom_kill` counter from
`memory.events.local` **before and after** the run; an increase classifies the
run as the new terminal status **`oom`** (distinct from `exited` and from an
explicit `killed`). A missing counter reads as `-1` (no counter), never a
fabricated 0, so a signal is never misreported as an OOM. The parent persists for
the workspace's lifetime, so the counter is readable after the run.

**The kill path survived the layout change (and is gated).** The engine used to
find and reap a run's cgroup by assuming `/sys/fs/cgroup/NSJAIL.<pid>` under the
root (`cgroupDirs`, `resolveCgroup`/`CgroupForNsjailPID`, `reapTrackedCgroup`,
`ReapCgroup`, `untrack`, and `Kill` "prefers the run's cgroup"). Once cgroups
stopped living under the root, run killing would have **silently degraded** to
the process group — a `setsid()` survivor would live — which is a worse
regression than the bug being fixed and would still pass the old gates.
`CgroupDirOfPID` now returns the pid's **full** `/proc/<pid>/cgroup` membership
(correct for both the nested `ws-<id>/NSJAIL.<pid>` and a legacy root-level
`NSJAIL.<pid>`), `listNsjailCgroups` enumerates both, and `ReapOrphans` reaps
workspace parents and stray root leaves. `scripts/spread-check.sh` and
`scripts/process-stop-check.sh` assert that a `setsid()`-escaped process is
**still killed** by the workspace stop — the assertion that proves the kill path
survived.

**Lifecycle.** The parent is created idempotently before every run/exec/session
(so a workspace created before this feature, or one whose limits were `PATCH`ed,
converges on its next use) and removed on workspace delete **after** every
run/session/process has been stopped; a cgroup that still has processes is never
`rmdir`'d — `cgroup.kill` is written first and the members drain. On startup
`ReapOrphans` also removes any workspace parent left by a hard daemon death.

**The nsjail-version situation.** The installed nsjail is an older build: it
rejects `--log-level` and has no apt package, so building it from source would be
the only way to change its behaviour. A config-level fix (one parent cgroup +
`cgroupv2_mount`) needs **no** nsjail change, which is why it is the right
mechanism here.

**The disk reserve hole was CHECKED and is NOT a hole — do not "fix" it.**
Suspected: the archive budget might spend ext4's 5 % reserved margin, since a
`disk_mb: 64` workspace shows `df` size 64M but only ~57M *available* (measured:
`total − f_bavail` = 7.6 MB while only 90 KB is used). The call path was checked
before asserting anything: `internal/archive/extract.go` writes through a `Sink`
interface, `internal/service/archive.go` supplies the workspace `Fs`, and
`internal/workspace/fs.go`'s `reserve()` refuses when `used+delta > diskLimit`
with a `QuotaError` (and `isNoSpace(err)` from the fs itself maps to a
`QuotaError`). `statvfs`/`Bavail` is **not on the archive path at all** (it is
used only in `diskfs.go` for image/mount sizing). So an extraction's budget is
the workspace's own allowance and a workload writing directly is bounded by the
ext4 image extent: **no over-limit path, no reserve leak.** The disk mechanism is
correct and is left alone; the ADR records this so nobody later "fixes" a correct
mechanism. (`MountedDiskUsed` still reports **used** as `total − f_bfree`, which
equals `df`; the USED figure and the BUDGET are different quantities, and the
budget is the workspace's own allowance.)

**Proof.** Unit tests in `internal/executor` (the parent path derivation, the
delegation/limit writes, the OOM-counter parse), `internal/workspace` (the quota
boundary: exactly-at-the-limit and one-block-over), and `internal/metrics` (the
sum with the enforced ceiling, `ram_used <= ram_limit`, the parent `memory.max`
read); plus `scripts/spread-check.sh`, which drives the REAL service, real
nsjail and a real FUSE disk and asserts: one parent cgroup per workspace with the
delegated controllers; the leaf nested inside it and zero `NSJAIL.*` at the root;
three concurrent runs whose SUM exceeds `memory_mb` are killed (one run of
2 × `memory_mb` is refused, the parent records `oom_kill`, a run reads as `oom`);
`ram_used <= ram_limit` with `ram_percent <= 100`; a `setsid()`-escaped process is
still killed; the parent is removed on delete with nothing left behind; and the
disk budget is the workspace's own allowance.

### ADR 95 — the per-workspace write-RATE ceiling: sampled, not a kernel boundary

**Decision.** A workspace's write RATE is capped by a **SAMPLED** watchdog that
measures the workspace's real disk growth every metrics tick and **stops the
offending run(s)** with the distinct terminal status **`rate_limited`** after a
configured number `N` of **CONSECUTIVE** over-limit samples. The feature is
**DEFAULT OFF** (`disk.write_rate_enabled: false`), so it changes no existing
deployment. The one-line enable is `disk: { write_rate_enabled: true }` plus a
per-workspace `limits.write_rate_mb_s`. Disk TOTAL, RAM and CPU stay
kernel-enforced (filesystem boundary, cgroup); this one is sampled and is stated
as such everywhere it appears.

**Why a kernel throttle was not used here (MEASURED — do not re-derive).**
`io.max` cannot limit write speed on this host. The `io` controller exists
(`cgroup.controllers` contains it) but the root does not delegate it by default
(`cgroup.subtree_control` = `cpu memory pids`); delegating needs root, and a
delegating parent cannot hold processes (cgroup v2's no-internal-process rule),
so a workload must live in a grandchild. The **valid** throughput test started
the writer with **`CLONE_INTO_CGROUP`**
(`syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: fd}` — verified working here)
so it was **really inside** the cgroup, writing **64 MB** with
`oflag=direct,conv=fsync`: **no cap → 3.7 GB/s; `io.max wbps=5242880` → 3.6
GB/s** — identical — and the cgroup's **`io.stat` stayed at 0 bytes in both
runs**. The controller does not account the write at all. **Reason:** the
workspace image and `/` are on **ZFS**, and the `bio` is generated by ZFS
threads **outside our cgroup**; there is nothing on our path for the kernel to
account or throttle. An earlier attempt of this author's was **INVALID** because
the writer's pid never entered the cgroup (the empty `io.stat` proved nothing);
the valid test asserts `/proc/<pid>/cgroup` names the cgroup **before** drawing
any conclusion. This ADR records both so nobody re-tries the dead end, and so
nobody re-delegates `io` on the host root (nothing needs it).

**Why not a token bucket in the engine's own write path (the trap).** The
engine's Go code writes **API file uploads/writes** (`Fs.Write`/`CreateStream`)
and **archive extraction** (`archive.Sink` → the workspace `Fs`). But a tenant's
**own** process — `dd`, `git clone`, `pip install`, a build — does **not** go
through our code: it runs inside nsjail and its writes go **tenant → kernel →
FUSE → the `fuse2fs` process → the image**. `Service.Exec` and
`Service.StartRun` talk to the executor/nsjail, **never** to `Fs`. A bucket in
`Fs.Write` would throttle API uploads and **nothing else**, so it must never be
presented as a workspace write ceiling.

**What DOES see every byte: the workspace's real disk growth.** `statfs` on the
workspace mount costs ~1.7 µs (`workspace.MountedDiskUsed`, `f_bfree`), and the
metrics sampler already touches every workspace on its tick. Growth rate is
therefore measurable at essentially zero cost and covers writes from **any**
source — API, archive, or a tenant's `dd`. The watchdog rides that sampler's
tick (it starts **no timer of its own**); per tick, per workspace, it computes
`delta = used_now − used_prev` over the **monotonic** elapsed time and hence
bytes/second. The pure arithmetic is `advanceRate` in
`internal/service/write_rate.go`.

**A rate-kill is a DISTINCT outcome.** The offending run ends as
**`rate_limited`** — never a clean `exited`, never a generic `failed`, and
distinct from `killed` (an explicit stop) and `oom` (the memory ceiling). The
engine's **own** write path (an API write/upload, an archive extraction) has no
run to stop, so it consults the same watchdog's flag and is **refused with HTTP
429 `write_rate_exceeded`** while the workspace is over the ceiling. Both paths
therefore enforce the **same** sampled ceiling, each in the only way its source
can be stopped; this is stated rather than hidden.

**Sustained, not instantaneous.** A **single** burst above the ceiling must not
kill a run: spiky-but-brief writes are legitimate (a compile, an unpack). The
watchdog requires **N consecutive** over-limit samples (`disk.write_rate_samples`,
default **3**); any under-limit sample resets the counter, so an intermittent
writer is never killed and a sustained one is. This is why the rule is
sustained rather than instantaneous: an instantaneous rule would turn every
short legitimate burst into a kill and make the feature unusable.

**The overshoot is a NUMBER, and it is NOT a kernel boundary.** The maximum
overshoot is the ceiling **times the detection window**: with `N` samples and a
sampler interval `T`, the watchdog acts within **`(N+1)·T`** of a sustained
offender, so a workspace can write **`N·T·rate`** above the ceiling line before
it is stopped. For the **shipped defaults** (`N = 3`, interval `3 s`) that is
**`3 · 3 s · write_rate_mb_s = 9 × write_rate_mb_s` MiB**: a workspace at a
`write_rate_mb_s: 16` ceiling may write up to **~144 MiB** past the line before
the kill. It is never presented as equivalent to the disk TOTAL, which really is
kernel-enforced by the ext4/FUSE image (ADR 85). The gate measures the bytes
written before the kill and asserts they are inside `free_rate · (N+3) · T`.

**Failure modes handled explicitly.**
- **Idle costs nothing.** The watchdog adds **no** read: it reuses the disk
  figure the sampler already took (one `statfs` per candidate per tick). A
  workspace with no live run costs zero cgroup reads and zero extra statfs. The
  gate asserts the deltas on the sampler's own stream.
- **Not a live FUSE mount.** A workspace whose disk is not a live mount yields
  no trustworthy growth (`disk_complete: false`), so the rate state is reset and
  the existing total-ceiling path applies rather than a rate computed from a
  zero reading.
- **Usage moving DOWN (a delete)** resets the baseline and never yields a
  negative rate or a violation.
- **A wall-clock jump cannot fabricate a spike**: the elapsed time is measured
  **monotonically** (`time.Now()` carries a monotonic reading).
- **A workspace already at its disk ceiling** stops growing (the kernel refuses
  the writes), so it is reset and skipped — it must not read as a rate
  violation every tick.
- **The first sample has no baseline**, so a fresh workspace is never killed on
  its first tick.

**A rate ceiling may never exceed the total ceiling.** A workspace cannot write
its whole disk in under a second, so `write_rate_mb_s > disk_mb` is **refused**,
never silently accepted: `Config.Validate` refuses it at load, and
`checkRateWithinDisk` refuses it on create and on PATCH (including when
`disk_mb` is lowered under an existing rate).

**Proof.** Pure unit tests in `internal/service` (`advanceRate` arithmetic:
first-sample, falling usage, zero interval, no ceiling; the N-consecutive rule;
the observer paths: single burst, under-limit, at-disk-ceiling, falling usage,
non-live mount, engine refusal; default-off; the rate-within-disk rule) and
`internal/config` (the defaults are off/0, `write_rate_samples` defaults to 3,
and `Load` refuses a rate above `disk_mb`), plus `internal/metrics`
(pin/observer). The acceptance gate `scripts/write-rate-check.sh` drives the
REAL service, real nsjail and a real FUSE disk and asserts: default-off changes
nothing; a **tenant's own** writer (a runs-API process, NOT an API upload) ends
`rate_limited`; the disk grew and the offender stopped before ENOSPC; a single
burst does not kill; a run under the ceiling is untouched; a delete is not a
violation; idle adds one statfs and zero cgroup reads; the engine's own write
path is refused 429; advertised == enforced; the overshoot is inside the
documented window; a rate above `disk_mb` is refused; and nothing is leaked.

## v0.0.20 — native port/service discovery (what a workspace is listening on)

One feature, on-demand: `GET /v1/workspaces/{id}/ports` answers **which ports a
workspace is listening on and which process serves each one**, so the separate
bot product can tell a user "your app is live on port 8080". It reads the live
kernel state of the workspace's processes and is **not** part of any stream. It
shares its process registry with `GET /v1/workspaces/{id}/processes`, so the two
can never disagree, and it is orthogonal to **tunnels** (which publish a port to
the public internet).

### ADR 96 — port discovery is an engine endpoint because the tenant cannot see its own sockets

**Decision.** The engine answers a workspace's listening ports through an
**on-demand** `GET /v1/workspaces/{id}/ports`, by reading
`/proc/<pid>/net/{tcp,tcp6,udp,udp6,unix}` for a pid that is already inside the
workspace's own network namespace, and attributing each socket to its owning
process by matching the socket inode against the workspace's process registry.
Per endpoint it reports the port, protocol, bind address, a derived **scope**,
the state, and — when resolvable — the owning **pid / kind / label**; an entry
whose owner cannot be resolved is reported with an explicit **`attributed:
false`**, never dropped and never guessed. Unix sockets are reported in a
**separate `unix` section**, never mixed into the port list.

**Why the endpoint MUST exist (MEASURED — do not re-litigate).** Inside a
workspace the tenant cannot answer this itself. `/proc/net` **does not exist**
(`ls /proc/net` → `No such file or directory`), because the curated `/proc`
(ADR 86) ships only the chosen files; and **`ss` and `netstat` are absent** from
the profile (`/bin/ss`, `/usr/bin/ss`, `/usr/sbin/ss`, `/bin/netstat` all
missing). A user's app can **bind** a port and the user has **no way to see it**
from inside. The engine is therefore the only thing that can tell them — which
is the whole justification for the feature.

**Why `/proc/<pid>/net/*` and not `ip netns exec <ns> ss` (MEASURED — do not
build on it).** The workspace's network namespace is created by **slirp**
(`sbxdws-<hex>` plus `sbxdrelay`), and the run's process reports
`net:[4026554106]`; `ip netns exec net:[4026554106] ss` fails with
`Cannot open network namespace: No such file or directory`. Reading
`/proc/<pid>/net/*` needs **no namespace handling at all**, because the pid is
**already inside** the namespace. (`ss` is not installed anyway.)

**Cost, and therefore ON-DEMAND, NOT on the metrics tick (MEASURED).** A direct
open of `/proc/<pid>/net/tcp` from a root process measured **4.6 ms** (15 ms
through `sudo` — which is why the read is a direct file read and never shells
out). That is fine for one on-demand request and **far too expensive** for the
3-second metrics stream: 4.6 ms × 1000 workspaces = **4.6 s per tick**. The
read is therefore kept **out of `internal/metrics`**, and a monotonic
`ports.reads_total` counter (advertised in `/v1/meta`) lets a gate prove it:
the counter does **not** move across metrics ticks, and **does** move on a
`/ports` request (so the assertion is not vacuous).

**Attribution rule and its ambiguity.** `/proc/net/*` gives a **socket inode**;
the owner is the process whose `/proc/<pid>/fd/*` contains
`socket:[<inode>]`. The scan runs **only over the pids the workspace's process
registry already reports** (never a blind `/proc` scan, and `/proc/<pid>/environ`
is never read). Bounds are explicit (`ports.max_pids`, default 64;
`ports.max_fds_per_pid`, default 512). Handling: a pid that **disappears
mid-scan** is skipped; a socket held by **several** pids reports **all** of them
(`owners`, sorted by pid) and names the **lowest pid** as primary,
deterministically; a socket whose inode **no live pid holds** is reported
`attributed:false`. **A bound hit sets `complete:false`** with a `notes` entry —
the same honesty rule the disk figure uses — so a partial list is never shown as
total.

**Scope, and why it matters for publishing.** A bind address is classified
**`loopback`** (`127.0.0.0/8`, `::1`), **`wildcard`** (`0.0.0.0`, `::`) or
**`specific`**. This is exactly what a user who wants to publish a port needs to
know: a `loopback`-only listener is **not** reachable from outside the workspace
even once a port is published, while a `wildcard` binding is. The API docs say
plainly that **a reported port is listening inside the workspace's private
namespace and does NOT mean it is reachable from outside** — publishing requires
a **tunnel** (`limits.allow_tunnels` **and** `tunnels.enabled`, both required,
else 422), so a client must never tell a user their app is public merely because
it appears in this list. Every response carries that reachability note.

**UDP has no listen state.** TCP listening sockets are reported `state:
"LISTEN"` (kernel state `0A`). UDP has **no** listen state, so a bound UDP
socket is reported with an **empty** `state` — the engine says so rather than
inventing "LISTEN". A UDP socket is reportable when it is **bound** (remote
unspecified, local port non-zero); a connected client flow (a DNS query in
flight) is not a service and is not reported.

**Unix sockets: a separate section.** They are readable from
`/proc/<pid>/net/unix` and are how many dev servers expose a socket instead of a
port, but they are **not ports**. They are reported in a **separate `unix`
array** (`ports.include_unix`, default on) and **never** mixed into the port
list, so the shape cannot pretend a unix socket is a reachable port. A unix
socket is attributed by the same inode→process rule.

**Default is ON, deliberately.** `ports.enabled` defaults to **true**. This is a
considered choice, not an inherited "new is off" default: the feature is
**read-only introspection of the tenant's OWN workspace** — it reveals nothing
about the host, another workspace or the operator — and with it off the user has
**no way at all** to see their own services (they cannot see `/proc/net`, and
`ss`/`netstat` are absent). Disabling it would leave the user blind to their own
work. An operator who nonetheless wants the surface absent sets
`ports.enabled: false`, and the route then reads as **404 `ports_disabled`**
(the surface is simply gone), and `/v1/meta` reports it off.

**Isolation is the load-bearing property.** The read is scoped **by construction**
to the workspace's own pids, so a **host** listener (or another tenant's) can
never appear. `scripts/ports-check.sh` asserts this directly — it starts a real
listener on the **host** and proves it **never** appears in a workspace's list —
and asserts that two workspaces binding the **same** port number each see only
themselves. This is why the read is host-side and scoped, rather than anything
the tenant could reach.

**We do not weaken the curated `/proc`.** The masking work (ADR 86) hides the
host on purpose. Whether the tenant should also see its own `/proc/net/tcp` from
**inside** is a **separate** decision with its own gate; it is **not** done here,
and nothing in this feature adds `/proc/net` back to the workspace view.

**Files.** `internal/ports` (the pure `/proc/net` parser, scope derivation,
deterministic sort, and the attribution/report types), `internal/executor/ports.go`
(the bounded host read and the inode→pid attribution), `internal/service/ports.go`
(the registry-scoped `ListPorts`), `internal/config` (`ports:` block, default
on), the API route and its capability in `/v1/meta`, and the gate
`scripts/ports-check.sh` (real nsjail; asserts the scope/protocol/state, the
exact-owner attribution, the host-isolation assertion, the cross-tenant
assertion, the empty-200, liveness, determinism, the not-in-metrics assertion,
the meta advertisement, and the no-leak check).

## Additional defaults chosen during implementation

- **Auth header format**: `Authorization: Bearer <key>`. _Rationale_: conventional bearer scheme.
- **Error codes**: `unauthorized`, `forbidden`, `not_found`, `conflict`, `workspace_suspended`,
  `too_many_sessions`, `session_ended`, `tunnels_disabled`, `too_many_tunnels`,
  `unknown_profile`, `session_needs_interpreter`, `subscription_expired`, `subscription_invalid`,
  `max_duration_exceeded`, `subscription_would_shorten`, `validation`,
  `unprocessable`, `not_directory`, `payload_too_large`, `timeout`, `tunnel_url_unavailable`,
  `unavailable`, `internal`, `write_rate_exceeded`, `ports_disabled`. _Rationale_: fixed vocabulary mapped 1:1 from HTTP status.
- **CWD default**: `/` normalizes to the workspace root `/workspace` inside the sandbox.
  _Rationale_: the workspace is the only mounted path users should see.
- **Log offset semantics**: byte offsets into the log file; `next` is the offset to pass on the
  next poll; `eof` is true when the reader reached the current end of file.
  _Rationale_: deterministic and simple to resume.
- **Go directive `1.23`**: the brief targeted Go 1.22, but `koanf`'s YAML parser (and several
  transitive deps) declare a `go 1.23` floor, so `go.mod` requires Go 1.23+.
  _Rationale_: the mandated YAML config dependency cannot build on 1.22 without pinning
  unsupported older versions; 1.23 is the smallest compatible toolchain.

### ADR 97 — a staged-content restore must RECREATE special files, never read them

**Decision.** `copyTree` — the copy that moves a pre-existing workspace's entries into the
new ext4 image when the FUSE disk backend is first provisioned — handles a **FIFO** by
recreating the node (`syscall.Mkfifo`) instead of copying bytes, and **refuses** a socket
or a device node with an error naming the file rather than dropping it silently.

**Why (a real defect, hit in production on the first upgrade).** Provisioning the FUSE
backend for an already-existing workspace renames its entries into a sibling
`.disk-stage`, creates the image, mounts it, and copies the entries back. The copy treated
every non-directory, non-symlink entry as content and called `os.ReadFile` on it. A
workspace **legitimately contains a FIFO**: the terminal session creates `.term/in`.
**Opening a FIFO for reading blocks until a writer appears**, so the copy hung on that
entry. `.disk-stage` is removed only after a successful restore, so the directory
survived and **every file of that workspace stayed inside it, invisible**: the file
listing returned nearly empty and `exec` never answered. Staging is one-shot (it only runs
when `disk.img` is absent), so the migration is the only trigger — but **any workspace with
a terminal session has a FIFO**, which is exactly the workspace a user cares about.

**Consequences and limits.** A FIFO round-trips with its type and mode preserved. A Unix
socket or device node fails the migration with an explicit message; neither is expected in
a user workspace. Reading with `O_NONBLOCK` was rejected: it returns zero bytes and would
have converted a hang into silent data loss. Operators upgrading a live host should check
for a surviving `.disk-stage` beside a workspace and restore it, since the pre-fix code
left the data there.

**Regression cover.** `TestRestoreStagedContentSurvivesAFifo` stages a regular file plus a
top-level and a nested FIFO, runs the restore on a goroutine with a ten-second deadline,
then asserts both FIFOs came back as FIFOs and the file's bytes survived. It fails on the
previous implementation by hanging.

### ADR 98 — hide the ext4-internal `lost+found` from the workspace's visible root, on every provisioning path

**Decision.** A workspace's disk is a real ext4 image (`mkfs.ext4`) mounted host-side with
`fuse2fs` at the workspace root. `mkfs.ext4` creates `/lost+found` in **every** new
filesystem and `e2fsck` **recreates** it whenever it runs, so a fresh workspace's visible
root showed only `lost+found` (the operator's report: `ls` in a new workspace returned
nothing but that directory, looking like leftover junk). The engine therefore removes
`/lost+found` from a **mounted** workspace root on **every** provisioning path, in one
place, **after** `mkfs.ext4` and **after** any `e2fsck` repair. The directory may exist
inside the image; it must not appear in what the tenant sees at the workspace root.

**Why it is there (so the wrong cause is not chased).** It is a property of ext4, **not** of
this codebase: `mkfs.ext4` creates it, and the engine mounts the image root at the workspace
root, so the image's `/lost+found` is exactly what the tenant's `ls` shows. Measured: the
inode's `ctime` equals the image's own write time to the second.

**Why the fix must be a LIFECYCLE step, not a one-shot edit.** Measured on this host:
`e2fsck -p -f` on an image whose `/lost+found` was removed prints
`/lost+found not found.  CREATED.` and puts it back (exit 1). The engine runs `e2fsck` on an
image it finds dirty (a workspace whose sandbox was killed mid-write), so removing the
directory at image creation would be undone by the first repair. The removal is re-applied
after the repair instead.

**The mechanism, and the one rejected one (measured — do not re-derive).** Removal is a
plain `os.Remove` of `{root}/lost+found` through the **already-mounted** workspace root, in
`clearLostFound`, called from `Ensure` on each path (including the already-mounted-at-the-
right-size early return, so a repaired image that needs no resize still converges). It is
idempotent: a missing directory is not an error. Mounting a **subdirectory** of the image as
the workspace root (`fuse2fs -o subdir=`) was **rejected**: it is not available here, because
this build links FUSE library 2.9.9 and the option is rejected
(`fuse: unknown option 'subdir=/home'`), so a design that mounts the image's `/home` (or any
subdir) as the workspace root is not available. No new mount and no second read-write mount
is introduced: the workspace's own mount is already read-write.

**The trade-off, stated honestly.** `/lost+found` is where `fsck` reconnects **orphaned
inodes** after a corruption. Removing it leaves a repair with nowhere to put such files.
This is normally empty: `mkfs.ext4` creates it empty and the measured `e2fsck -p -f` that
recreates it recreates it empty, and an empty directory holds nothing. So **an empty
`lost+found` is removed and no data of the tenant's is lost**: `e2fsck` never moves a
reachable file into `lost+found`, so the tenant's **intact** files are untouched. The only
thing lost is the recovery of **orphaned inodes on a genuinely corrupt image** (files that
were already unreachable before the repair). To keep the safety net from being destroyed
silently, a **non-empty** `/lost+found` is left in place rather than deleted: the only way
it becomes non-empty is a repair that actually reconnected orphaned inodes, and deleting it
then would destroy exactly the data it exists to hold. In that rare, corruption-only case
the directory stays visible so the tenant can recover the recovered files, rather than being
hidden at the cost of their contents.

**One edge case, stated.** The removal is by name (`{root}/lost+found`), so a tenant who
deliberately creates an **empty** directory with exactly that name is indistinguishable from
the ext4 one and it is removed too; a **non-empty** one is left. Creating that name at the
workspace root is vanishingly unlikely, and the operator's requirement is explicit that the
root show only the tenant's real content, so name-based removal is accepted rather than
matching on inode/ownership (which the `uid=`/`fakeroot` mount makes indistinguishable from
the tenant's own files anyway).

**Boundary and behaviour unchanged.** The removal does not change the image file's size or
the `rlimit_fsize` ceiling, and it does not change the mounted total: `df`/`stat -f`/
`os.statvfs()` inside a workspace still report the workspace's own `disk_mb`, and a write
past it still fails with `ENOSPC`. No workspace loses a file. The removal runs only through
`DiskManager.Ensure` under the manager's own lock, on a mount the manager created or
verified, and never touches a live workspace outside that lock.

**Files.** `internal/workspace/diskfs.go` (`clearLostFound` and its call sites in `Ensure`)
and the gate `scripts/lostfound-check.sh` (real nsjail + a real FUSE disk; asserts the fresh
path, the **repair** path — it dirties a real image, restarts, and asserts the directory is
absent *after* `e2fsck` — existing-workspace convergence with no file loss, the unchanged
disk boundary, and a no-leak check). `TestDiskManagerHidesLostFound` is the unit half.
