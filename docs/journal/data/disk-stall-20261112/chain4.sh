#!/usr/bin/env bash
for r in 1 2 3; do for m in direct direct+local fuse-raw ebpf-nocache ebpf-pcache; do
  /tmp/phase/burst4.sh $m $r > /tmp/phase/b4/run-$m-$r.log 2>&1 || echo "FAILED $m $r" >> /tmp/phase/b4/failed.txt
done; done
echo done > /tmp/phase/b4.DONE
