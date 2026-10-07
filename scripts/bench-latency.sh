#!/usr/bin/env bash
# scripts/bench-latency.sh — latency bench across up to 4 backends:
#   s3fs-ebpf B   — kmod arena path (admit済)
#   s3fs-fuse     — raw, no local cache (baseline)
#   fuse-cache    — use_cache + kernel_cache (best-case FUSE) [I-04]
#   mount-s3      — Mountpoint for Amazon S3 [I-01, conditional on install]
#
# Sections:
#   1.  hyperfine warm stat + warm cat
#   1b. hyperfine cold cat  drop_caches=3 (page cache + dentries/inodes)
#   1c. hyperfine cold cat  drop_caches=1 (page cache only) [I-02]
#   1d. cold-cache isolation: arena warm, page cache cold [I-03]
#   2.  microbench syscall-level stat/read
#   2b. perf stat CPU consumption
#   3.  fio filestat parallel IOPS
#   4.  large file throughput sweep

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
OUT_DIR=${OUT_DIR:-/tmp/bench}
MP_EBPF=/mnt/s3demo
MP_FUSE=/mnt/s3fuse
MP_FUSE_CACHE=/mnt/s3fuse-cache                          # I-04
MP_MOUNT_S3=/mnt/s3-mountpoint                           # I-01
FUSE_CACHE_DIR=${FUSE_CACHE_DIR:-/tmp/s3fs-fuse-cache}  # I-04
MOUNT_S3_CACHE_DIR=${MOUNT_S3_CACHE_DIR:-/tmp/mount-s3-cache}  # I-01
FILE=${FILE:-hello.txt}
BIG_FILE=${BIG_FILE:-big.bin}
WARMUP=${WARMUP:-5}
RUNS=${RUNS:-200}
MICRO_ITERS=${MICRO_ITERS:-5000}
FIO_TIME=${FIO_TIME:-5}
FIO_JOBS=${FIO_JOBS:-4}

mkdir -p "$OUT_DIR"

log() { echo "[bench] $*" >&2; }

check_prereq() {
	need_cmds hyperfine s3fs fio
	need_built
	minio_require "$FILE"
}

# --- lifecycle ---------------------------------------------------------------

FUSE_STARTED=0
start_fuse() {
	if mountpoint -q "$MP_FUSE"; then
		log "$MP_FUSE already mounted, reusing"
		return
	fi
	log "mounting s3fs-fuse at $MP_FUSE"
	fuse_mount "$MP_FUSE" -o compat_dir -o enable_noobj_cache \
		|| { echo "s3fs mount failed — check $S3FS_PASSWD_FILE and MinIO"; exit 1; }
	FUSE_STARTED=1
}

# I-04: fuse with use_cache + kernel_cache — best-case FUSE baseline.
FUSE_CACHE_STARTED=0
start_fuse_cache() {
	log "mounting s3fs-fuse+cache at $MP_FUSE_CACHE"
	fuse_mount_cache "$MP_FUSE_CACHE" "$FUSE_CACHE_DIR" || {
		log "fuse-cache mount failed — skipping I-04 arm"
		return
	}
	FUSE_CACHE_STARTED=1
	cat "$MP_FUSE_CACHE/$FILE" >/dev/null 2>&1 || true  # prime kernel_cache
}

# I-01: Mountpoint for Amazon S3 — conditional on mount-s3 being installed.
MOUNT_S3_STARTED=0
start_mount_s3() {
	if ! mount_s3_available; then
		log "mount-s3 not found — skipping I-01 arm (install mountpoint-s3 to enable)"
		return
	fi
	log "mounting mount-s3 at $MP_MOUNT_S3"
	mount_s3_mount "$MP_MOUNT_S3" "$MOUNT_S3_CACHE_DIR" || {
		log "mount-s3 mount failed — skipping I-01 arm"
		return
	}
	MOUNT_S3_STARTED=1
	cat "$MP_MOUNT_S3/$FILE" >/dev/null 2>&1 || true
}

start_ebpf() {
	log "loading kmod + mounting + starting daemon"
	ebpf_mount "$MP_EBPF"
	ebpf_start_daemon "$OUT_DIR/fastpath.log" --sample-every=60s --metrics-addr=:9600
	log "prewarm done"
}

