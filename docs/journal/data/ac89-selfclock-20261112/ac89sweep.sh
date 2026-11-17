#!/usr/bin/env bash
# AC-89: 100 MiB cold read, capacity 32 MiB vs unbounded; counts + timings per run
set -euo pipefail
TOPDIR=$(pwd); source "$TOPDIR/scripts/lib/bench.sh"
MOUNT=/mnt/s3demo; S3_BUCKET=s3fs-ac89; OUT=${OUT:-/tmp/ac89}; mkdir -p "$OUT"
need_built
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
docker exec "$MINIO_CONTAINER" mc stat "local/$S3_BUCKET/big.bin" >/dev/null 2>&1 || dd if=/dev/urandom bs=1M count=100 iflag=fullblock 2>/dev/null | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/big.bin" >/dev/null
trap 'ebpf_stop "$MOUNT" >/dev/null 2>&1 || true' EXIT
M=$((1024*1024))
metric() { curl -s localhost:9600/metrics | awk -v n="$1" '$1==n{print $2+0}'; }
run() { # label cap
  ebpf_mount "$MOUNT"; ebpf_open_check 0
  ebpf_start_daemon "$OUT/d-$1.log" --bucket="$S3_BUCKET" --arena-metadata-only --policy-bpf=builtin:policy_observe_ops --softcap=64 --cache-capacity="$2" --metrics-addr=:9600 --sample-every=1s --sync-every=0
  sleep 4
  local r=""
  for i in 1 2 3 4 5; do
    echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null
    local u0; u0=$(metric s3fs_ebpf_read_miss_upcalls_total)
    local t0=$(date +%s%N); sudo cat "$MOUNT/big.bin" >/dev/null; local t1=$(date +%s%N)
    r="$r $(( (t1-t0)/1000000 ))ms/up$(( $(metric s3fs_ebpf_read_miss_upcalls_total)-u0 ))"
  done
  curl -s localhost:9600/metrics | grep -E '^s3fs_ebpf_read_miss_latency_seconds_(sum|count)' | tr '\n' ' '; echo
  echo "$1:$r"
  ebpf_stop "$MOUNT" >/dev/null 2>&1 || true
}
for round in 1 2; do
  export S3FS_RA_AHEAD=0 S3FS_RA_CONC=0; run old-w3 $((32*M))
  for cfg in "3 3" "4 3" "5 3" "6 3" "5 4" "6 4"; do set -- $cfg
    export S3FS_RA_AHEAD=$1 S3FS_RA_CONC=$2; run "ahead$1-conc$2" $((32*M)); done
  export S3FS_RA_AHEAD=0 S3FS_RA_CONC=0; run unbounded 0
done
