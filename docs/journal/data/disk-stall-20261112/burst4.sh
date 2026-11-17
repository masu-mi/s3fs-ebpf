#!/usr/bin/env bash
# usage: burst4.sh <mode> <round>   modes: direct direct+local fuse-raw ebpf-nocache ebpf-pcache
set -euo pipefail
cd /home/masumi/dev/src/github.com/masu-mi/s3fs-ebpf/.claude/worktrees/bridge-cse_01Azpk8YZH5oNQ7CHFH2kHX1
TOPDIR=$PWD; source scripts/lib/bench.sh
mode=$1; rnd=$2; N=${N:-60}
OUT=/tmp/phase/b4; mkdir -p $OUT; tag=$mode-$rnd
S3_BUCKET=s3fs-burst4
C=$MINIO_CONTAINER
docker exec $C mc mb --ignore-existing local/$S3_BUCKET >/dev/null
[ -f $OUT/p100 ] || head -c $((100*1024*1024)) /dev/urandom > $OUT/p100
case $mode in direct|direct+local) ;; *) bench_up $mode $OUT/$tag.daemon.log --sample-every=60s --sync-every=0 ;; esac
sampler() { prev=""; while :; do
  l=$(awk '$3=="sda"{print $8, $10, $11, $13}' /proc/diskstats)   # writes, sectors, ms_writing, ms_io
  now=$(date +%s.%N); d=$(awk '/^Dirty:/{d=$2}/^Writeback:/{w=$2}END{printf "%d %d", d/1024, w/1024}' /proc/meminfo)
  psi=$(awk '/^some/{split($5,a,"=");print a[2]}' /proc/pressure/io)
  echo "$now $l $d $psi $(awk '/^pgscan_direct /{print $2}' /proc/vmstat) $(awk '/^allocstall_normal /{print $2}' /proc/vmstat)"; sleep 1; done; }
sampler > $OUT/$tag.samples &
SP=$!
: > $OUT/$tag.iters
for i in $(seq 1 $N); do
  t=$EPOCHREALTIME
  case $mode in
    direct) docker exec -i $C mc pipe local/$S3_BUCKET/o-$((i%4)) < $OUT/p100 >/dev/null ;;
    direct+local) docker exec -i $C mc pipe local/$S3_BUCKET/o-$((i%4)) < $OUT/p100 >/dev/null; dd if=$OUT/p100 of=$OUT/local-$((i%2)) bs=1M 2>/dev/null ;;
    *) sudo dd if=$OUT/p100 of=$BENCH_MP/b-$((i%4)).bin bs=1M conv=fsync 2>/dev/null ;;
  esac
  echo "$t $(awk -v a=$t -v b=$EPOCHREALTIME 'BEGIN{printf "%.0f",(b-a)*1000}')" >> $OUT/$tag.iters
done
kill $SP 2>/dev/null || true
case $mode in direct|direct+local) ;; *) bench_down ;; esac
docker exec $C mc rb --force local/$S3_BUCKET >/dev/null 2>&1 || true
rm -f $OUT/local-0 $OUT/local-1
