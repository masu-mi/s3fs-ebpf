#!/usr/bin/env bash
# bench-ab-arena-vs-pagecache.sh — AB-01/AB-02 arena vs page-cache comparison.
#
# What it measures:
#   AB-01: GUP (pin_user_pages_remote) latency per call while serving reads
#          from the arena. Reads IOC_STATS slots 3/4 (gup_calls / gup_ns)
#          from kmod; no bpftrace required.
#   AB-02: end-to-end read latency in two modes:
#          - arena path  (s3fs_arena_bypass=0, default)
#          - page-cache path (s3fs_arena_bypass=1, kmod module param at runtime)
#
# Phase 1 Gate decision:
#   If mean GUP latency > GATE_GUP_NS (500 ns) AND
#   page-cache mean latency < arena mean latency → GATE PASS → proceed to AB-04.
#   Otherwise → GATE FAIL → switch to plan-D (per-file admit cache).
#
# Prereqs: MinIO up (make compose-up), kmod + daemon built (make all).

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
BUCKET=${BUCKET:-s3fs-test}
OUT=${OUT:-/tmp/bench-ab-arena-vs-pagecache}
SETTLE=${SETTLE:-10}
N_READS=${N_READS:-200}   # reads per file per mode
SMALL_FILE=ab_small.txt   # 44 B
LARGE_FILE=ab_large.bin   # LARGE_BYTES below
LARGE_BYTES=${LARGE_BYTES:-1048576}  # 1 MiB (one arena chunk, many GUP pages)
GATE_GUP_NS=${GATE_GUP_NS:-500}     # Phase 1 Gate threshold (ns/call)
POLICIES=${POLICIES:-observe}

mkdir -p "$OUT"
log() { echo "[ab-bench] $*" >&2; }

# ── prereq ──────────────────────────────────────────────────────────────────

need_cmds docker curl python3 awk
need_built
minio_require

# ── ioctl stats helper ──────────────────────────────────────────────────────

# read_stats <dev> — emit "evictions folio_hits invalidations gup_calls gup_ns"
# by calling S3FS_IOC_STATS (5 slots) via the daemon's /dev/... path.
# We proxy through /metrics since the daemon mirrors IOC_STATS into Prometheus.
read_gup_stats() {
    local calls ns
    calls=$(curl -s "$METRICS_URL" | awk '$1=="s3fs_ebpf_gup_calls_total"{print $2}')
    ns=$(curl -s    "$METRICS_URL" | awk '$1=="s3fs_ebpf_gup_ns_total"{print $2}')
    echo "${calls:-0} ${ns:-0}"
}

# ── bucket seed ─────────────────────────────────────────────────────────────

seed_bucket() {
    log "seeding bucket: 1 small (44B) + 1 large (${LARGE_BYTES}B)"
    docker exec "$MINIO_CONTAINER" mc rm --recursive --force \
        "local/$BUCKET" >/dev/null 2>&1 || true
    docker exec "$MINIO_CONTAINER" mc mb --ignore-existing \
        "local/$BUCKET" >/dev/null

    printf 'ab_bench small file line1\nab_bench small file line2\n' \
        | docker exec -i "$MINIO_CONTAINER" \
            mc pipe "local/$BUCKET/$SMALL_FILE" >/dev/null

    dd if=/dev/urandom bs=1024 count=$((LARGE_BYTES / 1024)) 2>/dev/null \
        | docker exec -i "$MINIO_CONTAINER" \
            mc pipe "local/$BUCKET/$LARGE_FILE" >/dev/null
    log "bucket seeded"
}

# ── lifecycle ────────────────────────────────────────────────────────────────

cleanup() { ebpf_stop "$MOUNT"; }
trap cleanup EXIT

start_stack() {
    cleanup
    ebpf_mount "$MOUNT"
    ebpf_start_daemon "$OUT/daemon.log" \
        --policy-bpf="builtin:policy_observe" \
        --metrics-addr=:9600 \
        --sample-every=2s --sync-every=3s \
        --softcap=8 --cache-capacity=0
    log "daemon ready, settling ${SETTLE}s"
    sleep "$SETTLE"
}

warm_arena() {
    log "warming arena (reading both files once to trigger admit)"
    sudo cat "$MOUNT/$SMALL_FILE" >/dev/null
    sudo cat "$MOUNT/$LARGE_FILE" >/dev/null
    curl -s -X POST "${METRICS_URL%/metrics}/policy/eval" >/dev/null
    sleep 3
}

# ── timed read loop ──────────────────────────────────────────────────────────

# timed_reads <file> <n> → prints mean_ms_per_read to stdout
timed_reads() {
    local path=$1 n=$2
    local i start_ns end_ns
    start_ns=$(date +%s%N)
    for i in $(seq 1 "$n"); do
        sudo cat "$path" >/dev/null 2>&1
    done
    end_ns=$(date +%s%N)
    awk "BEGIN{printf \"%.3f\", ($end_ns - $start_ns) / 1000000 / $n}"
}

# ── arena mode (bypass=0) ───────────────────────────────────────────────────

