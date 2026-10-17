#!/usr/bin/env bash
# test-write-partial.sh — AC-51: partial_modify end to end.  A write into a
# 40 MiB existing object loads only the chunks it touches and the flush
# copies the rest server-side (multipart + UploadPartCopy).  Scenarios and
# checks are in test-write-partial.py; this script runs them with the daemon
# started with --partial-modify and checks that the copy path was used.
#
# Usage: sudo ./scripts/test-write-partial.sh
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-write-partial}
SETTLE=${SETTLE:-6}
TEST_NAME=${TEST_NAME:-test-write-partial}
S3_BUCKET=${WRITE_PARTIAL_BUCKET:-s3fs-write-partial}

log() { printf '[write-partial] %s\n' "$*" >&2; }
cleanup() {
    ebpf_stop "$MOUNT" 2>/dev/null || true
    echo 0 > /sys/module/s3fs_kfunc/parameters/partial_modify 2>/dev/null || true
}
trap cleanup EXIT
mkdir -p "$OUT"

need_cmds docker sha256sum python3
need_built

mc() { docker exec "$MINIO_CONTAINER" mc "$@"; }
mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null

SEED=$(mktemp -d /tmp/write-partial-XXXXXX)
trap 'cleanup; rm -rf "$SEED"' EXIT
python3 -I "$TOPDIR/scripts/test-write-partial.py" "$MOUNT" "$S3_BUCKET" "$MINIO_CONTAINER" seed "$SEED"

ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" \
    --bucket="$S3_BUCKET" --arena-metadata-only \
    --policy-bpf="builtin:policy_observe" \
    --softcap=64 --cache-capacity=0 --partial-modify \
    --metrics-addr=:9600 --sample-every=100ms --sync-every=0
sleep "$SETTLE"

overall=PASS
python3 -I "$TOPDIR/scripts/test-write-partial.py" "$MOUNT" "$S3_BUCKET" "$MINIO_CONTAINER" run "$SEED" || overall=FAIL
copied=$(grep -c 'rest copied' "$OUT/daemon.log" || true)
log "flushes that copied server-side: $copied"
if [ "$copied" -lt 3 ]; then
    log "the multipart copy path was not used (partial_modify not effective)"
    overall=FAIL
fi
ebpf_stop "$MOUNT" >/dev/null 2>&1 || true
mc rb --force "local/$S3_BUCKET" >/dev/null 2>&1 || true

echo "OVERALL: $overall" | tee "$OUT/write-partial-report.txt"
printf 'RESULT name=%s status=%s\n' "$TEST_NAME" "$overall"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
