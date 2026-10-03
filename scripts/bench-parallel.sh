#!/usr/bin/env bash
# scripts/bench-parallel.sh — J-05: run fuse-raw + fuse-cache + ebpf
# workloads *concurrently* so Grafana overlays their behaviour on the
# same timeline instead of showing three back-to-back sequential runs.
#
# What "1 graph で比較" means here in practice:
#   - MinIO ingress / egress (existing dashboard panel) sums PUT/GET
#     from all three mounts, so the total curve reflects combined load.
#   - The ebpf-side counters (fast-path hits, upcalls, admits, folio
#     hits, flush events) rise during its share of the workload; the
#     shape tells you what the ebpf stack does *while* fuse is also
#     under load, which serial bench-3way can't show.
#   - Go runtime panels (J-02) confirm the daemon holds its
#     goroutine/heap footprint even when read + write clients race.
#
# This is a smoke, not a peer-reviewed benchmark: numbers here mix
# S3-side contention with per-backend behaviour, so use bench-3way for
# apples-to-apples latency. Use this when you want to *watch* the
# system under mixed load.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

OUT_DIR=${OUT_DIR:-/tmp/bench-parallel}
MP_EBPF=/mnt/s3demo
MP_FUSE_RAW=/mnt/s3fuse
MP_FUSE_CACHE=/mnt/s3fuse-cache
FUSE_CACHE_DIR=/tmp/s3fs-fuse-cache
EBPF_CACHE_DIR=/tmp/s3fs-ebpf-cache
FILE=${FILE:-hello.txt}
BIG_FILE=${BIG_FILE:-big.bin}
DURATION=${DURATION:-30}      # seconds of concurrent workload
READERS_PER_MP=${READERS_PER_MP:-4}
WRITERS_PER_MP=${WRITERS_PER_MP:-1}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
DASHBOARD_URL=${DASHBOARD_URL:-http://localhost:3000/d/s3fs-ebpf-overview}

mkdir -p "$OUT_DIR" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR"
sudo chown "$(id -un):$(id -gn)" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR" 2>/dev/null || true

log() { echo "[bench-parallel] $*" >&2; }

check_prereq() {
	need_cmds s3fs
	need_built
	minio_require "$FILE"
}

start_fuse_raw() {
	log "mount fuse-raw at $MP_FUSE_RAW"
	fuse_mount_raw "$MP_FUSE_RAW"
}

start_fuse_cache() {
	log "mount fuse-cache at $MP_FUSE_CACHE (use_cache + kernel_cache)"
	fuse_mount_cache "$MP_FUSE_CACHE" "$FUSE_CACHE_DIR"
}

start_ebpf() {
	log "load kmod + mount ebpf + start daemon"
	ebpf_mount "$MP_EBPF"
	ebpf_start_daemon "$OUT_DIR/fastpath.log" \
		--cache-dir="$EBPF_CACHE_DIR" --sample-every=5s --sync-every=60s \
		--metrics-addr=:9600
}

stop_all() {
	log "stopping"
	sudo pkill -9 -f 'bench-parallel-reader' 2>/dev/null || true
	sudo pkill -9 -f 'bench-parallel-writer' 2>/dev/null || true
	ebpf_stop "$MP_EBPF"
	umount_hard "$MP_FUSE_CACHE"
	umount_hard "$MP_FUSE_RAW"
}
trap stop_all EXIT

# ----- worker pool -------------------------------------------------

# Spawn `count` reader loops against `mp` for `duration` seconds.
# Wrap the whole loop in `timeout` so a single blocked cat (fuse can
# stall arbitrarily on read errors) doesn't strand `wait` — we've
# had bench sessions hang for hours because of exactly this. Uses
# sudo since the ebpf mount owns entries as root; fuse mounts accept
# non-root but sudo works uniformly across backends.
spawn_readers() {
	local mp=$1 count=$2 duration=$3
	local i
	for i in $(seq 1 "$count"); do
		timeout --kill-after=5s "${duration}s" sudo -n bash -c "
			set +e
			end=\$((\$(date +%s) + $duration))
			cnt=0
			while [ \$(date +%s) -lt \$end ]; do
				timeout 2s cat $mp/$FILE >/dev/null 2>&1 && cnt=\$((cnt+1))
			done
			echo \"reader $mp #$i: \$cnt ops\"
		" > "$OUT_DIR/reader-${mp##*/}-$i.log" 2>&1 &
	done
}

# Spawn `count` writer loops (overwrite the same key). Cheap for
# small file; touches flush + PUT path on the ebpf side and PUT on
# fuse side. Same timeout + sudo scaffolding as spawn_readers.
spawn_writers() {
	local mp=$1 count=$2 duration=$3
	local i
	for i in $(seq 1 "$count"); do
		timeout --kill-after=5s "${duration}s" sudo -n bash -c "
			set +e
			end=\$((\$(date +%s) + $duration))
			cnt=0
			while [ \$(date +%s) -lt \$end ]; do
				timeout 2s dd if=/dev/zero of=$mp/w-parallel-$i.txt bs=64 count=1 conv=notrunc 2>/dev/null && cnt=\$((cnt+1))
			done
			echo \"writer $mp #$i: \$cnt ops\"
		" > "$OUT_DIR/writer-${mp##*/}-$i.log" 2>&1 &
	done
}

collect_ebpf_metrics_snapshot() {
	curl -sf "$METRICS_URL" > "$OUT_DIR/metrics-$1.txt" 2>/dev/null || true
}

# ----- main --------------------------------------------------------

check_prereq
start_fuse_raw
start_fuse_cache
start_ebpf

log "warm-up: single cat per mount so first-hit costs don't dominate"
cat "$MP_EBPF/$FILE"       >/dev/null 2>&1 || true
cat "$MP_FUSE_CACHE/$FILE" >/dev/null 2>&1 || true
cat "$MP_FUSE_RAW/$FILE"   >/dev/null 2>&1 || true

collect_ebpf_metrics_snapshot t0

log "firing ${READERS_PER_MP}× readers and ${WRITERS_PER_MP}× writers per mount for ${DURATION}s"
log "  → open $DASHBOARD_URL to watch MinIO traffic + Go runtime overlay live"

# Kick everything off as close to simultaneous as possible so
# Grafana panels show synchronized start times. Bash job control does
# fork-then-return per-line; a batched invocation is close enough.
spawn_readers "$MP_EBPF"       "$READERS_PER_MP" "$DURATION"
spawn_readers "$MP_FUSE_CACHE" "$READERS_PER_MP" "$DURATION"
spawn_readers "$MP_FUSE_RAW"   "$READERS_PER_MP" "$DURATION"
spawn_writers "$MP_EBPF"       "$WRITERS_PER_MP" "$DURATION"
spawn_writers "$MP_FUSE_CACHE" "$WRITERS_PER_MP" "$DURATION"
spawn_writers "$MP_FUSE_RAW"   "$WRITERS_PER_MP" "$DURATION"

# Bounded wait: DURATION for workers plus 10s slack for `timeout`
# escalation. If anything is still alive past that we sudo-kill and
# move on so the parent script can never wedge the shell.
end_ts=$(( $(date +%s) + DURATION + 10 ))
while [ "$(date +%s)" -lt "$end_ts" ] && jobs -p >/dev/null 2>&1 && [ -n "$(jobs -p)" ]; do
	sleep 1
done
if [ -n "$(jobs -p)" ]; then
	log "watchdog: workers still alive past deadline — SIGKILL"
	# shellcheck disable=SC2046
	sudo kill -9 $(jobs -p) 2>/dev/null || true
	wait 2>/dev/null || true
fi
collect_ebpf_metrics_snapshot t1

log "workload complete — per-worker ops:"
tail -n +1 "$OUT_DIR"/reader-*.log "$OUT_DIR"/writer-*.log 2>/dev/null | grep -E 'reader|writer' | sort

# Summary delta from the ebpf side. FUSE has no counterpart export
# path; the MinIO panel on the dashboard covers cross-backend PUT/GET
# aggregation.
if [ -f "$OUT_DIR/metrics-t0.txt" ] && [ -f "$OUT_DIR/metrics-t1.txt" ]; then
	log "ebpf delta over the ${DURATION}s window:"
	{
		echo '## ebpf metric delta'
		echo
		echo '```'
		for m in s3fs_ebpf_fast_path_hits_total \
			 s3fs_ebpf_read_miss_upcalls_total \
			 s3fs_ebpf_flush_events_total \
			 s3fs_ebpf_flush_bytes_total \
			 s3fs_ebpf_folio_hits_total; do
			a=$(awk -v k="$m" '$1==k {print $2; exit}' "$OUT_DIR/metrics-t0.txt")
			b=$(awk -v k="$m" '$1==k {print $2; exit}' "$OUT_DIR/metrics-t1.txt")
			printf '%-40s  %s → %s  (Δ %s)\n' "$m" "${a:-0}" "${b:-0}" \
				"$(awk -v x="${a:-0}" -v y="${b:-0}" 'BEGIN {print y - x}')"
		done
		echo '```'
	} > "$OUT_DIR/delta.md"
	cat "$OUT_DIR/delta.md" >&2
fi

log "done. logs in $OUT_DIR/"
