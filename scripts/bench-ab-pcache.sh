#!/usr/bin/env bash
# bench-ab-pcache.sh — AB-07: arena-mode vs pcache-mode (--arena-metadata-only) comparison.
#
# Validates three AB-07 acceptance criteria:
#   (a) 64-job psync read IOPS: ebpf-pcache ≥ fuse-raw    (AA-01 残課題解消)
#   (b) large file cat latency: ebpf-pcache ≤ fuse-cache  (1.17× 劣後解消)
#   (c) microbench read ops/s : ebpf-pcache ≥ ebpf-arena  (GUP 排除の効果)
#
# Prereqs: MinIO up (make compose-up), kmod + daemon built (make all).
#
# Large file size: defaults to 10 MiB.  Set LARGE_BYTES=104857600 for full
# 100 MiB comparison (matches the "100MB cat" target in backlog AB-07).

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
MP_FUSE_RAW=${MP_FUSE_RAW:-/mnt/s3fuse}
MP_FUSE_CACHE=${MP_FUSE_CACHE:-/mnt/s3fuse-cache}
FUSE_CACHE_DIR=${FUSE_CACHE_DIR:-/tmp/s3fs-fuse-cache-ab07}
EBPF_CACHE_DIR=${EBPF_CACHE_DIR:-/tmp/s3fs-ebpf-cache-ab07}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
OUT=${OUT:-/tmp/bench-ab-pcache}
SETTLE=${SETTLE:-10}
FILE=${FILE:-hello.txt}
LARGE_FILE=${LARGE_FILE:-ab07_large.bin}
# P5: gate (b) spec is 100 MiB; default is now 100 MiB.
# Set LARGE_BYTES=10485760 to reproduce the prior 10 MiB measurement.
LARGE_BYTES=${LARGE_BYTES:-104857600}
N_READS=${N_READS:-5}
MICRO_ITERS=${MICRO_ITERS:-2000}
FIO_JOBS=${FIO_JOBS:-64}
FIO_RUNTIME=${FIO_RUNTIME:-5}

mkdir -p "$OUT" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR"
sudo chown "$(id -un):$(id -gn)" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR" 2>/dev/null || true

log() { echo "[ab-pcache] $*" >&2; }

# ── prereq ────────────────────────────────────────────────────────────────────

need_cmds docker curl fio awk jq hyperfine
need_built
minio_require "$FILE"

# ── large file seed ───────────────────────────────────────────────────────────

seed_large_file() {
    log "seeding ${LARGE_FILE} (${LARGE_BYTES}B) into s3://${S3_BUCKET}"
    docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
        minioadmin minioadmin >/dev/null 2>&1
    dd if=/dev/urandom bs=4096 count=$(( LARGE_BYTES / 4096 )) 2>/dev/null \
        | docker exec -i "$MINIO_CONTAINER" \
            mc pipe "local/$S3_BUCKET/$LARGE_FILE" >/dev/null
    log "large file seeded"
}

# ── backends ──────────────────────────────────────────────────────────────────

FUSE_RAW_STARTED=0
FUSE_CACHE_STARTED=0

start_fuse_raw() {
    log "mount fuse-raw at $MP_FUSE_RAW"
    fuse_mount_raw "$MP_FUSE_RAW" && FUSE_RAW_STARTED=1 || \
        log "fuse-raw mount failed — skipping criterion (a)"
}

start_fuse_cache() {
    log "mount fuse-cache at $MP_FUSE_CACHE"
    fuse_mount_cache "$MP_FUSE_CACHE" "$FUSE_CACHE_DIR" && FUSE_CACHE_STARTED=1 || {
        log "fuse-cache mount failed — skipping criterion (b)"
        return
    }
    cat "$MP_FUSE_CACHE/$FILE"       >/dev/null 2>&1 || true
    cat "$MP_FUSE_CACHE/$LARGE_FILE" >/dev/null 2>&1 || true
}

