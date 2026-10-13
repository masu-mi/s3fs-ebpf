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
DAEMON_HTTP_URL=${DAEMON_HTTP_URL:-http://localhost:9601}

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
    --http-addr=:9601 \
    --sample-every=1s --sync-every=5s
log "daemon started, settling ${SETTLE}s"
sleep "$SETTLE"

# ── record dmesg timestamp before the run ─────────────────────────────────────
DMESG_BEFORE=$(dmesg --time-format=reltime 2>/dev/null | wc -l || dmesg | wc -l)

# ── background: hammer POST /policy/eval ─────────────────────────────────────
hammer_policy_eval() {
    while true; do
        curl -sf -X POST "${DAEMON_HTTP_URL}/policy/eval" \
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

# ── check lockdep ─────────────────────────────────────────────────────────────
LOCKDEP_WARNINGS=0
if dmesg --time-format=reltime 2>/dev/null | tail -n +"$DMESG_BEFORE" \
        | grep -qE 'possible deadlock|lock order|WARNING: (possible|suspicious)'; then
    LOCKDEP_WARNINGS=1
    log "WARNING: lockdep warning(s) found in dmesg — see below"
    dmesg --time-format=reltime 2>/dev/null | tail -n +"$DMESG_BEFORE" \
        | grep -E 'possible deadlock|lock order|WARNING: (possible|suspicious)' >&2
fi

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
echo ""
if [ -f "$READER_FAIL" ]; then
    printf "sha256     : FAIL (%s)\n" "$(cat "$READER_FAIL")"
elif [ "$READER_FAIL_COUNT" -gt 0 ]; then
    printf "sha256     : FAIL (%d reader(s) exited non-zero)\n" "$READER_FAIL_COUNT"
else
    echo "sha256     : PASS (all reads matched originals)"
fi
if [ "$LOCKDEP_WARNINGS" -eq 0 ]; then
    echo "lockdep    : PASS (no warnings in dmesg)"
else
    echo "lockdep    : FAIL (see dmesg above)"
fi
echo ""
if [ -f "$READER_FAIL" ] || [ "$READER_FAIL_COUNT" -gt 0 ] || [ "$LOCKDEP_WARNINGS" -ne 0 ]; then
    echo "OVERALL: FAIL"
else
    echo "OVERALL: PASS"
fi
echo "=============================================================="
} | tee "$OUT/evict-race-report.txt"

if [ -f "$READER_FAIL" ] || [ "$READER_FAIL_COUNT" -gt 0 ] || [ "$LOCKDEP_WARNINGS" -ne 0 ]; then
    exit 1
fi
