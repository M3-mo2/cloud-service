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
for term in suspend resume usage package run session; do
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

# --- tally ------------------------------------------------------------------

printf '\npassed=%d failed=%d\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
