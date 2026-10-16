#!/usr/bin/env bash
# test-pattern-aware-admit.sh — R-01 / AC-47 regression: the access pattern
# the read_iter fentry records still drives the full-file promotion.
#
# AC-47 moved the per-inode pattern from a hash map into per-CPU,
# direct-mapped slots (merged by the daemon).  The promotion decision
# (SeqRatio >= 0.70 -> full-file GET instead of 4 MiB chunk GETs) must not
# change:
#   seq    : one `cat` of a 10 MiB object -> a single upcall, promoted
#   random : two 4 KiB reads far apart are NOT promoted (chunk admits)
#   par    : 4 concurrent `cat`s -> still promoted (sequentiality is judged
#            per CPU, so readers no longer break each other's runs) and every
#            reader gets the right bytes
#   off    : --pattern-aware-admit=false never promotes
# Each case runs on a fresh daemon with a policy that does not prewarm the
# object, so every read starts as a miss.
#
# Usage:
#   sudo ./scripts/test-pattern-aware-admit.sh
#
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
#
# Requirements: ebpf kmod + daemon built (make all), MinIO up (make compose-up),
# docker, sha256sum.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-pattern-aware-admit}
SETTLE=${SETTLE:-5}
TEST_NAME=${TEST_NAME:-test-pattern-aware-admit}
S3_BUCKET=${PATTERN_BUCKET:-s3fs-pattern-aware}
FNAME=pattern.bin
POLICY="$TOPDIR/policies/bench-no-large-prewarm.cel"

log()  { printf '[pattern-aware] %s\n' "$*" >&2; }

TMPDIR_LOCAL=$(mktemp -d /tmp/pattern-aware-XXXXXX)
cleanup() {
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

mkdir -p "$OUT"

need_cmds docker sha256sum
need_built
[ -f "$POLICY" ] || { echo "policy missing: $POLICY" >&2; exit 2; }

log "seeding ${FNAME} (10 MiB random) into ${S3_BUCKET}"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
dd if=/dev/urandom bs=1M count=10 iflag=fullblock 2>/dev/null > "$TMPDIR_LOCAL/orig.bin"
WANT=$(sha256sum < "$TMPDIR_LOCAL/orig.bin" | awk '{print $1}')
docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$FNAME" \
    < "$TMPDIR_LOCAL/orig.bin" >/dev/null

declare -A UPCALLS PROMOTES SHA
start() {
    ebpf_mount "$MOUNT"
    DAEMON_READY_PATTERN='\[sync\] initial:' ebpf_start_daemon "$OUT/daemon-$1.log" \
        --bucket="$S3_BUCKET" --arena-metadata-only --policy="$POLICY" \
        --metrics-addr="" --sample-every=300s --sync-every=0 "${@:2}"
    sleep "$SETTLE"
}
finish() {
    sleep 1
    UPCALLS[$1]=$(grep -c '\[upcall\] miss ino=' "$OUT/daemon-$1.log" || true)
    PROMOTES[$1]=$(grep -c 'PROMOTE_SIGNAL=pattern-aware' "$OUT/daemon-$1.log" || true)
    SHA[$1]=$2
    log "  $1: upcalls=${UPCALLS[$1]} promotes=${PROMOTES[$1]} sha=$2"
    ebpf_stop "$MOUNT" >/dev/null 2>&1 || true
}
sum() { sudo cat "$MOUNT/$FNAME" | sha256sum | awk '{print $1}'; }
ok()  { [ "$1" = "$WANT" ] && echo OK || echo BAD; }

start seq
finish seq "$(ok "$(sum)")"

start random
sudo dd if="$MOUNT/$FNAME" of=/dev/null bs=4k skip=2000 count=1 2>/dev/null
sudo dd if="$MOUNT/$FNAME" of=/dev/null bs=4k skip=10 count=1 2>/dev/null
PROMOTES_BEFORE=$(grep -c 'PROMOTE_SIGNAL=pattern-aware' "$OUT/daemon-random.log" || true)
r=$(ok "$(sum)")
RANDOM_EARLY=$PROMOTES_BEFORE
finish random "$r"

start par
pids=()
for i in 1 2 3 4; do
    (sum > "$TMPDIR_LOCAL/par-$i") &
    pids+=($!)
done
wait "${pids[@]}"
par=OK
for i in 1 2 3 4; do [ "$(cat "$TMPDIR_LOCAL/par-$i")" = "$WANT" ] || par=BAD; done
finish par "$par"

start off --pattern-aware-admit=false
finish off "$(ok "$(sum)")"

overall=PASS
check() { # name condition description
    if eval "$2"; then log "  ok   : $3"; else log "  FAIL : $3"; overall=FAIL; fi
}
check seq    '[ "${UPCALLS[seq]}" -eq 1 ] && [ "${PROMOTES[seq]}" -ge 1 ]' "seq promoted with a single upcall"
check random '[ "$RANDOM_EARLY" -eq 0 ]' "two scattered 4 KiB reads were not promoted"
check par    '[ "${PROMOTES[par]}" -ge 1 ]' "4 concurrent readers still promoted"
check off    '[ "${PROMOTES[off]}" -eq 0 ]' "no promotion with --pattern-aware-admit=false"
for c in seq random par off; do
    check "$c" '[ "${SHA[$c]}" = OK ]' "$c: bytes match"
done

{
echo ""
echo "=============================================================="
echo "test-pattern-aware-admit  $(date +%Y-%m-%d)"
echo "=============================================================="
for c in seq random par off; do
    printf "%-7s upcalls=%s promotes=%s sha=%s\n" "$c" "${UPCALLS[$c]}" "${PROMOTES[$c]}" "${SHA[$c]}"
done
printf "random: promotions before the full read = %s\n" "$RANDOM_EARLY"
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/pattern-aware-admit-report.txt"

printf 'RESULT name=%s status=%s seq_promotes=%s par_promotes=%s random_early=%s off_promotes=%s\n' \
    "$TEST_NAME" "$overall" "${PROMOTES[seq]}" "${PROMOTES[par]}" "$RANDOM_EARLY" "${PROMOTES[off]}"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
