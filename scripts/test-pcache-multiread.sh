#!/usr/bin/env bash
# test-pcache-multiread.sh — P1 regression test for split-chunk pcache reads.
#
# Seeds a file that is larger than 2 × partialAdmitChunk (4 MiB each),
# causing handleReadMiss to serve it in ≥ 2 POPULATE_PAGES calls.
# Reads the file end-to-end via a single fd and compares sha256 with
# the original to catch the P1 bug (fc_pcache_check ignoring pos range
# → zero-filled second chunk on split admit).
#
# Usage:
#   sudo ./scripts/test-pcache-multiread.sh
#
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
#
# Requirements: ebpf-ebpf kmod + daemon built (make all), MinIO up
# (make compose-up), jq, sha256sum.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-pcache-multiread}
SETTLE=${SETTLE:-8}
# Two full 4 MiB chunks + 1 byte: forces exactly 3 POPULATE_PAGES calls
# for the strict partial-admit path (fullSize > partialAdmitThreshold).
FILE_SIZE=$(( 4 * 1024 * 1024 * 2 + 1 ))
TEST_FILE="pcache-split-test.bin"
TMPDIR_LOCAL=$(mktemp -d /tmp/pcache-split-XXXXXX)

log()  { printf '[pcache-split] %s\n' "$*" >&2; }

_RESULT_STATUS=FAIL
_RESULT_DETAIL=""
fail() {
    log "FAIL: $*"
    _RESULT_DETAIL="$*"
    exit 1
}

cleanup() {
    printf 'RESULT name=test-pcache-multiread status=%s sha256=%s\n' \
        "$_RESULT_STATUS" "$( [ "$_RESULT_STATUS" = PASS ] && echo ok || echo mismatch )"
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

# ── seed file into MinIO ──────────────────────────────────────────────────────
log "generating ${FILE_SIZE}B test file"
ORIG="$TMPDIR_LOCAL/orig.bin"
dd if=/dev/urandom bs=4096 count=$(( (FILE_SIZE + 4095) / 4096 )) \
   2>/dev/null | head -c "$FILE_SIZE" > "$ORIG"
ORIG_HASH=$(sha256sum "$ORIG" | awk '{print $1}')
log "original sha256: ${ORIG_HASH}"

log "seeding ${TEST_FILE} into MinIO"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$TEST_FILE" < "$ORIG" >/dev/null
log "seed done"

# ── mount ebpf in pcache mode ─────────────────────────────────────────────────
log "mounting s3fs-ebpf in pcache mode (--arena-metadata-only)"
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" \
    --arena-metadata-only \
    --policy-bpf="builtin:policy_observe" \
    --softcap=8 --cache-capacity=0
log "daemon started, settling ${SETTLE}s"
sleep "$SETTLE"

# ── trigger first read (populates both chunks) ────────────────────────────────
log "first read (cold, triggers ≥2 POPULATE_PAGES chunks)"
sudo cat "$MOUNT/$TEST_FILE" > /dev/null

# ── read back and compare ─────────────────────────────────────────────────────
log "reading back file for sha256 comparison"
READ_HASH=$(sudo sha256sum "$MOUNT/$TEST_FILE" | awk '{print $1}')
log "read sha256:     ${READ_HASH}"

if [ "$READ_HASH" = "$ORIG_HASH" ]; then
    log "PASS — sha256 matches (split-chunk pcache read correct)"
    _RESULT_STATUS=PASS
    exit 0
else
    fail "sha256 mismatch: expected ${ORIG_HASH}, got ${READ_HASH}"
fi
