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
# shellcheck source=lib/umount.sh
source "$TOPDIR/scripts/lib/umount.sh"
OUT_DIR=${OUT_DIR:-/tmp/bench-write}
MP_EBPF=/mnt/s3demo
MP_FUSE_RAW=/mnt/s3fuse
MP_FUSE_CACHE=/mnt/s3fuse-cache
FUSE_CACHE_DIR=/tmp/s3fs-fuse-cache
EBPF_CACHE_DIR=/tmp/s3fs-ebpf-cache
WARMUP=${WARMUP:-1}
RUNS=${RUNS:-5}

mkdir -p "$OUT_DIR" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR"
sudo chown "$USER:$USER" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR" 2>/dev/null || true

log() { echo "[bench-write] $*" >&2; }

check_prereq() {
	for t in hyperfine s3fs dd; do
		command -v "$t" >/dev/null || { echo "missing $t"; exit 1; }
	done
}

start_fuse_raw() {
	log "mount fuse-raw at $MP_FUSE_RAW"
	sudo mkdir -p "$MP_FUSE_RAW"
	umount_hard "$MP_FUSE_RAW"
	sudo s3fs s3fs-test "$MP_FUSE_RAW" \
		-o url=http://127.0.0.1:9000 -o use_path_request_style \
		-o passwd_file=/etc/passwd-s3fs -o allow_other \
		-o uid=$(id -u) -o gid=$(id -g) -o umask=0022
}

start_fuse_cache() {
	log "mount fuse-cache at $MP_FUSE_CACHE (use_cache + kernel_cache)"
	sudo mkdir -p "$MP_FUSE_CACHE"
	umount_hard "$MP_FUSE_CACHE"
	rm -rf "$FUSE_CACHE_DIR" && mkdir -p "$FUSE_CACHE_DIR"
	sudo s3fs s3fs-test "$MP_FUSE_CACHE" \
		-o url=http://127.0.0.1:9000 -o use_path_request_style \
		-o passwd_file=/etc/passwd-s3fs -o allow_other \
		-o uid=$(id -u) -o gid=$(id -g) -o umask=0022 \
		-o use_cache="$FUSE_CACHE_DIR" -o kernel_cache \
		-o max_stat_cache_size=100000
}

start_ebpf() {
	log "load kmod + mount ebpf + start daemon"
	lsmod | grep -q '^s3fs_kfunc' || sudo insmod "$TOPDIR/kmod/s3fs_kfunc.ko"
	mountpoint -q "$MP_EBPF" || { sudo mkdir -p "$MP_EBPF"; sudo mount -t s3fs_ebpf none "$MP_EBPF"; }
	sudo pkill -9 -f 's3fs-ebpf fast[p]ath' 2>/dev/null || true
	sleep 0.5
	LD_LIBRARY_PATH=$HOME/.local/lib sudo -E \
		"$TOPDIR/bin/s3fs-ebpf" fastpath --s3 \
		--cache-dir="$EBPF_CACHE_DIR" --sample-every=60s --sync-every=60s \
		>"$OUT_DIR/fastpath.log" 2>&1 &
	for _ in $(seq 1 60); do
		grep -q '\[admit\] ' "$OUT_DIR/fastpath.log" && break; sleep 0.3
	done
	sleep 0.3
}

stop_all() {
	log "stopping"
	sudo pkill -INT -f 's3fs-ebpf fast[p]ath' 2>/dev/null || true
	sleep 1
	umount_hard "$MP_EBPF"
	sudo rmmod s3fs_kfunc 2>/dev/null || true
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
	docker exec s3fs-ebpf-minio mc cat "local/s3fs-test/w-${label}.bin" > "$from_s3" 2>/dev/null || true
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
