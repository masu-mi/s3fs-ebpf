#!/usr/bin/env bash
# scripts/bench-capacity-pass.sh — what one capacity pass (s3fs_ebpf_capacity_pass)
# costs as the number of cached / instantiated inodes grows.
#
# Phase 3c collects the pass's candidates by walking sb->s_inodes (cost: all
# inodes of the superblock, cached or not); Phase 3b walked the admit hash
# (cost: cached entries only).  The interesting axis is therefore the number of
# inodes that are instantiated but NOT cached (stat only).
#
# Setup per cell: NCOLD files stat'ed (inodes exist, nothing cached), NHOT files
# read (cached).  Then POST /policy/eval PASSES times; a bpftrace kprobe /
# kretprobe on s3fs_ebpf_capacity_pass records each pass's duration (ns).
# CAPACITY large = collection + policy only; CAPACITY small = also evicts.
#
#   NCOLDS="0 1000 10000" NHOT=100 PASSES=200 scripts/bench-capacity-pass.sh
#   (run it from the checkout under test; the result file names the commit)
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/bench-capacity-pass}
NCOLDS=${NCOLDS:-"0 1000 10000"}
NHOT=${NHOT:-100}
PASSES=${PASSES:-200}
CAPACITY=${CAPACITY:-$((1024 * 1024 * 1024))}
BUCKET=${CAPPASS_BUCKET:-s3fs-capacity-pass}
METRICS=${METRICS_ADDR:-http://localhost:9600}
mkdir -p "$OUT"
need_cmds docker curl bpftrace
need_built

log() { printf '[capacity-pass] %s\n' "$*" >&2; }
MC() { docker exec "$MINIO_CONTAINER" mc "$@"; }

MAXCOLD=0
for n in $NCOLDS; do [ "$n" -gt "$MAXCOLD" ] && MAXCOLD=$n; done
SEED=$(mktemp -d /tmp/capacity-pass-seed-XXXXXX)
BT_PID=""
cleanup() {
	[ -n "$BT_PID" ] && sudo kill "$BT_PID" 2>/dev/null || true
	ebpf_stop "$MOUNT" >/dev/null 2>&1 || true
	MC rb --force "local/$BUCKET" >/dev/null 2>&1 || true
	rm -rf "$SEED"
}
trap cleanup EXIT

MC alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
MC rb --force "local/$BUCKET" >/dev/null 2>&1 || true
MC mb --ignore-existing "local/$BUCKET" >/dev/null
mkdir -p "$SEED/hot" "$SEED/cold"
for i in $(seq 1 "$NHOT"); do head -c 4096 /dev/urandom >"$SEED/hot/h$i"; done
for i in $(seq 1 "$MAXCOLD"); do printf 'c%d\n' "$i" >"$SEED/cold/c$i"; done
log "seeding $NHOT hot + $MAXCOLD cold objects"
docker cp "$SEED/." "$MINIO_CONTAINER:/tmp/seed" >/dev/null
MC cp --recursive /tmp/seed/ "local/$BUCKET/" >/dev/null
docker exec "$MINIO_CONTAINER" rm -rf /tmp/seed

sudo sysctl -q -w kernel.bpf_stats_enabled=0 || true
HEAD=$(git -C "$TOPDIR" rev-parse --short HEAD)
RES="$OUT/result-$HEAD.txt"
{
	echo "# commit $HEAD ($(git -C "$TOPDIR" log -1 --format=%s)); kernel $(uname -r); NHOT=$NHOT PASSES=$PASSES CAPACITY=$CAPACITY"
	echo "# cap_mode ncold inodes_delta n min_ns p50_ns p90_ns max_ns"
} >"$RES"

run_cell() {
	local capmode=$1 cap=$2 ncold=$3
	ebpf_mount "$MOUNT"
	ebpf_start_daemon "$OUT/daemon.log" --bucket="$BUCKET" --arena-metadata-only \
		--policy-bpf="builtin:policy_observe_ops" --softcap=1000000 \
		--cache-capacity="$cap" --metrics-addr=:9600 --sample-every=1s --sync-every=0
	sleep 2
	local ino0 ino1
	ino0=$(awk '{print $1}' /proc/sys/fs/inode-nr)
	# instantiate ncold inodes without caching content (stat only)
	if [ "$ncold" -gt 0 ]; then
		for i in $(seq 1 "$ncold"); do echo "$MOUNT/cold/c$i"; done | sudo xargs -d '\n' -n 500 stat -c %s >/dev/null
	fi
	for i in $(seq 1 "$NHOT"); do sudo cat "$MOUNT/hot/h$i" >/dev/null; done
	ino1=$(awk '{print $1}' /proc/sys/fs/inode-nr)
	sleep 1
	sudo bpftrace -e '
kprobe:s3fs_ebpf_capacity_pass { @s[tid] = nsecs; }
kretprobe:s3fs_ebpf_capacity_pass /@s[tid]/ { printf("T %llu\n", nsecs - @s[tid]); delete(@s[tid]); }' \
		>"$OUT/bt-$capmode-$ncold.txt" 2>&1 &
	BT_PID=$!
	sleep 3
	for _ in $(seq 1 "$PASSES"); do curl -sf -X POST "$METRICS/policy/eval" >/dev/null || true; done
	sleep 1
	sudo kill -INT "$BT_PID" 2>/dev/null || true
	wait "$BT_PID" 2>/dev/null || true
	BT_PID=""
	grep '^T ' "$OUT/bt-$capmode-$ncold.txt" | awk '{print $2}' | sort -n | awk -v m="$capmode" -v c="$ncold" -v d="$((ino1 - ino0))" '
		{ a[NR] = $1 } END { if (!NR) { print m, c, d, 0, "no-samples"; exit }
		  printf "%s %d %d %d %d %d %d %d\n", m, c, d, NR, a[1], a[int((NR+1)/2)], a[int(NR*0.9)], a[NR] }' | tee -a "$RES" >&2
	sudo dmesg | tail -200 | grep -E "BUG|Oops|WARNING|lockdep" && { echo "kernel warning" >&2; exit 1; } || true
	ebpf_stop "$MOUNT" >/dev/null 2>&1 || true
}

HOTBYTES=$((NHOT * 4096))
for n in $NCOLDS; do
	run_cell nocap "$CAPACITY" "$n"
	run_cell evict "$((HOTBYTES / 2))" "$n"
done
log "result: $RES"
cat "$RES"
