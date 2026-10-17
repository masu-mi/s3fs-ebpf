#!/usr/bin/env bash
# test-write-existing.sh — AC-50 regression: modifying an EXISTING object
# without reading it first must not destroy the rest of it.
#
# open(2) does not load the object, so a write / truncate on a file nobody
# has read started from an empty buffer (fd->size == 0): the flush then PUT
# only what had been written (an overwrite in the middle truncated the
# object to the written range, a shrink produced zeros, `: > f` never
# reached S3).  Each scenario below modifies a fresh 6 MiB random object
# (chunk 0 full + 2 MiB in chunk 1) through the mount without reading it,
# waits for the flush PUT, and compares the object in MinIO (and a re-read
# through the mount) with the expected bytes computed locally.
#
#   overwrite : 4 KiB of random data at 1 MiB and at 5 MiB (conv=notrunc)
#   shrink    : truncate to 5 MiB
#   extend    : truncate to 8 MiB (the new tail reads as zeros)
#   append    : 4 KiB appended
#   empty     : `: > f` (truncate to 0)
#   otrunc    : O_TRUNC open + small write (replaces the content)
#   append2   : append, drop caches, append again.  The daemon's size for the
#               object is the pre-append one (--sync-every=0), so the load must
#               follow the kmod's i_size or the second append fails with EIO.
#
# Run in both admit modes (MODES="pcache arena"): the loader fills the fd
# through POPULATE_PAGES either way.
#
# Usage:
#   sudo ./scripts/test-write-existing.sh
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
OUT=${OUT:-/tmp/test-write-existing}
SETTLE=${SETTLE:-6}
FLUSH_WAIT=${FLUSH_WAIT:-30}
TEST_NAME=${TEST_NAME:-test-write-existing}
S3_BUCKET=${WRITE_EXISTING_BUCKET:-s3fs-write-existing}
MIB=$(( 1024 * 1024 ))
SIZE=$(( 6 * MIB ))
MODES=${MODES:-"pcache arena"}

log()  { printf '[write-existing] %s\n' "$*" >&2; }

