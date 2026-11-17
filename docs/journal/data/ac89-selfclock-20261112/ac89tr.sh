#!/usr/bin/env bash
set -euo pipefail
TOPDIR=$(pwd); source "$TOPDIR/scripts/lib/bench.sh"
MOUNT=/mnt/s3demo; S3_BUCKET=s3fs-ac89; OUT=${OUT:-/tmp/ac89t}; mkdir -p "$OUT"
need_built
trap 'ebpf_stop "$MOUNT" >/dev/null 2>&1 || true' EXIT
M=$((1024*1024))
run() {
  ebpf_mount "$MOUNT"; ebpf_open_check 0
  ebpf_start_daemon "$OUT/d-$1.log" --bucket="$S3_BUCKET" --arena-metadata-only --policy-bpf=builtin:policy_observe_ops --softcap=64 --cache-capacity="$2" --metrics-addr=:9600 --sample-every=60s --sync-every=0
  sleep 4
  for i in 1 2 3 4; do
    echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null
    sleep 1
    t0=$(date +%s%N); sudo cat "$MOUNT/big.bin" >/dev/null; echo "$1 run$i $(( ($(date +%s%N)-t0)/1000000 )) ms"
  done
  ebpf_stop "$MOUNT" >/dev/null 2>&1 || true
}
run unbounded 0
run cap32 $((32*M))
