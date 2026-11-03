BINARY := sandboxd
PKG := ./...
GO ?= go
CC ?= cc

.PHONY: all build shim test test-race lint fmt vet run clean tidy integration acceptance session-check process-check tunnel-check identity-check egress-decoy-check subscription-check docs-check

all: fmt vet shim build test

# shim rebuilds the host-identity masking shared object from source. The .so is
# embedded in the sandboxd binary, so it must exist before `go build`. A plain
# `go build ./...` uses the committed .so (built from the same source), so a
# toolchain without a C compiler can still build the service.
#
# It is stripped and its .comment section removed at build time: a tenant can
# `strings` the shim, so it must carry no engine name, no compiler-version
# .comment line and no stale source path. `objcopy -R .comment` is required
# because the `.comment` section survives `strip` (measured).
#
# The version script (hostmask.map) is required: glibc VERSIONS its symbols, so
# the filesystem-hiding interposers must be exported under the version names
# their callers reference (stat64@GLIBC_2.33 etc.). Without it an unversioned
# definition does not satisfy a versioned reference and the hiding silently
# misses python's os.stat and bash's test -f (measured).
shim:
	$(CC) -shared -fPIC -O2 -Wall -s -fno-ident -Wl,-soname,rt.so -Wl,--version-script=internal/executor/hostmask/hostmask.map -o internal/executor/hostmask.so internal/executor/hostmask/hostmask.c -ldl
	objcopy -R .comment internal/executor/hostmask.so

build: shim
	$(GO) build -o bin/$(BINARY) ./cmd/sandboxd

test:
	$(GO) test $(PKG)

test-race:
	$(GO) test -race $(PKG)

integration:
	$(GO) test -tags integration $(PKG)

# Acceptance builds the official image and runs the assertion-based end-to-end
# transcript against a server backed by real nsjail. cgroup v2 ceilings require
# --cgroupns=host, and nsjail requires --privileged.
acceptance:
	docker build -t sandboxd:acceptance .
	docker run --rm --privileged --cgroupns=host -v "$(PWD)/scripts:/scripts:ro" \
		sandboxd:acceptance bash -lc '\
		useradd --system --no-create-home --shell /usr/sbin/nologin sandbox 2>/dev/null || true; \
		printf "%s\n" \
		"server: { addr: \"127.0.0.1:8099\" }" \
		"storage: { data_dir: \"/data\" }" \
		"sandbox: { nsjail_path: \"/usr/local/bin/nsjail\", user: \"sandbox\", group: \"sandbox\", kill_grace_sec: 2 }" \
		"limits: { cpu_cores: 1, memory_mb: 512, disk_mb: 1024, max_wall_time_sec: 60, max_runs: 3 }" \
		"max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true, allow_unlimited_runs: true }" \
		"keys:" \
		"  - { key: \"dev-key-please-rotate\", principal_id: \"user_dev\" }" \
		"  - { key: \"other-key\", principal_id: \"user_other\" }" \
		> /app/config.yaml; \
		sandboxd --config /app/config.yaml >/tmp/sandboxd.log 2>&1 & \
		sleep 2; \
		BASE=http://127.0.0.1:8099 KEY=dev-key-please-rotate OTHER_KEY=other-key DATA_DIR=/data NSJAIL=/usr/local/bin/nsjail \
		bash /scripts/acceptance.sh'

# Session acceptance against REAL nsjail on the host. Unlike `acceptance` this
# needs no container: it needs root (nsjail uses cgroup v2 and mount namespaces)
# and the real nsjail binary at /usr/local/bin/nsjail. It hosts the service
# itself and asserts the whole terminal-session contract, including a forced PTY
# allocation failure. The script prints SKIP and exits 0 when root, nsjail or
# the Go toolchain is absent.
session-check:
	sudo -E bash scripts/session-nsjail-check.sh

# Native process-control acceptance against REAL nsjail on the host (needs root
# and the real nsjail binary; no container). It asserts the process inventory,
# the cgroup-scoped stop (including a setsid()-escaping workload), the workspace
# stop leaving zero processes, isolation, no cgroup growth, and orphan reaping
# after a forced daemon death + restart.
process-check:
	sudo -E bash scripts/process-stop-check.sh

# Per-workspace egress and quick-tunnel acceptance against REAL nsjail,
# slirp4netns and cloudflared on the host (needs root, no container). It asserts
# DNS + fetch + pip install over the chained-slirp4netns egress, that the
# workspace cannot reach the host LAN, that a quick tunnel returns a real
# trycloudflare.com URL and that zero host interfaces/routes/namespaces are
# left behind. Cloudflare's quick-tunnel provisioning is unauthenticated and
# rate limited: on a 429 the tunnel-dependent assertions are reported as
# skipped-environment and the script still exits 0, so a run may legitimately
# report skips for that external reason (never a silent pass).
tunnel-check:
	sudo -E bash scripts/tunnel-check.sh

# Host-identity acceptance against REAL nsjail on the host (needs root, no
# container). It asserts that the filesystem view is closed (the "already
# closed" paths are unreadable), that hostname/DNS reveal nothing, that
# allow_net:false reaches no "what is my IP" service, that with masking on the
# reported cpu count and memory match the WORKSPACE's limits (not the host's)
# and the kernel string no longer identifies the platform, that with masking off
# the leak is documented, that the measured egress IP is a platform-shared NAT
# address, and that the host topology is untouched.
identity-check:
	sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin" bash scripts/identity-check.sh

# Egress-decoy acceptance against REAL nsjail, slirp4netns and nft on the host
# (needs root, no container). It asserts, from inside a real workspace with the
# decoy ON, that every covered channel (HTTP echo services, an invented hostname,
# all four DNS echo methods, DoH/DoT/direct resolver, all client runtimes) answers
# a consistent invented identity and NOT the real address nor a platform name,
# with no certificate warning, and that pip/github/npm/pypi still work. The layer
# is DEFAULT OFF; this target turns it on only for the check.
egress-decoy-check:
	sudo -n env "PATH=$PATH:/usr/local/go/bin:/usr/local/bin:/usr/sbin" bash scripts/egress-decoy-check.sh

# Subscription acceptance against REAL nsjail and a REAL FUSE disk on the host
# (needs root, no container). It compresses the clock via config
# (grace_period=4s, reap_interval=1s) and asserts the whole term lifecycle in
# seconds: create-with-duration, additive renewal while live, lock-at-expiry
# with real teardown (process stopped, disk unmounted), renewal-to-unlock,
# delete-after-grace with the data/disk.img/metadata gone and nothing leaked,
# manual delete of a locked workspace, an untouched no-subscription workspace,
# the backwards-clock rule, and tick idempotency.
subscription-check:
	sudo -E bash scripts/subscription-check.sh

# Documentation-consistency gate. It derives its assertions from the code
# (router, error constants, test count) rather than the prose, so it fails when
# the docs stop matching the implementation.
docs-check:
	bash scripts/docs-consistency.sh

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not found; running go vet"; \
		$(GO) vet $(PKG); \
	fi

fmt:
	$(GO) fmt $(PKG)
	gofmt -s -w .

vet:
	$(GO) vet $(PKG)

tidy:
	$(GO) mod tidy

run: build
	./bin/$(BINARY) --config config.yaml

clean:
	rm -rf bin data/tmp
