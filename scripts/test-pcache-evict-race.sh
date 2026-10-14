#!/usr/bin/env bash
# test-pcache-evict-race.sh — R1 stress test for the populate/evict race.
#
# Exercises the race fixed in R1:
#   s3fs_ebpf_populate_pages vs admit_evict_ino racing on the same ino.
# Prior to the fix, a pcache entry could be inserted after fd->buf was freed,
# causing the next read to return zeros from the NULL buf path (or corrupt data
# on a subsequent re-allocation).
#
# Test strategy:
#   1. Seed multiple large files (> 8 MiB each) into MinIO.
#   2. Start the daemon with --arena-metadata-only and a tiny capacity_bytes
#      (one 4 MiB chunk) so evictions happen after every other admit.
#   3. Read all files concurrently from N_READERS background processes, each
#      reading the full file and comparing sha256 against the original.
#   4. Simultaneously hammer POST /policy/eval at short intervals to exercise
#      the policy-eval path that calls admit_evict_ino.
#   5. Run for DURATION seconds, then verify all readers exited cleanly.
#   6. Scan dmesg for lockdep warnings emitted during the run.
#
# CONFIG_PROVE_LOCKING=y is required for lockdep coverage; if missing, the
# test still verifies data integrity but lockdep lines will not appear.
#
# Usage:
#   sudo ./scripts/test-pcache-evict-race.sh
#
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
#
# Requirements: ebpf kmod + daemon built (make all), MinIO up (make compose-up),
# docker, sha256sum, curl, jq.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-pcache-evict-race}
SETTLE=${SETTLE:-8}
DURATION=${DURATION:-120}       # seconds to run concurrent stress
N_FILES=${N_FILES:-4}           # number of large files to seed
N_READERS=${N_READERS:-4}       # concurrent reader processes per file
# 9 MiB: > 2 × 4 MiB partialAdmitChunk so at least 3 POPULATE_PAGES calls
FILE_SIZE=$(( 9 * 1024 * 1024 ))
POLICY_EVAL_INTERVAL=${POLICY_EVAL_INTERVAL:-0.2}  # seconds between eval hits
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
DAEMON_METRICS_URL=${DAEMON_METRICS_URL:-http://localhost:9600}

log()  { printf '[evict-race] %s\n' "$*" >&2; }
fail() { log "FAIL: $*"; exit 1; }

TMPDIR_LOCAL=$(mktemp -d /tmp/pcache-evict-race-XXXXXX)
READER_FAIL="$TMPDIR_LOCAL/reader.fail"

cleanup() {
    # Stop background hammers and readers first
    jobs -p 2>/dev/null | xargs -r kill 2>/dev/null || true
    wait 2>/dev/null || true
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

mkdir -p "$OUT"

# ── prereqs ───────────────────────────────────────────────────────────────────
need_cmds docker sha256sum curl jq
need_built

# ── seed files into MinIO ─────────────────────────────────────────────────────
log "seeding ${N_FILES} files (${FILE_SIZE}B each) into MinIO"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1

declare -a ORIG_HASHES
for i in $(seq 1 "$N_FILES"); do
    fname="evict-race-test-${i}.bin"
    orig="$TMPDIR_LOCAL/orig-${i}.bin"
    dd if=/dev/urandom bs=4096 count=$(( (FILE_SIZE + 4095) / 4096 )) \
       2>/dev/null | head -c "$FILE_SIZE" > "$orig"
    ORIG_HASHES[$i]=$(sha256sum "$orig" | awk '{print $1}')
    docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$fname" < "$orig" >/dev/null
    log "  seeded ${fname} (sha256=${ORIG_HASHES[$i]})"
done
log "seed done"

# ── mount in pcache mode with tiny capacity ───────────────────────────────────
# capacity_bytes = one 4 MiB chunk → every second chunk triggers eviction
CHUNK_BYTES=$(( 4 * 1024 * 1024 ))
log "mounting s3fs-ebpf in pcache mode (capacity_bytes=${CHUNK_BYTES})"
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" \
    --arena-metadata-only \
    --policy-bpf="builtin:policy_observe" \
    --softcap=2 \
    --cache-capacity="${CHUNK_BYTES}" \
    --metrics-addr=:9600 \
    --sample-every=1s --sync-every=5s
log "daemon started, settling ${SETTLE}s"
sleep "$SETTLE"

# ── S3-1: snapshot eviction counter before the run ───────────────────────────
# Parse Prometheus text format: handles both 'metric value' and 'metric{...} value'.
# Daemon exports evictions as s3fs_ebpf_policy_evictions_total (vals[0] from kmod stats).
get_evict_count() {
    curl -sf "${METRICS_URL}" 2>/dev/null \
        | awk '/^s3fs_ebpf_policy_evictions_total[{ ]/{print int($NF); exit}' \
        || echo "-1"
}
EVICT_BEFORE=$(get_evict_count)
log "evictions before stress: ${EVICT_BEFORE}"

# ── S3-2: detect lockdep availability ────────────────────────────────────────
LOCKDEP_AVAIL=0
[ -r /proc/lockdep_stats ] && LOCKDEP_AVAIL=1
if [ "$LOCKDEP_AVAIL" -eq 0 ]; then
    log "INFO: /proc/lockdep_stats absent (CONFIG_PROVE_LOCKING disabled) — lockdep check will be SKIP"
fi

# ── record dmesg line count before the run ───────────────────────────────────
DMESG_BEFORE=$(dmesg 2>/dev/null | wc -l || echo 0)

# ── background: hammer POST /policy/eval ─────────────────────────────────────
hammer_policy_eval() {
    while true; do
        curl -sf -X POST "${DAEMON_METRICS_URL}/policy/eval" \
            -H 'Content-Type: application/json' \
            -d '{"file":"/mnt/s3demo/evict-race-test-1.bin"}' \
            >/dev/null 2>&1 || true
        sleep "$POLICY_EVAL_INTERVAL"
    done
}
hammer_policy_eval &
HAMMER_PID=$!
log "policy/eval hammer started (pid=${HAMMER_PID})"

# ── background: concurrent readers ────────────────────────────────────────────
reader_loop() {
    local file_idx=$1
    local fname="evict-race-test-${file_idx}.bin"
    local expected="${ORIG_HASHES[$file_idx]}"
    local deadline=$(( $(date +%s) + DURATION ))

    while [ "$(date +%s)" -lt "$deadline" ]; do
        local got
        got=$(sudo sha256sum "$MOUNT/$fname" 2>/dev/null | awk '{print $1}')
        if [ "$got" != "$expected" ]; then
            printf 'MISMATCH file=%s expected=%s got=%s\n' \
                "$fname" "$expected" "$got" > "$READER_FAIL"
            exit 1
        fi
    done
    exit 0
}

READER_PIDS=()
for i in $(seq 1 "$N_FILES"); do
    for _ in $(seq 1 "$N_READERS"); do
        reader_loop "$i" &
        READER_PIDS+=($!)
    done
done
log "${#READER_PIDS[@]} reader processes started (${N_READERS} per file × ${N_FILES} files)"

# ── wait for duration then collect ────────────────────────────────────────────
log "stress running for ${DURATION}s …"
sleep "$DURATION"

# Stop hammer
kill "$HAMMER_PID" 2>/dev/null || true
wait "$HAMMER_PID" 2>/dev/null || true

# Collect reader exit codes
READER_FAIL_COUNT=0
for pid in "${READER_PIDS[@]}"; do
    if ! wait "$pid" 2>/dev/null; then
        READER_FAIL_COUNT=$(( READER_FAIL_COUNT + 1 ))
    fi
done

# ── S3-1: verify evictions actually occurred ─────────────────────────────────
EVICT_AFTER=$(get_evict_count)
EVICT_DELTA=$(( EVICT_AFTER - EVICT_BEFORE ))
log "evictions after stress: ${EVICT_AFTER} (delta=${EVICT_DELTA})"
EVICT_PASS=FAIL
if [ "$EVICT_BEFORE" -eq -1 ] || [ "$EVICT_AFTER" -eq -1 ]; then
    EVICT_PASS="SKIP(metrics unavailable)"
elif [ "$EVICT_DELTA" -gt 0 ]; then
    EVICT_PASS=PASS
else
    log "ERROR: zero evictions — capacity pressure did not fire; test may be vacuous"
fi

# ── S3-2/S3-3: check dmesg for kernel errors and lockdep warnings ─────────────
# S3-3: extended pattern set (lockdep + KASAN + sleeping-in-atomic)
DMESG_PATTERN='possible deadlock|lock order|WARNING: (possible|suspicious)|inconsistent lock state|circular locking|BUG: sleeping function called from invalid context|BUG: KASAN'
LOCKDEP_STATUS=SKIP
if [ "$LOCKDEP_AVAIL" -eq 1 ]; then
    NEW_DMESG=$(dmesg 2>/dev/null | tail -n +"$DMESG_BEFORE")
    if echo "$NEW_DMESG" | grep -qE "$DMESG_PATTERN"; then
        LOCKDEP_STATUS=FAIL
        log "WARNING: kernel warning(s) found in dmesg — see below"
        echo "$NEW_DMESG" | grep -E "$DMESG_PATTERN" >&2
    else
        LOCKDEP_STATUS=PASS
    fi
fi

# ── overall result (computed before the report so the variable survives tee) ──
# lockdep SKIP is acceptable (not a failure); sha256 + evictions must both PASS.
overall=PASS
[ -f "$READER_FAIL" ] && overall=FAIL
[ "$READER_FAIL_COUNT" -gt 0 ] && overall=FAIL
[[ "$EVICT_PASS" == FAIL ]] && overall=FAIL
[ "$LOCKDEP_STATUS" = FAIL ] && overall=FAIL

# ── report ────────────────────────────────────────────────────────────────────
{
echo ""
echo "=============================================================="
echo "test-pcache-evict-race  $(date +%Y-%m-%d)"
echo "=============================================================="
printf "files      : %d × %d MiB\n" "$N_FILES" "$(( FILE_SIZE / 1024 / 1024 ))"
printf "readers    : %d per file (%d total)\n" "$N_READERS" "$(( N_READERS * N_FILES ))"
printf "capacity   : %d MiB  (forces eviction after each chunk)\n" "$(( CHUNK_BYTES / 1024 / 1024 ))"
printf "duration   : %ds\n" "$DURATION"
printf "evictions  : before=%s after=%s delta=%d\n" \
    "$EVICT_BEFORE" "$EVICT_AFTER" "$EVICT_DELTA"
echo ""
if [ -f "$READER_FAIL" ]; then
    printf "sha256     : FAIL (%s)\n" "$(cat "$READER_FAIL")"
elif [ "$READER_FAIL_COUNT" -gt 0 ]; then
    printf "sha256     : FAIL (%d reader(s) exited non-zero)\n" "$READER_FAIL_COUNT"
else
    echo "sha256     : PASS (all reads matched originals)"
fi
printf "evictions  : %s\n" "$EVICT_PASS"
printf "lockdep    : %s\n" "$LOCKDEP_STATUS"
if [ "$LOCKDEP_AVAIL" -eq 0 ]; then
    echo "  (rerun with CONFIG_PROVE_LOCKING=y for full lockdep coverage)"
fi
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/evict-race-report.txt"

# Exit non-zero on any hard failure
[[ "$overall" == FAIL ]] && exit 1 || exit 0
