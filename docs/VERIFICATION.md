# Verification

Results for sandboxd v0.0.17 against the acceptance criteria in the spec.

Environment:

- Host: Ubuntu 24.04.4 LTS, kernel `7.1.8-zabbly+`, x86_64. The orchestrator user
  is unprivileged (`uid=2023`, `CapEff=0000000000000000`) for the non-root suites.
- nsjail: real binary at `/usr/local/bin/nsjail`
  (`sha256 9b9fb1df…c700f2`).
- slirp4netns 1.2.1, cloudflared 2026.10.7 (pinned).
- Go: `go1.23.4 linux/amd64`.
- Runtime container: `ubuntu:24.04`, started `--privileged --cgroupns=host`
  (used for the container half of the transcript).

The transcript below was produced against **real nsjail** (not the test stub)
inside a `docker run --privileged --cgroupns=host` container. `--cgroupns=host`
is required for cgroup v2 ceilings; `scripts/acceptance.sh` refuses to run unless
`NSJAIL` points at the real binary (it rejects the stub script) and `/readyz`
reports `"cgroups":true`.

> History: an earlier revision of this document presented a non-asserting
> transcript as evidence while executions were in fact failing. `acceptance.sh`
> is now a real test (it exits non-zero on any mismatch) and the results below
> are copied from a run where every assertion passed.

> Reproducibility note (current host). This document was assembled across two
> environments. The container transcript (the `acceptance.sh` run and the
> end-to-end package install) was produced in `ubuntu:24.04` via
> `docker run --privileged --cgroupns=host`. On the current host — Ubuntu 24.04.4,
> kernel `7.1.8-zabbly+` — the orchestrator is still unprivileged
> (`uid=2023`, `CapEff=0000000000000000`) and `docker` is **not** installed, so
> that container-only transcript could not be re-run here and is marked
> **[requires privileged container]**. What is different now: the **real nsjail
> binary is installed** at `/usr/local/bin/nsjail`, and the nsjail-dependent
> **host** suites run under `sudo -n` and pass — they were re-run for this
> revision:
>
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" HOME=/root go test -tags integration -count=1 ./internal/executor/` → `ok 4.664s`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/process-stop-check.sh` → `25 passed / 0 failed`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/session-nsjail-check.sh` → `18 passed / 0 failed`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/tunnel-check.sh` → `27 passed / 0 failed / 0 skipped`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/identity-check.sh` → `31 passed / 0 failed / 0 skipped`
>
> The unprivileged suites (`gofmt`, `go build`, `go vet`, `go test ./...`, the
> handler-level assertions and the sandbox-config render tests) were re-run and
> their output is copied below. The container-only parts (the `acceptance.sh`
> transcript and the end-to-end package install) still require a privileged
> container because `docker` is absent here; they are the only remaining
> **[requires privileged container]** items.

## 1. Build and tests

```
$ gofmt -l .              # empty
$ go build ./...          # exit 0
$ go vet ./...            # exit 0
$ go test ./... -race     # exit 0
ok  	sandboxd/internal/api
ok  	sandboxd/internal/auth
ok  	sandboxd/internal/config
ok  	sandboxd/internal/executor
ok  	sandboxd/internal/model
ok  	sandboxd/internal/service
ok  	sandboxd/internal/storage
ok  	sandboxd/internal/workspace
```

The `integration`-tagged tests invoke real nsjail. They were re-run **directly on
this host** (real nsjail at `/usr/local/bin/nsjail`, root, cgroup v2 writable) —
the host is the privileged environment, so no container is needed:

```
$ sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" HOME=/root \
    go test -tags integration -count=1 ./internal/executor/
ok  	sandboxd/internal/executor	4.664s
```

The four `integration` tests are `TestIntegrationPythonHello`,
`TestIntegrationTimeout`, `TestIntegrationSessionRealPTY` and the new
`TestIntegrationProcessInventoryAndCgroupKill` (native process control; see §9f).
The historical container transcript is retained for the first three:

```
$ docker run --rm --privileged --cgroupns=host \
    -v /usr:/usr:ro -v /lib:/lib:ro -v /lib64:/lib64:ro -v /bin:/bin:ro \
    -v /sbin:/sbin:ro -v /usr/local/bin/nsjail:/usr/local/bin/nsjail:ro \
    -v <repo>:/src:ro -v executor.test:/executor.test:ro \
    -w /src/internal/executor ubuntu:24.04 bash -c \
    'groupadd -g 971 sandbox; useradd -u 993 -g 971 -M sandbox; \
     /executor.test -test.v -test.run TestIntegration'
=== RUN   TestIntegrationPythonHello
--- PASS: TestIntegrationPythonHello (0.04s)
=== RUN   TestIntegrationTimeout
--- PASS: TestIntegrationTimeout (2.00s)
=== RUN   TestIntegrationSessionRealPTY
--- PASS: TestIntegrationSessionRealPTY (0.13s)
PASS
```

`golangci-lint` is not installed on this host, so `go vet ./...` is the lint
gate (both the Makefile and this document use `go vet`).

Test functions: **319** (315 unit + 4 `integration`-tagged), including regression
tests for the findings below (`TestRenderConfigRlimitUnits`,
`TestFormatEnvAllowlist`, `TestSandboxCwd`, `TestClassifyLaunchFailure`,
`TestRecoverRuns`, `TestCreateRunFileValidatesPath`,
`TestRenderConfigMountsResolverWhenNetAllowed`,
`TestRenderConfigOmitsResolverWhenNetDenied`), for the workspace control plane
(`TestPatchWorkspace*`, `TestSuspend*`, `TestStopWorkspace*`,
`TestListRunsFilterAndPage`, `TestPurgeRun*`, `TestUsageReportsDiskAndCounters`,
`TestNewEndpointsEnforceOwnership`), for the two-tier packages
(`TestInstallPackages*`, `TestListPrivatePackages`, `TestRemovePackage*`,
`TestPrivateTierIsolatedBetweenWorkspaces`, `TestPackagesCrossOwnerForbidden`),
for terminal sessions (`TestPTYHelper*`, `TestSession*`,
`TestRenderSessionConfigMountsHostDevpts`, `TestRenderConfigOmitsDevptsForOneShotExec`,
`TestPTYFailureDetectedFromHelperStderr`; see §9b) and for first-class file
browsing/viewing (`TestListFileNotDirectory`, `TestPutOverDirectoryRejected`,
`TestPutRequiresContent`, `TestReadDirectoryRejected`,
`TestListSortedWithMetadataAndPaging`, `TestStatEndpoint`,
`TestRangeReadHonestHash`, `TestRangeReadBadParams`,
`TestRangeReadLimitAboveCapRejected`, `TestLineRangeRead`,
`TestBinaryDetectionAndEncoding`, `TestDownloadFileExemptFromReadCap`,
`TestStatSymlink`, `TestReadRangeHonestHash`, `TestReadLines`; see §9c), for
native file execution / unbounded runs / workspace stop
(`TestStartRunFileInterpreterByExtension`,
`TestStartRunFileUnsupportedExtensionRejected`,
`TestStartRunExplicitInterpreterOverride`, `TestStartRunCwdValidation`,
`TestStartRunArgvAndFileConflict`, `TestStartRunUnlimited*`,
`TestRunFileUsesCwdAndInterpreter`, `TestRunFileExtensionSelection`,
`TestRunCwdMissingIs422Not503`, `TestRunNoTimeoutDeniedByDefault`,
`TestRunNoTimeoutAllowedRecordsUnlimited`, `TestFileStatAndListingRunnable`,
`TestStopWorkspaceStopsRunsAndSessions`; see §9d–§9e), and for egress and quick
tunnels (`TestAllocatorReserveRelease`, `TestMatchTunnelURL`,
`TestIsQuickTunnelArgs`, `TestShortIDStable`, `TestShellQuote`,
`TestCreateTunnelHappyPath`, `TestTunnelsDisabledByServer`,
`TestTunnelsDisabledByWorkspace`, `TestTunnelsNeedEgress`,
`TestTunnelsIsolationBetweenPrincipals`, `TestTunnelsUnknownIDs`,
`TestTunnelsBadBody`, `TestTunnelsMaxPerWorkspace`, `TestTunnelNoURLIs504`,
`TestDeleteTunnelStopsProcessAndRecord`, `TestStopSuspendDeleteReleaseTunnels`,
`TestPatchAllowTunnelsRejectedWhenServerDisabled`,
`TestCreateWorkspaceAllowTunnelsRejectedWhenServerDisabled`,
`TestMetaReportsTunnelsCapability`; see §9g).

The count is the number of `^func Test` declarations across every `*_test.go`
file (the same measure `scripts/docs-consistency.sh` asserts against):

```
$ find . -name '*_test.go' -exec grep -hE '^func Test' {} + | wc -l
316
```

## 2. Probes

- `GET /healthz` → 200 `{"status":"ok"}` always while the process is alive.
- `GET /readyz` → 200 only when the data directory is writable, nsjail is found,
  **and** cgroup v2 is usable (`--cgroupns=host`):
  503 `{"status":"not_ready","nsjail":true,"cgroups":false,"data_dir":true}`
  otherwise. Verified by `TestReadyzReportsNotReadyWithoutNsjail` and
  `TestReadyzReportsNotReadyWithoutCgroups`.

## 3. Full lifecycle transcript (asserting)

Reproduce against a real-nsjail server (`make acceptance` builds the official
image and runs this):

```sh
BASE=http://127.0.0.1:8099 KEY=dev-key-please-rotate OTHER_KEY=other-key \
  DATA_DIR=/data NSJAIL=/usr/local/bin/nsjail bash scripts/acceptance.sh
```

