#!/usr/bin/env bash
# scripts/bench-policy-cost.sh — AC-58: what a struct_ops policy costs on the
# read path.  fio psync bs=1 reads of one small file (every read passes the
# on_hit hook), numjobs=1/16/64, arms interleaved over ROUNDS so drift hits
# every arm equally.  `none` loads no policy program.  With bpf_stats on, also
# prints run_cnt / avg ns per struct_ops program after each arm.
#
#   ARMS="none sieve_ops sieve_pick_ops" ROUNDS=3 scripts/bench-policy-cost.sh
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
ARMS=${ARMS:-none sieve_ops sieve_pick_ops}
ROUNDS=${ROUNDS:-3}
JOBS_LIST=${JOBS_LIST:-"1 16 64"}
RUNTIME=${RUNTIME:-5}
FILE=${FILE:-hello.txt}
MP=/mnt/s3demo
OUT=${OUT:-/tmp/bench-policy-cost}
mkdir -p "$OUT"
need_cmds fio
need_built
minio_require "$FILE"
trap 'sudo sysctl -q -w kernel.bpf_stats_enabled=0 >/dev/null 2>&1 || true; ebpf_stop "$MP" >/dev/null 2>&1 || true' EXIT
sudo sysctl -q -w kernel.bpf_stats_enabled=1

iops() {
	fio --name=r --ioengine=psync --filename="$MP/$FILE" --rw=read --numjobs="$1" \
		--runtime="$RUNTIME" --time_based=1 --group_reporting=1 \
		--allow_file_create=0 --size=1 --bs=1 --thread 2>&1 |
		awk '/IOPS=/ { match($0, /IOPS=[^,]+/); print substr($0, RSTART+5, RLENGTH-5); exit }'
}

for r in $(seq "$ROUNDS"); do
	for arm in $ARMS; do
		flags=()
		[ "$arm" = none ] || flags=(--policy-bpf="builtin:policy_${arm}")
		ebpf_mount "$MP"
		ebpf_start_daemon "$OUT/daemon-$arm.log" --sample-every=60s --sync-every=60s "${flags[@]}"
		cat "$MP/$FILE" >/dev/null
		for j in $JOBS_LIST; do
			echo "RESULT round=$r arm=$arm jobs=$j iops=$(iops "$j")"
		done
		sudo bpftool prog show 2>/dev/null | awk -v A="$arm" -v R="$r" '
			/struct_ops/ { name=$0; getline; if ($0 ~ /run_time_ns/) {
				match($0, /run_time_ns [0-9]+/); t=substr($0, RSTART+12, RLENGTH-12)
				match($0, /run_cnt [0-9]+/); c=substr($0, RSTART+8, RLENGTH-8)
				if (c > 0) printf "PROG round=%s arm=%s %s run_cnt=%d avg_ns=%.0f\n", R, A, name, c, t/c } }' |
			sed -E 's/[0-9]+: struct_ops +//; s/ +tag [0-9a-f]+//; s/ +gpl//'
		ebpf_stop "$MP"
	done
done
