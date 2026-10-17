#!/usr/bin/env bash
# test-fsync-new-file.sh — a new file written + fsync'ed must stay what the
# fsync PUT wrote.
#
# .create enqueues an empty placeholder flush on the normal queue (drained by
# the daemon's 100 ms ticker, then debounced 200 ms).  fsync's event goes to
# the wait queue and is served at once, so the stale 0-byte placeholder was
# PUT after the fsync's PUT and truncated the object to 0 bytes.  flush_enqueue
# now drops the queued normal items of the ino when a WAIT event arrives.
#
# Usage: sudo ./scripts/test-fsync-new-file.sh      Exit: 0 PASS, 1 FAIL.
set -euo pipefail
TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
S3_BUCKET=${FSYNC_NEW_BUCKET:-s3fs-fsync-new}
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-fsync-new-file}
TEST_NAME=${TEST_NAME:-test-fsync-new-file}
mkdir -p "$OUT"
trap 'ebpf_stop "$MOUNT" >/dev/null 2>&1 || true' EXIT
need_cmds docker sha256sum python3
need_built
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
    --policy-bpf="builtin:policy_observe" --softcap=8 --cache-capacity=0 \
    --metrics-addr=:9600 --sample-every=1s --sync-every=0
sleep 3
head -c $((3 * 1024 * 1024)) /dev/urandom > "$OUT/payload"
want=$(sha256sum < "$OUT/payload" | awk '{print $1}')
sudo python3 - "$OUT/payload" "$MOUNT" <<'PY'
import os, sys
d = open(sys.argv[1], 'rb').read()
for n in ('a', 'b', 'c'):
    fd = os.open('%s/fs-%s.bin' % (sys.argv[2], n), os.O_CREAT | os.O_WRONLY | os.O_TRUNC, 0o644)
    os.write(fd, d)
    os.fsync(fd)
    if n == 'b':
        os.ftruncate(fd, len(d))
    os.close(fd)
PY
sleep 3   # > ticker (100 ms) + debounce (200 ms): a stale placeholder would have fired
overall=PASS
for n in a b c; do
    got=$(docker exec "$MINIO_CONTAINER" mc cat "local/$S3_BUCKET/fs-$n.bin" 2>/dev/null | sha256sum | awk '{print $1}')
    if [ "$got" = "$want" ]; then echo "fs-$n.bin PASS"; else echo "fs-$n.bin FAIL"; overall=FAIL; fi
done
printf 'RESULT name=%s status=%s\n' "$TEST_NAME" "$overall"
[[ "$overall" == PASS ]]
