#!/usr/bin/env bash
# bench-hitrate-mixed.sh — D-01 multi-range acceptance bench.
#
# What it measures: hit-rate with a MIXED working set of small files (44B)
# and large files (LARGE_FILE_BYTES, default 5MB — above the 4MB partial-admit
# threshold). This validates X-01~X-05 correctness under the D-01 partial-admit
# path where a single ino may occupy multiple arena ranges.
#
# Working set layout (Zipf-skewed, hottest first):
#   index 0          .. N_LARGE-1    → hitrate_large_%03d.bin (LARGE_FILE_BYTES each)
#   index N_LARGE    .. N_TOTAL-1    → hitrate_small_%03d.txt (44B each)
#
# Large files are placed at the hottest Zipf ranks so the policy must decide
# whether to keep an expensive (capacity-heavy) hot large file or multiple
# cheaper small files. Correct policies keep the large file; buggy eviction
# (e.g. pre-Y-02 idx tiebreak) would evict it despite high access frequency.
#
# Capacity: CAPACITY_BYTES defaults to 1 large file + SOFTCAP_SMALL small files,
# forcing eviction once the full working set is admitted.
#
# Prereqs: MinIO up (make compose-up), kmod + daemon built (make all).

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
BUCKET=${BUCKET:-s3fs-test}

N_LARGE=${N_LARGE:-3}               # large files (index 0..N_LARGE-1, hottest Zipf ranks)
N_SMALL=${N_SMALL:-10}              # small files (index N_LARGE..N_TOTAL-1)
LARGE_FILE_BYTES=${LARGE_FILE_BYTES:-5242880}  # 5 MiB — above 4 MiB partial-admit threshold
# One definition of the small files' content: the size is measured from it
# (a hand-written constant drifted from the real size once, issue #22).
small_content() { printf 'hitrate mixed small %03d line1\nhitrate mixed small %03d line2\n' "$1" "$1"; }
SMALL_FILE_BYTES=$(small_content 0 | wc -c)
N_TOTAL=$((N_LARGE + N_SMALL))

N_ACCESS=${N_ACCESS:-500}            # shorter than single-type bench; large-file GETs are slow
ZIPF_ALPHA=${ZIPF_ALPHA:-0.7}
SEED=${SEED:-42}

# Keep capacity for 1 large file + SOFTCAP_SMALL small files.
# Adjust SOFTCAP_SMALL to vary eviction pressure on small files.
SOFTCAP_SMALL=${SOFTCAP_SMALL:-4}
CAPACITY_BYTES=${CAPACITY_BYTES:-$((LARGE_FILE_BYTES + SOFTCAP_SMALL * SMALL_FILE_BYTES))}

# Slot-based softcap: N_LARGE + SOFTCAP_SMALL (slots, not bytes).
# Passed to kmod --softcap for legacy mode compatibility; byte-capacity is the
# real constraint when CAPACITY_BYTES > 0.
SOFTCAP=$((N_LARGE + SOFTCAP_SMALL))

SETTLE=${SETTLE:-15}
SYNC_EVERY=${SYNC_EVERY:-3s}
POST=${POST:-8}
INTERLEAVE=${INTERLEAVE:-25}
POLICIES=${POLICIES:-observe lru_ops sieve_ops s3fifo_ops wtinylfu_ops pressure_ops time_decay_ops}
OUT=${OUT:-/tmp/bench-hitrate-mixed}

mkdir -p "$OUT"
log() { echo "[hitrate-mixed] $*" >&2; }

# --- prereq -----------------------------------------------------------------

need_cmds docker curl python3
need_built
minio_require

# --- bucket seed ------------------------------------------------------------

