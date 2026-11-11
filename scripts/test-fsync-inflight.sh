#!/usr/bin/env bash
# test-fsync-inflight.sh — fsync of a file whose close(2) flush is still in flight
# must not return before the data is in S3 (AC-96, model finding FE-6).
#
# write + close on fd A starts an async flush (the daemon debounces it 200 ms).  A
# second open file B of the same inode is clean (fd->dirty was cleared by the flush
# enqueue), so fsync(B) used to find nothing to do and return 0 at once, with the
# object still stale.  Now it waits for the flush that covers the data.
#
# Both orders are tried: fsync right after close (the flush is still parked), and
# fsync of a clean file that has a flush in flight while a newer write is also pending.
#
# Usage: sudo ./scripts/test-fsync-inflight.sh      Exit: 0 PASS, 1 FAIL.
set -euo pipefail
TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
S3_BUCKET=${FSYNC_INFLIGHT_BUCKET:-s3fs-fsync-inflight}
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-fsync-inflight}
TEST_NAME=${TEST_NAME:-test-fsync-inflight}
mkdir -p "$OUT"
trap 'ebpf_stop "$MOUNT" >/dev/null 2>&1 || true' EXIT
need_cmds docker sha256sum python3
need_built
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
    --policy-bpf="builtin:policy_observe_ops" --softcap=8 --cache-capacity=0 \
    --metrics-addr=:9600 --sample-every=1s --sync-every=0
sleep 3
head -c $((2 * 1024 * 1024)) /dev/urandom > "$OUT/payload"
want=$(sha256sum < "$OUT/payload" | awk '{print $1}')
# For each file: open A and B, write on A, close A (async flush), fsync(B), and ask S3
# at once whether the object holds the payload.
sudo python3 - "$OUT/payload" "$MOUNT" "$S3_BUCKET" "$MINIO_CONTAINER" "$want" <<'PY'
import hashlib, os, subprocess, sys
payload, mount, bucket, container, want = sys.argv[1:6]
d = open(payload, 'rb').read()
res = {}
for n in range(6):
    path = '%s/inflight-%d.bin' % (mount, n)
    a = os.open(path, os.O_CREAT | os.O_WRONLY | os.O_TRUNC, 0o644)
    # the .create placeholder flush is not what is under test: let it land
    os.fsync(a)
    b = os.open(path, os.O_RDONLY)
    os.write(a, d)
    os.close(a)            # async flush starts; fd->dirty is now clear
    os.fsync(b)            # clean file, flush in flight
    got = subprocess.run(['docker', 'exec', container, 'mc', 'cat', 'local/%s/inflight-%d.bin' % (bucket, n)],
                         capture_output=True).stdout
    res[n] = hashlib.sha256(got).hexdigest() == want
    os.close(b)
print(' '.join('%d=%s' % (n, 'PASS' if ok else 'FAIL') for n, ok in sorted(res.items())))
sys.exit(0 if all(res.values()) else 1)
PY
rc=$?
overall=PASS; [ "$rc" = 0 ] || overall=FAIL
printf 'RESULT name=%s status=%s\n' "$TEST_NAME" "$overall"
[[ "$overall" == PASS ]]
