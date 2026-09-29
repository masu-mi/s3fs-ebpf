#!/usr/bin/env bash
# scripts/bench-3way.sh — 3 バージョン間の fair comparison。
#
#   s3fs-fuse (raw):    毎回 S3 access、キャッシュ無効。max FUSE overhead。
#   s3fs-fuse (cache):  -o use_cache=<dir> -o kernel_cache -o max_stat_cache_size で
#                       ローカルディスク staging + kernel page cache 有効。
#   s3fs-ebpf (A+C):    daemon --cache-dir + kmod .read_folio (VFS page cache 経由)。
#
# 3 者を hyperfine warm/cold + microbench + perf stat + big-file cat で並べる。
# 結果は $OUT_DIR/3way-*.md に出力。docs/benchmark-results.md §7 で解釈。

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/umount.sh
source "$TOPDIR/scripts/lib/umount.sh"
OUT_DIR=${OUT_DIR:-/tmp/bench-3way}
MP_EBPF=/mnt/s3demo
MP_FUSE_RAW=/mnt/s3fuse            # raw、キャッシュ無効
MP_FUSE_CACHE=/mnt/s3fuse-cache    # cache 有効
FUSE_CACHE_DIR=/tmp/s3fs-fuse-cache
EBPF_CACHE_DIR=/tmp/s3fs-ebpf-cache
FILE=${FILE:-hello.txt}
BIG_FILE=${BIG_FILE:-big.bin}
WARMUP=${WARMUP:-5}
RUNS=${RUNS:-200}
MICRO_ITERS=${MICRO_ITERS:-20000}
PERF_ITERS=${PERF_ITERS:-200}

mkdir -p "$OUT_DIR" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR"
sudo chown "$USER:$USER" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR" 2>/dev/null || true

log() { echo "[bench-3way] $*" >&2; }

