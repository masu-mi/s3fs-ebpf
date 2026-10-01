#!/usr/bin/env bash
# scripts/bench-scale.sh — Week 4 (e) 追加: 並列度スケール。
#
# fio filestat (stat) と read (small file) を numjobs=1..64 で sweep し
# aggregate IOPS + avg latency (nsec) を取る。ebpf の kmod mutex 経路と
# FUSE daemon の single-threaded 経路のスケール限界を可視化する。

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/umount.sh
source "$TOPDIR/scripts/lib/umount.sh"
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
sudo chown "$USER:$USER" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR" 2>/dev/null || true

log() { echo "[bench-scale] $*" >&2; }

check_prereq() {
	for t in fio hyperfine s3fs; do
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
	log "mount fuse-cache at $MP_FUSE_CACHE"
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
	# Prime kernel_cache on fuse-cache so 2nd+ reads hit it.
	cat "$MP_FUSE_CACHE/$FILE" >/dev/null 2>&1 || true
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
