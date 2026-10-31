# Independent Review — `sandboxd` v0.0.5

**Repository:** `M3-mo2/cloud-service`
**Revision reviewed:** `dfc18ee` (`main` — "update docs")
**Review date:** 2026-10-31
**Reviewer:** external engineering review (independent reproduction)
**Deliverable type:** defect report + remediation guidance

---

## 1. Executive summary

The service is well-architected: clean layered decomposition (`api → service → {workspace, storage, executor}`),
a stubbed runner interface that makes the whole HTTP surface testable without nsjail, atomic metadata
writes, and 111 passing tests. Several isolation properties are **verified working** and are listed in §5
so they are not lost in the noise of this report.

However, **the service cannot currently be considered a secure sandbox**, and this is not a theoretical
claim. Running the documented setup, I executed code inside the "sandbox" that:

- wrote **300 MiB** to a single file under a configuration that declares a 256 MB ceiling;
- created **120 live forked processes** under a configuration that declares a 192-process ceiling;
- ran under `RLIMIT_AS` of **512 TiB** rather than the intended 512 MiB.

All three resource ceilings fail for two independent, individually correct reasons:

1. **Unit error in the nsjail config.** nsjail interprets its `rlimit_*` fields in **megabytes**;
   the renderer supplies **bytes**. The resulting limits are ~10⁶× larger than intended, i.e. effectively
   unlimited.
2. **cgroup v2 is unusable in the documented deployment mode.** nsjail aborts before `execve`, which means
   every execution fails (`exit_code 255`, no output) — *and* the cgroup ceilings (`memory`, `pids`) that
   the spec relies on are never applied.

Compounding both, `scripts/acceptance.sh` contains no assertions and never exits non-zero, so the
"verification transcript" in `docs/VERIFICATION.md` reports an all-green isolation table while every
execution in the environment it claims to cover was in fact failing outright.

**Recommended immediate action:** establish one reproducible green baseline (CI job, official Dockerfile
image, assertion-based acceptance script) before any further trust is placed in `docs/VERIFICATION.md`.

### Findings at a glance

| ID | Finding | Severity | Impact |
|----|---------|----------|--------|
| F-1 | `rlimit_as` / `rlimit_fsize` sent in bytes; nsjail expects MB | **Blocker** | Memory & file-size ceilings are effectively unlimited |
| F-2 | `cgroup v2` ceils unusable in documented deployment | **Blocker** | Every execution fails, or all cgroup ceilings are silently absent |
| F-3 | `acceptance.sh` asserts nothing | **Blocker** | The verification record is not a test |
| F-4 | `rlimit_nproc` does not constrain process count | High | Fork-bomb ceiling absent in the default mode |
| F-5 | Free-form `env` passthrough permits `LD_PRELOAD` | High | User-controlled code injection at the trust boundary |
| F-6 | Documented `cwd` parameter is ignored | Medium | Contract field with no effect |
| F-7 | Orphaned runs after restart; no startup recovery | Medium | Runs report `running` indefinitely; polling never terminal |
| F-8 | `kind=file` accepts absolute paths unvalidated | Medium | Deferred failure instead of explicit rejection |
| F-9 | `upload` returns a temporary filename | Low | Misleading API response |
| F-10 | No CI in the repository | Low | No enforced baseline for the above |

---

## 2. Review environment

| Item | Value |
|------|-------|
| OS / kernel | Ubuntu 24.04, kernel `7.1.8-zabbly+` |
| Sandbox tool | nsjail from the distribution package, `/usr/bin/nsjail` |
| Go | 1.23.4 |
| Privileges | unprivileged container user; user namespaces available (`unshare -U` succeeds) |

