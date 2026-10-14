#!/usr/bin/env bash
# test-write-readback.sh — T1 regression: locally-written files must be
# readable immediately after write without -EIO.
#
# Prior to the T1 fix, s3fs_folio_fill_from_kmalloc checked the populated_chunks
# bitmap even for dirty (user-written) inodes.  Since .write never sets bitmap
# bits, buf_valid was always false for dirty inodes — every read_folio call on a
# user-written file returned -EIO.
#
# Three sub-tests are covered:
#   1. echo / cat round-trip on a newly-created file
#   2. Append that crosses the 4 MiB chunk boundary (sha256 check)
#   3. Truncate-extend (setattr ATTR_SIZE grow) then write + read
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
fail() { log "FAIL: $*"; exit 1; }

TMPDIR_LOCAL=$(mktemp -d /tmp/write-readback-XXXXXX)
cleanup() {
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

mkdir -p "$OUT"

# ── prereqs ───────────────────────────────────────────────────────────────────
need_cmds docker sha256sum
need_built

# ── mount (no S3 files needed for this test) ──────────────────────────────────
log "mounting s3fs-ebpf"
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" \
    --arena-metadata-only \
    --policy-bpf="builtin:policy_observe" \
    --softcap=8 --cache-capacity=0 \
    --metrics-addr=:9600 \
    --sample-every=1s --sync-every=5s
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

# ── sub-test 4: flush to S3, drop page cache, re-read (U5 path) ──────────────
# Writes a file and closes it (triggering async flush to S3).  Waits for the
# daemon to PUT the object to MinIO, then drops the VFS page cache so the next
# read must go through the miss path (dirty=false, bitmap possibly empty).
# Verifies that content is still correct after a cold re-read via daemon upcall.
log "sub-test 4: flush → drop_caches → re-read (bitmap-empty path)"
TEST4_FILE="$MOUNT/t4-flush-reread.bin"
TEST4_KEY="t4-flush-reread.bin"
ORIG4="$TMPDIR_LOCAL/orig4.bin"
dd if=/dev/urandom bs=65536 count=16 iflag=fullblock 2>/dev/null > "$ORIG4"
ORIG4_HASH=$(sha256sum "$ORIG4" | awk '{print $1}')
log "  reference sha256: $ORIG4_HASH"

# Write and close (triggers fire-and-forget flush)
sudo dd if="$ORIG4" of="$TEST4_FILE" bs=65536 2>/dev/null

# Poll MinIO until the daemon's PUT lands (up to 30s)
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
FLUSH_OK=false
for _ in $(seq 1 30); do
    if docker exec "$MINIO_CONTAINER" mc stat \
            "local/$S3_BUCKET/$TEST4_KEY" >/dev/null 2>&1; then
        FLUSH_OK=true
        break
    fi
    sleep 1
done
if ! $FLUSH_OK; then
    log "  sub-test 4 FAIL: flush timeout — MinIO object not found after 30s"
    exit 1
fi
log "  flush confirmed in MinIO"

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
echo ""
echo "OVERALL: PASS"
echo "=============================================================="
} | tee "$OUT/write-readback-report.txt"

exit 0