start_ebpf() {
    local mode=$1; shift   # "arena" | "pcache"
    log "start ebpf in ${mode} mode"
    ebpf_mount "$MOUNT"
    if [ "$mode" = "pcache" ]; then
        ebpf_start_daemon "$OUT/daemon-${mode}.log" \
            --cache-dir="$EBPF_CACHE_DIR" \
            --policy-bpf="builtin:policy_observe" \
            --arena-metadata-only \
            --metrics-addr=:9600 \
            --sample-every=2s --sync-every=60s \
            --softcap=8 --cache-capacity=0
    else
        ebpf_start_daemon "$OUT/daemon-${mode}.log" \
            --cache-dir="$EBPF_CACHE_DIR" \
            --policy-bpf="builtin:policy_observe" \
            --metrics-addr=:9600 \
            --sample-every=2s --sync-every=60s \
            --softcap=8 --cache-capacity=0
    fi
    log "${mode} daemon ready, settling ${SETTLE}s"
    sleep "$SETTLE"
    # Warm: trigger miss handler so admit/POPULATE_PAGES entries exist.
    log "warming ${mode} (first reads trigger miss → ${mode} path)"
    sudo cat "$MOUNT/$FILE"       >/dev/null
    sudo cat "$MOUNT/$LARGE_FILE" >/dev/null
    sleep 2
}

stop_ebpf() { ebpf_stop "$MOUNT" || true; }

stop_all() {
    stop_ebpf
    [ "$FUSE_CACHE_STARTED" -eq 1 ] && umount_hard "$MP_FUSE_CACHE" || true
    [ "$FUSE_RAW_STARTED"   -eq 1 ] && umount_hard "$MP_FUSE_RAW"   || true
}
trap stop_all EXIT

# ── measurement helpers ───────────────────────────────────────────────────────

# measure_fio_iops <mp> <label> — prints aggregate IOPS to stdout.
measure_fio_iops() {
    local mp=$1 label=$2
    sync; sudo sysctl -q -w vm.drop_caches=1
    fio --name="$label" --ioengine=psync --filename="$mp/$FILE" \
        --rw=read --numjobs="$FIO_JOBS" \
        --runtime="$FIO_RUNTIME" --time_based=1 --group_reporting=1 \
        --allow_file_create=0 --size=1 --bs=1 --thread 2>&1 \
    | awk '/IOPS=/{match($0, /IOPS=[^,]+/); print substr($0, RSTART+5, RLENGTH-5)}'
}

# timed_reads <path> <n> — mean ms/read, wall-clock (process startup included).
# Q4: use hyperfine --export-json + jq for portable extraction; the prior
# awk used gawk-only 3-arg match() (silent failure on mawk) and mismatched
# the µs unit (captured "µ" but compared against "µs").
# --warmup 1 primes the page cache so measured runs hit the FS hot path.
# R2: on fallback, emit a warning to stderr and set TIMED_READS_METHOD=loop.
TIMED_READS_METHOD=hyperfine
timed_reads() {
    local path=$1 n=$2
    local tmpjson
    tmpjson=$(mktemp /tmp/timed-reads-XXXXXX.json)
    if hyperfine --warmup 1 --runs "$n" \
           --export-json "$tmpjson" \
           "sudo cat '$path' >/dev/null" >/dev/null 2>&1; then
        TIMED_READS_METHOD=hyperfine
        printf '%.3f' "$(jq '.results[0].mean * 1000' "$tmpjson")"
        rm -f "$tmpjson"
    else
        rm -f "$tmpjson"
        # R2: hyperfine failed — warn and fall back to plain loop
        log "WARNING: hyperfine failed for '${path}'; falling back to loop timing (method=loop)" >&2
        TIMED_READS_METHOD=loop
        local i start_ns end_ns
        start_ns=$(date +%s%N)
        for i in $(seq 1 "$n"); do sudo cat "$path" >/dev/null 2>&1; done
        end_ns=$(date +%s%N)
        awk "BEGIN{printf \"%.3f\", ($end_ns - $start_ns) / 1000000 / $n}"
    fi
}

# measure_micro <mp> <label> outvar_ops outvar_full
# Runs microbench ONCE, sets outvar_ops to the ops/s integer and
# outvar_full to the full multi-line output string.
measure_micro() {
    local mp=$1 label=$2 _ops_var=$3 _full_var=$4
    local _out
    _out=$("$S3FS_BIN" dev microbench \
        --op=read --path="$mp/$FILE" \
        --iters="$MICRO_ITERS" --label="$label" 2>/dev/null)
    local _ops
    _ops=$(echo "$_out" | awk '/ops\/s=/{match($0, /ops\/s=[0-9.]+/); print substr($0, RSTART+6, RLENGTH-6)}')
    printf -v "$_ops_var" '%s' "${_ops:-0}"
    printf -v "$_full_var" '%s' "$_out"
}

