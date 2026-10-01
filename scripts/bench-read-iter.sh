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
# shellcheck source=lib/umount.sh
source "$TOPDIR/scripts/lib/umount.sh"

OUT_DIR=${OUT_DIR:-/tmp/bench}
MP_EBPF=/mnt/s3demo
FILE=${FILE:-hello.txt}
# Default 10 000 iters: gives stable percentiles in ~10s per phase.
# Override with MICRO_ITERS=50000 for tighter confidence intervals.
MICRO_ITERS=${MICRO_ITERS:-10000}

mkdir -p "$OUT_DIR"
log() { echo "[bench-read-iter] $*" >&2; }
BIN="$TOPDIR/bin/s3fs-ebpf"

check_prereq() {
	docker inspect s3fs-ebpf-minio >/dev/null 2>&1 \
		|| { echo "s3fs-ebpf-minio container missing — run 'make compose-up'"; exit 1; }
	docker exec s3fs-ebpf-minio mc alias set local http://localhost:9000 \
		minioadmin minioadmin >/dev/null 2>&1
	docker exec s3fs-ebpf-minio mc stat "local/s3fs-test/$FILE" >/dev/null 2>&1 \
		|| { echo "s3://s3fs-test/$FILE missing — run 'make compose-up'"; exit 1; }
}

# start_ebpf [extra-flags]
# Loads kmod, mounts, starts daemon, waits for prewarm.
start_ebpf() {
	local extra="${1:-}"
	log "starting daemon flags='$extra'"
	lsmod | grep -q '^s3fs_kfunc' || sudo insmod "$TOPDIR/kmod/s3fs_kfunc.ko"
	mountpoint -q "$MP_EBPF" \
		|| { sudo mkdir -p "$MP_EBPF"; sudo mount -t s3fs_ebpf none "$MP_EBPF"; }
	# Kill any stale daemon. Character-class trick avoids pkill matching itself.
	sudo pkill -9 -f 's3fs-ebpf fast[p]ath' 2>/dev/null || true
	sleep 0.5
	LD_LIBRARY_PATH=$HOME/.local/lib sudo -E \
		"$TOPDIR/bin/s3fs-ebpf" fastpath --s3 --sample-every=300s \
		--metrics-addr="" $extra \
		>"$OUT_DIR/fastpath-ri.log" 2>&1 &
	for _ in $(seq 1 60); do
		grep -q '\[admit\] ' "$OUT_DIR/fastpath-ri.log" && break
		sleep 0.2
	done
	sleep 0.3
	log "prewarm done (log: $OUT_DIR/fastpath-ri.log)"
}

stop_ebpf() {
	sudo pkill -INT -f 's3fs-ebpf fast[p]ath' 2>/dev/null || true
	sleep 1
	umount_hard "$MP_EBPF"
	sudo rmmod s3fs_kfunc 2>/dev/null || true
}
trap stop_ebpf EXIT

check_prereq

# ── Phase 1: WITH fentry/generic_file_read_iter ──────────────────────────
log "=== phase 1: WITH hook (default) ==="
start_ebpf ""
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
