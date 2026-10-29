#!/usr/bin/env bash
# test-sync-keeps-cache.sh — a bucket sync that finds nothing new must not empty the cache.
#
# s3fs_ebpf_entries_replace() used to call shrink_dcache_sb() on every sync, changed
# listing or not.  That drops every unused dentry, so every idle inode is evicted and
# its chunks freed: the pcache content, the capacity accounting and the policy's view
# of what is resident, once per --sync-every (30 s by default).  Found in issue #22
# while diffing a live policy against its simulator (resident inos reported 0 bytes
# after each tick).  The fix shrinks only when the listing changed.
#
#   1. Mount in pcache mode with --sync-every=2s; read a file (one upcall).
#   2. Wait three sync ticks; read it again: NO new upcall.
#   3. Add an object to the bucket; after the next tick it must be readable (a changed
#      listing still refreshes the dentries).
#
# Usage: scripts/test-sync-keeps-cache.sh    Exit 0 = PASS.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-sync-keeps-cache}
SYNC=${SYNC:-2}
S3_BUCKET=${SYNC_CACHE_BUCKET:-s3fs-sync-cache}
TEST_NAME=${TEST_NAME:-test-sync-keeps-cache}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
mkdir -p "$OUT"
log() { printf '[sync-keeps-cache] %s\n' "$*" >&2; }
cleanup() { ebpf_stop "$MOUNT" 2>/dev/null || true; }
trap cleanup EXIT

need_cmds docker curl
need_built

docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
for i in 1 2 3; do
	printf 'sync-keeps-cache object %d\n' "$i" | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/obj$i.txt" >/dev/null
done

ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
	--metrics-addr=:9600 --sample-every=1s --sync-every="${SYNC}s"
sleep 4

upcalls() { curl -s "$METRICS_URL" | awk '$1=="s3fs_ebpf_read_miss_upcalls_total" { print $2 }'; }
rounds()  { curl -s "$METRICS_URL" | awk '$1=="s3fs_ebpf_sync_rounds_total" { print $2 }'; }

status=PASS
detail=""

sudo cat "$MOUNT/obj1.txt" >/dev/null
sleep 3                                  # let the counter mirror catch up
u0=$(upcalls); r0=$(rounds)
sleep $((SYNC * 3 + 2))                  # at least three ticks, listing unchanged
r1=$(rounds)
sudo cat "$MOUNT/obj1.txt" >/dev/null
sleep 3
u1=$(upcalls)
log "upcalls $u0 -> $u1 across $((r1 - r0)) sync rounds"
if [ $((r1 - r0)) -lt 2 ]; then status=FAIL; detail="$detail too-few-sync-rounds($((r1 - r0)))"; fi
if [ "$u1" != "$u0" ]; then status=FAIL; detail="$detail cache-dropped-by-sync(upcalls $u0->$u1)"; fi

# a changed listing still takes effect
printf 'added after mount\n' | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/obj4.txt" >/dev/null
sleep $((SYNC * 2 + 2))
got=$(sudo cat "$MOUNT/obj4.txt" 2>/dev/null || true)
if [ "$got" != "added after mount" ]; then status=FAIL; detail="$detail new-object-not-visible"; fi

echo "RESULT name=$TEST_NAME status=$status sync_rounds=$((r1 - r0)) upcalls_before=$u0 upcalls_after=$u1$detail"
[ "$status" = PASS ]
