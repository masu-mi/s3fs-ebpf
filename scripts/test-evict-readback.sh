#!/usr/bin/env bash
# test-evict-readback.sh — Q1/S8 regression: eviction must not alter the
# visible file size (i_size) or corrupt the readable content.
#
# Q1: s3fs_file_data_reset must NOT call i_size_write(0). i_size reflects the
# S3 object size and must survive eviction so stat(2) and read_iter return the
# correct file size after eviction.
#
# S8: populate_pages must only GROW i_size, never shrink it. A partial
# re-populate (e.g. chunk 0 only of a 9 MiB file) must not report 4 MiB to
# user-space.
#
# Test strategy:
#   1. Seed a 9 MiB file (> 2 × 4 MiB chunk, partial-admit path) into MinIO.
#   2. Mount in pcache mode with 1-chunk capacity (forces eviction).
#   3. Read the full file (admits all 3 chunks).
#   4. Force eviction via POST /policy/eval.
#   5. Verify stat(2) still reports 9 MiB (Q1 check).
#   6. Read the file again end-to-end and verify sha256 (S8 + correctness).
#
# Usage:
#   sudo ./scripts/test-evict-readback.sh
#
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
#
# Requirements: kmod + daemon built (make all), MinIO up (make compose-up),
# docker, sha256sum, curl.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-evict-readback}
SETTLE=${SETTLE:-10}
# 9 MiB: > 2 × 4 MiB so at least 3 POPULATE_PAGES calls
FILE_SIZE=$(( 9 * 1024 * 1024 ))
TEST_FILE="evict-readback-test.bin"
CHUNK_BYTES=$(( 4 * 1024 * 1024 ))
DAEMON_METRICS_URL=${DAEMON_METRICS_URL:-http://localhost:9600}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}

log()  { printf '[evict-readback] %s\n' "$*" >&2; }
fail() { log "FAIL: $*"; exit 1; }

TMPDIR_LOCAL=$(mktemp -d /tmp/evict-readback-XXXXXX)
cleanup() {
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

mkdir -p "$OUT"

# ── prereqs ───────────────────────────────────────────────────────────────────
need_cmds docker sha256sum curl
need_built

# ── seed file ─────────────────────────────────────────────────────────────────
log "generating ${FILE_SIZE}B test file"
ORIG="$TMPDIR_LOCAL/orig.bin"
dd if=/dev/urandom bs=$(( 1024 * 1024 )) count=9 iflag=fullblock 2>/dev/null > "$ORIG"
ORIG_HASH=$(sha256sum "$ORIG" | awk '{print $1}')
log "original sha256: ${ORIG_HASH}"

log "seeding ${TEST_FILE} into MinIO"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$TEST_FILE" < "$ORIG" >/dev/null
log "seed done"

# ── mount with 1-chunk capacity to force eviction ────────────────────────────
log "mounting s3fs-ebpf in pcache mode (capacity_bytes=${CHUNK_BYTES}, forces eviction)"
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" \
    --arena-metadata-only \
    --policy-bpf="builtin:policy_observe" \
    --softcap=2 \
    --cache-capacity="${CHUNK_BYTES}" \
    --metrics-addr=:9600 \
    --sample-every=1s --sync-every=5s
log "daemon started, settling ${SETTLE}s"
sleep "$SETTLE"

get_evict_count() {
    curl -sf "${METRICS_URL}" 2>/dev/null \
        | awk '/^s3fs_ebpf_policy_evictions_total[{ ]/{print int($NF); exit}' \
        || echo "-1"
}

# ── step 1: full read to populate all chunks ─────────────────────────────────
log "step 1: cold read to populate all 3 chunks"
sudo cat "$MOUNT/$TEST_FILE" > /dev/null
log "initial read done"

# ── step 2: check file size before eviction ───────────────────────────────────
SIZE_BEFORE=$(sudo stat -c %s "$MOUNT/$TEST_FILE")
log "stat size before eviction: ${SIZE_BEFORE} (expected ${FILE_SIZE})"
if [ "$SIZE_BEFORE" -ne "$FILE_SIZE" ]; then
    fail "pre-eviction size wrong: expected ${FILE_SIZE}, got ${SIZE_BEFORE}"
fi

# ── step 3: force eviction ────────────────────────────────────────────────────
EVICT_BEFORE=$(get_evict_count)
log "evictions before: ${EVICT_BEFORE}"

log "step 3: forcing eviction via POST /policy/eval"
for _ in $(seq 1 5); do
    curl -sf -X POST "${DAEMON_METRICS_URL}/policy/eval" \
        -H 'Content-Type: application/json' \
        -d "{\"file\":\"$MOUNT/$TEST_FILE\"}" \
        >/dev/null 2>&1 || true
    sleep 0.5
done
sleep 2

EVICT_AFTER=$(get_evict_count)
EVICT_DELTA=$(( EVICT_AFTER - EVICT_BEFORE ))
log "evictions after: ${EVICT_AFTER} (delta=${EVICT_DELTA})"

EVICT_PASS=PASS
if [ "$EVICT_BEFORE" -eq -1 ] || [ "$EVICT_AFTER" -eq -1 ]; then
    EVICT_PASS=SKIP
elif [ "$EVICT_DELTA" -lt 1 ]; then
    log "FAIL: no evictions occurred (delta=${EVICT_DELTA}) — test is vacuous (check capacity_bytes setting)"
    EVICT_PASS=FAIL
fi

# ── step 4: check file size after eviction (Q1 check) ────────────────────────
log "step 4: stat after eviction (Q1: i_size must survive eviction)"
SIZE_AFTER=$(sudo stat -c %s "$MOUNT/$TEST_FILE")
log "stat size after eviction: ${SIZE_AFTER} (expected ${FILE_SIZE})"
Q1_PASS=FAIL
if [ "$SIZE_AFTER" -eq "$FILE_SIZE" ]; then
    Q1_PASS=PASS
    log "  Q1 PASS: i_size=${SIZE_AFTER} preserved"
else
    log "  Q1 FAIL: i_size=${SIZE_AFTER} ≠ ${FILE_SIZE}"
fi

# ── step 5: full re-read and sha256 check (S8 + correctness) ─────────────────
log "step 5: re-reading full file after eviction (S8: partial re-populate must not shrink size)"
READ_HASH=$(sudo sha256sum "$MOUNT/$TEST_FILE" | awk '{print $1}')
log "re-read sha256: ${READ_HASH}"
SHA_PASS=FAIL
[ "$READ_HASH" = "$ORIG_HASH" ] && SHA_PASS=PASS

# ── step 6: final size check (S8 check after re-populate) ────────────────────
SIZE_FINAL=$(sudo stat -c %s "$MOUNT/$TEST_FILE")
log "stat size after re-read: ${SIZE_FINAL} (expected ${FILE_SIZE})"
S8_PASS=FAIL
[ "$SIZE_FINAL" -eq "$FILE_SIZE" ] && S8_PASS=PASS

# ── report ────────────────────────────────────────────────────────────────────
overall=PASS
[ "$Q1_PASS" = "FAIL" ] && overall=FAIL
[ "$SHA_PASS" = "FAIL" ] && overall=FAIL
[ "$S8_PASS" = "FAIL" ] && overall=FAIL
[[ "$EVICT_PASS" == FAIL ]] && overall=FAIL

{
echo ""
echo "=============================================================="
echo "test-evict-readback  $(date +%Y-%m-%d)"
echo "=============================================================="
printf "file size      : %d MiB\n" "$(( FILE_SIZE / 1024 / 1024 ))"
printf "evictions      : before=%s after=%s delta=%d\n" \
    "$EVICT_BEFORE" "$EVICT_AFTER" "$EVICT_DELTA"
echo ""
printf "Q1 (i_size survives eviction)            : %s  (before=%d after=%d)\n" \
    "$Q1_PASS" "$SIZE_BEFORE" "$SIZE_AFTER"
printf "sha256 (content correct after re-read)   : %s\n" "$SHA_PASS"
printf "S8 (i_size not shrunk by partial repop.) : %s  (final=%d)\n" \
    "$S8_PASS" "$SIZE_FINAL"
printf "evictions occurred                       : %s  (delta=%d)\n" \
    "$EVICT_PASS" "$EVICT_DELTA"
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/evict-readback-report.txt"

[[ "$overall" == FAIL ]] && exit 1 || exit 0
