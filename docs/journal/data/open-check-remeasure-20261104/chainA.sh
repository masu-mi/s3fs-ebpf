#!/usr/bin/env bash
cd "$1" || exit 1
R=$2; mkdir -p "$R"
for rep in 1 2 3; do
 for oc in 0 1; do
  BENCH_OPEN_CHECK=$oc OUT="$R/A-oc$oc-rep$rep" STEPS=A BACKENDS="fuse-raw ebpf-nocache" scripts/bench-pcache-remeasure.sh > "$R/A-oc$oc-rep$rep.log" 2>&1
 done
done
echo DONE > "$R/DONE"
