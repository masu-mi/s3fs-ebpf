#!/usr/bin/env bash
# scripts/bench-3way.sh — 3 バージョン間の fair comparison。
#
#   s3fs-fuse (raw):    毎回 S3 access、キャッシュ無効。max FUSE overhead。
#   s3fs-fuse (cache):  -o use_cache=<dir> -o kernel_cache -o max_stat_cache_size で
#                       ローカルディスク staging + kernel page cache 有効。
#   s3fs-ebpf (A+C):    daemon --cache-dir + kmod .read_folio (VFS page cache 経由)。
#   s3fs-ebpf (nocache): fuse-raw の対照群。kmod nocache=1 で open ごとに
#                       page cache + arena admit を捨て、daemon は --cache-dir
#                       無しで再起動 → open+read のたびに upcall + S3 GET。
#                       FUSE (kernel_cache 無し) と同じ「毎 open で userspace
#                       経由で取り直す」形で、差は transport と metadata
#                       (getattr/open upcall の有無) だけになる。
#
# 3 者を hyperfine warm/cold + microbench + perf stat + big-file cat で並べ、
# 最後に同じ計測を ebpf-nocache で取る (NOCACHE=0 で skip)。
# 結果は $OUT_DIR/3way-*.md に出力。docs/journal/benchmark-results.md §7 で解釈。

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
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
NOCACHE=${NOCACHE:-1}

mkdir -p "$OUT_DIR" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR"
sudo chown "$(id -un):$(id -gn)" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR" 2>/dev/null || true

log() { echo "[bench-3way] $*" >&2; }

PERF_BIN=$(perf_bin)

# --- setup ---------------------------------------------------------
check_prereq() {
	need_cmds hyperfine s3fs
	need_built
	minio_require "$FILE" "$BIG_FILE"
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
	log "load kmod + mount ebpf + start daemon with --cache-dir"
	ebpf_mount "$MP_EBPF"
	ebpf_start_daemon "$OUT_DIR/fastpath.log" \
		--cache-dir="$EBPF_CACHE_DIR" --sample-every=60s
}

stop_all() {
	log "stopping"
	ebpf_stop "$MP_EBPF"
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
	BIN="$S3FS_BIN"
	# fuse-cache prime
	cat "$MP_FUSE_CACHE/$FILE" >/dev/null 2>&1 || true
	{
		echo '## microbench read (syscall-level)'
		echo '```'
		"$BIN" dev microbench --op=read --path="$MP_FUSE_RAW/$FILE"   --iters="$MICRO_ITERS" --label='fuse-raw   '
		"$BIN" dev microbench --op=read --path="$MP_FUSE_CACHE/$FILE" --iters="$MICRO_ITERS" --label='fuse-cache '
		"$BIN" dev microbench --op=read --path="$MP_EBPF/$FILE"       --iters="$MICRO_ITERS" --label='ebpf-A+C   '
		echo '```'
	} > "$OUT_DIR/3way-micro.md"
	cat "$OUT_DIR/3way-micro.md" >&2
}

# --- 4. perf stat CPU consumption ---------------------------------
# perf_cat <mp> <label> — perf stat over $PERF_ITERS cats of $FILE.
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

run_perf() {
	log "perf stat CPU consumption ($PERF_ITERS cats each)"
	cat "$MP_FUSE_CACHE/$FILE" >/dev/null 2>&1 || true
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

# --- 5. ebpf-nocache (fuse-raw counterpart) -----------------------
# Same mount, daemon restarted without --cache-dir, kmod nocache=1.
# Runs after the A+C phases because the kmod/mount is shared.
run_nocache() {
	log "ebpf-nocache: restart daemon without --cache-dir, kmod nocache=1"
	ebpf_start_daemon "$OUT_DIR/fastpath-nocache.log" --sample-every=60s
	ebpf_nocache 1
	{
		echo '## ebpf-nocache (kmod nocache=1, daemon without --cache-dir)'
		echo
		echo '### hyperfine cat (vs fuse-raw)'
	} > "$OUT_DIR/3way-nocache.md"
	hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none \
		--export-markdown "$OUT_DIR/3way-nocache-cat.md" \
		--command-name 'fuse-raw'      "cat $MP_FUSE_RAW/$FILE" \
		--command-name 'ebpf-nocache'  "cat $MP_EBPF/$FILE" >&2
	hyperfine --warmup 2 --min-runs 20 --shell=none \
		--export-markdown "$OUT_DIR/3way-nocache-big.md" \
		--command-name 'fuse-raw'      "cat $MP_FUSE_RAW/$BIG_FILE" \
		--command-name 'ebpf-nocache'  "cat $MP_EBPF/$BIG_FILE" >&2
	cat "$OUT_DIR/3way-nocache-cat.md" "$OUT_DIR/3way-nocache-big.md" >> "$OUT_DIR/3way-nocache.md"
	{
		echo
		echo '### microbench read (open+read+close)'
		echo '```'
		"$S3FS_BIN" dev microbench --op=read --path="$MP_EBPF/$FILE" --iters="$MICRO_ITERS" --label='ebpf-nocache'
		echo '```'
		echo
		echo '### perf stat (per cat)'
		echo '```'
		perf_cat "$MP_EBPF" 'ebpf-nocache'
		echo '```'
		echo
		echo "read_miss_upcalls_total after run: $(curl -s localhost:9600/metrics | awk '$1=="s3fs_ebpf_read_miss_upcalls_total"{print $2}')"
	} >> "$OUT_DIR/3way-nocache.md"
	ebpf_nocache 0
	cat "$OUT_DIR/3way-nocache.md" >&2
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
if [ "$NOCACHE" = 1 ]; then
	run_nocache
fi

log "done. results in $OUT_DIR/"
