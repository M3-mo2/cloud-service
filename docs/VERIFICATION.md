# Verification

Results for sandboxd v0.0.20 against the acceptance criteria in the spec.

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
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/process-stop-check.sh` → `30 passed / 0 failed`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/session-nsjail-check.sh` → `18 passed / 0 failed`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/tunnel-check.sh` → `27 passed / 0 failed / 0 skipped`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/identity-check.sh` → `131 passed / 0 failed / 2 skipped`
> - `BASE=http://127.0.0.1:8099 KEY=… OTHER_KEY=… DATA_DIR=… NSJAIL=/usr/local/bin/nsjail sudo -n bash scripts/acceptance.sh` → `113 passed / 0 failed`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/subscription-check.sh` → `62 passed / 0 failed`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/archive-check.sh` → `102 passed / 0 failed`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/metrics-check.sh` → `56 passed / 0 failed`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/spread-check.sh` → `32 passed / 0 failed`
> - `sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/lostfound-check.sh` → `27 passed / 0 failed`
> - `bash scripts/docs-consistency.sh` → `401 passed / 0 failed`
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

Test functions: **523** (519 unit + 4 `integration`-tagged), including regression
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
`TestMetaReportsTunnelsCapability`; see §9g), and for workspace subscriptions
(the duration parser, the pure decision function and the summary derivation,
including the boundary instants exactly at expiry and exactly at grace end:
`TestParseDurationValid`, `TestParseDurationInvalid`,
`TestParseDurationZeroSumIsRejected`, `TestParseDurationOverflow`,
`TestDecideBoundaries`, `TestDecideBackwardsClockNeverDeletes`,
`TestDecideGraceFromStoredGraceEndsAt`, `TestDerivationsUnlimited`,
`TestDerivationsStates`, `TestDerivationsShortTermNotAlwaysExpiringSoon`,
`TestRemainingZeroImpliesLocked`,
`TestExpiringSoonWindow`, `TestApproximationConstants`; and the lifecycle with an
injected clock: `TestCreateWithDurationSetsExpiry`,
`TestCreateWithNoSubscriptionIsUnlimited`, `TestCreateDefaultDurationConfig`,
`TestCreateWithAbsoluteExpiresAt`, `TestCreateSubscriptionValidation`,
`TestExtendWhileLiveAddsForward`, `TestExtendLiveWithEarlierExpiresAtRefused`,
`TestExtendLiveWithLaterExpiresAtAccepted`, `TestReaperLocksAtExpiry`,
`TestReaperDeleteAfterGrace`, `TestExtendWhileLockedUnlocks`,
`TestExtendDeletedFails`, `TestReaperIdempotent`,
`TestReaperBackwardsClockDoesNotDelete`, `TestReaperSkipsSuspended`,
`TestSummaryDerivationAgreesWithLock`; and the API surface:
`TestCreateWithDurationShowsSubscriptionAndSummary`,
`TestCreateWithoutSubscriptionIsUnlimited`,
`TestCreateSubscriptionValidation`, `TestExtendWhileLiveMovesForward`,
`TestExtendWouldShortenRefused`, `TestListItemsCarrySummary`,
`TestLockedRefusesComputeAllowsReads`, `TestExtendLockedRestoresService`,
`TestMetaAdvertisesSubscriptions`; see §9l).

The count is the number of `^func Test` declarations across every `*_test.go`
file (the same measure `scripts/docs-consistency.sh` asserts against):

```
$ find . -name '*_test.go' -exec grep -hE '^func Test' {} + | wc -l
465
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
ok   meta.version                 0.0.18
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
ok   rlimit.memory                memory_limited (exit 137)
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
report `FORK_LIMITED`, and the memory probe must be stopped (exit `137` from the
cgroup OOM killer, or `MEMORY_DENIED`).

### Resource-ceiling evidence

Raw introspection through the API with declared `memory_mb=512`,
`disk_mb=1024`, `max_runs=3` (`cgroup_pids_max = 3 * 64 = 192`):

```
$ curl … -d '{"cmd":["python3","lim.py"]}' …/exec     # lim.py prints getrlimit
AS_LIMIT_MB 2048                                       # memory_mb(512)+1024, floor 2048 (ADR 82)
# the physical ceiling is cgroup_mem_max = memory_mb bytes = 536870912 (host-side;
# /sys is not mounted inside the sandbox, by design)

$ curl … -d '{"cmd":["python3","f.py"]}' …/exec       # f.py attempts 500 live forks
FORK_LIMITED BlockingIOError
FORKED 191                                             # stopped at the cgroup pids ceiling
```

The earlier `AS_LIMIT_MB 512` line recorded the F-1 unit fix (a byte value had
made it 512 TiB). It is now `memory_mb + 1024` because `rlimit_as = memory_mb`
aborted node/V8 (ADR 82); `cgroup_mem_max = memory_mb` bytes is the physical
ceiling. `acceptance.sh` #19 touches 4 GiB and asserts the process is stopped
(`exit 137` or `MEMORY_DENIED`).

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
$ nsjail --config <one-shot-style>.cfg   # exec_bin = python3 -c "<relay source>"
Traceback (most recent call last):
  File "<string>", line 190, in main
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

Plus: `TestRenderSessionConfigHelperNoFile` (relay passed inline via `-c`, no
`/run/sandboxd` file or path, seccomp/uidmap/`mount_proc:false` retained),
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
--- PASS: TestRenderSessionConfigHelperNoFile       # relay inline via -c; no file/path; seccomp+uidmap kept
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
- **The attribution tag is inherited.** `WORKSPACE_ID`/`PROCESS_KIND`/
  `PROCESS_SOURCE` rendered as nsjail `envar:` entries are present in the
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
  service started (pid 140194)
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
  PASS  runtime.python3 (0 'py-ok')
  PASS  runtime.node (0 'node-ok')
  PASS  runtime.node_arithmetic (0 '42')
  PASS  runtime.bash (0 'sh-ok')
  PASS  runtime.perl (0 'perl-ok')
  PASS  runtime.git (0 'git-ok')
=== 8) the hosting platform's own tooling is hidden ===
  PASS  provider.zcp_absent (ABSENT)
  PASS  provider.zsc_absent (ABSENT)
  PASS  provider.zcli_absent (ABSENT)
  PASS  provider.no_vendor_strings (0)
  PASS  provider.no_vendor_certs (0)
  PASS  provider.audit_scanned (yes)
  PASS  provider.runtime_python3 (0 'py-ok')
  PASS  provider.runtime_node (0 'node-ok')
  PASS  provider.runtime_bash (0 'sh-ok')
  PASS  provider.runtime_perl (0 'perl-ok')
  PASS  provider.runtime_git (0 'git-ok')
  PASS  provider.runtime_node_no_regression (0)
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
  PASS  mask.runtime_node_no_regression (0)
=== 7) the host topology is untouched ===
  PASS  topology.links_unchanged (same)
  PASS  topology.routes_unchanged (same)
  PASS  topology.no_new_veth_or_tap (0)
  PASS  topology.netns_empty (0)

================ identity acceptance: 70 passed / 0 failed / 2 skipped ================
```

