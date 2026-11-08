#!/usr/bin/env bash
# test-cold-signal.sh — a signal during the upcall wait must not turn an ordinary
# (cache-mode) cold read into -EIO.
#
# AC-87 was found with nocache=1 and fixed by making the upcall wait killable
# (test-nocache-signal).  This is the same check without nocache: N distinct small
# files, each read once for the first time, so every read is a cold read that waits
# for the daemon (an upcall) and registers a chunk.  `dev microbench --op=read` is a Go
# program whose runtime sends SIGURG to its own threads every few ms, so signals land
# inside read(2).
#
#   1. Put N files in the bucket, mount in the default cache mode.
#   2. microbench reads file-0 .. file-(N-1) once each.
#      Expect: exit 0 (no EIO) and read_miss_upcalls grew by >= N.
#
# Usage: scripts/test-cold-signal.sh     Exit 0 = PASS, 1 = FAIL.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-cold-signal}
ITERS=${ITERS:-3000}
S3_BUCKET=${COLD_SIGNAL_BUCKET:-s3fs-cold-signal}
TEST_NAME=${TEST_NAME:-test-cold-signal}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
mkdir -p "$OUT"
cleanup() {
	ebpf_stop "$MOUNT" 2>/dev/null || true
	docker exec "$MINIO_CONTAINER" mc rb --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
	docker exec "$MINIO_CONTAINER" rm -rf /tmp/cold-signal >/dev/null 2>&1 || true
}
trap cleanup EXIT
need_cmds docker curl
need_built

docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
docker exec "$MINIO_CONTAINER" sh -c "rm -rf /tmp/cold-signal && mkdir -p /tmp/cold-signal && i=0 && while [ \$i -lt $ITERS ]; do echo cold-\$i > /tmp/cold-signal/f\$i; i=\$((i+1)); done && mc cp --recursive /tmp/cold-signal/ local/$S3_BUCKET/ >/dev/null"

ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
	--policy-bpf=builtin:policy_observe_ops --metrics-addr=:9600 --sample-every=1s --sync-every=0
sleep 3

metric() { curl -s "$METRICS_URL" | awk -v m="$1" '$1==m { print $2+0 }'; }
u0=$(metric s3fs_ebpf_read_miss_upcalls_total)

overall=PASS
bad() { echo "FAIL: $*" >&2; overall=FAIL; }

rc=0
sudo "$S3FS_BIN" dev microbench --op=read --path="$MOUNT/f%d" --iters="$ITERS" --warmup=0 \
	--label=cold-signal >"$OUT/microbench.log" 2>&1 || rc=$?
tail -3 "$OUT/microbench.log"
[ "$rc" -eq 0 ] || bad "microbench exited $rc (a read returned an error): $(grep -m1 'error' "$OUT/microbench.log" || true)"

sleep 2
u1=$(metric s3fs_ebpf_read_miss_upcalls_total)
echo "cold reads: upcalls +$((u1 - u0)) over $ITERS distinct files"
[ $((u1 - u0)) -ge "$ITERS" ] || bad "upcalls grew by $((u1 - u0)), want >= $ITERS (reads were not cold)"

printf 'RESULT name=%s status=%s iters=%d upcalls=%d\n' "$TEST_NAME" "$overall" "$ITERS" "$((u1 - u0))"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
