#!/usr/bin/env bash
# H (5 policies) x {no delay, silo egress netem 10-15ms} x seeds
cd "$1" || exit 1
R=$2; mkdir -p "$R"
PID=$(docker inspect -f '{{.State.Pid}}' s3fs-ebpf-minio)
tcq() { sudo nsenter -t "$PID" -n tc "$@"; }
trap 'tcq qdisc del dev eth0 root 2>/dev/null' EXIT
for seed in 42 43 44; do
 for cond in nodelay delay; do
  tcq qdisc del dev eth0 root 2>/dev/null
  [ $cond = delay ] && tcq qdisc add dev eth0 root netem delay 12.5ms 2.5ms
  tcq qdisc show dev eth0 > "$R/qdisc-$cond-$seed.txt"
  N_FILES=30 N_ACCESS=2000 ZIPF_ALPHA=0.7 SOFTCAP=8 SEED=$seed PCACHE_DROP=1 DAEMON_EXTRA_FLAGS=--arena-metadata-only \
   POLICIES="observe_ops lru_pick_ops sieve_pick_ops s3fifo_pick_ops wtinylfu_pick_ops" OUT="$R/$cond-seed$seed" \
   scripts/bench-hitrate.sh > "$R/$cond-seed$seed.log" 2>&1
 done
done
echo DONE > "$R/DONE"