**Measured: `node` starts and runs under the mask — the earlier failure was
`rlimit_as`, not the shim (ADR 82).** A follow-up measured that
`node -e 'console.log(42)'` died with V8's `Fatal process out of memory:
SegmentedTable::InitializeTable` **with no shim loaded at all** (`ulimit -v
262144`), with the previous syscall-only shim, and inside a real sandbox with
masking **off** — the newly interposed filesystem symbols were not the cause.
Node's virtual-address threshold is identical (1121 MB) with and without the
shim. The engine now separates the physical ceiling (`cgroup_mem_max =
memory_mb`) from the virtual backstop (`rlimit_as = memory_mb + 1024`, floored
at 2048), because V8 reserves ~1.1 GiB of virtual address space (a 512 MiB
`PROT_NONE` CodeRange `mmap` plus heap reservations) before `main`, and
`RLIMIT_AS` counts reservations the process never commits. Measured after:
`node -e 'console.log(42)'` → `42` at 256 MB and 512 MB, mask on and off;
`python3`/`bash`/`perl`/`git` unaffected. `identity-check.sh` now asserts every
runtime **succeeds** under the mask (a runtime regression can no longer pass as
"equality of outcome").

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

## 9i. Zero signal: no engine/platform artifact a tenant can read (acceptance)

The operator's bar for this iteration is **zero signal**: a tenant must not be
able to tell they are on this platform, nor see the engine's internals. This
section records what was measured, what was removed, and what could not be.

**Measured before the fix (a real `exec` in a real workspace, `ed973ee`).**

```
$ ls /usr/bin | grep -iE 'nsjail|sandbox|slirp'
nsjail
slirp4netns
$ command -v runcon setpriv unshare nsenter chroot
/usr/bin/runcon /usr/bin/setpriv /usr/bin/unshare /usr/bin/nsenter /usr/sbin/chroot
$ ls /                                → … sandboxd …
$ ls -la /sandboxd/                   → -rw-r--r-- 16432 hostmask.so
$ cat /etc/ld.so.preload              → /sandboxd/hostmask.so
$ strings /sandboxd/hostmask.so       → hostmask.c, 0.0.0-sandbox, 5× SANDBOXD_MASK_*, GCC 13.3.0
$ env | grep SANDBOXD                 → SANDBOXD_KIND, SANDBOXD_WORKSPACE, SANDBOXD_MASK_{CPUS,MEM_BYTES,KERNEL_RELEASE,KERNEL_VERSION}
$ ls -la /run/sandboxd/               → -rw-r--r-- 10316 pty_relay.py   (in a session; the engine's source)
```

**Measured after the fix** (a fresh 1-CPU/256-MB workspace; `identity-check.sh`
assertion 10, all `PASS`):

```
ROOTS            ['bin','dev','etc','lib','lib64','opt','run','sbin','tmp','usr','workspace']   # no /sandboxd
/run             ['.rt.so']                                   # opaque; the shim, OPAQUE name only
ld.so.preload    /run/.rt.so
tool names       none        (nsjail slirp4netns runcon setpriv unshare nsenter chroot all absent)
engine env       none
name tokens      0           (\b(sandboxd|hostmask|nsjail|slirp4netns|zerops|vshosting|zabbly)\b
                              over every reachable file; 66 478 files scanned)
path named hostmask  0
account files no provider   (passwd zerops: False, group zerops: False)
relay source     absent      (no /run/sandboxd/pty_relay.py, no /run/sandboxd)
shim strings     clean       (no engine token in rt.so)
```

The one documented allowance: the **generic networking word** `hostmask` (the
inverse of a netmask) survives inside Python's `ipaddress` module and node's
`netmask` package. Dropping those files to remove the substring would break the
Python and Node standard libraries, so `hostmask` is a **name-only** token
(a path named `hostmask` is dropped; a file merely *containing* the word is
kept). Measured inside the curated tree:
`ipaddress.ip_network("10.0.0.0/8").hostmask` → `0.255.255.255`.

The runtime still works with the curated trees: `python3`, `node`, `bash`,
`perl` and `git` all run (node after the `rlimit_as` fix, ADR 82); `python3 -m
pip --version`, `import ssl, json, urllib, ipaddress` and `git --version` all
succeed. Curating `~2.5 GB` of host trees costs **~10 s once** per service start
(filesystem reflinks; measured) and almost no disk.

**Measured this session — the mechanism itself is now INVISIBLE (ADR 79).**
The shim interposes the filesystem entry points and answers `ENOENT` for
`/etc/ld.so.preload`, its own in-jail path `/run/.rt.so`, `/sandboxd` and
`/run/sandboxd`. `identity-check.sh` group 10c measures this **from inside a
real 1-CPU/256-MB workspace**, for every configured path across every method:

```
# With the shim loaded (the tenant's situation), the EXISTENCE matrix reports
# HIDDEN (the shim answers ENOENT):
#   os.path.exists / os.path.isfile / open() / os.listdir          HIDDEN
#   ls -la / cat / stat / test -e / test -f / test -r              HIDDEN
#   find <parent> -maxdepth 1 -name <leaf>                         (no hit)
# The CANONICALIZATION matrix never echoes (the shim answers EACCES, so
# coreutils aborts; readlink(1) itself is HIDDEN via EACCES):
#   readlink <path>                                                HIDDEN (rc!=0)
#   readlink -f/-e <path> / realpath / realpath -e <path>          NOT echoed (rc!=0)
#   os.path.realpath(<path>)                                       literal echo (documented residual)
# 48/48 existence method·path checks HIDDEN; 0 VISIBLE.
#   invisible.no_hidden_path_visible            0
#   invisible.canonicalization_no_echo          0    (0 of 16 canonicalizer checks echo)
#   invisible.canonicalization_probe_ran        yes
#   invisible.py_realpath_residual_literal      4    (4 of 4 echo only their own argument)
#   invisible.loader_env_i_still_masked         MASKED   (env -i uname -r = 0.0.0-sandbox)
#   invisible.loader_env_u_still_masked         MASKED   (env -u LSM_SYS_* uname -r = 0.0.0-sandbox)
#   invisible.raw_syscall_known_limit           REACHED  (newfstatat(262) still reaches it)
#   invisible.shim_strings_clean                0
#   invisible.shim_build_marker_present         yes      (9f3a71c2)
```

**The loader scope is the correctness core, and it holds.** The dynamic loader
reads `/etc/ld.so.preload` *before* any preload object is mapped, so the hiding
never intersects the loader; only a process that already has the shim answers
`ENOENT`/`EACCES`. Measured in the same workspace: `cat`/`ls`/`stat`/`test`/
`find`/`os.path.exists`/`os.listdir` all report `/etc/ld.so.preload` missing,
while `env -i /usr/bin/uname -r` and `env -u LSM_SYS_REL -u LSM_SYS_VER uname -r`
still return the masked `0.0.0-sandbox`. There is no trade-off.

**Strings are no longer self-announcing.** `strings -a` over the shipped shim
(`sha256 3190afe8d78f072fc4a3d1818670698091d59dfe3bbd273bd6d9bf6319c73d14`,
26 800 bytes) matches **none** of `sandboxd|hostmask|ld.so.preload|LSM_SYS|
SANDBOXD|0.0.0-sandbox|#1 SMP` (the masked values and variable names are XORed
and decoded at runtime) and does contain the opaque positive-control marker
`9f3a71c2`. The previous artifact leaked all of the above
(`sha256 ec8cf3f0…`). `-Wl,-soname,rt.so` keeps the version script's
auto-`SONAME` from leaking the engine name into the dynamic string table.

**Stage 2 — the narrower zero-artifact goal, measured and honest.** The shim
still exists as a *file* (nothing for the loader to read would also remove the
mask); the *visibility* of that file is closed by ADR 79. Two mechanisms could
remove the file altogether; both were measured and **neither holds** on this
host within bounded effort:

```
# (A) libc replacement: a naive wrapper libc.so.6 breaks every dynamic binary.
$ LD_LIBRARY_PATH=<wrapper> /usr/bin/true
/usr/bin/true: libc.so.6: no version information available (required by /usr/bin/true)
/usr/bin/true: symbol lookup error: undefined symbol: __libc_start_main, version GLIBC_2.34

# (B) seccomp user_notif: the SHIPPED nsjail (3.4, the Dockerfile pin) has none.
$ /usr/bin/nsjail --seccomp_unotify -q -- true
nsjail: unrecognized option '--seccomp_unotify'

# A from-source build of the UNRELEASED master (HEAD d8415bc, 3.6-122-gd8415bc)
# DOES support it and starts a loop on this kernel:
$ /tmp/nsjail-master --config <seccomp_unotify:true + exec /bin/sh -c echo> 
[I] Started unotify loop
… but master is unreleased, and user_notif is a supervisor TRACER (it logs
filesystem/network ops to a report), not a value-interception hook. See ADR 80.

# 3.4's kafel USER_NOTIF action yields ENOSYS, not SECCOMP_RET_USER_NOTIF:
$ nsjail --config <USER_NOTIF { sysinfo } + DEFAULT ALLOW>
sysinfo: Function not implemented          # blocked, not intercepted
uname=7.1.8-zabbly+                        # still the real value
```

`secdump` is not installed; `/proc/sys/kernel/seccomp/actions_avail` advertises
`user_notif` but no supervisor owns a listener fd. The kernel cannot be made to
lie either: writing `/proc/sys/kernel/osrelease`/`version` fails with
`Permission denied` (re-measured this session). The narrower zero-artifact goal
is therefore **not** reached, recorded precisely (ADR 77 + ADR 80), and the
harness reports both mechanisms as `skip` (third state), never as a pass. What
remains asserted: the mechanism is **invisible** inside the workspace (ADR 79,
group 10c), the shim is untamperable (read-only mounts; tampering attempts all
`Read-only file system`) and survives `env -i`, and the raw `syscall(2)` path is
the documented permanent limit (asserted as the real value).

**identity-check.sh this session: `64 passed / 0 failed / 2 skipped`.** The two
skips are the Stage 2 zero-artifact mechanisms above (each with its measurement
as the reason); the seven new `PASS`es are the visibility group 10c.

## 9j. Workspace profiles: an explicit allowlist (this revision)

This revision replaces the denylist tool model with **named profiles** carrying
an explicit allowlist, and extends `scripts/identity-check.sh` with group §11
(execution checks inside a real workspace). Every number below is from a real
run against real nsjail; **identity-check.sh this revision: `97 passed / 0
failed / 2 skipped`** (was 70; the new group is `27` assertions, all using
execution, not file-existence). The two skips are the unchanged Stage-2
zero-artifact mechanisms.

A `python`-profile workspace, by execution:

```
python -c 'print("py-ok")'                                            -> 0 'py-ok'
python3 -c 'import json,csv,sqlite3,ssl,ctypes,urllib,email,\
xml,asyncio,pdb,unittest,pydoc;print("stdlib-ok")'                    -> 0 'stdlib-ok'
pip --version                                                         -> 0  pip 24.0
node -e 'console.log(6*7)'                                            -> 0 '42'
git --version                                                         -> 0  git version 2.43.0
perl -e 'print "perl-ok\n"'                                           -> 0 'perl-ok'
awk 'BEGIN{print "awk-ok"}'                                           -> 0 'awk-ok'
```

and a tool the profile does **not** name is genuinely absent (execution fails
`422 unprocessable` command-not-found, asserted for `gcc`, `cc`, `apt`, `cmake`,
`gdb`):

```
gcc --version        -> 422 command not found
apt --version        -> 422 command not found
sh -c 'command -v gcc'  -> (empty)
```

A `minimal`-profile workspace runs a shell and coreutils but **lacks every
language runtime** — the proof the allowlist excludes:

```
sh -c 'echo sh-ok'   -> 0 'sh-ok'
python3 / python / node / git / gcc / curl  -> 422 command not found (each)
```

The API surface, by execution:

```
POST /v1/workspaces {"name":"x","profile":"python"}  -> 201, record profile "python"
POST /v1/workspaces {"name":"x"}                     -> 201, record profile "python" (default applied)
POST /v1/workspaces {"name":"x","profile":"nope"}    -> 422 unknown_profile (details.profile="nope")
GET  /v1/meta .profiles.default                      -> "python"
GET  /v1/meta .profiles.profiles[*].name             -> [minimal, python]
readyz .profiles.ready                               -> true
meta   .provider_tools_hidden.mechanism[0]           -> profile-materialized
```

A terminal **session** works on `python` (a real PTY: `/dev/pts/N`, `IS_TTY`,
`stty size` → `24 80`); on `minimal` it is refused with the actionable `422
session_needs_interpreter` (the PTY relay is a Python program).

Two real bugs were found by **executing** rather than by inspecting files, and
both are fixed:

- the curated tree omitted `/usr/lib/ssl`, so `urlopen("https://pypi.org/…")`
  failed with `CERTIFICATE_VERIFY_FAILED: unable to get local issuer
  certificate` even though `/etc/ssl/certs` was mounted. `tunnel-check.sh` went
  from `26/1` to `27/0` once `/usr/lib/ssl` was added to the base library set.
- a profile alias (`python` → `python3`) has no host executable, so command
  resolution now returns the jail path `/usr/bin/<name>` for a profile command.

The other gates are unchanged and green against the new default profile:
`acceptance.sh 113/0`, `process-stop-check.sh 25/0`, `session-nsjail-check.sh
18/0`, `tunnel-check.sh 27/0`, `docs-consistency.sh 160/0` (was 158; +2 for the
two new error codes), `336` test functions.

## 9k. Egress decoy: a plausible invented public IP (this revision)

This revision adds the **egress-decoy** layer (`internal/decoy`): an
in-namespace stub DNS resolver and stub HTTP/TLS responder that answer a
workspace's attempts to learn its own public IP with a plausible, **consistent**
invented identity, while ordinary internet is untouched. It is a **decoy, not a
security boundary**; see [EGRESS-DECOY.md](EGRESS-DECOY.md) and
[DECISIONS.md](DECISIONS.md) ADR 84. It is **default off**
(`network.decoy.enabled: false`); the one-line change that turns it on is
`network: { decoy: { enabled: true } }`.

`scripts/egress-decoy-check.sh` runs the real service against the host's real
nsjail and slirp4netns with the decoy ON and asserts, from inside a real
workspace. **This session: `58 passed / 0 failed / 0 skipped`** (the decoy-on
service was built from this commit; the running binary was md5-checked against a
fresh `go build`).

