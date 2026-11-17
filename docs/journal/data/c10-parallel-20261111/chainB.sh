#!/usr/bin/env bash
N=/home/masumi/dev/src/github.com/masu-mi/s3fs-ebpf/.claude/worktrees/bridge-cse_01Azpk8YZH5oNQ7CHFH2kHX1
B=/tmp/scratch/base
run() { # name tree
  (cd $2 && OUT=/tmp/r/B-$1 STEPS=B RUNS_W=10 BACKENDS="fuse-cache ebpf-pcachedrop ebpf-pcache" scripts/bench-pcache-remeasure.sh) > /tmp/r/B-$1.log 2>&1
}
run r1-new $N; run r2-base $B; run r3-new $N; run r4-base $B
echo done > /tmp/r/B.DONE
