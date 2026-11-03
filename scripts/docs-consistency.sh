#!/usr/bin/env bash
#
# docs-consistency.sh — assert that the documentation matches the implemented
# service. It is the acceptance gate for the documentation-completion task:
# every assertion below is derived from the code, never from the prose.
#
# Each check prints "ok <name>" or "FAIL <name>: <detail>"; the script exits
# non-zero if any check fails and always prints a pass/fail tally.
#
# Usage: scripts/docs-consistency.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); printf 'ok   %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf 'FAIL %s: %s\n' "$1" "$2"; }

# --- sources of truth -------------------------------------------------------

# Every route registered in the chi router, as "<METHOD> <path>" with the
# router-nested prefixes reconstructed. `r.Route("x", ...)` pushes x and the
# matching `})` pops it; leaf routes are emitted as METHOD and full path. The
# awk blocks are brace-trimmed to the literal go-chi call shapes in
# internal/api/router.go.
routes() {
  awk '
  {
    line=$0; sub(/#.*/,"",line)
  }
  line ~ /r\.Route\("([^"]+)"/ {
    match(line, /r\.Route\("[^"]+"/)
    s=substr(line, RSTART+9, RLENGTH-9)
    depth++; stack[depth]=s; next
  }
  line ~ /^[[:space:]]*\}\)/ { if (depth>0) { delete stack[depth]; depth-- }; next }
  match(line, /r\.(Get|Post|Put|Patch|Delete)\("[^"]*"/) {
    ms=substr(line, RSTART, RLENGTH)
    match(ms, /r\.[A-Za-z]+/); method=substr(ms, 3, RLENGTH-2)
    sub(/^r\.[A-Za-z]+\("/, "", ms); sub(/"$/, "", ms)
    base=""; for(i=1;i<=depth;i++){ base=base stack[i] }
    if (ms=="/") p=base; else p=base ms
    gsub(/"/,"",p)
    if (p=="") p="/"
    print method, p
  }
  ' internal/api/router.go
}

# Every error-code constant defined in internal/model/errors.go, as the code
# string (the value, not the Go identifier).
error_codes() {
  grep -oE 'Code[A-Za-z]+ += "[a-z_]+"' internal/model/errors.go |
    sed -E 's/.*= "([a-z_]+)"/\1/'
}

# Total test functions across every *_test.go file.
test_count() {
  find . -name '*_test.go' -exec grep -hE '^func Test' {} + | wc -l | tr -d ' '
}

ROUTES="$(routes)"
CODES="$(error_codes)"
TESTS="$(test_count)"

# --- 1. every route appears in docs/API.md ---------------------------------

while IFS=' ' read -r method path; do
  [ -n "$method" ] || continue
  method="$(printf '%s' "$method" | tr '[:lower:]' '[:upper:]')"
  if ! grep -qF -- "$method $path" docs/API.md; then
    fail "api.md.route.$method.$path" "not in docs/API.md"
  else
    ok "api.md.route $method $path"
  fi
done <<< "$ROUTES"

# --- 2. every error code appears in the docs/API.md error table ------------

while IFS= read -r code; do
  [ -n "$code" ] || continue
  if ! grep -qE "\`$code\`" docs/API.md; then
    fail "api.md.code.$code" "not in docs/API.md"
  else
    ok "api.md.code $code"
  fi
done <<< "$CODES"

# --- 3. every new endpoint path appears in docs/SPEC.md ---------------------

NEW_PATHS=(
  "/v1/workspaces/{id}/usage"
  "/v1/workspaces/{id}/packages"
  "/v1/workspaces/{id}/packages/install"
  "/v1/workspaces/{id}/suspend"
  "/v1/workspaces/{id}/resume"
  "/v1/workspaces/{id}/stop"
  "/v1/workspaces/{id}/sessions"
  "/v1/workspaces/{id}/sessions/{sessionID}"
  "/v1/workspaces/{id}/sessions/{sessionID}/input"
  "/v1/workspaces/{id}/sessions/{sessionID}/signal"
  "/v1/workspaces/{id}/sessions/{sessionID}/resize"
  "/v1/workspaces/{id}/sessions/{sessionID}/output"
  "/v1/workspaces/{id}/processes"
  "/v1/workspaces/{id}/processes/{processID}/stop"
  "/v1/workspaces/{id}/tunnels"
  "/v1/workspaces/{id}/tunnels/{tunnelID}"
  "/v1/metrics"
  "/v1/metrics/stream"
  "/v1/workspaces/{id}/metrics"
  "/v1/workspaces/{id}/metrics/stream"
  "/v1/workspaces/{id}/ports"
)
for p in "${NEW_PATHS[@]}"; do
  # SPEC.md uses the {id} spelling; accept the {workspaceID} spelling too.
  variant="$(printf '%s' "$p" | sed 's/{id}/{workspaceID}/')"
  if grep -qF -- "$p" docs/SPEC.md || grep -qF -- "$variant" docs/SPEC.md; then
    ok "spec.md.path $p"
  else
    fail "spec.md.path.$p" "endpoint path not documented in docs/SPEC.md"
  fi
done

# SPEC.md must mention the new features explicitly.
for term in suspend resume usage package run session process; do
  if grep -qi "$term" docs/SPEC.md; then
    ok "spec.md.term $term"
  else
    fail "spec.md.term.$term" "'$term' absent from docs/SPEC.md"
  fi
done

# --- 4. VERIFICATION.md has a packages section and a resolver section -------

if grep -qiE '^#+ .*package' docs/VERIFICATION.md; then
  ok "verification.packages-section"
else
  fail "verification.packages-section" "no packages section in docs/VERIFICATION.md"
fi
if grep -qiE 'resolv|name resolution' docs/VERIFICATION.md; then
  ok "verification.resolver-section"
else
  fail "verification.resolver-section" "no resolver-fix section in docs/VERIFICATION.md"
fi
if grep -qiE '^#+ .*session' docs/VERIFICATION.md; then
  ok "verification.sessions-section"
else
  fail "verification.sessions-section" "no sessions section in docs/VERIFICATION.md"
fi

# --- 4b. the privileged session acceptance harness exists and is documented
# The real-nsjail session harness is the only check that catches a dead PTY
# where the unit tests stay green, so it must exist, be executable, and be named
# in VERIFICATION.md -- it cannot silently rot.

if [ -f scripts/session-nsjail-check.sh ]; then
  ok "session-harness.exists"
else
  fail "session-harness.exists" "scripts/session-nsjail-check.sh is missing"
fi
if [ -x scripts/session-nsjail-check.sh ]; then
  ok "session-harness.executable"
else
  fail "session-harness.executable" "scripts/session-nsjail-check.sh is not executable"
fi
if grep -qF 'session-nsjail-check.sh' docs/VERIFICATION.md; then
  ok "session-harness.documented"
else
  fail "session-harness.documented" "docs/VERIFICATION.md does not mention scripts/session-nsjail-check.sh"
fi

# --- 4c. the native process-control acceptance harness exists and is documented
# The real-nsjail process-stop harness is the only check that catches a jailed,
# setsid()-escaped process surviving a process-group kill, so it must exist, be
# executable, and be named in VERIFICATION.md -- it cannot silently rot.

if [ -f scripts/process-stop-check.sh ]; then
  ok "process-harness.exists"
else
  fail "process-harness.exists" "scripts/process-stop-check.sh is missing"
fi
if [ -x scripts/process-stop-check.sh ]; then
  ok "process-harness.executable"
else
  fail "process-harness.executable" "scripts/process-stop-check.sh is not executable"
fi
if grep -qF 'process-stop-check.sh' docs/VERIFICATION.md; then
  ok "process-harness.documented"
else
  fail "process-harness.documented" "docs/VERIFICATION.md does not mention scripts/process-stop-check.sh"
fi

# --- 4d. the egress/tunnel acceptance harness exists and is documented -------
# tunnel-check.sh is the only check that measures the live chained-slirp4netns
# egress and a real cloudflared quick tunnel, and that the host topology is left
# untouched, so it must exist, be executable, and be named in VERIFICATION.md.
# It may legitimately report skipped-environment assertions on a Cloudflare 429.

if [ -f scripts/tunnel-check.sh ]; then
  ok "tunnel-harness.exists"
else
  fail "tunnel-harness.exists" "scripts/tunnel-check.sh is missing"
fi
if [ -x scripts/tunnel-check.sh ]; then
  ok "tunnel-harness.executable"
else
  fail "tunnel-harness.executable" "scripts/tunnel-check.sh is not executable"
fi
if grep -qF 'tunnel-check.sh' docs/VERIFICATION.md; then
  ok "tunnel-harness.documented"
else
  fail "tunnel-harness.documented" "docs/VERIFICATION.md does not mention scripts/tunnel-check.sh"
fi

# --- 4d-bis. the host-identity acceptance harness exists and is documented --
# identity-check.sh is the only check that measures the syscall-reachable host
# facts a mount namespace cannot hide (uname/sysconf/sysinfo) and that the
# masking switch really masks, so it must exist, be executable, and be named in
# VERIFICATION.md. It may legitimately report an environment-skip for the
# egress-IP assertion when the public internet is unreachable.

if [ -f scripts/identity-check.sh ]; then
  ok "identity-harness.exists"
else
  fail "identity-harness.exists" "scripts/identity-check.sh is missing"
fi
if [ -x scripts/identity-check.sh ]; then
  ok "identity-harness.executable"
else
  fail "identity-harness.executable" "scripts/identity-check.sh is not executable"
fi
if grep -qF 'identity-check.sh' docs/VERIFICATION.md; then
  ok "identity-harness.documented"
else
  fail "identity-harness.documented" "docs/VERIFICATION.md does not mention scripts/identity-check.sh"
fi

# --- 4d-ter. the egress-decoy acceptance harness exists and is documented ----
# egress-decoy-check.sh is the only check that measures, from a real workspace
# with the decoy ON, that every covered channel answers the invented identity and
# that ordinary internet still works, so it must exist, be executable, and be
# named in VERIFICATION.md and wired into the Makefile.

if [ -f scripts/egress-decoy-check.sh ]; then
  ok "egress-decoy-harness.exists"
else
  fail "egress-decoy-harness.exists" "scripts/egress-decoy-check.sh is missing"
fi
if [ -x scripts/egress-decoy-check.sh ]; then
  ok "egress-decoy-harness.executable"
else
  fail "egress-decoy-harness.executable" "scripts/egress-decoy-check.sh is not executable"
fi
if grep -qF 'egress-decoy-check.sh' docs/VERIFICATION.md; then
  ok "egress-decoy-harness.documented"
else
  fail "egress-decoy-harness.documented" "docs/VERIFICATION.md does not mention scripts/egress-decoy-check.sh"
fi
if grep -qE '^egress-decoy-check:' Makefile; then
  ok "makefile.target.egress-decoy-check"
else
  fail "makefile.target.egress-decoy-check" "Makefile has no 'egress-decoy-check:' target"
fi
if grep -qE '^\.PHONY:.*\begress-decoy-check\b' Makefile; then
  ok "makefile.phony.egress-decoy-check"
else
  fail "makefile.phony.egress-decoy-check" "'egress-decoy-check' is not in .PHONY"
fi

# --- 4d-quater. the subscription acceptance harness exists and is documented ---
# subscription-check.sh is the only check that drives the REAL reaper against a
# real sandbox and a real FUSE disk, so it must exist, be executable, be named in
# VERIFICATION.md and be wired into the Makefile -- it cannot silently rot.

if [ -f scripts/subscription-check.sh ]; then
  ok "subscription-harness.exists"
else
  fail "subscription-harness.exists" "scripts/subscription-check.sh is missing"
fi
if [ -x scripts/subscription-check.sh ]; then
  ok "subscription-harness.executable"
else
  fail "subscription-harness.executable" "scripts/subscription-check.sh is not executable"
fi
if grep -qF 'subscription-check.sh' docs/VERIFICATION.md; then
  ok "subscription-harness.documented"
else
  fail "subscription-harness.documented" "docs/VERIFICATION.md does not mention scripts/subscription-check.sh"
fi
if grep -qE '^subscription-check:' Makefile; then
  ok "makefile.target.subscription-check"
else
  fail "makefile.target.subscription-check" "Makefile has no 'subscription-check:' target"
fi
if grep -qE 'bash scripts/subscription-check.sh' Makefile; then
  ok "makefile.sudo.subscription-check"
else
  fail "makefile.sudo.subscription-check" "Makefile's 'subscription-check' target does not run scripts/subscription-check.sh"
fi
if grep -qE '^\.PHONY:.*\bsubscription-check\b' Makefile; then
  ok "makefile.phony.subscription-check"
else
  fail "makefile.phony.subscription-check" "'subscription-check' is not in .PHONY"
fi

# The subscription feature (this iteration): the config section, the ADRs, the
# locked-vs-suspended distinction, the allow/refuse split, the duration
# approximation caveat, the absolute-instant recovery model and the
# backwards-clock rule must all be documented and wired, so they cannot silently
# regress out of the docs.
if grep -qF 'subscription:' configs/config.example.yaml && grep -qF 'Subscription SubscriptionConfig' internal/config/config.go; then
  ok "subscription.config"
else
  fail "subscription.config" "the subscription section is missing from configs/config.example.yaml or internal/config/config.go"
fi
if grep -qF 'default_duration: ""' configs/config.example.yaml; then
  ok "subscription.config-default-empty"
else
  fail "subscription.config-default-empty" "configs/config.example.yaml must show default_duration: \"\" (no expiry by default)"
fi
if grep -qF 'ADR 87' docs/DECISIONS.md && grep -qF 'ADR 88' docs/DECISIONS.md; then
  ok "subscription.adrs"
else
  fail "subscription.adrs" "docs/DECISIONS.md is missing ADR 87 (subscription model) or ADR 88 (duration grammar)"
fi
if grep -qi 'locked' docs/DECISIONS.md && grep -qi 'suspended' docs/DECISIONS.md && grep -qi 'subscription_expired' docs/DECISIONS.md; then
  ok "subscription.locked-vs-suspended"
else
  fail "subscription.locked-vs-suspended" "the locked-vs-suspended distinction / subscription_expired code is not in docs/DECISIONS.md"
fi
if grep -qiF 'FIXED-LENGTH' docs/DECISIONS.md && grep -qiE '30.?(day|d)|365' docs/DECISIONS.md; then
  ok "subscription.duration-approximation"
else
  fail "subscription.duration-approximation" "the mo=30d / y=365d approximation caveat is not in docs/DECISIONS.md"
fi
if grep -qiF 'backwards' docs/DECISIONS.md && grep -qiF 'absolute' docs/DECISIONS.md; then
  ok "subscription.backwards-clock-and-absolute-instants"
else
  fail "subscription.backwards-clock-and-absolute-instants" "the backwards-clock rule or the absolute-instant recovery model is not in docs/DECISIONS.md"
fi
if grep -qF 'subscription' api/openapi.yaml && grep -qF 'subscription' docs/API.md; then
  ok "subscription.documented"
else
  fail "subscription.documented" "subscription is not documented in docs/API.md and api/openapi.yaml"
fi
if grep -qiF 'expiring_soon' docs/API.md && grep -qiF 'remaining_sec' docs/API.md; then
  ok "subscription.summary-documented"
else
  fail "subscription.summary-documented" "the summary states (expiring_soon/remaining_sec) are not documented in docs/API.md"
fi
# The pure decision function and the parser must exist in the subscription pkg.
if [ -f internal/subscription/subscription.go ] && grep -qF 'func Decide(' internal/subscription/subscription.go && grep -qF 'func ParseDuration(' internal/subscription/subscription.go; then
  ok "subscription.pure-functions"
else
  fail "subscription.pure-functions" "internal/subscription is missing the pure Decide or ParseDuration function"
fi

# --- 4d-quinquies. the archive acceptance harness exists and is documented -----
# archive-check.sh is the only check that drives the REAL extraction boundary
# against real nsjail and a real FUSE disk, so it must exist, be executable, be
# named in VERIFICATION.md and be wired into the Makefile -- it cannot silently
# rot. It reads fixtures only from the COMMITTED internal/archive/testdata tree.

if [ -f scripts/archive-check.sh ]; then
  ok "archive-harness.exists"
else
  fail "archive-harness.exists" "scripts/archive-check.sh is missing"
fi
if [ -x scripts/archive-check.sh ]; then
  ok "archive-harness.executable"
else
  fail "archive-harness.executable" "scripts/archive-check.sh is not executable"
fi
if grep -qF 'archive-check.sh' docs/VERIFICATION.md; then
  ok "archive-harness.documented"
else
  fail "archive-harness.documented" "docs/VERIFICATION.md does not mention scripts/archive-check.sh"
fi
if grep -qE '^archive-check:' Makefile; then
  ok "makefile.target.archive-check"
else
  fail "makefile.target.archive-check" "Makefile has no 'archive-check:' target"
fi
if grep -qE 'bash scripts/archive-check.sh' Makefile; then
  ok "makefile.sudo.archive-check"
else
  fail "makefile.sudo.archive-check" "Makefile's 'archive-check' target does not run scripts/archive-check.sh"
fi
if grep -qE '^\.PHONY:.*\barchive-check\b' Makefile; then
  ok "makefile.phony.archive-check"
else
  fail "makefile.phony.archive-check" "'archive-check' is not in .PHONY"
fi

# The archive feature (this iteration): the config section, the ADR, the
# in-engine extraction, the streaming budget, the read-only-RAR asymmetry, the
# committed fixtures, the export and the meta capability must all be documented
# and wired, so they cannot silently regress.
if grep -qF 'archives:' configs/config.example.yaml && grep -qF 'ArchivesConfig struct' internal/config/config.go; then
  ok "archive.config"
else
  fail "archive.config" "the archives section is missing from configs/config.example.yaml or internal/config/config.go"
fi
if grep -qF 'max_total_mb: 2048' configs/config.example.yaml && grep -qF 'formats: ["zip", "rar"]' configs/config.example.yaml; then
  ok "archive.config-defaults"
else
  fail "archive.config-defaults" "configs/config.example.yaml must show the real archive defaults (max_total_mb: 2048, formats: [\"zip\", \"rar\"])"
fi
if grep -qF 'ADR 92' docs/DECISIONS.md; then
  ok "archive.adr"
else
  fail "archive.adr" "docs/DECISIONS.md is missing ADR 92 (native archives)"
fi
if grep -qiF 'rardecode' docs/DECISIONS.md && grep -qiF 'read-only' docs/DECISIONS.md; then
  ok "archive.adr-rardecode-readonly"
else
  fail "archive.adr-rardecode-readonly" "ADR 92 must record the rardecode/v2 dependency and that RAR is read-only"
fi
if grep -qiF 'streaming budget' docs/DECISIONS.md; then
  ok "archive.adr-streaming-budget"
else
  fail "archive.adr-streaming-budget" "ADR 92 must record the streaming budget (validate/write before trusting headers)"
fi
if grep -qiF 'rar3-' docs/DECISIONS.md; then
  ok "archive.adr-rar3-naming-note"
else
  fail "archive.adr-rar3-naming-note" "ADR 92 must note the rar3-* / RAR4 naming (RAR3.x == RAR4 generation)"
fi
if [ -d internal/archive/testdata/zip ] && [ -f internal/archive/testdata/rar4/subdirs.rar ] && [ -f internal/archive/testdata/rar5/subdirs.rar ]; then
  ok "archive.fixtures-committed"
else
  fail "archive.fixtures-committed" "the committed internal/archive/testdata fixtures are missing"
fi
if grep -qF 'internal/archive/testdata' scripts/archive-check.sh && ! grep -qF '/tmp/fixtures' scripts/archive-check.sh; then
  ok "archive.harness-reads-committed-fixtures"
else
  fail "archive.harness-reads-committed-fixtures" "scripts/archive-check.sh must read fixtures from internal/archive/testdata, never /tmp"
fi
if grep -qi 'archives' internal/api/router.go && grep -qF 'refusal_codes' internal/api/router.go; then
  ok "archive.meta-advertised"
else
  fail "archive.meta-advertised" "GET /v1/meta does not advertise the archives capability"
fi
if grep -qF 'POST /v1/workspaces/{workspaceID}/files/archive' docs/API.md && grep -qF 'GET /v1/workspaces/{workspaceID}/files/archive' docs/API.md && grep -qF 'POST /v1/workspaces/{workspaceID}/files/extract' docs/API.md; then
  ok "archive.routes-documented"
else
  fail "archive.routes-documented" "the three archive routes are not all documented in docs/API.md"
fi
if grep -qF 'files/archive' api/openapi.yaml && grep -qF 'files/extract' api/openapi.yaml && grep -qF 'ArchiveResult' api/openapi.yaml; then
  ok "archive.openapi"
else
  fail "archive.openapi" "the archive routes/schema are missing from api/openapi.yaml"
fi
if grep -qF 'archive.BuildMarker' cmd/sandboxd/main.go && grep -qF 'BuildMarker' internal/archive/format.go; then
  ok "archive.build-marker"
else
  fail "archive.build-marker" "the archive build marker is not logged at startup / defined"
fi

# --- 4d-sexies. the per-workspace metrics harness exists and is documented -----
# metrics-check.sh is the only check that drives the REAL cheap-stream contract
# against real nsjail and a real FUSE disk (RAM as the SUM over per-run/per-
# session cgroups, idle elision, fan-out, no tree walk, the interval floor,
# coalescing, clean disconnect), so it must exist, be executable, be named in
# VERIFICATION.md and be wired into the Makefile -- it cannot silently rot.

if [ -f scripts/metrics-check.sh ]; then
  ok "metrics-harness.exists"
else
  fail "metrics-harness.exists" "scripts/metrics-check.sh is missing"
fi
if [ -x scripts/metrics-check.sh ]; then
  ok "metrics-harness.executable"
else
  fail "metrics-harness.executable" "scripts/metrics-check.sh is not executable"
fi
if grep -qF 'metrics-check.sh' docs/VERIFICATION.md; then
  ok "metrics-harness.documented"
else
  fail "metrics-harness.documented" "docs/VERIFICATION.md does not mention scripts/metrics-check.sh"
fi
if grep -qE '^metrics-check:' Makefile; then
  ok "makefile.target.metrics-check"
else
  fail "makefile.target.metrics-check" "Makefile has no 'metrics-check:' target"
fi
if grep -qE 'bash scripts/metrics-check.sh' Makefile; then
  ok "makefile.sudo.metrics-check"
else
  fail "makefile.sudo.metrics-check" "Makefile's 'metrics-check' target does not run scripts/metrics-check.sh"
fi
if grep -qE '^\.PHONY:.*\bmetrics-check\b' Makefile; then
  ok "makefile.phony.metrics-check"
else
  fail "makefile.phony.metrics-check" "'metrics-check' is not in .PHONY"
fi

# The metrics feature (this iteration): the config section, the ADR, the service-
# wide lazy sampler, the SSE surface and the disk-reporting fix must all be
# documented and wired, so they cannot silently regress.
if grep -qF 'metrics:' configs/config.example.yaml && grep -qF 'MetricsConfig struct' internal/config/config.go; then
  ok "metrics.config"
else
  fail "metrics.config" "the metrics section is missing from configs/config.example.yaml or internal/config/config.go"
fi
if grep -qF 'ADR 93' docs/DECISIONS.md; then
  ok "metrics.adr"
else
  fail "metrics.adr" "docs/DECISIONS.md is missing ADR 93 (per-workspace resource stream)"
fi
if grep -qiF 'statfs' docs/DECISIONS.md && grep -qiE '82|tree walk' docs/DECISIONS.md; then
  ok "metrics.adr-statfs-not-walk"
else
  fail "metrics.adr-statfs-not-walk" "ADR 93 must record statfs (O(1)) vs the tree walk (82x, per-file cost)"
fi
if grep -qiF 'SUM' docs/DECISIONS.md && grep -qiF 'per-run' docs/DECISIONS.md; then
  ok "metrics.adr-sum-cgroups"
else
  fail "metrics.adr-sum-cgroups" "ADR 93 must record that RAM is the SUM over per-run/per-session cgroups"
fi
if grep -qiF 'f_bfree' internal/workspace/statfs_linux.go && grep -qiF 'f_bfree' docs/DECISIONS.md; then
  ok "metrics.disk-bfree-fix"
else
  fail "metrics.disk-bfree-fix" "MountedDiskUsed must use f_bfree and ADR 93 must record the fix"
fi
if grep -qF 'internal/metrics' internal/service/metrics.go && grep -qF 'text/event-stream' internal/api/metrics_stream.go; then
  ok "metrics.service-and-sse"
else
  fail "metrics.service-and-sse" "the metrics package/service wiring or the SSE content type is missing"
fi
if grep -qF 'GET /v1/metrics/stream' docs/API.md && grep -qF 'GET /v1/workspaces/{workspaceID}/metrics/stream' docs/API.md; then
  ok "metrics.routes-documented"
else
  fail "metrics.routes-documented" "the metrics stream routes are not documented in docs/API.md"
fi
if grep -qF '/v1/metrics' api/openapi.yaml && grep -qF '/v1/workspaces/{workspaceId}/metrics/stream' api/openapi.yaml; then
  ok "metrics.openapi"
else
  fail "metrics.openapi" "the metrics routes are missing from api/openapi.yaml"
fi
if grep -qF 'metrics' internal/api/router.go && grep -qF 'metricsCap' internal/api/router.go; then
  ok "metrics.meta-advertised"
else
  fail "metrics.meta-advertised" "GET /v1/meta does not advertise the metrics capability"
fi

# --- 4d-septies. the per-WORKSPACE ceiling mechanism and its harness ----------
# The workspace's RAM/CPU/pids ceiling must be per WORKSPACE (one parent cgroup,
# limits on the parent, nsjail pointed at it), not per run. This block pins the
# mechanism, the ADR, the new `oom` run status, the dropped contradictory limit
# field, and the spread-check harness -- so none can silently regress.
if grep -qF 'ADR 94' docs/DECISIONS.md; then
  ok "ceiling.adr"
else
  fail "ceiling.adr" "docs/DECISIONS.md is missing ADR 94 (per-workspace resource ceiling)"
fi
if grep -qF 'WorkspaceCgroupPrefix' internal/executor/cgroup.go && \
   grep -qF 'cgroupv2_mount' configs/nsjail_python.cfg.tmpl && \
   grep -qF 'cgroupv2_mount' internal/executor/nsjail.go; then
  ok "ceiling.parent-cgroup"
else
  fail "ceiling.parent-cgroup" "the per-workspace parent cgroup / cgroupv2_mount mechanism is missing"
fi
if grep -qF 'EnsureWorkspaceCgroup' internal/executor/cgroup.go && \
   grep -qF 'RemoveWorkspaceCgroup' internal/executor/cgroup.go; then
  ok "ceiling.parent-lifecycle"
else
  fail "ceiling.parent-lifecycle" "the workspace parent cgroup lifecycle (create/remove) is missing"
fi
if grep -qE 'RunOOM[[:space:]]+RunStatus = "oom"' internal/model/types.go && \
   grep -qF 'RunOOM' internal/service/runs.go; then
  ok "ceiling.oom-status"
else
  fail "ceiling.oom-status" "the distinct `oom` run status is missing from the model or the run classifier"
fi
if grep -qF 'oom' api/openapi.yaml && grep -qF 'interrupted, oom' api/openapi.yaml; then
  ok "ceiling.oom-documented"
else
  fail "ceiling.oom-documented" "the `oom` run status is not in the openapi run-status enum"
fi
if ! grep -qF 'ram_cgroup_limit_bytes' internal/metrics/metrics.go; then
  ok "ceiling.no-contradictory-limit"
else
  fail "ceiling.no-contradictory-limit" "ram_cgroup_limit_bytes (the contradictory second limit) is still present"
fi
if grep -qF 'ram_limit_bytes' docs/API.md && grep -qiF 'enforced' docs/API.md; then
  ok "ceiling.ram-limit-documented"
else
  fail "ceiling.ram-limit-documented" "ram_limit_bytes is not documented as the ENFORCED ceiling in docs/API.md"
fi
if [ -f scripts/spread-check.sh ]; then
  ok "ceiling-harness.exists"
else
  fail "ceiling-harness.exists" "scripts/spread-check.sh is missing"
fi
if [ -x scripts/spread-check.sh ]; then
  ok "ceiling-harness.executable"
else
  fail "ceiling-harness.executable" "scripts/spread-check.sh is not executable"
fi
if grep -qF 'spread-check.sh' docs/VERIFICATION.md; then
  ok "ceiling-harness.documented"
else
  fail "ceiling-harness.documented" "docs/VERIFICATION.md does not mention scripts/spread-check.sh"
fi
if grep -qE '^spread-check:' Makefile; then
  ok "makefile.target.spread-check"
else
  fail "makefile.target.spread-check" "Makefile has no 'spread-check:' target"
fi
if grep -qE 'bash scripts/spread-check.sh' Makefile; then
  ok "makefile.sudo.spread-check"
else
  fail "makefile.sudo.spread-check" "Makefile's 'spread-check' target does not run scripts/spread-check.sh"
fi
if grep -qE '^\.PHONY:.*\bspread-check\b' Makefile; then
  ok "makefile.phony.spread-check"
else
  fail "makefile.phony.spread-check" "'spread-check' is not in .PHONY"
fi

# --- 4d-octies. the write-RATE ceiling and its harness ------------------------
# The per-workspace write-RATE ceiling is SAMPLED and default OFF. It must be
# documented (the measured io.max negative result, the coverage claim, the
# overshoot number, the sustained-vs-instantaneous rule) and its artifact and
# harness must not silently regress out of the tree.
if [ -f scripts/write-rate-check.sh ]; then
  ok "write-rate-harness.exists"
else
  fail "write-rate-harness.exists" "scripts/write-rate-check.sh is missing"
fi
if [ -x scripts/write-rate-check.sh ]; then
  ok "write-rate-harness.executable"
else
  fail "write-rate-harness.executable" "scripts/write-rate-check.sh is not executable"
fi
if grep -qF 'write-rate-check.sh' docs/VERIFICATION.md; then
  ok "write-rate-harness.documented"
else
  fail "write-rate-harness.documented" "docs/VERIFICATION.md does not mention scripts/write-rate-check.sh"
fi
if grep -qE '^write-rate-check:' Makefile; then
  ok "makefile.target.write-rate-check"
else
  fail "makefile.target.write-rate-check" "Makefile has no 'write-rate-check:' target"
fi
if grep -qE 'bash scripts/write-rate-check.sh' Makefile; then
  ok "makefile.sudo.write-rate-check"
else
  fail "makefile.sudo.write-rate-check" "Makefile's 'write-rate-check' target does not run scripts/write-rate-check.sh"
fi
if grep -qE '^\.PHONY:.*\bwrite-rate-check\b' Makefile; then
  ok "makefile.phony.write-rate-check"
else
  fail "makefile.phony.write-rate-check" "'write-rate-check' is not in .PHONY"
fi
# The feature: the config keys, the ADR, the distinct status, the pure
# arithmetic and the sampled (never kernel) framing must all be documented.
if grep -qF 'write_rate_enabled' internal/config/config.go && grep -qF 'write_rate_mb_s' internal/config/config.go; then
  ok "write-rate.config"
else
  fail "write-rate.config" "the write-rate config keys are missing from internal/config/config.go"
fi
if grep -qF 'ADR 95' docs/DECISIONS.md; then
  ok "write-rate.adr"
else
  fail "write-rate.adr" "docs/DECISIONS.md is missing ADR 95 (write-rate ceiling)"
fi
if grep -qF 'RunRateLimited' internal/model/types.go && grep -qF 'rate_limited' docs/DECISIONS.md; then
  ok "write-rate.status"
else
  fail "write-rate.status" "the distinct rate_limited run status is missing from the model or ADR 95"
fi
if grep -qF 'advanceRate' internal/service/write_rate.go; then
  ok "write-rate.arithmetic"
else
  fail "write-rate.arithmetic" "the pure advanceRate arithmetic is missing from internal/service/write_rate.go"
fi
if grep -qiF 'io.max' docs/DECISIONS.md && grep -qiF 'ZFS' docs/DECISIONS.md; then
  ok "write-rate.adr-iomax-negative"
else
  fail "write-rate.adr-iomax-negative" "ADR 95 must record the measured io.max/ZFS negative result"
fi
if grep -qiF 'overshoot' docs/DECISIONS.md && grep -qF 'write_rate' api/openapi.yaml; then
  ok "write-rate.documented"
else
  fail "write-rate.documented" "the write-rate capability is not documented in docs/DECISIONS.md and api/openapi.yaml"
fi
if grep -qF 'write_rate_mb_s' docs/API.md && grep -qF 'write_rate_exceeded' docs/API.md; then
  ok "write-rate.api-documented"
else
  fail "write-rate.api-documented" "write_rate_mb_s / write_rate_exceeded are not documented in docs/API.md"
fi

# --- 4d-nonies. native port/service discovery and its harness ---------------
# The endpoint answers which ports a workspace is listening on and which process
# serves each one. It must exist, be documented (why the tenant cannot see its
# own sockets, why /proc/<pid>/net and not ip netns exec, the measured cost, the
# inode->process rule, the loopback/wildcard/specific distinction, the separate
# unix section, the default-ON reasoning and the complete/honesty rule), be
# advertised in /v1/meta, and its harness must not silently rot.
if [ -f scripts/ports-check.sh ]; then
  ok "ports-harness.exists"
else
  fail "ports-harness.exists" "scripts/ports-check.sh is missing"
fi
if [ -x scripts/ports-check.sh ]; then
  ok "ports-harness.executable"
else
  fail "ports-harness.executable" "scripts/ports-check.sh is not executable"
fi
if grep -qF 'ports-check.sh' docs/VERIFICATION.md; then
  ok "ports-harness.documented"
else
  fail "ports-harness.documented" "docs/VERIFICATION.md does not mention scripts/ports-check.sh"
fi
if grep -qE '^ports-check:' Makefile; then
  ok "makefile.target.ports-check"
else
  fail "makefile.target.ports-check" "Makefile has no 'ports-check:' target"
fi
if grep -qE 'bash scripts/ports-check.sh' Makefile; then
  ok "makefile.sudo.ports-check"
else
  fail "makefile.sudo.ports-check" "Makefile's 'ports-check' target does not run scripts/ports-check.sh"
fi
if grep -qE '^\.PHONY:.*\bports-check\b' Makefile; then
  ok "makefile.phony.ports-check"
else
  fail "makefile.phony.ports-check" "'ports-check' is not in .PHONY"
fi
if grep -qF 'PortsConfig struct' internal/config/config.go && grep -qF 'Ports PortsConfig' internal/config/config.go; then
  ok "ports.config"
else
  fail "ports.config" "the config.PortsConfig type / Ports field is missing from internal/config/config.go"
fi
if grep -qF 'ports:' configs/config.example.yaml && grep -qF 'enabled: true' configs/config.example.yaml; then
  ok "ports.config-example"
else
  fail "ports.config-example" "the ports: block (default on) is missing from configs/config.example.yaml"
fi
if grep -qF 'ADR 96' docs/DECISIONS.md; then
  ok "ports.adr"
else
  fail "ports.adr" "docs/DECISIONS.md is missing ADR 96 (native port/service discovery)"
fi
if grep -qiF 'no /proc/net' docs/DECISIONS.md || grep -qiF 'does not exist' docs/DECISIONS.md; then
  ok "ports.adr-tenant-cannot-see"
else
  fail "ports.adr-tenant-cannot-see" "ADR 96 must record that the tenant has no /proc/net and no ss/netstat"
fi
if grep -qF '4.6 ms' docs/DECISIONS.md && grep -qiF 'metrics tick' docs/DECISIONS.md; then
  ok "ports.adr-cost-and-tick"
else
  fail "ports.adr-cost-and-tick" "ADR 96 must record the measured 4.6 ms cost and why it is NOT on the metrics tick"
fi
if grep -qiF 'ip netns exec' docs/DECISIONS.md && grep -qiF 'inode' docs/DECISIONS.md; then
  ok "ports.adr-netns-and-inode"
else
  fail "ports.adr-netns-and-inode" "ADR 96 must record why /proc/<pid>/net (not ip netns exec) and the socket-inode attribution"
fi
if grep -qiF 'loopback' docs/DECISIONS.md && grep -qiF 'wildcard' docs/DECISIONS.md && grep -qiF 'complete' docs/DECISIONS.md; then
  ok "ports.adr-scope-and-complete"
else
  fail "ports.adr-scope-and-complete" "ADR 96 must record the loopback/wildcard/specific scope, the unix section and the complete/honesty rule"
fi
if grep -qF 'GET /v1/workspaces/{workspaceID}/ports' docs/API.md && grep -qiF 'reachable from outside' docs/API.md; then
  ok "ports.api-documented"
else
  fail "ports.api-documented" "GET .../ports or its not-reachable-from-outside statement is missing from docs/API.md"
fi
if grep -qF '/v1/workspaces/{workspaceId}/ports' api/openapi.yaml && grep -qF 'PortList' api/openapi.yaml; then
  ok "ports.openapi"
else
  fail "ports.openapi" "the ports path/schema is missing from api/openapi.yaml"
fi
if grep -qF 'reads_total' internal/api/router.go && grep -qF 'portsCap' internal/api/router.go; then
  ok "ports.meta-advertised"
else
  fail "ports.meta-advertised" "GET /v1/meta does not advertise the ports capability (portsCap) with the reads_total counter"
fi
if [ -f internal/ports/parse.go ] && grep -qF 'func ParseTable(' internal/ports/parse.go && grep -qF 'func Build(' internal/ports/discover.go; then
  ok "ports.pure-parser"
else
  fail "ports.pure-parser" "the pure internal/ports parser (ParseTable/Build) is missing"
fi
if grep -qF 'NetTablesForPids' internal/executor/ports.go && grep -qF 'socket:[' internal/executor/ports.go; then
  ok "ports.host-read"
else
  fail "ports.host-read" "the bounded /proc/<pid>/net read + socket-inode attribution is missing from internal/executor/ports.go"
fi
if grep -qF 'ports_disabled' internal/model/errors.go && grep -qF 'ports_disabled' docs/API.md; then
  ok "ports.disabled-code"
else
  fail "ports.disabled-code" "the ports_disabled refusal code is missing from errors.go or docs/API.md"
fi

# The masking shim source and the embedded artifact must both be present, so a
# stale or missing .so cannot silently disable the mask.
if [ -f internal/executor/hostmask/hostmask.c ]; then
  ok "identity-shim.source"
else
  fail "identity-shim.source" "internal/executor/hostmask/hostmask.c is missing"
fi
if [ -f internal/executor/hostmask.so ]; then
  ok "identity-shim.built"
else
  fail "identity-shim.built" "internal/executor/hostmask.so is missing (run \`make shim\`)"
fi
if grep -qi 'identity_masking' docs/API.md && grep -qi 'identity_masking' api/openapi.yaml; then
  ok "identity-masking.documented"
else
  fail "identity-masking.documented" "the identity_masking capability is not documented in docs/API.md and api/openapi.yaml"
fi

# The provider-identity work (this iteration): the overlay switch, the
# dlsym-bypass closure and the egress-address finding must all be documented and
# wired, so they cannot silently regress out of the docs.
if grep -qi 'provider_tools_hidden' docs/API.md && grep -qi 'provider_tools_hidden' api/openapi.yaml; then
  ok "provider-tools.documented"
else
  fail "provider-tools.documented" "provider_tools_hidden is not documented in docs/API.md and api/openapi.yaml"
fi
if grep -qF 'hide_provider_tools' configs/config.example.yaml; then
  ok "provider-tools.config"
else
  fail "provider-tools.config" "identity.hide_provider_tools is missing from configs/config.example.yaml"
fi
if grep -qF 'dlsym' internal/executor/hostmask/hostmask.c && grep -qi 'dlsym' docs/DECISIONS.md; then
  ok "dlsym-bypass.documented"
else
  fail "dlsym-bypass.documented" "the dlsym interposition / ADR 72 correction is not in both hostmask.c and docs/DECISIONS.md"
fi
if grep -qi 'egress_proxy' configs/config.example.yaml && grep -qi 'ZEROPS-IO\|RDAP\|registered' docs/DECISIONS.md; then
  ok "egress-address.documented"
else
  fail "egress-address.documented" "the egress-address ADR / egress_proxy knob is not documented"
fi
# The egress-decoy work (this iteration): the switch, the capability, the honest
# limits and the shape-pattern approach must all be documented and wired, so they
# cannot silently regress out of the docs.
if grep -qi 'egress_decoy' docs/API.md && grep -qi 'egress_decoy' api/openapi.yaml; then
  ok "egress-decoy.documented"
else
  fail "egress-decoy.documented" "egress_decoy is not documented in docs/API.md and api/openapi.yaml"
fi
if grep -qF 'network.decoy.enabled' internal/config/config.go && grep -qF 'decoy:' configs/config.example.yaml; then
  ok "egress-decoy.config"
else
  fail "egress-decoy.config" "network.decoy is missing from internal/config/config.go or configs/config.example.yaml"
fi
if grep -qiF 'decoy layer, not a security boundary' docs/DECISIONS.md; then
  ok "egress-decoy.adr-boundary-statement"
else
  fail "egress-decoy.adr-boundary-statement" "ADR 84's 'a decoy layer, not a security boundary' sentence is missing"
fi
if grep -qF 'ADR 84' docs/DECISIONS.md && grep -qiF 'own endpoint' docs/DECISIONS.md; then
  ok "egress-decoy.adr-own-endpoint"
else
  fail "egress-decoy.adr-own-endpoint" "ADR 84 or its own-endpoint limit is missing"
fi
if [ -f docs/EGRESS-DECOY.md ] && grep -qiF 'not a security boundary' docs/EGRESS-DECOY.md; then
  ok "egress-decoy.enumeration-doc"
else
  fail "egress-decoy.enumeration-doc" "docs/EGRESS-DECOY.md is missing or lacks the boundary statement"
fi
if grep -qF 'hostPatterns' internal/decoy/profile.go && grep -qF 'MatchesHost' internal/decoy/profile.go; then
  ok "egress-decoy.pattern-not-list"
else
  fail "egress-decoy.pattern-not-list" "the decoy hostname SHAPE patterns are missing from internal/decoy/profile.go"
fi
if grep -qF 'invented.agrees_with_decoy' scripts/egress-decoy-check.sh; then
  ok "egress-decoy.harness-invented"
else
  fail "egress-decoy.harness-invented" "scripts/egress-decoy-check.sh has no invented-hostname assertion"
fi
if grep -qF 'control.pip_download' scripts/egress-decoy-check.sh; then
  ok "egress-decoy.harness-negative-control"
else
  fail "egress-decoy.harness-negative-control" "scripts/egress-decoy-check.sh has no pip negative control"
fi
# The identity harness must assert the two new groups (provider tools + dlsym).
if grep -qF 'provider.no_vendor_strings' scripts/identity-check.sh; then
  ok "identity-harness.provider-assertions"
else
  fail "identity-harness.provider-assertions" "scripts/identity-check.sh has no provider-tool assertions"
fi
if grep -qF 'bypass.named_handle_masked' scripts/identity-check.sh; then
  ok "identity-harness.bypass-assertions"
else
  fail "identity-harness.bypass-assertions" "scripts/identity-check.sh has no dlsym-bypass assertions"
fi
# The runtime group (node/python3/bash/perl/git succeed under the mask) and the
# canonicalization-no-echo group are direct guards for the two regressions fixed
# after the hiding pass; they must stay or the regression can return silently.
if grep -qF 'runtime.node' scripts/identity-check.sh; then
  ok "identity-harness.runtime-group"
else
  fail "identity-harness.runtime-group" "scripts/identity-check.sh has no runtime group (node/python3/bash/perl)"
fi
if grep -qF 'invisible.canonicalization_no_echo' scripts/identity-check.sh; then
  ok "identity-harness.canonicalization-group"
else
  fail "identity-harness.canonicalization-group" "scripts/identity-check.sh has no readlink/realpath no-echo assertions"
fi
# ADR 81 (canonicalization EACCES) and ADR 82 (rlimit_as != memory_mb) must be
# recorded in DECISIONS.md, and the address-space model in code+docs+harness.
if grep -qF 'ADR 81' docs/DECISIONS.md && grep -qF 'ADR 82' docs/DECISIONS.md; then
  ok "identity-adrs.81-82"
else
  fail "identity-adrs.81-82" "docs/DECISIONS.md is missing ADR 81 (canonicalization) or ADR 82 (rlimit_as)"
fi
if grep -qF 'addressSpaceMB' internal/executor/nsjail.go && grep -qF 'readlinkat' internal/executor/hostmask/hostmask.c; then
  ok "identity-fix.code"
else
  fail "identity-fix.code" "the rlimit_as headroom (addressSpaceMB) or the readlinkat interposer is missing"
fi

# The identity harness must be wired into the Makefile (the entry point an
# operator uses) and be in .PHONY.
if grep -qE '^identity-check:' Makefile; then
  ok "makefile.target.identity-check"
else
  fail "makefile.target.identity-check" "Makefile has no 'identity-check:' target"
fi
if grep -qE 'bash scripts/identity-check.sh' Makefile; then
  ok "makefile.sudo.identity-check"
else
  fail "makefile.sudo.identity-check" "Makefile's 'identity-check' target does not run scripts/identity-check.sh"
fi
if grep -qE '^\.PHONY:.*\bidentity-check\b' Makefile; then
  ok "makefile.phony.identity-check"
else
  fail "makefile.phony.identity-check" "'identity-check' is not in .PHONY"
fi

# --- 4e. the Makefile wires every acceptance harness it names ---------------
# The Makefile is the entry point operators use; a harness with no target is one
# nobody runs. Assert the three sudo-style host suites have targets and appear
# in .PHONY (the established pattern), pinned to the code's script names.

for tgt_script in "session-check:session-nsjail-check.sh" \
                  "process-check:process-stop-check.sh" \
                  "tunnel-check:tunnel-check.sh"; do
  tgt="${tgt_script%%:*}"
  scr="${tgt_script##*:}"
  if grep -qE "^$tgt:" Makefile; then
    ok "makefile.target.$tgt"
  else
    fail "makefile.target.$tgt" "Makefile has no '$tgt:' target"
  fi
  if grep -qE "sudo -E bash scripts/$scr" Makefile; then
    ok "makefile.sudo.$tgt"
  else
    fail "makefile.sudo.$tgt" "Makefile's '$tgt' target does not run 'sudo -E bash scripts/$scr'"
  fi
  if grep -qE "^\.PHONY:.*\b$tgt\b" Makefile; then
    ok "makefile.phony.$tgt"
  else
    fail "makefile.phony.$tgt" "'$tgt' is not in .PHONY"
  fi
done

# --- 5. stated test count equals the real test count -----------------------

STATED="$(grep -oE 'Test functions:[^0-9]*[0-9]+' docs/VERIFICATION.md | grep -oE '[0-9]+' | head -1)"
if [ -z "$STATED" ]; then
  fail "verification.test-count" "docs/VERIFICATION.md states no 'Test functions: N' count"
elif [ "$STATED" != "$TESTS" ]; then
  fail "verification.test-count" "states $STATED, actual $TESTS"
else
  ok "verification.test-count $STATED"
fi

# --- 6. DECISIONS.md has an entry for every required decision ---------------

REQUIRED_TERMS=(
  "PATCH"
  "maxima"
  "Lowering"
  "suspend"
  "stop"
  "purge"
  "usage"
  "read-only"
  "resolv"
  "privileged"
  "session"
  "PTY"
  "devpts"
  "idle"
  "lifetime"
  "stdin"
  "diagnostic"
  "cgroup"
  "detached"
)
for term in "${REQUIRED_TERMS[@]}"; do
  if grep -qi -- "$term" docs/DECISIONS.md; then
    ok "decisions.term $term"
  else
    fail "decisions.term.$term" "no decision covering '$term'"
  fi
done

# --- 7. OpenAPI lint (best-effort: needs npx and may need the network) ------

if command -v npx >/dev/null 2>&1; then
  if timeout 120 npx --yes @redocly/cli lint api/openapi.yaml >/tmp/openapi-lint.$$ 2>&1; then
    ok "openapi.lint"
  else
    if grep -qE 'getaddrinfo|ENOTFOUND|EAI_AGAIN|network' /tmp/openapi-lint.$$; then
      printf 'skip openapi.lint: npx could not reach the registry (offline)\n'
    else
      fail "openapi.lint" "redocly lint failed (see /tmp/openapi-lint.$$)"
    fi
  fi
  rm -f /tmp/openapi-lint.$$
else
  printf 'skip openapi.lint: npx not installed\n'
fi

# --- 8. the docs change left the build and tests green ----------------------

if [ -z "$(gofmt -l . )" ]; then
  ok "gofmt.clean"
else
  fail "gofmt.clean" "gofmt -l . printed files"
fi
if go build ./... >/tmp/docs-build.$$ 2>&1; then
  ok "go.build"
else
  fail "go.build" "go build ./... failed (see /tmp/docs-build.$$)"
fi
if go test ./... >/tmp/docs-test.$$ 2>&1; then
  ok "go.test"
else
  fail "go.test" "go test ./... failed (see /tmp/docs-test.$$)"
fi
rm -f /tmp/docs-build.$$ /tmp/docs-test.$$

# --- 9. the documented service version equals the code's model.Version ------
# model.Version is the single source of truth; every human-facing doc that
# states a concrete version must state that same one. Version strings that are
# clearly the decision-log history ("v0.0.6 … v0.0.15", a superseded rationale)
# are out of scope; this pins the current string.

MODEL_VERSION="$(grep -oE 'const Version = "[0-9.]+"' internal/model/types.go | sed -E 's/.*"([0-9.]+)"/\1/')"
if [ -z "$MODEL_VERSION" ]; then
  fail "version.model" "could not read model.Version from internal/model/types.go"
else
  ok "version.model $MODEL_VERSION"
  for doc in README.md docs/SPEC.md docs/VERIFICATION.md docs/ARCHITECTURE.md docs/API.md api/openapi.yaml; do
    if grep -qF -- "$MODEL_VERSION" "$doc"; then
      ok "version.doc.$doc"
    else
      fail "version.doc.$doc" "$doc does not mention the current service version $MODEL_VERSION"
    fi
  done
  if grep -qE "^  version: $MODEL_VERSION$" api/openapi.yaml && \
     grep -qF -- "examples: [\"$MODEL_VERSION\"]" api/openapi.yaml; then
    ok "version.openapi"
  else
    fail "version.openapi" "api/openapi.yaml does not carry $MODEL_VERSION in both its info.version and Meta examples"
  fi
fi

# --- tally ------------------------------------------------------------------

printf '\npassed=%d failed=%d\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