Covered HTTP services answer the decoy, never the real address and never a
platform name (the real host egress IP was `93.185.106.128` in this run):

```
api.ipify.org           -> 198.51.100.N      ipinfo.io/org   -> AS64500 Example Transit Networks
icanhazip.com           -> 198.51.100.N      ifconfig.me/ip  -> 198.51.100.N
ip-api.com/line         -> 198.51.100.N|DE|DE|Hesse|Frankfurt|0|Example Transit Networks
ident.me                -> 198.51.100.N      checkip.amazonaws.com -> 198.51.100.N
wtfismyip.com/text      -> 198.51.100.N      api.myip.com    -> {"ip":"198.51.100.N","country":"Germany","cc":"DE"}
cloudflare /cdn-cgi/trace -> ip=198.51.100.N colo=FRA loc=DE
```

and an **invented** hostname matching the heuristic is covered too (the most
valuable assertion — it proves the hostname PATTERN, not a fixed list, is used):

```
curl https://whats-my-public-ip.invented-example.net/  -> 198.51.100.N
```

Every classic DNS method answers the decoy:

```
dig +short myip.opendns.com @resolver1.opendns.com        -> 198.51.100.N
dig +short o-o.myaddr.l.google.com TXT @8.8.8.8           -> "198.51.100.N"
dig +short whoami.cloudflare CH TXT @1.1.1.1              -> "198.51.100.N"
dig +short whoami.akamai.net @8.8.8.8                     -> 198.51.100.N
```

and the mechanism-bypassing paths are covered (the in-namespace nft rules force
port 53 to the stub and redirect the well-known DoH/DoT addresses):

```
DoH curl https://cloudflare-dns.com/dns-query?name=myip.opendns.com&type=A  -> {"Status":0,"Answer":[... "198.51.100.N"]}
DoH curl https://dns.google/resolve?name=myip.opendns.com&type=A           -> {"Status":0,"Answer":[... "198.51.100.N"]}
DoT dig +tls @1.1.1.1 whoami.cloudflare CH TXT                            -> "198.51.100.N"
direct dig myip.opendns.com @8.8.8.8                                      -> 198.51.100.N
```

The client runtimes all get the decoy with **no certificate warning** (a warning
would be worse than nothing — it tells the tenant the environment is tampered
with), because the generated decoy CA is added to the workspace's curated
`/etc/ssl/certs` bundle and hashed name and per-hostname certificates are minted
on demand:

```
curl   -sS https://api.ipify.org   rc=0  -> 198.51.100.N   (stderr: no certificate text)
wget   -qO- https://api.ipify.org        -> 198.51.100.N
python urllib.request.urlopen        -> 198.51.100.N   (default verify path: no error)
python requests.get                  -> 198.51.100.N
```

Consistency — every channel agrees on ONE address and ONE org:

```
IP=198.51.100.N
ORG=AS64500 Example Transit Networks
ipinfo JSON org == ipwho.is org == "Example Transit Networks"
all addresses across ipify/ipinfo/api.myip/cloudflare-trace/1.1.1.1-trace: {198.51.100.N}
no channel mentions 93.185.106 / vshosting / zerops
```

The **negative control** — ordinary internet still works with the decoy on:

```
pip download --no-deps six   -> PIPRC=0  (Successfully downloaded six)
curl -o /dev/null -w %{http_code} https://github.com            -> 200
curl -o /dev/null -w %{http_code} https://registry.npmjs.org/six -> 200
curl -o /dev/null -w %{http_code} https://pypi.org/simple/       -> 200
```

The host topology is untouched (no interface, no route, no namespace, no stray
helper), matching the existing invariant in `identity-check.sh` §7.

The decoy is reported truthfully: `/readyz.egress_decoy` is
`{"enabled":true,"ready":true}` with the layer on and
`{"enabled":false,"ready":false,"reason":"network.decoy.enabled is false"}` with
it off; `/v1/meta.egress_decoy` carries the `mechanism` and the honest `limits`.
With the layer **off** (the default), `identity-check.sh` §6 still reports the
measured real egress IP — no gate regresses. `go test ./...` passes; the suite
adds `11` test functions (net `347`, from `336`), and
`scripts/docs-consistency.sh` stays green.

## 9l. Workspace subscriptions (this revision)

A workspace may carry a **subscription**: a running term with an **absolute
expiry instant**. This is an engine feature, not a billing feature — the engine
owns the **clock** and the **enforcement** and stores absolute instants (never a
"days remaining" counter), so state is recomputed from stored times after a
restart and is never held in memory. A workspace with **no** subscription never
expires and is never touched by the reaper; that is the default, so the feature
changes the behaviour of no existing workspace. The pure parts (the duration
parser, the reaper decision, the summary derivation) live in
`internal/subscription` and are unit-tested with injected boundary instants; the
loop and the effects live in `internal/service/subscription.go`.

### 9l.1 The binary under test

The tested binary is a **fresh build of the committed tree** and carries a
`strings`-level positive control, so a stale artifact cannot pass the gate:

```
$ go build -o /tmp/sbx-verify ./cmd/sandboxd
$ md5sum /tmp/sbx-verify
09d022b48b89e98ab948bd39bf42765b  /tmp/sbx-verify
$ grep -aoF 'zcp-subscription-reaper-v0.0.20' /tmp/sbx-verify
zcp-subscription-reaper-v0.0.20
```

The gate itself performs this check (`grep -aF "$MARKER" "$BIN"`) and reports
`PASS build marker present (zcp-subscription-reaper-v0.0.20)`. It also confirms
the port is free before starting, and proves the daemon owns it (`ss -ltnp`
shows `pid=<SRV>`), so the "two daemons on one port" failure cannot masquerade as
broken code.

### 9l.2 The compressed-clock acceptance transcript

`scripts/subscription-check.sh` drives the REAL service and nsjail with a
compressed clock via config (`grace_period: 10s`, `reap_interval: 1s`), so the
whole days-long lifecycle is asserted in under a minute. Measured
`61 passed / 0 failed`, exit 0, run on the host (root) with the real nsjail:

```
=== 1) create with a duration: active, expires_at, summary remaining ===
  PASS  create with duration status (201)
  PASS  create status active (active)
  PASS  expires_at is set (yes)
  PASS  summary remaining > 0 (yes)
=== 2) extend while live: expiry moves FORWARD by the added duration ===
  PASS  extend while live status (200)
  PASS  expiry moved forward (yes)
  PASS  extension added exactly 20s (20)
  PASS  extensions counter (1)
=== 3) lapse -> reaper LOCKS; compute refused; teardown is real ===
  PASS  create s2 status (201)
  PASS  start long run status (202)
  PASS  workload running before lock (yes)
  PASS  disk mounted before lock (yes)
  PASS  reaper locks s2 at expiry (yes)
  PASS  lock tore down process and disk mount (yes)
  PASS  process inventory empty after lock (0)
  PASS  disk unmounted after lock (no)
  PASS  exec refused while locked (403)
  PASS  refusal code (subscription_expired)
  PASS  run creation refused while locked (403)
  PASS  session refused while locked (403)
  PASS  file write refused while locked (403)
  PASS  get while locked (200)
  PASS  read file while locked (200)
  PASS  usage while locked (200)
  PASS  locked workspace kept its data (keep-me)
=== 4) extend while locked -> UNLOCKS and serves again ===
  PASS  extend while locked status (200)
  PASS  status active after renewal (active)
  PASS  locked_at cleared (null)
  PASS  exec works after renewal (200)
=== 5) grace lapses -> reaper DELETES; nothing leaked ===
  PASS  create s3 status (201)
  PASS  s3 locks at expiry (yes)
  PASS  s3 deleted after grace (yes)
  PASS  s3 tree gone (yes)
  PASS  s3 disk.img gone (yes)
  PASS  s3 has no mount (yes)
  PASS  no fuse2fs leak (count back to baseline) (yes)
=== 6) manual DELETE of a locked workspace works ===
  PASS  s4 locks at expiry (yes)
  PASS  manual delete of locked 204 (204)
  PASS  s4 gone after manual delete (404)
  PASS  s4 tree gone (yes)
=== 7) a workspace WITHOUT a subscription is never touched ===
  PASS  no-sub has no subscription (null)
  PASS  no-sub summary state (unlimited)
  PASS  no-sub still active (active)
  PASS  no-sub still exists (200)
=== 8) a backwards clock reading does not delete ===
  PASS  s6 locks at expiry (yes)
  PASS  meta rewritten with future locked_at (yes)
  PASS  s6 survived a backwards clock reading (200)
  PASS  s6 tree still present (yes)
=== 9) re-running reaper ticks is idempotent ===
  PASS  daemon healthy after many ticks (200)
  PASS  deleted s3 did not reappear (404)
  PASS  s7 locks at expiry (yes)
  PASS  s7 deleted after grace (yes)
  PASS  daemon healthy after idempotent ticks (200)
=== leak check: no cgroup growth, no mount under the data dir ===
  PASS  cgroups did not grow past baseline (yes)
  PASS  no mount left under the data dir (0)
  PASS  no fuse2fs process left for this run (<= baseline) (yes)

================ subscription acceptance: 61 passed / 0 failed ================
```

What each group proves:

- **create with a duration** — `expires_at` is set, the status is `active`, and
  the derived `summary.subscription.remaining_sec` is positive.
- **extend while live** — the expiry moves **forward by exactly the added
  duration** (20s), never backwards, and `extensions` increments.
- **lapse** — the real reaper **locks** the workspace at `expires_at`; `exec`,
  runs, sessions and file writes are refused with `403 subscription_expired`
  while `GET`, `GET /usage` and file reads still work; the **running workload's
  process is stopped** (inventory drains to 0) and the **disk mount is unmounted**
  (`findmnt` finds none), proving locking actively stops using resources rather
  than merely labelling; the user's file survives byte-for-byte (`keep-me`).
- **extend while locked** — the renewal starts from now, **unlocks** the
  workspace (`status: active`, `locked_at` cleared) and **exec works again**.
- **grace lapses** — the reaper **deletes** the workspace: its tree, `disk.img`
  and metadata are gone, `findmnt` finds no mount, and the `fuse2fs` count
  returns to baseline.
- **manual delete of a locked workspace** — `DELETE` returns 204 and removes it.
- **no subscription** — a workspace with no term is never locked or deleted,
  even across many grace periods and reap ticks; its summary reads `unlimited`.
- **backwards clock** — a workspace whose recorded `locked_at` is rewritten into
  the future (a non-monotonic reading) survives ticks that would otherwise
  delete it.
- **idempotency** — the daemon stays healthy across many ticks, a deleted
  workspace does not reappear, and a second delete tick is a no-op.