**Important for interpreting this report:** the reviewer environment is *not* the environment described in
`docs/VERIFICATION.md` (which specifies a `docker run --privileged --cgroupns=host` container and an nsjail
binary at `/usr/local/bin/nsjail`). Some findings below are environment-dependent and are labelled as such.
Findings F-1 (unit error) and F-3 (no assertions) are **not** environment-dependent: F-1 is decided by
nsjail's own documented argument semantics, and F-3 is a property of the script text. F-1 was additionally
confirmed by direct experiment, where the corrected unit produced the intended ceiling.

Method: build, run, exercise the full acceptance transcript against real nsjail, then reproduce every
isolation claim by invoking nsjail **directly**, outside the service, to separate defects in the Go code from
behaviour of the nsjail build in use. The repository was left unmodified (`git status` clean).

---

## 3. Findings

### F-1 — `rlimit_as` / `rlimit_fsize` use the wrong unit — **Blocker**

**Where**

- `internal/executor/nsjail.go` → `renderConfig()`:
  ```go
  FsizeBytes:  int64(lim.DiskMB) * 1024 * 1024,   // 256 MiB expressed in bytes
  AsBytes:     int64(lim.MemoryMB) * 1024 * 1024, // 512 MiB expressed in bytes
  ```
- `configs/nsjail_python.cfg.tmpl:178-179` → `rlimit_as: {{ .AsBytes }}`, `rlimit_fsize: {{ .FsizeBytes }}`

**Why it is wrong**

nsjail documents these fields as megabytes:

```
$ nsjail --help | grep -A2 rlimit_as
  --rlimit_as VALUE     RLIMIT_AS in MB, 'max' or 'hard' for the current hard limit, ...
  --rlimit_fsize VALUE  RLIMIT_FSIZE in MB, ...
```

The rendered config therefore requests `rlimit_as: 536870912` **MB** and `rlimit_fsize: 268435456` **MB**.

**Evidence — observed limits inside the jail, using the service's own rendered config**

```
RLIMIT_AS     = 562949953421312 bytes = 512 TiB   (intended: 512 MiB)
RLIMIT_FSIZE  = 281474976710656 bytes = 256 TiB   (intended: 256 MiB)
```

**Evidence — practical impact**, executed through `POST /v1/workspaces/{id}/exec`:

```
WROTE_MB= 300        # 300 MiB written successfully under a declared 256 MB ceiling
```

**Evidence — the mechanism is otherwise correct.** With the unit corrected (`rlimit_as: 100`),
the jail reports `AS = 100 MB` and a 300 MB allocation raises `MemoryError`. The defect is the unit,
not the plumbing.

**Recommendation**

1. Pass megabytes: `MemoryMB: lim.MemoryMB`, `FsizeMB: lim.DiskMB`.
2. Rename the template fields and `tmplData` members from `...Bytes` to `...MB` so the unit is
   self-documenting and a future reader cannot repeat the mistake.
3. Add a unit test that renders the template and asserts `rlimit_as <= limits.MemoryMB` (and likewise
   for `fsize`). This is cheap and would have caught the defect at build time.

**Related design note.** `rlimit_fsize` bounds a *single file*, not a tree. Tree-level accounting
(`disk_mb`) is enforced only on the file API path (`internal/workspace/fs.go`), so code running inside the
sandbox can currently write to its workspace without quota enforcement. `docs/ARCHITECTURE.md` §12
acknowledges the scope, but the practical consequence — `disk_mb` is not enforced from inside the sandbox —
deserves to be stated explicitly, especially in combination with F-1 where even the per-file ceiling is inert.

---

### F-2 — cgroup v2 unusable in the documented deployment mode — **Blocker**

**Where**

- `configs/nsjail_python.cfg.tmpl:22` → `use_cgroupv2: true`
- `configs/nsjail_python.cfg.tmpl:185-187` → `cgroup_mem_max`, `cgroup_pids_max`, `cgroup_cpu_ms_per_sec`
- `Dockerfile` / `README.md` — do not state that `--cgroupns=host` is required

**Observed behaviour**

```
[E] enableCgroupSubtree():101 Could not apply '+memory' to cgroup.subtree_control in '/sys/fs/cgroup'
[E] main():380 Couldn't setup parent cgroup (cgroupv2)
```

