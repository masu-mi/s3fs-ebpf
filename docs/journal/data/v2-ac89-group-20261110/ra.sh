#!/usr/bin/env bash
set -euo pipefail
TOPDIR=$(pwd)
source "$TOPDIR/scripts/lib/bench.sh"
MOUNT=/mnt/s3demo; S3_BUCKET=s3fs-ac89; OUT=${OUT:-/tmp/ra}; mkdir -p "$OUT"
need_built
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
if ! docker exec "$MINIO_CONTAINER" mc stat "local/$S3_BUCKET/big.bin" >/dev/null 2>&1; then
  dd if=/dev/urandom bs=1M count=100 iflag=fullblock 2>/dev/null | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/big.bin" >/dev/null
fi
trap 'ebpf_stop "$MOUNT" >/dev/null 2>&1 || true' EXIT
run() { # label cap group
  ebpf_mount "$MOUNT"; ebpf_open_check 0
  S3FS_RA_GROUP=$3 ebpf_start_daemon "$OUT/d-$1.log" --bucket="$S3_BUCKET" --arena-metadata-only \
     --policy-bpf=builtin:policy_observe_ops --softcap=64 --cache-capacity="$2" --metrics-addr=:9600 --sample-every=1s --sync-every=0
  sleep 4
  local r=""
  for i in 1 2 3 4 5; do
    echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null
    local t0=$(date +%s%N); sudo cat "$MOUNT/big.bin" >/dev/null; local t1=$(date +%s%N)
    r="$r $(( (t1-t0)/1000000 ))"
  done
  echo "$1:$r"
  ebpf_stop "$MOUNT" >/dev/null 2>&1 || true
}
M=$((1024*1024))
for round in 1 2; do
 run unbounded 0 1
 run cap32-g1 $((32*M)) 1
 run cap32-g2 $((32*M)) 2
 run cap32-g3 $((32*M)) 3
done
