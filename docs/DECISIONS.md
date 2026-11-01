# Decisions

All resolved decisions for sandboxd v0.0.5. Each decision is fixed for this version;
deferred work is noted explicitly. Do not re-litigate these without a new version.

1. **Isolation model**: per-run user namespaces + shared `sandbox` UID/GID + per-run cgroups.
   _Rationale_: cheap, portable isolation that works without a per-user UID allocation scheme.
   Per-UID-per-workspace deferred to v0.1.
2. **Network**: disabled by default; workspace field `allow_net: bool` (default false).
   _Rationale_: least privilege by default; no host allowlist exists in v0.0.5.
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
17. **Version**: the service version string is `0.0.5`.
    _Rationale_: single source of truth matching the API version contract.
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

## Additional defaults chosen during implementation

- **Auth header format**: `Authorization: Bearer <key>`. _Rationale_: conventional bearer scheme.
- **Error codes**: `unauthorized`, `forbidden`, `not_found`, `conflict`, `workspace_suspended`,
  `validation`, `unprocessable`, `payload_too_large`, `timeout`, `unavailable`, `internal`.
  _Rationale_: fixed vocabulary mapped 1:1 from HTTP status.
- **CWD default**: `/` normalizes to the workspace root `/workspace` inside the sandbox.
  _Rationale_: the workspace is the only mounted path users should see.
- **Log offset semantics**: byte offsets into the log file; `next` is the offset to pass on the
  next poll; `eof` is true when the reader reached the current end of file.
  _Rationale_: deterministic and simple to resume.
- **Go directive `1.23`**: the brief targeted Go 1.22, but `koanf`'s YAML parser (and several
  transitive deps) declare a `go 1.23` floor, so `go.mod` requires Go 1.23+.
  _Rationale_: the mandated YAML config dependency cannot build on 1.22 without pinning
  unsupported older versions; 1.23 is the smallest compatible toolchain.
