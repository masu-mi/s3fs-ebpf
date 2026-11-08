#!/usr/bin/env bash
# test-nocache-signal.sh — a signal during the upcall wait must not turn a cold read into -EIO.
#
# `dev microbench --op=read` is a Go program: its runtime sends SIGURG to its own
# threads every few ms to preempt goroutines, so signals land inside read(2).  With
# nocache=1 every open drops the chunk table, so every read is a cold read that waits
# for the daemon (an upcall).  The wait used to be interruptible: a signal ended it with
# a negative result that matched neither branch of read_folio, which fell through to the
# fallback fill, found no chunk, and returned -EIO ("warmup #N: read ...: input/output
# error" within the first few hundred reads).  The wait is killable now.
#
#   1. Mount in nocache mode, run the microbench read loop (ITERS opens + reads).
#      Expect: exit 0 (no EIO) and read_miss_upcalls grew by >= ITERS (every read
#      really was a cold read, so the loop exercised the wait).
#
# Usage: scripts/test-nocache-signal.sh     Exit 0 = PASS, 1 = FAIL.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-nocache-signal}
ITERS=${ITERS:-3000}
S3_BUCKET=${NOCACHE_SIGNAL_BUCKET:-s3fs-nocache-signal}
TEST_NAME=${TEST_NAME:-test-nocache-signal}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
mkdir -p "$OUT"
cleanup() {
	ebpf_stop "$MOUNT" 2>/dev/null || true
	docker exec "$MINIO_CONTAINER" mc rb --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
}
trap cleanup EXIT
need_cmds docker curl
need_built

docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
printf 'hello, nocache signal\n' | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/hello.txt" >/dev/null

ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
	--policy-bpf=builtin:policy_observe_ops --metrics-addr=:9600 --sample-every=1s --sync-every=0
sleep 3
ebpf_nocache 1

metric() { curl -s "$METRICS_URL" | awk -v m="$1" '$1==m { print $2+0 }'; }
u0=$(metric s3fs_ebpf_read_miss_upcalls_total)

overall=PASS
bad() { echo "FAIL: $*" >&2; overall=FAIL; }

rc=0
sudo "$S3FS_BIN" dev microbench --op=read --path="$MOUNT/hello.txt" --iters="$ITERS" \
	--label=nocache-signal >"$OUT/microbench.log" 2>&1 || rc=$?
tail -3 "$OUT/microbench.log"
[ "$rc" -eq 0 ] || bad "microbench exited $rc (a read returned an error): $(grep -m1 'error' "$OUT/microbench.log" || true)"

sleep 2
u1=$(metric s3fs_ebpf_read_miss_upcalls_total)
echo "nocache reads: upcalls +$((u1 - u0)) over $ITERS iterations (+1000 warmup)"
[ $((u1 - u0)) -ge "$ITERS" ] || bad "upcalls grew by $((u1 - u0)), want >= $ITERS (reads were not cold)"

ebpf_nocache 0
printf 'RESULT name=%s status=%s iters=%d upcalls=%d\n' "$TEST_NAME" "$overall" "$ITERS" "$((u1 - u0))"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
