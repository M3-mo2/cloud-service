# Verification

Results for sandboxd v0.0.5 against the acceptance criteria in the spec.

Environment:

- Host: Ubuntu 26.04 x86_64, kernel `7.1.8-zabbly+`, orchestrator unprivileged
  (`CapEff=0`, `kernel.apparmor_restrict_unprivileged_userns=1`).
- nsjail: real binary, `sha256 91e2049e…b680c6` (`/usr/local/bin/nsjail`).
- Go: `go1.26.8 linux/amd64`.
- Runtime container: `ubuntu:24.04`, started `--privileged --cgroupns=host`.

The transcript below was produced against **real nsjail** (not the test stub)
inside a `docker run --privileged --cgroupns=host` container. `--cgroupns=host`
is required for cgroup v2 ceilings; `scripts/acceptance.sh` refuses to run unless
`NSJAIL` points at the real binary (it rejects the stub script) and `/readyz`
reports `"cgroups":true`.

> History: an earlier revision of this document presented a non-asserting
> transcript as evidence while executions were in fact failing. `acceptance.sh`
> is now a real test (it exits non-zero on any mismatch) and the results below
> are copied from a run where every assertion passed.

> Reproducibility note (audit environment). The privileged-container half of
> this document was produced on the host described above. The audit environment
> in which the new sections were added runs as an **unprivileged** user
> (`uid=2023`, `CapEff=0000000000000000`) with **no `nsjail` binary** and no
> `docker`, so the real-nsjail acceptance transcript and the end-to-end package
> install could **not** be re-run there. Those parts are marked
> **[requires privileged container]**. Everything that *can* run unprivileged —
> `gofmt`, `go build`, `go test ./...`, the handler-level assertions and the
> sandbox-config render tests — was re-run and its output is copied below.

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

The `integration`-tagged tests invoke real nsjail and are run inside the same
privileged container against a compiled test binary:

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

Test functions: **253** (250 unit + 3 `integration`-tagged), including regression
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
`TestStatSymlink`, `TestReadRangeHonestHash`, `TestReadLines`; see §9c).

The count is the number of `^func Test` declarations across every `*_test.go`
file (the same measure `scripts/docs-consistency.sh` asserts against):

```
$ find . -name '*_test.go' -exec grep -hE '^func Test' {} + | wc -l
253
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
ok   meta.version                 0.0.5
### 3. create workspace
workspace id: ws_01M69M8Y86VCFBX9291SA7ZWHY
### 4. mkdir /src
ok   mkdir.src                    201
### 5. PUT /main.py
ok   put.main.py                  204
### 6. upload /uploaded.py (multipart)
ok   upload.status                201
### 7. read /main.py (meta)
ok   read.main.py                 0ca9091eb4e31fb1ab24c8c5de92a08e4e5f402919f82ea3ca784f38534f03f3
### 8. exec (sync) -- a successful execution
ok   exec.exit_code               0
ok   exec.stdout                  hi
### 9. cwd is honored
ok   exec.cwd                     /workspace/src
### 10. start run (file)
run id: run_01M69M8YFKZ2V4XDX5GFXSV2ZV
ok   run.status                   exited
ok   run.logs.stdout              hi
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
### 19. resource ceiling: memory limit enforced
ok   rlimit.memory                contains MEMORY_DENIED
### 20. stop long run kills process group
ok   stop.run_status              killed
ok   stop.no_survivors            0
### 21. delete workspace (expect 204) and get after delete (expect 404)
ok   delete.workspace             204
ok   get.after_delete             404

passed=30 failed=0
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

## 10. Decisions

`docs/DECISIONS.md` contains **58** numbered resolved decisions, each with a
one-line rationale, plus implementation defaults (including #54, the engine/
frontend product boundary, and #55–58, the file browsing/viewing contract).

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
- Multi-node storage is deferred; storage is local filesystem JSON.
- DB-backed/dynamic authentication is deferred; keys are static in the config
  (bearer auth itself is exercised by `TestAuth*`).
- Per-UID-per-workspace isolation is deferred to v0.1 (v0.0.5 uses a shared
  `sandbox` UID/GID with per-run user namespaces and cgroups).
- `rlimit_fsize` bounds a single file, and `disk_mb` is enforced on the file API
  path and via a `du` recheck on workspace load; it is not enforced by the
  kernel against the sandboxed process tree.
- `RLIMIT_NPROC` does not bind for the root-run sandbox; the process ceiling is
  therefore provided by `cgroup_pids_max`, which is why `--cgroupns=host` is a
  hard requirement.
- Real nsjail needs namespace/cgroup privileges. On hosts with
  `kernel.apparmor_restrict_unprivileged_userns=1` and no `CAP_SYS_ADMIN` the
  bare-host run fails writing `uid_map`; this is a host restriction, not a bug in
  sandboxd. Verification therefore runs the service in a privileged container
  with `--cgroupns=host`.
