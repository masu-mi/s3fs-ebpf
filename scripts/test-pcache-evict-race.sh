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
N_READERS=${N_READERS:-8}       # T3: 8 readers stress the folio-lock / evict deadlock window
# 9 MiB: > 2 × 4 MiB partialAdmitChunk so at least 3 POPULATE_PAGES calls
FILE_SIZE=$(( 9 * 1024 * 1024 ))
POLICY_EVAL_INTERVAL=${POLICY_EVAL_INTERVAL:-0.2}  # seconds between eval hits
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
DAEMON_METRICS_URL=${DAEMON_METRICS_URL:-http://localhost:9600}
TEST_NAME=${TEST_NAME:-test-pcache-evict-race}

log()  { printf '[evict-race] %s\n' "$*" >&2; }
fail() { log "FAIL: $*"; exit 1; }

TMPDIR_LOCAL=$(mktemp -d /tmp/pcache-evict-race-XXXXXX)
READER_FAIL="$TMPDIR_LOCAL/reader.fail"   # sha256 content mismatch (data corruption)
READER_ERROR="$TMPDIR_LOCAL/reader.error" # sha256sum exited non-zero (-EIO)

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
# T4: retry_exhausted counter — non-zero means capacity-pressure -EIO (not corruption).
get_retry_exhausted_count() {
    curl -sf "${METRICS_URL}" 2>/dev/null \
        | awk '/^s3fs_ebpf_upcall_retry_exhausted_total[{ ]/{print int($NF); exit}' \
        || echo "-1"
}
# U11: timeout counter — non-zero confirms goroutine-pool back-pressure is exercised.
get_upcall_timeout_count() {
    curl -sf "${METRICS_URL}" 2>/dev/null \
        | awk '/^s3fs_ebpf_upcall_timeout_total[{ ]/{print int($NF); exit}' \
        || echo "-1"
}
# D-diag: gen=0 failure reason counters.
get_gen0_fail_counts() {
    curl -sf "${METRICS_URL}" 2>/dev/null | awk '
        /^s3fs_ebpf_gen0_fail_buf_null_total[{ ]/ { buf_null=int($NF) }
        /^s3fs_ebpf_gen0_fail_bitmap_total[{ ]/   { bitmap=int($NF) }
        /^s3fs_ebpf_gen0_fail_other_total[{ ]/    { other=int($NF) }
        END { printf "%d %d %d", buf_null+0, bitmap+0, other+0 }' \
        || echo "-1 -1 -1"
}
EVICT_BEFORE=$(get_evict_count)
RETRY_BEFORE=$(get_retry_exhausted_count)
TIMEOUT_BEFORE=$(get_upcall_timeout_count)
GEN0_BEFORE=$(get_gen0_fail_counts)
log "evictions before stress: ${EVICT_BEFORE}"
log "retry_exhausted before stress: ${RETRY_BEFORE}"
log "upcall_timeout before stress: ${TIMEOUT_BEFORE}"

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
        local sha_out sha_rc got
        # Use `|| sha_rc=$?` so set -e does not fire on sha256sum -EIO (exit 1);
        # sha_rc is set to 0 on success or the non-zero exit code on failure.
        sha_out="" sha_rc=0
        sha_out=$(sudo sha256sum "$MOUNT/$fname" 2>/dev/null) || sha_rc=$?
        if [ "$sha_rc" -ne 0 ]; then
            # sha256sum itself failed — file read returned -EIO.
            # T4: capacity-pressure retry exhaustion causes this, not data corruption.
            # Exit 2 so the collector distinguishes it from a MISMATCH (exit 1).
            printf 'READ_ERROR file=%s\n' "$fname" > "$READER_ERROR"
            exit 2
        fi
        got=$(printf '%s' "$sha_out" | awk '{print $1}')
        if [ "$got" != "$expected" ]; then
            # Find first differing byte (decimal offset) for root-cause analysis.
            # cmp exits 1 when files differ; || true prevents set -e from
            # killing the subshell before we write READER_FAIL.
            local first_diff=""
            first_diff=$(sudo cmp -l "$TMPDIR_LOCAL/orig-${file_idx}.bin" \
                "$MOUNT/$fname" 2>/dev/null | head -3 | \
                awk '{printf "off=%d exp=0%s got=0%s; ", $1, $2, $3}' || true)
            printf 'MISMATCH file=%s expected=%s got=%s first_diff=[%s]\n' \
                "$fname" "$expected" "$got" "$first_diff" > "$READER_FAIL"
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

# Collect reader exit codes — distinguish MISMATCH (1) from READ_ERROR (2).
READER_FAIL_COUNT=0   # sha256 wrong (data corruption)
READER_ERROR_COUNT=0  # sha256sum failed (-EIO, capacity pressure)
for pid in "${READER_PIDS[@]}"; do
    rc=0
    wait "$pid" 2>/dev/null || rc=$?
    case $rc in
        0) ;;
        2) READER_ERROR_COUNT=$(( READER_ERROR_COUNT + 1 )) ;;
        *) READER_FAIL_COUNT=$(( READER_FAIL_COUNT + 1 )) ;;
    esac
done

# ── S3-1: verify evictions actually occurred ─────────────────────────────────
EVICT_AFTER=$(get_evict_count)
RETRY_AFTER=$(get_retry_exhausted_count)
TIMEOUT_AFTER=$(get_upcall_timeout_count)
GEN0_AFTER=$(get_gen0_fail_counts)
EVICT_DELTA=$(( EVICT_AFTER - EVICT_BEFORE ))
RETRY_DELTA=$(( RETRY_AFTER - RETRY_BEFORE ))
TIMEOUT_DELTA=$(( TIMEOUT_AFTER - TIMEOUT_BEFORE ))
read -r GEN0_NULL_BEFORE GEN0_BMAP_BEFORE GEN0_OTHER_BEFORE <<< "$GEN0_BEFORE"
read -r GEN0_NULL_AFTER  GEN0_BMAP_AFTER  GEN0_OTHER_AFTER  <<< "$GEN0_AFTER"
GEN0_NULL_DELTA=$(( GEN0_NULL_AFTER - GEN0_NULL_BEFORE ))
GEN0_BMAP_DELTA=$(( GEN0_BMAP_AFTER - GEN0_BMAP_BEFORE ))
GEN0_OTHER_DELTA=$(( GEN0_OTHER_AFTER - GEN0_OTHER_BEFORE ))
log "evictions after stress: ${EVICT_AFTER} (delta=${EVICT_DELTA})"
log "retry_exhausted after stress: ${RETRY_AFTER} (delta=${RETRY_DELTA})"
log "upcall_timeout after stress: ${TIMEOUT_AFTER} (delta=${TIMEOUT_DELTA})"
log "gen0_fail_buf_null after stress: ${GEN0_NULL_AFTER} (delta=${GEN0_NULL_DELTA})"
log "gen0_fail_bitmap after stress: ${GEN0_BMAP_AFTER} (delta=${GEN0_BMAP_DELTA})"
log "gen0_fail_other after stress: ${GEN0_OTHER_AFTER} (delta=${GEN0_OTHER_DELTA})"
EVICT_PASS=FAIL
if [ "$EVICT_BEFORE" -eq -1 ] || [ "$EVICT_AFTER" -eq -1 ]; then
    EVICT_PASS="SKIP(metrics unavailable)"
elif [ "$EVICT_DELTA" -gt 0 ]; then
    EVICT_PASS=PASS
else
    log "ERROR: zero evictions — capacity pressure did not fire; test may be vacuous"
fi

# ── S3-2/S3-3: check dmesg for kernel errors and lockdep warnings ─────────────
# S3-3/T3: extended pattern set (lockdep + KASAN + sleeping-in-atomic + hung task).
# "blocked for more than" catches the T3 deadlock: a reader holding a folio lock
# waiting for a daemon upcall while the daemon calls invalidate_inode_pages2 (old
# behaviour) — both sides block indefinitely.  With invalidate_mapping_pages (T3
# fix) the ioctl path never waits on the folio lock so this cannot trigger.
DMESG_PATTERN='possible deadlock|lock order|WARNING: (possible|suspicious)|inconsistent lock state|circular locking|BUG: sleeping function called from invalid context|BUG: KASAN|blocked for more than'
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
# lockdep SKIP is acceptable (not a failure); sha256 corruption + evictions must PASS.
# READ_ERROR (exit 2) = -EIO: U8+U10 protect fd->buf so the gen=0 fallback should
# never fail; any -EIO is a regression and must FAIL.
# MISMATCH (exit 1 / READER_FAIL_COUNT) = data corruption → FAIL.
overall=PASS
[ -f "$READER_FAIL" ] && overall=FAIL
[ "$READER_FAIL_COUNT" -gt 0 ] && overall=FAIL
[ "$READER_ERROR_COUNT" -gt 0 ] && overall=FAIL
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
printf "retry_exh  : before=%s after=%s delta=%d  (T4: capacity-pressure -EIO)\n" \
    "$RETRY_BEFORE" "$RETRY_AFTER" "$RETRY_DELTA"
printf "upcall_tmo : before=%s after=%s delta=%d  (U11: goroutine-pool timeout retries)\n" \
    "$TIMEOUT_BEFORE" "$TIMEOUT_AFTER" "$TIMEOUT_DELTA"
printf "gen0_fail  : buf_null=%d bitmap=%d other=%d  (D-diag: gen=0 fallback breakdown)\n" \
    "$GEN0_NULL_DELTA" "$GEN0_BMAP_DELTA" "$GEN0_OTHER_DELTA"
echo ""
if [ -f "$READER_FAIL" ]; then
    printf "sha256     : FAIL (%s)\n" "$(cat "$READER_FAIL")"
elif [ "$READER_FAIL_COUNT" -gt 0 ]; then
    printf "sha256     : FAIL (%d reader(s) data-corruption)\n" "$READER_FAIL_COUNT"
else
    echo "sha256     : PASS (all reads matched originals)"
fi
if [ "$READER_ERROR_COUNT" -gt 0 ]; then
    if [ -f "$READER_ERROR" ]; then
        printf "read_err   : FAIL %d reader(s) got -EIO (%s) — U8+U10 should prevent this\n" \
            "$READER_ERROR_COUNT" "$(cat "$READER_ERROR")"
    else
        printf "read_err   : FAIL %d reader(s) got -EIO — U8+U10 should prevent this\n" \
            "$READER_ERROR_COUNT"
    fi
else
    echo "read_err   : PASS (no -EIO observed)"
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

printf 'RESULT name=%s status=%s sha256_fail=%d read_err=%d evict_delta=%d retry_exh_delta=%d timeout_delta=%d lockdep=%s\n' \
    "$TEST_NAME" "$overall" "$READER_FAIL_COUNT" "$READER_ERROR_COUNT" \
    "$EVICT_DELTA" "$RETRY_DELTA" "$TIMEOUT_DELTA" "$LOCKDEP_STATUS"
if [[ "$overall" == FAIL ]]; then
    [ "$READER_FAIL_COUNT" -gt 0 ] && \
        printf 'DETAIL sha256_fail=%d (data corruption — non-zero wrong content)\n' "$READER_FAIL_COUNT"
    [ "$READER_ERROR_COUNT" -gt 0 ] && \
        printf 'DETAIL read_err=%d (-EIO: gen=0 fallback failed — U8+U10 race still present)\n' "$READER_ERROR_COUNT"
    [[ "$EVICT_PASS" == FAIL ]] && printf 'DETAIL evict_delta=0 (no evictions — test vacuous, check capacity_bytes)\n'
    [ "$LOCKDEP_STATUS" = FAIL ] && printf 'DETAIL lockdep=FAIL (kernel WARN/BUG in dmesg during run)\n'
    [ "$RETRY_DELTA" -gt 0 ] && \
        printf 'DETAIL retry_exh_delta=%d (upcall retries exhausted under capacity pressure)\n' "$RETRY_DELTA"
fi

# Exit non-zero on any hard failure
[[ "$overall" == FAIL ]] && exit 1 || exit 0