TMPDIR_LOCAL=$(mktemp -d /tmp/write-existing-XXXXXX)
cleanup() {
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

mkdir -p "$OUT"

# ── prereqs ───────────────────────────────────────────────────────────────────
need_cmds docker sha256sum
need_built

SCENARIOS=(overwrite shrink extend append empty otrunc append2)

head -c 4096 /dev/urandom > "$TMPDIR_LOCAL/patch1"
head -c 4096 /dev/urandom > "$TMPDIR_LOCAL/patch2"
head -c 4096 /dev/urandom > "$TMPDIR_LOCAL/append"
printf 'replaced by O_TRUNC\n' > "$TMPDIR_LOCAL/small"

overall=PASS
declare -A S3OK READOK
BAD=""

run_mode() {
    local mode=$1 sc f p got rd deadline
    local -A WANT

    log "[$mode] seeding ${#SCENARIOS[@]} x 6 MiB random objects into ${S3_BUCKET}"
    docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
        minioadmin minioadmin >/dev/null 2>&1
    docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
    docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null

    for sc in "${SCENARIOS[@]}"; do
        f="$TMPDIR_LOCAL/$sc.exp"
        dd if=/dev/urandom bs=1M count=6 iflag=fullblock 2>/dev/null > "$f"
        docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$sc.bin" < "$f" >/dev/null
        case $sc in
            overwrite)
                dd if="$TMPDIR_LOCAL/patch1" of="$f" bs=1M seek=1 conv=notrunc 2>/dev/null
                dd if="$TMPDIR_LOCAL/patch2" of="$f" bs=1M seek=5 conv=notrunc 2>/dev/null ;;
            shrink)  truncate -s $(( 5 * MIB )) "$f" ;;
            extend)  truncate -s $(( 8 * MIB )) "$f" ;;
            append)  cat "$TMPDIR_LOCAL/append" >> "$f" ;;
            empty)   : > "$f" ;;
            otrunc)  cp "$TMPDIR_LOCAL/small" "$f" ;;
            append2) cat "$TMPDIR_LOCAL/append" "$TMPDIR_LOCAL/append" >> "$f" ;;
        esac
        WANT[$sc]=$(sha256sum < "$f" | awk '{print $1}')
    done

    # The objects are listed by the startup sync; arena mode also prewarms them.
    log "[$mode] mounting s3fs-ebpf"
    ebpf_mount "$MOUNT"
    local extra=()
    [ "$mode" = pcache ] && extra=(--arena-metadata-only)
    ebpf_start_daemon "$OUT/daemon-$mode.log" \
        --bucket="$S3_BUCKET" \
        "${extra[@]}" \
        --policy-bpf="builtin:policy_observe" \
        --softcap=8 --cache-capacity=0 \
        --metrics-addr=:9600 \
        --sample-every=1s --sync-every=0
    sleep "$SETTLE"

    # Modify through the mount WITHOUT reading first.
    for sc in "${SCENARIOS[@]}"; do
        p="$MOUNT/$sc.bin"
        case $sc in
            overwrite)
                sudo dd if="$TMPDIR_LOCAL/patch1" of="$p" bs=1M seek=1 conv=notrunc 2>/dev/null
                sudo dd if="$TMPDIR_LOCAL/patch2" of="$p" bs=1M seek=5 conv=notrunc 2>/dev/null ;;
            shrink)  sudo truncate -s $(( 5 * MIB )) "$p" ;;
            extend)  sudo truncate -s $(( 8 * MIB )) "$p" ;;
            append)  sudo sh -c "cat '$TMPDIR_LOCAL/append' >> '$p'" ;;
            empty)   sudo sh -c ": > '$p'" ;;
            otrunc)  sudo sh -c "cat '$TMPDIR_LOCAL/small' > '$p'" ;;
            append2) sudo sh -c "cat '$TMPDIR_LOCAL/append' >> '$p'"
                     sleep 2; sudo sysctl -q -w vm.drop_caches=3; sleep 1
                     sudo sh -c "cat '$TMPDIR_LOCAL/append' >> '$p'" ;;
        esac
    done

    # Verify: the object in MinIO (after the async flush) and a re-read.
    for sc in "${SCENARIOS[@]}"; do
        got=""; deadline=$(( SECONDS + FLUSH_WAIT ))
        while :; do
            got=$(docker exec "$MINIO_CONTAINER" mc cat "local/$S3_BUCKET/$sc.bin" 2>/dev/null \
                  | sha256sum | awk '{print $1}')
            [ "$got" = "${WANT[$sc]}" ] && break
            [ $SECONDS -ge $deadline ] && break
            sleep 1
        done
        if [ "$got" = "${WANT[$sc]}" ]; then S3OK[$mode.$sc]=PASS; else S3OK[$mode.$sc]=FAIL; overall=FAIL; fi
        rd=$(sudo cat "$MOUNT/$sc.bin" 2>/dev/null | sha256sum | awk '{print $1}')
        if [ "$rd" = "${WANT[$sc]}" ]; then READOK[$mode.$sc]=PASS; else READOK[$mode.$sc]=FAIL; overall=FAIL; fi
        log "  [$mode] ${sc}: S3 object ${S3OK[$mode.$sc]}, re-read ${READOK[$mode.$sc]}"
        if [ "${S3OK[$mode.$sc]}" != PASS ] || [ "${READOK[$mode.$sc]}" != PASS ]; then
            BAD="$BAD $mode:$sc"
        fi
    done
    ebpf_stop "$MOUNT" >/dev/null 2>&1 || true
}

for m in $MODES; do run_mode "$m"; done

{
echo ""
echo "=============================================================="
echo "test-write-existing  $(date +%Y-%m-%d)"
echo "=============================================================="
printf "%-8s %-10s %-10s %-10s\n" mode scenario S3-object re-read
for m in $MODES; do
    for sc in "${SCENARIOS[@]}"; do
        printf "%-8s %-10s %-10s %-10s\n" "$m" "$sc" "${S3OK[$m.$sc]}" "${READOK[$m.$sc]}"
    done
done
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/write-existing-report.txt"

printf 'RESULT name=%s status=%s failed=%s\n' "$TEST_NAME" "$overall" "${BAD:- none}"
[[ "$overall" == FAIL ]] && \
    printf 'DETAIL scenarios that lost or corrupted data:%s (AC-50)\n' "$BAD"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
