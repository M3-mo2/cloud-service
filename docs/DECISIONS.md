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
