#!/usr/bin/env bash
# MinIO HEAD/GET deltas for N open+read+close of hello.txt, per backend / open_check
set -euo pipefail
cd "$1"; TOPDIR=$PWD; S3_BUCKET=s3fs-bench-s18
source scripts/lib/bench.sh
cnt() { sleep 12; curl -s http://localhost:9000/minio/v2/metrics/cluster | awk -v a="$1" '$0 ~ "^minio_s3_requests_total\\{api=\""a"\"" {print $2}'; }
trap 'bench_down' EXIT
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
printf 'Hello, s3fs-ebpf!\n' | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/hello.txt" >/dev/null
N=1000
for cfg in "fuse-raw 0" "ebpf-nocache 0" "ebpf-nocache 1"; do
  set -- $cfg; be=$1; export BENCH_OPEN_CHECK=$2
  bench_up "$be" "/tmp/claude-1000/-home-masumi-dev-src-github-com-masu-mi-s3fs-ebpf--claude-worktrees-bridge-cse-01Azpk8XEC6H6mEosrxiZAeh/016a0002-ee2f-56f4-98ef-f2a312ee2d66/scratchpad/count-$be-$BENCH_OPEN_CHECK.log" --sync-every=0
  sleep 3; sudo cat "$BENCH_MP/hello.txt" >/dev/null
  g0=$(cnt getobject); h0=$(cnt headobject)
  "$S3FS_BIN" dev microbench --op=read --path="$BENCH_MP/hello.txt" --iters=$N --warmup=0 --label=x >/dev/null
  g1=$(cnt getobject); h1=$(cnt headobject)
  echo "$be open_check=$BENCH_OPEN_CHECK N=$N: GET +$((g1-g0))  HEAD +$((h1-h0))"
  bench_down
done