PERF_BIN=$(ls /usr/lib/linux-tools/*/perf 2>/dev/null \
	| awk -F/ '{ print $NF" "$0 }' | sort -Vr | awk 'NR==1 {print $2}')
PERF_BIN=${PERF_BIN:-perf}

# --- setup ---------------------------------------------------------
check_prereq() {
	for t in hyperfine s3fs; do
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
	log "load kmod + mount ebpf + start daemon with --cache-dir"
	lsmod | grep -q '^s3fs_kfunc' || sudo insmod "$TOPDIR/kmod/s3fs_kfunc.ko"
	mountpoint -q "$MP_EBPF" || { sudo mkdir -p "$MP_EBPF"; sudo mount -t s3fs_ebpf none "$MP_EBPF"; }
	sudo pkill -9 -f 's3fs-ebpf fastpath' 2>/dev/null || true
	sleep 0.5
	LD_LIBRARY_PATH=$HOME/.local/lib sudo -E \
		"$TOPDIR/bin/s3fs-ebpf" fastpath --s3 \
		--cache-dir="$EBPF_CACHE_DIR" --sample-every=60s \
		>"$OUT_DIR/fastpath.log" 2>&1 &
	for _ in $(seq 1 40); do
		grep -q '\[admit\] ' "$OUT_DIR/fastpath.log" && break; sleep 0.2
	done
	sleep 0.2
}

stop_all() {
	log "stopping"
	sudo pkill -INT -f 's3fs-ebpf fastpath' 2>/dev/null || true
	sleep 1
	umount_hard "$MP_EBPF"
	sudo rmmod s3fs_kfunc 2>/dev/null || true
	umount_hard "$MP_FUSE_CACHE"
	# leave MP_FUSE_RAW mounted for the next bench-latency run
}
trap stop_all EXIT

# --- 1. warm cat --------------------------------------------------
run_hf_warm() {
	log "hyperfine warm cat ($RUNS runs)"
	# Prime kernel_cache on fuse-cache so 2nd+ reads hit it.
	cat "$MP_FUSE_CACHE/$FILE" >/dev/null 2>&1 || true
	hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none \
		--export-markdown "$OUT_DIR/3way-cat.md" \
		--command-name 'fuse-raw'    "cat $MP_FUSE_RAW/$FILE" \
		--command-name 'fuse-cache'  "cat $MP_FUSE_CACHE/$FILE" \
		--command-name 'ebpf-A+C'    "cat $MP_EBPF/$FILE" >&2
}

# --- 2. cat 1 MiB --------------------------------------------------
run_hf_big() {
	log "hyperfine cat 1 MiB"
	cat "$MP_FUSE_CACHE/$BIG_FILE" >/dev/null 2>&1 || true
	hyperfine --warmup 2 --min-runs 20 --shell=none \
		--export-markdown "$OUT_DIR/3way-big.md" \
		--command-name 'fuse-raw'    "cat $MP_FUSE_RAW/$BIG_FILE" \
		--command-name 'fuse-cache'  "cat $MP_FUSE_CACHE/$BIG_FILE" \
		--command-name 'ebpf-A+C'    "cat $MP_EBPF/$BIG_FILE" >&2
}

# --- 3. microbench read (syscall latency) -------------------------
run_micro() {
	log "microbench read syscall-level ($MICRO_ITERS iters)"
	BIN="$TOPDIR/bin/s3fs-ebpf"
	# fuse-cache prime
	cat "$MP_FUSE_CACHE/$FILE" >/dev/null 2>&1 || true
	{
		echo '## microbench read (syscall-level)'
		echo '```'
		"$BIN" microbench --op=read --path="$MP_FUSE_RAW/$FILE"   --iters="$MICRO_ITERS" --label='fuse-raw   '
		"$BIN" microbench --op=read --path="$MP_FUSE_CACHE/$FILE" --iters="$MICRO_ITERS" --label='fuse-cache '
		"$BIN" microbench --op=read --path="$MP_EBPF/$FILE"       --iters="$MICRO_ITERS" --label='ebpf-A+C   '
		echo '```'
	} > "$OUT_DIR/3way-micro.md"
	cat "$OUT_DIR/3way-micro.md" >&2
}

# --- 4. perf stat CPU consumption ---------------------------------
run_perf() {
	log "perf stat CPU consumption ($PERF_ITERS cats each)"
	cat "$MP_FUSE_CACHE/$FILE" >/dev/null 2>&1 || true
	perf_cat() {
		local mp=$1 label=$2
		sudo "$PERF_BIN" stat -e task-clock,cycles,instructions,cache-references,cache-misses \
			--no-inherit -x, -- \
			bash -c "for i in \$(seq 1 $PERF_ITERS); do cat $mp/$FILE >/dev/null; done" \
			2>&1 | awk -F',' -v L="$label" -v N="$PERF_ITERS" '
				$3=="task-clock"       { printf "%-14s | task-clock=%s ms per-op=%.2f μs\n", L, $1, ($1*1000)/N }
				$3=="cycles"           { printf "%-14s | cycles/op=%.0f freq=%s %s\n",       L, $1/N, $6, $7 }
				$3=="instructions"     { printf "%-14s | inst/op=%.0f IPC=%s\n",             L, $1/N, $6 }
				$3=="cache-references" { printf "%-14s | cache-refs/op=%.1f\n",              L, $1/N }
				$3=="cache-misses"     { printf "%-14s | cache-miss/op=%.2f miss%%=%s\n",    L, $1/N, $6 }
			'
	}
	{
		echo '## perf stat 3-way (per cat)'
		echo '```'
		echo "iterations per label: $PERF_ITERS cats"
		perf_cat "$MP_FUSE_RAW"   'fuse-raw'
		perf_cat "$MP_FUSE_CACHE" 'fuse-cache'
		perf_cat "$MP_EBPF"       'ebpf-A+C'
		echo '```'
	} > "$OUT_DIR/3way-perf.md"
	cat "$OUT_DIR/3way-perf.md" >&2
}

# --- execute -------------------------------------------------------
check_prereq
start_fuse_raw
start_fuse_cache
start_ebpf

run_hf_warm
run_hf_big
run_micro
run_perf

log "done. results in $OUT_DIR/"
