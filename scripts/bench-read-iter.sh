#!/usr/bin/env bash
# scripts/bench-read-iter.sh — R-01 hook overhead bench.
#
# Measures the per-read syscall latency overhead of the
# fentry/generic_file_read_iter BPF hook by comparing:
#   Phase 1 — daemon running WITH  the hook (default)
#   Phase 2 — daemon running WITHOUT the hook (--no-read-iter-hook)
#
# The hook is an observer only (no return-value modification). Expected
# overhead is sub-microsecond (hash lookup + two atomic increments +
# optional ringbuf reserve). This script quantifies that claim.
#
# Prerequisites:
#   - MinIO up (make compose-up); s3fs-test/$FILE seeded
#   - kmod + daemon built (make build)

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

OUT_DIR=${OUT_DIR:-/tmp/bench}
MP_EBPF=/mnt/s3demo
FILE=${FILE:-hello.txt}
# Default 10 000 iters: gives stable percentiles in ~10s per phase.
# Override with MICRO_ITERS=50000 for tighter confidence intervals.
MICRO_ITERS=${MICRO_ITERS:-10000}

mkdir -p "$OUT_DIR"
log() { echo "[bench-read-iter] $*" >&2; }
BIN="$S3FS_BIN"

check_prereq() {
	need_built
	minio_require "$FILE"
}

# start_ebpf [extra-flags...]
# Loads kmod, mounts, starts daemon, waits for prewarm.
start_ebpf() {
	log "starting daemon flags='$*'"
	ebpf_mount "$MP_EBPF"
	ebpf_start_daemon "$OUT_DIR/fastpath-ri.log" \
		--sample-every=300s --metrics-addr="" "$@"
	log "prewarm done (log: $OUT_DIR/fastpath-ri.log)"
}

stop_ebpf() {
	ebpf_stop "$MP_EBPF"
}
trap stop_ebpf EXIT

check_prereq

# ── Phase 1: WITH fentry/generic_file_read_iter ──────────────────────────
log "=== phase 1: WITH hook (default) ==="
start_ebpf
# Warmup to stabilise TLB / pipeline before timing.
"$BIN" dev microbench --op=read --path="$MP_EBPF/$FILE" \
	--iters=500 --warmup=200 --label='warmup' >/dev/null 2>&1 || true
WITH_OUT=$("$BIN" dev microbench --op=read --path="$MP_EBPF/$FILE" \
	--iters="$MICRO_ITERS" --warmup=1000 --label='with-hook ' 2>&1)
echo "$WITH_OUT" >&2
stop_ebpf

# ── Phase 2: WITHOUT hook ─────────────────────────────────────────────────
log "=== phase 2: WITHOUT hook (--no-read-iter-hook) ==="
start_ebpf "--no-read-iter-hook"
"$BIN" dev microbench --op=read --path="$MP_EBPF/$FILE" \
	--iters=500 --warmup=200 --label='warmup' >/dev/null 2>&1 || true
WITHOUT_OUT=$("$BIN" dev microbench --op=read --path="$MP_EBPF/$FILE" \
	--iters="$MICRO_ITERS" --warmup=1000 --label='no-hook  ' 2>&1)
echo "$WITHOUT_OUT" >&2
stop_ebpf
trap - EXIT

# ── Report ────────────────────────────────────────────────────────────────
{
	echo '## R-01 fentry/generic_file_read_iter hook overhead'
	echo
	echo "file: \`$FILE\`  iters: $MICRO_ITERS  warmup: 1000"
	echo
	echo '```'
	echo "$WITH_OUT"
	echo
	echo "$WITHOUT_OUT"
	echo '```'
	echo
	echo '### Interpretation'
	echo 'The hook is an observer-only fentry: one LRU_HASH lookup, two'
	echo 'atomic increments, and a conditional ringbuf reserve. Expected'
	echo 'overhead < 1 μs per read call; any delta at p50 is typically'
	echo 'measurement noise. Significant p99 delta would indicate BPF JIT'
	echo 'contention or LRU eviction pressure on s3fs_read_pattern_map.'
} >"$OUT_DIR/read-iter-overhead.md"

cat "$OUT_DIR/read-iter-overhead.md" >&2
log "done → $OUT_DIR/read-iter-overhead.md"
