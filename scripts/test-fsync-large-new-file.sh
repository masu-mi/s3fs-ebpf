#!/usr/bin/env bash
# test-fsync-large-new-file.sh — fsync of a NEW large file means the object is in S3 (issue #35).
#
# `dd conv=fsync` of a new 100 MiB file: .create queues a 0-byte placeholder flush; the ticker
# loop parks it in the coalescer (200 ms debounce) while the data is still being written. When
# the fsync (WAIT) event arrived, the parked placeholder was not cancelled, and it fired inside
# or after the fsync's PUT:
#   * its READ_DIRTY made kmod treat the fsync's flush as covered, so fsync returned BEFORE the
#     PUT had landed (3-6 of 10 runs at 100 MiB), and
#   * it PUT ev.Size == 0 bytes over the finished object (the object ended up empty).
#
#   For each of N new files: dd conv=fsync, then at once HEAD the object — its Content-Length
#   must already be the full size.  After all of them and a settle time, every object must hold
#   the payload's sha256 (none overwritten by a late empty PUT).
#
# Usage: sudo -E scripts/test-fsync-large-new-file.sh   (N=12 SIZE_MIB=100 POLICY=builtin:...)
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-fsync-large-new-file}
N=${N:-12}
SIZE_MIB=${SIZE_MIB:-100}
SETTLE=${SETTLE:-3}
POLICY=${POLICY:-builtin:policy_observe_ops}
S3_BUCKET=${FSYNC_LARGE_BUCKET:-s3fs-fsync-large}
TEST_NAME=${TEST_NAME:-test-fsync-large-new-file}
mkdir -p "$OUT"
cleanup() { ebpf_stop "$MOUNT" 2>/dev/null || true; }
trap cleanup EXIT
need_cmds docker curl sha256sum dd
need_built

mc() { docker exec "$MINIO_CONTAINER" mc "$@"; }
mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
head_len() { # <key> — Content-Length of the object, empty if absent
	curl -sI --aws-sigv4 "aws:amz:us-east-1:s3" --user "minioadmin:minioadmin" "$S3_URL/$S3_BUCKET/$1" \
		| tr -d '\r' | awk -F': ' 'tolower($1)=="content-length"{print $2}'
}

dd if=/dev/urandom of="$OUT/payload" bs=1M count="$SIZE_MIB" iflag=fullblock 2>/dev/null
want=$(sha256sum < "$OUT/payload" | awk '{print $1}')
size=$((SIZE_MIB * 1024 * 1024))

ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
	--policy-bpf="$POLICY" --softcap=8 --cache-capacity=0 \
	--metrics-addr=:9600 --sample-every=1s --sync-every=0
sleep 4

early=0
for i in $(seq 1 "$N"); do
	sudo dd if="$OUT/payload" of="$MOUNT/f-$i.bin" bs=1M conv=fsync 2>/dev/null
	got=$(head_len "f-$i.bin")
	if [ "$got" != "$size" ]; then
		echo "f-$i.bin: fsync returned with Content-Length='${got:-absent}', want $size" >&2
		early=$((early + 1))
	fi
done
sleep "$SETTLE"

bad=0
for i in $(seq 1 "$N"); do
	h=$(mc cat "local/$S3_BUCKET/f-$i.bin" | sha256sum | awk '{print $1}')
	if [ "$h" != "$want" ]; then
		echo "f-$i.bin: object is $(head_len "f-$i.bin") B, sha256 differs from the payload" >&2
		bad=$((bad + 1))
	fi
done
echo "files=$N fsync_returned_early=$early wrong_content=$bad"

overall=PASS
[ "$early" -eq 0 ] || overall=FAIL
[ "$bad" -eq 0 ] || overall=FAIL
mc rb --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
printf 'RESULT name=%s status=%s files=%s early=%s wrong=%s\n' "$TEST_NAME" "$overall" "$N" "$early" "$bad"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
