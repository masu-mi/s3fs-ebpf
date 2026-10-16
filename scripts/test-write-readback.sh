#!/usr/bin/env bash
# test-write-readback.sh — T1 regression: locally-written files must be
# readable immediately after write without -EIO.
#
# Prior to the T1 fix, s3fs_folio_fill_from_kmalloc checked the populated_chunks
# bitmap even for dirty (user-written) inodes.  Since .write never sets bitmap
# bits, buf_valid was always false for dirty inodes — every read_folio call on a
# user-written file returned -EIO.
#
# Five sub-tests are covered:
#   1. echo / cat round-trip on a newly-created file
#   2. Append that crosses the 4 MiB chunk boundary (sha256 check)
#   3. Truncate-extend (setattr ATTR_SIZE grow) then write + read
#   4. Flush to S3 (content verified in MinIO) → drop_caches → re-read
#   5. drop_caches right after close, before the flush PUT (W1: data must
#      survive inode eviction and still reach S3)
#
# The daemon does NOT need to be running for these tests: .write works entirely
# in-kernel (fd->buf), and the read-back path uses the dirty fast-path added by
# T1.  The daemon is started in pcache mode so .create ioctls succeed, but the
# written files are never admitted to S3 before the test reads them.
#
# Usage:
#   sudo ./scripts/test-write-readback.sh
#
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
#
# Requirements: kmod built (make kmod), daemon built (make daemon),
# MinIO up (make compose-up), docker, sha256sum.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-write-readback}
SETTLE=${SETTLE:-8}

CHUNK_BYTES=$(( 4 * 1024 * 1024 ))   # 4 MiB = S3FS_PCACHE_CHUNK_SHIFT
CROSS_CHUNK_EXTRA=$(( 512 * 1024 ))   # 0.5 MiB past the boundary

log()  { printf '[write-readback] %s\n' "$*" >&2; }

_RESULT_STATUS=FAIL
_RESULT_DETAIL=""
fail() {
    log "FAIL: $*"
    _RESULT_DETAIL="$*"
    exit 1
}

