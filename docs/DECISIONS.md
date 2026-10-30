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

## Additional defaults chosen during implementation

- **Auth header format**: `Authorization: Bearer <key>`. _Rationale_: conventional bearer scheme.
- **Error codes**: `unauthorized`, `forbidden`, `not_found`, `conflict`, `validation`,
  `unprocessable`, `payload_too_large`, `timeout`, `unavailable`, `internal`.
  _Rationale_: fixed vocabulary mapped 1:1 from HTTP status.
- **CWD default**: `/` normalizes to the workspace root `/workspace` inside the sandbox.
  _Rationale_: the workspace is the only mounted path users should see.
- **Log offset semantics**: byte offsets into the log file; `next` is the offset to pass on the
  next poll; `eof` is true when the reader reached the current end of file.
  _Rationale_: deterministic and simple to resume.