```
### 0. environment
base=http://127.0.0.1:8099 data_dir=/data
ok   readyz.status                ready
ok   readyz.nsjail                True
### 1. healthz
ok   healthz.status               ok
### 2. meta
ok   meta.version                 0.0.17
### 3. create workspace
workspace id: ws_01M6BNEZGVE2AFRADVKB01FFC2
### 4. mkdir /src
ok   mkdir.src                    201
### 5. PUT /main.py
ok   put.main.py                  204
### 6. upload /uploaded.py (multipart)
ok   upload.status                201
### 7. read /main.py (meta)
ok   read.main.py                 0ca9091eb4e31fb1ab24c8c5de92a08e4e5f402919f82ea3ca784f38534f03f3
### 7b. first-class file browsing and viewing
ok   list.file_422                422
ok   list.order                   zdir,a.txt,b.txt
ok   list.first_type              dir
ok   list.total                   3
ok   list.count_eq_total          3
ok   list.not_truncated           False
ok   list.metadata                True
ok   list.page_count              1
ok   list.page_truncated          True
ok   stat.file_type               file
ok   stat.file_sha                0ca9091eb4e31fb1ab24c8c5de92a08e4e5f402919f82ea3ca784f38534f03f3
ok   stat.file_size               12
ok   stat.missing_404             404
ok   put.over_dir_422             422
ok   put.no_content_4xx           400
ok   put.no_content_no_file       404
ok   read.dir_422                 422
ok   range.content                print
ok   range.length                 5
ok   range.offset                 0
ok   range.total_size             12
ok   range.whole_file             False
ok   range.truncated              True
ok   range.partial_hash           ce953a0eb08246617b7f849486c4b26a7af37e9d2e8f0e13b3ae1bf0da8a70a2
ok   range.partial_ne_full        DIFF
ok   range.full_eq_file           0ca9091eb4e31fb1ab24c8c5de92a08e4e5f402919f82ea3ca784f38534f03f3
ok   range.full_whole_file        True
ok   binary.flag                  True
ok   binary.encoding              base64
ok   lines.content                twothree
ok   lines.start_line             2
ok   lines.end_line               3
ok   lines.line_count             2
ok   lines.total_lines            4
ok   download.big_upload          201
ok   download.big_code            200
ok   download.big_len             6291456
ok   download.content_length      6291456
ok   download.dir_422             422
ok   download.stat_size           6291456
### 8. exec (sync) -- a successful execution
ok   exec.exit_code               0
ok   exec.stdout                  hi
### 9. cwd is honored
ok   exec.cwd                     /workspace/src
### 10. start run (file)
run id: run_01M6BNF0Y860R5XE18QNW4ESMC
ok   run.status                   exited
ok   run.logs.stdout              hi
### 10b. runs: cwd, interpreter selection, runnable metadata
ok   run.cwd_recorded             /rundir
ok   run.cwd_status               exited
ok   run.cwd_is_realdir           /workspace/rundir
ok   run.cwd_missing_422          422
ok   exec.cwd_missing_422         422
ok   run.py_backcompat_interpreter python3
ok   run.py_backcompat_status     exited
ok   stat.runnable_py             python3
ok   stat.runnable_txt_none       None
ok   run.sh_by_extension          bash
ok   run.sh_by_extension_status   exited
ok   run.sh_by_extension_out      ext-sh
ok   run.interpreter_override     bash
ok   run.interpreter_override_status exited
ok   run.interpreter_override_out sh-ok
ok   run.unsupported_ext_code     unprocessable
ok   run.unsupported_ext_names_set contains .py
ok   run.argv_file_conflict_422   422
### 11. security: traversal rejected (expect 422)
ok   traversal.read               422
ok   traversal.write              422
ok   traversal.delete             422
### 12. security: auth and ownership
ok   auth.missing                 401
ok   auth.cross_owner             403
### 13. security: oversized JSON body (expect 413)
ok   body.too_large               413
### 14. security: symlink escape (expect 422)
ok   symlink.escape               422
### 15. isolation: network egress blocked
ok   net.blocked                  contains NET_BLOCKED
ok   net.exit_code                0
### 16. isolation: /etc/shadow not reachable
ok   shadow.denied                SHADOW_DENIED: FileNotFoundError
### 17. isolation: mount(2)/ptrace killed by seccomp (SIGSYS=159)
ok   seccomp.mount                159
### 18. isolation: fork bomb limited by cgroup pids.max
ok   cgroup.fork_limit            contains FORK_LIMITED
### 19. resource ceiling: memory limit enforced (F-1)
ok   rlimit.memory                contains MEMORY_DENIED
### 20. stop long run kills process group
ok   stop.run_status              killed
ok   stop.no_survivors            0
### 20b. unbounded runs: opt-in, honest record, real survival, zero survivors
ok   unlimited.denied_code        unprocessable
ok   unlimited.denied_config      limits.allow_unlimited_runs
ok   unlimited.recorded_flag      True
ok   unlimited.recorded_timeout_null None
ok   finite.recorded_timeout      2
ok   finite.times_out             timeout
ok   unlimited.survives_past_ceiling running
ok   unlimited.stop_status        killed
ok   unlimited.stop_no_survivors  0
### 21. terminal session: a real PTY inside the sandbox
ok   session.start_status         running
ok   session.tty_device           contains /dev/pts/
ok   session.is_a_tty             contains TTY_OK
ok   session.no_pty_error         no PTY error / traceback
ok   session.offset_suffix        contains $'\r\n$ '
ok   session.delete               200
### 21b. workspace stop stops runs AND sessions, leaving zero processes
ok   stop.runs_count              1
ok   stop.sessions_count          1
ok   stop.run_id_reported         run_01M6BNHBYHPKD8MTFK2KB1SE4E
ok   stop.session_id_reported     ses_01M6BNHBZHGRKCPVC739DKRMTF
ok   stop.run_status              killed
ok   stop.session_status          killed
ok   stop.session_reason          workspace_stopped
ok   stop.zero_survivors          0
ok   stop.workspace_active        active
ok   stop.delete_ws               204
### 22. delete workspace (expect 204) and get after delete (expect 404)
ok   delete.workspace             204
ok   get.after_delete             404

passed=113 failed=0
```

Every step asserts on content, not merely on the absence of a crash: a
successful execution must print `hi` with exit code 0, the network probe must
report `NET_BLOCKED`, the seccomp probe must exit `159`, the fork bomb must
report `FORK_LIMITED`, and the memory probe must report `MEMORY_DENIED`.

### Resource-ceiling evidence

Raw introspection through the API with declared `memory_mb=512`,
`disk_mb=1024`, `max_runs=3` (`cgroup_pids_max = 3 * 64 = 192`):

```
$ curl … -d '{"cmd":["python3","lim.py"]}' …/exec     # lim.py prints getrlimit
AS_LIMIT_MB 512                                        # was 512 TiB before the unit fix

$ curl … -d '{"cmd":["python3","f.py"]}' …/exec       # f.py attempts 500 live forks
FORK_LIMITED BlockingIOError
FORKED 191                                             # stopped at the cgroup pids ceiling
```

## 4. Security rejections

All verified by `TestPathTraversalRejections`, `TestSymlinkEscapeRejected`,
`TestCrossOwnerFileAccessForbidden`, `TestOversizedJSONBody`, and the transcript
above:

| case | result |
|------|--------|
| `path=../etc/passwd` on read | 422 `unprocessable` |
| `path=../evil` on write | 422 `unprocessable` |
| `path=../etc/passwd` on delete | 422 `unprocessable` |
| symlink inside workspace → `/etc/passwd` | 422 `unprocessable` |
| missing/bad token | 401 `unauthorized` |
| cross-owner access | 403 `forbidden` |
| oversized (>1 MB) JSON body | 413 `payload_too_large` |
| `kind=file` with `../../etc/passwd` | 422 `unprocessable` |
| `kind=file` with a missing file | 404 `not_found` |
| `cwd` escaping the workspace | 422 `unprocessable` |
| user env var not on the allowlist (`LD_PRELOAD`, …) | dropped silently |
| sandbox launch failure (no `--cgroupns=host`) | 503 `unavailable` + nsjail diagnostic |

## 5. Process-group kill

`exec.Cmd` starts with `SysProcAttr{Setpgid: true}`. `stop` and timeout send
`SIGTERM` to `-pgid`, wait the configured grace (3 s), then `SIGKILL` to
`-pgid`. `TestKillProcessGroupReal` starts a real `sleep` in its own group,
kills it, and asserts a signalled exit. The transcript (step 20) shows the run
reaches `killed` and no `long.py` process survives stop.

Reproduce the process check manually (inside the nsjail container the process
runs in):

```sh
docker exec <container> pgrep -af 'long.py|nsjail'
```

## 6. Sandbox launch failure is explicit

Without `--cgroupns=host`, cgroup v2 is not usable and nsjail aborts before
`execve`. This is surfaced explicitly rather than as a silent exit 255:

```
$ curl -s -o body -w 'HTTP %{http_code}\n' -d '{"cmd":["python3","-c","print(1)"]}' …/exec
HTTP 503
{"error":{"code":"unavailable","message":"sandbox unavailable: nsjail could not set up or launch the sandbox",
 "details":{"nsjail":"[E]… main():380 Couldn't setup parent cgroup (cgroupv2)"}}}
```

For async runs the run is recorded `failed` and the nsjail diagnostic is written
to the run's stderr log.

## 7. Workspace control plane (acceptance)

The control plane added in `7e1b256` (`PATCH`, lifecycle, run list/purge,
usage) is a handler-level test, so it does **not** need nsjail: it runs under
`go test ./...` with the `FakeRunner`. The acceptance is an asserting transcript
in the form of the API-suite tests, each asserting the exact status/code per
step. Observed (all pass):

```
$ go test ./internal/api/ -run 'TestPatchWorkspace|TestSuspend|TestStopWorkspace|TestListRunsFilterAndPage|TestPurge|TestUsageReports|TestNewEndpoints' -v
--- PASS: TestPatchWorkspaceSingleField
--- PASS: TestPatchWorkspaceAboveMaximumRejected      # 422 unprocessable
--- PASS: TestPatchWorkspaceNonPositiveRejected        # 422
--- PASS: TestPatchWorkspacePersistsAcrossRestart
--- PASS: TestSuspendRefusesWorkAndResumeRestores      # exec/run 409 workspace_suspended; reads 200
--- PASS: TestSuspendStopsRunningRuns
--- PASS: TestSuspensionSurvivesRestart                # still 409 after restart
--- PASS: TestStopWorkspaceKillsRuns                   # status stays active
--- PASS: TestStopWorkspaceNoRunsIsNoop                # stopped_run_count 0
--- PASS: TestListRunsFilterAndPage                    # total=3 filter, page total=2, unknown status 400
--- PASS: TestPurgeRunsRefusesWhileRunning             # 409 conflict, then deleted=2
--- PASS: TestPurgeSingleRun                           # 204 finished, 409 running, 404 missing
--- PASS: TestUsageReportsDiskAndCounters              # disk_bytes=11, complete=true, counters
--- PASS: TestNewEndpointsEnforceOwnership             # 403 on each new endpoint
```

Per-step expected results asserted (from `internal/api/control_test.go`):

| Step | Assertion |
|------|-----------|
| `PATCH limits.memory_mb=256` | 200, field applies, untouched limits keep their values |
| `PATCH limits.memory_mb=999999` | 422 `unprocessable`; the stored value is unchanged (512) |
| `PATCH limits.disk_mb=0` | 422 |
| `PATCH` then a fresh service over the same data dir | patched value survives (disk_mb 4096) |
| `suspend` | 200, `status:suspended`, `changed:true` |
| `exec` / `runs` while suspended | 409 `workspace_suspended` |
| file read / file list / run list while suspended | 200 (reads keep working) |
| second `suspend` | `changed:false` (idempotent) |
| `resume` then `exec` | 200 |
| suspended + restart | still `suspended`; `runs` still 409 |
| `GET runs?status=exited,killed` | `total:3`; repeatable form same; `kind=file` `total:1` |
| `GET runs?status=exited&limit=1&offset=0/1` | same `total:2`, distinct single run per page |
| `GET runs?status=bogus` | 400 `validation` |
| `DELETE runs` with a live run | 409 `conflict`, nothing deleted |
| `DELETE runs` after it finishes | 200 `{"deleted":2}`, list empty |
| `DELETE runs/{finished}` | 204; `DELETE runs/{running}` 409; unknown id 404 |
| `GET usage` | `disk_bytes:11`, `disk_bytes_complete:true`, `runs_total:3`, `active_runs:0` |
| each new endpoint as a foreign principal | 403 `forbidden` |

