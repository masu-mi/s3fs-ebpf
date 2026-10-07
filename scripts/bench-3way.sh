#!/usr/bin/env bash
# scripts/bench-3way.sh — 最大 5 バージョン間の fair comparison。
#
#   s3fs-fuse (raw):    毎回 S3 access、キャッシュ無効。max FUSE overhead。
#   s3fs-fuse (cache):  -o use_cache=<dir> -o kernel_cache -o max_stat_cache_size で
#                       ローカルディスク staging + kernel page cache 有効。
#   s3fs-ebpf (A+C):    daemon --cache-dir + kmod .read_folio (VFS page cache 経由)。
#   mount-s3:           Mountpoint for Amazon S3 [I-01、mount-s3 未インストール時は skip]
#   s3fs-ebpf (nocache): fuse-raw の対照群。kmod nocache=1 で open ごとに
#                       page cache + arena admit を捨て、daemon は --cache-dir
#                       無しで再起動 → open+read のたびに upcall + S3 GET。
#                       FUSE (kernel_cache 無し) と同じ「毎 open で userspace
#                       経由で取り直す」形で、差は transport と metadata
#                       (getattr/open upcall の有無) だけになる。
#
# セクション:
#   1. hyperfine warm cat + big cat
#   1c. cold cat drop_caches=1 (page cache のみ, inode 保持) [I-02]
#   1d. cold-cache isolation (arena 生存 + page cache 落とし) [I-03]
#   2. microbench read syscall-level
#   3. perf stat CPU
#   4. ebpf-nocache (NOCACHE=0 で skip)
#
# 結果は $OUT_DIR/3way-*.md に出力。docs/journal/benchmark-results.md §7 で解釈。

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
OUT_DIR=${OUT_DIR:-/tmp/bench-3way}
MP_EBPF=/mnt/s3demo
MP_FUSE_RAW=/mnt/s3fuse            # raw、キャッシュ無効
MP_FUSE_CACHE=/mnt/s3fuse-cache    # cache 有効
MP_MOUNT_S3=/mnt/s3-mountpoint     # I-01
FUSE_CACHE_DIR=/tmp/s3fs-fuse-cache
EBPF_CACHE_DIR=/tmp/s3fs-ebpf-cache
MOUNT_S3_CACHE_DIR=${MOUNT_S3_CACHE_DIR:-/tmp/mount-s3-cache}  # I-01
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
	cat "$MP_FUSE_CACHE/$FILE" >/dev/null 2>&1 || true  # prime kernel_cache
}

start_ebpf() {
	log "load kmod + mount ebpf + start daemon with --cache-dir"
	ebpf_mount "$MP_EBPF"
	ebpf_start_daemon "$OUT_DIR/fastpath.log" \
		--cache-dir="$EBPF_CACHE_DIR" --sample-every=60s
}

# I-01: Mountpoint for Amazon S3, conditional.
MOUNT_S3_STARTED=0
start_mount_s3() {
	if ! mount_s3_available; then
		log "mount-s3 not found — skipping I-01 arm (install mountpoint-s3 to enable)"
		return
	fi
	log "mount mount-s3 at $MP_MOUNT_S3"
	mount_s3_mount "$MP_MOUNT_S3" "$MOUNT_S3_CACHE_DIR" || {
		log "mount-s3 mount failed — skipping I-01 arm"
		return
	}
	MOUNT_S3_STARTED=1
	cat "$MP_MOUNT_S3/$FILE" >/dev/null 2>&1 || true
}

stop_all() {
	log "stopping"
	ebpf_stop "$MP_EBPF"
	umount_hard "$MP_FUSE_CACHE"
	if [ "$MOUNT_S3_STARTED" -eq 1 ]; then umount_hard "$MP_MOUNT_S3"; fi
	# leave MP_FUSE_RAW mounted for the next bench-latency run
}
trap stop_all EXIT

# --- 1. warm cat --------------------------------------------------
run_hf_warm() {
	log "hyperfine warm cat ($RUNS runs)"
	local extra=()
	[ "$MOUNT_S3_STARTED" -eq 1 ] && extra+=(--command-name 'mount-s3' "cat $MP_MOUNT_S3/$FILE")
	hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none \
		--export-markdown "$OUT_DIR/3way-cat.md" \
		--command-name 'fuse-raw'    "cat $MP_FUSE_RAW/$FILE" \
		--command-name 'fuse-cache'  "cat $MP_FUSE_CACHE/$FILE" \
		--command-name 'ebpf-A+C'    "cat $MP_EBPF/$FILE" \
		"${extra[@]}" >&2
}

# --- 2. cat 1 MiB --------------------------------------------------
run_hf_big() {
	log "hyperfine cat 1 MiB"
	cat "$MP_FUSE_CACHE/$BIG_FILE" >/dev/null 2>&1 || true
	local extra=()
	[ "$MOUNT_S3_STARTED" -eq 1 ] && extra+=(--command-name 'mount-s3' "cat $MP_MOUNT_S3/$BIG_FILE")
	hyperfine --warmup 2 --min-runs 20 --shell=none \
		--export-markdown "$OUT_DIR/3way-big.md" \
		--command-name 'fuse-raw'    "cat $MP_FUSE_RAW/$BIG_FILE" \
		--command-name 'fuse-cache'  "cat $MP_FUSE_CACHE/$BIG_FILE" \
		--command-name 'ebpf-A+C'    "cat $MP_EBPF/$BIG_FILE" \
		"${extra[@]}" >&2
}

