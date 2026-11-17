#!/usr/bin/env bash
# phase timing of 4 KiB mid-write + fsync on an existing 10 MiB object (write vs fsync, per backend)
set -euo pipefail
cd /home/masumi/dev/src/github.com/masu-mi/s3fs-ebpf/.claude/worktrees/bridge-cse_01Azpk8YZH5oNQ7CHFH2kHX1
TOPDIR=$PWD
OUT=/tmp/phase/out; mkdir -p $OUT
source scripts/lib/bench.sh
S3_BUCKET=s3fs-bench-s18
mc() { docker exec "$MINIO_CONTAINER" mc "$@"; }
mc mb --ignore-existing local/$S3_BUCKET >/dev/null
head -c 4096 /dev/urandom > $OUT/patch4k
for be in ${BACKENDS:-fuse-cache ebpf-pcache}; do
  for i in $(seq 1 8); do head -c $((10*1024*1024)) /dev/urandom | docker exec -i "$MINIO_CONTAINER" mc pipe local/$S3_BUCKET/ph-$be-$i.bin >/dev/null; done
  bench_up $be $OUT/$be.log --sample-every=60s --sync-every=0
  for i in $(seq 1 8); do
    echo -n "$be "; sudo python3 /tmp/phase/wf.py $BENCH_MP/ph-$be-$i.bin
  done
  bench_down
done
