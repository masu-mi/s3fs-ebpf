#!/usr/bin/env bash
# bench-read-64jobs.sh — raw fio JSON for the AB-07 gate (a) workload: 64 threads,
# psync, bs=1 reads of one small file (hello.txt), time_based RUNTIME s, REPS runs per
# backend.  Same fio line as bench-ab-pcache.sh measure_fio_iops, but the full
# `--output-format=json` is kept so another run can be compared later.
# Backends: fuse-raw, fuse-cache, ebpf-arena, ebpf-pcache, ebpf-pcache-nohook.
set -euo pipefail
TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}; MP_FUSE_RAW=${MP_FUSE_RAW:-/mnt/s3fuse}; MP_FUSE_CACHE=${MP_FUSE_CACHE:-/mnt/s3fuse-cache}
FUSE_CACHE_DIR=${FUSE_CACHE_DIR:-/tmp/s3fs-fuse-cache-64j}; EBPF_CACHE_DIR=${EBPF_CACHE_DIR:-/tmp/s3fs-ebpf-cache-64j}
OUT=${OUT:-/tmp/bench-read-64jobs}; FILE=${FILE:-hello.txt}
JOBS=${JOBS:-64}; RUNTIME=${RUNTIME:-5}; REPS=${REPS:-5}; SETTLE=${SETTLE:-10}
need_cmds fio jq docker; need_built; minio_require "$FILE"
mkdir -p "$OUT"; rm -f "$OUT"/*.json
FC=0 FR=0
cleanup() { ebpf_stop "$MOUNT" || true; bench_down; }
trap cleanup EXIT
log() { echo "[read-64jobs] $*" >&2; }

fio_runs() { # label mp
	local label=$1 mp=$2 r
	sudo cat "$mp/$FILE" >/dev/null   # warm path (miss handler / caches)
	for r in $(seq "$REPS"); do
		sync; sudo sysctl -q -w vm.drop_caches=1
		fio --name="$label" --ioengine=psync --filename="$mp/$FILE" --rw=read --numjobs="$JOBS" \
			--runtime="$RUNTIME" --time_based=1 --group_reporting=1 --allow_file_create=0 --size=1 --bs=1 --thread \
			--output-format=json --output="$OUT/$label.$r.json"
		log "$label #$r IOPS=$(jq '.jobs[0].read.iops|floor' "$OUT/$label.$r.json")"
	done
}
ebpf_run() { # label flags...
	local label=$1; shift
	sudo rm -rf "$EBPF_CACHE_DIR"; mkdir -p "$EBPF_CACHE_DIR"
	ebpf_mount "$MOUNT"
	ebpf_start_daemon "$OUT/daemon-$label.log" --cache-dir="$EBPF_CACHE_DIR" --policy-bpf=builtin:policy_observe_ops \
		--metrics-addr=:9600 --sample-every=2s --sync-every=60s --softcap=8 --cache-capacity=0 "$@"
	sleep "$SETTLE"
	fio_runs "$label" "$MOUNT"
	ebpf_stop "$MOUNT" || true
}
BACKENDS=${BACKENDS:-$BENCH_BACKENDS_ALL}
{
	echo "date_utc: $(date -u +%FT%TZ)"; echo "git: $(git -C "$TOPDIR" rev-parse HEAD)"
	echo "kernel: $(uname -srv)"; echo "cpus: $(nproc)"; echo "s3fs: $(s3fs --version 2>&1 | head -1)"; echo "fio: $(fio --version)"
	echo "jobs=$JOBS runtime=${RUNTIME}s reps=$REPS file=$FILE (psync, bs=1, size=1, --thread)"
	echo "backends: $BACKENDS"; echo "ebpf-pcache: --arena-metadata-only; nohook adds --no-read-iter-hook --pattern-aware-admit=false; arena = neither"
} >"$OUT/environment.txt"

# BACKENDS: the six configurations of issue #35 by default; the old ebpf-arena /
# ebpf-pcache-nohook variants stay available by name.
for b in $BACKENDS; do
	case $b in
	ebpf-arena|ebpf-pcache-nohook) ;;   # handled by ebpf_run below
	*) bench_up "$b" "$OUT/daemon-$b.log" --sample-every=2s --sync-every=60s --softcap=8 --cache-capacity=0
	   [ "$BENCH_KIND" = ebpf ] && sleep "$SETTLE"
	   fio_runs "$b" "$BENCH_MP"; bench_down ;;
	esac
done
for b in $BACKENDS; do
	case $b in
	ebpf-arena) ebpf_run ebpf-arena ;;
	ebpf-pcache-nohook) ebpf_run ebpf-pcache-nohook --arena-metadata-only --no-read-iter-hook --pattern-aware-admit=false ;;
	esac
done

echo
printf '%-20s %12s %10s %10s %10s\n' backend iops_median iops_min iops_max clat_p99_us
for b in $BACKENDS; do
	jq -rs --arg b "$b" '[.[]|{i:.jobs[0].read.iops, p:(.jobs[0].read.clat_ns.percentile["99.000000"] // 0)}] as $v
		| ($v|map(.i)|sort) as $i | ($v|map(.p)|sort) as $p
		| [$b, ($i[($i|length/2|floor)]|floor), ($i[0]|floor), ($i[-1]|floor), ($p[($p|length/2|floor)]/1000|.*10|floor/10)] | @tsv' "$OUT/$b".[0-9]*.json
done | column -t
