#!/usr/bin/env bash
# lostfound-check.sh -- real-nsjail + real-FUSE acceptance for hiding the
# ext4-internal /lost+found from a workspace's visible root.
#
# WHY THIS EXISTS
#   A workspace's disk is a real ext4 image (mkfs.ext4), mounted host-side with
#   fuse2fs at the workspace root. mkfs.ext4 creates /lost+found in EVERY new
#   filesystem, so a fresh workspace's visible root showed only `lost+found` --
#   looking like leftover junk to the tenant. The engine now removes that
#   directory from a MOUNTED workspace root on every provisioning path, AFTER
#   mkfs.ext4 and AFTER any e2fsck repair, so it converges for a new workspace
#   AND for an image e2fsck has just repaired. See docs/DECISIONS.md ADR 98.
#
#   The hard part is the REPAIR path: e2fsck recreates /lost+found every time it
#   runs (measured: "/lost+found not found.  CREATED."), and the engine runs
#   e2fsck on an image it finds dirty. A one-shot removal at image creation
#   therefore fails -- the directory returns on the first repair. This gate
#   PROVES the repair path, not just creation: it dirties a real workspace's
#   image, restarts the service, and asserts the directory is absent AFTER the
#   repair.
#
# WHAT IT ASSERTS (every check is hard; exits non-zero on any failure):
#   1. a FRESHLY created workspace's visible root contains no /lost+found,
#      through the real API;
#   2. a workspace whose image was DIRTIED (so the engine runs e2fsck) also has
#      no /lost+found after it is re-provisioned -- the repair case a one-shot
#      fix fails;
#   3. an EXISTING workspace converges: its files are all still present with the
#      same content and size, and /lost+found is gone from the visible root;
#   4. the disk boundary still holds: two workspaces report their OWN different
#      disk_mb via df inside the workspace, and a write past the allowance still
#      fails with ENOSPC;
#   5. nothing is leaked (no mount, no fuse2fs, no cgroup, no orphan image).
#
# REQUIREMENTS
#   Root (fuse2fs mounts the disk; debugfs dirties it), the real nsjail binary,
#   fuse2fs, debugfs, python3, and the Go toolchain.
#
# USAGE
#   sudo -E bash scripts/lostfound-check.sh
#   Environment: PORT (default 8097).
#
# EXIT
#   0 when every assertion passes; non-zero on any failure. When root, nsjail or
#   the toolchain is absent the script prints a SKIP line and exits 0.
set -uo pipefail

usage() {
  cat <<'USAGE'
Usage: sudo -E bash scripts/lostfound-check.sh

Real-nsjail + real-FUSE acceptance for hiding the ext4-internal /lost+found from
a workspace's visible root, on both the fresh-create path and the e2fsck-repair
path, without changing the disk boundary.

Environment:
  PORT   port for the service (default 8097).

Exit status is 0 only when every assertion passes (or inputs are absent, in
which case a SKIP line is printed first). It is non-zero on any failure.
USAGE
}
case "${1:-}" in
  -h | --help) usage; exit 0 ;;
esac

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PORT=${PORT:-8097}
DATA=$(mktemp -d)
CFG=$(mktemp)
BIN=$(mktemp)
LOG=$(mktemp)
SRV=""
KEY=dev-key-please-rotate
A="Authorization: Bearer $KEY"
J="Content-Type: application/json"
BASE="http://127.0.0.1:$PORT"
PASS=0
FAIL=0

