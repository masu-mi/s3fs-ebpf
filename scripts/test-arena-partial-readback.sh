#!/usr/bin/env bash
# test-arena-partial-readback.sh — AC-48 regression: chunk (D-01 partial)
# admits in arena mode must not change the file size.
#
# admit_replace used to i_size_write() each admit entry's size.  An entry is
# a range, so admitting chunk [4, 8) MiB of a 10 MiB file set i_size to
# 4 MiB: the triggering read returned 0 bytes and a later full read stopped
# at 4 MiB with exit 0 (and the evict path persisted the wrong size).
#
# Test strategy:
#   1. Seed a FILE_MIB (default 10) random object into a dedicated bucket.
#   2. Start the daemon in arena mode with `prewarm: false` and
#      --pattern-aware-admit=false so every miss is a 4 MiB chunk admit.
#   3. Random-first: read 4 KiB inside chunk 1, then 4 KiB inside chunk 0,
#      then the whole file.  Size and sha256 must match.
#   4. Parallel: on a fresh daemon, 4 concurrent full reads; every sha256
#      must match.
#
# Usage:
#   sudo ./scripts/test-arena-partial-readback.sh
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
OUT=${OUT:-/tmp/test-arena-partial-readback}
SETTLE=${SETTLE:-6}
FILE_MIB=${FILE_MIB:-10}
N_PAR=${N_PAR:-4}
TEST_NAME=${TEST_NAME:-test-arena-partial-readback}
S3_BUCKET=${ARENA_PARTIAL_BUCKET:-s3fs-arena-partial}
FNAME=partial.bin

log()  { printf '[arena-partial] %s\n' "$*" >&2; }

TMPDIR_LOCAL=$(mktemp -d /tmp/arena-partial-readback-XXXXXX)
cleanup() {
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

mkdir -p "$OUT"

# ── prereqs ───────────────────────────────────────────────────────────────────
need_cmds docker sha256sum
need_built

# ── seed ──────────────────────────────────────────────────────────────────────
log "seeding ${FNAME} (${FILE_MIB} MiB random) into ${S3_BUCKET}"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
dd if=/dev/urandom bs=1M count="$FILE_MIB" iflag=fullblock 2>/dev/null > "$TMPDIR_LOCAL/orig.bin"
WANT_HASH=$(sha256sum < "$TMPDIR_LOCAL/orig.bin" | awk '{print $1}')
WANT_SIZE=$(( FILE_MIB * 1024 * 1024 ))
docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$FNAME" \
    < "$TMPDIR_LOCAL/orig.bin" >/dev/null
printf 'prewarm: false\n' > "$TMPDIR_LOCAL/prewarm-none.cel"
chmod 644 "$TMPDIR_LOCAL/prewarm-none.cel"

start_arena() {
    ebpf_mount "$MOUNT"
    ebpf_start_daemon "$OUT/daemon-$1.log" \
        --bucket="$S3_BUCKET" \
        --policy="$TMPDIR_LOCAL/prewarm-none.cel" \
        --pattern-aware-admit=false \
        --metrics-addr="" \
        --sample-every=300s --sync-every=0
    sleep "$SETTLE"
}

# ── random-first ──────────────────────────────────────────────────────────────
log "random-first: 4 KiB in chunk 1, 4 KiB in chunk 0, then full read"
start_arena random
PROBE1=$(sudo dd if="$MOUNT/$FNAME" bs=4k skip=1500 count=1 2>/dev/null | wc -c)
PROBE0=$(sudo dd if="$MOUNT/$FNAME" bs=4k skip=10 count=1 2>/dev/null | wc -c)
sudo cat "$MOUNT/$FNAME" > "$TMPDIR_LOCAL/got.bin" || true
GOT_SIZE=$(stat -c %s "$TMPDIR_LOCAL/got.bin")
GOT_HASH=$(sha256sum < "$TMPDIR_LOCAL/got.bin" | awk '{print $1}')
STAT_SIZE=$(stat -c %s "$MOUNT/$FNAME")
UPCALLS_RANDOM=$(grep -c '\[upcall\] miss ino=' "$OUT/daemon-random.log" || true)
RANDOM_PASS=PASS
if [ "$PROBE1" -ne 4096 ] || [ "$PROBE0" -ne 4096 ] || \
   [ "$GOT_SIZE" -ne "$WANT_SIZE" ] || [ "$STAT_SIZE" -ne "$WANT_SIZE" ] || \
   [ "$GOT_HASH" != "$WANT_HASH" ]; then
    RANDOM_PASS=FAIL
fi
log "  probes=${PROBE1}/${PROBE0} read=${GOT_SIZE} stat=${STAT_SIZE} sha=$([ "$GOT_HASH" = "$WANT_HASH" ] && echo ok || echo mismatch) → ${RANDOM_PASS}"
ebpf_stop "$MOUNT" >/dev/null 2>&1 || true

# ── parallel ──────────────────────────────────────────────────────────────────
log "parallel: ${N_PAR} concurrent full reads"
start_arena parallel
pids=()
for i in $(seq 1 "$N_PAR"); do
    (sudo cat "$MOUNT/$FNAME" | sha256sum | awk '{print $1}' > "$TMPDIR_LOCAL/par-$i") &
    pids+=($!)
done
wait "${pids[@]}"
PAR_BAD=0
for i in $(seq 1 "$N_PAR"); do
    [ "$(cat "$TMPDIR_LOCAL/par-$i")" = "$WANT_HASH" ] || PAR_BAD=$(( PAR_BAD + 1 ))
done
PAR_PASS=PASS
[ "$PAR_BAD" -eq 0 ] || PAR_PASS=FAIL
log "  mismatched readers=${PAR_BAD}/${N_PAR} → ${PAR_PASS}"

# ── verdict ───────────────────────────────────────────────────────────────────
overall=PASS
[ "$RANDOM_PASS" = PASS ] && [ "$PAR_PASS" = PASS ] || overall=FAIL

{
echo ""
echo "=============================================================="
echo "test-arena-partial-readback  $(date +%Y-%m-%d)"
echo "=============================================================="
printf "object     : %s (%d MiB random), arena mode, chunk admits only\n" "$FNAME" "$FILE_MIB"
printf "random     : %s  (probes %d/%d B, read %d B, stat %d B, upcalls %d)\n" \
    "$RANDOM_PASS" "$PROBE1" "$PROBE0" "$GOT_SIZE" "$STAT_SIZE" "$UPCALLS_RANDOM"
printf "parallel   : %s  (%d/%d readers mismatched)\n" "$PAR_PASS" "$PAR_BAD" "$N_PAR"
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/arena-partial-readback-report.txt"

printf 'RESULT name=%s status=%s random=%s read_size=%d stat_size=%d par_bad=%d\n' \
    "$TEST_NAME" "$overall" "$RANDOM_PASS" "$GOT_SIZE" "$STAT_SIZE" "$PAR_BAD"
[[ "$overall" == FAIL ]] && \
    printf 'DETAIL chunk admit changed the file size or content (AC-48)\n'
[[ "$overall" == FAIL ]] && exit 1 || exit 0
