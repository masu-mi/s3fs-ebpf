#!/usr/bin/env bash
# scripts/bench-scale.sh — Week 4 (e) 追加: 並列度スケール。
#
# fio filestat (stat) と read (small file) を numjobs=1..64 で sweep し
# aggregate IOPS + avg latency (nsec) を取る。ebpf の kmod mutex 経路と
# FUSE daemon の single-threaded 経路のスケール限界を可視化する。

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
OUT_DIR=${OUT_DIR:-/tmp/bench-scale}
MP_EBPF=/mnt/s3demo
MP_FUSE_RAW=/mnt/s3fuse
MP_FUSE_CACHE=/mnt/s3fuse-cache
FUSE_CACHE_DIR=/tmp/s3fs-fuse-cache
EBPF_CACHE_DIR=/tmp/s3fs-ebpf-cache
FILE=${FILE:-hello.txt}
RUNTIME=${RUNTIME:-5}
JOBS_LIST=${JOBS_LIST:-"1 2 4 8 16 32 64"}

mkdir -p "$OUT_DIR" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR"
sudo chown "$(id -un):$(id -gn)" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR" 2>/dev/null || true

log() { echo "[bench-scale] $*" >&2; }

check_prereq() {
	need_cmds fio hyperfine s3fs
	need_built
	minio_require "$FILE"
}

start_fuse_raw() {
	log "mount fuse-raw at $MP_FUSE_RAW"
	fuse_mount_raw "$MP_FUSE_RAW"
}

start_fuse_cache() {
	log "mount fuse-cache at $MP_FUSE_CACHE"
	fuse_mount_cache "$MP_FUSE_CACHE" "$FUSE_CACHE_DIR"
}

start_ebpf() {
	log "load kmod + mount ebpf + start daemon"
	ebpf_mount "$MP_EBPF"
	ebpf_start_daemon "$OUT_DIR/fastpath.log" \
		--cache-dir="$EBPF_CACHE_DIR" --sample-every=60s --sync-every=60s
	# Prime kernel_cache on fuse-cache so 2nd+ reads hit it.
	cat "$MP_FUSE_CACHE/$FILE" >/dev/null 2>&1 || true
}

stop_all() {
	log "stopping"
	ebpf_stop "$MP_EBPF"
	umount_hard "$MP_FUSE_CACHE"
	umount_hard "$MP_FUSE_RAW"
}
trap stop_all EXIT

# ------------------------------------------------------------------
# fio filestat sweep. Extracts aggregate IOPS + avg clat.
# ------------------------------------------------------------------
run_stat() {
	local mp=$1 label=$2 jobs=$3
	fio --name="$label" --ioengine=filestat --filename="$mp/$FILE" \
		--stat_type=stat --numjobs="$jobs" \
		--runtime="$RUNTIME" --time_based=1 --group_reporting=1 \
		--allow_file_create=0 --size=1 --bs=1 2>&1 \
		| awk -v L="$label" -v J="$jobs" '
			/IOPS=/ {
				# read: IOPS=15.4M, BW=...
				match($0, /IOPS=[^,]+/); iops=substr($0, RSTART+5, RLENGTH-5)
			}
			/clat \(nsec\)/ {
				match($0, /avg=[0-9.]+/); avg=substr($0, RSTART+4, RLENGTH-4)
			}
			END { printf "%-11s %3d %10s %10s\n", L, J, iops, avg }
		'
}

# ------------------------------------------------------------------
# fio read sweep on the same $FILE, with numjobs threads. Small file
# so this reduces to "many threads doing tiny reads on a shared fd";
# for ebpf that hits the .read_folio → arena path repeatedly and
# stresses the folio lock, for fuse-raw it forces a FUSE daemon
# round-trip per read.
# ------------------------------------------------------------------
run_read() {
	local mp=$1 label=$2 jobs=$3
	fio --name="$label" --ioengine=psync --filename="$mp/$FILE" \
		--rw=read --numjobs="$jobs" \
		--runtime="$RUNTIME" --time_based=1 --group_reporting=1 \
		--allow_file_create=0 --size=1 --bs=1 --thread 2>&1 \
		| awk -v L="$label" -v J="$jobs" '
			/IOPS=/ {
				match($0, /IOPS=[^,]+/); iops=substr($0, RSTART+5, RLENGTH-5)
			}
			/clat \(nsec\)/ && !seen {
				match($0, /avg=[0-9.]+/); avg=substr($0, RSTART+4, RLENGTH-4)
				seen = 1
			}
			END { printf "%-11s %3d %10s %10s\n", L, J, iops, avg }
		'
}

# ------------------------------------------------------------------
check_prereq
start_fuse_raw
start_fuse_cache
start_ebpf

echo "" > "$OUT_DIR/stat.md"
{
	echo '## fio filestat parallel scale (stat, small file)'
	echo
	echo '```'
	printf "%-11s %3s %10s %10s\n" "label" "job" "IOPS" "clat_ns"
	for jobs in $JOBS_LIST; do
		log "stat numjobs=$jobs"
		run_stat "$MP_EBPF"       'ebpf'       "$jobs"
		run_stat "$MP_FUSE_RAW"   'fuse-raw'   "$jobs"
		run_stat "$MP_FUSE_CACHE" 'fuse-cache' "$jobs"
	done
	echo '```'
} > "$OUT_DIR/stat.md"
cat "$OUT_DIR/stat.md" >&2

{
	echo '## fio psync read scale (single small file, shared)'
	echo
	echo '```'
	printf "%-11s %3s %10s %10s\n" "label" "job" "IOPS" "clat_ns"
	for jobs in $JOBS_LIST; do
		log "read numjobs=$jobs"
		run_read "$MP_EBPF"       'ebpf'       "$jobs"
		run_read "$MP_FUSE_RAW"   'fuse-raw'   "$jobs"
		run_read "$MP_FUSE_CACHE" 'fuse-cache' "$jobs"
	done
	echo '```'
} > "$OUT_DIR/read.md"
cat "$OUT_DIR/read.md" >&2

log "done. results in $OUT_DIR/"
