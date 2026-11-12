#!/usr/bin/env bash
# O_DIRECT 100 MiB writes to a file on / (no page cache, no MinIO, no fs under test); per-iteration ms
OUT=/tmp/phase/b4; f=/tmp/phase/rawdisk.bin
: > $OUT/raw-$1.iters
for i in $(seq 1 ${N:-150}); do
  t=$EPOCHREALTIME
  dd if=/dev/zero of=$f bs=1M count=100 oflag=direct conv=notrunc 2>/dev/null
  echo "$t $(awk -v a=$t -v b=$EPOCHREALTIME 'BEGIN{printf "%.0f",(b-a)*1000}')" >> $OUT/raw-$1.iters
done
rm -f $f
