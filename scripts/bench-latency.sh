#!/usr/bin/env bash
# scripts/bench-latency.sh — 3 段の latency bench:
#   1. hyperfine: end-to-end (fork/exec 込み)
#   2. microbench: syscall-level (fork/exec 排除、Go の time.Now を直接使う)
#   3. fio filestat: 並列度上げた IOPS スケール
#
# 対象: s3fs-ebpf (Path B, admit 済) と s3fs-fuse
# 前提:
#   - MinIO up (docker compose)
#   - s3fs-fuse が /mnt/s3fuse に uid=$USER でマウント済 (README 参照)
#   - kmod + daemon ビルド済 (make build)

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
OUT_DIR=${OUT_DIR:-/tmp/bench}
MP_EBPF=/mnt/s3demo
MP_FUSE=/mnt/s3fuse
FILE=${FILE:-hello.txt}
BIG_FILE=${BIG_FILE:-big.bin}
WARMUP=${WARMUP:-5}
RUNS=${RUNS:-200}
# fuse read は ~5 ms/iter なので 100000 だと 8 分。開発サイクルに乗る
# 5000 (~25s) を default に。ちゃんとした数字を取るときは
# MICRO_ITERS=100000 make bench で override。
MICRO_ITERS=${MICRO_ITERS:-5000}
FIO_TIME=${FIO_TIME:-5}
FIO_JOBS=${FIO_JOBS:-4}

mkdir -p "$OUT_DIR"

log() { echo "[bench] $*" >&2; }

check_prereq() {
	need_cmds hyperfine s3fs fio
	need_built
	# The object must exist on the S3 side, otherwise the fuse mount will
	# not see it (see docs/dev-notes/lessons-learned.md §3.1 note on the
	# "hello.txt disappeared from MinIO" incident).
	minio_require "$FILE"
}

# Auto-mount fuse-raw at $MP_FUSE so runs from a clean host (`make
# compose-up; make all; make bench`) succeed. An operator-owned mount is
# reused and left in place.
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

start_ebpf() {
	log "loading kmod + mounting + starting daemon"
	ebpf_mount "$MP_EBPF"
	ebpf_start_daemon "$OUT_DIR/fastpath.log" --sample-every=60s --metrics-addr=:9600
	log "prewarm done"
}

stop_ebpf() {
	log "stopping ebpf stack"
	ebpf_stop "$MP_EBPF"
	# Only unmount fuse if we brought it up (avoid stealing an
	# operator-owned pre-existing mount).
	if [ "$FUSE_STARTED" -eq 1 ]; then
		umount_hard "$MP_FUSE"
	fi
}
trap stop_ebpf EXIT

check_prereq
start_fuse
start_ebpf

# ---------- 1. hyperfine: end-to-end (fork/exec 込み) ------------------
log "hyperfine: warm stat (end-to-end, $RUNS runs after $WARMUP warmups)"
# hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none --style=basic \
hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none \
	--export-markdown "$OUT_DIR/hf-stat.md" \
	--command-name 's3fs-ebpf B' "stat --format=%s $MP_EBPF/$FILE" \
	--command-name 's3fs-fuse'   "stat --format=%s $MP_FUSE/$FILE" >&2

log "hyperfine: warm cat"
hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none \
	--export-markdown "$OUT_DIR/hf-cat.md" \
	--command-name 's3fs-ebpf B' "cat $MP_EBPF/$FILE" \
	--command-name 's3fs-fuse'   "cat $MP_FUSE/$FILE" >&2

# ---------- 1b. hyperfine: cold cat (kernel page cache drop) -----------
# 毎回の cat 前に sync + drop_caches。s3fs-fuse は fuse daemon の内部
# キャッシュを飛ばせないので厳密な "cold" ではないが、少なくとも VFS
# page cache の効きを除外できる。s3fs-ebpf B は admit table が kmod
# 側で残るので B path のまま (VFS page cache の外)。
log "hyperfine: cold cat (drop_caches per run)"
# --shell=none で prepare は exec 直接なので複合コマンド不可 → sysctl 単発。
hyperfine --warmup 2 --min-runs 30 --shell=none \
	--prepare 'sudo sysctl -q -w vm.drop_caches=3' \
	--export-markdown "$OUT_DIR/hf-cat-cold.md" \
	--command-name 's3fs-ebpf B' "cat $MP_EBPF/$FILE" \
	--command-name 's3fs-fuse'   "cat $MP_FUSE/$FILE" >&2 || true

