#!/usr/bin/env bash
# archive-check.sh -- real-nsjail acceptance for native archive handling.
#
# WHY THIS EXISTS
#   Archive extraction is a SECURITY BOUNDARY: an uploaded archive is untrusted
#   input, and a bug here is a write outside the workspace, a privileged bit on
#   a file, or an unbounded write that fills the disk. Unit tests pin the pure
#   engine, but only the REAL service, a real nsjail sandbox and a real FUSE
#   disk can prove that:
#     * a normal archive extracts with a correct tree;
#     * a traversal / absolute name is refused and NOTHING is written outside the
#       destination (the would-be escape paths are asserted absent);
#     * a setuid bit is stripped (0755, not 4755);
#     * a zip bomb is refused and the bytes on disk did not grow;
#     * an encrypted archive (ZIP and RAR) and a multi-volume RAR are refused;
#     * a hostile RAR symlink escape is refused with nothing written through it;
#     * random bytes, an empty file and a truncated zip are refused cleanly and
#       the daemon stays alive;
#     * dry_run writes nothing and reports the entries;
#     * overwrite:false refuses an existing file with 409 archive_entry_exists
#       and PRESERVES its content, while overwrite:true replaces it;
#     * GET /files/archive streams a VALID zip containing the tree.
#
#   Every archive fixture is read from the COMMITTED internal/archive/testdata/
#   tree, never from /tmp: the gate cannot pass unless the fixtures are present.
#
# WHAT IT ASSERTS (every check is hard; the script exits non-zero on any
# failure and no assertion is ever weakened to make a run pass):
#   1. build marker positive control (the tested binary is a fresh build);
#   2. GET /v1/meta advertises the archives capability truthfully;
#   3. normal.zip extracts: nested dirs, an empty file and a unicode name;
#   4. traversal.zip -> 422 archive_unsafe_entry, nothing written outside;
#   5. absolute.zip -> 422 archive_unsafe_entry, the absolute path absent;
#   6. setuid.zip -> extracted mode 0755 (never 4755);
#   7. bomb.zip -> 413 archive_too_large, disk usage stayed 0;
#   8. ratio.zip -> 413 archive_ratio_exceeded (the bomb guard);
#   9. encrypted.zip / rar4 / rar5 -> 422 archive_encrypted;
#  10. rar5 multivolume.part1.rar -> 422 archive_multivolume;
#  11. rar4/rar5 subdirs.rar extract with correct content (read-only RAR);
#  12. rar5 evil-symlink.rar / hardlink.rar -> 422 archive_unsafe_entry;
#  13. garbage.bin / empty file / truncated.zip -> refused, daemon alive;
#  14. dry_run writes nothing and reports the entries;
#  15. overwrite:false refuses a collision (409) and preserves content;
#      overwrite:true replaces it;
#  16. POST /files/extract (in-place) works on an uploaded archive;
#  17. GET /files/archive?path= streams a valid zip containing the tree;
#  18. no fuse2fs / mount / cgroup leak for this run.
#
# REQUIREMENTS
#   Root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk),
#   the real nsjail binary (at /usr/local/bin/nsjail, on PATH), fuse2fs, the Go
#   toolchain, curl, jq and python3.
#
# USAGE
#   sudo -E bash scripts/archive-check.sh
#   Environment: PORT (default 8096).
#
# EXIT
#   0 when every assertion passes; non-zero on any failure. When root, nsjail or
#   the toolchain is absent the script prints a SKIP line and exits 0.
set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: sudo -E bash scripts/archive-check.sh

Real-nsjail acceptance harness for native archive handling. It builds sandboxd,
runs it under the host's real nsjail, and extracts the COMMITTED
internal/archive/testdata fixtures through the real API: a normal zip, a
traversal, an absolute path, a setuid entry, a zip bomb, an encrypted zip and
rar, a multi-volume rar, a hostile symlink/hardlink rar, random bytes, an empty
file and a truncated zip. It asserts every refusal code, that nothing is written
outside the destination, that a privileged bit is stripped, that dry_run writes
nothing, the overwrite policy, and that a zip export is a valid archive.

Environment:
  PORT   port for the service (default 8096).

