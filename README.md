# sandboxd

A secure per-user code execution backend exposed over HTTP/JSON.

`sandboxd` lets clients create isolated workspaces, upload files, and run code inside
[nsjail](https://github.com/google/nsjail) with CPU/memory/disk limits and no network by default.

> Status: **v0.0.5** (pre-alpha). SSE log streaming and multi-node storage are deferred to v0.1.

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
make build       # builds bin/sandboxd
make test        # go test ./...
make test-race   # go test -race ./...
make lint        # golangci-lint if present, else go vet
make integration # tests that require a real nsjail binary (//go:build integration)
```

Acceptance evidence lives in [docs/VERIFICATION.md](docs/VERIFICATION.md). The
end-to-end HTTP transcript can be reproduced with `scripts/acceptance.sh`.

## Requirements

- Go 1.23+
- `nsjail` (the service runs without it, but `/readyz` reports 503 and runs return
  `503 unavailable`)
- A host user and group named `sandbox` for sandboxed processes

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

nsjail needs to create user/mount/pid namespaces, mount cgroup v2, and chown the
workspace. Run the service either as **root**, with nsjail setuid, or inside a
privileged container. On hosts that restrict unprivileged user namespaces
(`kernel.apparmor_restrict_unprivileged_userns=1`, common on Ubuntu 24.04) and
without `CAP_SYS_ADMIN`, nsjail fails at `uid_map`. A working local/e2e setup:

```sh
docker run --rm --privileged --cgroupns=host -p 8080:8080 \
  -v "$PWD":/src -w /src ubuntu:24.04 \
  ./bin/sandboxd --config config.yaml
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
