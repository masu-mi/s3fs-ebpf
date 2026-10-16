#!/usr/bin/env bash
# scripts/bench-write.sh — Week 4 (e) 追加: write throughput 比較。
#
# `dd conv=notrunc` で overwrite in place を 1 MiB / 10 MiB / 100 MiB
# 各サイズで実施し、wall clock を hyperfine で計測。
#
# durability の違いに注意:
#   - s3fs-fuse (raw/cache) は close() 内で S3 PUT を同期実行するので、
#     dd の wall time = write + PUT 完了 (close-to-durable)。
#   - ebpf の close() (.release) は flush を enqueue して即 return する
#     fire-and-forget。PUT は daemon が非同期に行うため、`ebpf` 行は
#     「PUT 完了」を含まない。
#   - `ebpf-fsync` は conv=notrunc,fsync で .fsync (daemon の PUT 完了
#     まで block) を通す。fuse と同じ「durable になるまで」の比較はこちら。
#
# fuse-raw は毎 write に FUSE upcall → close で daemon が S3 PUT。
# fuse-cache は kernel_cache 経由なので同じシナリオでも挙動が変わる。

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
OUT_DIR=${OUT_DIR:-/tmp/bench-write}
MP_EBPF=/mnt/s3demo
MP_FUSE_RAW=/mnt/s3fuse
MP_FUSE_CACHE=/mnt/s3fuse-cache
FUSE_CACHE_DIR=/tmp/s3fs-fuse-cache
EBPF_CACHE_DIR=/tmp/s3fs-ebpf-cache
WARMUP=${WARMUP:-1}
RUNS=${RUNS:-5}
DAEMON_EXTRA_FLAGS=${DAEMON_EXTRA_FLAGS:-}  # e.g. --arena-metadata-only for AB-09

mkdir -p "$OUT_DIR" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR"
sudo chown "$(id -un):$(id -gn)" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR" 2>/dev/null || true

log() { echo "[bench-write] $*" >&2; }

check_prereq() {
	need_cmds hyperfine s3fs dd
	need_built
	minio_require
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
	# shellcheck disable=SC2086
	ebpf_start_daemon "$OUT_DIR/fastpath.log" \
		--cache-dir="$EBPF_CACHE_DIR" --sample-every=60s --sync-every=60s \
		$DAEMON_EXTRA_FLAGS
}

stop_all() {
	log "stopping"
	ebpf_stop "$MP_EBPF"
	umount_hard "$MP_FUSE_CACHE"
	umount_hard "$MP_FUSE_RAW"
}
trap stop_all EXIT

# ------------------------------------------------------------------
# Per-size run. Each iteration writes `size` bytes via dd conv=notrunc
# to an existing target file (avoids create/truncate overhead on the
# fuse side). Randomized payload comes from /tmp/w-<size>.bin so
# hyperfine doesn't hit /dev/urandom repeatedly.
# ------------------------------------------------------------------
run_size() {
	local label=$1 sizebytes=$2 sizehuman=$3
	# Each backend gets a distinct S3 key so overwrite races don't hide
	# durability failures. ebpf (fire-and-forget) and ebpf-fsync (blocking)
	# are also separated so the ebpf verify is meaningful on its own.
	local target_fuse_raw="$MP_FUSE_RAW/w-${label}-fuse-raw.bin"
	local target_fuse_cache="$MP_FUSE_CACHE/w-${label}-fuse-cache.bin"
	local target_ebpf="$MP_EBPF/w-${label}-ebpf.bin"
	local target_ebpf_fsync="$MP_EBPF/w-${label}-fsync.bin"
	local payload="/tmp/w-${label}.bin"

	log "prep $sizehuman payload"
	dd if=/dev/urandom of="$payload" bs="$sizebytes" count=1 2>/dev/null

	log "seed targets ($sizehuman)"
	# Use dd conv=notrunc so write-back overwrites the whole file in
	# place — matches the common "log rotation" style workload.
	sudo dd if="$payload" of="$target_fuse_raw"   bs="$sizebytes" count=1 conv=notrunc 2>/dev/null
	sudo dd if="$payload" of="$target_fuse_cache" bs="$sizebytes" count=1 conv=notrunc 2>/dev/null
	sudo dd if="$payload" of="$target_ebpf"       bs="$sizebytes" count=1 conv=notrunc 2>/dev/null
	sudo dd if="$payload" of="$target_ebpf_fsync" bs="$sizebytes" count=1 conv=notrunc 2>/dev/null

	log "hyperfine dd overwrite ($sizehuman, $RUNS runs)"
	hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none \
		--export-markdown "$OUT_DIR/w-${label}.md" \
		--command-name "fuse-raw"   "sudo dd if=$payload of=$target_fuse_raw   bs=$sizebytes count=1 conv=notrunc" \
		--command-name "fuse-cache" "sudo dd if=$payload of=$target_fuse_cache bs=$sizebytes count=1 conv=notrunc" \
		--command-name "ebpf"       "sudo dd if=$payload of=$target_ebpf       bs=$sizebytes count=1 conv=notrunc" \
		--command-name "ebpf-fsync" "sudo dd if=$payload of=$target_ebpf_fsync bs=$sizebytes count=1 conv=notrunc,fsync" \
		>&2 || true

	log "verify byte-for-byte S3 roundtrip per backend"
	# fuse backends are synchronous (close-to-durable); check immediately.
	for backend_key in "fuse-raw:w-${label}-fuse-raw.bin" \
	                   "fuse-cache:w-${label}-fuse-cache.bin" \
	                   "ebpf-fsync:w-${label}-fsync.bin"; do
		local bname="${backend_key%%:*}" bkey="${backend_key##*:}"
		local from_s3="/tmp/w-${label}-${bname}.from-s3"
		mc_cat "$bkey" > "$from_s3" 2>/dev/null || true
		if diff -q "$payload" "$from_s3" >/dev/null 2>&1; then
			log "s3 MATCH  $bname ($bkey)"
		else
			log "s3 MISMATCH $bname ($bkey)"
		fi
	done
	# ebpf (fire-and-forget): PUT is async, allow coalescer+network time.
	log "waiting 5s for ebpf fire-and-forget PUT to land..."
	sleep 5
	local from_s3_ebpf="/tmp/w-${label}-ebpf.from-s3"
	mc_cat "w-${label}-ebpf.bin" > "$from_s3_ebpf" 2>/dev/null || true
	if diff -q "$payload" "$from_s3_ebpf" >/dev/null 2>&1; then
		log "s3 MATCH  ebpf (w-${label}-ebpf.bin) — PUT completed within 5s"
	else
		log "s3 MISMATCH ebpf (w-${label}-ebpf.bin) — fire-and-forget PUT not yet durable"
	fi
}

# ------------------------------------------------------------------
check_prereq
start_fuse_raw
start_fuse_cache
start_ebpf

run_size 1m   1048576   "1 MiB"
run_size 10m  10485760  "10 MiB"
run_size 100m 104857600 "100 MiB"

log "done. results in $OUT_DIR/"
