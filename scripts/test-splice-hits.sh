#!/usr/bin/env bash
# test-splice-hits.sh — sendfile / splice reads are counted as hits (hit_accum).
#
# With hit_accum=1 the kmod counts one access per open file in .read_iter.  sendfile(2),
# splice(2) and copy_file_range(2) reach the page cache through .splice_read and never
# touch .read_iter, so s3fs_ebpf_splice_read counts too.  Without it a file served to a
# web server by sendfile would look cold to the policy.
#
#   1. Mount in pcache mode with a capacity (so a miss runs an admit_replace round) and a
#      pick_victim policy.
#   2. cat file A once (a miss: it enters the cache), then sendfile it N times, each
#      from a fresh open (one access per open).
#   3. cat file B (a miss) to run a round; the round replays what was counted.
#   4. s3fs_ebpf_policy_hits_replayed_total must have grown by at least N.
#
# Usage: scripts/test-splice-hits.sh     Exit 0 = PASS.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-splice-hits}
N=${N:-6}
S3_BUCKET=${SPLICE_HITS_BUCKET:-s3fs-splice-hits}
TEST_NAME=${TEST_NAME:-test-splice-hits}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
mkdir -p "$OUT"
cleanup() { ebpf_stop "$MOUNT" 2>/dev/null || true; }
trap cleanup EXIT
need_cmds docker curl python3
need_built

docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
printf 'splice-hits file A\n' | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/a.txt" >/dev/null
printf 'splice-hits file B\n' | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/b.txt" >/dev/null

# HIT_ACCUM=1 explicitly: this test is about the hit_accum path.
HIT_ACCUM=1 ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
	--cache-capacity=100000000 --policy-bpf=builtin:policy_sieve_pick_ops \
	--metrics-addr=:9600 --sample-every=1s --sync-every=0
sleep 4

replayed() { curl -s "$METRICS_URL" | awk '$1=="s3fs_ebpf_policy_hits_replayed_total" { print $2+0 }'; }
sendfile_once() {	# one fresh open, sent with sendfile(2) to /dev/null
	sudo python3 - "$MOUNT/a.txt" <<'PY'
import os, sys
fd = os.open(sys.argv[1], os.O_RDONLY)
out = os.open("/dev/null", os.O_WRONLY)
os.sendfile(out, fd, 0, os.fstat(fd).st_size)
os.close(fd); os.close(out)
PY
}

sudo cat "$MOUNT/a.txt" >/dev/null           # miss: A enters the cache
sleep 3
base=$(replayed)
for _ in $(seq "$N"); do sendfile_once; done
sudo cat "$MOUNT/b.txt" >/dev/null           # miss: a round replays what was counted
sleep 4
after=$(replayed)
delta=$((after - base))
echo "[splice-hits] replayed $base -> $after (+$delta) for $N sendfile opens"

status=PASS
# B's own first access is counted too, hence >= N rather than == N.
if [ "$delta" -lt "$N" ]; then status=FAIL; fi
echo "RESULT name=$TEST_NAME status=$status sendfile_opens=$N replayed_delta=$delta"
[ "$status" = PASS ]
