#!/usr/bin/env bash
N=/home/masumi/dev/src/github.com/masu-mi/s3fs-ebpf/.claude/worktrees/bridge-cse_01Azpk8YZH5oNQ7CHFH2kHX1
B=/tmp/scratch/base
run() { (cd $2 && OUT=/tmp/r/B-$1 STEPS=B RUNS_W=10 BACKENDS="fuse-cache ebpf-pcache" scripts/bench-pcache-remeasure.sh) > /tmp/r/B-$1.log 2>&1; }
run r5-new $N; run r6-base $B; run r7-new $N; run r8-base $B; run r9-new $N; run r10-base $B
echo done > /tmp/r/B2.DONE
