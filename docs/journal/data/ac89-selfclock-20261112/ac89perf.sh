#!/usr/bin/env bash
set -euo pipefail
TOPDIR=$(pwd); source "$TOPDIR/scripts/lib/bench.sh"
MOUNT=/mnt/s3demo; S3_BUCKET=s3fs-ac89; OUT=${OUT:-/tmp/ac89p}; mkdir -p "$OUT"
need_built
P=$(perf_bin)
trap 'ebpf_stop "$MOUNT" >/dev/null 2>&1 || true' EXIT
M=$((1024*1024))
run() { # label cap
  ebpf_mount "$MOUNT"; ebpf_open_check 0
  ebpf_start_daemon "$OUT/d-$1.log" --bucket="$S3_BUCKET" --arena-metadata-only --policy-bpf=builtin:policy_observe_ops --softcap=64 --cache-capacity="$2" --metrics-addr=:9600 --sample-every=60s --sync-every=0
  sleep 4
  ( for i in $(seq 1 30); do echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null; t0=$(date +%s%N); sudo cat "$MOUNT/big.bin" >/dev/null; echo $(( ($(date +%s%N)-t0)/1000000 )); done ) > "$OUT/t-$1.txt" &
  LP=$!
  sleep 1
  sudo "$P" record -a -g -F 2000 -o "$OUT/perf-$1.data" -- sleep 5 >/dev/null 2>&1
  wait $LP
  echo "$1 run ms: $(sort -n "$OUT/t-$1.txt" | awk '{a[NR]=$1}END{print a[int(NR/2)+1]}')"
  ebpf_stop "$MOUNT" >/dev/null 2>&1 || true
}
run unbounded 0
run cap32 $((32*M))
