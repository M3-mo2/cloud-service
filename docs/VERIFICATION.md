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
PASS
```

`golangci-lint` is not installed on this host, so `go vet ./...` is the lint
gate (both the Makefile and this document use `go vet`).

Test functions: 119 (117 unit + 2 `integration`-tagged), including regression
tests for the findings below (`TestRenderConfigRlimitUnits`,
`TestFormatEnvAllowlist`, `TestSandboxCwd`, `TestClassifyLaunchFailure`,
`TestRecoverRuns`, `TestCreateRunFileValidatesPath`).

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

## 7. Decisions

`docs/DECISIONS.md` contains all 34 resolved decisions, each with a one-line
rationale, plus implementation defaults.

## 8. OpenAPI

`api/openapi.yaml` validates as OpenAPI 3.1:

```
$ npx @redocly/cli lint api/openapi.yaml
Woohoo! Your API description is valid. 🎉
You have 9 warnings.          # operation-4xx-response on unauthenticated probes
```

All paths, methods, request/response schemas, and error codes are described.

## 9. Git

`git log --oneline` shows the milestone commits; `git status` is clean.

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
