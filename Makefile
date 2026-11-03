BINARY := sandboxd
PKG := ./...
GO ?= go
CC ?= cc

.PHONY: all build shim test test-race lint fmt vet run clean tidy integration acceptance session-check process-check spread-check tunnel-check identity-check egress-decoy-check subscription-check archive-check metrics-check write-rate-check ports-check lostfound-check docs-check

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

# Per-WORKSPACE resource-ceiling acceptance against REAL nsjail and a REAL FUSE
# disk on the host (needs root, no container). It asserts the mechanism that
# makes a workspace a server-shaped thing with ITS OWN quota: one parent cgroup
# per workspace with the delegated controllers, a run's leaf cgroup nested
# inside it and zero NSJAIL.* at the root, the SUM across three concurrent runs
# bounded by the advertised memory_mb (a single 2x memory_mb allocation is
# OOM-killed and reads as `oom`, never a clean `exited`), ram_used <= ram_limit
# with ram_percent <= 100, the setsid()-escape kill path still working, the
# parent cgroup removed on delete, and the disk budget still the workspace's own
# allowance.
spread-check:
	sudo -E bash scripts/spread-check.sh

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

# Native archive acceptance against REAL nsjail and a REAL FUSE disk on the host
# (needs root, no container). It extracts the COMMITTED
# internal/archive/testdata fixtures through the real API and asserts the whole
# security boundary: a normal zip extracts, a traversal / absolute name is
# refused with nothing written outside, a setuid bit is stripped, a zip bomb and
# a ratio bomb are refused without the disk growing, an encrypted ZIP/RAR and a
# multi-volume RAR are refused, a hostile RAR symlink/hardlink escape is refused,
# random bytes / an empty file / a truncated zip are refused with the daemon
# alive, dry_run writes nothing, the overwrite collision policy holds, and a zip
# export is a valid archive. It reads fixtures only from the committed tree, so
# it cannot pass without them.
archive-check:
	sudo -E bash scripts/archive-check.sh

# Per-workspace resource-stream acceptance against REAL nsjail and a REAL FUSE
# disk on the host (needs root, no container). It asserts the whole cheap-stream
# contract: both SSE routes return text/event-stream and deliver a frame
# promptly, frames arrive on the configured cadence with monotonic timestamps,
# the RAM figure tracks a real allocation and is the SUM over the workspace's
# per-run and per-session cgroups (not one cgroup), an idle workspace costs zero
# cgroup reads, the per-tick reads do not grow with the subscriber count, there
# is no tree walk (disk is one statfs), the interval floor is enforced, a stalled
# subscriber is coalesced (drop counter moves), disconnects are clean with a
# lazy start/stop, and the reported disk equals `df -B1 --output=used /workspace`
# run INSIDE the workspace. Every wait is bounded and at most two workspaces run
# concurrently (this check is about resource cost, so it must not itself hammer
# the host).
metrics-check:
	sudo -E bash scripts/metrics-check.sh

# Per-workspace write-RATE ceiling acceptance against REAL nsjail and a REAL
# FUSE disk on the host (needs root, no container). It asserts the SAMPLED
# ceiling that stops a workspace starving others: DEFAULT OFF changes nothing;
# a tenant's OWN writer (a runs-API process, NOT an API upload) trips a
# distinct rate_limited outcome; a single burst does not kill; a run under the
# ceiling is untouched; a DELETE is not a violation; idle costs nothing; the
# engine's own write path is refused with 429 write_rate_exceeded too; the
# advertised limit equals the enforced one; the overshoot is inside the
# documented window; and nothing is leaked.
write-rate-check:
	sudo -E bash scripts/write-rate-check.sh

# Native port/service discovery acceptance against REAL nsjail on the host
# (needs root, no container). It asserts the whole contract of GET
# /v1/workspaces/{id}/ports: a loopback and a wildcard TCP listener and a UDP
# socket are reported with the right port/protocol/scope/state, the owning
# process is resolved to the pid the test started, a HOST port never leaks into
# a workspace's list (the isolation assertion), two workspaces see only their
# own ports, an empty workspace is an honest empty 200, the list is live (not
# cached) and deterministically sorted, the endpoint is NOT part of the metrics
# stream (the read counter does not move on a tick), /v1/meta advertises it, and
# nothing is leaked.
ports-check:
	sudo -E bash scripts/ports-check.sh

# lost+found-hiding acceptance against REAL nsjail and a REAL FUSE disk on the
# host (needs root, no container). It asserts the ext4-internal /lost+found is
# absent from a workspace's visible root on BOTH the fresh-create path AND the
# e2fsck-repair path (it dirties a real workspace image, restarts the service,
# and asserts the directory is gone AFTER the repair — the case a one-shot
# removal at image creation fails), that an existing workspace converges with no
# file loss, and that the disk boundary is unchanged (two workspaces report their
# own different disk_mb and a write past the allowance still fails ENOSPC).
lostfound-check:
	sudo -E bash scripts/lostfound-check.sh

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