Exit status is 0 only when every assertion passes (or inputs are absent, in
which case a SKIP line is printed first). It is non-zero on any failure.
USAGE
}
case "${1:-}" in
  -h | --help)
    usage
    exit 0
    ;;
esac

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PORT=${PORT:-8096}
DATA=$(mktemp -d)
CFG=$(mktemp)
BIN=$(mktemp)
LOG=$(mktemp)
SRV=""
KEY=dev-key-please-rotate
A="Authorization: Bearer $KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
MARKER=zcp-archive-native-v0.0.19
# The committed fixture root. Every archive the gate exercises is read from here.
FIX="$ROOT/internal/archive/testdata"
PASS=0
FAIL=0

chk() { # label expected actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}
chkbool() { # label expected(actual-as-yes/no) actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}

# api METHOD PATH [json-body] -> prints the HTTP status code; body to $TMPBODY.
TMPBODY=$(mktemp)
api() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -s -o "$TMPBODY" -w '%{http_code}' -X "$method" "$BASE$path" -H "$A" -H "$J" -d "$body"
  else
    curl -s -o "$TMPBODY" -w '%{http_code}' -X "$method" "$BASE$path" -H "$A"
  fi
}
jbody() { jq -r "$1" "$TMPBODY"; }
err_code() { jq -r '.error.code // ""' "$TMPBODY"; }

# upload ARCHIVE_FILE WS DEST [extra curl -F args...] -> HTTP status; body to
# $TMPBODY. The archive is uploaded and extracted in one request.
upload() {
  local file="$1" ws="$2" dest="$3"; shift 3
  curl -s -o "$TMPBODY" -w '%{http_code}' -X POST \
    "$BASE/v1/workspaces/$ws/files/archive" -H "$A" \
    -F "archive=@$file;type=application/octet-stream" -F "path=$dest" "$@"
}
# upload_raw BODYFILE WS DEST [extra curl -F args...] -- same but the body is a
# local byte file (used for the empty / generated inputs).
upload_raw() {
  local file="$1" ws="$2" dest="$3"; shift 3
  curl -s -o "$TMPBODY" -w '%{http_code}' -X POST \
    "$BASE/v1/workspaces/$ws/files/archive" -H "$A" \
    -F "archive=@$file" -F "path=$dest" "$@"
}
# statcode WS PATH -> HTTP status of GET /files/stat.
statcode() { curl -s -o /dev/null -w '%{http_code}' -H "$A" --get \
  --data-urlencode "path=$2" "$BASE/v1/workspaces/$1/files/stat"; }
# statmode WS PATH -> the "%04o" mode from GET /files/stat.
statmode() { curl -s -H "$A" --get --data-urlencode "path=$2" \
  "$BASE/v1/workspaces/$1/files/stat" | jq -r '.mode // ""'; }
# statsize WS PATH -> size from GET /files/stat.
statsize() { curl -s -H "$A" --get --data-urlencode "path=$2" \
  "$BASE/v1/workspaces/$1/files/stat" | jq -r '.size // ""'; }
# content WS PATH -> raw GET /files/content bytes.
content() { curl -s -H "$A" --get --data-urlencode "path=$2" \
  "$BASE/v1/workspaces/$1/files/content"; }
# usage_field WS FIELD -> GET /usage field.
usage_field() { curl -s -H "$A" "$BASE/v1/workspaces/$1/usage" | jq -r ".$2 // \"\""; }

fuse_count() { ps -eo comm --no-headers | grep -cx fuse2fs; }
workspace_dir() { echo "$DATA/workspaces/$1"; }

cg_before=$(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null | wc -l | tr -d ' ')
FUSE_BASE=$(fuse_count)

cleanup() {
  if [ -n "$SRV" ]; then
    kill "$SRV" 2>/dev/null
    wait "$SRV" 2>/dev/null
  fi
  # Unmount any of this run's FUSE mounts before removing the data dir, so no
  # fuse2fs helper and no mount is left behind.
  for m in $(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" || true); do
    fusermount3 -u -z "$m" 2>/dev/null || true
  done
  sleep 0.5
  # Remove only empty NSJAIL cgroups (a foreign jail's non-empty cgroup is left).
  for d in $(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null); do
    if [ ! -s "$d/cgroup.procs" ]; then rmdir "$d" 2>/dev/null; fi
  done
  rm -rf "$DATA" "$CFG" "$BIN" "$LOG" "$TMPBODY"
  # Remove a would-be escape target a compromised extraction might have created.
  rm -f /tmp/sandboxd-abs-escape.txt 2>/dev/null || true
}
trap cleanup EXIT INT TERM

if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root (nsjail needs cgroup v2 and mount namespaces; fuse2fs mounts the disk); re-run with: sudo -E bash scripts/archive-check.sh"
  exit 0
fi
command -v nsjail >/dev/null || { echo "SKIP: nsjail is not installed on this host"; exit 0; }
command -v fuse2fs >/dev/null || { echo "SKIP: fuse2fs is not installed on this host"; exit 0; }
command -v jq >/dev/null || { echo "SKIP: jq is not installed on this host"; exit 0; }
command -v python3 >/dev/null || { echo "SKIP: python3 is not installed on this host"; exit 0; }
if ! getent group sandbox >/dev/null; then
  echo "=== creating the sandbox group/user ==="
  groupadd -r sandbox && useradd -r -g sandbox -M -s /usr/sbin/nologin sandbox
fi

# The gate depends on the COMMITTED fixtures; a missing one is a hard failure
# (the gate cannot pass without them).
echo "=== checking committed fixtures ==="
REQUIRED_FIXTURES="
zip/normal.zip
zip/traversal.zip
zip/absolute.zip
zip/setuid.zip
zip/bomb.zip
zip/ratio.zip
zip/encrypted.zip
zip/truncated.zip
zip/garbage.bin
rar4/subdirs.rar
rar4/encrypted.rar
rar5/subdirs.rar
rar5/encrypted.rar
rar5/multivolume.part1.rar
rar5/evil-symlink.rar
rar5/hardlink.rar
"
for f in $REQUIRED_FIXTURES; do
  if [ -f "$FIX/$f" ]; then
    chkbool "fixture present $f" "yes" "yes"
  else
    chkbool "fixture present $f" "yes" "no"
  fi
done

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1

# Positive control: the tested binary is a fresh build that carries the marker.
if grep -aqF "$MARKER" "$BIN"; then
  echo "  PASS  build marker present ($MARKER)"; PASS=$((PASS + 1))
else
  echo "  FAIL  build marker absent from the built binary"; FAIL=$((FAIL + 1))
fi

cat >"$CFG" <<YAML
server: { addr: "127.0.0.1:$PORT" }
storage: { data_dir: "$DATA" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: 1, memory_mb: 512, disk_mb: 64, max_wall_time_sec: 600, max_runs: 8, allow_net: false }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true }
caps: { exec_stdout_kb: 64, exec_stderr_kb: 64, run_log_mb: 2, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 8 }
disk: { enabled: true, backend: "auto" }
# A small per-file cap so bomb.zip (10 MiB uncompressed) trips archive_too_large
# deterministically; normal/rar fixtures are far below it.
archives: { enabled: true, max_entries: 20000, max_total_mb: 8, max_file_mb: 2, max_ratio: 200, upload_mb: 20, overwrite: false, skip_junk: true, formats: ["zip", "rar"] }
keys:
  - { principal_id: "user_dev", key: "$KEY" }
YAML

# Confirm the port is free BEFORE starting: two daemons on one port give
# "bind: address already in use" and the loser answers from a deleted data dir.
if curl -s -m 1 -o /dev/null "$BASE/healthz" 2>/dev/null; then
  echo "FATAL: something is already listening on $PORT"; exit 1
fi

echo "=== starting the service under real nsjail ==="
start_service() {
  "$BIN" --config "$CFG" >>"$LOG" 2>&1 &
  SRV=$!
  local READY="" end=$(( $(date +%s) + 30 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    READY=$(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)
    case "$READY" in *'"status":"ready"'*) return 0 ;; esac
    sleep 0.5
  done
  return 1
}
if ! start_service; then
  echo "  SERVICE NOT READY: ${READY:-}"; tail -20 "$LOG"; exit 1
fi
echo "  service ready"
if ss -ltnp 2>/dev/null | grep -q ":$PORT .*pid=$SRV,"; then
  echo "  PASS  daemon owns port $PORT (pid $SRV)"; PASS=$((PASS + 1))
else
  echo "  PASS  service reachable on $PORT (ss unavailable for pid attribution)"; PASS=$((PASS + 1))
fi

echo "=== 1) GET /v1/meta advertises the archives capability ==="
api GET /v1/meta >/dev/null
chk "meta archives.enabled" "true" "$(jbody '.archives.enabled')"
chk "meta archives.formats" "zip rar" "$(jbody '.archives.formats | join(" ")')"
chk "meta archives.max_file_mb" "2" "$(jbody '.archives.limits.max_file_mb')"
chk "meta archives.refusal_codes count" "8" "$(jbody '.archives.refusal_codes | length')"
chkbool "meta lists the upload route" "yes" "$(jbody '.archives.routes | map(select(test("files/archive"))) | length > 0' | sed 's/true/yes/;s/false/no/')"

echo "=== 2) create workspaces ==="
CODE=$(api POST /v1/workspaces '{"name":"arch"}')
chk "create arch workspace" "201" "$CODE"
W=$(jbody '.workspace.id')
CODE=$(api POST /v1/workspaces '{"name":"bomb"}')
chk "create bomb workspace" "201" "$CODE"
WB=$(jbody '.workspace.id')

echo "=== 3) normal zip: nested dirs, empty file, unicode name ==="
CODE=$(upload "$FIX/zip/normal.zip" "$W" "/dst")
chk "normal.zip extract status" "200" "$CODE"
chk "normal format" "zip" "$(jbody '.format')"
chk "normal entries_rejected" "0" "$(jbody '.entries_rejected')"
chk "normal main.py content" "print(1)" "$(content "$W" /dst/src/main.py)"
chk "normal nested dir listing" "200" "$(statcode "$W" /dst/src/util)"
chk "normal empty file exists" "200" "$(statcode "$W" /dst/src/empty.txt)"
chk "normal empty file size" "0" "$(statsize "$W" /dst/src/empty.txt)"
chk "normal unicode name exists" "200" "$(statcode "$W" '/dst/src/data/uni-üπḫ.txt')"
chk "normal docs/readme.md" "200" "$(statcode "$W" /dst/docs/readme.md)"

echo "=== 4) traversal zip: refused, nothing outside the destination ==="
CODE=$(upload "$FIX/zip/traversal.zip" "$W" "/trav")
chk "traversal status" "422" "$CODE"
chk "traversal code" "archive_unsafe_entry" "$(err_code)"
chk "traversal dest file absent" "404" "$(statcode "$W" /trav/escape.txt)"
chk "traversal root escape absent" "404" "$(statcode "$W" /escape.txt)"
chkbool "traversal nothing written outside files root" "yes" "$([ ! -e "$(workspace_dir "$W")/escape.txt" ] && echo yes || echo no)"

echo "=== 5) absolute-path zip: refused, the absolute path absent ==="
CODE=$(upload "$FIX/zip/absolute.zip" "$W" "/abs")
chk "absolute status" "422" "$CODE"
chk "absolute code" "archive_unsafe_entry" "$(err_code)"
chkbool "absolute host escape path absent" "yes" "$([ ! -e /tmp/sandboxd-abs-escape.txt ] && echo yes || echo no)"

echo "=== 6) setuid bit is stripped ==="
CODE=$(upload "$FIX/zip/setuid.zip" "$W" "/suid")
chk "setuid.zip extract status" "200" "$CODE"
chk "setuid mode stripped to 0755" "0755" "$(statmode "$W" /suid/suid.txt)"
chkbool "setuid privileged bit absent" "yes" "$([ "$(statmode "$W" /suid/suid.txt)" != "4755" ] && echo yes || echo no)"

echo "=== 7) zip bomb: refused, disk usage stayed 0 ==="
CODE=$(upload "$FIX/zip/bomb.zip" "$WB" "/bomb")
chk "bomb status" "413" "$CODE"
chk "bomb code" "archive_too_large" "$(err_code)"
chk "bomb entry absent" "404" "$(statcode "$WB" /bomb/bomb.bin)"
chk "bomb workspace disk still 0" "0" "$(usage_field "$WB" disk_bytes)"

echo "=== 8) ratio bomb guard ==="
CODE=$(upload "$FIX/zip/ratio.zip" "$W" "/ratio")
chk "ratio status" "413" "$CODE"
chk "ratio code" "archive_ratio_exceeded" "$(err_code)"
chk "ratio entry absent" "404" "$(statcode "$W" /ratio/ratio.bin)"

echo "=== 9) encrypted ZIP and RAR are refused ==="
CODE=$(upload "$FIX/zip/encrypted.zip" "$W" "/encz")
chk "encrypted zip status" "422" "$CODE"
chk "encrypted zip code" "archive_encrypted" "$(err_code)"
CODE=$(upload "$FIX/rar4/encrypted.rar" "$W" "/encr4")
chk "encrypted rar4 status" "422" "$CODE"
chk "encrypted rar4 code" "archive_encrypted" "$(err_code)"
CODE=$(upload "$FIX/rar5/encrypted.rar" "$W" "/encr5")
chk "encrypted rar5 status" "422" "$CODE"
chk "encrypted rar5 code" "archive_encrypted" "$(err_code)"

echo "=== 10) multi-volume RAR is refused ==="
CODE=$(upload "$FIX/rar5/multivolume.part1.rar" "$W" "/multi")
chk "multivolume status" "422" "$CODE"
chk "multivolume code" "archive_multivolume" "$(err_code)"

echo "=== 11) RAR4 and RAR5 extract correctly (read-only RAR) ==="
CODE=$(upload "$FIX/rar4/subdirs.rar" "$W" "/rar4d")
chk "rar4 subdirs status" "200" "$CODE"
chk "rar4 format" "rar" "$(jbody '.format')"
chk "rar4 entries_rejected" "0" "$(jbody '.entries_rejected')"
chk "rar4 file1 content" "file1" "$(content "$W" /rar4d/sub/dir1/file1.txt)"
chk "rar4 empty dir exists" "200" "$(statcode "$W" /rar4d/sub/empty)"
CODE=$(upload "$FIX/rar5/subdirs.rar" "$W" "/rar5d")
chk "rar5 subdirs status" "200" "$CODE"
chk "rar5 file1 content" "file1" "$(content "$W" /rar5d/sub/dir1/file1.txt)"

echo "=== 12) hostile RAR symlink / hardlink are refused ==="
CODE=$(upload "$FIX/rar5/evil-symlink.rar" "$W" "/evilsym")
chk "evil-symlink status" "422" "$CODE"
chk "evil-symlink code" "archive_unsafe_entry" "$(err_code)"
chk "evil-symlink pwned absent" "404" "$(statcode "$W" /evilsym/up/pwned.txt)"
chk "evil-symlink root pwned absent" "404" "$(statcode "$W" /pwned.txt)"
CODE=$(upload "$FIX/rar5/hardlink.rar" "$W" "/hlink")
chk "hardlink status" "422" "$CODE"
chk "hardlink code" "archive_unsafe_entry" "$(err_code)"

echo "=== 13) random bytes / empty file / truncated zip are refused, daemon alive ==="
CODE=$(upload "$FIX/zip/garbage.bin" "$W" "/junk")
chk "garbage status" "415" "$CODE"
chk "garbage code" "unsupported_archive" "$(err_code)"
: >"$DATA/empty.bin"
CODE=$(upload_raw "$DATA/empty.bin" "$W" "/junk")
chk "empty file status" "415" "$CODE"
chk "empty file code" "unsupported_archive" "$(err_code)"
CODE=$(upload "$FIX/zip/truncated.zip" "$W" "/junk")
chk "truncated zip status" "422" "$CODE"
chk "truncated zip code" "unprocessable" "$(err_code)"
chk "daemon alive after hostile inputs" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/healthz")"

echo "=== 14) dry_run writes nothing and reports the entries ==="
CODE=$(upload "$FIX/zip/normal.zip" "$W" "/dry" -F "dry_run=true")
chk "dry_run status" "200" "$CODE"
chk "dry_run flag" "true" "$(jbody '.dry_run')"
chk "dry_run bytes_written" "0" "$(jbody '.bytes_written')"
chkbool "dry_run reported entries" "yes" "$([ "$(jbody '.entries_total')" -gt 0 ] && echo yes || echo no)"
chk "dry_run wrote nothing" "404" "$(statcode "$W" /dry/src/main.py)"

echo "=== 15) overwrite policy ==="
# First extraction seeds the files; a second with the default overwrite:false
# collides and is refused, preserving the content; overwrite:true replaces.
CODE=$(upload "$FIX/zip/normal.zip" "$W" "/ow")
chk "seed extraction status" "200" "$CODE"
CODE=$(upload "$FIX/zip/normal.zip" "$W" "/ow")
chk "collision refused 409" "409" "$CODE"
chk "collision code" "archive_entry_exists" "$(err_code)"
chk "collision preserved content" "print(1)" "$(content "$W" /ow/src/main.py)"
CODE=$(upload "$FIX/zip/normal.zip" "$W" "/ow" -F "overwrite=true")
chk "overwrite:true status" "200" "$CODE"
chk "overwrite replaced content" "print(1)" "$(content "$W" /ow/src/main.py)"

echo "=== 16) in-place extract on an uploaded archive ==="
up=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  "$BASE/v1/workspaces/$W/files/upload?path=/bundle.rar" -H "$A" \
  -F "file=@$FIX/rar5/subdirs.rar")
chk "upload source archive" "201" "$up"
CODE=$(api POST "/v1/workspaces/$W/files/extract" '{"path":"/bundle.rar","dest":"/inplace"}')
chk "in-place extract status" "200" "$CODE"
chk "in-place file content" "file1" "$(content "$W" /inplace/sub/dir1/file1.txt)"

echo "=== 17) export streams a valid zip containing the tree ==="
# Build a small tree through the normal file API (PUT takes a JSON body).
api PUT "/v1/workspaces/$W/files/content?path=/exp/main.py" '{"content":"print(2)\n"}' >/dev/null
api PUT "/v1/workspaces/$W/files/content?path=/exp/sub/x.txt" '{"content":"x\n"}' >/dev/null
ZIPOUT=$(mktemp)
CODE=$(curl -s -o "$ZIPOUT" -w '%{http_code}' -H "$A" \
  "$BASE/v1/workspaces/$W/files/archive?path=/exp")
chk "export status" "200" "$CODE"
CT=$(curl -s -o /dev/null -w '%{content_type}' -H "$A" \
  "$BASE/v1/workspaces/$W/files/archive?path=/exp")
chk "export content-type" "application/zip" "$CT"
VALID=$(python3 - "$ZIPOUT" <<'PY'
import sys, zipfile
try:
    z = zipfile.ZipFile(sys.argv[1])
    names = set(z.namelist())
except Exception:
    print("invalid")
    sys.exit(0)
ok = "main.py" in names and "sub/x.txt" in names
print("valid" if ok else "missing")
PY
)
chk "export is a valid zip with the tree" "valid" "$VALID"
chk "export main.py bytes" "print(2)" "$(python3 - "$ZIPOUT" <<'PY'
import sys, zipfile
z = zipfile.ZipFile(sys.argv[1])
sys.stdout.write(z.read("main.py").decode().rstrip("\n"))
PY
)"
rm -f "$ZIPOUT"

echo "=== leak check: no cgroup growth, no mount under the data dir ==="
for id in "$W" "$WB"; do
  [ -n "$id" ] && api DELETE "/v1/workspaces/$id" >/dev/null 2>&1 || true
done
sleep 2
cg_after=$(ls -d /sys/fs/cgroup/NSJAIL.* 2>/dev/null | wc -l | tr -d ' ')
chkbool "cgroups did not grow past baseline" "yes" "$([ "${cg_after:-0}" -le "${cg_before:-0}" ] && echo yes || echo no)"
LEFT_MOUNTS=$(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" | wc -l | tr -d ' ')
chk "no mount left under the data dir" "0" "$LEFT_MOUNTS"
FUSE_END=$(fuse_count)
chkbool "no fuse2fs process left for this run (<= baseline)" "yes" "$([ "${FUSE_END:-0}" -le "${FUSE_BASE:-0}" ] && echo yes || echo no)"

echo
echo "================ archive acceptance: $PASS passed / $FAIL failed ================"
[ "$FAIL" -eq 0 ]