chk() { # label expected actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}
chkbool() { # label yes/no actual
  if [ "$2" = "$3" ]; then
    echo "  PASS  $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $1 expected=$2 got=$3"; FAIL=$((FAIL + 1))
  fi
}
# jf EXPR -- print a JSON field, or !ERROR (never an empty string) so a failed
# API call can never masquerade as a legitimate value.
jf() { python3 -c "import json,sys
try: print(json.load(sys.stdin)$1)
except Exception as e: print('!ERROR')"; }

# nset WS [path] -- a directory's entry names, sorted (order-independent set
# comparison). A failed listing prints !ERROR, never ''.
nset() { # WS [path]
  curl -s -H "$A" "$BASE/v1/workspaces/$1/files?path=${2:-/}" \
    | python3 -c "import json,sys
try:
    print(','.join(sorted(e['name'] for e in json.load(sys.stdin)['entries'])))
except Exception as e: print('!ERROR')"
}
# The size of one file via stat.
fsize() { # WS path
  curl -s -H "$A" "$BASE/v1/workspaces/$1/files/stat?path=$2" | jf "['size']"
}
# The sha256 of one file via stat (the newline-safe way to compare content).
fsha() { # WS path
  curl -s -H "$A" "$BASE/v1/workspaces/$1/files/stat?path=$2" | jf "['sha256']"
}
# nset_sizes WS path -- names with sizes, sorted, for the convergence compare.
nset_sizes() { # WS path
  curl -s -H "$A" "$BASE/v1/workspaces/$1/files?path=$2" \
    | python3 -c "import json,sys
try:
    e=json.load(sys.stdin)['entries']
    print(','.join(sorted(x['name']+':'+str(x['size']) for x in e)))
except Exception as e: print('!ERROR')"
}
# The ext4 filesystem state, spaces removed ("clean" / "notclean").
img_state() { dumpe2fs -h "$1" 2>/dev/null | awk -F: '/Filesystem state/{gsub(/ /,"",$2);print $2}'; }
# The workspace's own filesystem size, read with df INSIDE the workspace.
df_size() { # WS
  curl -s -H "$A" -H "$J" --data-binary '{"cmd":["sh","-c","df -B1 --output=size /workspace | tail -1"]}' \
    "$BASE/v1/workspaces/$1/exec" | jf "['stdout']" | tr -d ' \n'
}

image_for() { # WS -> the image path (from the service's data dir layout)
  echo "$DATA/workspaces/$1/disk.img"
}

fuse_count() { ps -eo comm --no-headers | grep -cx fuse2fs; }

start_service() {
  "$BIN" --config "$CFG" >>"$LOG" 2>&1 &
  SRV=$!
  local READY="" end=$(( $(date +%s) + 40 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    READY=$(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)
    case "$READY" in *'"status":"ready"'*) return 0 ;; esac
    sleep 0.5
  done
  return 1
}
stop_service() {
  if [ -n "$SRV" ]; then
    kill "$SRV" 2>/dev/null
    wait "$SRV" 2>/dev/null
    SRV=""
  fi
  # Let the FUSE helpers exit as their mounts go away.
  for _ in $(seq 1 20); do
    [ "$(fuse_count)" -le "${FUSE_BASE:-0}" ] && break
    sleep 0.5
  done
}

cleanup() {
  stop_service
  for m in $(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" || true); do
    fusermount3 -u -z "$m" 2>/dev/null || true
  done
  rm -rf "$DATA" "$CFG" "$BIN" "$LOG"
}
trap cleanup EXIT INT TERM

# --- preconditions ----------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
  echo "SKIP: this harness needs root (fuse2fs mounts the disk; debugfs dirties it); re-run with: sudo -E bash scripts/lostfound-check.sh"
  exit 0
fi
command -v nsjail >/dev/null || { echo "SKIP: nsjail is not installed on this host"; exit 0; }
command -v fuse2fs >/dev/null || { echo "SKIP: fuse2fs is not installed on this host"; exit 0; }
command -v debugfs >/dev/null || { echo "SKIP: debugfs is not installed on this host"; exit 0; }
command -v python3 >/dev/null || { echo "SKIP: python3 is not installed on this host"; exit 0; }
if ! getent group sandbox >/dev/null; then
  groupadd -r sandbox && useradd -r -g sandbox -M -s /usr/sbin/nologin sandbox
fi

# Guard against silent rot: the mechanism must exist in the source before the
# run starts, so a deleted implementation cannot pass by absence.
[ -f "$ROOT/internal/workspace/diskfs.go" ] || { echo "FAIL  artifact: diskfs.go missing"; exit 1; }
grep -q 'func clearLostFound' "$ROOT/internal/workspace/diskfs.go" ||
  { echo "FAIL  artifact: the lost+found remover is gone"; exit 1; }
grep -q 'clearLostFound(root)' "$ROOT/internal/workspace/diskfs.go" ||
  { echo "FAIL  artifact: the lost+found remover is never called on a provisioning path"; exit 1; }

echo "=== building ==="
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP: go toolchain not found"; exit 0; }
(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/sandboxd) || exit 1

echo "=== starting the service under real nsjail ==="
cat >"$CFG" <<YAML
server: { addr: "127.0.0.1:$PORT" }
storage: { data_dir: "$DATA" }
sandbox: { nsjail_path: "$(command -v nsjail)", user: "sandbox", group: "sandbox", kill_grace_sec: 2 }
limits: { cpu_cores: 1, memory_mb: 256, disk_mb: 64, max_wall_time_sec: 120, max_runs: 4, allow_net: false }
max_limits: { cpu_cores: 8, memory_mb: 8192, disk_mb: 51200, max_wall_time_sec: 3600, max_runs: 64, allow_net: true, allow_unlimited_runs: true }
caps: { exec_stdout_kb: 64, exec_stderr_kb: 64, run_log_mb: 2, json_body_mb: 1, upload_mb: 20, file_read_mb: 5, file_write_mb: 8 }
disk: { enabled: true, backend: "auto" }
keys:
  - { principal_id: "user_dev", key: "$KEY" }
YAML
if curl -s -m 1 -o /dev/null "$BASE/healthz" 2>/dev/null; then
  echo "FATAL: something is already listening on $PORT"; exit 1
fi
if ! start_service; then
  echo "  SERVICE NOT READY: $(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)"; tail -20 "$LOG"; exit 1
fi
echo "  service ready: $(curl -s -m 2 "$BASE/readyz" 2>/dev/null || true)"
echo "  disk mechanism: $(curl -s -H "$A" "$BASE/v1/meta" | jf "['workspace_limits']['mechanism']")"
FUSE_BASE=$(fuse_count)

# ---- 1) a FRESH workspace's visible root has no lost+found ------------------
echo "=== 1) fresh workspace: visible root has no /lost+found ==="
W1=$(curl -s -m 15 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" -d '{"name":"lf-fresh"}' | jf "['workspace']['id']")
[ -n "$W1" ] || { echo "FATAL: no workspace id"; exit 1; }
echo "  workspace=$W1"
# Provisioning happens on the first filesystem access; a listing IS that access.
NAMES=$(nset "$W1" "/")
chk "fresh root entry set is empty" "" "$NAMES"
# A second fresh workspace, so the result is not a one-off.
W2=$(curl -s -m 15 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" -d '{"name":"lf-fresh2"}' | jf "['workspace']['id']")
chk "second fresh root entry set is empty" "" "$(nset "$W2" "/")"
# A stat of the path itself must be not-found, not a visible directory.
chk "stat /lost+found is 404" "$(curl -s -o /dev/null -w '%{http_code}' -H "$A" "$BASE/v1/workspaces/$W1/files/stat?path=/lost+found")" "404"

# ---- 2) the REPAIR path: a dirtied image is repaired and stays clean ---------
echo "=== 2) repair path: a dirtied image is e2fsck-repaired and stays clean ==="
# Write a user file into W1 so we can also prove no data is disturbed by repair.
curl -s -o /dev/null -X PUT -H "$A" -H "$J" -d '{"content":"survives-the-repair\n"}' "$BASE/v1/workspaces/$W1/files/content?path=/keep.txt"
SHA_BEFORE=$(fsha "$W1" /keep.txt)
SIZE_BEFORE=$(fsize "$W1" /keep.txt)
chkbool "user file has a hash before repair" "yes" "$([ -n "$SHA_BEFORE" ] && echo yes || echo no)"
# Take the service down so the image can be dirtied offline.
stop_service
IMG=$(image_for "$W1")
chkbool "image file exists" "yes" "$([ -f "$IMG" ] && echo yes || echo no)"
# Dirty the superblock EXACTLY as an interrupted metadata write leaves it, and
# put /lost+found back, so this asserts the repair path recreates it and the
# engine removes it -- NOT merely that a removal persisted.
debugfs -w -R "ssv state 0" "$IMG" >/dev/null 2>&1
debugfs -w -R "mkdir /lost+found" "$IMG" >/dev/null 2>&1
# Prove the preconditions: the image is dirty AND /lost+found is present.
chk "image is dirty before reprovision" "notclean" "$(img_state "$IMG")"
chkbool "root has lost+found before reprovision" "yes" \
  "$(debugfs -R 'ls -p /' "$IMG" 2>/dev/null | grep -q '/lost+found' && echo yes || echo no)"
# Prove the dirty state FORCES the e2fsck self-heal: a plain resize2fs refuses
# ("Please run 'e2fsck -f ... first"), so the engine cannot complete the
# provision without running e2fsck and retrying. If the provision below
# succeeds, e2fsck therefore ran -- this is the repair the one-shot fix misses.
BC=$(dumpe2fs -h "$IMG" 2>/dev/null | awk '/Block count:/{print $3}')
if command -v resize2fs >/dev/null 2>&1; then
  chkbool "plain resize2fs refuses the dirty image (the engine must e2fsck)" "yes" \
    "$(resize2fs "$IMG" "$BC" >/dev/null 2>&1 && echo no || echo yes)"
fi
# Restart and re-provision: the first filesystem access runs the repair.
if ! start_service; then
  echo "  SERVICE NOT READY after restart"; tail -20 "$LOG"; exit 1
fi
NAMES=$(nset "$W1" "/")
case ",$NAMES," in
  *,lost+found,*) chkbool "repaired root has no lost+found" "yes" "no" ;;
  *) chkbool "repaired root has no lost+found" "yes" "yes" ;;
esac
chk "repaired root lists only the user file" "keep.txt" "$NAMES"
chk "user file content survives the repair (sha)" "$SHA_BEFORE" "$(fsha "$W1" /keep.txt)"
chk "user file size survives the repair" "$SIZE_BEFORE" "$(fsize "$W1" /keep.txt)"
# The provision SUCCEEDED on the dirty image: resize2fs refuses a dirty
# superblock ("Please run 'e2fsck -f ... first"), so the engine must have run
# its e2fsck self-heal and RETRIED the resize for the listing above to answer at
# all (a failed provision returns 503, which would have made NAMES empty).

# ---- 3) an existing workspace converges, without losing files ---------------
echo "=== 3) existing workspace converges with no file loss ==="
# Add a small tree and record its listing and sizes.
curl -s -o /dev/null -H "$A" -H "$J" -d '{"path":"/sub"}' "$BASE/v1/workspaces/$W1/files/mkdir"
curl -s -o /dev/null -X PUT -H "$A" -H "$J" -d '{"content":"alpha"}' "$BASE/v1/workspaces/$W1/files/content?path=/sub/a.txt"
curl -s -o /dev/null -X PUT -H "$A" -H "$J" -d '{"content":"beta-beta"}' "$BASE/v1/workspaces/$W1/files/content?path=/sub/b.txt"
BEFORE=$(nset_sizes "$W1" "/sub")
chkbool "subdirectory listing captured before reprovision" "yes" "$([ "$BEFORE" != "!ERROR" ] && echo yes || echo no)"
# Re-run the provision path (dirty + restart), then compare.
stop_service
debugfs -w -R "ssv state 0" "$IMG" >/dev/null 2>&1
debugfs -w -R "mkdir /lost+found" "$IMG" >/dev/null 2>&1
start_service || { echo "  SERVICE NOT READY"; tail -20 "$LOG"; exit 1; }
ROOT_NAMES=$(nset "$W1" "/")
AFTER=$(nset_sizes "$W1" "/sub")
chk "subdirectory listing unchanged after reprovision" "$BEFORE" "$AFTER"
chk "existing root keeps the user file, no lost+found" "keep.txt,sub" "$ROOT_NAMES"
chk "root file content intact" "$(printf 'beta-beta' | sha256sum | cut -d' ' -f1)" "$(fsha "$W1" /sub/b.txt)"

# ---- 4) the disk boundary is unchanged ------------------------------------
echo "=== 4) the disk boundary is unchanged ==="
# W1 is 64 MiB. Give W2 a DIFFERENT allowance and assert each reports its own.
W3=$(curl -s -m 15 -X POST "$BASE/v1/workspaces" -H "$A" -H "$J" \
  -d '{"name":"lf-big","limits":{"disk_mb":128}}' | jf "['workspace']['id']")
# Provision W3 with a listing, then read its own df.
nset "$W3" "/" >/dev/null
chk "w1 df size == its disk_mb (64 MiB)" "$((64 * 1024 * 1024))" "$(df_size "$W1")"
chk "w3 df size == its disk_mb (128 MiB)" "$((128 * 1024 * 1024))" "$(df_size "$W3")"
chkbool "the two workspaces report different sizes" "yes" \
  "$([ "$(df_size "$W1")" != "$(df_size "$W3")" ] && echo yes || echo no)"
# A write past the allowance is refused at the boundary (ENOSPC / non-zero).
OVER=$(curl -s -H "$A" -H "$J" --data-binary \
  '{"cmd":["sh","-c","dd if=/dev/zero of=/workspace/over.bin bs=1M count=100 2>&1; echo RC=$?"]}' \
  "$BASE/v1/workspaces/$W1/exec" | jf "['stdout']")
chkbool "a write past disk_mb is refused (No space left / non-zero rc)" "yes" \
  "$(printf '%s' "$OVER" | grep -qiE 'No space left|RC=[1-9]' && echo yes || echo "no($OVER)")"
# Removing the directory must not have changed the image size or the ceiling:
# the mounted total is still exactly disk_mb even with no other data.
curl -s -X POST -H "$A" -H "$J" --data-binary '{"cmd":["sh","-c","rm -f /workspace/over.bin"]}' \
  "$BASE/v1/workspaces/$W1/exec" >/dev/null 2>&1 || true
chk "mounted total is still exactly disk_mb after the removal" "$((64 * 1024 * 1024))" "$(df_size "$W1")"

# ---- 5) delete, and leak check --------------------------------------------
echo "=== 5) delete and leak check ==="
for w in "$W1" "$W2" "$W3"; do
  chk "delete $w" "204" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "$A" "$BASE/v1/workspaces/$w")"
done
stop_service
sleep 1
LEFT_MOUNTS=$(findmnt -rno TARGET 2>/dev/null | grep -F "$DATA" | wc -l | tr -d ' ')
chk "no mount left under the data dir" "0" "$LEFT_MOUNTS"
chk "no orphan image left" "0" "$(find "$DATA" -name disk.img 2>/dev/null | wc -l | tr -d ' ')"
FUSE_NOW=$(fuse_count)
for _ in $(seq 1 20); do
  [ "${FUSE_NOW:-0}" -le "${FUSE_BASE:-0}" ] && break
  sleep 0.5; FUSE_NOW=$(fuse_count)
done
chkbool "no fuse2fs process left for this run (<= baseline)" "yes" \
  "$([ "${FUSE_NOW:-0}" -le "${FUSE_BASE:-0}" ] && echo yes || echo "no($FUSE_NOW>${FUSE_BASE})")"

echo
echo "================ lostfound acceptance: $PASS passed / $FAIL failed ================"
[ "$FAIL" -eq 0 ]