stop_all() {
	log "stopping"
	ebpf_stop "$MP_EBPF"
	if [ "$FUSE_CACHE_STARTED" -eq 1 ]; then umount_hard "$MP_FUSE_CACHE"; fi
	if [ "$MOUNT_S3_STARTED"   -eq 1 ]; then umount_hard "$MP_MOUNT_S3";   fi
	if [ "$FUSE_STARTED"       -eq 1 ]; then umount_hard "$MP_FUSE";        fi
}
trap stop_all EXIT

check_prereq
start_fuse
start_fuse_cache  # I-04
start_mount_s3    # I-01
start_ebpf

# Build optional extra arms (re-evaluated per section).
_fuse_cache_cat_arm()  { [ "$FUSE_CACHE_STARTED" -eq 1 ] && printf '%s\n' "--command-name" "fuse-cache" "cat $MP_FUSE_CACHE/$1" || true; }
_mount_s3_cat_arm()    { [ "$MOUNT_S3_STARTED"   -eq 1 ] && printf '%s\n' "--command-name" "mount-s3"  "cat $MP_MOUNT_S3/$1"   || true; }
_fuse_cache_stat_arm() { [ "$FUSE_CACHE_STARTED" -eq 1 ] && printf '%s\n' "--command-name" "fuse-cache" "stat --format=%s $MP_FUSE_CACHE/$1" || true; }
_mount_s3_stat_arm()   { [ "$MOUNT_S3_STARTED"   -eq 1 ] && printf '%s\n' "--command-name" "mount-s3"  "stat --format=%s $MP_MOUNT_S3/$1"   || true; }

# mapfile reads null-separated tokens into an array — but printf above uses \n.
# Use readarray with process substitution instead.
extra_cat_arms=()
[ "$FUSE_CACHE_STARTED" -eq 1 ] && extra_cat_arms+=(--command-name 'fuse-cache' "cat $MP_FUSE_CACHE/$FILE")
[ "$MOUNT_S3_STARTED"   -eq 1 ] && extra_cat_arms+=(--command-name 'mount-s3'   "cat $MP_MOUNT_S3/$FILE")

extra_stat_arms=()
[ "$FUSE_CACHE_STARTED" -eq 1 ] && extra_stat_arms+=(--command-name 'fuse-cache' "stat --format=%s $MP_FUSE_CACHE/$FILE")
[ "$MOUNT_S3_STARTED"   -eq 1 ] && extra_stat_arms+=(--command-name 'mount-s3'   "stat --format=%s $MP_MOUNT_S3/$FILE")

# ---------- 1. hyperfine: end-to-end (fork/exec 込み) -------------------------
log "hyperfine: warm stat ($RUNS runs)"
hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none \
	--export-markdown "$OUT_DIR/hf-stat.md" \
	--command-name 's3fs-fuse'   "stat --format=%s $MP_FUSE/$FILE" \
	--command-name 's3fs-ebpf B' "stat --format=%s $MP_EBPF/$FILE" \
	"${extra_stat_arms[@]}" >&2

log "hyperfine: warm cat"
hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none \
	--export-markdown "$OUT_DIR/hf-cat.md" \
	--command-name 's3fs-fuse'   "cat $MP_FUSE/$FILE" \
	--command-name 's3fs-ebpf B' "cat $MP_EBPF/$FILE" \
	"${extra_cat_arms[@]}" >&2

# ---------- 1b. cold cat: drop_caches=3 (page cache + dentries/inodes) --------
# drop_caches=3 は inode も全部飛ぶので path lookup が測定値を支配する。
# fuse-raw との差分は「S3 round-trip - path lookup」であり純粋な読み性能ではない。
# 参考値として残す。
log "hyperfine: cold cat drop_caches=3 (page+inode flush)"
hyperfine --warmup 2 --min-runs 30 --shell=none \
	--prepare 'sudo sysctl -q -w vm.drop_caches=3' \
	--export-markdown "$OUT_DIR/hf-cat-cold3.md" \
	--command-name 's3fs-fuse'   "cat $MP_FUSE/$FILE" \
	--command-name 's3fs-ebpf B' "cat $MP_EBPF/$FILE" \
	"${extra_cat_arms[@]}" >&2 || true

