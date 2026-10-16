#!/usr/bin/env bash
# test-pcache-prewarm-none.sh — AC-43 regression: reads must work in pcache
# mode even when prewarm admits nothing.
#
# The read-miss upcall is gated on s3fs_admitted_mm.  Before AC-43 only an
# admit_replace with count > 0 set it, so a policy that prewarms nothing
# (prewarm: false, as in policies/prewarm-meta-only.cel) pushed count=0, the
# pointer stayed NULL, no upcall ever fired and every read failed.  AC-43
# hands the daemon's mm over on count=0 as well.
#
# Test strategy:
#   1. Seed a tiny object and a multi-chunk object (FILE_MIB, default 9 MiB
#      = three 4 MiB chunks) into a dedicated bucket.
#   2. Start the daemon in pcache mode with a generated `prewarm: false`
#      policy, so the initial admit push is empty.
#   3. Read every object in full and compare sha256 with the original.
#
# Usage:
#   sudo ./scripts/test-pcache-prewarm-none.sh
#
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
#
# Requirements: ebpf kmod + daemon built (make all), MinIO up (make compose-up),
# docker, sha256sum.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-pcache-prewarm-none}
SETTLE=${SETTLE:-8}
FILE_MIB=${FILE_MIB:-9}
READ_TIMEOUT=${READ_TIMEOUT:-60}
TEST_NAME=${TEST_NAME:-test-pcache-prewarm-none}
# Dedicated bucket so that only this test's objects are listed.
S3_BUCKET=${PREWARM_NONE_BUCKET:-s3fs-prewarm-none}

log()  { printf '[prewarm-none] %s\n' "$*" >&2; }

TMPDIR_LOCAL=$(mktemp -d /tmp/pcache-prewarm-none-XXXXXX)
cleanup() {
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

mkdir -p "$OUT"

# ── prereqs ───────────────────────────────────────────────────────────────────
need_cmds docker sha256sum timeout
need_built

# ── seed files into MinIO ─────────────────────────────────────────────────────
log "seeding objects into MinIO (bucket=${S3_BUCKET})"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null

declare -A HASHES
printf 'prewarm-none small object for AC-43\n' > "$TMPDIR_LOCAL/small.txt"
dd if=/dev/urandom bs=1M count="$FILE_MIB" iflag=fullblock 2>/dev/null \
    > "$TMPDIR_LOCAL/large.bin"
for fname in small.txt large.bin; do
    HASHES[$fname]=$(sha256sum < "$TMPDIR_LOCAL/$fname" | awk '{print $1}')
    docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$fname" \
        < "$TMPDIR_LOCAL/$fname" >/dev/null
done
printf 'prewarm: false\n' > "$TMPDIR_LOCAL/prewarm-none.cel"
chmod 644 "$TMPDIR_LOCAL/prewarm-none.cel"
log "seed done"

# ── mount in pcache mode ──────────────────────────────────────────────────────
log "mounting s3fs-ebpf in pcache mode with an empty prewarm"
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" \
    --bucket="$S3_BUCKET" \
    --arena-metadata-only \
    --policy="$TMPDIR_LOCAL/prewarm-none.cel" \
    --metrics-addr=:9600 \
    --sample-every=1s --sync-every=0
log "daemon started, settling ${SETTLE}s"
sleep "$SETTLE"

# ── reads ─────────────────────────────────────────────────────────────────────
READ_FAIL=0
for fname in small.txt large.bin; do
    got=$(sudo timeout "$READ_TIMEOUT" cat "$MOUNT/$fname" 2>/dev/null \
        | sha256sum | awk '{print $1}') || true
    if [ "$got" = "${HASHES[$fname]}" ]; then
        log "  ${fname}: sha256 OK"
    else
        log "  ${fname}: sha256 mismatch (got=${got})"
        READ_FAIL=$(( READ_FAIL + 1 ))
    fi
done
ADMITTED=$(grep -oE '\[admit\] [0-9]+ entries' "$OUT/daemon.log" | head -1 || true)

# ── verdict ───────────────────────────────────────────────────────────────────
overall=PASS
[ "$READ_FAIL" -gt 0 ] && overall=FAIL

{
echo ""
echo "=============================================================="
echo "test-pcache-prewarm-none  $(date +%Y-%m-%d)"
echo "=============================================================="
printf "objects    : small.txt, large.bin (%d MiB)\n" "$FILE_MIB"
printf "prewarm    : %s\n" "${ADMITTED:-unknown}"
printf "read fail  : %d\n" "$READ_FAIL"
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/prewarm-none-report.txt"

printf 'RESULT name=%s status=%s read_fail=%d\n' "$TEST_NAME" "$overall" "$READ_FAIL"
[[ "$overall" == FAIL ]] && \
    printf 'DETAIL read_fail=%d with an empty prewarm (upcall not armed — AC-43)\n' "$READ_FAIL"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