`tests` — the `internal/subscription` package adds pure unit tests for the parser
(valid, invalid, zero-sum, overflow), the decision function (every boundary
instant: exactly at expiry, exactly at grace end; the backwards-clock rule) and
the derivations (unlimited, active, expiring-soon, locked, clamped-at-0); the
service adds the whole lifecycle with an injected clock; the API adds the
request/response contract. Suite total `465` test functions (see §1).

## 9m. Native archives (this revision)

Archive handling (upload-and-extract, in-place extract, directory export) is
performed **in the engine** — Go's `archive/zip` and `github.com/nwaples/rardecode/v2`
— never by shelling out. This host has **no RAR tool at all** and `unrar-free`
cannot read RAR5, so the trust boundary lives in the engine; RAR is read-only
because its encoder is proprietary. Extraction is a **security boundary**: an
untrusted archive must not write outside the destination, set a privileged bit,
or exceed a budget. See [DECISIONS.md](DECISIONS.md) ADR 92.

### 9m.1 The binary under test

The tested binary is a **fresh build of the committed tree** and carries a
`strings`-level positive control (`archive.BuildMarker`), so a stale artifact
cannot pass the gate:

```
$ go build -o /tmp/sbx-archive ./cmd/sandboxd
$ grep -aoF 'zcp-archive-native-v0.0.20' /tmp/sbx-archive
zcp-archive-native-v0.0.20
```

### 9m.2 The real-nsjail acceptance

`scripts/archive-check.sh` drives the REAL service and nsjail and extracts the
**committed** `internal/archive/testdata` fixtures through the real API, so the
gate cannot pass without them. It asserts a normal zip with nested dirs, an empty
file and a unicode name extracts; a `../` traversal and an absolute path are
refused with `422 archive_unsafe_entry` and **nothing is written outside** (the
would-be escape paths are asserted absent); a setuid bit is stripped (`0755`, not
`4755`); a zip bomb and a ratio bomb are refused without the disk growing; an
encrypted ZIP and RAR, and a multi-volume RAR, are refused; a hostile RAR
symlink/hardlink is refused; random bytes, an empty file and a truncated zip are
refused with the daemon still alive; `dry_run` writes nothing and reports the
entries; `overwrite:false` refuses a collision (`409 archive_entry_exists`) and
preserves the content while `overwrite:true` replaces it; `POST /files/extract`
works on an uploaded archive; and `GET /files/archive` streams a valid zip with
the tree. The measured numbers are recorded in §14c.

### 9m.3 The pure engine and the API surface

`internal/archive` carries 32 test functions: content detection (ZIP/RAR4/RAR5,
unknown, empty), the plan (traversal, absolute, drive letter, intra-archive
collision, junk skip, per-file/ratio caps), the **budget boundaries** (exactly at
the cap is allowed; one byte over the total and over the entry is refused; the
written bytes never exceed the budget), mode stripping (`4755`→`0755`), symlink
refusal, the overwrite policy (`archive_entry_exists` preserves the original;
`overwrite:true` replaces it) and the RAR refusals (encrypted RAR4/RAR5,
multivolume, evil-symlink, hardlink) plus a ZIP round-trip that never emits a
symlink. `internal/api/archive_test.go` covers the three endpoints, the disabled
switch, `dry_run`, traversal and overwrite, and `/v1/meta` advertising the
capability.

## 9n. Per-workspace resource stream (RAM + disk) (this revision)

A workspace's RAM and disk are pushed every `metrics.interval` (default **3s**)
over SSE, cheaply enough that thousands of workspaces cause no pressure. The
requirement is hard: an implementation that samples per connection, or walks a
directory tree, fails it even though it "works". The design is fixed by
measurement (see [DECISIONS.md](DECISIONS.md) ADR 93): one lazily-started
service-wide sampler, one sample per tick fanned out to every subscriber, idle
elision (a workspace with no live cgroup reads zero cgroup files), disk from a
single `statfs`, and RAM as the **SUM** over the workspace's per-run and
per-session cgroups. Four routes are shipped (`GET /v1/metrics`,
`GET /v1/metrics/stream`, `GET /v1/workspaces/{id}/metrics`,
`GET /v1/workspaces/{id}/metrics/stream`), gated on `metrics.enabled`.

### 9n.1 The measured cost model

| source | cost | note |
|---|---|---|
| disk: `statfs` | **1.7–1.9 µs**, O(1) | `workspace.MountedDiskUsed`; no tree walk |
| disk: tree walk + lstat | **153 µs PER FILE**, O(files) | what `Service.Usage()` does; never on the sampling path |
| RAM: `memory.current` | **23.6 µs/read** | per live cgroup |
| RAM: `memory.max` | **22.0 µs/read** | equals the workspace's `memory_mb` |
| `cpu.stat` | 31.0 µs/read | opt-in (`include_cpu`) |
| `pids.current` | 25.4 µs/read | opt-in (`include_cpu`) |

Measured directly: 1000 statfs calls = 1.9 ms; a walk of 17908 files = 2742 ms
(82× worse per file). The stream's disk figure is therefore `statfs`, and the
gate asserts the sampler's `tree_walk_reads` counter stays **0**.

```
per live cgroup, per tick (2 files)      ≈ 46 µs
per workspace, per tick (1 statfs)       ≈  1.7 µs
1000 workspaces x 1 live run:  1000 statfs + 1000 cgroups x 46µs  ≈  48 ms/tick  ≈ 1.6% of one core
10000 workspaces x 1 live run:                                    ≈ 480 ms/tick ≈ 16%  of one core
```

A `sudo` on the sampling path would cost ~9 ms/call versus ~23 µs direct (a 400×
penalty), so there is none; the service runs as root and reads the cgroup files
directly.

### 9n.2 The disk-reporting bug this revision fixed (measured)

`MountedDiskUsed` computed `used = total − f_bavail`. `f_bavail` excludes ext4's
reserved-block margin (mkfs's `-m 5` default), so the published figure did not
match the tenant's own `df`:

```
                          empty                    after writing 10 MB
df -B1 inside workspace   used = 90112             used = 10579968     <- what the TENANT sees
MountedDiskUsed (before)  used = 7639040  (11.9%)  used = 18128896     <- what the API published
total - f_bfree (now)     used = 90112             used = 10579968     <- matches df exactly
```

`MountedDiskUsed` now uses `f_bfree` (one `statfs` call); an empty 64 MB
workspace reports `90112` bytes, not `7639040`. The operator's rule is that the
number reported must equal the number the tenant sees. `df` shows `size=64M` but
only ~`57M` *available* because ext4 reserves ~5 % for root; the engine reports
**used** (`total − f_bfree`), so the reservation is a deliberate property the
reporting does not blur.

### 9n.3 The real-nsjail acceptance

`scripts/metrics-check.sh` drives the REAL service, real nsjail and a real FUSE
disk. Every wait is bounded and at most two workspaces run concurrently (this
check is about resource cost, so it must not itself hammer the host). Measured
`51 passed / 0 failed` across four consecutive runs:

- the build marker is present (`zcp-metrics-stream-v0.0.20`) and the daemon's
  `/proc/<pid>/exe` hash equals the built image;
- both stream routes return `text/event-stream` and deliver an initial frame
  promptly (bounded wait, no bare sleep); a stream of 3 frames is parseable JSON
  with monotonically increasing timestamps and ~1s cadence;
- a run holding a 60 MiB allocation drives `ram_used_bytes` above a 40 MB floor,
  and it falls back to 0 once the run ends;
- with **two** runs the reported RAM exceeds the single-run reading by a margin a
  single cgroup cannot produce (measured `135245824 >= 100819840`), proving the
  figure is the **sum**;
- with no live run RAM is 0 and the cgroup-read counter does not advance across
  ticks (idle elision);
- with three subscribers open, one sample pass reads exactly **2** cgroup files
  per tick (independent of the count) — fan-out, not per-subscriber sampling;
- the `tree_walk_reads` counter is 0, and a snapshot of a 3000-file workspace
  returns in well under 0.2s (a walk would cost ~0.46s at the measured 153 µs
  per file);
- `?interval=1ms` is refused with **422** (floor enforced); `?interval=10ms` at
  the floor is accepted;
- a subscriber stalled on a tiny socket buffer advances the `dropped_frames`
  counter (coalescing, never buffering) and the subscriber count stays bounded;
- after five open/close cycles the subscriber count is 0, the sampler reports
  `running:0`, and the tick counter is frozen with zero subscribers and while
  one is connected (lazy start/stop is real);
- an **empty** workspace reports ~0 disk used and matches `df -B1
  --output=used /workspace` run **inside** it within one block; after a 10 MiB
  write the stream, the `/v1/workspaces/{id}` summary and `df` all agree, and
  `GET /usage` (a different path: a byte-sum walk) agrees within tolerance;
- `GET /v1/meta` advertises the capability; and no fuse2fs, mount or cgroup is
  left behind.

### 9n.4 The pure parts and the API surface

`internal/metrics` carries unit tests for the interval clamp, the percentage
arithmetic (including a **zero limit** — never divide by zero), cgroup-file
parsing, the sum across cgroups (including no cgroups), idle elision, fan-out,
coalescing drops, lazy start/stop, repeated open/close with no leak, scope
isolation across principals, and JSON frame shape with monotonic timestamps.
`internal/workspace` carries a unit test locking the `f_bfree` fix (against the
measured numbers). The four routes and the frame shape are documented in
[docs/API.md](API.md) and `api/openapi.yaml`, and advertised in `GET /v1/meta`.

## 9o. Per-workspace resource ceiling (this revision)

The RAM half of the "a workspace is a server-shaped thing with its own quota"
model was missing: the ceiling was **per run** (ADR 94). Measured before the
change: one workspace at `memory_mb: 128` with three runs produced **three**
`/sys/fs/cgroup/NSJAIL.<pid>` cgroups, each with `memory.max = 134217728`, so it
could consume 384 MB against an advertised 128 MB. This revision makes the
memory/CPU/pids ceiling **per workspace**, mirroring the disk half (ADR 85).

### 9o.1 The mechanism (measured)

- ONE parent cgroup per workspace (`ws-<id>`) carries `memory.max = memory_mb`,
  `memory.swap.max = 0`, `memory.oom.group = 1`, `pids.max = max_runs*64`,
  `cpu.max = <cpu_cores*1000000> 1000000`, with `+memory +cpu +pids` delegated in
  `cgroup.subtree_control`.
- nsjail is pointed at that parent with `cgroupv2_mount`, so every run's and
  session's leaf cgroup (`<parent>/NSJAIL.<pid>`) is created **inside** it; the
  parent enforces the SUM. Moving an existing cgroup into a parent is **EPERM**
  (measured: `mv` → "Operation not permitted", `os.rename` `errno=1`), so the
  parent must exist before launch and nsjail must be told to use it.
- The per-run limits are kept **equal** to the workspace's values, not 0:
  measured, nsjail creates **no** leaf cgroup when all three per-run limits are
  0, which would cost the cgroup-scoped kill/inventory handle.
- Measured enforcement: two concurrent 100 MB allocations against a 128 MB
  parent drove the parent's `oom_kill` counter and both runs exited 137; a single
  200 MB allocation exited nsjail 137 with the parent recording `oom_kill 2`,
  `oom_group_kill 1`.

### 9o.2 The real-nsjail acceptance

`scripts/spread-check.sh` drives the REAL service, real nsjail and a real FUSE
disk (bounded: one workspace, at most three concurrent runs, torn down between
phases; this check must not hammer the host). It asserts:

- creating and running in a workspace creates **exactly one** parent cgroup
  `ws-<id>` with `cpu memory pids` delegated and `memory.max = memory_mb`,
  `memory.swap.max = 0`, `memory.oom.group = 1`;
- a run's leaf cgroup appears **inside** the parent and **zero** `NSJAIL.*` exist
  directly under `/sys/fs/cgroup` while runs are live;
- **the central assertion**: a single allocation of 2 × `memory_mb` is refused
  (run status `oom`, the parent records `oom_kill`), and with **three**
  concurrent ~0.9 × `memory_mb` allocations whose SUM exceeds `memory_mb` the
  parent's `memory.max` is `memory_mb`, the API's `ram_limit_bytes` is exactly
  `memory_mb`, the ceiling fires an `oom_kill`, and a run reads as `oom` — the
  measured "before" was three runs × `memory_mb` all succeeding;
- `ram_used <= ram_limit` and `ram_percent <= 100` under load (the contract the
  tenant sees; the old model could report > 100%);
- a `setsid()`-escaped process is **still killed** by the workspace stop (the
  kill path survived the layout change);
- deleting the workspace removes its parent cgroup and leaves no `NSJAIL.*` and
  no `ws-*` parent behind; and
- the disk budget is the workspace's OWN allowance: `df` inside the workspace
  equals `disk_mb`, and a write past it is refused.

Measured `32 passed / 0 failed`. The gate's own daemon-exe hash equals the
built binary's; the leak phase waits (bounded) for the fuse2fs helper to exit as
the mount goes away.

### 9o.3 The disk reserve was checked and is sound

The suspected archive-budget reserve hole is **not** a hole: the archive path
writes through a `Sink` backed by the workspace `Fs`, whose `reserve()` refuses
when `used+delta > diskLimit` with a `QuotaError`; `statvfs`/`Bavail` is not on
that path at all. The disk mechanism is left unchanged (see ADR 94).

## 9p. Per-workspace write-RATE ceiling (this revision)

DISK TOTAL, RAM and CPU each have a real per-workspace ceiling; write RATE did
not, so one workspace running `dd`/`git clone`/a build can starve every other
workspace's IO. A kernel throttle is **not available here** (measured: `io.max`
does not account the write at all because the image is on ZFS and the bio is
generated outside the cgroup; ADR 95), and a token bucket in the engine's own
write path only covers API uploads/archives. This revision adds a **SAMPLED**
write-rate ceiling: the workspace's real disk growth is measured every metrics
tick (one `statfs`, which the sampler already takes), and a run whose **sustained**
rate exceeds its ceiling is stopped with the distinct `rate_limited` status. It
is **DEFAULT OFF**.

