#!/usr/bin/env bash
# test-pcache-capacity-seq.sh — AC-45 C3 regression: one file larger than
# capacity_bytes, read front to back, must not accumulate in fd->buf.
#
# While a reader waits on a chunk the whole ino used to be exempt from
# eviction (pending_exists, buf_readers), so a sequential read of a file
# bigger than the capacity kept every chunk it had read: the peak was the
# file size.  C3 pins only the chunk a reader waits on and lets eviction
# free the rest; the daemon also refuses to promote such a file to a
# full-file admit (which would populate it all before any eviction runs).
#
# Test strategy:
#   1. Seed one FILE_MIB (default 40) random file; capacity is CAPACITY (12 MiB).
#   2. Two phases on a fresh daemon each: (a) --pattern-aware-admit=false,
#      (b) the default (pattern-aware on).  No /policy/eval, sync off.
#   3. Read the file in 1 MiB steps (a short pause per step so the gauge
#      can be sampled), pipe to sha256sum, and track the peak of
#      s3fs_ebpf_fd_buf_bytes with a 0.2 s poller.
#   4. sha256 must match and the peak must stay <= CAPACITY + SLACK_MIB
#      (default 8 MiB: the chunk being read plus one populated ahead).
#
# Usage:
#   sudo ./scripts/test-pcache-capacity-seq.sh
#
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
#
# Requirements: ebpf kmod + daemon built (make all), MinIO up (make compose-up),
# docker, sha256sum, curl.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-pcache-capacity-seq}
SETTLE=${SETTLE:-6}
FILE_MIB=${FILE_MIB:-40}
CAPACITY=${CAPACITY:-$(( 12 * 1024 * 1024 ))}
SLACK_MIB=${SLACK_MIB:-8}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
TEST_NAME=${TEST_NAME:-test-pcache-capacity-seq}
S3_BUCKET=${CAP_SEQ_BUCKET:-s3fs-capacity-seq}
FNAME=capacity-seq.bin

log()  { printf '[capacity-seq] %s\n' "$*" >&2; }

TMPDIR_LOCAL=$(mktemp -d /tmp/pcache-capacity-seq-XXXXXX)
POLL_PID=""
cleanup() {
    [ -n "$POLL_PID" ] && kill "$POLL_PID" 2>/dev/null || true
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

mkdir -p "$OUT"

# ── prereqs ───────────────────────────────────────────────────────────────────
need_cmds docker sha256sum curl
need_built

get_metric() {
    local out
    out=$(curl -sf "${METRICS_URL}" 2>/dev/null) || { echo -1; return; }
    printf '%s\n' "$out" | awk -v m="$1" '
        index($0, m) == 1 && substr($0, length(m) + 1, 1) ~ /[{ ]/ { v = int($NF) }
        END { print (v == "" ? -1 : v) }'
}

# ── seed ──────────────────────────────────────────────────────────────────────
log "seeding ${FNAME} (${FILE_MIB} MiB random) into ${S3_BUCKET}"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
dd if=/dev/urandom bs=1M count="$FILE_MIB" iflag=fullblock 2>/dev/null > "$TMPDIR_LOCAL/orig.bin"
WANT_HASH=$(sha256sum < "$TMPDIR_LOCAL/orig.bin" | awk '{print $1}')
docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$FNAME" \
    < "$TMPDIR_LOCAL/orig.bin" >/dev/null
SLACK=$(( SLACK_MIB * 1024 * 1024 ))
LIMIT=$(( CAPACITY + SLACK ))

declare -A PEAK HASHOK
run_phase() {
    local phase=$1; shift
    log "phase ${phase}: daemon flags: $*"
    ebpf_mount "$MOUNT"
    ebpf_start_daemon "$OUT/daemon-${phase}.log" \
        --bucket="$S3_BUCKET" \
        --arena-metadata-only \
        --policy-bpf="builtin:policy_observe" \
        --softcap=64 \
        --cache-capacity="${CAPACITY}" \
        --metrics-addr=:9600 \
        --sample-every=100ms --sync-every=0 "$@"
    sleep "$SETTLE"

    echo 0 > "$TMPDIR_LOCAL/peak"
    ( while true; do
          v=$(get_metric s3fs_ebpf_fd_buf_bytes)
          [ "$v" -gt "$(cat "$TMPDIR_LOCAL/peak")" ] && echo "$v" > "$TMPDIR_LOCAL/peak"
          sleep 0.2
      done ) &
    POLL_PID=$!

    local got
    got=$( for i in $(seq 0 $(( FILE_MIB - 1 ))); do
               sudo dd if="$MOUNT/$FNAME" bs=1M skip="$i" count=1 2>/dev/null
               sleep 0.08
           done | sha256sum | awk '{print $1}' )
    sleep 0.5
    kill "$POLL_PID" 2>/dev/null || true; POLL_PID=""
    PEAK[$phase]=$(cat "$TMPDIR_LOCAL/peak")
    if [ "$got" = "$WANT_HASH" ]; then HASHOK[$phase]=PASS; else HASHOK[$phase]=FAIL; fi
    log "  phase ${phase}: peak=${PEAK[$phase]} B sha=${HASHOK[$phase]}"
    ebpf_stop "$MOUNT" >/dev/null 2>&1 || true
}

run_phase chunk --pattern-aware-admit=false
run_phase auto

# ── verdict ───────────────────────────────────────────────────────────────────
overall=PASS
declare -A PEAKOK
for ph in chunk auto; do
    if [ "${PEAK[$ph]}" -le "$LIMIT" ]; then PEAKOK[$ph]=PASS; else PEAKOK[$ph]=FAIL; overall=FAIL; fi
    [ "${HASHOK[$ph]}" = PASS ] || overall=FAIL
done

{
echo ""
echo "=============================================================="
echo "test-pcache-capacity-seq  $(date +%Y-%m-%d)"
echo "=============================================================="
printf "file       : %d MiB, read in 1 MiB steps, no /policy/eval\n" "$FILE_MIB"
printf "capacity   : %d MiB, limit for the peak: %d MiB\n" \
    "$(( CAPACITY / 1048576 ))" "$(( LIMIT / 1048576 ))"
for ph in chunk auto; do
    printf "%-10s : peak=%d B  peak=%s  sha256=%s\n" "$ph" "${PEAK[$ph]}" \
        "${PEAKOK[$ph]}" "${HASHOK[$ph]}"
done
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/capacity-seq-report.txt"

printf 'RESULT name=%s status=%s peak_chunk=%d peak_auto=%d limit=%d sha_chunk=%s sha_auto=%s\n' \
    "$TEST_NAME" "$overall" "${PEAK[chunk]}" "${PEAK[auto]}" "$LIMIT" \
    "${HASHOK[chunk]}" "${HASHOK[auto]}"
for ph in chunk auto; do
    [[ "${PEAKOK[$ph]}" == FAIL ]] && \
        printf 'DETAIL phase=%s peak=%d > limit=%d (a file larger than capacity accumulated in fd->buf — AC-45)\n' \
            "$ph" "${PEAK[$ph]}" "$LIMIT"
done
[[ "$overall" == FAIL ]] && exit 1 || exit 0
