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
OUT_DIR=${OUT_DIR:-/tmp/bench}
MP_EBPF=/mnt/s3demo
MP_FUSE=/mnt/s3fuse
FILE=${FILE:-hello.txt}
WARMUP=${WARMUP:-5}
RUNS=${RUNS:-200}
MICRO_ITERS=${MICRO_ITERS:-100000}
FIO_TIME=${FIO_TIME:-5}
FIO_JOBS=${FIO_JOBS:-4}

mkdir -p "$OUT_DIR"

log() { echo "[bench] $*" >&2; }

check_prereq() {
	for t in hyperfine s3fs fio; do
		command -v "$t" >/dev/null || { echo "missing $t"; exit 1; }
	done
	mountpoint -q "$MP_FUSE" || { echo "$MP_FUSE not mounted"; exit 1; }
	[ -f "$MP_FUSE/$FILE" ] || { echo "$MP_FUSE/$FILE missing"; exit 1; }
}

start_ebpf() {
	log "loading kmod + mounting + starting daemon"
	lsmod | grep -q '^s3fs_kfunc' || sudo insmod "$TOPDIR/kmod/s3fs_kfunc.ko"
	mountpoint -q "$MP_EBPF" || { sudo mkdir -p "$MP_EBPF"; sudo mount -t s3fs_ebpf none "$MP_EBPF"; }
	sudo pkill -9 -f 's3fs-ebpf fastpath' 2>/dev/null || true
	sleep 0.5
	LD_LIBRARY_PATH=$HOME/.local/lib sudo -E \
		"$TOPDIR/bin/s3fs-ebpf" fastpath --s3 --sample-every=60s \
		>"$OUT_DIR/fastpath.log" 2>&1 &
	for _ in $(seq 1 40); do
		grep -q '\[admit\] ' "$OUT_DIR/fastpath.log" && break
		sleep 0.2
	done
	sleep 0.2
	log "prewarm done"
}

stop_ebpf() {
	log "stopping ebpf stack"
	sudo pkill -INT -f 's3fs-ebpf fastpath' 2>/dev/null || true
	sleep 1
	sudo umount "$MP_EBPF" 2>/dev/null || true
	sudo rmmod s3fs_kfunc 2>/dev/null || true
}
trap stop_ebpf EXIT

check_prereq
start_ebpf

# ---------- 1. hyperfine: end-to-end (fork/exec 込み) ------------------
log "hyperfine: warm stat (end-to-end, $RUNS runs after $WARMUP warmups)"
hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none \
	--export-markdown "$OUT_DIR/hf-stat.md" \
	--command-name 's3fs-ebpf B' "stat --format=%s $MP_EBPF/$FILE" \
	--command-name 's3fs-fuse'   "stat --format=%s $MP_FUSE/$FILE" >&2

log "hyperfine: warm cat"
hyperfine --warmup "$WARMUP" --min-runs "$RUNS" --shell=none \
	--export-markdown "$OUT_DIR/hf-cat.md" \
	--command-name 's3fs-ebpf B' "cat $MP_EBPF/$FILE" \
	--command-name 's3fs-fuse'   "cat $MP_FUSE/$FILE" >&2

# ---------- 2. microbench: syscall-level (fork/exec 排除) --------------
log "microbench: syscall-level stat/read ($MICRO_ITERS iters each)"
BIN="$TOPDIR/bin/s3fs-ebpf"
{
	echo '## microbench (syscall-level, no fork/exec)'
	echo
	for op in stat read; do
		echo "### $op $FILE"
		echo '```'
		"$BIN" microbench --op="$op" --path="$MP_EBPF/$FILE" \
			--iters="$MICRO_ITERS" --label='s3fs-ebpf B '
		"$BIN" microbench --op="$op" --path="$MP_FUSE/$FILE" \
			--iters="$MICRO_ITERS" --label='s3fs-fuse   '
		echo '```'
		echo
	done
} > "$OUT_DIR/micro.md"
cat "$OUT_DIR/micro.md" >&2

# ---------- 3. fio filestat: 並列度 IOPS スケール ----------------------
log "fio filestat: numjobs=$FIO_JOBS runtime=${FIO_TIME}s"
run_fio() {
	local mp=$1 label=$2
	# --allow_file_create=0: fio が demo file を作り直そうとして
	#   kmod の readonly unlink で失敗するのを回避。
	# --filesize/--size/--bs は demo object (16 B) 相当。
	fio --name="$label" --ioengine=filestat --filename="$mp/$FILE" \
		--stat_type=stat --numjobs="$FIO_JOBS" \
		--runtime="$FIO_TIME" --time_based=1 --group_reporting=1 \
		--allow_file_create=0 --filesize=16 --size=16 --bs=16 \
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

log "done. results in $OUT_DIR/"
