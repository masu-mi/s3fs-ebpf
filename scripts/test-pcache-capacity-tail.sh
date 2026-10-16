#!/usr/bin/env bash
# test-pcache-capacity-tail.sh — AC-41 regression: capacity_bytes must bound
# the memory actually held in fd->buf, not just the admitted chunk bytes.
#
# s3fs_file_data_reserve sizes fd->buf to the populate end, so reading only
# the tail chunk of a 9 MiB file admits 1 MiB but allocates 9 MiB.  Before
# AC-41 the capacity loop charged the 1 MiB, so N tail reads stayed "under
# capacity" while fd->buf held N × 9 MiB.  AC-41 charges pcache inos by
# fd->cap; the gauge must settle at or below capacity_bytes.
#
# Test strategy:
#   1. Seed N_FILES × 9 MiB files into MinIO.
#   2. Start the daemon in pcache mode with capacity_bytes = CAPACITY and
#      --pattern-aware-admit=false (a sequential tail read would otherwise be
#      promoted to a full-file admit and hide the accounting gap).
#   3. For each file read only [8 MiB, 9 MiB), verify its sha256, then run a
#      policy eval round.
#      A generated CEL policy prewarms only a tiny sentinel object: default
#      prewarm would stage each 9 MiB file as an arena admit and fill
#      capacity_bytes by itself, while prewarming nothing leaves
#      s3fs_admitted_mm unset (no admit ever lands), so no upcall can fire.
#   4. After a final eval, s3fs_ebpf_fd_buf_bytes must be <= CAPACITY.
#
# Usage:
#   sudo ./scripts/test-pcache-capacity-tail.sh
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
OUT=${OUT:-/tmp/test-pcache-capacity-tail}
SETTLE=${SETTLE:-8}
N_FILES=${N_FILES:-4}
FILE_MIB=9
TAIL_SKIP_MIB=8                       # read [8 MiB, 9 MiB): the last chunk only
CAPACITY=${CAPACITY:-$(( 12 * 1024 * 1024 ))}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
DAEMON_METRICS_URL=${DAEMON_METRICS_URL:-http://localhost:9600}
TEST_NAME=${TEST_NAME:-test-pcache-capacity-tail}
# Dedicated bucket: objects other tests leave in the shared bucket are
# prewarmed as arena admits and push live_bytes over capacity on their own,
# so every tail would be evicted immediately and the test would pass vacuously.
S3_BUCKET=${CAP_TAIL_BUCKET:-s3fs-capacity-tail}

log()  { printf '[capacity-tail] %s\n' "$*" >&2; }

TMPDIR_LOCAL=$(mktemp -d /tmp/pcache-capacity-tail-XXXXXX)
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

policy_eval() {
    curl -sf -X POST "${DAEMON_METRICS_URL}/policy/eval" \
        -H 'Content-Type: application/json' \
        -d "{\"file\":\"$MOUNT/$1\"}" >/dev/null 2>&1 || true
}

# ── seed files into MinIO ─────────────────────────────────────────────────────
log "seeding ${N_FILES} files (${FILE_MIB} MiB each) into MinIO"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null

declare -a TAIL_HASHES
for i in $(seq 1 "$N_FILES"); do
    fname="capacity-tail-${i}.bin"
    orig="$TMPDIR_LOCAL/orig-${i}.bin"
    dd if=/dev/urandom bs=1M count="$FILE_MIB" iflag=fullblock 2>/dev/null > "$orig"
    TAIL_HASHES[$i]=$(dd if="$orig" bs=1M skip="$TAIL_SKIP_MIB" 2>/dev/null \
        | sha256sum | awk '{print $1}')
    docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$fname" < "$orig" >/dev/null
done
printf 'sentinel\n' | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/capacity-tail-sentinel.txt" >/dev/null
printf 'prewarm: entry.size < 4096\n' > "$TMPDIR_LOCAL/prewarm-sentinel.cel"
chmod 644 "$TMPDIR_LOCAL/prewarm-sentinel.cel"
log "seed done"

# ── mount in pcache mode ──────────────────────────────────────────────────────
log "mounting s3fs-ebpf in pcache mode (capacity_bytes=${CAPACITY})"
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" \
    --bucket="$S3_BUCKET" \
    --arena-metadata-only \
    --pattern-aware-admit=false \
    --policy="$TMPDIR_LOCAL/prewarm-sentinel.cel" \
    --policy-bpf="builtin:policy_observe" \
    --softcap=64 \
    --cache-capacity="${CAPACITY}" \
    --metrics-addr=:9600 \
    --sample-every=1s --sync-every=0
log "daemon started, settling ${SETTLE}s"
sleep "$SETTLE"

# ── tail reads ────────────────────────────────────────────────────────────────
TAIL_FAIL=0
FDBUF_PEAK=0
for i in $(seq 1 "$N_FILES"); do
    fname="capacity-tail-${i}.bin"
    got=$(sudo dd if="$MOUNT/$fname" bs=1M skip="$TAIL_SKIP_MIB" 2>/dev/null \
        | sha256sum | awk '{print $1}')
    if [ "$got" != "${TAIL_HASHES[$i]}" ]; then
        log "  ${fname}: tail sha256 mismatch"
        TAIL_FAIL=$(( TAIL_FAIL + 1 ))
    fi
    policy_eval "$fname"
    sleep 1.5   # let the 1s stats sampler publish the gauge
    v=$(get_metric s3fs_ebpf_fd_buf_bytes)
    [ "$v" -gt "$FDBUF_PEAK" ] && FDBUF_PEAK=$v
    log "  read tail of ${fname}: fd_buf=${v} B"
done

policy_eval "capacity-tail-1.bin"
sleep 2
FDBUF_SETTLED=$(get_metric s3fs_ebpf_fd_buf_bytes)
EVICTIONS=$(get_metric s3fs_ebpf_policy_evictions_total)

# ── verdict ───────────────────────────────────────────────────────────────────
overall=PASS
TAIL_PASS=PASS
[ "$TAIL_FAIL" -gt 0 ] && { TAIL_PASS=FAIL; overall=FAIL; }
if [ "$FDBUF_SETTLED" -lt 0 ]; then
    FDBUF_PASS="SKIP(metrics unavailable)"
    overall=FAIL
elif [ "$FDBUF_SETTLED" -gt "$CAPACITY" ]; then
    FDBUF_PASS=FAIL
    overall=FAIL
else
    FDBUF_PASS=PASS
fi

{
echo ""
echo "=============================================================="
echo "test-pcache-capacity-tail  $(date +%Y-%m-%d)"
echo "=============================================================="
printf "files      : %d × %d MiB, tail read [%d MiB, %d MiB) only\n" \
    "$N_FILES" "$FILE_MIB" "$TAIL_SKIP_MIB" "$FILE_MIB"
printf "capacity   : %d MiB\n" "$(( CAPACITY / 1048576 ))"
printf "fd_buf     : peak=%d B settled=%d B  evictions=%s\n" \
    "$FDBUF_PEAK" "$FDBUF_SETTLED" "$EVICTIONS"
echo ""
printf "tail sha256: %s\n" "$TAIL_PASS"
printf "fd_buf     : %s  (settled <= capacity)\n" "$FDBUF_PASS"
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/capacity-tail-report.txt"

printf 'RESULT name=%s status=%s tail_fail=%d fd_buf_peak=%d fd_buf_settled=%d capacity=%d\n' \
    "$TEST_NAME" "$overall" "$TAIL_FAIL" "$FDBUF_PEAK" "$FDBUF_SETTLED" "$CAPACITY"
[[ "$FDBUF_PASS" == FAIL ]] && \
    printf 'DETAIL fd_buf_settled=%d > capacity=%d (fd->buf not bounded by capacity_bytes — AC-41)\n' \
        "$FDBUF_SETTLED" "$CAPACITY"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