# ---------- 1c. cold cat: drop_caches=1 (page cache only) [I-02] --------------
# drop_caches=1 は page cache のみ。dentry/inode は残るので path lookup の
# ノイズが消え、純粋な「page cache の有無」による差が測れる。
# s3fs-ebpf B は arena (BPF arena = kernel memory) が page cache と独立している
# ため、drop_caches=1 後でも arena から folio を補充できる。
log "hyperfine: cold cat drop_caches=1 (page cache only) [I-02]"
hyperfine --warmup 2 --min-runs 30 --shell=none \
	--prepare 'sudo sysctl -q -w vm.drop_caches=1' \
	--export-markdown "$OUT_DIR/hf-cat-cold1.md" \
	--command-name 's3fs-fuse'   "cat $MP_FUSE/$FILE" \
	--command-name 's3fs-ebpf B' "cat $MP_EBPF/$FILE" \
	"${extra_cat_arms[@]}" >&2 || true

# ---------- 1d. cold-cache isolation: arena warm, page cache cold [I-03] ------
# daemon + kmod 起動中 (admit table 生存)、page cache だけ落とした状態。
# s3fs-ebpf B は arena から folio 補充 (S3 upcall なし)。
# fuse-cache は kernel_cache 消失後 fuse daemon の disk cache から再 staging。
# 差分 = arena の page cache 非依存な高速補充 vs disk-staged FUSE の差。
log "cold-cache isolation: upcalls before/after drop_caches=1 [I-03]"
UPCALLS_BEFORE=$(curl -s localhost:9600/metrics \
	| awk '$1=="s3fs_ebpf_read_miss_upcalls_total"{print $2}' || echo 0)
sudo sysctl -q -w vm.drop_caches=1
hyperfine --warmup 1 --min-runs 30 --shell=none \
	--prepare 'sudo sysctl -q -w vm.drop_caches=1' \
	--export-markdown "$OUT_DIR/hf-cold-isolate.md" \
	--command-name 's3fs-fuse'   "cat $MP_FUSE/$FILE" \
	--command-name 's3fs-ebpf B' "cat $MP_EBPF/$FILE" \
	"${extra_cat_arms[@]}" >&2 || true
UPCALLS_AFTER=$(curl -s localhost:9600/metrics \
	| awk '$1=="s3fs_ebpf_read_miss_upcalls_total"{print $2}' || echo 0)
{
	echo "## cold-cache isolation [I-03]"
	echo
	echo "drop_caches=1 between every run; daemon+kmod alive (admit table intact)."
	echo "upcalls delta = $((UPCALLS_AFTER - UPCALLS_BEFORE))"
	echo "(0 = arena served all folios without S3 round-trip)"
	echo
	cat "$OUT_DIR/hf-cold-isolate.md"
} > "$OUT_DIR/cold-isolate.md"
cat "$OUT_DIR/cold-isolate.md" >&2

# ---------- 2. microbench: syscall-level (fork/exec 排除) ----------------------
log "microbench: syscall-level stat/read ($MICRO_ITERS iters each)"
BIN="$S3FS_BIN"
{
	echo '## microbench (syscall-level, no fork/exec)'
	echo
	for op in stat read; do
		echo "### $op $FILE"
		echo '```'
		"$BIN" dev microbench --op="$op" --path="$MP_FUSE/$FILE"   --iters="$MICRO_ITERS" --label='s3fs-fuse   '
		"$BIN" dev microbench --op="$op" --path="$MP_EBPF/$FILE"   --iters="$MICRO_ITERS" --label='s3fs-ebpf B '
		[ "$FUSE_CACHE_STARTED" -eq 1 ] && \
			"$BIN" dev microbench --op="$op" --path="$MP_FUSE_CACHE/$FILE" --iters="$MICRO_ITERS" --label='fuse-cache  '
		[ "$MOUNT_S3_STARTED"   -eq 1 ] && \
			"$BIN" dev microbench --op="$op" --path="$MP_MOUNT_S3/$FILE"   --iters="$MICRO_ITERS" --label='mount-s3    '
		echo '```'
		echo
	done
} > "$OUT_DIR/micro.md"
cat "$OUT_DIR/micro.md" >&2

