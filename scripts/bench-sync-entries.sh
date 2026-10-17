#!/usr/bin/env bash
# scripts/bench-sync-entries.sh — AC-55: directory scale.  N small objects in
# one bucket; time the startup sync, `ls -l`, a stat of every file after the
# dentries are dropped (every stat is a lookup), and `cat`-free random stats.
#
#   N=10000 scripts/bench-sync-entries.sh
#   N=100000 OUT=/tmp/x scripts/bench-sync-entries.sh
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
N=${N:-10000}
S3_BUCKET=${SYNC_BUCKET:-s3fs-bench-sync-$N}
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
OUT=${OUT:-/tmp/bench-sync-entries}
MP=/mnt/s3demo
mkdir -p "$OUT"
need_cmds docker
need_built
minio_require
mc() { docker exec "$MINIO_CONTAINER" mc "$@"; }
now() { echo "$EPOCHREALTIME"; }
ms_since() { awk -v a="$1" -v b="$EPOCHREALTIME" 'BEGIN{printf "%.0f", (b-a)*1000}'; }
trap 'ebpf_stop "$MP" >/dev/null 2>&1 || true' EXIT

have=$(mc ls "local/$S3_BUCKET" 2>/dev/null | wc -l || true)
if [ "$have" != "$N" ]; then
	echo "[sync-bench] seeding $N objects" >&2
	mc rb --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
	mc mb "local/$S3_BUCKET" >/dev/null
	docker exec "$MINIO_CONTAINER" sh -c "rm -rf /tmp/syncseed && mkdir /tmp/syncseed && cd /tmp/syncseed && i=0 && while [ \$i -lt $N ]; do head -c 4096 /dev/zero > f\$(printf %06d \$i).dat; i=\$((i+1)); done" 
	mc cp --recursive /tmp/syncseed/ "local/$S3_BUCKET/" >/dev/null
	docker exec "$MINIO_CONTAINER" rm -rf /tmp/syncseed
fi

ebpf_mount "$MP"
t=$(now)
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only --sample-every=60s --sync-every=0 --softcap=20000
until [ "$(ls "$MP" 2>/dev/null | wc -l)" -ge "$N" ]; do sleep 0.2; done
echo "ready_ms=$(ms_since "$t")  (daemon start to all $N names listed)"

drop() { sync; sudo sysctl -q -w vm.drop_caches="$1"; }
drop 3
t=$(now); ls -l "$MP" | wc -l >/dev/null; echo "ls_l_cold_ms=$(ms_since "$t")"
t=$(now); ls -l "$MP" | wc -l >/dev/null; echo "ls_l_warm_ms=$(ms_since "$t")"
drop 2
t=$(now); find "$MP" -type f -printf '%s\n' | wc -l >/dev/null; echo "find_all_after_drop_ms=$(ms_since "$t")"
drop 2
ls "$MP" | shuf -n 2000 --random-source=<(yes) > "$OUT/names.txt"
t=$(now); (cd "$MP" && xargs -a "$OUT/names.txt" stat -c %s >/dev/null); echo "stat_2000_random_after_drop_ms=$(ms_since "$t")"