A cross-principal request returns **403 `forbidden`** on every new endpoint:
`PATCH`, `suspend`, `resume`, `stop`, `usage`, `DELETE .../runs` and
`DELETE .../runs/{id}` (the package endpoints are covered in §9).

## 8. Resolver fix (DNS inside the jail)

**Symptom.** For a workspace with `allow_net:true`, the network namespace is
shared with the host (TCP to a raw IP worked), yet any hostname lookup failed
inside the jail with `Temporary failure in name resolution`. `pip install`
against an index therefore failed even though the network was reachable.

**Root cause.** The rendered nsjail config binds the runtime and a handful of
`/etc` files, but **not** `/etc/resolv.conf` (or `/etc/hosts`). With no resolver
configuration inside the mount namespace, libc's resolver had no nameserver to
query, so `getaddrinfo` failed regardless of connectivity.

**Fix (`044d452`).** `configs/nsjail_python.cfg.tmpl` now bind-mounts
`/etc/resolv.conf` and `/etc/hosts` **read-only**, but **only when `AllowNet` is
true** (`{{ if .AllowNet }}`). A workspace without `allow_net` must not see the
host resolver at all, so that namespace does not leak the host's search domain
or nameservers.

**Post-fix evidence.** The fix is a config-render change, so it is verified by
the render tests (they run unprivileged, no nsjail needed) — both directions:

```
$ go test ./internal/executor/ -run 'TestRenderConfig.*Resolver' -v
=== RUN   TestRenderConfigMountsResolverWhenNetAllowed
--- PASS: TestRenderConfigMountsResolverWhenNetAllowed (0.00s)
=== RUN   TestRenderConfigOmitsResolverWhenNetDenied
--- PASS: TestRenderConfigOmitsResolverWhenNetDenied (0.00s)
```

`TestRenderConfigMountsResolverWhenNetAllowed` asserts the rendered config
contains, for both files, `is_bind: true` / `rw: false`, and keeps
`clone_newnet: false`. `TestRenderConfigOmitsResolverWhenNetDenied` asserts the
opposite: no `/etc/resolv.conf` mount and `clone_newnet: true`.

**[requires privileged container] End-to-end name resolution.** The following
was **not** re-run in the audit environment (no nsjail, unprivileged); it is the
expected real-nsjail confirmation of the same behaviour:

```
$ curl … -d '{"cmd":["python3","-c","import socket;print(socket.gethostbyname(\"pypi.org\"))"]}' …/exec
# {"exit_code":0,"stdout":"151.101.x.x\n", ...}   # resolves after the fix
# before the fix: exit_code 1, stderr "Temporary failure in name resolution"
```

## 9. Two-tier Python packages (acceptance)

The package feature (`54fc792`) has a handler-level suite (no nsjail required)
plus the sandbox mount-config tests. Observed (all pass):

```
$ go test ./internal/api/ ./internal/service/ ./internal/executor/ -run 'Package|Packages' -v
--- PASS: TestListPackagesEmpty
--- PASS: TestListPackagesReportsSharedTier          # shared_path /opt/python/shared
--- PASS: TestListPackagesMissingSharedUnavailable   # enabled but available=false
--- PASS: TestInstallPackagesHostileRejected         # each hostile req -> 422, runner never invoked
--- PASS: TestInstallPackagesEmptyListRejected       # 422
--- PASS: TestInstallPackagesSuccess                 # --target /opt/python/private
--- PASS: TestInstallPackagesSuspended               # 409 workspace_suspended
--- PASS: TestListPackagesSuspended                  # 409 (package listing is refused too)
--- PASS: TestDeleteAllPackagesSuspended             # 409
--- PASS: TestDeletePackagesRequiresNameOrFlag       # bare DELETE -> 400 validation
--- PASS: TestDeletePackagesNameAndAllConflict       # ?name&?all -> 400
--- PASS: TestDeleteAllPackages                      # removed=2, all=true
--- PASS: TestDeletePackage                          # 204 then 404
--- PASS: TestPackagesCrossOwnerForbidden            # 403 on all four package endpoints
--- PASS: TestPackageIsolationAcrossWorkspaces       # A's private pkg absent from B
--- PASS: TestListPrivatePackages                    # sorted, metadata-backed
--- PASS: TestPrivateTierIsolatedBetweenWorkspaces   # distinct host dirs, no shared location
--- PASS: TestRemovePackage / TestRemovePackageNormalizedName / TestRemoveAllPackages
--- PASS: TestExecWiresPackageDirs / TestStartRunWiresPackageDirs
```

What the assertions establish:

| Property | Evidence |
|----------|----------|
| Shared tier is importable and **read-only** | merge test `TestPackageTierSharedAvailableWhenDirPresent` resolves `SharedPackagesMount`; the mount renders `rw:false` (see the render tests below) |
| A write to the shared tier is refused | the shared mount is `rw:false`, verified by `TestRenderConfigMounts*` (no `rw:true` shared mount) — **[requires privileged container]** an actual `open(...,O_WRONLY)` in a live jail would `EROFS` |
| Workspace A's package is not importable in B | `TestPackageIsolationAcrossWorkspaces`, `TestPrivateTierIsolatedBetweenWorkspaces` |
| The shared tier is visible to both | shared mount is unconditional per workspace; `TestExecWiresPackageDirs`/`TestStartRunWiresPackageDirs` |
| Hostile requirement strings are rejected with 422 | `TestInstallPackagesHostileRejected` (12 inputs: `-e`, `--upgrade`, `pkg; rm -rf /`, `../../etc/passwd`, backticks, `$( )`, 10 000 chars, …) and the runner is never invoked |
| Install targets the private tier | `TestInstallPackagesSuccess` asserts `--target /opt/python/private` in argv |

**[requires privileged container] End-to-end import / cross-workspace proof.**
`allow_net=true`, a real index fetch and an actual sandbox `import` were **not**
re-run in the audit environment. The recorded expected transcript:

```sh
# A: install six into the private tier (needs allow_net + the resolver fix)
curl … -d '{"packages":["six==1.16.0"]}' …/packages/install
# {"exit_code":0,"output":"… Successfully installed six-1.16.0 …","target":"/opt/python/private"}
curl … -d '{"cmd":["python3","-c","import six; print(six.__version__)"]}' …/exec
# {"exit_code":0,"stdout":"1.16.0\n"}
# B: the same import must fail (isolation)
curl …/workspaces/$B/exec -d '{"cmd":["python3","-c","import six"]}'
# {"exit_code":1,"stderr":"ModuleNotFoundError: No module named 'six'\n"}
# shared import works in both A and B with no install step (when shared_dir holds it)
```

## 9b. Terminal sessions (acceptance)

Server-side interactive terminal sessions have four layers of evidence, the last
of which is the **measured real-nsjail before/after** for the devpts fix:

1. **A real PTY** — the relay helper is exercised directly as a subprocess
   (`internal/executor/pty_test.go`), no nsjail required, proving `pty.fork()`
   gives a real `/dev/pts/N`.
2. **The sandbox changes** — the rendered nsjail config is asserted to mount the
   helper read-only, to bind `/dev/pts` and `/dev/ptmx` **for a PTY session
   only**, and to keep the full workload isolation (also no nsjail required).
3. **The HTTP contract** — handler-level tests over the `FakeRunner`
   (`internal/api/sessions_test.go`) assert every status/code, including the
   timeouts **and the loud PTY failure** (`TestSessionPTYFailureIsLoud`).
4. **A real session inside a real nsjail** (`TestIntegrationSessionRealPTY`,
   `//go:build integration`, [requires privileged container]) — the measured
   transcript below.
5. **The privileged acceptance harness** (`scripts/session-nsjail-check.sh`) —
   the end-to-end real-nsjail run of the whole session matrix, including the
   forced PTY-allocation failure and the cross-principal / ended-session
   contracts (see §9b.4). It is the check that catches a dead PTY where the unit
   tests stay green.

### 9b.1 The measured devpts failure and fix

`nsjail` has no PTY support; the relay helper allocates the terminal with
`pty.fork()`, which opens **`/dev/ptmx`** and needs a working **devpts**. The
one-shot `/dev` (null/urandom/zero only) has neither, so the helper dies. The
measured sequence, run on this host by hand inside `nsjail` with the *exact*
per-run mounts, and then re-confirmed by the integration test:

```
# Failure: the relay under the unpatched run config (no devpts mount).
$ nsjail --config <one-shot-style>.cfg   # exec_bin = python3 /run/sandboxd/pty_relay.py
Traceback (most recent call last):
  File "/run/sandboxd/pty_relay.py", line 190, in main
    child, master = pty.fork()
  File "/usr/lib/python3.12/pty.py", line 107, in fork
    master_fd, slave_fd = openpty()
  ...
OSError: out of pty devices

# A private devpts instance was measured NOT to work: it creates
# /dev/pts/ptmx but not /dev/ptmx, which openpty() opens.
$ grep -c "newinstance" privdevpts.cfg && nsjail --config <privdevpts>.cfg
PTY_FORK_FAIL: [Errno 2] No such file or directory: '/dev/ptmx'
# /dev listing with a private devpts:  ['null', 'pts', 'urandom', 'zero']  (no /dev/ptmx)

# The fix: bind the host's /dev/pts AND /dev/ptmx (read-only, PTY sessions only).
# /dev listing after the fix: ['null', 'ptmx', 'pts', 'urandom', 'zero']
$ nsjail --config <host-devpts>.cfg        # exec_bin = pty_relay.py '{"shell":"/bin/sh",...}'
$ echo MARKER_42
tty
test -t 1 && echo TTY_OK
MARKER_42
$ /dev/pts/3
$ TTY_OK
```

Exact mounts that make it work (rendered only when `Request.PTY` is set):

```
mount { src: "/dev/pts"  dst: "/dev/pts"  is_bind: true rw: false mandatory: false }
mount { src: "/dev/ptmx" dst: "/dev/ptmx" is_bind: true rw: false mandatory: false }
```

Both are read-only binds of device nodes: a PTY is a **terminal** (a byte stream
plus a window size), not a filesystem or process view. uid/gid, seccomp,
`clone_newnet`/`newns`/`newpid`/… and `mount_proc: false` are unchanged; the
sandbox is not weakened. See [DECISIONS.md](DECISIONS.md) #52–53 and
[ARCHITECTURE.md](ARCHITECTURE.md) §5.2.

### 9b.2 The measured real-nsjail session transcript

`TestIntegrationSessionRealPTY` runs a session through the real `Start` path
under root with the compiled nsjail and asserts the session output. Observed on
this host (nsjail built from source, `--privileged`, root):