# Naming convention: large files sort before small files alphabetically
# (hitrate_large_* < hitrate_small_*), so ino assignment from S3 listing
# is: ino[N_LARGE+i] = 100+N_LARGE+i for small files,
#     ino[i]         = 100+i         for large files.
# This means Zipf rank i → ino 100+i exactly. No alphabetical-sort bias.
seed_bucket() {
    log "clearing bucket $BUCKET and seeding $N_LARGE large + $N_SMALL small files"
    docker exec "$MINIO_CONTAINER" mc rm --recursive --force \
        "local/$BUCKET" >/dev/null 2>&1 || true
    docker exec "$MINIO_CONTAINER" mc mb --ignore-existing \
        "local/$BUCKET" >/dev/null

    local i
    # Large files: hitrate_large_%03d.bin, LARGE_FILE_BYTES bytes of pattern content.
    for i in $(seq 0 $((N_LARGE-1))); do
        # Generate deterministic content (pattern-based so S3 serves the same
        # bytes each time; SHA256 is reproducible). Use dd + /dev/urandom for
        # distinct content per file to prevent S3/minio dedup.
        dd if=/dev/urandom bs=1024 count=$((LARGE_FILE_BYTES / 1024)) 2>/dev/null \
            | docker exec -i "$MINIO_CONTAINER" \
                mc pipe "local/$BUCKET/hitrate_large_$(printf '%03d' "$i").bin" >/dev/null
        log "seeded large file $i (${LARGE_FILE_BYTES}B)"
    done

    # Small files: hitrate_small_%03d.txt, 44B fixed content.
    for i in $(seq 0 $((N_SMALL-1))); do
        small_content "$i" \
            | docker exec -i "$MINIO_CONTAINER" \
                mc pipe "local/$BUCKET/hitrate_small_$(printf '%03d' "$i").txt" >/dev/null
    done
    local got
    got=$(docker exec "$MINIO_CONTAINER" mc stat "local/$BUCKET/hitrate_small_000.txt" 2>/dev/null |
        awk '/^Size/ { print $3 }')
    if [ "$got" != "$SMALL_FILE_BYTES" ]; then
        echo "bench-hitrate-mixed: small file is ${got:-?} B but SMALL_FILE_BYTES=$SMALL_FILE_BYTES" >&2
        exit 1
    fi
    log "seeded $N_SMALL small files (${SMALL_FILE_BYTES}B each)"
}

# --- workload ---------------------------------------------------------------

# Emits N_ACCESS integers in [0, N_TOTAL) Zipf(alpha)-sampled.
# Index 0..N_LARGE-1 → large files (hottest), N_LARGE..N_TOTAL-1 → small files.
gen_workload() {
    python3 - "$N_TOTAL" "$N_ACCESS" "$ZIPF_ALPHA" "$SEED" <<'PY'
import random, sys
n, k, alpha, seed = int(sys.argv[1]), int(sys.argv[2]), float(sys.argv[3]), int(sys.argv[4])
random.seed(seed)
w = [1.0/((i+1)**alpha) for i in range(n)]
tot = sum(w)
cum = []
c = 0.0
for x in w:
    c += x/tot
    cum.append(c)
for _ in range(k):
    r = random.random()
    lo, hi = 0, n-1
    while lo < hi:
        mid = (lo+hi)//2
        if cum[mid] < r: lo = mid+1
        else: hi = mid
    print(lo)
PY
}

# Map Zipf index to mount path.
idx_to_path() {
    local idx=$1
    if [ "$idx" -lt "$N_LARGE" ]; then
        printf '%s/hitrate_large_%03d.bin' "$MOUNT" "$idx"
    else
        printf '%s/hitrate_small_%03d.txt' "$MOUNT" "$((idx - N_LARGE))"
    fi
}

# --- lifecycle --------------------------------------------------------------

cleanup() {
    ebpf_stop "$MOUNT"
}
trap cleanup EXIT

start_stack() {
    local pol=$1
    cleanup
    ebpf_mount "$MOUNT"
    ebpf_start_daemon "$OUT/daemon-${pol}.log" \
        --policy-bpf="builtin:policy_${pol}" \
        --metrics-addr=:9600 \
        --sample-every=2s --sync-every="$SYNC_EVERY" \
        --softcap="$SOFTCAP" --cache-capacity="$CAPACITY_BYTES"
    log "$pol: prewarm done, forcing 4 convergence eval rounds"
    local i
    for i in 1 2 3 4; do
        curl -s -X POST "${METRICS_URL%/metrics}/policy/eval" >/dev/null
        sleep 3
    done
    log "$pol: convergence done, settling ${SETTLE}s"
    sleep "$SETTLE"
}