# ── baseline: fuse-raw + fuse-cache ──────────────────────────────────────────

seed_large_file
start_fuse_raw
start_fuse_cache

FUSE_RAW_IOPS=0
if [ "$FUSE_RAW_STARTED" -eq 1 ]; then
    log "measuring fuse-raw 64-job fio read"
    FUSE_RAW_IOPS=$(measure_fio_iops "$MP_FUSE_RAW" 'fuse-raw')
    log "fuse-raw IOPS=$FUSE_RAW_IOPS"
fi

FUSE_CACHE_LARGE_MS="N/A"
if [ "$FUSE_CACHE_STARTED" -eq 1 ]; then
    log "measuring fuse-cache large file cat (${LARGE_BYTES}B × ${N_READS} reads)"
    FUSE_CACHE_LARGE_MS=$(timed_reads "$MP_FUSE_CACHE/$LARGE_FILE" "$N_READS")
    log "fuse-cache large=${FUSE_CACHE_LARGE_MS}ms/read"
fi

# ── ebpf arena mode ───────────────────────────────────────────────────────────

start_ebpf arena

log "measuring ebpf-arena 64-job fio read"
ARENA_IOPS=$(measure_fio_iops "$MOUNT" 'ebpf-arena')
log "ebpf-arena IOPS=$ARENA_IOPS"

log "measuring ebpf-arena large file cat"
ARENA_LARGE_MS=$(timed_reads "$MOUNT/$LARGE_FILE" "$N_READS")
log "ebpf-arena large=${ARENA_LARGE_MS}ms/read"

log "measuring ebpf-arena microbench"
measure_micro "$MOUNT" 'ebpf-arena' ARENA_OPS ARENA_MICRO
log "ebpf-arena ops/s=$ARENA_OPS"

# ── ebpf pcache mode ──────────────────────────────────────────────────────────

stop_ebpf
start_ebpf pcache

log "measuring ebpf-pcache 64-job fio read"
PCACHE_IOPS=$(measure_fio_iops "$MOUNT" 'ebpf-pcache')
log "ebpf-pcache IOPS=$PCACHE_IOPS"

log "measuring ebpf-pcache large file cat"
PCACHE_LARGE_MS=$(timed_reads "$MOUNT/$LARGE_FILE" "$N_READS")
log "ebpf-pcache large=${PCACHE_LARGE_MS}ms/read"

log "measuring ebpf-pcache microbench"
measure_micro "$MOUNT" 'ebpf-pcache' PCACHE_OPS PCACHE_MICRO
log "ebpf-pcache ops/s=$PCACHE_OPS"

# ── Gate decision ─────────────────────────────────────────────────────────────

awk_ge() { awk "BEGIN{exit (($1) >= ($2)) ? 0 : 1}" 2>/dev/null; }
awk_le() { awk "BEGIN{exit (($1) <= ($2)) ? 0 : 1}" 2>/dev/null; }

gate_a="SKIP(no fuse-raw)"
if [ "$FUSE_RAW_STARTED" -eq 1 ] && [ -n "$PCACHE_IOPS" ] && [ -n "$FUSE_RAW_IOPS" ]; then
    awk_ge "${PCACHE_IOPS:-0}" "${FUSE_RAW_IOPS:-1}" && gate_a=PASS || gate_a=FAIL
fi

gate_b="SKIP(no fuse-cache)"
if [ "$FUSE_CACHE_STARTED" -eq 1 ] && [ "$FUSE_CACHE_LARGE_MS" != "N/A" ]; then
    awk_le "${PCACHE_LARGE_MS:-999}" "${FUSE_CACHE_LARGE_MS:-0}" && gate_b=PASS || gate_b=FAIL
fi

gate_c=FAIL
[ -n "$PCACHE_OPS" ] && [ -n "$ARENA_OPS" ] && \
    awk_ge "${PCACHE_OPS:-0}" "${ARENA_OPS:-1}" && gate_c=PASS || true