```
$ go test -tags integration -run TestIntegrationSessionRealPTY -v ./internal/executor/
=== RUN   TestIntegrationSessionRealPTY
--- PASS: TestIntegrationSessionRealPTY (0.13s)
PASS
ok  	sandboxd/internal/executor	0.131s
```

The test asserts, on the persisted session output:

- `tty` printed a device matching `/dev/pts/` (a real slave terminal);
- `test -t 1 && echo TTY_OK` printed `TTY_OK` (stdout **is** a tty);
- the output contains **neither** `out of pty devices` **nor** `Traceback`;
- `cat marker.txt` output arrived and is readable through the byte-offset reader
  from 0 and from a late offset (the same `offset`/`next` mechanism
  `GET .../sessions/{id}/output` uses).

The full unit suite (`go test ./...`) and `go test -race ./...` remain green.

### 9b.3 The loud-failure contract

A relay that cannot allocate a PTY no longer leaves a `running` session with a
traceback. The helper writes `sandboxd-pty-error: …` to stderr and exits
`69` (EX_UNAVAILABLE):

```
# patched helper, unpatched (no devpts) config:
$ nsjail --config <one-shot-style>.cfg
sandboxd-pty-error: cannot allocate a PTY: out of pty devices
the sandbox is missing a working /dev/ptmx and devpts
exit=69
```

The executor turns that marker into a `SandboxError`, and the service records
the session as `failed` / `sandbox_unavailable` with the actionable
`diagnostic`; `StartSession` waits the short `sessions.start_probe_ms` window
(250 ms default, 2 s cap) so even the create response is honest. Verified
unprivileged by `TestSessionPTYFailureIsLoud` (API over the `FakeRunner`) and
`TestPTYFailureDetectedFromHelperStderr` (executor classification), and asserted
against the embedded helper by `TestPTYRelayScriptHandlesAllocationFailure`.

The full matrix that the spec required:

