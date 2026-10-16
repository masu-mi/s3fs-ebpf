#!/usr/bin/env bash
# test-pcache-capacity-noeval.sh — AC-45 regression: capacity_bytes must bound
# fd->buf memory even when no policy eval / sync round ever runs.
#
# The capacity eviction only ran inside kmod's admit_replace.  A pcache miss
# (populate ioctls + signal_admit) never called it, so reading file after
# file kept every fd->buf until some unrelated eval came along: N files of
# F MiB held N x F MiB against a capacity of a few MiB.  C2 makes the daemon
# run the pass after a miss's last populate (before signal_admit).
#
# Test strategy:
#   1. Seed N_FILES x FILE_MIB random files into a dedicated bucket.
#   2. Start the daemon in pcache mode with capacity_bytes = CAPACITY and
#      --pattern-aware-admit=false (every 4 MiB chunk is its own miss).
#   3. Read each file in full and verify its sha256.  POST /policy/eval is
#      NEVER called and the sync loop is off.
#   4. s3fs_ebpf_fd_buf_bytes must settle at or below CAPACITY, and the
#      policy must have evicted something.
#
# Usage:
#   sudo ./scripts/test-pcache-capacity-noeval.sh
#
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
#
# Requirements: ebpf kmod + daemon built (make all), MinIO up (make compose-up),
# docker, sha256sum, curl.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-pcache-capacity-noeval}
SETTLE=${SETTLE:-8}
N_FILES=${N_FILES:-4}
FILE_MIB=${FILE_MIB:-9}
CAPACITY=${CAPACITY:-$(( 12 * 1024 * 1024 ))}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
TEST_NAME=${TEST_NAME:-test-pcache-capacity-noeval}
S3_BUCKET=${CAP_NOEVAL_BUCKET:-s3fs-capacity-noeval}

log()  { printf '[capacity-noeval] %s\n' "$*" >&2; }

TMPDIR_LOCAL=$(mktemp -d /tmp/pcache-capacity-noeval-XXXXXX)
cleanup() {
    fd_buf_audit
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

mkdir -p "$OUT"

# ── prereqs ───────────────────────────────────────────────────────────────────
need_cmds docker sha256sum curl
need_built

# Fetch /metrics into a variable first (see test-pcache-evict-race.sh).
get_metric() {
    local out
    out=$(curl -sf "${METRICS_URL}" 2>/dev/null) || { echo -1; return; }
    printf '%s\n' "$out" | awk -v m="$1" '
        index($0, m) == 1 && substr($0, length(m) + 1, 1) ~ /[{ ]/ { v = int($NF) }
        END { print (v == "" ? -1 : v) }'
}

# ── seed files into MinIO ─────────────────────────────────────────────────────
log "seeding ${N_FILES} files (${FILE_MIB} MiB each) into ${S3_BUCKET}"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null

declare -a HASHES
for i in $(seq 1 "$N_FILES"); do
    fname="capacity-noeval-${i}.bin"
    orig="$TMPDIR_LOCAL/orig-${i}.bin"
    dd if=/dev/urandom bs=1M count="$FILE_MIB" iflag=fullblock 2>/dev/null > "$orig"
    HASHES[$i]=$(sha256sum < "$orig" | awk '{print $1}')
    docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$fname" < "$orig" >/dev/null
done
log "seed done"

# ── mount in pcache mode ──────────────────────────────────────────────────────
log "mounting s3fs-ebpf in pcache mode (capacity_bytes=${CAPACITY}, no eval)"
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" \
    --bucket="$S3_BUCKET" \
    --arena-metadata-only \
    --pattern-aware-admit=false \
    --policy-bpf="builtin:policy_observe" \
    --softcap=64 \
    --cache-capacity="${CAPACITY}" \
    --metrics-addr=:9600 \
    --sample-every=1s --sync-every=0
log "daemon started, settling ${SETTLE}s"
sleep "$SETTLE"

# ── full reads, no eval ───────────────────────────────────────────────────────
READ_FAIL=0
FDBUF_PEAK=0
for i in $(seq 1 "$N_FILES"); do
    fname="capacity-noeval-${i}.bin"
    got=$(sudo cat "$MOUNT/$fname" | sha256sum | awk '{print $1}')
    if [ "$got" != "${HASHES[$i]}" ]; then
        log "  ${fname}: sha256 mismatch"
        READ_FAIL=$(( READ_FAIL + 1 ))
    fi
    sleep 1.5   # let the 1s stats sampler publish the gauge
    v=$(get_metric s3fs_ebpf_fd_buf_bytes)
    [ "$v" -gt "$FDBUF_PEAK" ] && FDBUF_PEAK=$v
    log "  read ${fname}: fd_buf=${v} B"
done

sleep 2
FDBUF_SETTLED=$(get_metric s3fs_ebpf_fd_buf_bytes)
EVICTIONS=$(get_metric s3fs_ebpf_policy_evictions_total)

# ── verdict ───────────────────────────────────────────────────────────────────
overall=PASS
READ_PASS=PASS
[ "$READ_FAIL" -gt 0 ] && { READ_PASS=FAIL; overall=FAIL; }
if [ "$FDBUF_SETTLED" -lt 0 ]; then
    FDBUF_PASS="SKIP(metrics unavailable)"
    overall=FAIL
elif [ "$FDBUF_SETTLED" -gt "$CAPACITY" ]; then
    FDBUF_PASS=FAIL
    overall=FAIL
else
    FDBUF_PASS=PASS
fi
EVICT_PASS=PASS
if [ "$EVICTIONS" -le 0 ]; then
    EVICT_PASS=FAIL
    overall=FAIL
fi

{
echo ""
echo "=============================================================="
echo "test-pcache-capacity-noeval  $(date +%Y-%m-%d)"
echo "=============================================================="
printf "files      : %d x %d MiB, full read each, no /policy/eval\n" "$N_FILES" "$FILE_MIB"
printf "capacity   : %d MiB\n" "$(( CAPACITY / 1048576 ))"
printf "fd_buf     : peak=%d B settled=%d B  evictions=%s\n" \
    "$FDBUF_PEAK" "$FDBUF_SETTLED" "$EVICTIONS"
echo ""
printf "sha256     : %s\n" "$READ_PASS"
printf "fd_buf     : %s  (settled <= capacity)\n" "$FDBUF_PASS"
printf "evictions  : %s  (> 0)\n" "$EVICT_PASS"
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/capacity-noeval-report.txt"

printf 'RESULT name=%s status=%s read_fail=%d fd_buf_peak=%d fd_buf_settled=%d capacity=%d evictions=%s\n' \
    "$TEST_NAME" "$overall" "$READ_FAIL" "$FDBUF_PEAK" "$FDBUF_SETTLED" "$CAPACITY" "$EVICTIONS"
[[ "$FDBUF_PASS" == FAIL ]] && \
    printf 'DETAIL fd_buf_settled=%d > capacity=%d (no capacity eviction after populate — AC-45)\n' \
        "$FDBUF_SETTLED" "$CAPACITY"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
