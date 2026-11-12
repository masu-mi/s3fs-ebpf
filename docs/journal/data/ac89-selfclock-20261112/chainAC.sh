#!/usr/bin/env bash
N=/home/masumi/dev/src/github.com/masu-mi/s3fs-ebpf/.claude/worktrees/bridge-cse_01Azpk8YZH5oNQ7CHFH2kHX1
B=/tmp/scratch/base
for r in 1 2 3; do
  for t in new base; do d=$N; [ $t = base ] && d=$B
    (cd $d && OUT=/tmp/ac89/$t-$r /tmp/scratch/ac89.sh 2>&1 | grep -v s3fs_ebpf_read | sed "s/^/$t r$r /") 
  done
done > /tmp/ac89-chain2.log 2>&1
echo done > /tmp/ac89-chain2.DONE