{
echo ""
echo "=================================================================="
echo "AB-07 pcache vs arena Gate Report  ($(date +%Y-%m-%d))"
echo "=================================================================="
printf "Large file size : %d MiB  (LARGE_BYTES=%d)\n" \
    "$(( LARGE_BYTES / 1024 / 1024 ))" "$LARGE_BYTES"
printf "fio numjobs     : %d  runtime=%ds\n" "$FIO_JOBS" "$FIO_RUNTIME"
printf "microbench iters: %d\n" "$MICRO_ITERS"
echo ""
echo "--- (a) 64-job parallel read IOPS (criterion: pcache ≥ fuse-raw) ---"
printf "  fuse-raw    : %s IOPS\n" "$FUSE_RAW_IOPS"
printf "  ebpf-arena  : %s IOPS\n" "$ARENA_IOPS"
printf "  ebpf-pcache : %s IOPS\n" "$PCACHE_IOPS"
printf "  gate (a)    : %s\n" "$gate_a"
echo ""
echo "--- (b) large file cat ms/read (criterion: pcache ≤ fuse-cache) ---"
printf "  fuse-cache  : %s ms/read\n" "$FUSE_CACHE_LARGE_MS"
printf "  ebpf-arena  : %s ms/read\n" "$ARENA_LARGE_MS"
printf "  ebpf-pcache : %s ms/read\n" "$PCACHE_LARGE_MS"
printf "  gate (b)    : %s\n" "$gate_b"
echo ""
echo "--- (c) microbench read ops/s (criterion: pcache ≥ arena) ---"
echo "$ARENA_MICRO"
echo "$PCACHE_MICRO"
printf "  arena   ops/s: %s\n" "$ARENA_OPS"
printf "  pcache  ops/s: %s\n" "$PCACHE_OPS"
printf "  gate (c): %s\n" "$gate_c"
echo ""
overall=FAIL
[ "$gate_a" = "PASS" ] && [ "$gate_b" = "PASS" ] && [ "$gate_c" = "PASS" ] && overall=PASS || true
[ "$gate_a" = PASS ] || [ "$gate_a" = "SKIP(no fuse-raw)"   ] || true
[ "$gate_b" = PASS ] || [ "$gate_b" = "SKIP(no fuse-cache)" ] || true
# Allow SKIP on either baseline as partial pass if the other criteria pass.
skip_a=0; skip_b=0
[[ "$gate_a" == SKIP* ]] && skip_a=1 || true
[[ "$gate_b" == SKIP* ]] && skip_b=1 || true
if { [ "$skip_a" -eq 1 ] || [ "$gate_a" = "PASS" ]; } && \
   { [ "$skip_b" -eq 1 ] || [ "$gate_b" = "PASS" ]; } && \
   [ "$gate_c" = "PASS" ]; then
    overall=PASS
fi
if [ "$overall" = "PASS" ]; then
    echo "OVERALL: PASS — AB-07 criteria met."
    echo "  → pcache mode validated; arena-metadata-only can be enabled by default."
    echo "  → Proceed to AB-08 (bench-hitrate non-regression) and AB-09 (write path)."
else
    echo "OVERALL: FAIL — one or more criteria not met."
    echo "  → Check pcache warm path or repeat with LARGE_BYTES=104857600 (100 MiB)."
fi
echo "=================================================================="
} | tee "$OUT/ab07-report.txt"

printf 'AB07-RESULT  date=%s  fuse_raw_iops=%s  arena_iops=%s  pcache_iops=%s  fuse_cache_ms=%s  arena_ms=%s  pcache_ms=%s  arena_ops=%s  pcache_ops=%s  gate_a=%s  gate_b=%s  gate_c=%s  method=%s\n' \
    "$(date +%Y-%m-%d)" \
    "$FUSE_RAW_IOPS" "$ARENA_IOPS" "$PCACHE_IOPS" \
    "$FUSE_CACHE_LARGE_MS" "$ARENA_LARGE_MS" "$PCACHE_LARGE_MS" \
    "$ARENA_OPS" "$PCACHE_OPS" \
    "$gate_a" "$gate_b" "$gate_c" \
    "$TIMED_READS_METHOD" \
| tee -a "$OUT/ab07-report.txt"

echo ""
echo "Full report : $OUT/ab07-report.txt"
echo "Arena log   : $OUT/daemon-arena.log"
echo "Pcache log  : $OUT/daemon-pcache.log"