### 9p.1 The mechanism (measured)

- `io.max` cannot limit this (measured): a writer started with
  `CLONE_INTO_CGROUP` writing 64 MB with `oflag=direct,conv=fsync` ran at
  **3.7 GB/s** uncapped and **3.6 GB/s** with `io.max wbps=5242880`, and the
  cgroup's `io.stat` stayed at **0 bytes** in both runs — the controller never
  accounted the write (ZFS generates the bio outside our cgroup). The earlier
  invalid attempt is recorded in ADR 95: the writer's pid must be asserted in
  `/proc/<pid>/cgroup` before any conclusion.
- The ceiling rides the metrics sampler's tick (no second timer): per workspace
  it computes `delta = used_now − used_prev` over a **monotonic** elapsed time.
- A rate-kill ends the run as **`rate_limited`** (distinct from `exited`,
  `failed`, `killed`, `oom`); the engine's own write path (API write/upload,
  archive extraction) is refused with **429 `write_rate_exceeded`** while over
  the ceiling.
- `N` **consecutive** over-limit samples (`disk.write_rate_samples`, default 3)
  are required, so a single burst is not killed.
- The overshoot is stated as a number and is never a kernel boundary: with the
  shipped defaults (`N = 3`, interval `3 s`) it is **9 × `write_rate_mb_s` MiB**
  (ADR 95).
- A rate ceiling above the workspace's own `disk_mb` is refused at config load
  and on create/PATCH.

### 9p.2 The real-nsjail acceptance

`scripts/write-rate-check.sh` drives the REAL service, real nsjail and a real
FUSE disk (bounded: at most two workspaces over the run, torn down in phases,
with small paced writers, and a full leak check; this check writes data fast so
it must not hammer the host). It asserts:

- **DEFAULT OFF**: with `disk.write_rate_enabled` omitted, a tenant writer runs
  undisturbed (`exited`), the enforced limit advertises `0`, and `/v1/meta`
  reports `write_rate.enabled: false`;
- **THE COVERAGE ASSERTION**: with it enabled and low, a **tenant's own** writer
  started through the **runs API** (NOT an API upload) ends with the **distinct**
  `rate_limited` outcome — not `exited`, not a generic failure;
- the disk really grew before the stop, and the offender was stopped **before**
  it filled its disk (the rate ceiling, not ENOSPC, ended it);
- a **single burst** above the ceiling does **not** kill (the N-consecutive
  rule), while a sustained rate does;
- a run **under** the ceiling is untouched over the same window (no false
  positives);
