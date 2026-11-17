#!/usr/bin/env bash
set -euo pipefail
cd /home/masumi/dev/src/github.com/masu-mi/s3fs-ebpf/.claude/worktrees/bridge-cse_01Azpk8YZH5oNQ7CHFH2kHX1
TOPDIR=$PWD; OUT=/tmp/phase/b3; mkdir -p $OUT
source scripts/lib/bench.sh
S3_BUCKET=s3fs-burst3
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing local/$S3_BUCKET >/dev/null
head -c $((100*1024*1024)) /dev/urandom > $OUT/p100
be=${BE:-ebpf-pcache}
bench_up $be $OUT/$be.log --sample-every=60s --sync-every=0
( while :; do echo "$(date +%s.%N) $(awk '/^some/{split($2,a,"=");print a[2]}' /proc/pressure/io|head -1) $(awk '/^Dirty:/{d=$2}/^Writeback:/{w=$2}END{print d/1024, w/1024}' /proc/meminfo) $(vmstat 1 2 | tail -1 | awk '{print "bi="$9" bo="$10" wa="$16" st="$17" free="$4}')"; done ) > $OUT/samples.txt &
SP=$!
for i in $(seq 1 ${N:-40}); do
  t=$EPOCHREALTIME
  sudo dd if=$OUT/p100 of=$BENCH_MP/b-$i.bin bs=1M conv=fsync 2>/dev/null
  echo "$t $(awk -v a=$t -v b=$EPOCHREALTIME 'BEGIN{printf "%.0f",(b-a)*1000}')" >> $OUT/iters.txt
done
kill $SP 2>/dev/null || true
bench_down
docker exec "$MINIO_CONTAINER" mc rb --force local/$S3_BUCKET >/dev/null; rm -f $OUT/p100