# ---------- 2b. perf stat: CPU consumption -----------------------------------
PERF_BIN=$(perf_bin)
log "perf stat: CPU consumption (using $PERF_BIN)"
PERF_ITERS=$(( MICRO_ITERS / 100 ))
perf_cat() {
	local mp=$1 label=$2
	sudo "$PERF_BIN" stat -e task-clock,cycles,instructions,cache-references,cache-misses \
		--no-inherit -x, -- \
		bash -c "for i in \$(seq 1 $PERF_ITERS); do cat $mp/$FILE >/dev/null; done" \
		2>&1 | awk -F',' -v L="$label" -v N="$PERF_ITERS" '
			$3=="task-clock"       { printf "%-14s | task-clock=%s ms  per-op=%.2f μs\n", L, $1, ($1*1000)/N }
			$3=="cycles"           { printf "%-14s | cycles/op=%.0f  freq=%s %s\n",       L, $1/N, $6, $7 }
			$3=="instructions"     { printf "%-14s | inst/op=%.0f    IPC=%s\n",           L, $1/N, $6 }
			$3=="cache-references" { printf "%-14s | cache-refs/op=%.1f\n",               L, $1/N }
			$3=="cache-misses"     { printf "%-14s | cache-miss/op=%.2f  miss%%=%s\n",    L, $1/N, $6 }
		'
}
{
	echo '## perf stat (CPU consumption per cat)'
	echo
	echo '```'
	echo "iterations per label: $PERF_ITERS cats"
	perf_cat "$MP_FUSE"  's3fs-fuse'
	perf_cat "$MP_EBPF"  's3fs-ebpf B'
	[ "$FUSE_CACHE_STARTED" -eq 1 ] && perf_cat "$MP_FUSE_CACHE" 'fuse-cache'
	[ "$MOUNT_S3_STARTED"   -eq 1 ] && perf_cat "$MP_MOUNT_S3"   'mount-s3'
	echo '```'
} > "$OUT_DIR/perf.md"
cat "$OUT_DIR/perf.md" >&2

# ---------- 3. fio filestat: 並列度 IOPS スケール -----------------------------
log "fio filestat: numjobs=$FIO_JOBS runtime=${FIO_TIME}s"
run_fio() {
	local mp=$1 label=$2
	fio --name="$label" --ioengine=filestat --filename="$mp/$FILE" \
		--stat_type=stat --numjobs="$FIO_JOBS" \
		--runtime="$FIO_TIME" --time_based=1 --group_reporting=1 \
		--allow_file_create=0 --size=1 --bs=1 \
		2>&1 | grep -E 'IOPS|clat \(nsec\)' \
		| awk -v L="$label" '{ printf "%-14s | %s\n", L, $0 }'
}
{
	echo '## fio filestat (parallel stat IOPS)'
	echo
	echo '```'
	run_fio "$MP_FUSE"  's3fs-fuse'
	run_fio "$MP_EBPF"  's3fs-ebpf B'
	[ "$FUSE_CACHE_STARTED" -eq 1 ] && run_fio "$MP_FUSE_CACHE" 'fuse-cache'
	[ "$MOUNT_S3_STARTED"   -eq 1 ] && run_fio "$MP_MOUNT_S3"   'mount-s3'
	echo '```'
} > "$OUT_DIR/fio.md"
cat "$OUT_DIR/fio.md" >&2

# ---------- 4. large file throughput (1 MiB / 10 MiB / 100 MiB) --------------
BLOB_FILES=${BLOB_FILES:-big.bin 10m.bin 100m.bin}
for blob in $BLOB_FILES; do
	# Require file present on every started mount; skip blob if any is missing.
	missing=0
	for mp in "$MP_FUSE" "$MP_EBPF"; do
		[ -f "$mp/$blob" ] || { missing=1; break; }
	done
	[ "$FUSE_CACHE_STARTED" -eq 1 ] && [ ! -f "$MP_FUSE_CACHE/$blob" ] && missing=1
	[ "$MOUNT_S3_STARTED"   -eq 1 ] && [ ! -f "$MP_MOUNT_S3/$blob"   ] && missing=1
	if [ "$missing" -eq 1 ]; then
		log "skip $blob (missing on one or more mounts)"
		continue
	fi

	log "hyperfine: cat $blob throughput"
	blob_arms=()
	[ "$FUSE_CACHE_STARTED" -eq 1 ] && blob_arms+=(--command-name 'fuse-cache' "cat $MP_FUSE_CACHE/$blob")
	[ "$MOUNT_S3_STARTED"   -eq 1 ] && blob_arms+=(--command-name 'mount-s3'   "cat $MP_MOUNT_S3/$blob")
	hyperfine --warmup 2 --min-runs 10 --shell=none \
		--export-markdown "$OUT_DIR/hf-$blob.md" \
		--command-name 's3fs-fuse'   "cat $MP_FUSE/$blob" \
		--command-name 's3fs-ebpf B' "cat $MP_EBPF/$blob" \
		"${blob_arms[@]}" >&2
done

log "done. results in $OUT_DIR/"