nsjail aborts during setup, before `execve`. Every execution through the service returns:

```json
{"exit_code":255, "stdout":"", "stderr":"", "truncated":false}
```

Note the empty `stderr`: the failure is completely silent at the API surface.

**Consequence for the resource model.** All three cgroup ceilings are absent in this mode while the spec
treats them as active (`docs/SPEC.md:122`: *"cgroups: memory = limits.memory_mb, pids = max_runs * 64,
cpu derived from cpu_cores"*). Verified absent: a 1 GB allocation succeeded, 120 concurrent children
survived, and no CPU throttling was observable.

**This is not a defective configuration — it is an undeclared requirement.** The project's own transcript
shows the cgroup path working when the container is started with `--cgroupns=host`: their run records
`FORK_LIMITED: BlockingIOError`. The gap is that the default, documented deployment cannot reach that state,
and nothing tells the operator.

**Recommendation** (choose one and make it explicit):

1. **Declare the requirement and enforce it.** Add `--cgroupns=host` to every `docker run` in `README.md`
   and to the documented E2E procedure. Prefer `--cap-add=SYS_ADMIN` with `--cap-drop=ALL` over
   `--privileged` where the kernel permits it.
2. **Or make cgroups optional by design**, and state plainly that only `rlimit_*` applies.
3. **Regardless of the choice:** never surface a sandbox failure as a silent `255`. Classify non-zero
   nsjail exits, return an explicit error (`503 sandbox_unavailable` with the nsjail diagnostic), and log
   nsjail's `stderr` — its absence is what allowed F-3 to mask this failure for an entire release.

---

### F-3 — `scripts/acceptance.sh` contains no assertions — **Blocker**

**Observation.** The script is a sequence of `echo` and `curl` calls. It contains no comparison of expected
against actual output, no failure counter, and no non-zero exit on failure. The only conditional in the file
(lines 25-27) checks that `NSJAIL` is not a shell script.

**Consequence.** `docs/VERIFICATION.md` §3 presents the script's output as a transcript and §5 draws
conclusions from it: *"network egress fails, `/etc/shadow` is unreachable, `mount(2)`/`ptrace` are killed by
seccomp, the fork bomb is stopped by the cgroup pids limit"*. Running the same script here produced
byte-identical *shape* for steps 20-24 while every single execution had failed with `exit_code 255` and no
output at all. The `255` results were read as evidence of containment; they were evidence of total failure.
An empty `stdout` is indistinguishable from a blocked syscall unless something asserts on the content.

**Risk.** Any reader — including a future maintainer or an external security reviewer — is presently led to
accept a security property that does not hold in the environment the document describes.

**Recommendation.** Convert it to an actual test and let CI run it:

```sh
fail=0
check() { # label actual expected
  [ "$2" = "$3" ] || { printf 'FAIL %s: got %s want %s\n' "$1" "$2" "$3"; fail=1; }
}
check "traversal-read"  "$(curl -s -o /dev/null -w '%{http_code}' ...)" 422
check "net-blocked"     "$(printf '%s' "$OUT" | grep -c NET_BLOCKED)"   1
check "seccomp-kill"    "$(printf '%s' "$OUT" | jq -r .exit_code)"      159
exit $fail
```

Then regenerate the table in `docs/VERIFICATION.md` **from the official Dockerfile image**, not from a
hand-built container, and record the image digest and nsjail version alongside the results.

---

### F-4 — `rlimit_nproc` does not constrain process count — **High**

**Where.** `configs/nsjail_python.cfg.tmpl:181` → `rlimit_nproc: {{ .PidsMax }}`
(`PidsMax = limits.MaxRuns * 64`, i.e. 192 by default, per `internal/executor/nsjail.go`).

**Evidence.**

```
NPROC = (-1, -1)      # unlimited, inside the jail
FORKED_ALIVE 120      # 120 live children, with rlimit_nproc declared as 192
```

`RLIMIT_NPROC` is enforced against the *real* UID's process count and is not applied to `root`, which is the
documented operating mode. In the project's own transcript the fork bomb *is* stopped — but by
`cgroup_pids_max` (`FORK_LIMITED: BlockingIOError`), which F-2 shows is unavailable by default. The two
mechanisms therefore do not overlap where it matters, and the combined result in the default configuration is
no process ceiling at all.

**Recommendation.**

1. Do not rely on `rlimit_nproc` as the pids control. Make `cgroup_pids_max` mandatory (it demonstrably works,
   including for children, per the project's own run), **or**
2. Run the jailed process as a non-root user with a genuine privilege drop, and only then rely on
   `RLIMIT_NPROC`.
3. Add an acceptance assertion: 300 attempted live forks must be refused, not succeed. Assert on the count,
   not on the absence of a crash.

---

### F-5 — Free-form `env` passthrough permits `LD_PRELOAD` — **High**

**Where.** `internal/executor/nsjail.go` → `formatEnv()` / `sanitizeEnvKey()` validate the *characters* of the
key but impose no allowlist on the *name*. `configs/nsjail_python.cfg.tmpl:199` emits every user variable into
the jail.

**Evidence.** Supplying `LD_PRELOAD=/workspace/evil.so` (the `.so` placed in the workspace) via
`POST /exec {"env": {...}}` produced, inside the sandbox:

```
  >>> LD_PRELOAD EXECUTED INSIDE SANDBOX <<<
env user vars: {'LD_PRELOAD': '/workspace/evil.so', 'PYTHONPATH': '/workspace'}
```

**Assessment — stated precisely.** This is *not* a sandbox escape: the caller already controls the executed
code and gains no privilege beyond what that code has. The concern is structural:

1. It contradicts the trust model the design is built on — "the jail contains hostile code" — by allowing the
   caller to load native code into any process inside the jail, including the interpreter the service chose.
2. It undermines service-mediated guarantees. `exec_bin.path` is validated and documented as the entry point;
   a preloaded object can alter that binary's behaviour before `main`, so the record of *what ran* is no
   longer authoritative.
3. It is a building block, not an endpoint. Combined with F-1 and F-4 (no effective memory or process limits)
   the in-jail attack surface is materially larger than the documentation implies.

**Recommendation.** Replace passthrough with an allowlist: `PYTHONPATH`, `PYTHONUNBUFFERED`,
`PYTHONDONTWRITEBYTECODE`, `LANG`, `TZ`, and reject `LD_*`, `DYLD_*`, `GLIBC_TUNABLES` by name. Record the
decision (and its rationale) in `docs/DECISIONS.md`.

---

### F-6 — The documented `cwd` parameter has no effect — **Medium**

**Where.** Documented in `api/openapi.yaml:616` and `docs/SPEC.md:77`; threaded through
`internal/service/exec.go` and populated into the template data (`internal/executor/nsjail.go:417`).
The template, however, hardcodes the working directory:

```
configs/nsjail_python.cfg.tmpl:155 →  cwd: "/workspace"
```

No `{{ .Cwd }}` reference exists, so the value is silently discarded. Verified: `"cwd":"/src"` returned
`exit_code 255` (the F-2 failure) — i.e. it never influenced execution in any configuration tested.

**Recommendation.** Either emit `cwd: "{{ .Cwd }}"` with validation that the path stays inside `/workspace`,
or drop `cwd` from the contract and specify that the working directory is fixed at `/workspace`. A field that
exists in the API, the spec and the transport layer but not on the wire is a trap for the next implementer.

---

### F-7 — No startup recovery; runs can be orphaned and never reach a terminal state — **Medium**

**Observations.**

1. `cmd/sandboxd/main.go` handles `SIGTERM`/`SIGINT` with an HTTP shutdown only. It does not call
   `service.StopAllRuns` or `service.WaitAll` (both exist in `internal/service/runs.go:164,204` and are used
   only by workspace deletion and tests). Processes left in their own process group survive a restart with no
   remaining handle to them.
2. There is no reconciliation on startup: a run whose `meta.json` says `running` stays `running` forever.
   Observed: a long-running run recorded as `exited` immediately after restart and its `stop` call returned
   the stale status with the same `exit_code 255` from the failed launch — i.e. an unrecoverable, misleading
   record for the client polling it.
3. `StopRun` returns `run.Status` read from disk rather than the state it just drove, so a caller cannot
   distinguish "signalled a live process" from "the record was already stale".

**Recommendation.**

- On startup, sweep `runs/*/meta.json` and move any `queued`/`running` entry to a terminal
  `interrupted`/`failed` state.
- On shutdown, `StopAllRuns` then `WaitAll` under a bounded deadline.
- Return the post-signal status from `StopRun`.
- Record the recovery policy in `docs/DECISIONS.md`.

---

### F-8 — `kind=file` accepts absolute paths without validation — **Medium**

**Where.** `internal/service/runs.go:36`

```go
file := strings.TrimPrefix(spec.File, "/")
argv = append([]string{"python3", file}, spec.Args...)
```

**Evidence.** `{"kind":"file","file":"/etc/passwd"}` is accepted, produces
`argv: ["python3","etc/passwd"]`, and returns `202` with `status: "queued"`. There is no traversal here in
practice — the sandbox CWD is `/workspace` and the relativised name has no separator — but the API reports
success for a request it should reject, and defers the failure to execution time (or, worse, to a future
change in `argv` construction).

**Recommendation.** Validate through `workspace.Resolve` and return `422` for `IsUnsafe` / `404` for
`IsNotFound` at request time. Apply the same treatment to `cwd` if it is retained (F-6).

---

### F-9 — `upload` returns a temporary filename — **Low**

`POST .../files/upload` responded `{"name":"tmp.aYWeQKK0R6", ...}`. The multipart part's filename is
available as `header.Filename` at `internal/api/files.go:226` and is evidently unused in practice.

**Recommendation.** Return the client-supplied filename, or drop the field from the response DTO.

---

### F-10 — No CI — **Low** (but the enabling condition for F-3)

There is no `.github/` directory and no pipeline definition. Consequently nothing enforces the test suite,
the race detector, formatting, or the acceptance transcript on change. Given F-3, this is the structural
reason the reported baseline could diverge from reality without anyone noticing.

**Recommendation.** Add one workflow: `go vet`, `gofmt -l`, `go test -race ./...`, `docker build`, then the
(assertion-based) acceptance script inside the official image.

---

## 4. Documentation corrections required

| Document | Location | Issue |
|----------|----------|-------|
| `docs/VERIFICATION.md` | §1 | `go test ./... -race → exit 0` is not robust across environments; one failure observed (`KillProcs`/`Blocking`) caused by the F-2 abort, not by the assertion. Record the exact image and nsjail build with the result. |
| `docs/VERIFICATION.md` | §3, steps 20-24 | The isolation transcript is only meaningful on a run where executions actually succeed. As written it is reproducible with a total launch failure and identical values. |
| `docs/VERIFICATION.md` | §5 | `processes before stop: 486` is inconsistent with F-2 under a failed cgroup setup. |
| `docs/SPEC.md` | 122 | "cgroups: memory = `limits.memory_mb`, pids = `max_runs * 64` …" does not hold in the documented deployment (F-2). |
| `docs/ARCHITECTURE.md` | §5.1 | The "What nsjail enforces" table lists rlimits and cgroup ceilings as effective. Add a column stating in which execution path each is actually applied, and record F-1. |
| `docs/ARCHITECTURE.md` | §10 | "**Dynamic/DB authentication** — keys are static in config" is correct but reads as if authentication itself is unverified; note explicitly that bearer auth is tested (it is: `TestAuth*`). |
| `docs/ARCHITECTURE.md` | §12 | The note that disk is "bounded by `rlimit_fsize` per file" should also state that `rlimit_fsize` itself was mis-scaled until F-1 is fixed. |
| `README.md`, `Dockerfile` | — | Do not state that `--cgroupns=host` is required, nor that a non-root jailed user is required for `RLIMIT_NPROC` to bind. |

---

## 5. Verified working (do not regress)

Each item below was confirmed by direct experiment **in the same session** in which the findings above were
observed, and is therefore a positive result rather than an absence of testing.

| # | Property | Evidence |
|---|----------|----------|
| 1 | Network isolation | `NET_BLOCKED: OSError` inside the jail |
| 2 | `/etc/shadow` not reachable | `SHADOW_DENIED: FileNotFoundError` (not mounted) |
| 3 | seccomp kills `mount(2)`/`ptrace` | `exit_code: 159` (SIGSYS) on a completed execution |
| 4 | PID namespace + `mount_proc: false` | host process table not visible |
| 5 | Minimal mount set | `/usr`, `/lib`, `/lib64`, `/bin`, `/sbin`, selected `/etc`, three `/dev` nodes |
| 6 | Path safety | `422` for traversal read, write, delete, and symlink escape |
| 7 | Authentication and ownership | `401` without a token, `403` across principals |
| 8 | Body caps | `413` for oversized JSON and multipart |
| 9 | Process-group termination | `status: killed`, no surviving PID verified on the host |
| 10 | Atomic metadata writes | `fsync` + `rename` in `writeJSONAtomic` |
| 11 | Unit suite | `go vet` clean, `go test ./...` and `go test -race ./...` green (111 test functions) |
| 12 | Layering | `api → service → {workspace, storage, executor}`; runner behind a stub-able interface |

---

## 6. Recommended sequencing

1. **One green baseline** (F-10 + F-3). An assertion-based acceptance job running inside the official image.
   Regenerate `docs/VERIFICATION.md` from it, and annotate every claim with the image digest and nsjail build.
2. **Unit correctness** (F-1): megabytes, not bytes; renamed fields; a rendered-config unit test.
3. **Isolation truth** (F-2): declare and test `--cgroupns=host`, or document that cgroups are not applied —
   and make every sandbox launch failure explicit to the caller.
4. **Process ceiling and env allowlist** (F-4, F-5).
5. **Lifecycle hardening** (F-7): startup recovery and bounded shutdown drain.
6. **Contract cleanup** (F-6, F-8, F-9) and the documentation corrections in §4.

---

## 7. Reproduction notes

Commands used, in order:

```sh
# Build and static checks
go build ./...            # clean
go vet ./...              # clean
go test ./...             # 111 test functions, pass
go test -race ./...       # pass

# Run the service against real nsjail
go build -o bin/sandboxd ./cmd/sandboxd
./bin/sandboxd --config config.yaml
curl -s localhost:8099/readyz        # {"data_dir":true,"nsjail":true,"status":"ready"}

# Full transcript (prints; does not assert)
BASE=http://127.0.0.1:8099 KEY=... OTHER_KEY=... \
DATA_DIR=$PWD/data NSJAIL=/usr/bin/nsjail bash scripts/acceptance.sh

# In-jail limit introspection
curl -s -H "$K" -H 'Content-Type: application/json' \
  -d '{"cmd":["python3","lim.py"]}' "$B/v1/workspaces/$WS/exec"
#   lim.py prints resource.getrlimit for AS / NPROC / FSIZE / NOFILE / CPU

# Confirming F-1's mechanism in isolation: same config, unit corrected
#   rlimit_as: 100  →  AS = 100 MB  →  MemoryError on a 300 MB allocation
```

All temporary changes made during the review were reverted; the working tree was left clean.
Failure modes were reproduced both through the service and by invoking nsjail directly with the rendered
config, so each finding is attributed to either the Go code or to the nsjail build in use.