run_arena_mode() {
    log "=== arena mode (s3fs_arena_bypass=0) ==="
    echo 0 | sudo tee /sys/module/s3fs_kfunc/parameters/arena_bypass >/dev/null

    local b_calls b_ns
    read -r b_calls b_ns <<< "$(read_gup_stats)"
    log "baseline gup_calls=$b_calls gup_ns=$b_ns"

    log "timing small file ($N_READS reads)"
    SMALL_ARENA_MS=$(timed_reads "$MOUNT/$SMALL_FILE" "$N_READS")
    log "timing large file ($N_READS reads)"
    LARGE_ARENA_MS=$(timed_reads "$MOUNT/$LARGE_FILE" "$N_READS")

    local f_calls f_ns
    read -r f_calls f_ns <<< "$(read_gup_stats)"
    local d_calls d_ns
    d_calls=$(( f_calls - b_calls ))
    d_ns=$(( f_ns - b_ns ))

    if [ "$d_calls" -gt 0 ]; then
        GUP_MEAN_NS=$(awk "BEGIN{printf \"%.1f\", $d_ns / $d_calls}")
    else
        GUP_MEAN_NS="N/A (no GUP calls — metrics not yet exported)"
    fi

    log "arena: small=${SMALL_ARENA_MS}ms/read  large=${LARGE_ARENA_MS}ms/read  gup_calls=${d_calls}  gup_mean_ns=${GUP_MEAN_NS}"
}

# ── page-cache mode (bypass=1) ──────────────────────────────────────────────

run_pagecache_mode() {
    log "=== page-cache mode (s3fs_arena_bypass=1) ==="
    echo 1 | sudo tee /sys/module/s3fs_kfunc/parameters/arena_bypass >/dev/null
    # Drop page cache so the first read re-populates it, subsequent reads hit.
    echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null
    # One warm-up pass to populate page cache.
    sudo cat "$MOUNT/$SMALL_FILE" >/dev/null
    sudo cat "$MOUNT/$LARGE_FILE" >/dev/null

    log "timing small file ($N_READS reads, page-cache path)"
    SMALL_PC_MS=$(timed_reads "$MOUNT/$SMALL_FILE" "$N_READS")
    log "timing large file ($N_READS reads, page-cache path)"
    LARGE_PC_MS=$(timed_reads "$MOUNT/$LARGE_FILE" "$N_READS")

    # Restore bypass=0 after measurement.
    echo 0 | sudo tee /sys/module/s3fs_kfunc/parameters/arena_bypass >/dev/null
    log "page-cache: small=${SMALL_PC_MS}ms/read  large=${LARGE_PC_MS}ms/read"
}

# ── Phase 1 Gate ─────────────────────────────────────────────────────────────

gate_decision() {
    local gup_ns_val small_faster large_faster gate_pass=0

    # AB-01 gate: gup mean > GATE_GUP_NS
    if [ "$GUP_MEAN_NS" = "N/A (no GUP calls — metrics not yet exported)" ]; then
        log "WARNING: gup_ns metric not yet mirrored by daemon — AB-01 inconclusive"
        log "         Tip: update daemon to mirror s3fs_ebpf_gup_calls_total / gup_ns_total"
        gup_ns_val=0
    else
        gup_ns_val=${GUP_MEAN_NS%.*}
    fi

    # AB-02 gate: page-cache path < arena path for large file (main bottleneck)
    small_faster=$(awk "BEGIN{print ($SMALL_PC_MS < $SMALL_ARENA_MS) ? 1 : 0}")
    large_faster=$(awk "BEGIN{print ($LARGE_PC_MS < $LARGE_ARENA_MS) ? 1 : 0}")

    echo ""
    echo "=================================================================="
    echo "AB-01/AB-02 Phase 1 Gate Report  ($(date +%Y-%m-%d))"
    echo "=================================================================="
    printf "GUP mean latency : %s ns/call  (gate threshold: >%s ns)\n" \
        "$GUP_MEAN_NS" "$GATE_GUP_NS"
    printf "Small file (44B) : arena=%-8s ms/read  page-cache=%-8s ms/read  pc_faster=%s\n" \
        "$SMALL_ARENA_MS" "$SMALL_PC_MS" "$([ "$small_faster" -eq 1 ] && echo YES || echo NO)"
    printf "Large file (%dB): arena=%-8s ms/read  page-cache=%-8s ms/read  pc_faster=%s\n" \
        "$LARGE_BYTES" "$LARGE_ARENA_MS" "$LARGE_PC_MS" \
        "$([ "$large_faster" -eq 1 ] && echo YES || echo NO)"
    echo ""

    if [ "$gup_ns_val" -gt "$GATE_GUP_NS" ] && [ "$large_faster" -eq 1 ]; then
        gate_pass=1
    fi

    if [ "$gate_pass" -eq 1 ]; then
        echo "GATE: PASS"
        echo "  GUP > ${GATE_GUP_NS}ns AND page-cache faster for large file."
        echo "  → Proceed to AB-04 (arena metadata-only refactor)."
    else
        echo "GATE: FAIL"
        [ "$gup_ns_val" -le "$GATE_GUP_NS" ] && \
            echo "  GUP ≤ ${GATE_GUP_NS}ns — GUP is not the bottleneck."
        [ "$large_faster" -eq 0 ] && \
            echo "  Arena is NOT slower than page-cache for large file."
        echo "  → Switch to plan-D (per-file admit cache in file->private_data)."
    fi
    echo "=================================================================="

    # Write summary line.
    printf 'AB-RESULT  date=%s  gup_calls=%s  gup_mean_ns=%s  small_arena=%s  small_pc=%s  large_arena=%s  large_pc=%s  gate=%s\n' \
        "$(date +%Y-%m-%d)" \
        "${GUP_CALLS_DELTA:-N/A}" "$GUP_MEAN_NS" \
        "$SMALL_ARENA_MS" "$SMALL_PC_MS" \
        "$LARGE_ARENA_MS" "$LARGE_PC_MS" \
        "$([ "$gate_pass" -eq 1 ] && echo PASS || echo FAIL)" \
        | tee -a "$OUT/summary.txt"
}

# ── main ─────────────────────────────────────────────────────────────────────

: > "$OUT/summary.txt"
seed_bucket
start_stack
warm_arena
run_arena_mode
run_pagecache_mode
gate_decision

echo ""
echo "Full results: $OUT/summary.txt"
echo "Daemon log:   $OUT/daemon.log"
