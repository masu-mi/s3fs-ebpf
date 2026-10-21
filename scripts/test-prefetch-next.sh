#!/usr/bin/env bash
# test-prefetch-next.sh — AC-68: sibling read-ahead (CEL prefetch_next).
#
# In pcache mode, a read miss on the first chunk of a file evaluates the
# policy's `prefetch_next` and the daemon reads that many following files of
# the same directory (by name) through the mount.
#
# Test strategy:
#   1. Seed train/shard-0001 .. shard-0005 (1 MiB each) into a dedicated bucket.
#   2. Start the daemon in pcache mode with
#        prefetch_next: entry.s3_key.startsWith("train/") ? 2 : 0
#      and no content prewarm.
#   3. Read shard-0001 and wait for the prefetch of shard-0002 / 0003.
#   4. Read shard-0002 and shard-0003: sha256 must match and neither may take
#      a new upcall (the worker already loaded them).
#   5. Read shard-0005 directly (sha256 only).
#
# Usage:
#   sudo ./scripts/test-prefetch-next.sh
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
OUT=${OUT:-/tmp/test-prefetch-next}
READ_TIMEOUT=${READ_TIMEOUT:-60}
PREFETCH_WAIT=${PREFETCH_WAIT:-30}
SETTLE=${SETTLE:-8}
TEST_NAME=${TEST_NAME:-test-prefetch-next}
# Dedicated bucket so that only this test's objects are listed.
S3_BUCKET=${PREFETCH_NEXT_BUCKET:-s3fs-prefetch-next}

log()  { printf '[prefetch-next] %s\n' "$*" >&2; }

TMPDIR_LOCAL=$(mktemp -d /tmp/prefetch-next-XXXXXX)
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
for i in 1 2 3 4 5; do
    fname=$(printf 'shard-%04d' "$i")
    dd if=/dev/urandom bs=1M count=1 iflag=fullblock 2>/dev/null > "$TMPDIR_LOCAL/$fname"
    HASHES[$fname]=$(sha256sum < "$TMPDIR_LOCAL/$fname" | awk '{print $1}')
    docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/train/$fname" \
        < "$TMPDIR_LOCAL/$fname" >/dev/null
done
cat > "$TMPDIR_LOCAL/prefetch.cel" <<'CEL'
prewarm: false
prefetch_next: entry.s3_key.startsWith("train/") ? 2 : 0
CEL
chmod 644 "$TMPDIR_LOCAL/prefetch.cel"
log "seed done"

# ── mount in pcache mode ──────────────────────────────────────────────────────
log "mounting s3fs-ebpf in pcache mode with prefetch_next"
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" \
    --bucket="$S3_BUCKET" \
    --arena-metadata-only \
    --pcache-prewarm-mount="$MOUNT" \
    --policy="$TMPDIR_LOCAL/prefetch.cel" \
    --metrics-addr=:9600 \
    --sample-every=1s --sync-every=0
log "daemon started, settling ${SETTLE}s"
sleep "$SETTLE"

upcalls_for() {
    grep -cE "\[upcall\] ino=[0-9]+ $1 pcache" "$OUT/daemon.log" || true
}

FAIL=0
read_check() {
    local fname=$1 got
    got=$(sudo timeout "$READ_TIMEOUT" cat "$MOUNT/train/$fname" 2>/dev/null \
        | sha256sum | awk '{print $1}') || true
    if [ "$got" = "${HASHES[$fname]}" ]; then
        log "  ${fname}: sha256 OK"
    else
        log "  ${fname}: sha256 mismatch (got=${got})"
        FAIL=$(( FAIL + 1 ))
    fi
}

# ── reads ─────────────────────────────────────────────────────────────────────
read_check shard-0001
PREFETCH_LINE=""
for _ in $(seq 1 "$PREFETCH_WAIT"); do
    PREFETCH_LINE=$(grep -E '\[pcache-prewarm\] [0-9]+ files' "$OUT/daemon.log" | head -1 || true)
    [ -n "$PREFETCH_LINE" ] && break
    sleep 1
done
log "prefetch: ${PREFETCH_LINE:-not finished after ${PREFETCH_WAIT}s}"
[ -z "$PREFETCH_LINE" ] && FAIL=$(( FAIL + 1 ))

for fname in shard-0002 shard-0003; do
    before=$(upcalls_for "$fname")
    [ "$before" -gt 0 ] || { log "  ${fname}: was not prefetched"; FAIL=$(( FAIL + 1 )); }
    read_check "$fname"
    after=$(upcalls_for "$fname")
    if [ "$after" -ne "$before" ]; then
        log "  ${fname}: read took $(( after - before )) upcall(s) after prefetch"
        FAIL=$(( FAIL + 1 ))
    fi
done
read_check shard-0005

# ── verdict ───────────────────────────────────────────────────────────────────
overall=PASS
[ "$FAIL" -gt 0 ] && overall=FAIL

{
echo ""
echo "=============================================================="
echo "test-prefetch-next  $(date +%Y-%m-%d)"
echo "=============================================================="
echo "objects    : train/shard-0001 .. shard-0005 (1 MiB each)"
printf "prefetch   : %s\n" "${PREFETCH_LINE:-not finished}"
printf "failures   : %d\n" "$FAIL"
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/prefetch-next-report.txt"

printf 'RESULT name=%s status=%s failures=%d\n' "$TEST_NAME" "$overall" "$FAIL"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