read_counter() {
    local name=$1
    curl -s "$METRICS_URL" | awk -v n="$name" '$1==n { print $2 }'
}

# --- one policy run ---------------------------------------------------------

run_one() {
    local pol=$1
    log "==== policy: $pol ===="
    start_stack "$pol"

    local b_miss b_reload b_evict b_admits b_folio_hits b_read_iter_hits b_read_iter_pos0_hits
    b_miss=$(read_counter s3fs_ebpf_read_miss_upcalls_total)
    b_reload=$(read_counter s3fs_ebpf_read_miss_errors_total)
    b_evict=$(read_counter s3fs_ebpf_policy_evictions_total)
    b_admits=$(read_counter s3fs_ebpf_admits_current)
    b_folio_hits=$(read_counter s3fs_ebpf_folio_hits_total)
    b_read_iter_hits=$(read_counter s3fs_ebpf_read_iter_hits_total)
    b_read_iter_pos0_hits=$(read_counter s3fs_ebpf_read_iter_pos0_hits_total)
    b_read_iter_pos0_hits=${b_read_iter_pos0_hits:-0}
    log "$pol: baseline miss=$b_miss evict=$b_evict admits=$b_admits folio=$b_folio_hits"

    local adm_sum=0 adm_n=0 adm_max=0 adm
    sample_admits() {
        adm=$(read_counter s3fs_ebpf_admits_current)
        adm=${adm:-0}
        adm=${adm%.*}
        adm_sum=$((adm_sum + adm))
        adm_n=$((adm_n + 1))
        [ "$adm" -gt "$adm_max" ] && adm_max=$adm
        return 0
    }

    log "$pol: running $N_ACCESS accesses (INTERLEAVE=$INTERLEAVE)"
    local start_ns end_ns i=0 errs=0 large_hits=0 small_hits=0 large_miss=0 small_miss=0
    start_ns=$(date +%s%N)
    while read -r idx; do
        local path
        path=$(idx_to_path "$idx")
        if sudo cat "$path" >/dev/null 2>&1; then
            if [ "$idx" -lt "$N_LARGE" ]; then large_hits=$((large_hits+1))
            else small_hits=$((small_hits+1)); fi
        else
            errs=$((errs+1))
        fi
        i=$((i+1))
        if [ "$INTERLEAVE" -gt 0 ] && [ $((i % INTERLEAVE)) -eq 0 ]; then
            sample_admits
            curl -s -X POST "${METRICS_URL%/metrics}/policy/eval" >/dev/null
        fi
    done < "$OUT/workload.txt"
    end_ns=$(date +%s%N)
    sample_admits

    log "$pol: workload done, settling ${POST}s"
    sleep "$POST"

    local f_miss f_reload f_evict f_folio_hits f_read_iter_hits f_read_iter_pos0_hits
    f_miss=$(read_counter s3fs_ebpf_read_miss_upcalls_total)
    # shellcheck disable=SC2034
    f_reload=$(read_counter s3fs_ebpf_read_miss_errors_total)
    f_evict=$(read_counter s3fs_ebpf_policy_evictions_total)
    f_folio_hits=$(read_counter s3fs_ebpf_folio_hits_total)
    f_read_iter_hits=$(read_counter s3fs_ebpf_read_iter_hits_total)
    f_read_iter_pos0_hits=$(read_counter s3fs_ebpf_read_iter_pos0_hits_total)
    f_read_iter_pos0_hits=${f_read_iter_pos0_hits:-0}

    local d_miss d_evict d_folio_hits d_read_iter_hits d_read_iter_pos0_hits d_page_cache_hits
    local total rate_arena hit_rate iter_mult ms_per
    d_miss=$((f_miss - b_miss))
    d_evict=$((f_evict - b_evict))
    d_folio_hits=$((f_folio_hits - b_folio_hits))
    d_read_iter_hits=$((f_read_iter_hits - b_read_iter_hits))
    d_read_iter_pos0_hits=$((f_read_iter_pos0_hits - b_read_iter_pos0_hits))
    if [ "$d_read_iter_pos0_hits" -gt 0 ]; then
        d_page_cache_hits=$(( d_read_iter_pos0_hits - d_folio_hits - d_miss ))
    else
        d_page_cache_hits=$(( d_read_iter_hits - d_folio_hits - d_miss ))
    fi
    [ "$d_page_cache_hits" -lt 0 ] && d_page_cache_hits=0
    total=$N_ACCESS
    if [ "$total" -gt 0 ]; then
        rate_arena=$(awk "BEGIN{printf \"%.2f\", 100*$d_folio_hits/$total}")
        hit_rate=$(awk "BEGIN{printf \"%.2f\", 100*(1 - $d_miss/$total)}")
        iter_mult=$(awk "BEGIN{d=$d_read_iter_pos0_hits; n=$d_read_iter_hits; printf \"%.2f\", (d>0) ? n/d : 0}")
    else
        rate_arena="-"; hit_rate="-"; iter_mult="-"
    fi
    ms_per=$(awk "BEGIN{printf \"%.2f\", ($end_ns-$start_ns)/1000000/$N_ACCESS}")

    local adm_mean
    adm_mean=$(awk "BEGIN{printf \"%.1f\", $adm_sum/($adm_n>0?$adm_n:1)}")

    # Extra D-01 column: folio_hits per folio_hit / N_ACCESS is inflated for
    # large files (many pages per access). Report raw folio count separately.
    printf 'RESULT policy=%-14s  folio=%6d  pc=%5d  miss=%5d  err=%3d  rate_arena=%6s%%  hit_rate=%6s%%  evictions=%5d  adm_mean=%5s  adm_max=%3d  iter_mult=%s  ms/access=%s\n' \
        "$pol" "$d_folio_hits" "$d_page_cache_hits" "$d_miss" "$errs" \
        "$rate_arena" "$hit_rate" "$d_evict" "$adm_mean" "$adm_max" \
        "$iter_mult" "$ms_per" | tee -a "$OUT/summary.txt"
}