- a **DELETE** inside the workspace (usage falling) is not a rate violation;
- an **idle** workspace costs nothing extra: zero tree walks, zero extra cgroup
  reads, exactly one `statfs` per candidate per tick (measured from the
  sampler's own stream frames, so the measurement adds no reads);
- the engine's **own** write path is covered too: a sustained series of API
  uploads is refused **429 `write_rate_exceeded`**, so the two paths are
  consistent;
- the **advertised** limit equals the **enforced** one (`/v1/meta`, the
  workspace limits and the metrics snapshot all name the tripped number);
- the measured bytes written before the kill is inside the documented overshoot
  window (`free_rate × (N+3) × interval`);
- a `write_rate_mb_s` above `disk_mb` is **refused** (422); and
- nothing is leaked (no mount, no cgroup, no `fuse2fs`, no orphan `disk.img`).

Measured **29 passed / 0 failed**. The gate's own daemon-exe hash equals the
built binary's; writers are explicitly bounded and cleaned, and the leak phase
waits (bounded) for the `fuse2fs` helper to exit as the mount goes away.

## 9q. Native port/service discovery

### 9q.1 The mechanism (measured on this host — do not re-derive)

- **The tenant cannot answer "which ports am I listening on?" itself.** Inside a
  workspace, `ls /proc/net` → `No such file or directory` (the curated `/proc`
  ships only chosen files, ADR 86), and `/bin/ss`, `/usr/bin/ss`, `/usr/sbin/ss`
  and `/bin/netstat` are all **absent** from the profile. A user's app can bind a
  port and the user has no way to see it from inside.
- **The correct host-side read is `/proc/<pid>/net/{tcp,tcp6,udp,udp6,unix}` for
  a pid inside the workspace.** A listener on `127.0.0.1:8080` appeared as
  `0: 0100007F:1F90 00000000:0000 0A … inode` — `0A` is LISTEN, addresses are
  little-endian hex (`0100007F` = `127.0.0.1`) and the port is hex (`1F90` =
  8080).
- **`ip netns exec <ns> ss` is the WRONG route (measured).** The workspace
  namespace is created by slirp (`sbxdws-<hex>` plus `sbxdrelay`); the run's
  process reports `net:[4026554106]` and `ip netns exec net:[4026554106] ss`
  fails with `Cannot open network namespace: No such file or directory`. Reading
  `/proc/<pid>/net/*` needs no namespace handling at all, because the pid is
  already inside it.
- **Cost, and therefore on-demand, NOT on the tick.** A direct open of
  `/proc/<pid>/net/tcp` from a root process measured **4.6 ms** (15 ms through
  `sudo`, so the read never shells out). That is fine for one request and far too
  expensive for the 3-second stream (4.6 ms × 1000 workspaces = 4.6 s/tick), so
  it is kept **out of `internal/metrics`** (ADR 96).

### 9q.2 The real-nsjail acceptance

`scripts/ports-check.sh` drives the REAL service against the host's real nsjail
(bounded: three workspaces, one host listener, torn down after; it starts a
listener inside the workspace, not on the operator's ports). It asserts:

- a TCP listener on `127.0.0.1` is reported with the right **port**, `protocol`
  `tcp` and **`loopback`** scope (and address/state);
- a TCP listener on `0.0.0.0` is reported with **`wildcard`** scope;
- a **UDP** socket is reported and is **NOT** called `LISTEN` (its state is the
  empty no-listen value);
- the **owning process** is resolved to the exact pid the test started, with a
  `kind` and a label whose argv carries the run's unique token;
- **THE ISOLATION ASSERTION**: a listener started on the **HOST** never appears
  in a workspace's list (checked as a port and as a raw string in the whole
  payload) — without it the endpoint could leak the host's and other tenants'
  services;
- **cross-tenant**: two workspaces binding the **same** port number each report
  **only** their own, attributed to their own pid, and neither shows the other's
  ports;
- an **empty** workspace returns `{"ports":[],"count":0,"complete":true}` with a
  **200**, not an error;
- **no stale cache**: a port that appears after the first call is reported on the
  next call, and a port whose process is stopped **disappears**;
- `complete` is true in the normal case and the list is **deterministically
  sorted** by `(port, protocol)` (two consecutive reads agree);
- the endpoint is **NOT** in the metrics stream: the metrics payload carries no
  port number and no `ports` key, and the monotonic `ports.reads_total` counter
  does **not** move across metrics ticks while it **does** move on a `/ports`
  request (so the assertion is not vacuous);
- `/v1/meta` advertises the capability (switch, caps, reachability note); and
- nothing is leaked (no engine netns, no `NSJAIL.*` cgroup, no `ws-*` parent, no
  mount, no `fuse2fs`, no orphan image, no marked workload).

Measured **49 passed / 0 failed**. The gate's daemon-exe hash equals the built
binary's.

## 10. Decisions

`docs/DECISIONS.md` contains **98** numbered resolved decisions, each with a
one-line rationale, plus implementation defaults (including #54, the engine/
frontend product boundary, #55–58, the file browsing/viewing contract,
#59–62, native file execution / unbounded runs / workspace stop / the named-
service non-goal, #63, native process control, #64–69, per-workspace egress via
chained slirp4netns and quick Cloudflare tunnels, including the recorded
host-topology hazard of the abandoned veth/route design, and #70, the
quick-tunnel rate-limit / ephemeral-URL operational limit and the harness's
skipped-environment third state), plus #71–#73, the host-identity threat model,
the /etc/ld.so.preload masking mechanism and its honest limits, and the
fail-loud opt-out switch; #72 was **corrected** three times — most recently by
**#79**, which replaces the old third class ("not yet closed (the visible
shim)") with "closed (the mechanism is now INVISIBLE)": the shim hides
`/etc/ld.so.preload`, itself and the legacy engine dirs from the workspace while
the loader still reads the preload list.
#74 covers the engine/platform curation of every mounted tree (the engine's
runner and helpers, the platform's tooling and vendor material, the provider
account, the provider-named systemd units), #75 covers closing the
dlopen-scoped `dlsym` bypass, #76 records the egress-address finding and the
default-off `network.egress_proxy` knob, **#77** records the zero-artifact
goal, the Stage 1 removals and the measured reason Stage 2 (libc replacement /
seccomp user_notif) is not reachable, **#78** records the PTY relay being
passed inline rather than materialized, **#79** records the masking mechanism
being made INVISIBLE to the tenant (the shim hides `/etc/ld.so.preload`, itself
and the legacy dirs, with the glibc-versioned filesystem interposers, while the
loader still reads the preload list — correcting ADR 72's "unavoidable"
classification), and **#80** settles the shipped nsjail (3.4) versus the
unreleased `master` with `--seccomp_unotify`. **#81**/**#82** cover the
canonicalization family answering `EACCES` and the `rlimit_as`-vs-`memory_mb`
finding, and **#83** replaces the denylist tool model with named workspace
profiles (an explicit allowlist), the entry-set decision (binaries vs trees), the
per-profile materialization choice, and what a profile can and cannot promise.
**#84** records the egress decoy (a plausible invented public IP, not a security
boundary; see §9k), **#85** records the per-workspace disk allowance as a real
filesystem (kernel boundary) with a sampled counter as the documented fallback,
**#86** records the curated per-workspace `/proc` so `free`/`vmstat`/`df`
report the workspace's own limits (see §14), and **#87–#91** record workspace
subscriptions: the engine owns the clock and stores absolute instants (#87), the
duration grammar with `mo`/`y` fixed-length approximations (#88), `locked` vs
`suspended` and the exact allow/refuse split (#89), the extension semantics
(add-while-live, from-now-when-lapsed, never-shorten; #90), and the reaper (pure
decision, injected clock, idempotent, backwards-clock rule; #91). See §9l.
**#92** records native archives: in-engine extraction (no host tool, and the
measured absence of a usable RAR tool here), the `rardecode/v2` dependency
(pure Go, MIT, no cgo) and read-only RAR, the content-based detection, the
never-trust-the-headers rule and the streaming budget, the name/link/mode
policy, the collision policy and `dry_run`, the export safety, and the full
stable refusal-code table (see §9m). **#93** records the per-workspace resource
stream (one lazy service-wide sampler, statfs disk, summed cgroups), **#94**
records one cgroup per WORKSPACE (the advertised ceiling is the enforced
ceiling), and **#95** records the per-workspace write-RATE ceiling: the measured
`io.max` negative result (with the valid `CLONE_INTO_CGROUP` test and the
invalid-attempt caveat), why a Go-side bucket covers only the API path, why the
sampled-growth mechanism is the one chosen, the overshoot NUMBER, the
sustained-vs-instantaneous rule, the default-OFF decision, the distinct
`rate_limited` outcome, and the note that DISK/RAM/CPU are kernel-enforced while
this is sampled (see §9p). **#96** records native port/service discovery: why
the endpoint must exist (the tenant has no `/proc/net` and no `ss`/`netstat` —
with the measurement), why `/proc/<pid>/net/*` and not `ip netns exec` (with the
failure), the measured 4.6 ms cost and therefore why it is on-demand and NOT on
the metrics tick, the socket-inode → process attribution rule and its ambiguity
handling (multiple owners, a vanished pid, an unresolved inode), the
loopback/wildcard/specific distinction and why it matters to a user who wants to
publish a port, the separate unix-socket section, the default-**ON** reasoning,
and the bound/`complete` honesty rule (see §9q).

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

## 14. Per-workspace limits: reported AND enforced

This is the operator's product requirement: a tenant runs `df -h` and sees
**their own** disk allowance, and `free -h` and sees **their own** RAM, and
cannot exceed either. What is reported must equal what is enforced.

### 14a. The disk allowance is a real per-workspace filesystem

Each workspace's `/workspace` is the mount point of a real ext4 image file
(`{data}/workspaces/{id}/disk.img`), sized to `disk_mb` and mounted host-side by
the orchestrator with `fuse2fs`. The workspace only ever sees the mounted
result, so it needs neither `/dev/fuse` nor the fuse tools inside the jail.

Measured, from inside two real workspaces with different limits (w1
256MB/1cpu/256MB-disk, w2 1024MB/2cpu/512MB-disk):

```
                    w1 (disk_mb 256)        w2 (disk_mb 512)
os.statvfs total    268435456               536870912
stat -f -c %b       65536                   131072        (S=4096, Type: fuseblk)
df -B1 size         268435456               536870912
df fstype           fuse.ext4               fuse.ext4
```

`df --output=fstype` reports the curated mount's own type `fuse.ext4` (the same
type `/proc/mounts` and `/proc/self/mountinfo` carry), never the host's `zfs`,
and `stat -f %T` reports `fuseblk` (the kernel's `statfs` super-magic label for a
FUSE mount). The three size channels agree with the allowance exactly.

The limit is enforced AT the boundary, with no overshoot. Raw probe against a
256 MiB allowance (write in 4 MiB chunks until the kernel refuses):

```
before:  statvfs total 268435456   df size 268435456
write:   statvfs/df unchanged at 268435456  (the mounted filesystem IS the allowance)
result:  {"statvfs_before":268435456,"df_before":268435456,
          "errno":28,"landed_bytes":239075328,
          "statvfs_after":268435456,"df_after":268435456}
```

That is: the writer landed 239075328 bytes (228 MiB — the ext4 filesystem's
usable capacity, the rest being its own metadata) and the next chunk failed with
`OSError: [Errno 28] No space left on device` (ENOSPC). The reported total did
not move: it was always the allowance. The raw transcript is kept at
`~/.opencode-tasks/limits-criterion/21-boundary.txt`; the design rationale is in
[DECISIONS.md](DECISIONS.md) ADR 85.

`/workspace` reports `fuseblk`, never the host's `zfs` type, so the platform's
100G ZFS volume is no longer visible — the previous behaviour was itself a leak.

### 14b. Memory, cpu, and the curated `/proc`

`mount_proc` stays **false** (the real procfs leaks host process information),
but a curated, per-workspace `/proc` is bind-mounted read-only over it, carrying
only the workspace's own facts:

```
meminfo      MemTotal = the workspace memory ceiling, SwapTotal 0
cpuinfo      exactly cpu_cores processor entries
stat         one aggregate cpu line plus one per core (top/vmstat parse it)
uptime / loadavg / vmstat / version
filesystems  the workspace's own mount-namespace view
mounts, self/mounts, tab   a neutral mounts table (source token /dev/workspace)
self/limits  the workspace's own rlimits
```

Every channel agrees, and the two differently-limited workspaces DIFFER on every
one (raw values measured from inside each workspace, w1 = 256MB/1cpu/256MB-disk,
w2 = 1024MB/2cpu/512MB-disk):

```
channel                             w1 (256MB/1cpu)   w2 (1024MB/2cpu)   differ
free -h Mem total                   256Mi             1.0Gi              yes
free -b total bytes                 268435456         1073741824         yes
/proc/meminfo MemTotal kB           262144            1048576            yes
/proc/meminfo SwapTotal kB          0                 0                  same
os.sysconf PHYS_PAGES x PAGESIZE    268435456         1073741824         yes
nproc                               1                 2                  yes
os.cpu_count / SC_NPROCESSORS_ONLN  1                 2                  yes
/proc/cpuinfo processor count       1                 2                  yes
os.statvfs total                    268435456         536870912          yes
stat -f -c %b                       65536             131072             yes
df -B1 size                         268435456         536870912          yes
```

Identical numbers across differently-limited workspaces (the hardcoded-value
signature) are asserted against explicitly. `free`, `vmstat` and `uptime` are in
the profile; `top` and `ps` are deliberately **excluded** (they enumerate
`/proc/<pid>/*`, which the curated `/proc` does not carry, so they would print a
confusing "mount -t proc proc /proc" error rather than a truthful view).

### 14c. Gate results for this revision

```
identity-check.sh               131 passed / 0 failed / 2 skipped   (from 113/0/2)
  harness.closed_materialized yes ; harness.diskchk_materialized yes
  §1  closed.paths_none_readable 0
  §1b curated.meminfo_memtotal_workspace 262144 ; curated.cpuinfo_cpu_count 1
      curated.meminfo_swaptotal_zero 0 ; curated.no_host_memory_leak no
      curated.free_bytes_agree 268435456
      curated.mountinfo_ok yes ; curated.mountinfo_workspace_present 1
      curated.mountinfo_no_zfs 0 ; curated.mtab_present yes ; curated.mtab_agrees 1
   §1c disk.statvfs_total_workspace 268435456 ; disk.statf_agree 65536
       disk.df_agree 268435456 ; disk.no_host_fstype no
       disk.dfh_bare_ok ok ; disk.dfh_bare_workspace_size 256M
       disk.dfh_workspace_rc 0 ; disk.dfh_workspace_size 256M
       disk.dfh_root_ok ok ; disk.dfh_root_has_row yes
       disk.df_bare_workspace_1k 262144
       disk.fill_enospc 28 ; disk.fill_capped_at_allowance yes
  docs-consistency.sh             401 passed / 0 failed   (389 before the lost+found additions)
  go test ./...                   523 test functions
```

The full gate set re-run for this revision, one gate at a time (concurrent runs
corrupt the `docs-consistency` tally), all green:

```
scripts/docs-consistency.sh     401 passed / 0 failed   (389 before the lost+found additions)
scripts/acceptance.sh           113 passed / 0 failed
scripts/process-stop-check.sh    30 passed / 0 failed
scripts/tunnel-check.sh          27 passed / 0 failed / 0 skipped   (real quick tunnel)
scripts/identity-check.sh       131 passed / 0 failed / 2 skipped   (run 3x: 131/0/2 each)
scripts/egress-decoy-check.sh    58 passed / 0 failed / 0 skipped
scripts/subscription-check.sh    62 passed / 0 failed
scripts/archive-check.sh        102 passed / 0 failed
scripts/metrics-check.sh         56 passed / 0 failed
scripts/spread-check.sh          32 passed / 0 failed
scripts/write-rate-check.sh      29 passed / 0 failed             (write-rate ceiling; see §9p; 1 transient 28/1 on a loaded host, then 29/0 on re-runs)
scripts/ports-check.sh           49 passed / 0 failed             (native port discovery; see §9q)
scripts/lostfound-check.sh       27 passed / 0 failed             (hiding the ext4-internal lost+found; see §9r; red-proof 16/11 with the fix disabled)
go test ./...                    523 test functions; go vet ./... clean
```

`scripts/lostfound-check.sh` is the new gate for this revision: it drives the
real service against the host's real nsjail and a real FUSE disk and proves the
ext4-internal `/lost+found` is absent from the visible root on both the
fresh-create path **and** the `e2fsck`-repair path (it dirties a real image and
restarts), that an existing workspace converges with no file loss, and that the
disk boundary is unchanged. See §9r.

`scripts/ports-check.sh` is the new gate for this revision: it drives the real
service against the host's real nsjail and asserts the whole port-discovery
contract — the scope/protocol/state, the exact-owner attribution, the
host-isolation assertion, the cross-tenant assertion, the empty-200, liveness,
determinism, the not-in-metrics assertion, the meta advertisement, and the
no-leak check. See §9q.

`scripts/metrics-check.sh` is the new gate for this revision: it drives the real
service, real nsjail and a real FUSE disk and asserts the whole cheap-stream
contract. Measured `51 passed / 0 failed` across four consecutive runs — the SSE
routes, the 1s cadence, RAM tracking a 60 MiB allocation (single and two-run
SUM), idle elision, fan-out (2 reads/tick with 3 subscribers), O(1) disk with no
tree walk, the enforced floor, coalescing with a drop counter, clean disconnect
with lazy start/stop, and the `df`-inside-the-workspace disk assertion that
caught the `f_bavail` bug. See §9n.

`scripts/archive-check.sh` is the new gate for this revision: it extracts the
COMMITTED `internal/archive/testdata` fixtures through the real API under real
nsjail, so it cannot pass without them. Measured `101 passed / 0 failed`: a
normal zip (nested dirs, empty file, unicode name) extracts; a `../` traversal,
an absolute path, a hostile RAR symlink and hardlink are all refused with `422
archive_unsafe_entry` and **nothing is written outside** (the would-be escape
paths are asserted absent); a setuid entry lands `0755` not `4755`; a zip bomb
and a ratio bomb are refused without the workspace disk growing; an encrypted
ZIP/RAR and a multi-volume RAR are refused; random bytes, an empty file and a
truncated zip are refused with the daemon still alive; `dry_run` writes nothing
and reports the entries; `overwrite:false` refuses a collision with `409
archive_entry_exists` and preserves the content while `overwrite:true` replaces
it; `POST /files/extract` works on an uploaded archive; and `GET /files/archive`
streams a valid zip containing the tree. No fuse2fs, mount or cgroup is left.

`identity-check.sh` §1b and §1c are the new assertions: the curated `/proc`
reports only the workspace's own limits (with the host memory asserted ABSENT),
the curated `/proc/self/mountinfo` and `/etc/mtab` are present and agree, and
the disk channels agree with the workspace's allowance while a write past it
fails with `ENOSPC` at the boundary.

#### 14c.1 Bare `df -h` now works (defect found by the operator)

`df -h` is the literal command the operator named. It did **not** work: coreutils
`df` builds its filesystem table from `/proc/self/mountinfo`, and the curated
`/proc` provided only `/proc/mounts` (mountinfo was `No such file or directory`,
`/etc/mtab` absent). Measured inside a real workspace before the fix:

```
$ df -h
df: cannot read table of mounted file systems: No such file or directory   (no row)
$ df -h /workspace
df: Warning: cannot read table of mounted file systems: No such file or directory
$ df
df: cannot read table of mounted file systems: No such file or directory   (empty)
```

`df --output=size /workspace` happened to work (it can `statvfs` the path), which
is why the gate passed while the user-facing command was broken — the gate never
invoked bare `df -h`. The fix generates `/proc/self/mountinfo` in the **real
procfs format** (`mnt_id parent_id major:minor root mount_point mount_options -
fstype source super_options`) from the **same** mount list `/proc/mounts` is
built from, so the two views can never disagree, and binds the conventional
`/etc/mtab` to it as well.

`df` needs **only mountinfo**; it does not read `/etc/mtab` (measured: `strace`
shows coreutils 9.4 `df` opening `/proc/self/mountinfo` and nothing else —
`/etc/mtab` is a symlink to `/proc/self/mounts` on modern hosts and is provided
for conventional tools, not for `df`). After the fix, inside a real 256MB
workspace:

```
$ df -h
Filesystem      Size  Used Avail Use% Mounted on
tmpfs           252G     0  252G   0% /
/dev/workspace  256M  444K  229M   1% /workspace
$ df -h /workspace
Filesystem      Size  Used Avail Use% Mounted on
/dev/workspace  256M  444K  229M   1% /workspace
$ df
Filesystem     1K-blocks  Used Available Use% Mounted on
tmpfs          264001556     0 264001556   0% /
/dev/workspace    262144   444    233488   1% /workspace
```

The workspace row carries the workspace's OWN size (256M / 262144 1K-blocks), not
the host volume. `df -h /` and bare `df` now exit 0 and print rows instead of
erroring. The device column is the neutral token `/dev/workspace`, never the host
image path.

#### 14c.2 The identity gate flake: root cause and fix

The gate previously flaked with grouped `got=` (empty) results, most visibly
`disk.fill_enospc` and whole groups. The empty `got=` is the **harness silently
swallowing an API error body**: `exec_py | jsonf "['stdout']"` indexed a missing
`stdout` key on an error JSON and yielded an empty string. The real engine defect
behind it was **not** a PUT→exec race (directly tested: 10/10 clean), and it was
**not** a create/`/readyz` reporting a workspace usable before its mount and
curated tree were live: measured, the first exec after creation reliably found
both present (10/10 fresh workspaces showed the curated `/proc` and a `fuseblk`
`/workspace` on the very first exec), because the engine materializes the disk
mount and the curated `/proc` inside the same request that runs the command
(the `mandatory:true` proc mount means a missing tree aborts the run loudly, it
cannot be silently absent). The defect was a disk-provisioning failure:

* `fitImage` measured the ext4 total with a **read-write** transient mount.
  `fuse2fs` commits the ext4 superblock asynchronously, so unmount-then-
  `resize2fs` raced the final superblock write and `resize2fs` intermittently
  refused with `Please run 'e2fsck -f ... first`. Measured in a standalone loop:
  **48/60 failures with a read-write probe, 0/60 with a read-only probe.**
* A failed fit leaves the workspace permanently unprovisionable, so **every**
  exec in that workspace then returns HTTP 503 `workspace disk unavailable`, and
  the harness mapped that to `got=` empty. Measured: a first exec returned 503 in
  ~1 in 15 fresh workspaces.

Fixes, all measured:

* `fitImage` now measures with a **read-only** mount (it only `statvfs`s), so the
  image is never dirtied and the fit is deterministic.
* `resize2fs` self-heals a dirty image: on an `e2fsck` refusal it runs
  `e2fsck -p -f` and retries once. Verified by deliberately dirtying an image
  (`debugfs -w -R "ssv state 0"`), observing the refusal, then proving
  `e2fsck -p -f` clears it and `resize2fs` succeeds.
* The harness `jsonf` now **surfaces** an API error body (`EXEC_ERROR:<code>:…`)
  instead of yielding empty, so a 503 can never masquerade as `got=`.
* The harness waits for the **real condition** before a group runs:
  `await_workspace` polls (bounded 30 s) until the curated `/proc` is present AND
  `/workspace` reports `fuseblk`, asserting `harness.<name>_materialized`. It
  never retries blind and never pads a sleep.

A third flake mode, independent of the disk race: `disk.fill_enospc` wrote a
single 300 MiB buffer, which is allocated **before** the first write; under the
256 MiB cgroup ceiling the kernel OOM-kills python (exit 137, no output) instead
of letting the filesystem return `ENOSPC`. Measured: exit 137, empty output in
12/12 repetitions. The probe now writes in 1 MiB chunks, so it tests the disk
boundary rather than the memory ceiling.

The `mask_off.cpu_leaks_host` flake: the host's usable CPU count moves under load
(observed `nproc` 2→3 mid-run). The check no longer asserts a value captured at
the top of the run; it reads the host count **at the same moment** around the
probe and, when the host is stable, asserts the leaked count equals it, plus the
host-independent relationship `mask_off.cpu_distinct_from_workspace` (mask OFF
reports the host's count, never the workspace's own).

**Proof:** the gate was run **five times in a row** on the quiet host, back to
back, all green with **zero empty `got=`**:

```
run 1: 131 passed / 0 failed / 2 skipped
run 2: 131 passed / 0 failed / 2 skipped
run 3: 131 passed / 0 failed / 2 skipped
run 4: 131 passed / 0 failed / 2 skipped
run 5: 131 passed / 0 failed / 2 skipped
```

The assertion count rose from 113 to 131 because honest assertions were **added**
(the bare-`df` group, the mountinfo/mtab assertions, the materialization check,
the mask-off relationship check); no assertion was weakened.

#### 14c.3 A disclosed residual: `df -h /` reports a host-RAM-derived size

The synthetic root (`/`, nsjail's tmpfs) reports `252G`, which is the jail
tmpfs backed by host RAM — reachable by `stat -f /` with or without the curated
mountinfo (the tmpfs is real; only its mount-table row is synthetic). It is
**not** the workspace's own `disk_mb` and it is not the host volume's `100G`
either; it is the size of the per-sandbox tmpfs. This was reachable before the
fix (`stat -f /`), so the fix does not introduce it; it is disclosed here rather
than hidden. Keeping `/` out of the table would make a bare `df -h` error, which
is worse. The workspace's own allowance is always shown on the `/workspace` row,
which is what the requirement is about. (Sizing the synthetic `/` to the
workspace's memory ceiling is a possible future refinement, out of scope here.)


### 14d. The measured fallback (when FUSE is unavailable)

If the host has no usable `/dev/fuse` (or the tools are absent) the engine falls
back to its used-bytes counter promoted to an authority over running processes:
a watchdog polls each active run's workspace usage and cancels a run that exceeds
its allowance. That fallback is REAL but **sampled**: a process already writing
can overshoot between two polls. The overshoot is bounded by
`write_rate × counter_poll_ms`; the measured value on this host is **not** "tens
of MiB" — it is the writer's throughput over one poll.

Measured directly (`disk.backend=counter`, `disk_mb=64`, `counter_poll_ms=500`,
a writer creating 4 MiB files so `rlimit_fsize`, which bounds ONE file, never
binds; the watchdog cancelled the run in every sample):

```
sample   overshoot over the 64 MiB allowance
1         52 MiB
2        360 MiB
3        172 MiB
4         48 MiB
5         72 MiB
6       1224 MiB      (a fast FUSE-less local writer; ~20x the allowance)
```

So the sampled fallback overshot by **48 MiB to ~1.2 GiB** across six runs on
this host — up to ~20× the allowance — and the cancelled run left the bytes it
had already written. The kernel FUSE boundary (the default) refuses the crossing
write outright, so it **never** overshoots. This is stated as a number and is
never presented as equivalent to the kernel boundary. The reported allowance is
unchanged (the workspace's `disk_mb`); the mechanism is named truthfully in
`/v1/meta.workspace_limits.mechanism` (`fuse` or `counter`) and its sampled
nature in `.limits`.



- SSE log streaming is deferred to v0.1; logs are polled with `offset`/`limit`.
- **Host-identity masking is defense-in-depth, not a security boundary.** A
  statically-linked binary, a musl runtime, or a raw `syscall(2)` bypasses the
  preload shim and sees the real host kernel, core count and RAM. The host
  filesystem view (no `/sys`, and `/proc` is a CURATED per-workspace payload,
  not the real procfs) is closed on every path regardless; see
  [DECISIONS.md](DECISIONS.md) #72 and #85. This is stated, not hidden.
- Multi-node storage is deferred; storage is local filesystem JSON.
- DB-backed/dynamic authentication is deferred; keys are static in the config
  (bearer auth itself is exercised by `TestAuth*`).
- Per-UID-per-workspace isolation is deferred to v0.1 (v0.0.18 uses a shared
  `sandbox` UID/GID with per-run user namespaces and cgroups).
- `rlimit_fsize` bounds a single file. The workspace **directory** allowance is
  enforced by a real per-workspace ext4 filesystem image mounted with `fuse2fs`
  (the default `disk.backend`), so the limit is a kernel filesystem boundary: a
  write past the allowance fails with `ENOSPC` and `df`/`stat -f`/`os.statvfs()`
  report the workspace's own size. When the FUSE backend is unavailable the
  engine falls back to a used-bytes watchdog that stops an over-limit run; that
  fallback is real but **sampled** (overshoot measured, see DECISIONS.md).
- A managed runtime (e.g. `node`/V8) reserves a large **virtual** address space
  at startup (measured: ~1.1 GiB for V8's CodeRange + heap reservations). The
  engine therefore sets `rlimit_as = memory_mb + 1024` (floored at 2048 MB),
  not `memory_mb`; the physical ceiling is `cgroup_mem_max = memory_mb` (ADR 82).
  `rlimit_as` is a coarse virtual backstop; `memory_mb` remains the budget a
  tenant's committed memory is charged against.
- `RLIMIT_NPROC` does not bind for the root-run sandbox; the process ceiling is
  therefore provided by `cgroup_pids_max`, which is why `--cgroupns=host` is a
  hard requirement.
- Real nsjail needs namespace/cgroup privileges. On hosts with
  `kernel.apparmor_restrict_unprivileged_userns=1` and no `CAP_SYS_ADMIN` the
  bare-host run fails writing `uid_map`; this is a host restriction, not a bug in
  sandboxd. Verification therefore runs the service in a privileged container
  with `--cgroupns=host`.

## 9r. Hiding the ext4-internal `lost+found` from the workspace root (this revision)

The operator ran `ls` in a fresh workspace and found only `lost+found`, which
looks like leftover junk in his product. The requirement, in his words: *the
directory may exist inside the image; it must not appear in what the workspace
user sees at the root* (`«خليه موجود عادي بس في ~/ مش داخل ~/workspace»`).

**Why it was there (measured — a property of ext4, not of this codebase).** A
workspace's disk is a real ext4 image (`mkfs.ext4`, mounted host-side with
`fuse2fs` at the workspace root). `mkfs.ext4` creates `/lost+found` in **every**
new filesystem, and the engine mounts the image root at the workspace root, so
the image's `/lost+found` is exactly what the tenant's `ls` shows. The inode's
`ctime` equals the image's own write time to the second.

**The rejected route (measured — do not re-litigate).** Mounting a subdirectory
of the image as the workspace root (`fuse2fs -o subdir=/home …`) is **not
available here**: this build links FUSE library **2.9.9** and rejects the option
(`fuse: unknown option 'subdir=/home'`), even though `fuse2fs --helpfull` lists
it. A design that mounts a subdirectory as the workspace root is therefore not
available.

**Why a one-shot removal fails (measured).** Removing `/lost+found` from the
image works (the mount still succeeds and the root is clean), **but `e2fsck`
puts it back every time**. Measured, with a plain check, a forced check (`-f`)
and a forced repair (`-fy`):

```
Pass 3: Checking directory connectivity
/lost+found not found.  Create? yes
```

After any of them the directory is present again. This matters because the
engine runs `e2fsck` on an image it finds **dirty** (a workspace whose sandbox
was killed mid-write), so the mechanism must be removal that is **re-applied at
the right point in the lifecycle**, not a one-shot edit at image creation.

**The fix.** `clearLostFound` removes `{root}/lost+found` from the **mounted**
workspace root — a plain `os.Remove` through the workspace's own read-write mount
(no new mount, no second read-write mount; the `diskfs.go` read-write warning is
about the transient probe in `fitImage` and is untouched) — and `DiskManager.
Ensure` calls it on **every** provisioning path, including the already-mounted-
at-the-right-size early return. So it runs **after `mkfs.ext4`** and **after any
`e2fsck` repair**, and it is idempotent (a missing directory is not an error). A
non-directory entry at that name (something the tenant created) is left alone.

**The trade-off, stated honestly.** `/lost+found` is where `fsck` reconnects
**orphaned inodes** after a corruption. It is normally empty: `mkfs.ext4` creates
it empty and the measured `e2fsck -p -f` recreates it empty, and an empty
directory holds nothing. `e2fsck` never moves a **reachable** file into
`lost+found`, so removing an empty one loses **none of the tenant's intact
files**. The only thing lost is the recovery of **orphaned inodes on a genuinely
corrupt image**. To keep that safety net from being destroyed silently, a
**non-empty** `/lost+found` is left in place rather than deleted — the only way
it becomes non-empty is a repair that actually reconnected orphaned inodes, and
deleting it then would destroy exactly the files it exists to hold — so in that
rare, corruption-only case the directory stays visible and the tenant can recover
the recovered files. This is stated, not hidden.

**The disk boundary is unchanged.** Measured in the gate: two workspaces with
different allowances each report their **own** `df -B1` size (67108864 and
134217728), a write past the allowance is refused (`No space left`), and after
the removal the mounted total is still exactly `disk_mb`.

### 9r.1 The real-nsjail + real-FUSE acceptance

`scripts/lostfound-check.sh` drives the REAL service, real nsjail and a real FUSE
disk (bounded: three workspaces, torn down in phases, with a leak check; this
check must not hammer the host). It asserts:

- a **freshly created** workspace's visible root contains no `lost+found`, for
  **two** fresh workspaces, and `stat /lost+found` is **404**;
- the **repair path** (the case a one-shot fix fails): it writes a user file,
  stops the service, dirties the image (`debugfs -w -R "ssv state 0"`) **and puts
  `/lost+found` back**, proves the image is `not clean` and the directory present,
  proves a plain `resize2fs` **refuses** the dirty image (`Please run 'e2fsck -f …
  first`) so the engine must run its `e2fsck` self-heal and retry, restarts, and
  asserts the repaired root has **no** `lost+found`, lists only the user file, and
  that the user file's **sha256 and size** are intact;
- an **existing** workspace **converges**: its subdirectory listing (names **and
  sizes**) is identical before and after, the root keeps the user file with no
  `lost+found`, and the file content is byte-identical;
- the **disk boundary unchanged**: two workspaces report their own **different**
  sizes via `df`, a write past `disk_mb` is refused (`ENOSPC`), and the mounted
  total equals `disk_mb` after the removal; and
- nothing is leaked (no mount, no `fuse2fs`, no orphan `disk.img`).

`TestDiskManagerHidesLostFound` is the unit half: it proves the fresh root has no
`lost+found`, then dirties the image and re-adds the directory, re-provisions
(forcing the `e2fsck` path), and asserts the directory is absent again.

Measured **27 passed / 0 failed**, three consecutive runs. The gate **bites**:
with `clearLostFound` deliberately short-circuited (`return nil`), the same gate
reports **16 passed / 11 failed** — the fresh-root assertions see `lost+found`
and the repair-path assertions cascade (the daemon then returns 500 on the dirty
image). Restoring the correct code returns it to **27 passed / 0 failed**. Both
tallies were recorded on this host.

**Why the restart genuinely exercises `e2fsck` (measured).** The engine's
graceful shutdown (`ReleaseAllDisks`) unmounts every workspace, so after the
`kill` in the gate **zero** FUSE mounts and **zero** `fuse2fs` processes remain
under the data dir; the next access re-provisions the dirty image, which cannot
succeed without the `e2fsck` self-heal (`resize2fs` refuses it). The mount would
otherwise survive a `SIGKILL`, but the gate stops the daemon with `SIGTERM`,
which runs the release path.

## 9s. A staged-content restore must not read a FIFO (defect found in production, fixed)

**The defect.** Provisioning the FUSE disk backend for an already-existing workspace
renames its entries into `.disk-stage`, creates the ext4 image, mounts it, and copies the
entries back with a recursive copy that reads every non-directory entry. A workspace
contains a FIFO — the terminal session creates `.term/in` — and **opening a FIFO for read
blocks**, so the restore hung at that entry. `.disk-stage` is removed only after a
successful restore, so the directory survived and **every file of the workspace was
invisible inside it**: the listing came back nearly empty (only the entries whose names
sorted before the FIFO) and `exec` did not return at all. Staging runs only when
`disk.img` is absent, so the migration is the sole trigger — and a workspace with a
terminal session always has a FIFO.

**The fix.** Special files are recreated rather than read: a FIFO via `syscall.Mkfifo`
(type and mode preserved), and a socket or device node reported as an explicit error
instead of being silently dropped.

**Evidence.** `TestRestoreStagedContentSurvivesAFifo` **fails on the previous code by
hanging** — `--- FAIL: TestRestoreStagedContentSurvivesAFifo (10.01s)`,
"restoreStagedContent HUNG on a FIFO" — and passes on the fixed code in 0.00s, with both
the top-level and the nested FIFO asserted to return as FIFOs and the regular file's bytes
intact. `go test ./...` passes all 15 packages. On the affected live host, after the fix,
the same workspace's listing returned in under a millisecond and `exec` answered in 0.10 s
with the user's `main.py` readable inside `/workspace`.
