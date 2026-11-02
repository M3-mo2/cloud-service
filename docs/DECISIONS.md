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
    installed build), so the rendered config passes `memory_mb` / `disk_mb` directly.
    _Rationale_: passing bytes multiplied every ceiling by ~10^6, effectively disabling them.
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
**workspace's** limits. Two properties make it robust against the obvious
evasions, both **measured**:

- **`/etc/ld.so.preload` is read by the dynamic loader for every
  dynamically-linked exec, independent of the environment.** A tenant cannot
  defeat it with `env -u LD_PRELOAD` or `env -i`: `env -i PATH=… nproc` and
  `env -i PATH=… uname -r` still return the masked values
  (`scripts/identity-check.sh` assertion 4). This is the reason `LD_PRELOAD`
  alone (an environment variable, trivially cleared) was **rejected**.
- **Both the shim and the preload list are bind-mounted `rw:false` and the
  mounts are `mandatory:true`.** A tenant cannot replace the shim, and if either
  mount cannot be applied nsjail aborts the run rather than starting a sandbox
  that would leak (`identity-check.sh` asserts the shim and preload files are not
  writable; the Go test asserts the mount is read-only and the render refuses
  with `ErrMaskUnavailable` when the shim cannot be materialized).

**Inherently bypassable vs a gap we closed — a corrected classification.** An
earlier version of this ADR filed *every* `dlsym`/`dlopen` route as "not closable
by this mechanism". That was wrong: a **handle-scoped `dlsym`** of an interposed
symbol is a gap in the shim, not a property of preload interposition, and it is
now **closed**. The two classes are kept separate on purpose:

**A. Closed this iteration (a gap in the shim, now shut).**
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

**Decision.** A new default-on switch `identity.hide_provider_tools`. When it is
on, the sandbox **overlays** `/usr/local` and `/etc/ssl/certs` with curated,
read-only copies:

- **`/usr/local`** is replaced by an operator-providable curated directory
  (`identity.tools_dir`). When no directory is configured it is **empty**: the
  workspace keeps the whole of `/usr/bin` (where `python3`, `node`, `bash`,
  `perl`, `git`, `curl` and the Go-less toolchain live) and sees **no** host
  `/usr/local` material. A curated directory is preferred over exposing a real
  host directory because the host directory is exactly what leaks.
- **`/etc/ssl/certs`** is rebuilt from the host's own trust store with every
  certificate that **names the provider** dropped — from the per-certificate
  files and from the concatenated `ca-certificates.crt` bundle. The provider's
  name lives in the **decoded DER** subject/issuer, not the base64 PEM text, so
  the filter decodes each certificate before testing (a base64 substring test
  finds nothing; measured). Generic roots are untouched, so TLS keeps working.

Both mounts are `mandatory: true`: if a curated tree cannot be mounted the run
**aborts** (`503 sandbox_unavailable`) rather than expose the host's directory —
the same fail-loud contract as the masking shim. The overlay is materialized by
the same one-time `masker.prepare()` pass into the service tmp dir, owned by the
orchestrator; a workspace cannot write it, and it is bind-mounted read-only.

**Why not "hide the strings by stripping binaries"?** The binary is the
operator's; modifying it would break their tooling and signatures. The workspace
does not need it, so the correct fix is not to mount it.

**Vendor-string audit is asserted, not assumed.** `scripts/identity-check.sh`
assertion 8 runs `strings` over **every** mounted binary path (not just checks
that `zcp` is absent) and asserts none names the provider, while asserting
`python3`/`node`/`bash`/`perl`/`git` still run. This is the lesson from the
brief: `zcp`/`zsc` were not the whole list.

**Honest limit.** `/usr/bin` still contains distro packages; the filter hides
*files that name this provider*, not every byte of a large distro. It does not
touch the egress address (ADR 76).

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

## Additional defaults chosen during implementation

- **Auth header format**: `Authorization: Bearer <key>`. _Rationale_: conventional bearer scheme.
- **Error codes**: `unauthorized`, `forbidden`, `not_found`, `conflict`, `workspace_suspended`,
  `too_many_sessions`, `session_ended`, `tunnels_disabled`, `too_many_tunnels`, `validation`,
  `unprocessable`, `not_directory`, `payload_too_large`, `timeout`, `tunnel_url_unavailable`,
  `unavailable`, `internal`. _Rationale_: fixed vocabulary mapped 1:1 from HTTP status.
- **CWD default**: `/` normalizes to the workspace root `/workspace` inside the sandbox.
  _Rationale_: the workspace is the only mounted path users should see.
- **Log offset semantics**: byte offsets into the log file; `next` is the offset to pass on the
  next poll; `eof` is true when the reader reached the current end of file.
  _Rationale_: deterministic and simple to resume.
- **Go directive `1.23`**: the brief targeted Go 1.22, but `koanf`'s YAML parser (and several
  transitive deps) declare a `go 1.23` floor, so `go.mod` requires Go 1.23+.
  _Rationale_: the mandated YAML config dependency cannot build on 1.22 without pinning
  unsupported older versions; 1.23 is the smallest compatible toolchain.
