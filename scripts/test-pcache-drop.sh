#!/usr/bin/env bash
# test-pcache-drop.sh — pcache_drop=1 drops only the page cache on open.
#
# With pcache_drop each open runs invalidate_inode_pages2 but keeps the
# admit ranges and the chunk table, so every read enters .read_folio and a
# chunk-table hit returns without an upcall.
#
#   1. Mount in pcache mode, cat the file once (a miss: one upcall, the
#      chunk table now holds it).
#   2. pcache_drop=1; cat it N more times, each from a fresh open.
#      Expect: s3fs_ebpf_folio_hits_total grows by >= N (each open
#      reaches .read_folio) and s3fs_ebpf_read_miss_upcalls_total does
#      not grow (no daemon round trip).  sha256 matches every time.
#   3. nocache=1 and pcache_drop=1 together: nocache wins, so the next cat
#      upcalls again (admit was dropped).
#
# Usage: scripts/test-pcache-drop.sh     Exit 0 = PASS, 1 = FAIL.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-pcache-drop}
N=${N:-8}
S3_BUCKET=${PCACHE_DROP_BUCKET:-s3fs-pcache-drop}
TEST_NAME=${TEST_NAME:-test-pcache-drop}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
mkdir -p "$OUT"
cleanup() { ebpf_stop "$MOUNT" 2>/dev/null || true; }
trap cleanup EXIT
need_cmds docker curl sha256sum
need_built

docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
head -c $((3 * 1024 * 1024)) /dev/urandom > "$OUT/a.bin"
WANT=$(sha256sum < "$OUT/a.bin" | cut -d' ' -f1)
docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/a.bin" < "$OUT/a.bin" >/dev/null

ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
	--policy-bpf=builtin:policy_observe_ops --softcap=64 --cache-capacity=0 \
	--metrics-addr=:9600 --sample-every=1s --sync-every=0
sleep 4

metric() { curl -s "$METRICS_URL" | awk -v m="$1" '$1==m { print $2+0 }'; }
folio_hits() { metric s3fs_ebpf_folio_hits_total; }
upcalls()    { metric s3fs_ebpf_read_miss_upcalls_total; }
sum() { sudo sha256sum < "$MOUNT/a.bin" | cut -d' ' -f1; }

overall=PASS
bad() { echo "FAIL: $*" >&2; overall=FAIL; }

[ "$(sum)" = "$WANT" ] || bad "first read sha256 mismatch"   # miss: upcall + admit
sleep 2
h0=$(folio_hits); u0=$(upcalls)

ebpf_pcache_drop 1
for _ in $(seq "$N"); do
	[ "$(sum)" = "$WANT" ] || bad "sha256 mismatch under pcache_drop"
done
sleep 2
h1=$(folio_hits); u1=$(upcalls)
echo "pcache_drop: folio_hits +$((h1 - h0)) upcalls +$((u1 - u0)) over $N opens"
[ $((h1 - h0)) -ge "$N" ] || bad "folio_hits grew by $((h1 - h0)), want >= $N (page cache not dropped on open?)"
[ "$u1" -eq "$u0" ] || bad "upcalls grew by $((u1 - u0)) under pcache_drop (chunk table not kept?)"

# nocache wins over pcache_drop.
ebpf_nocache 1
[ "$(sum)" = "$WANT" ] || bad "sha256 mismatch under nocache+pcache_drop"
sleep 2
u2=$(upcalls)
[ "$u2" -gt "$u1" ] || bad "nocache+pcache_drop did not upcall (nocache should win)"
ebpf_nocache 0
ebpf_pcache_drop 0

docker exec "$MINIO_CONTAINER" mc rb --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
printf 'RESULT name=%s status=%s\n' "$TEST_NAME" "$overall"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