# ---------- 2. microbench: syscall-level (fork/exec 排除) --------------
log "microbench: syscall-level stat/read ($MICRO_ITERS iters each)"
BIN="$S3FS_BIN"
{
	echo '## microbench (syscall-level, no fork/exec)'
	echo
	for op in stat read; do
		echo "### $op $FILE"
		echo '```'
		"$BIN" dev microbench --op="$op" --path="$MP_EBPF/$FILE" \
			--iters="$MICRO_ITERS" --label='s3fs-ebpf B '
		"$BIN" dev microbench --op="$op" --path="$MP_FUSE/$FILE" \
			--iters="$MICRO_ITERS" --label='s3fs-fuse   '
		echo '```'
		echo
	done
} > "$OUT_DIR/micro.md"
cat "$OUT_DIR/micro.md" >&2

# ---------- 2b. perf stat: CPU consumption --------------------------
# 各 FS で cat をループ実行し cycles / instructions / cache-misses を
# 見る。1 cat あたりの CPU 消費が Fast Path でどれだけ削れるかの根拠。
# perf-shim は kernel version 依存でエラーになるので手掘り fallback。
PERF_BIN=$(perf_bin)
log "perf stat: CPU consumption (using $PERF_BIN)"
PERF_ITERS=$(( MICRO_ITERS / 100 ))
perf_cat() {
	local mp=$1 label=$2
	# -x, で CSV: value,unit,event,run_ns,pct,derived,derived_unit
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
	perf_cat "$MP_EBPF" 's3fs-ebpf B'
	perf_cat "$MP_FUSE" 's3fs-fuse'
	echo '```'
} > "$OUT_DIR/perf.md"
cat "$OUT_DIR/perf.md" >&2

# ---------- 3. fio filestat: 並列度 IOPS スケール ----------------------
log "fio filestat: numjobs=$FIO_JOBS runtime=${FIO_TIME}s"
run_fio() {
	local mp=$1 label=$2
	# --allow_file_create=0: fio が demo file を作り直そうとして
	#   kmod の readonly unlink で失敗するのを回避。
	# --size=1 --bs=1: 実 file size に依存しない最小単位。time_based=1
	#   で runtime 秒回す。--filesize や大きな --size を与えると fio が
	#   「file が指定サイズより小さい」と判断して unlink→再 layout を
	#   試み、readonly FS で iops=0 になる。
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
	run_fio "$MP_EBPF" 's3fs-ebpf B'
	run_fio "$MP_FUSE" 's3fs-fuse'
	echo '```'
} > "$OUT_DIR/fio.md"
cat "$OUT_DIR/fio.md" >&2


# ---------- 4. large file cat throughput (1 MiB / 10 MiB / 100 MiB) ---
# Ranges the arena capacity (kmod bumped to 128 MiB for Week 4 (e)).
# BLOB_FILES env can override the size sweep; default hits the three
# seeded blobs.
BLOB_FILES=${BLOB_FILES:-big.bin 10m.bin 100m.bin}
for blob in $BLOB_FILES; do
	if [ ! -f "$MP_EBPF/$blob" ] || [ ! -f "$MP_FUSE/$blob" ]; then
		log "skip $blob (missing on one mount)"
		continue
	fi
	log "hyperfine: cat $blob throughput"
	hyperfine --warmup 2 --min-runs 10 --shell=none \
		--export-markdown "$OUT_DIR/hf-$blob.md" \
		--command-name 's3fs-ebpf B' "cat $MP_EBPF/$blob" \
		--command-name 's3fs-fuse'   "cat $MP_FUSE/$blob" >&2
done

log "done. results in $OUT_DIR/"
