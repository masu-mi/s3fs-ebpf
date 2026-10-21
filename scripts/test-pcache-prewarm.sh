#!/usr/bin/env bash
# test-pcache-prewarm.sh — AC-67: content prewarm in pcache mode.
#
# With --arena-metadata-only --pcache-prewarm the daemon reads each file the
# prewarm policy selects once through the mount after startup, so the first
# read by a user is served from the page cache without an upcall.
#
# Test strategy:
#   1. Seed warm-small.txt, warm-large.bin (FILE_MIB, default 9 MiB = three
#      4 MiB chunks) and cold.bin into a dedicated bucket.
#   2. Start the daemon in pcache mode with `prewarm: entry.name.startsWith("warm-")`
#      and --pcache-prewarm, and wait for the "[pcache-prewarm] N files" line.
#   3. Read the warm files: sha256 must match and the daemon must log no new
#      pcache upcall for them.
#   4. Read cold.bin: sha256 must match and it must take an upcall (the policy
#      did not select it).
#
# Usage:
#   sudo ./scripts/test-pcache-prewarm.sh
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
OUT=${OUT:-/tmp/test-pcache-prewarm}
FILE_MIB=${FILE_MIB:-9}
READ_TIMEOUT=${READ_TIMEOUT:-60}
PREWARM_WAIT=${PREWARM_WAIT:-60}
TEST_NAME=${TEST_NAME:-test-pcache-prewarm}
# Dedicated bucket so that only this test's objects are listed.
S3_BUCKET=${PCACHE_PREWARM_BUCKET:-s3fs-pcache-prewarm}

log()  { printf '[pcache-prewarm] %s\n' "$*" >&2; }

TMPDIR_LOCAL=$(mktemp -d /tmp/pcache-prewarm-XXXXXX)
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
printf 'pcache prewarm small object for AC-67\n' > "$TMPDIR_LOCAL/warm-small.txt"
dd if=/dev/urandom bs=1M count="$FILE_MIB" iflag=fullblock 2>/dev/null \
    > "$TMPDIR_LOCAL/warm-large.bin"
dd if=/dev/urandom bs=1M count=1 iflag=fullblock 2>/dev/null \
    > "$TMPDIR_LOCAL/cold.bin"
for fname in warm-small.txt warm-large.bin cold.bin; do
    HASHES[$fname]=$(sha256sum < "$TMPDIR_LOCAL/$fname" | awk '{print $1}')
    docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$fname" \
        < "$TMPDIR_LOCAL/$fname" >/dev/null
done
printf 'prewarm: entry.name.startsWith("warm-")\n' > "$TMPDIR_LOCAL/prewarm-warm.cel"
chmod 644 "$TMPDIR_LOCAL/prewarm-warm.cel"
log "seed done"

# ── mount in pcache mode with --pcache-prewarm ───────────────────────────────
log "mounting s3fs-ebpf in pcache mode with --pcache-prewarm"
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" \
    --bucket="$S3_BUCKET" \
    --arena-metadata-only \
    --pcache-prewarm \
    --pcache-prewarm-mount="$MOUNT" \
    --policy="$TMPDIR_LOCAL/prewarm-warm.cel" \
    --metrics-addr=:9600 \
    --sample-every=1s --sync-every=0

PREWARM_LINE=""
for _ in $(seq 1 "$PREWARM_WAIT"); do
    PREWARM_LINE=$(grep -E '\[pcache-prewarm\] [0-9]+ files' "$OUT/daemon.log" | head -1 || true)
    [ -n "$PREWARM_LINE" ] && break
    sleep 1
done
log "prewarm: ${PREWARM_LINE:-not finished after ${PREWARM_WAIT}s}"

upcalls_for() {
    grep -cE "\[upcall\] ino=[0-9]+ $1 pcache" "$OUT/daemon.log" || true
}

# ── reads ─────────────────────────────────────────────────────────────────────
FAIL=0
read_check() {
    local fname=$1 got
    got=$(sudo timeout "$READ_TIMEOUT" cat "$MOUNT/$fname" 2>/dev/null \
        | sha256sum | awk '{print $1}') || true
    if [ "$got" = "${HASHES[$fname]}" ]; then
        log "  ${fname}: sha256 OK"
    else
        log "  ${fname}: sha256 mismatch (got=${got})"
        FAIL=$(( FAIL + 1 ))
    fi
}

[ -z "$PREWARM_LINE" ] && FAIL=$(( FAIL + 1 ))
for fname in warm-small.txt warm-large.bin; do
    before=$(upcalls_for "$fname")
    [ "$before" -gt 0 ] || { log "  ${fname}: prewarm took no upcall"; FAIL=$(( FAIL + 1 )); }
    read_check "$fname"
    after=$(upcalls_for "$fname")
    if [ "$after" -ne "$before" ]; then
        log "  ${fname}: first read took $(( after - before )) upcall(s) after prewarm"
        FAIL=$(( FAIL + 1 ))
    fi
done
before=$(upcalls_for cold.bin)
read_check cold.bin
after=$(upcalls_for cold.bin)
if [ "$after" -le "$before" ]; then
    log "  cold.bin: expected an upcall (not selected by the policy)"
    FAIL=$(( FAIL + 1 ))
fi

# ── verdict ───────────────────────────────────────────────────────────────────
overall=PASS
[ "$FAIL" -gt 0 ] && overall=FAIL

{
echo ""
echo "=============================================================="
echo "test-pcache-prewarm  $(date +%Y-%m-%d)"
echo "=============================================================="
printf "objects    : warm-small.txt, warm-large.bin (%d MiB), cold.bin\n" "$FILE_MIB"
printf "prewarm    : %s\n" "${PREWARM_LINE:-not finished}"
printf "failures   : %d\n" "$FAIL"
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/pcache-prewarm-report.txt"

printf 'RESULT name=%s status=%s failures=%d\n' "$TEST_NAME" "$overall" "$FAIL"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
