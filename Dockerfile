# Build stage
FROM golang:1.23-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/sandboxd ./cmd/sandboxd

# Runtime stage: Ubuntu + nsjail + python3.
FROM ubuntu:24.04 AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        python3 \
        nsjail \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system sandbox \
    && useradd --system --gid sandbox --no-create-home --shell /usr/sbin/nologin sandbox

WORKDIR /app
COPY --from=build /out/sandboxd /usr/local/bin/sandboxd
COPY configs/ /app/configs/
COPY api/ /app/api/

RUN mkdir -p /app/data && chown -R sandbox:sandbox /app/data

# nsjail needs privileges to create namespaces; run the container with
# --cap-add=SYS_ADMIN (or --privileged) and an unchanged seccomp profile.
ENV SANDBOXD_CONFIG=/app/config.yaml
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/sandboxd"]
CMD ["--config", "/app/config.yaml"]
