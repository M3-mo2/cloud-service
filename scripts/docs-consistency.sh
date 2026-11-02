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
