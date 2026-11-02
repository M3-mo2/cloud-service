# Build stage: the sandboxd binary.
FROM golang:1.23-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Build the host-identity masking shim from source, so the embedded copy in the
# binary is always the committed source (the committed .so is a convenience for
# toolchains without a C compiler). It is a plain -shared object (no cgo).
RUN cc -shared -fPIC -O2 -Wall \
    -o internal/executor/hostmask.so internal/executor/hostmask/hostmask.c -ldl
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/sandboxd ./cmd/sandboxd

# Build nsjail from source: it is not packaged in Ubuntu/Debian, and the user
# namespace + cgroup v2 features this service relies on change across versions,
# so we pin a known-good release.
FROM debian:bookworm-slim AS nsjail-build
ARG NSJAIL_VERSION=3.4
RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates build-essential bison flex pkg-config \
        libprotobuf-dev protobuf-compiler libnl-3-dev libnl-route-3-dev \
        libcap-dev \
    && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch "${NSJAIL_VERSION}" \
        https://github.com/google/nsjail.git /nsjail \
    && make -C /nsjail

# Runtime stage: nsjail + python3 + cloudflared + slirp4netns, no toolchain.
FROM ubuntu:24.04 AS runtime

# cloudflared is fetched from its own release (it is not packaged). It lives in
# /usr/local/bin on the host; the engine runs it inside the workspace's network
# namespace. With identity.hide_provider_tools on (the default) the workspace's
# curated /usr/local does not expose it, so it is not on PATH inside a
# workspace. Pin a known-good version.
ARG CLOUDFLARED_VERSION=2026.10.7
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        python3 \
        slirp4netns \
        libprotobuf32t64 \
        libnl-3-200 \
        libnl-route-3-200 \
        libstdc++6 \
        zlib1g \
    && curl -fsSL -o /usr/local/bin/cloudflared \
        "https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VERSION}/cloudflared-linux-amd64" \
    && chmod +x /usr/local/bin/cloudflared \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system sandbox \
    && useradd --system --gid sandbox --no-create-home --shell /usr/sbin/nologin sandbox

COPY --from=nsjail-build /nsjail/nsjail /usr/local/bin/nsjail

WORKDIR /app
COPY --from=build /out/sandboxd /usr/local/bin/sandboxd
COPY configs/ /app/configs/
COPY api/ /app/api/

RUN mkdir -p /app/data && chown -R sandbox:sandbox /app/data

# nsjail needs privileges to create namespaces and write the cgroup v2 subtree.
# Egress uses slirp4netns over UNIX sockets and needs the host netns to create
# the relay; it changes no host interface or route. Run the container with:
#   --privileged                 (or --cap-add=SYS_ADMIN --cap-add=NET_ADMIN)
#   --cgroupns=host              (required for cgroup v2 ceilings)
# Without --cgroupns=host, nsjail cannot apply memory/pids/cpu limits and every
# execution fails; the service reports this as 503 sandbox_unavailable and
# /readyz reports cgroups=false.
ENV SANDBOXD_CONFIG=/app/config.yaml
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/sandboxd"]
CMD ["--config", "/app/config.yaml"]
