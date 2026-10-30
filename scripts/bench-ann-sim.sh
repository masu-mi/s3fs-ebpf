#!/usr/bin/env bash
# P2 matrix of docs/design/ann-workload-measurement-plan.md: synthetic IVF /
# HNSW / mixed traces replayed chunk-granular through every policy.
# No kmod, MinIO or root needed. Results: $OUT/*.md (default /tmp/bench-ann-sim).
set -euo pipefail
TOP=$(cd "$(dirname "$0")/.." && pwd)
OUT=${OUT:-/tmp/bench-ann-sim}
BIN=$OUT/s3fs-ebpf
mkdir -p "$OUT"
(cd "$TOP/daemon" && go build -o "$BIN" ./cmd/s3fs-ebpf)
SEEDS=${SEEDS:-5}

run() { # name args...
	local name=$1
	shift
	echo "==> $name" >&2
	"$BIN" dev ann-sim --seeds="$SEEDS" "$@" >"$OUT/$name.md"
}

# IVF: dimension x cluster popularity (Q1, Q2)
for d in 128 768; do
	for a in 0 0.7 1.1; do
		run "ivf-d$d-a$a" --workload=ivf --d=$d --alpha=$a --queries=20000
	done
done
# IVF: nprobe, skewed cluster sizes
for np in 1 32; do
	run "ivf-d128-a1.1-nprobe$np" --workload=ivf --nprobe=$np --queries=20000
done
run "ivf-d128-a1.1-sizeskew" --workload=ivf --size-skew --queries=20000
# HNSW: layout x popularity (Q1, Q3)
for l in bfs insertion; do
	for a in 0 1.1; do
		run "hnsw-$l-a$a" --workload=hnsw --layout=$l --alpha=$a --queries=1500 \
			--policies=lru,sieve,s3fifo,wtinylfu,opt,noevict
	done
done
# Mixed: hot IVF + periodic one-shot scan (Q5)
run "mixed-d128-a1.1" --workload=mixed --alpha=1.1 --queries=20000
# Readahead on/off (Q4); d=768 so a list can span chunks
run "ivf-d768-a1.1-ra0" --workload=ivf --d=768 --alpha=1.1 --queries=20000 --policies=lru,s3fifo,wtinylfu,noevict
run "ivf-d768-a1.1-ra1" --workload=ivf --d=768 --alpha=1.1 --queries=20000 --readahead=1 --policies=lru,s3fifo,wtinylfu,noevict
# Chunk size (Q6); OPT is O(cap) per eviction, so left out
for c in 262144 1048576 4194304; do
	run "ivf-d128-a1.1-chunk$c" --workload=ivf --alpha=1.1 --queries=20000 --chunk=$c --policies=lru,sieve,s3fifo,wtinylfu,noevict
done
echo "done: $OUT" >&2
