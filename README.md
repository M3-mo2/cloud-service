# sandboxd

A secure per-principal code execution backend exposed over HTTP/JSON. End-user
accounts, per-user quota, plans and rate limiting are the separate frontend
product's job; this service authenticates only static per-principal API keys.

`sandboxd` lets clients create isolated workspaces, upload files, and run code inside
[nsjail](https://github.com/google/nsjail) with CPU/memory/disk limits and no network by default.

> Status: **v0.0.17** (pre-alpha). The service version string (`model.Version`)
> tracks the highest feature group resolved in [docs/DECISIONS.md](docs/DECISIONS.md),
> which now runs to v0.0.17; it is independent of the `/v1` API contract version.
> SSE log streaming and multi-node storage are deferred to v0.1.

## Features

- **Workspaces** — create, list, inspect and delete per-principal workspaces; mutate
  `name` and resource limits at runtime with `PATCH` (above-maximum values are
  rejected, not clamped).
- **Lifecycle** — `suspend` (refuse new work, stop running runs, keep data
  readable), `resume`, and `stop` (kill running runs, keep accepting work).
- **Files** — list, read (text/base64), write, mkdir, rename and multipart
  upload, with path-traversal/symlink defense and a per-workspace disk quota.
- **Execution** — synchronous `exec` (hard 60 s cap, capped output) and
  asynchronous `runs` (streamed logs, filterable/paginated listing, purge of
  history, stop).
- **Interactive terminal sessions** — first-class server-side sessions
  (`ses_<ULID>`) hosting a real PTY inside the workspace sandbox: create, input,
  read incremental output, resize, signal the foreground process group, and
  close, all over HTTP. Idle and absolute timeouts, a per-workspace cap, and an
  injection-proof control channel; output tails with the same `offset`/`limit`
  mechanism as run logs.
- **Usage** — disk use, run counters and effective limits via
  `GET /v1/workspaces/{id}/usage`.
- **Two-tier Python packages** — a shared read-only tier plus a private writable
  per-workspace tier, with `GET/POST/DELETE .../packages`.
- **Isolation** — no network by default (`allow_net` opts in). An `allow_net`
  workspace gets its own network namespace with outbound internet through
  **chained slirp4netns over UNIX sockets** (zero host interfaces or routes),
  isolated from the host LAN. See [docs/DECISIONS.md](docs/DECISIONS.md) #64.
- **Quick public tunnels** — one command exposes a workspace port to the public
  internet with cloudflared quick tunnels (no account, no token): `POST
  /v1/workspaces/{id}/tunnels {"port":5000}` returns the observed
  `https://…trycloudflare.com` URL. Default-deny, and the URL is never fabricated.
  Quick tunnels are **unauthenticated and rate limited** by Cloudflare: repeated
  creation can return `429`, surfaced honestly as `504 tunnel_url_unavailable`,
  and the URL is **ephemeral** (never cache it). See
  [docs/DECISIONS.md](docs/DECISIONS.md) #70.
- **Host-identity masking** — a workspace cannot identify the machine it runs on.
  No `/proc`, no `/sys`, a generic hostname, no internal DNS, and a preload shim
  (on by default, `identity.enabled`) that makes `uname`/`sysconf`/`sysinfo`
  report the **workspace's own** cpu and memory rather than the host's real size
  or a platform kernel fingerprint. The shim is reached through a read-only
  `/etc/ld.so.preload` a tenant cannot clear, both files are read-only mounts,
  and the shim also interposes `dlsym` so a handle-scoped lookup
  (`ctypes.CDLL("libc.so.6").uname`) cannot step behind it. The platform's own
  tooling is hidden too (`identity.hide_provider_tools`): `/usr/local` and
  `/etc/ssl/certs` are replaced with curated trees, so `zcp`/`zsc`/`yq`, the
  vendor strings and the provider-named certificates are not visible, while
  `python3`/`node`/`bash`/`perl`/`git` still work. The honest limits — a
  statically-linked, musl, raw-`syscall(2)` or handle-scoped-`dlvsym(3)` binary
  bypasses it — are documented, not implied away. `GET /readyz` and
  `GET /v1/meta` report `identity_masking` and `provider_tools_hidden`
  truthfully. See [docs/DECISIONS.md](docs/DECISIONS.md) #71–#76 and
  [scripts/identity-check.sh](scripts/identity-check.sh).

See [docs/API.md](docs/API.md) for the full endpoint reference.

## Quick start

```sh
cp configs/config.example.yaml config.yaml
make build
./bin/sandboxd --config config.yaml
```

Then:

```sh
curl -s localhost:8080/healthz
curl -s -H 'Authorization: Bearer dev-key-please-rotate' localhost:8080/v1/meta
```

See [docs/API.md](docs/API.md) for a full endpoint guide and curl examples, and
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for a diagrammed walkthrough of how
the service is built and how the isolation works.

## Build, test, lint

```sh
make build       # builds bin/sandboxd (rebuilds the masking shim first)
make shim        # rebuilds internal/executor/hostmask.so from hostmask.c
make test        # go test ./...
make test-race   # go test -race ./...
make lint        # golangci-lint if present, else go vet
make integration # tests that require a real nsjail binary (//go:build integration)
```

Acceptance evidence lives in [docs/VERIFICATION.md](docs/VERIFICATION.md). The
end-to-end HTTP transcript is an **asserting** test: `scripts/acceptance.sh`
compares every step against its expected value and exits non-zero on any
mismatch. It requires a server backed by real nsjail inside a container started
with `--privileged --cgroupns=host`. `make acceptance` builds the official image
and runs it.

The host suites run against real nsjail on the host (root, no container), each
with a one-line pass/fail tally: `make process-check`, `make session-check`,
`make tunnel-check` (may report environment-skips on a Cloudflare `429`) and
`make identity-check` (host-identity masking; may report an environment-skip if
the public internet is unreachable). `make docs-check` asserts the docs match
the code.

### Host-identity masking

Masking is **on by default** (`identity.enabled: true`). It is an `LD_PRELOAD`
shim reached through a read-only `/etc/ld.so.preload`, so a workspace sees its
own cpu/memory limits and a generic kernel string instead of the host's real
size and platform build. It cannot be cleared with `env -i`, the shim and
preload list are read-only bind mounts, and the shim interposes `dlsym` so a
handle-scoped `dlopen("libc.so.6")` + `dlsym` lookup returns the masked value.
Turn it off only to measure what leaks (that leak is itself asserted by
`scripts/identity-check.sh`). A binary that is statically linked, built against
musl, calls a raw `syscall(2)`, or resolves a masked symbol with `dlvsym(3)`
bypasses the shim — the honest limits are in
[docs/DECISIONS.md](docs/DECISIONS.md) #72 (corrected) / #75. Building the
service from source needs a C compiler for the shim; a plain `go build ./...`
uses the committed `internal/executor/hostmask.so`.

Hiding the platform's own tooling is also on by default
(`identity.hide_provider_tools: true`): a curated read-only `/usr/local` (empty,
or `identity.tools_dir`) and a provider-filtered `/etc/ssl/certs` replace the
host's, so the operator's CLI (`zcp`, `zsc`, `yq`) and vendor material are not
visible inside a workspace. The egress address still names the provider (it is
the platform-shared NAT) — that is structural and documented in
[docs/DECISIONS.md](docs/DECISIONS.md) #76; an optional, default-off
`network.egress_proxy` is the only lever and changes the operator's reputation
profile.

## Requirements

Required for every deployment:

- Go 1.23+
- `nsjail` (the service runs without it, but `/readyz` reports 503 and runs return
  `503 unavailable`)
- A host user and group named `sandbox` for sandboxed processes

Required only for the opt-in egress and tunnel features:

- `slirp4netns` — only for `allow_net:true` workspaces. Without it the service
  still starts, but `/readyz` reports the `network` capability as
  `available:false` and an `allow_net` workspace gets a private **empty**
  namespace (no egress), never the host network.
- `cloudflared` — only for tunnels. It must live somewhere the sandbox already
  bind-mounts **read-only**, e.g. `/usr/local/bin` (the sandbox mounts `/usr`
  read-only at the same path), so it is on `PATH` inside a workspace and a
  tenant can execute it but cannot replace it with a Trojan binary.

### Installing the optional tools

```sh
sudo apt-get install -y slirp4netns
# cloudflared is not packaged; pin a known-good release into a read-only bind:
CF=2026.10.7
sudo curl -fsSL -o /usr/local/bin/cloudflared \
  "https://github.com/cloudflare/cloudflared/releases/download/$CF/cloudflared-linux-amd64"
sudo chmod +x /usr/local/bin/cloudflared
```

### Turning egress and tunnels on

Both features are **default-deny** at the server, and each also needs a
**per-workspace opt-in**, so enabling the server block alone grants nothing:

```yaml
# config.yaml
network:            # egress (outbound internet) — default false
  enabled: true
tunnels:            # quick public tunnels — default false
  enabled: true
```

```sh
# per-workspace opt-in; both are needed, and a tunnel also needs allow_net
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"web","limits":{"allow_net":true,"allow_tunnels":true}}' \
  localhost:8080/v1/workspaces
```

### Expose a port publicly (quick start)

One API call starts `cloudflared tunnel --url` inside the workspace's own
namespace and returns the observed `https://<name>.trycloudflare.com` URL; the
URL is never fabricated. `cloudflared` also works from a shell inside the
workspace (`cloudflared tunnel --url http://localhost:5000`), which is the same
mechanism:

```sh
# start an app in the workspace, then expose its port 5000 to the internet
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"command","argv":["python3","-m","http.server","5000","--bind","0.0.0.0"],"no_timeout":true}' \
  localhost:8080/v1/workspaces/$WS/runs
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"port":5000}' localhost:8080/v1/workspaces/$WS/tunnels
# {"tunnel":{"url":"https://....trycloudflare.com", ...}}
```

Quick tunnels are **unauthenticated and rate limited** by Cloudflare: repeated
creation can return `429`, surfaced honestly as `504 tunnel_url_unavailable`,
and the URL is **ephemeral** (never cache it). See
[docs/API.md](docs/API.md#quick-tunnels-and-egress) and
[docs/DECISIONS.md](docs/DECISIONS.md) #70.

### Installing nsjail

```sh
sudo apt-get install -y libprotobuf-dev libnl-3-dev libnl-route-3-dev \
    libcap-dev pkg-config bison flex
git clone https://github.com/google/nsjail.git
cd nsjail && make
sudo cp nsjail /usr/local/bin/
```

### Creating the sandbox user

```sh
sudo groupadd --system sandbox 2>/dev/null || true
sudo useradd --system --gid sandbox --no-create-home --shell /usr/sbin/nologin sandbox 2>/dev/null || true
```

The sandbox user needs no home directory; the workspace is bind-mounted at `/workspace`.

### Host requirements for nsjail

nsjail needs to create user/mount/pid namespaces and write the cgroup v2 subtree.
Run the service either as **root**, with nsjail setuid, or inside a container
started with `--privileged` (or `--cap-add=SYS_ADMIN`) **and `--cgroupns=host`**.
`--cgroupns=host` is required: nsjail applies the memory/pids/cpu ceilings through
cgroup v2, and without it nsjail aborts before `execve`. In that case `/readyz`
reports `"cgroups":false` and every execution returns `503 unavailable` with
nsjail's diagnostic — it is never reported as a successful run.

On hosts that restrict unprivileged user namespaces
(`kernel.apparmor_restrict_unprivileged_userns=1`, common on Ubuntu 24.04) and
without `CAP_SYS_ADMIN`, nsjail fails at `uid_map`. A working local/e2e setup:

```sh
docker run --rm --privileged --cgroupns=host -p 8080:8080 \
  -v "$PWD":/src -w /src ubuntu:24.04 \
  ./bin/sandboxd --config config.yaml
```

The `Dockerfile` builds nsjail from source (it is not packaged in Ubuntu/Debian)
and is the recommended runtime image:

```sh
docker build -t sandboxd .
docker run --rm --privileged --cgroupns=host -p 8080:8080 sandboxd
```

The executor maps the sandbox identity by privilege level: as root it maps
`inside sandbox <-> outside sandbox` and chowns the workspace tree; as a
non-root orchestrator it maps `inside sandbox <-> outside orchestrator` and
leaves ownership alone.

## Architecture

```
cmd/sandboxd          entrypoint
internal/api          chi router, handlers, DTOs, middleware, error mapping
internal/service      orchestration + ownership checks
internal/executor     nsjail config builder, process lifecycle, log capture
internal/workspace    filesystem ops, path safety, quota tracking
internal/storage      JSON metadata + log storage
internal/auth         bearer middleware
internal/config       koanf loader + struct
internal/model        domain types
configs/              example config + nsjail template
api/openapi.yaml      OpenAPI 3.1 spec (source of truth)
docs/                 ARCHITECTURE, SPEC, API guide, decisions, verification
```

## License

MIT — see [LICENSE](LICENSE).
