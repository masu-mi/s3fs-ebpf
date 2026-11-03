#!/usr/bin/env bash
# bench-layer-latency.sh — latency at each point where a read "turns around" (issue #35).
#
# A read of the same object is answered at a different layer depending on the
# configuration. Each row below is one such layer, measured as open + read whole
# file + close (`dev microbench --op=read`), p50/p95/p99 over TARGET_SEC seconds:
#
#   ebpf-pcache            page cache hit (the kernel answers; no s3fs-ebpf code but .open)
#   ebpf-pcachedrop        kmod chunk table hit (page cache dropped on open; .read_folio copies
#                          from the chunk table, no upcall)
#   ebpf-nocache-diskcache upcall to the daemon, answered from its disk cache (--cache-dir)
#   ebpf-nocache           upcall to the daemon, which GETs from S3 (MinIO)
#   fuse-cache / fuse-diskcache / fuse-raw   s3fs-fuse: kernel_cache, disk cache only, S3 every time
#
# Per row it also records the deltas of: daemon read-miss handler histogram (the
# daemon's own share of an upcall), upcalls, kmod folio hits, MinIO GetObject count.
# Each run is posted to Grafana as an annotation (GRAFANA_URL, admin:admin) so the
# Prometheus panels show which layer was running.
#
# Usage: scripts/bench-layer-latency.sh      OUT=… TARGET_SEC=15 SIZES="18B 4MiB 64MiB"
set -euo pipefail
TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
S3_BUCKET=${LAYER_BUCKET:-s3fs-layer-latency}
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

OUT=${OUT:-/tmp/bench-layer-latency}
TARGET_SEC=${TARGET_SEC:-15}
LAYERS=${LAYERS:-"ebpf-pcache ebpf-pcachedrop ebpf-nocache-diskcache ebpf-nocache fuse-cache fuse-diskcache fuse-raw"}
SIZES=${SIZES:-"18B 4MiB 64MiB"}
# MinIO's cluster metrics are cached for several seconds: a snapshot taken right after a run
# misses its last GETs, which then show up in the next row's delta. Wait this long before
# each snapshot so the s3_gets column belongs to its row.
MINIO_METRICS_SETTLE=${MINIO_METRICS_SETTLE:-12}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
MINIO_METRICS=${MINIO_METRICS:-http://localhost:9000/minio/v2/metrics/cluster}
GRAFANA_URL=${GRAFANA_URL:-http://localhost:3000}
mkdir -p "$OUT"
need_cmds docker curl python3
need_built
trap 'bench_down' EXIT
log() { echo "[layer-latency] $*" >&2; }
mc() { docker exec "$MINIO_CONTAINER" mc "$@"; }
LL="python3 -I $TOPDIR/scripts/lib/layer-latency.py"

mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
printf 'Hello, s3fs-ebpf!\n' | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/obj-18B" >/dev/null
for sz in 4 64; do
	head -c $((sz * 1024 * 1024)) /dev/urandom | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/obj-${sz}MiB" >/dev/null
done

annotate() { # <start-ms> <end-ms> <layer> <size>
	curl -s -u admin:admin -H 'Content-Type: application/json' -X POST "$GRAFANA_URL/api/annotations" \
		-d "{\"time\":$1,\"timeEnd\":$2,\"tags\":[\"layer-latency\",\"$3\",\"$4\"],\"text\":\"$3 $4\"}" >/dev/null 2>&1 || true
}

{ bench_env_report; echo "layers=$LAYERS sizes=$SIZES target_sec=$TARGET_SEC"; echo "ebpf-nocache-diskcache: kmod nocache=1 + daemon --cache-dir"; } > "$OUT/environment.txt"
echo "layer,size,iters,p50_us,p95_us,p99_us,min_us,max_us,ops_per_s,upcalls,folio_hits,s3_gets,hist_count,daemon_p50_us,daemon_p99_us,daemon_mean_us" > "$OUT/layer-latency.csv"

for layer in $LAYERS; do
	bench_up "$layer" "$OUT/daemon-$layer.log" --sample-every=60s --sync-every=0 --softcap=64
	sleep 3
	for size in $SIZES; do
		f="$BENCH_MP/obj-$size"
		sudo cat "$f" >/dev/null; sudo cat "$f" >/dev/null     # warm every cache this layer has
		"$S3FS_BIN" dev microbench --op=read --path="$f" --iters=40 --warmup=5 --label=cal > "$OUT/cal.txt"
		ops=$(awk -F'ops/s=' '/ops\/s=/{print $2+0}' "$OUT/cal.txt")
		iters=$(awk -v o="$ops" -v t="$TARGET_SEC" 'BEGIN{n=int(o*t); if(n<30)n=30; if(n>200000)n=200000; print n}')
		sleep "$MINIO_METRICS_SETTLE"
		$LL snap "$METRICS_URL" "$MINIO_METRICS" > "$OUT/snap-a.json"
		t0=$(date +%s%3N)
		"$S3FS_BIN" dev microbench --op=read --path="$f" --iters="$iters" --warmup=20 --label="$layer/$size" > "$OUT/micro-$layer-$size.txt"
		t1=$(date +%s%3N)
		sleep "$MINIO_METRICS_SETTLE"
		$LL snap "$METRICS_URL" "$MINIO_METRICS" > "$OUT/snap-b.json"
		annotate "$t0" "$t1" "$layer" "$size"
		row=$($LL delta "$OUT/snap-a.json" "$OUT/snap-b.json" "$OUT/micro-$layer-$size.txt" "$layer" "$size" "$iters")
		echo "$row" >> "$OUT/layer-latency.csv"
		log "$row"
	done
	bench_down
done
log "done. $OUT/layer-latency.csv"