TMPDIR_LOCAL=$(mktemp -d /tmp/write-readback-XXXXXX)
cleanup() {
    printf 'RESULT name=test-write-readback status=%s\n' "$_RESULT_STATUS"
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

# ── fresh keys for sub-tests 4/5 (must precede daemon start: prewarm) ──────
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
# Remove leftover objects so the files are new to the daemon's entry table
# and the write goes through .create (local entry) — the W2 path.  A
# leftover from an earlier run would be prewarmed as a dyn entry instead.
minio_rm() {
    docker exec "$MINIO_CONTAINER" mc rm --force "local/$S3_BUCKET/$1" >/dev/null 2>&1 || true
}

minio_rm t4-flush-reread.bin
minio_rm t5-evict-before-flush.bin

# ── mount (no S3 files needed for this test) ──────────────────────────────────
log "mounting s3fs-ebpf"
ebpf_mount "$MOUNT"
# --sync-every=0: startup-only sync.  A periodic sync promotes .create'd
# files into the dyn table (with their S3 size), which masks W2 whenever a
# round lands between the PUT and drop_caches — keep sub-tests 4/5 on the
# local-entry path deterministically.
ebpf_start_daemon "$OUT/daemon.log" \
    --arena-metadata-only \
    --policy-bpf="builtin:policy_observe" \
    --softcap=8 --cache-capacity=0 \
    --metrics-addr=:9600 \
    --sample-every=1s --sync-every=0
log "daemon started, settling ${SETTLE}s"
sleep "$SETTLE"

# ── sub-test 1: echo/cat on new file ─────────────────────────────────────────
log "sub-test 1: echo/cat on new file"
TEST1_FILE="$MOUNT/t1-echo-test.txt"
EXPECTED1="hello s3fs-ebpf write-readback"
echo "$EXPECTED1" | sudo tee "$TEST1_FILE" > /dev/null
GOT1=$(sudo cat "$TEST1_FILE")
if [ "$GOT1" = "$EXPECTED1" ]; then
    log "  sub-test 1 PASS"
else
    log "  sub-test 1 FAIL: expected='$EXPECTED1' got='$GOT1'"
    exit 1
fi

# ── sub-test 2: append across 4 MiB chunk boundary ───────────────────────────
log "sub-test 2: write crossing 4 MiB chunk boundary"
TEST2_FILE="$MOUNT/t1-cross-chunk.bin"
ORIG2="$TMPDIR_LOCAL/orig2.bin"
TOTAL_SIZE=$(( CHUNK_BYTES + CROSS_CHUNK_EXTRA ))

# Create reference file locally
dd if=/dev/urandom bs=1024 count=$(( TOTAL_SIZE / 1024 )) \
   iflag=fullblock 2>/dev/null > "$ORIG2"
ORIG2_HASH=$(sha256sum "$ORIG2" | awk '{print $1}')
log "  reference sha256: $ORIG2_HASH (${TOTAL_SIZE} bytes)"

# Write to the mount point
sudo dd if="$ORIG2" of="$TEST2_FILE" bs=65536 2>/dev/null

# Read back and compare
READ2_HASH=$(sudo sha256sum "$TEST2_FILE" | awk '{print $1}')
log "  readback sha256:  $READ2_HASH"
if [ "$READ2_HASH" = "$ORIG2_HASH" ]; then
    log "  sub-test 2 PASS"
else
    log "  sub-test 2 FAIL: sha256 mismatch"
    exit 1
fi

# ── sub-test 3: truncate-extend then write + read ────────────────────────────
log "sub-test 3: truncate-extend (setattr) then write and read"
TEST3_FILE="$MOUNT/t1-truncate-extend.bin"
ORIG3="$TMPDIR_LOCAL/orig3.bin"
EXTEND_SIZE=$(( CHUNK_BYTES / 2 + 1024 ))   # 2 MiB + 1 KiB

dd if=/dev/urandom bs=1024 count=$(( EXTEND_SIZE / 1024 )) \
   iflag=fullblock 2>/dev/null > "$ORIG3"
ORIG3_HASH=$(sha256sum "$ORIG3" | awk '{print $1}')
log "  reference sha256: $ORIG3_HASH (${EXTEND_SIZE} bytes)"

# Create a small file then truncate-extend; conv=notrunc preserves the
# extended size so dd writes over the zero-padded region without O_TRUNC.
echo "seed" | sudo tee "$TEST3_FILE" > /dev/null
sudo truncate -s "$EXTEND_SIZE" "$TEST3_FILE"
sudo dd if="$ORIG3" of="$TEST3_FILE" bs=65536 conv=notrunc 2>/dev/null

READ3_HASH=$(sudo sha256sum "$TEST3_FILE" | awk '{print $1}')
log "  readback sha256:  $READ3_HASH"
if [ "$READ3_HASH" = "$ORIG3_HASH" ]; then
    log "  sub-test 3 PASS"
else
    log "  sub-test 3 FAIL: sha256 mismatch"
    exit 1
fi

# ── helpers for sub-tests 4/5: MinIO object content ─────────────────────────
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1

minio_sha() {
    docker exec "$MINIO_CONTAINER" mc cat "local/$S3_BUCKET/$1" 2>/dev/null \
        | sha256sum | awk '{print $1}'
}

# Wait (up to $3 s) until the MinIO object's content matches sha $2.
# Existence alone is not enough: an object left over from an earlier run
# satisfies `mc stat` before the daemon's PUT has happened.
wait_minio_sha() {
    local key=$1 want=$2 timeout=$3
    for _ in $(seq 1 "$timeout"); do
        [ "$(minio_sha "$key")" = "$want" ] && return 0
        sleep 1
    done
    return 1
}

# ── sub-test 4: flush to S3, drop page cache, re-read (U5 path) ──────────────
# Writes a file and closes it (triggering async flush to S3).  Waits for the
# daemon's PUT to land with the new content, then drops the VFS page cache so
# the next read must go through the miss path (dirty=false, bitmap possibly
# empty).  Verifies that content is still correct after a cold re-read.
# W2: the inode is evicted after the PUT; the re-created inode must take its
# size from the local entry (it read back empty before W2).
log "sub-test 4: flush → drop_caches → re-read (bitmap-empty path)"
TEST4_FILE="$MOUNT/t4-flush-reread.bin"
TEST4_KEY="t4-flush-reread.bin"
ORIG4="$TMPDIR_LOCAL/orig4.bin"
dd if=/dev/urandom bs=65536 count=16 iflag=fullblock 2>/dev/null > "$ORIG4"
ORIG4_HASH=$(sha256sum "$ORIG4" | awk '{print $1}')
log "  reference sha256: $ORIG4_HASH"

# Write and close (triggers fire-and-forget flush)
sudo dd if="$ORIG4" of="$TEST4_FILE" bs=65536 2>/dev/null

if ! wait_minio_sha "$TEST4_KEY" "$ORIG4_HASH" 30; then
    log "  sub-test 4 FAIL: flush timeout — MinIO content != reference after 30s"
    exit 1
fi
log "  flush confirmed in MinIO (content matches)"

# Drop VFS page cache — next read must re-fetch via daemon upcall
echo 3 | sudo tee /proc/sys/vm/drop_caches > /dev/null

READ4_HASH=$(sudo sha256sum "$TEST4_FILE" | awk '{print $1}')
log "  re-read sha256:   $READ4_HASH"
if [ "$READ4_HASH" = "$ORIG4_HASH" ]; then
    log "  sub-test 4 PASS"
else
    log "  sub-test 4 FAIL: sha256 mismatch after flush+drop_caches"
    exit 1
fi

# ── sub-test 5: drop_caches BEFORE the flush PUT (W1 regression) ─────────────
# Close, then drop caches immediately — while the daemon's coalescer still
# holds the flush event.  Before W1, drop_caches evicted the inode and
# evict_inode freed the unflushed fd->buf: the PUT never happened (daemon log
# "read dirty: no such file or directory") and the re-read returned whatever
# S3 held before (nothing, for a new file).
log "sub-test 5: write → close → drop_caches before PUT → S3 + re-read"
TEST5_FILE="$MOUNT/t5-evict-before-flush.bin"
TEST5_KEY="t5-evict-before-flush.bin"
ORIG5="$TMPDIR_LOCAL/orig5.bin"
dd if=/dev/urandom bs=65536 count=16 iflag=fullblock 2>/dev/null > "$ORIG5"
ORIG5_HASH=$(sha256sum "$ORIG5" | awk '{print $1}')
log "  reference sha256: $ORIG5_HASH"

sudo dd if="$ORIG5" of="$TEST5_FILE" bs=65536 2>/dev/null
echo 3 | sudo tee /proc/sys/vm/drop_caches > /dev/null

READ5_HASH=$(sudo sha256sum "$TEST5_FILE" | awk '{print $1}')
log "  re-read sha256:   $READ5_HASH"
if [ "$READ5_HASH" != "$ORIG5_HASH" ]; then
    log "  sub-test 5 FAIL: re-read mismatch — unflushed write lost on inode eviction"
    exit 1
fi
if ! wait_minio_sha "$TEST5_KEY" "$ORIG5_HASH" 30; then
    log "  sub-test 5 FAIL: MinIO content != reference after 30s — flush lost on inode eviction"
    exit 1
fi
log "  sub-test 5 PASS"

# ── report ────────────────────────────────────────────────────────────────────
{
echo ""
echo "=============================================================="
echo "test-write-readback  $(date +%Y-%m-%d)"
echo "=============================================================="
echo "sub-test 1 (echo/cat new file)             : PASS"
echo "sub-test 2 (cross-chunk write/read)        : PASS"
echo "sub-test 3 (truncate-extend + write)       : PASS"
echo "sub-test 4 (flush+drop_caches re-read)     : PASS"
echo "sub-test 5 (drop_caches before flush PUT)   : PASS"
echo ""
echo "OVERALL: PASS"
echo "=============================================================="
} | tee "$OUT/write-readback-report.txt"

_RESULT_STATUS=PASS
exit 0
