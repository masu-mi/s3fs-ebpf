#!/usr/bin/env bash
# test-large-file-read.sh — T2 regression: files > 256 MiB (chunk ≥ 64) must
# be readable end-to-end in pcache mode.
#
# Prior to the T2 fix, s3fs_folio_fill_from_kmalloc had a hard `ci < 64` guard
# in the bitmap check.  For files ≥ 64 × 4 MiB = 256 MiB, chunks 64+ never
# set buf_valid = true, causing read_folio to return -EIO for any byte past the
# 256 MiB boundary.
#
# The fix: in the pcache path (gen != 0), the R1 invariant guarantees that
# fd->buf covers any range with a live pcache entry — the bitmap is skipped
# entirely for that path.
#
# Test strategy:
#   1. Seed a 300 MiB file with random data into MinIO.
#   2. Mount in pcache mode (--arena-metadata-only).
#   3. Read the file end-to-end and compare sha256 against the original.
#
# Usage:
#   sudo ./scripts/test-large-file-read.sh
#
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
#
# Requirements: kmod + daemon built (make all), MinIO up (make compose-up),
# docker, sha256sum.  Generating and hashing a 300 MiB file takes ~10s on
# typical hardware; total runtime ≈ 2-3 min including daemon settle.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-large-file-read}
SETTLE=${SETTLE:-10}
# 300 MiB: well past the 256 MiB (64 × 4 MiB) bitmap limit
FILE_SIZE=$(( 300 * 1024 * 1024 ))
TEST_FILE="large-file-read-test.bin"

log()  { printf '[large-file-read] %s\n' "$*" >&2; }

_RESULT_STATUS=FAIL
_RESULT_DETAIL=""
fail() {
    log "FAIL: $*"
    _RESULT_DETAIL="$*"
    exit 1
}

TMPDIR_LOCAL=$(mktemp -d /tmp/large-file-read-XXXXXX)
cleanup() {
    printf 'RESULT name=test-large-file-read status=%s file_size_mib=300\n' "$_RESULT_STATUS"
    [ "$_RESULT_STATUS" = FAIL ] && [ -n "$_RESULT_DETAIL" ] && \
        printf 'DETAIL %s\n' "$_RESULT_DETAIL"
    fd_buf_audit
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

mkdir -p "$OUT"

# ── prereqs ───────────────────────────────────────────────────────────────────
need_cmds docker sha256sum
need_built

# ── generate and seed file ────────────────────────────────────────────────────
log "generating ${FILE_SIZE}B ($(( FILE_SIZE / 1024 / 1024 )) MiB) test file"
ORIG="$TMPDIR_LOCAL/orig.bin"
# Use urandom in 4 MiB blocks to avoid large single-read stalls
dd if=/dev/urandom bs=$(( 4 * 1024 * 1024 )) \
   count=$(( FILE_SIZE / (4 * 1024 * 1024) )) \
   iflag=fullblock 2>/dev/null > "$ORIG"
ORIG_HASH=$(sha256sum "$ORIG" | awk '{print $1}')
log "original sha256: ${ORIG_HASH}"

log "seeding ${TEST_FILE} into MinIO ($(( FILE_SIZE / 1024 / 1024 )) MiB)"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$TEST_FILE" < "$ORIG" >/dev/null
log "seed done"

# ── mount in pcache mode ──────────────────────────────────────────────────────
log "mounting s3fs-ebpf in pcache mode (--arena-metadata-only)"
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" \
    --arena-metadata-only \
    --policy-bpf="builtin:policy_observe" \
    --softcap=8 --cache-capacity=0 \
    --metrics-addr=:9600 \
    --sample-every=1s --sync-every=5s
log "daemon started, settling ${SETTLE}s"
sleep "$SETTLE"

# ── first read (cold — populates all chunks including chunk ≥ 64) ─────────────
log "cold read pass 1 (populates all $(( FILE_SIZE / (4*1024*1024) )) chunks)"
sudo cat "$MOUNT/$TEST_FILE" > /dev/null
log "cold read done"

# ── read back and compare sha256 ─────────────────────────────────────────────
log "reading back file for sha256 comparison"
READ_HASH=$(sudo sha256sum "$MOUNT/$TEST_FILE" | awk '{print $1}')
log "readback sha256: ${READ_HASH}"

if [ "$READ_HASH" != "$ORIG_HASH" ]; then
    fail "sha256 mismatch: expected ${ORIG_HASH}, got ${READ_HASH}"
fi

# ── also verify the file size is preserved ────────────────────────────────────
READ_SIZE=$(sudo stat -c %s "$MOUNT/$TEST_FILE")
if [ "$READ_SIZE" -ne "$FILE_SIZE" ]; then
    fail "size mismatch: expected ${FILE_SIZE}, got ${READ_SIZE}"
fi

# ── report ────────────────────────────────────────────────────────────────────
{
echo ""
echo "=============================================================="
echo "test-large-file-read  $(date +%Y-%m-%d)"
echo "=============================================================="
printf "file size : %d MiB (%d bytes)\n" "$(( FILE_SIZE / 1024 / 1024 ))" "$FILE_SIZE"
printf "chunks    : %d  (last chunk index %d, well above 64-chunk limit)\n" \
    "$(( FILE_SIZE / (4 * 1024 * 1024) ))" \
    "$(( FILE_SIZE / (4 * 1024 * 1024) - 1 ))"
printf "sha256    : PASS (%s)\n" "$ORIG_HASH"
printf "file size : PASS (%d bytes)\n" "$READ_SIZE"
echo ""
echo "OVERALL: PASS"
echo "=============================================================="
} | tee "$OUT/large-file-read-report.txt"

_RESULT_STATUS=PASS
exit 0