# --- 2b. cold cat drop_caches=1 (page cache only) [I-02] -----------
# drop_caches=1 は page cache のみ破棄。dentry/inode は残るため path
# lookup のノイズが消え、純粋な「page cache の有無」が測れる。
# s3fs-ebpf の arena は BPF arena (kernel memory) なので page cache と
# 独立して生存し、cold 後でも folio 補充できる。
run_hf_cold1() {
	log "hyperfine cold cat drop_caches=1 [I-02]"
	local extra=()
	[ "$MOUNT_S3_STARTED" -eq 1 ] && extra+=(--command-name 'mount-s3' "cat $MP_MOUNT_S3/$FILE")
	hyperfine --warmup 1 --min-runs 30 --shell=none \
		--prepare 'sudo sysctl -q -w vm.drop_caches=1' \
		--export-markdown "$OUT_DIR/3way-cat-cold1.md" \
		--command-name 'fuse-raw'    "cat $MP_FUSE_RAW/$FILE" \
		--command-name 'fuse-cache'  "cat $MP_FUSE_CACHE/$FILE" \
		--command-name 'ebpf-A+C'    "cat $MP_EBPF/$FILE" \
		"${extra[@]}" >&2 || true
}

# --- 2c. cold-cache isolation [I-03] --------------------------------
# daemon + kmod 生存 (admit table 保持)、page cache だけ落とした状態。
# fuse-raw: page cache なし + 毎回 upcall → S3。
# fuse-cache: kernel_cache 破棄 → fuse daemon の disk cache から staging。
# ebpf-A+C: arena 生存 → .read_folio が arena から folio を補充 (upcall 0)。
# upcall delta = 0 なら ebpf が page cache の外で完結していることの証明。
run_cold_isolate() {
	log "cold-cache isolation: arena warm, page cache cold [I-03]"
	local u_before u_after
	u_before=$(curl -s localhost:9600/metrics \
		| awk '$1=="s3fs_ebpf_read_miss_upcalls_total"{print $2}' || echo 0)
	sudo sysctl -q -w vm.drop_caches=1
	local extra=()
	[ "$MOUNT_S3_STARTED" -eq 1 ] && extra+=(--command-name 'mount-s3' "cat $MP_MOUNT_S3/$FILE")
	hyperfine --warmup 1 --min-runs 30 --shell=none \
		--prepare 'sudo sysctl -q -w vm.drop_caches=1' \
		--export-markdown "$OUT_DIR/3way-cold-isolate.md" \
		--command-name 'fuse-raw'    "cat $MP_FUSE_RAW/$FILE" \
		--command-name 'fuse-cache'  "cat $MP_FUSE_CACHE/$FILE" \
		--command-name 'ebpf-A+C'    "cat $MP_EBPF/$FILE" \
		"${extra[@]}" >&2 || true
	u_after=$(curl -s localhost:9600/metrics \
		| awk '$1=="s3fs_ebpf_read_miss_upcalls_total"{print $2}' || echo 0)
	{
		echo "## cold-cache isolation [I-03]"
		echo
		echo "daemon+kmod alive (arena intact); drop_caches=1 between runs."
		echo "upcalls delta = $((u_after - u_before))"
		echo "(0 = arena served all folios without S3 round-trip)"
		echo
		cat "$OUT_DIR/3way-cold-isolate.md"
	} > "$OUT_DIR/3way-cold-isolate-report.md"
	cat "$OUT_DIR/3way-cold-isolate-report.md" >&2
}

# --- 3. microbench read (syscall latency) -------------------------
run_micro() {
	log "microbench read syscall-level ($MICRO_ITERS iters)"
	BIN="$S3FS_BIN"
	{
		echo '## microbench read (syscall-level)'
		echo '```'
		"$BIN" dev microbench --op=read --path="$MP_FUSE_RAW/$FILE"   --iters="$MICRO_ITERS" --label='fuse-raw   '
		"$BIN" dev microbench --op=read --path="$MP_FUSE_CACHE/$FILE" --iters="$MICRO_ITERS" --label='fuse-cache '
		"$BIN" dev microbench --op=read --path="$MP_EBPF/$FILE"       --iters="$MICRO_ITERS" --label='ebpf-A+C   '
		[ "$MOUNT_S3_STARTED" -eq 1 ] && \
			"$BIN" dev microbench --op=read --path="$MP_MOUNT_S3/$FILE" --iters="$MICRO_ITERS" --label='mount-s3   '
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
	{
		echo '## perf stat 3-way (per cat)'
		echo '```'
		echo "iterations per label: $PERF_ITERS cats"
		perf_cat "$MP_FUSE_RAW"   'fuse-raw'
		perf_cat "$MP_FUSE_CACHE" 'fuse-cache'
		perf_cat "$MP_EBPF"       'ebpf-A+C'
		[ "$MOUNT_S3_STARTED" -eq 1 ] && perf_cat "$MP_MOUNT_S3" 'mount-s3'
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
start_mount_s3  # I-01 (no-op if mount-s3 not installed)

run_hf_warm
run_hf_big
run_hf_cold1        # I-02
run_cold_isolate    # I-03
run_micro
run_perf
if [ "$NOCACHE" = 1 ]; then
	run_nocache
fi

log "done. results in $OUT_DIR/"
