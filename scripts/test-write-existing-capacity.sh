#!/usr/bin/env bash
# test-write-existing-capacity.sh — AC-50 race regression: modifying an
# existing object bigger than capacity_bytes, while other reads keep the
# capacity pass busy, must still PUT the whole object.
#
# s3fs_fd_load_for_modify unpins a chunk before the writer takes fd->lock;
# a shed (s3fs_ino_shed_unpinned) or admit_evict_ino -> reset in that window
# freed the chunk and the write / truncate then flushed a hole as zeros.
# write and setattr now re-check under fd->lock and load again.
#
# Strategy: capacity 12 MiB; N_FILES objects of FILE_MIB (24 MiB, bigger
# than the capacity) are modified in parallel (4 KiB patch in the middle,
# or a shrink to SHRINK_MIB) without reading them first, while a background
# loop reads a 40 MiB pressure file front to back.  Each object in MinIO
# and a re-read through the mount must match the locally computed bytes.
#
# Usage: sudo ./scripts/test-write-existing-capacity.sh
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-write-existing-capacity}
SETTLE=${SETTLE:-6}
FLUSH_WAIT=${FLUSH_WAIT:-60}
TEST_NAME=${TEST_NAME:-test-write-existing-capacity}
S3_BUCKET=${WRITE_CAP_BUCKET:-s3fs-write-capacity}
CAPACITY=${CAPACITY:-$(( 12 * 1024 * 1024 ))}
FILE_MIB=${FILE_MIB:-24}
SHRINK_MIB=${SHRINK_MIB:-17}
N_FILES=${N_FILES:-4}          # per kind (patch / shrink)
ROUNDS=${ROUNDS:-3}
MIB=$(( 1024 * 1024 ))

log() { printf '[write-capacity] %s\n' "$*" >&2; }

TMPDIR_LOCAL=$(mktemp -d /tmp/write-capacity-XXXXXX)
PRESS_PID=""
cleanup() {
    [ -n "$PRESS_PID" ] && kill "$PRESS_PID" 2>/dev/null || true
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT
mkdir -p "$OUT"

need_cmds docker sha256sum
need_built

mc() { docker exec "$MINIO_CONTAINER" mc "$@"; }
mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1

head -c 4096 /dev/urandom > "$TMPDIR_LOCAL/patch"
overall=PASS; BAD=""
declare -A WANT

for round in $(seq 1 "$ROUNDS"); do
    log "round $round: seeding"
    mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
    mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
    dd if=/dev/urandom bs=1M count=40 iflag=fullblock 2>/dev/null > "$TMPDIR_LOCAL/press.bin"
    docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/press.bin" < "$TMPDIR_LOCAL/press.bin" >/dev/null
    for i in $(seq 1 "$N_FILES"); do
        for kind in patch shrink; do
            f="$TMPDIR_LOCAL/$kind$i.exp"
            dd if=/dev/urandom bs=1M count="$FILE_MIB" iflag=fullblock 2>/dev/null > "$f"
            docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$kind$i.bin" < "$f" >/dev/null
            if [ "$kind" = patch ]; then
                dd if="$TMPDIR_LOCAL/patch" of="$f" bs=1M seek=$(( 8 + i )) conv=notrunc 2>/dev/null
            else
                truncate -s $(( (SHRINK_MIB + i) * MIB )) "$f"
            fi
            WANT[$kind$i]=$(sha256sum < "$f" | awk '{print $1}')
        done
    done

    # Two writers racing on one clean object: the loser used to get -EIO
    # (the daemon answered -EBUSY once the winner had made the fd dirty).
    head -c 4096 /dev/urandom > "$TMPDIR_LOCAL/patchB"
    for i in $(seq 1 "$N_FILES"); do
        f="$TMPDIR_LOCAL/conc$i.exp"
        dd if=/dev/urandom bs=1M count=6 iflag=fullblock 2>/dev/null > "$f"
        docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/conc$i.bin" < "$f" >/dev/null
        dd if="$TMPDIR_LOCAL/patch" of="$f" bs=1M seek=1 conv=notrunc 2>/dev/null
        dd if="$TMPDIR_LOCAL/patchB" of="$f" bs=1M seek=4 conv=notrunc 2>/dev/null
        WANT[conc$i]=$(sha256sum < "$f" | awk '{print $1}')
    done

    ebpf_mount "$MOUNT"
    ebpf_start_daemon "$OUT/daemon-$round.log" \
        --bucket="$S3_BUCKET" --arena-metadata-only \
        --policy-bpf="builtin:policy_observe" \
        --softcap=64 --cache-capacity="$CAPACITY" \
        --metrics-addr=:9600 --sample-every=100ms --sync-every=0
    sleep "$SETTLE"

    ( while :; do sudo cat "$MOUNT/press.bin" >/dev/null 2>&1; done ) &
    PRESS_PID=$!
    sleep 1

    pids=()
    for i in $(seq 1 "$N_FILES"); do
        sudo dd if="$TMPDIR_LOCAL/patch" of="$MOUNT/patch$i.bin" bs=1M seek=$(( 8 + i )) conv=notrunc 2>/dev/null &
        pids+=($!)
        sudo truncate -s $(( (SHRINK_MIB + i) * MIB )) "$MOUNT/shrink$i.bin" &
        pids+=($!)
        sudo dd if="$TMPDIR_LOCAL/patch" of="$MOUNT/conc$i.bin" bs=1M seek=1 conv=notrunc 2>/dev/null &
        pids+=($!)
        sudo dd if="$TMPDIR_LOCAL/patchB" of="$MOUNT/conc$i.bin" bs=1M seek=4 conv=notrunc 2>/dev/null &
        pids+=($!)
    done
    for p in "${pids[@]}"; do wait "$p" || { log "  writer pid $p failed"; overall=FAIL; BAD="$BAD r$round:writer"; }; done
    kill "$PRESS_PID" 2>/dev/null || true; PRESS_PID=""

    for name in "${!WANT[@]}"; do
        got=""; deadline=$(( SECONDS + FLUSH_WAIT ))
        while :; do
            got=$(mc cat "local/$S3_BUCKET/$name.bin" 2>/dev/null | sha256sum | awk '{print $1}')
            [ "$got" = "${WANT[$name]}" ] && break
            [ $SECONDS -ge $deadline ] && break
            sleep 1
        done
        rd=$(sudo cat "$MOUNT/$name.bin" 2>/dev/null | sha256sum | awk '{print $1}')
        s=PASS; [ "$got" = "${WANT[$name]}" ] || s=FAIL
        r=PASS; [ "$rd" = "${WANT[$name]}" ] || r=FAIL
        log "  r$round $name: S3 $s, re-read $r"
        if [ "$s" != PASS ] || [ "$r" != PASS ]; then overall=FAIL; BAD="$BAD r$round:$name"; fi
    done
    ebpf_stop "$MOUNT" >/dev/null 2>&1 || true
done

echo "OVERALL: $overall" | tee "$OUT/write-capacity-report.txt"
printf 'RESULT name=%s status=%s failed=%s\n' "$TEST_NAME" "$overall" "${BAD:- none}"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