| # | Scenario | Test (all pass) |
|---|----------|-----------------|
| 1 | session starts; a real `tty` is reported (`test -t 1`) | `TestPTYHelperRealTTY` (real PTY) + `TestIntegrationSessionRealPTY` (real nsjail) + `TestSessionCreateAndInspect` (API, asserts the PTY launch path) |
| 2 | state persists: `cd /tmp` then `pwd` → `/tmp` | `TestPTYHelperRealTTY` (real PTY) |
| 3 | `alias` and a function persist to the next command | `TestPTYHelperStatePersists` (real PTY); also asserts **no framing bytes** leak into the stream |
| 4 | `signal INT` interrupts `sleep 30`, session survives and runs again | `TestPTYHelperSignalInterruptsForeground` (real PTY) + `TestSessionSignalRelayed` (API, status stays `running`) |
| 5 | `resize` reaches the PTY (`stty size`) | `TestPTYHelperRealTTY` (real PTY: `50 120`) + `TestSessionResizeRelayed` (API) |
| 6 | `DELETE` ends, is idempotent, then input/output report the ended-session error | `TestSessionDeleteEndsAndIsIdempotent` |
| 7 | suspended workspace refuses create and input | `TestSessionSuspendedWorkspace` |
| 8 | session cap enforced with the documented code/default | `TestSessionCap` |
| 9 | hostile input `{"type":"close"}` delivered as literal text | `TestSessionHostileInput` + `TestPTYHelperInjectionProof` |
| 10 | foreign principal gets 403 on every session route | `TestSessionOwnership` |
| 11 | idle timeout and absolute lifetime fire with documented status/reason | `TestSessionIdleTimeout`, `TestSessionLifetime`, `TestSessionLifetimeRequestedShorter` |
| 12 | no leaked process after `DELETE` | `TestSessionNoLeakAfterDelete` (live-process interface; real process tree is nsjail's) |
| 13 | a PTY failure is a `failed` session with a diagnostic, never `running` | `TestSessionPTYFailureIsLoud`, `TestPTYFailureDetectedFromHelperStderr`, `TestPTYRelayScriptHandlesAllocationFailure` |
| 14 | a PTY session renders the host `/dev/pts`+`/dev/ptmx` mounts; one-shot does not | `TestRenderSessionConfigMountsHostDevpts`, `TestRenderConfigOmitsDevptsForOneShotExec` |
| 15 | `gofmt`/`vet`/`test`/`test -race` green | the build/test transcript above |

Plus: `TestRenderSessionConfigHelperMount` (helper mounted `rw:false` at
`/run/sandboxd`, seccomp/uidmap/`mount_proc:false` retained),
`TestRenderSessionConfigNoHelperMount` (non-session config untouched),
`TestStartLiveProcessRoundTrip` and `TestStartKillProcessGroup` (the real
`Start`/`LiveProcess` plumbing through a config-parsing nsjail stub),
`TestStartUnavailable`, `TestSessionRecoveryMarksInterrupted`,
`TestSessionInputTooLarge`, `TestSessionCwdSafety`,
`TestSessionRejectsAbsurdSize`, `TestSessionDefaultSize`,
`TestSessionListFilterAndPage`, `TestSessionInputTooLarge`,
`TestPTYRelayScriptIsStdlibOnly`.

Observed (unprivileged host; real helper subprocess, fake sandbox for HTTP):

```
$ go test ./internal/executor/ -run 'PTY|RenderSession|RenderConfigOmitsDevpts' -v
--- PASS: TestPTYRelayScriptIsStdlibOnly
--- PASS: TestPTYHelperRealTTY                    # test -t 1 → TTY_OK; cd /tmp; pwd → /tmp; stty size → 50 120
--- PASS: TestPTYHelperStatePersists              # alias + function persist; no framing in output
--- PASS: TestPTYHelperSignalInterruptsForeground # sleep 30 interrupted; session runs again
--- PASS: TestPTYHelperInjectionProof             # {"type":"close"} arrives as literal text
--- PASS: TestPTYHelperCloseEndsSession
--- PASS: TestPTYRelayScriptHandlesAllocationFailure
--- PASS: TestPTYFailureDetectedFromHelperStderr
--- PASS: TestRenderSessionConfigMountsHostDevpts    # host /dev/pts + /dev/ptmx RO, PTY only
--- PASS: TestRenderConfigOmitsDevptsForOneShotExec  # one-shot /dev stays null/zero/urandom
--- PASS: TestRenderSessionConfigHelperMount      # /run/sandboxd mounted rw:false; seccomp+uidmap kept
--- PASS: TestRenderSessionConfigNoHelperMount

$ go test ./internal/api/ -run 'Session' -v
--- PASS: TestSessionPTYFailureIsLoud              # failed + diagnostic, never running
--- PASS: TestSessionCreateAndInspect
--- PASS: TestSessionDefaultSize
--- PASS: TestSessionRejectsAbsurdSize
--- PASS: TestSessionCwdSafety
--- PASS: TestSessionInputAndOutput
--- PASS: TestSessionSignalRelayed
--- PASS: TestSessionResizeRelayed
--- PASS: TestSessionDeleteEndsAndIsIdempotent
--- PASS: TestSessionDeleteUnknownIs404
--- PASS: TestSessionSuspendedWorkspace
--- PASS: TestSessionCap
--- PASS: TestSessionHostileInput
--- PASS: TestSessionOwnership
--- PASS: TestSessionIdleTimeout
--- PASS: TestSessionOutputCountsAsActivity
--- PASS: TestSessionLifetime
--- PASS: TestSessionLifetimeRequestedShorter
--- PASS: TestSessionNoLeakAfterDelete
--- PASS: TestSessionListFilterAndPage
--- PASS: TestSessionRecoveryMarksInterrupted
--- PASS: TestSessionInputTooLarge
```

**[requires privileged container] Real-nsjail session transcript.** The session
inside a real nsjail instance was run on this host with a real jail, a real shell
on a real PTY (`/dev/pts/3`), a real `test -t 1`, and a real file read through
the offset reader (`TestIntegrationSessionRealPTY`, transcript in §9b.2). The
unprivileged suite asserts the launch path against the rendered config and the
live-process interface instead, so the feature is covered on hosts without nsjail
too.

### 9b.4 The privileged session acceptance harness

`scripts/session-nsjail-check.sh` is the purpose-built real-nsjail acceptance
harness for terminal sessions. It hosts the service itself (no container), so an
operator with a host that can run real nsjail (root, cgroup v2, the real
`nsjail` binary) can reproduce the whole session contract in one command:

```sh
sudo -E bash scripts/session-nsjail-check.sh
```

No assertion is ever weakened to make a run pass, and the script exits non-zero
on any failure. It builds the binary, starts the service against the host's real
nsjail on a spare port (`PORT`, default 18099), and asserts:

1. a **real terminal** — `tty` prints a `/dev/pts/N` device, `test -t 1`
   succeeds, and the stream carries **neither** `out of pty devices` **nor** a
   traceback;
2. **state survives** between commands (`cd /tmp` then `pwd` → `/tmp`);
3. an **interrupt does not end the session** (signal `INT` to `sleep 30`, the
   session is still `running` afterwards);
4. **resize reaches the PTY** (`stty size` reports the new `rows cols`);
5. a **foreign principal** (a second API key in the test config) is refused with
   `403`/`404` — never success — on create, input, output and delete;
6. `DELETE` ends the session cleanly;
7. `POST /sessions/{id}/input` on the ended session returns **409** with the
   documented code `session_ended`;
8. a **failed PTY allocation** surfaces as a session in `failed` state with
   reason `sandbox_unavailable` and a diagnostic carrying the PTY error — never
   a session reported `running` whose stream is a traceback.

Check 8 is forced honestly: a **second** sandboxd instance is started whose
nsjail template is a copy of `configs/nsjail_python.cfg.tmpl` with its
`{{- if .PTY }}` guard rewritten to `{{- if false }}`, so that instance
deliberately never binds `/dev/pts` and `/dev/ptmx` into a session's jail.
Inside that jail `pty.fork()` fails with `out of pty devices`, and the check
proves the service turns that into a failed session with a diagnostic. It is the
real-nsjail analogue of the unit test `TestSessionPTYFailureIsLoud` (which forces
the same condition through a fake-runner error) and asserts the real failure
path, not a mock.

Observed on this host (real `nsjail` at `/usr/local/bin/nsjail`, root):

```
$ sudo -E bash scripts/session-nsjail-check.sh
=== building ===
=== starting the service under real nsjail ===
  service ready: {"cgroups":true,"data_dir":true,"nsjail":true,"status":"ready"}
  workspace=ws_01M6B9GQFV7N5EXYNXXHVEWSTD session=ses_01M6B9GQH62FGMB6C8NAPQKPEX
=== 1) the session must provide a REAL terminal ===
  PASS  no PTY allocation failure (yes)
  PASS  tty is a pts device (yes)
  PASS  stdout is a tty (yes)
  PASS  no traceback in the stream (yes)
=== 2) state survives between commands ===
  PASS  cd then pwd (yes)
=== 3) an interrupt must not end the session ===
  PASS  session still running after SIGINT (running)
=== 4) resize reaches the PTY ===
  PASS  stty size reflects the resize (yes)
=== 5) a foreign principal is refused on every session route ===
  PASS  foreign POST /sessions refused (refused)
  PASS  foreign POST /sessions/ses_01M6B9GQH62FGMB6C8NAPQKPEX/input refused (refused)
  PASS  foreign GET /sessions/ses_01M6B9GQH62FGMB6C8NAPQKPEX/output?offset=0 refused (refused)
  PASS  foreign DELETE /sessions/ses_01M6B9GQH62FGMB6C8NAPQKPEX refused (refused)
=== 6) cleanup ===
  PASS  delete ends the session (yes)
=== 7) input on an ended session returns the documented 409 ===
  PASS  input on an ended session is 409 (409)
  PASS  ended-session error code (session_ended)
=== 8) a failed PTY allocation surfaces as a failed session ===
  PASS  forced PTY failure session status (failed)
  PASS  forced PTY failure reason (sandbox_unavailable)
  PASS  forced PTY failure carries the PTY diagnostic (yes)
  PASS  forced PTY failure is terminal on GET (failed)

================ session acceptance: 18 passed / 0 failed ================
```

The harness stops each service by its **exact PID** (never `pkill -f <pattern>`,
which can match the shell running it) and cleans up in a `trap`. On a host
without root, without nsjail, or without the Go toolchain it prints a `SKIP`
line and exits 0 — it makes no claim it cannot prove. `make session-check` runs
it; unlike `make acceptance` it needs **no container**, only root and a real
nsjail on the host.

**This is the check that catches a dead PTY where unit tests stay green.** The
feature once shipped with every unit test passing while sessions returned
`201 Created` and reported `running` with a terminal stream that was a Python
`OSError: out of pty devices` traceback. Only a session run against the **real**
nsjail can observe that the sandbox's `/dev` lacks a working devpts; the stub
runner and the rendered-config assertions cannot.

## 9c. First-class file browsing and viewing (acceptance)

The file API was made complete and honest for a file browser/viewer client.
The measured defects before the change (live server, real nsjail):

- `GET /files?path=/f.py` (a **file**) returned `200 {"entries":[]}` — silently
  empty, no error.
- `GET /files/content?path=/f.py/x` (a directory as a file) returned
  `404 file not found`.
- `PUT /files/content?path=<dir>` (**writing over a directory**) returned
  `500 internal` (the internal `errNotDirectory` was never translated).
- `PUT /files/content` with body `{}` returned `204` and wrote a **0-byte** file.
- A file larger than `caps.file_read_mb` returned a **silent truncated prefix**
  with `truncated:true`; **413 was never returned** on the read path, and
  **`sha256` was computed over the returned prefix**, not the file.
- `Service.StatFile` existed but had **no HTTP route**.
- No range/offset/limit support, no binary detection, and no way to obtain a
  file larger than `file_read_mb` at all.

The change resolves `docs/API.md` "Known discrepancies" items **6, 7, 8 and 9**:
a request `limit` above `file_read_mb` is `413`; `sha256` is over exactly the
returned bytes with `range`/`total_size`/`whole_file` exposing partial delivery;
`PUT /files/content` requires `content`; and wrong-node-type operations return
`422 not_directory` (a new code), never `500` and never `404` when the path
exists. It also adds `GET .../files/stat`, byte and line range reads, bounded
paged listings with metadata (`mode`, symlink `target`/`target_inside`,
deterministic directories-first ordering, `count`/`total`/`truncated`), explicit
binary detection (NUL / invalid UTF-8 in a bounded 8192-byte sample), and a
streaming `GET .../files/download` that is exempt from `file_read_mb`.

The asserting acceptance additions live in `scripts/acceptance.sh` §7b:
`list.file_422`, the sorted `list.order`/`list.first_type`/`list.total` plus
metadata presence, `list.page_count`/`list.page_truncated`,
`stat.file_type`/`stat.file_sha`/`stat.file_size`/`stat.missing_404`,
`put.over_dir_422`, `put.no_content_4xx`/`put.no_content_no_file`,
`read.dir_422`, the range checks (`range.content`, `range.length`,
`range.offset`, `range.total_size`, `range.whole_file`, `range.truncated`), the
integrity pair `range.partial_hash` and **`range.partial_ne_full`** (a partial
read's hash ≠ the full file's `sha256sum`) plus **`range.full_eq_file`** (a full
read's hash = the file's `sha256sum`), `binary.flag`/`binary.encoding`,
`lines.content`/`lines.start_line`/`lines.end_line`/`lines.line_count`/`lines.total_lines`,
and `download.big_code`/`download.big_len`/`download.content_length`
(6 MiB, larger than the 5 MiB read cap) plus `download.dir_422`.

## 9d. Native file execution: cwd, interpreters, runnable (Deliverable A)

The runs path was made able to run a chosen file, in a chosen directory, with a
chosen interpreter. The measured defects before the change (live server, real
nsjail):

- `kind:"file"` was hardcoded to Python: `runs.go` built
  `append([]string{"python3", file}, spec.Args...)` with no extension awareness.
- there was **no `cwd` on runs at all** — `cwd` on `POST /runs` was an unknown
  field → `400`, so "run this in this directory" was unsupported.
- a `cwd` (or any run target) that did not resolve produced **`503 sandbox
  unavailable`** (defect N-1), falsely implying the isolation layer was broken.

The change adds `cwd` (validated to exist and be a directory), interpreter
selection by extension with an explicit allowlist override, and a `runnable`
field on file `stat`/listing metadata.

### 9d.1 Defect N-1 — bad `cwd` was 503, now 422 (before/after)

Before (real nsjail, live server on 8099):

```
$ curl -s -w '\nHTTP %{http_code}\n' -H "$A" -H "$J" \
    -d '{"cmd":["pwd"],"cwd":"/nope"}' "$BASE/v1/workspaces/$WS/exec"
{"error":{"code":"unavailable","message":"sandbox unavailable: nsjail could not set up or launch the sandbox","details":{"nsjail":"[F][2026-11-01T13:25:35+0000][1] runChild():486 Launching child process failed"}}}
HTTP 503
```

The root cause was nsjail failing its own `chdir` inside the sandbox:

```
[E][…][1] initNsInternal():499 chdir('/workspace/does-not-exist'): No such file or directory
[F][…][1] runChild():486 Launching child process failed
```

After (real nsjail): the executor now classifies a `chdir` failure as
`ErrInvalidCwd` → `422`, and the service validates `cwd` at request time:

```
$ curl -s -w '\nHTTP %{http_code}\n' -H "$A" -H "$J" \
    -d '{"cmd":["pwd"],"cwd":"/nope"}' "$BASE/v1/workspaces/$WS/exec"
{"error":{"code":"unprocessable","message":"cwd does not exist","details":{"cwd":"/nope"}}}
HTTP 422
```

The acceptance suite asserts both directions: `run.cwd_missing_422` and
`exec.cwd_missing_422` are `422` (never `503`). A genuine sandbox setup failure
(e.g. a host without `--cgroupns=host`) is still `503 unavailable`.

### 9d.2 cwd is a real working directory

`run.cwd_is_realdir` runs `/rundir/whereami.py` with `cwd:/rundir`; the file
prints `os.getcwd()` and the assertion is `/workspace/rundir` — i.e. the process
really chdir'd into the requested directory. The file is passed to the
interpreter as an **absolute** sandbox path (`/workspace/rundir/whereami.py`),
so `cwd` is a working directory and **not** a relative-path prefix for `file`:

```
$ run.cwd_recorded            /rundir
$ run.cwd_is_realdir          /workspace/rundir
```

### 9d.3 Interpreter selection and runnable metadata

```
$ run.py_backcompat_interpreter   python3      # {"kind":"file","file":"/main.py"} still Python
$ run.sh_by_extension             bash         # .sh → bash, no interpreter field
$ run.sh_by_extension_out         ext-sh
$ run.interpreter_override        bash         # /runme.txt + {"interpreter":"bash"}
$ run.interpreter_override_out    sh-ok
$ run.unsupported_ext_code        unprocessable
$ run.unsupported_ext_names_set   contains .py
$ run.argv_file_conflict_422      422          # argv + kind=file is a hard conflict
$ stat.runnable_py                python3
$ stat.runnable_txt_none          None
```

Unit coverage (unprivileged): `TestStartRunFileInterpreterByExtension`,
`TestStartRunFileUnsupportedExtensionRejected`,
`TestStartRunExplicitInterpreterOverride`, `TestStartRunCwdValidation`,
`TestStartRunArgvAndFileConflict`, `TestRunFileUsesCwdAndInterpreter`,
`TestRunFileExtensionSelection`, `TestRunFileUnsupportedExtension422`,
`TestRunFileExplicitInterpreterOverride`, `TestRunCwdMissingIs422Not503`,
`TestRunArgvAndFileConflict422`, `TestFileStatAndListingRunnable`,
`TestMetaReportsInterpreters`.

## 9e. Unbounded runs and workspace stop (Deliverables B and C)

### 9e.1 Which interpreters exist in the sandbox (measured)

Interpreter selection is an **allowlist**, and every entry was measured
executable **inside the real sandbox** (not the host shell) with
`{"cmd":["<bin>","--version"]}` through `POST …/exec`:

| Binary | In the sandbox? | Evidence |
|--------|-----------------|----------|
| `python3` | yes | `Python 3.12.3` |
| `node` | yes | `v24.22.2` |
| `perl` | yes | `This is perl 5, version 38, subversion 2 (v5.38.2)` |
| `bash` | yes | `GNU bash, version 5.2.21(1)-release` |
| `python` | **no** | `422 command not found` |
| `ruby`, `php`, `lua` | **no** | `422 command not found` |

The allowlist is therefore `python3` (`.py`), `node` (`.js`/`.mjs`/`.cjs`),
`bash` (`.sh`/`.bash`) and `perl` (`.pl`/`.pm`) — matching the sandbox image's
verified set. `python3 -c`, `python3 file.py` and `bash file.sh` were each run to
prove the interpreters actually execute a script, not just print a version.

### 9e.2 nsjail `time_limit: 0` means "no limit" (measured)

The unbounded mechanism relies on nsjail's `time_limit`. Measured on this host
with the exact per-run mounts:

```
# time_limit: 0
$ sudo /usr/local/bin/nsjail -q -L 3 --config <time_limit_0>.cfg
SURVIVED                # a 6 s sleep ran to completion
# time_limit: 3
$ sudo /usr/local/bin/nsjail -q -L 3 --config <time_limit_3>.cfg
rc=137                  # killed at 3 s
```

So `no_timeout:true` renders `time_limit: 0` and layers no timeout context; the
run ends only on exit or an explicit stop.

### 9e.3 Unbounded runs (acceptance, real nsjail)

The suite creates a workspace with a deliberately small `max_wall_time_sec: 5`
so survival past the ceiling is provable without an hour-long test, and with
`allow_unlimited_runs: true` (the server-level switch is enabled in the
acceptance config):

```
$ unlimited.denied_code            unprocessable   # default WS (no workspace opt-in)
$ unlimited.denied_config          limits.allow_unlimited_runs
$ unlimited.recorded_flag          True
$ unlimited.recorded_timeout_null  None            # no fake 3600
$ finite.recorded_timeout          2
$ finite.times_out                 timeout         # the 5 s ceiling is real
$ unlimited.survives_past_ceiling  running         # alive 8 s later
$ unlimited.stop_status            killed
$ unlimited.stop_no_survivors      0
```

The same workspace's **finite** run (`timeout_sec: 2`) reaches `timeout`, proving
the ceiling still binds and that `no_timeout` is exactly what bypasses it; the
**unbounded** run is still `running` after 8 s and, once stopped, leaves **zero**
surviving processes and status `killed` — the kill path is proven, not assumed.
An unbounded run still holds a `max_runs` slot and its cgroup share (CPU /
memory / pids ceilings unchanged — the fork-bomb and memory-ceiling checks still
pass).

Unit coverage: `TestStartRunUnlimitedForbiddenByServerDefault`,
`TestStartRunUnlimitedForbiddenByWorkspace`,
`TestStartRunUnlimitedAllowedRecordsHonestly`,
`TestCreateWorkspaceUnlimitedAboveServerMaxima`, `TestRunNoTimeoutDeniedByDefault`,
`TestRunNoTimeoutAllowedRecordsUnlimited`.

### 9e.4 Workspace stop now stops runs *and* sessions (acceptance)

The gap (found by reading `service/control.go`): `StopWorkspace` called
`StopAllRuns` only, so stopping a workspace left its terminal sessions running.
Fixed to stop both and report both counts. The suite creates a long run and a
live session on a workspace, then:

```
$ stop.runs_count              1
$ stop.sessions_count          1
$ stop.run_id_reported         run_…
$ stop.session_id_reported     ses_…
$ stop.run_status              killed
$ stop.session_status          killed
$ stop.session_reason          workspace_stopped
$ stop.zero_survivors          0     # no process of the workspace's tree survives
$ stop.workspace_active        active
```

`suspend` and `delete` were audited too: both already stopped sessions
(`SuspendWorkspace` calls `StopWorkspaceSessions`; `DeleteWorkspace` does as
well), and the suite confirms `session.reason workspace_stopped` and the delete
path leaves nothing behind — no known limitation remains. Unit coverage:
`TestStopWorkspaceStopsRunsAndSessions` (and the existing
`TestStopWorkspaceKillsRuns`/`TestSuspendStopsRunningRuns`).

## 9f. Native process control: inventory, stop, no leaks (acceptance)

The measured defect (see [DECISIONS.md](DECISIONS.md) #63): a process started
inside a session was invisible to and unreachable from the API. `DELETE` of the
session returned `200` and the process survived; `POST .../stop` left it alive;
`GET .../runs` and `GET .../usage` reported zero; and `grep -c processes
api/openapi.yaml` was `0`. There was **no API that could stop it**.

The change adds `GET /v1/workspaces/{id}/processes`,
`POST /v1/workspaces/{id}/processes/{processID}/stop`, extends
`POST /v1/workspaces/{id}/stop` with the process fields, removes a run's/session's
cgroup when it ends, and reaps orphaned workloads and cgroups on startup.

### 9f.1 The measured mechanism (real nsjail, on this host)

Every claim below was measured on the host with the real nsjail and cgroup v2
before it was written into the code:

- **A jail's cgroup is named after its workload.** A jail's workload (PID 1 of
  its own PID namespace) is the first child of the nsjail process, and nsjail
  names the cgroup `NSJAIL.<that host pid>`. Measured:
  `/proc/<workload>/cgroup → 0::/NSJAIL.<workload>` and
  `/proc/<workload>/status → NSpid: <host-pid> 1`.
- **The cgroup is the only reliable handle.** A workload started with
  `/usr/bin/setsid` leaves its process group, so `kill(-pgid)` misses it, but its
  `/proc/<pid>/cgroup` still points at the jail's cgroup. `cgroup.kill` (writing
  `1` to `<dir>/cgroup.kill`) SIGKILLs every member regardless. Measured: after
  `echo 1 > NSJAIL.<n>/cgroup.kill` the setsid'd process was gone; the base
  binary's pgid-only kill leaves it alive.
- **The attribution tag is inherited.** `SANDBOXD_WORKSPACE`/`SANDBOXD_KIND`/
  `SANDBOXD_SOURCE` rendered as nsjail `envar:` entries are present in the
  `/proc/<pid>/environ` of the jailed process **and of a descendant** (a
  `setsid`'d child), which is what lets the inventory map a detached process back
  to its workspace/session.
- **nsjail cleans up its own cgroup on a clean exit, but not after a SIGKILL.**
  A quick `true` jail and a `SIGTERM` to nsjail both removed the cgroup and left
  no surviving workload; a `SIGKILL` to nsjail left the cgroup **and** the
  reparented workload alive (this is exactly why the startup reap is required).

### 9f.2 The real-nsjail integration test

`TestIntegrationProcessInventoryAndCgroupKill` (`//go:build integration`,
[requires privileged container]) is the in-repo proof: it starts a run whose
entry shell launches a `setsid sleep`, finds it in `ListJailProcesses` with
`kind=run` and the right `source_id`, and asserts `KillProcessTree` stops it.

```
$ sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" HOME=/root \
    go test -tags integration -count=1 -run TestIntegrationProcessInventoryAndCgroupKill -v ./internal/executor/
=== RUN   TestIntegrationProcessInventoryAndCgroupKill
--- PASS: TestIntegrationProcessInventoryAndCgroupKill (0.13s)
PASS
ok  	sandboxd/internal/executor	0.131s
```

### 9f.3 The self-test script

`scripts/process-stop-check.sh` is the purpose-built real-nsjail acceptance
harness (Deliverable D). It hosts the service against the host's real nsjail,
uses **two principals** (`dev-key-please-rotate`→`user_dev` and `other-key`→
`user_other`, so the cross-owner check expects `403`), writes a config in which
`allow_unlimited_runs: true` appears in **both** `limits` and `max_limits`
(explicitly, never regex-patched — a substitution would produce a duplicate YAML
key and read as a broken binary), puts `data_dir` outside the repo, and uses port
`8099`. It stops its own workloads by exact pid / exact cgroup and never touches
another tenant's.

```
$ sudo -E bash scripts/process-stop-check.sh
=== building ===
=== starting the service under real nsjail ===
  service ready: {"cgroups":true,"data_dir":true,"nsjail":true,"status":"ready"}
=== 1) a session-started detached process is listed with kind/source_id ===
  PASS  detached process listed (yes)
  PASS  detached process kind (session)
  PASS  detached process source_id (ses_…)
=== 2) DELETE of the session ends its whole process tree (the contract) ===
  PASS  session delete (200)
  PASS  session delete leaves zero processes (0)
  PASS  no surviving marked process (bracketed pgrep) (0)
=== 3) POST /workspaces/{id}/stop leaves zero processes for the workspace ===
  PASS  workspace has live processes before stop (yes)
  PASS  workspace stop returned 200 (200)
  PASS  workspace stop leaves zero processes (0)
  PASS  workspace stop: no marked survivor (0)
=== 4) a setsid()-escaping workload is stopped by the single-process stop ===
  PASS  setsid-escaped process listed (yes)
  PASS  single-process stop status (200)
  PASS  setsid-escaped process stopped (gone)
  PASS  workspace empty after the single stop + cleanup (0)
=== 5) isolation: workspace A's processes are not visible from workspace B ===
  PASS  foreign inventory refused (403)
  PASS  foreign process stop refused (403)
  PASS  workspace A sees its own process (yes)
  PASS  workspace B inventory empty (0)
=== 6) NSJAIL cgroup count returns to its pre-run value ===
  PASS  cgroups did not grow past baseline (no)
  PASS  cgroups back to pre-run value (0)
=== 7) a forced daemon death + restart reaps the orphan ===
  PASS  marked workload alive before the kill (yes)
  PASS  nsjail survived the hard daemon death (1)
  PASS  nsjail reaped on restart (0)
  PASS  orphan workload reaped on restart (0)
  PASS  orphan cgroup removed on restart (0)

================ process-stop acceptance: 25 passed / 0 failed ================
```

**Session-DELETE contract, stated and asserted.** `DELETE` of a session ends the
session **and its process tree**: the executor reaps the session's cgroup when the
session ends (any reason), so a process the session started cannot outlive it.
The harness asserts exactly that (checks in §2 above): after `DELETE`, the
inventory for the workspace is empty and no marked process survives. This is the
stronger of the two contracts the task allowed ("does not leave it", or "still
listed and stoppable"); the harness asserts the stronger one.

**This is the check that catches the gap where unit tests stay green.** A stub
runner cannot observe that a jailed, `setsid()`-escaped process survives a
process-group kill; only the real nsjail + cgroup v2 can. The unprivileged suite
covers the contract at the handler level (`TestListProcesses*`,
`TestStopProcess*`, `TestStopWorkspaceStopsDetachedProcesses`,
`TestProcessesCrossOwnerForbidden`) and the executor renders/parses the tag and
`/proc` inputs (`TestTagEnvRenderedIntoConfig`, `TestParseStatPPIDAndStart`,
`TestCgroupDirOfPIDSelf`); the harness proves the real behaviour.

## 9g. Egress and quick tunnels (acceptance)

`scripts/tunnel-check.sh` is the real-nsjail acceptance harness for per-workspace
egress and quick Cloudflare tunnels. It builds sandboxd, runs it under the host's
REAL nsjail, slirp4netns and cloudflared, and asserts the whole contract — and,
critically, that the host's interface and route topology is byte-for-byte
unchanged. It prints `SKIP` and exits 0 when root, nsjail, slirp4netns or
cloudflared is absent (it cannot prove anything without them).

**The third state — skipped-environment.** Cloudflare's quick-tunnel
provisioning is unauthenticated and **rate limited**, and `cloudflared` prints
the literal line `quick tunnel provisioning failed with status 429` when the
shared quota is spent (the engine surfaces that as `504 tunnel_url_unavailable`
with cloudflared's own text in `details.reason`). The harness matches **that
message — never a timeout** to distinguish "somebody else's quota ran out" from
"the engine is broken", retries the create with bounded backoff
(`TUNNEL_RETRIES` attempts, `TUNNEL_BACKOFF` seconds base), and if the limit
persists records the six tunnel-dependent assertions
(`tunnel.url_is_trycloudflare`, `tunnel.public_fetch_serves_workspace`,
`tunnel.delete_204`, `tunnel.url_dead_after_delete`,
`lifetime.cf_running_before`, `lifetime.cf_gone_after_delete`) as **SKIP** with
the reason and exits **0**. Any **other** failure stays a hard **FAIL** — the
skip is never blanket — and the documented `504` + `tunnel_url_unavailable`
contract is asserted in both cases. The tally separates the three states.

```
$ sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/tunnel-check.sh
=== building ===
=== host topology before: 0 netns entries ===
=== starting the service under real nsjail ===
  service started
=== 1) allow_net workspace: DNS + pypi + package install ===
  PASS  egress.dns_resolves (yes)
  PASS  egress.pypi_http (yes)
  PASS  egress.package_installs (0)
=== 2) isolation: host LAN + another workspace unreachable ===
  PASS  isolation.host_22_blocked (blocked)
  PASS  isolation.host_dns_blocked (blocked)
  PASS  isolation.control_443_blocked (blocked)
  PASS  isolation.other_workspace_blocked (blocked)
=== 3) allow_net:false workspace reaches nothing ===
  PASS  no_net.host_22_blocked (blocked)
  PASS  no_net.public_ip_blocked (blocked)
=== 4) create a tunnel and fetch it publicly ===
  PASS  tunnel.app_listens_inside (tunnelcheck-…)
  PASS  tunnel.url_is_trycloudflare (yes)
  PASS  tunnel.public_fetch_serves_workspace (yes)
=== 5) delete the tunnel; the URL stops working ===
  PASS  tunnel.delete_204 (204)
  PASS  tunnel.url_dead_after_delete (yes)
=== 6) error contracts (403/404/400) and the documented 5xx ===
  PASS  contract.foreign_create_403 (403)
  PASS  contract.unknown_tunnel_404 (404)
  PASS  contract.bad_port_400 (400)
  PASS  contract.out_of_range_400 (400)
  PASS  contract.no_url_code (tunnel_url_unavailable)
  PASS  contract.no_url_status (504)
=== 7) the tunnel dies with its workspace ===
  PASS  lifetime.cf_running_before (yes)
  PASS  lifetime.ws_delete_204 (204)
  PASS  lifetime.cf_gone_after_delete (0)
=== 8) zero host topology ===
  PASS  topology.links_unchanged (same)
  PASS  topology.routes_unchanged (same)
  PASS  topology.no_new_veth_or_tap (0)
  PASS  topology.netns_empty_after (0)

================ tunnel acceptance: 27 passed / 0 failed ================
```

**Measured: a rate-limited run is skipped, not failed.** The same host, on a day
when Cloudflare's quick-tunnel quota was spent, produced (raw transcript, exit
`0`):

```
=== 4) create a tunnel and fetch it publicly ===
  PASS  tunnel.app_listens_inside (tunnelcheck-46865)
  cloudflare 429 on attempt 1/3; retrying in 5s
  cloudflare 429 on attempt 2/3; retrying in 10s
  cloudflare quick-tunnel rate limit persisted after 3 attempt(s)
  cloudflare quick-tunnel rate limit (HTTP 429 from provisioning); tunnel-dependent assertions will be SKIPPED
  SKIP  tunnel.url_is_trycloudflare (cloudflare quick-tunnel rate limit (HTTP 429 from provisioning))
  SKIP  tunnel.public_fetch_serves_workspace (cloudflare quick-tunnel rate limit (HTTP 429 from provisioning))
...
  PASS  contract.no_url_code (tunnel_url_unavailable)
  PASS  contract.no_url_status (504)
...
  PASS  topology.links_unchanged (same)
  PASS  topology.routes_unchanged (same)
  PASS  topology.no_new_veth_or_tap (0)
  PASS  topology.netns_empty_after (0)

================ tunnel acceptance: 21 passed / 0 failed / 6 skipped (cloudflare quick-tunnel rate limit (HTTP 429 from provisioning)) ================
```

**Measured: a non-rate-limit failure still FAILS.** With a stub `cloudflared`
that prints `quick tunnel provisioning failed with status 530` (not 429), the
same harness reported `23 passed / 4 failed / 0 skipped` and exited **1** — the
skip is not blanket. The `contract.no_url_code` / `contract.no_url_status`
assertions stayed green in every case, proving the documented `504
tunnel_url_unavailable` path (the state a rate limit also produces) is under
test.

**Measured mechanism.** The egress fabric is chained slirp4netns:
`workspace ns --slirp(UNIX)--> relay ns --slirp(UNIX)--> host`. The relay carries
a blackhole route for every operator-private range, so a workspace reaches the
public internet and a public resolver but not the host LAN, the host's own
services, or another workspace. No host interface, address or route is ever
created: the harness asserts `ip -o link` and `ip route` are identical before and
after, no `veth`/`tap` interface exists, and `/run/netns` is left empty.

**What is genuinely host-privileged here.** The harness needs root only for the
real nsjail (cgroup v2 + mount namespaces) and for slirp4netns to create the
engine namespaces. The unprivileged suite covers the API contract
(`TestCreateTunnelHappyPath`, `TestTunnelNoURLIs504`,
`TestTunnelsDisabledBy*`, `TestTunnelsNeedEgress`, `TestTunnelsMaxPerWorkspace`,
`TestDeleteTunnelStopsProcessAndRecord`, `TestStopSuspendDeleteReleaseTunnels`,
`TestPatchAllowTunnelsRejectedWhenServerDisabled`,
`TestMetaReportsTunnelsCapability`) and the network layer's pure logic
(`TestMatchTunnelURL`, `TestIsQuickTunnelArgs`, `TestAllocatorReserveRelease`,
`TestShortIDStable`, `TestShellQuote`).

## 9h. Host identity: what a workspace can and cannot observe (acceptance)

`scripts/identity-check.sh` is the real-nsjail acceptance harness for
host-identity masking. It builds sandboxd, runs it under the host's REAL nsjail
(mask-on and mask-off instances), and measures and asserts the whole property.
It prints `SKIP` and exits 0 when root, nsjail, slirp4netns or the Go toolchain
is absent.

**The third state — skipped-environment.** Assertion 6 (the egress IP) depends
on the public internet reachable from the workspace. If it is not, that one
assertion is recorded as **SKIP** with the reason and the suite still exits `0`;
a suite that goes red because the internet is down trains people to ignore it.
This is **never** used for a masking, filesystem-view or topology failure — those
are always hard **FAIL**. The tally separates the three states.

```
$ sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/identity-check.sh
=== building ===
=== host baseline ===
  host kernel=7.1.8-zabbly+ cpus=2 hostname=node-id-1.runtime.zcp.zerops
  workspace limits: cpus=1 mem=256MB
=== starting the mask-on service under real nsjail ===
  service started (pid 109069)
  readyz.identity_masking: {'enabled': True, 'ready': True}
  PASS  readyz.identity_masking.ready (True)
  PASS  readyz.provider_tools_hidden.ready (True)
  PASS  meta.provider_tools_hidden.mechanism (curated)
=== 1) the 'already closed' paths are unreadable ===
  PASS  closed.paths_none_readable (0)
=== 2) hostname and internal DNS reveal nothing ===
  PASS  host.gethostname_generic (sandbox)
  PASS  host.gethostname_not_host (no)
  PASS  host.hostname_f_fails (no)
  PASS  host.hostname_I_empty ()
  PASS  host.internal_dns_unresolved ()
=== 3) allow_net:false reaches no what-is-my-IP service ===
  PASS  no_net.no_ip_service_reached (0)
=== 4) masking ON reports the workspace's limits (and survives env -i) ===
  PASS  mask.release_not_host (no)
  PASS  mask.release_generic (0.0.0-sandbox)
  PASS  mask.cpu_count_workspace (1)
  PASS  mask.sysconf_cpu_workspace (1)
  PASS  mask.memory_workspace_bytes (268435456)
  PASS  mask.cpu_not_host (no)
  PASS  mask.env_i_nproc (1)
  PASS  mask.env_i_uname (0.0.0-sandbox)
  PASS  mask.env_i_getconf (1)
  PASS  mask.shim_not_writable (0)
  PASS  mask.runtime_python3 (0 'py-ok')
  PASS  mask.runtime_bash (0 'sh-ok')
  PASS  mask.runtime_perl (0 'perl-ok')
=== 8) the hosting platform's own tooling is hidden ===
  PASS  provider.zcp_absent (ABSENT)
  PASS  provider.zsc_absent (ABSENT)
  PASS  provider.zcli_absent (ABSENT)
  PASS  provider.no_vendor_strings (0)
  PASS  provider.no_vendor_certs (0)
  PASS  provider.audit_scanned (yes)
  PASS  provider.runtime_python3 (0 'py-ok')
  PASS  provider.runtime_node (0 'node-ok')
  PASS  provider.runtime_git (0 'git-ok')
=== 9) the dlopen-scoped dlsym bypass is closed (syscall(2) is known) ===
  PASS  bypass.default_handle_masked (0.0.0-sandbox)
  PASS  bypass.named_handle_masked (0.0.0-sandbox)
  PASS  bypass.raw_syscall_known_real (7.1.8-zabbly+)
=== 6) measured egress IP on the allow_net:true path ===
  measured egress IP: 93.185.106.128
  PASS  egress.ip_is_public (yes)
  egress provider identity: ORG AS43541 VSHosting s.r.o. (colo=PRG loc=CZ)
  note: workspace egress IP equals the host's own public egress IP (platform-shared NAT, ADR 76)
=== 5) masking OFF leaks the host values (the switch is verifiable) ===
  PASS  mask_off.readyz_enabled (False)
  PASS  mask_off.release_leaks_host (7.1.8-zabbly+)
  PASS  mask_off.cpu_leaks_host (2)
  PASS  mask_off.mem_leaks_host (yes)
  PASS  mask.runtime_node_no_regression (133)
=== 7) the host topology is untouched ===
  PASS  topology.links_unchanged (same)
  PASS  topology.routes_unchanged (same)
  PASS  topology.no_new_veth_or_tap (0)
  PASS  topology.netns_empty (0)

================ identity acceptance: 48 passed / 0 failed / 0 skipped ================
```

**Measured: `node` is constrained by `rlimit_as` independently of masking.**
`node -e 'console.log(42)'` exits `133` with V8's `Fatal process out of memory:
SegmentedTable::InitializeTable` **both** with masking on and with masking off,
because nsjail renders `rlimit_as = memory_mb` (256 MB) and V8 reserves a large
virtual address space at startup. The harness records that outcome and asserts
the masked and unmasked results are **equal** (no regression from masking)
rather than asserting a success that the image does not support; `python3`,
`bash` and `perl` all produce their output under the mask. This is a
pre-existing resource-model limitation, not a masking defect.

**The measured numbers (this session, target host).** The workspace was capped at
`cpu_cores: 1, memory_mb: 256`; the host is `7.1.8-zabbly+` with 2 cores and
`132000778 × 4096 = 540675186688` bytes (≈540 GB) of RAM.

| Probe | Host (mask OFF) | Workspace (mask ON) |
|-------|-----------------|---------------------|
| `uname.release` | `7.1.8-zabbly+` | `0.0.0-sandbox` |
| `uname.version` | `#ubuntu24.04 SMP PREEMPT_DYNAMIC Mon Aug 10 04:00:53 UTC 2026` | `#1 SMP` |
| `os.cpu_count()` / `getconf _NPROCESSORS_ONLN` | `2` | `1` |
| `sysconf(_SC_PHYS_PAGES)·4096` | `540675186688` | `268435456` |
| `env -i … nproc` | `2` | `1` |
| `gnu_get_libc_version()` | `2.39` (distro `2.39-0ubuntu8.9` trimmed) | `2.39` |

**Measured: a genuine `env -i` cannot defeat the mask.** Because the shim is
reached through a read-only `/etc/ld.so.preload` (read by the dynamic loader for
every dynamically-linked exec) rather than the `LD_PRELOAD` environment variable,
`env -i PATH=/usr/bin:/bin nproc` and `env -i PATH=/usr/bin:/bin uname -r` inside
the workspace still return the masked values. This is why the plain `LD_PRELOAD`
environment variable was rejected (it is cleared by `env -i`).

**Measured leak the fix closed: the host `/etc/hosts`.** On the `allow_net:true`
path the engine previously bind-mounted the host's own `/etc/hosts`, which on
this host contains `10.9.112.7  node-id-1.runtime.zcp.zerops` — the operator's
server FQDN and LAN address. The engine now writes a per-workspace hosts file
holding only generic localhost entries and mounts that; the host's file is never
mounted into a workspace (`TestRenderConfigMountsResolverWhenNetAllowed` asserts
`src: "/etc/hosts"` never appears).

**Measured: the egress IP is a platform-shared NAT address.** An `allow_net:true`
workspace fetching `https://api.ipify.org`, `https://icanhazip.com` and
`https://ifconfig.me/ip` presented **`93.185.106.128`** — which is also the
host's own public egress IP (measured identically from the host), i.e. a
platform-shared NAT address, not a per-workload one. To measure it the harness
copies `scripts/tunnel-check.sh`'s exact service startup and network block (the
same `network:` config and `start_service`), because a plain standalone harness
that did not configure the relay hit `interface did not come up in sbxdws-…`.

**Measured leak the fix closed: the platform's own CLI and vendor material.**
With `identity.enabled=true, allow_net=true` inside a real workspace, before the
fix:

```
$ command -v zcp      → /usr/local/bin/zcp
$ ls /usr/local/bin/  → cloudflared nsjail yq zcp zsc
$ strings /usr/local/bin/zcp | grep -i zerops
    ZEROPS_Y
    ub.com/zeropsio
```

The root cause was the `/usr` bind mount bringing `/usr/local` along. A full
audit found the vendor strings not only in `zcp`/`zsc` but in
`/usr/local/sbin/includes/basic_inc` (`bubenik@vshosting.cz`), the vendor CA
sources `Zerops Infra` / `Zerops Service Intermediate`, and the vendor Go
toolchain. The curated overlays (ADR 74) remove all of it; assertion 8 runs a
`strings`-style audit over `/usr/local`, `/usr/sbin`, `/opt`, `/sbin` and the
trust store and asserts **no** file names the vendor (`zcp`/`zsc`/`zcli` absent,
`PROVIDER_HITS=0`, `CERT_HITS=0`), while `python3`/`node`/`git` still run. The
TLS filter matches on the **decoded DER** (the provider name is base64 in the PEM
text, so a substring test finds nothing — measured), and drops the provider
certificates from both the per-cert files and the `ca-certificates.crt` bundle.

**Measured leak the fix closed: the dlopen-scoped `dlsym` bypass.**
The previous task's ADR 72 filed this under "inherently bypassable". Measurement
showed otherwise: inside a masked workspace,

```python
ctypes.CDLL("libc.so.6", use_errno=True).uname(buf)   # → 7.1.8-zabbly+  (REAL)
ctypes.CDLL(None).uname(buf)                          # → 0.0.0-sandbox  (masked)
libc.syscall(63, buf)                                 # → 7.1.8-zabbly+  (raw)
```

`dlopen("libc.so.6")` returns a specific object handle whose `dlsym` searches
only libc's local scope, skipping the shim, with **no raw syscall**. The shim now
interposes `dlsym` (hostmask.c): a request to resolve a masked symbol from **any**
handle returns the shim's own function — the same value the `RTLD_DEFAULT` path
already returned. Measured after the fix (assertion 9):

| Route | Before | After |
|-------|--------|-------|
| `ctypes.CDLL("libc.so.6").uname` | `7.1.8-zabbly+` | `0.0.0-sandbox` |
| `ctypes.CDLL(None).uname` | `0.0.0-sandbox` | `0.0.0-sandbox` |
| `libc.dlopen("libc.so.6")` + `dlsym` | `7.1.8-zabbly+` | `0.0.0-sandbox` |
| `dlsym(RTLD_NEXT, "uname")` | `7.1.8-zabbly+` | `0.0.0-sandbox` |
| `dlvsym(handle, "uname", "GLIBC_2.2.5")` | `7.1.8-zabbly+` | `7.1.8-zabbly+` (residual) |
| `libc.syscall(63, buf)` | `7.1.8-zabbly+` | `7.1.8-zabbly+` (inherent, asserted) |

Why `dlvsym` is not interposed: on this glibc, defining **both** `dlsym` and
`dlvsym` in the preload object makes a versioned reference (`dlsym@GLIBC_2.2.5`)
bind back to the shim's own unversioned definition, and the dynamic loader's
bootstrap recurses at process start — measured, reproducible, drops every
process. `dlvsym` also requires compiled code or an FFI that exposes it (CPython
`ctypes` does not; `ctypes.CDLL` is `dlsym`-based), so the one-liner is closed
and the residual is documented, not hidden. The raw `syscall(2)` path is asserted
as a **known** result (`bypass.raw_syscall_known_real`), not left a surprise.

**Measured: the egress address names the provider.** An `allow_net:true`
workspace presented `93.185.106.128` (also the host's own egress). Three
independent services name the provider:

```
ipinfo.io        → org: "AS43541 VSHosting s.r.o."  (Prague, CZ)
rdap.db.ripe.net → name: "ZEROPS-IO"  (93.185.106.0/24, CZ; MNT-VSHOSTING, VS2641-RIPE)
cloudflare trace → colo=PRG loc=CZ
```

Reverse DNS of the neighbouring `93.185.106.1` is `gw.vshosting.cz`. The
NEEDS-CONFIRMATION finding (ADR 76): **no** egress address outside the vendor's
registered ranges is obtainable here — the host's only public path is the
`10.9.112.0/22` private interface NATed to `93.185.106.128`, there is no proxy
configured or available, and the `/24` is registered to `ZEROPS-IO`. The
optional `network.egress_proxy` knob (default **off**) is the only lever, and it
changes the operator's abuse/reputation profile, so it is their call.

**The honest limits.** A statically-linked binary, a musl runtime (whose loader
ignores `/etc/ld.so.preload`), a raw `syscall(2)`, and a handle-scoped
`dlvsym(3)` bypass the shim; those limits are stated in [DECISIONS.md](DECISIONS.md)
#72, in the config example, and in `/v1/meta`'s `identity_masking.limits`. Masking
is defense-in-depth, not a security boundary; the filesystem view stays closed by
the mount namespace on every path (assertion 1), and that part holds for
**every** binary.

## 10. Decisions

`docs/DECISIONS.md` contains **76** numbered resolved decisions, each with a
one-line rationale, plus implementation defaults (including #54, the engine/
frontend product boundary, #55–58, the file browsing/viewing contract,
#59–62, native file execution / unbounded runs / workspace stop / the named-
service non-goal, #63, native process control, #64–69, per-workspace egress via
chained slirp4netns and quick Cloudflare tunnels, including the recorded
host-topology hazard of the abandoned veth/route design, and #70, the
quick-tunnel rate-limit / ephemeral-URL operational limit and the harness's
skipped-environment third state), plus #71–#73, the host-identity threat model,
the /etc/ld.so.preload masking mechanism and its honest limits, and the
fail-loud opt-out switch; #72 was **corrected** this iteration to separate
"inherently bypassable" from "a gap we closed". #74 covers hiding the platform's
own tooling and vendor material (the curated /usr/local and /etc/ssl/certs
overlays), #75 covers closing the dlopen-scoped `dlsym` bypass, and #76 records
the egress-address finding and the default-off `network.egress_proxy` knob.

## 11. OpenAPI

`api/openapi.yaml` validates as OpenAPI 3.1:

```
$ npx @redocly/cli lint api/openapi.yaml
Woohoo! Your API description is valid. 🎉
You have 10 warnings.         # operation-4xx-response on unauthenticated probes
```

All paths, methods, request/response schemas, and error codes are described.

## 12. Git

`git log --oneline` shows the milestone commits; `git status` is clean.

## 13. Documentation consistency

`scripts/docs-consistency.sh` asserts that the documents match the code: every
route in `internal/api/router.go` appears in `docs/API.md`, every error code in
`internal/model/errors.go` appears in the API error table, every new endpoint
(including all session routes) appears in `docs/SPEC.md`,
`docs/VERIFICATION.md` has packages, resolver and sessions sections, the stated
test count equals the real one, `docs/DECISIONS.md` covers each required
decision (including the session/PTY/idle/lifetime/stdin decisions), the OpenAPI
lint passes, and `gofmt`/`go build`/`go test` stay green. It exits non-zero on
any failure.

```
$ bash scripts/docs-consistency.sh
…
passed=<N> failed=0
```

## Known limitations

- SSE log streaming is deferred to v0.1; logs are polled with `offset`/`limit`.
- **Host-identity masking is defense-in-depth, not a security boundary.** A
  statically-linked binary, a musl runtime, or a raw `syscall(2)` bypasses the
  preload shim and sees the real host kernel, core count and RAM. The
  filesystem view (no `/proc`, no `/sys`) is closed on every path regardless;
  see [DECISIONS.md](DECISIONS.md) #72. This is stated, not hidden.
- Multi-node storage is deferred; storage is local filesystem JSON.
- DB-backed/dynamic authentication is deferred; keys are static in the config
  (bearer auth itself is exercised by `TestAuth*`).
- Per-UID-per-workspace isolation is deferred to v0.1 (v0.0.17 uses a shared
  `sandbox` UID/GID with per-run user namespaces and cgroups).
- `rlimit_fsize` bounds a single file, and `disk_mb` is enforced on the file API
  path and via a `du` recheck on workspace load; it is not enforced by the
  kernel against the sandboxed process tree.
- `node` (V8) reserves a large virtual address space at startup and hits
  `rlimit_as` (`memory_mb`) on this image, exiting `133` with a V8 OOM — with or
  without host-identity masking. Raising `memory_mb` or running V8 with
  `--max-old-space-size` within the limit is the operator's lever; it is a
  resource-model property, unrelated to masking.
- `RLIMIT_NPROC` does not bind for the root-run sandbox; the process ceiling is
  therefore provided by `cgroup_pids_max`, which is why `--cgroupns=host` is a
  hard requirement.
- Real nsjail needs namespace/cgroup privileges. On hosts with
  `kernel.apparmor_restrict_unprivileged_userns=1` and no `CAP_SYS_ADMIN` the
  bare-host run fails writing `uid_map`; this is a host restriction, not a bug in
  sandboxd. Verification therefore runs the service in a privileged container
  with `--cgroupns=host`.
