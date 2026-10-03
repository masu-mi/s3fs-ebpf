#!/usr/bin/env bash
# scripts/bench-write.sh — Week 4 (e) 追加: write throughput 比較。
#
# `dd conv=notrunc` で overwrite in place を 1 MiB / 10 MiB / 100 MiB
# 各サイズで実施し、wall clock を hyperfine で計測。write は close 時
# flush なので dd 単独の time が close-to-durable 相当。
#
# ebpf の write path は Week 4 (c) で入った close/fsync flush。
# fuse-raw は毎 write に FUSE upcall → daemon が S3 PUT。
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
	ebpf_start_daemon "$OUT_DIR/fastpath.log" \
		--cache-dir="$EBPF_CACHE_DIR" --sample-every=60s --sync-every=60s
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
	local target_ebpf="$MP_EBPF/w-${label}.bin"
	local target_fuse_raw="$MP_FUSE_RAW/w-${label}.bin"
	local target_fuse_cache="$MP_FUSE_CACHE/w-${label}.bin"
	local payload="/tmp/w-${label}.bin"

	log "prep $sizehuman payload"
	dd if=/dev/urandom of="$payload" bs="$sizebytes" count=1 2>/dev/null

	log "seed targets ($sizehuman)"
	# Use dd conv=notrunc so write-back overwrites the whole file in
	# place — matches the common "log rotation" style workload.
	sudo dd if="$payload" of="$target_ebpf"       bs="$sizebytes" count=1 conv=notrunc 2>/dev/null
	sudo dd if="$payload" of="$target_fuse_raw"   bs="$sizebytes" count=1 conv=notrunc 2>/dev/null
	sudo dd if="$payload" of="$target_fuse_cache" bs="$sizebytes" count=1 conv=notrunc 2>/dev/null

	log "hyperfine dd overwrite ($sizehuman, $RUNS runs)"
	# --show-output=0 keeps stderr clean; each dd emits a stats line.
	hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none \
		--export-markdown "$OUT_DIR/w-${label}.md" \
		--command-name "fuse-raw"   "sudo dd if=$payload of=$target_fuse_raw   bs=$sizebytes count=1 conv=notrunc" \
		--command-name "fuse-cache" "sudo dd if=$payload of=$target_fuse_cache bs=$sizebytes count=1 conv=notrunc" \
		--command-name "ebpf"       "sudo dd if=$payload of=$target_ebpf       bs=$sizebytes count=1 conv=notrunc" \
		>&2 || true

	log "verify byte-for-byte roundtrip via S3"
	local from_s3="/tmp/w-${label}.from-s3"
	mc_cat "w-${label}.bin" > "$from_s3" 2>/dev/null || true
	if diff -q "$payload" "$from_s3" >/dev/null 2>&1; then
		log "s3 body MATCH for w-${label}.bin"
	else
		log "s3 body MISMATCH for w-${label}.bin"
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
