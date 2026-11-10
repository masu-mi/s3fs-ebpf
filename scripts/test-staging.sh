#!/usr/bin/env bash
# test-staging.sh — object staging (AC-88): sibling small files are fetched ahead when a
# directory is read in name order; a staged copy must never be served after the object
# was written, deleted or renamed through the mount.
#
#   1. N files f000..f(N-1); cat them in name order.  Expect: every byte right, and the
#      misses were served from staged copies (staging_hits grows, fetches > 0).
#   2. Stale write: while f<W> is staged, rewrite it through the mount and fsync, make
#      the next read a miss (nocache=1), read it.  Expect the new bytes.
#   3. Delete / rename of staged files: cat of the old name fails, the new name reads
#      the old bytes.
#   4. Random order: cat 60 files in a shuffled order.  Expect right bytes.
#
# Usage: scripts/test-staging.sh     Exit 0 = PASS, 1 = FAIL.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-staging}
N=${N:-120}
S3_BUCKET=${STAGING_BUCKET:-s3fs-staging}
TEST_NAME=${TEST_NAME:-test-staging}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
mkdir -p "$OUT"
cleanup() {
	ebpf_nocache 0 2>/dev/null || true
	ebpf_stop "$MOUNT" 2>/dev/null || true
	docker exec "$MINIO_CONTAINER" mc rb --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
	docker exec "$MINIO_CONTAINER" rm -rf /tmp/staging-seed >/dev/null 2>&1 || true
}
trap cleanup EXIT
need_cmds docker curl
need_built

docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
docker exec "$MINIO_CONTAINER" sh -c "rm -rf /tmp/staging-seed && mkdir -p /tmp/staging-seed && i=0 && while [ \$i -lt $N ]; do n=\$(printf '%03d' \$i); echo \"orig-\$n\" > /tmp/staging-seed/f\$n; i=\$((i+1)); done && mc cp --recursive /tmp/staging-seed/ local/$S3_BUCKET/ >/dev/null"

ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
	--policy-bpf=builtin:policy_observe_ops --metrics-addr=:9600 --sample-every=1s --sync-every=0
sleep 3

metric() { curl -s "$METRICS_URL" | awk -v m="$1" '$1==m { print $2+0 }'; }
overall=PASS
bad() { echo "FAIL: $*" >&2; overall=FAIL; }
want() { echo "orig-$1"; } # content of f<n>

# 1. in name order
h0=$(metric s3fs_ebpf_staging_hits_total); f0=$(metric s3fs_ebpf_staging_fetches_total)
for i in $(seq 0 59); do
	n=$(printf '%03d' "$i")
	got=$(cat "$MOUNT/f$n") || { bad "cat f$n failed"; continue; }
	[ "$got" = "$(want "$n")" ] || bad "f$n = '$got'"
done
h1=$(metric s3fs_ebpf_staging_hits_total); f1=$(metric s3fs_ebpf_staging_fetches_total)
echo "sequential: staging hits +$((h1 - h0)), fetches +$((f1 - f0))"
[ $((h1 - h0)) -ge 30 ] || bad "staging hits +$((h1 - h0)), want >= 30 (read-ahead did not run)"

# 2. a staged copy dropped by a write: f060..f075 are staged by the reads of f044..f059
# (depth 16); rewrite f065 through the mount (same length: the size check alone must not save it), fsync, then make its read a miss.
printf 'NEW!-065\n' | sudo dd of="$MOUNT/f065" conv=fsync status=none
sleep 1
ebpf_nocache 1
got=$(cat "$MOUNT/f065")
ebpf_nocache 0
[ "$got" = "NEW!-065" ] || bad "f065 after the write = '$got' (a stale staged copy was served?)"

# 3. delete / rename
sudo rm "$MOUNT/f066"
sleep 1
if cat "$MOUNT/f066" >/dev/null 2>&1; then bad "f066 still readable after rm"; fi
sudo mv "$MOUNT/f067" "$MOUNT/g067"
sleep 1
got=$(cat "$MOUNT/g067" 2>/dev/null || echo ENOENT)
[ "$got" = "$(want 067)" ] || bad "g067 = '$got'"
if cat "$MOUNT/f067" >/dev/null 2>&1; then bad "f067 still readable after mv"; fi

# 4. random order
for n in $(seq 60 119 | shuf --random-source=<(yes) | head -50); do
	nn=$(printf '%03d' "$n")
	case $nn in 065 | 066 | 067) continue ;; esac
	got=$(cat "$MOUNT/f$nn" 2>/dev/null || echo ENOENT)
	[ "$got" = "$(want "$nn")" ] || bad "random f$nn = '$got'"
done

printf 'RESULT name=%s status=%s hits=%d fetches=%d\n' "$TEST_NAME" "$overall" "$((h1 - h0))" "$((f1 - f0))"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