# --- reference bounds -------------------------------------------------------

reference_bounds() {
    {
        echo
        echo "REFERENCE (dev trace-replay on $OUT/trace.jsonl, softcap=$SOFTCAP, cold start)"
        echo "  capacity_bytes=$CAPACITY_BYTES  large_file_bytes=$LARGE_FILE_BYTES"
        echo "  note: trace-replay uses slot-counts not byte-capacity; opt is an upper bound only"
        "$S3FS_BIN" dev trace-replay --trace="$OUT/trace.jsonl" \
            --softcap="$SOFTCAP" --policies=lru,sieve,opt,noevict
    } | tee -a "$OUT/summary.txt"
}

# --- main -------------------------------------------------------------------

: > "$OUT/summary.txt"
seed_bucket

gen_workload > "$OUT/workload.txt"

# Trace: ino = 100 + index (large files: 100..100+N_LARGE-1,
#                            small files: 100+N_LARGE..100+N_TOTAL-1)
awk '{ printf "{\"ts\":%d,\"ino\":%d,\"op\":\"read\"}\n", (NR-1)*1000000, 100+$1 }' \
    "$OUT/workload.txt" > "$OUT/trace.jsonl"

echo "=================================================================="
echo "hit-rate MIXED bench — ${N_LARGE} large (${LARGE_FILE_BYTES}B) + ${N_SMALL} small (${SMALL_FILE_BYTES}B)"
echo "  Zipf α=$ZIPF_ALPHA  $N_ACCESS accesses  cap=$CAPACITY_BYTES  softcap=$SOFTCAP  seed=$SEED"
echo "=================================================================="
for pol in $POLICIES; do
    run_one "$pol"
done
reference_bounds

echo
echo "==== summary ($OUT/summary.txt) ===="
cat "$OUT/summary.txt"
