#!/usr/bin/env bash
# test-pick-victim-stale.sh — AC-64: an ino that is no longer a candidate (its
# object was deleted, so a sync dropped it from the admit set) must not stay in
# the policy's own list.  When pick_victim names such an ino, the kmod calls
# on_remove for it and asks again; it used to stop asking (the rest of the round
# fell back to score order and the ino stayed in the BPF map for good).
#
# Flow: 8 small objects, capacity 2 of them, policy sieve_pick_ops (its FIFO is
# the `idx` map).  Prewarm leaves 2 survivors in the FIFO.  Delete both objects,
# wait for a sync, read 3 evicted files back (3 candidates > capacity): the
# round's pick_victim returns the survivors first.  Neither may be in `idx`.
#
# Usage: sudo ./scripts/test-pick-victim-stale.sh
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-pick-victim-stale}
S3_BUCKET=${PICK_STALE_BUCKET:-s3fs-pick-stale}
TEST_NAME=${TEST_NAME:-test-pick-victim-stale}
METRICS=${METRICS_URL:-http://localhost:9600}
N=8
FILE_BYTES=100

log() { printf '[pick-stale] %s\n' "$*" >&2; }
cleanup() { ebpf_stop "$MOUNT" 2>/dev/null || true; mc rb --force "local/$S3_BUCKET" >/dev/null 2>&1 || true; }
mc() { docker exec "$MINIO_CONTAINER" mc "$@"; }
trap cleanup EXIT
mkdir -p "$OUT"
need_cmds docker curl python3 bpftool
need_built
mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
mc rb --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
mc mb "local/$S3_BUCKET" >/dev/null
for i in $(seq 0 $((N - 1))); do
    head -c "$FILE_BYTES" /dev/zero | tr '\0' "$(printf '%x' $((i + 10)))" |
        docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/stale_$(printf %03d "$i").txt" >/dev/null
done

# idx_inos: the inos in the policy's `idx` map, one per line.
idx_inos() {
    sudo bpftool map dump name idx -j 2>/dev/null | python3 -I -c '
import json, sys
def num(v):
    if isinstance(v, int): return v
    if isinstance(v, list): return int.from_bytes(bytes(int(x, 16) if isinstance(x, str) else x for x in v), "little")
    return int(v, 0)
for e in json.load(sys.stdin):
    print(num(e["key"] if "key" in e else e["formatted"]["key"]))
'
}

ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" \
    --policy-bpf=builtin:policy_sieve_pick_ops --metrics-addr=:9600 \
    --sample-every=2s --sync-every=2s --softcap=64 --cache-capacity=$((2 * FILE_BYTES))
for _ in 1 2 3; do curl -s -X POST "$METRICS/policy/eval" >/dev/null; sleep 2; done

mapfile -t survivors < <(idx_inos)
log "policy list after prewarm: ${survivors[*]:-(empty)}"
if [ "${#survivors[@]}" -lt 1 ]; then
    log "no candidates in the policy list: the policy did not run"
    echo "RESULT name=$TEST_NAME status=FAIL"; exit 1
fi

# Names of the survivors (ls -i prints inos), then delete their objects.
declare -A name_of
while read -r ino name; do name_of[$ino]=$name; done < <(ls -i "$MOUNT" | awk '{print $1, $2}')
for ino in "${survivors[@]}"; do
    n=${name_of[$ino]:-}
    [ -n "$n" ] || { log "ino $ino has no name in the mount"; continue; }
    log "deleting $n (ino $ino)"
    mc rm "local/$S3_BUCKET/$n" >/dev/null
done
sleep 6   # >= 2 sync intervals: the deleted inos leave the admit set

# Read back the evicted files: 3 candidates > capacity (2).
read_back=0
for n in "$MOUNT"/stale_*.txt; do
    [ -e "$n" ] || continue
    cat "$n" >/dev/null 2>&1 || continue
    read_back=$((read_back + 1))
    [ "$read_back" -ge 3 ] && break
done
for _ in 1 2 3; do curl -s -X POST "$METRICS/policy/eval" >/dev/null; sleep 2; done

mapfile -t after < <(idx_inos)
log "policy list after the round: ${after[*]:-(empty)}"
overall=PASS
for ino in "${survivors[@]}"; do
    for a in "${after[@]:-}"; do
        if [ "$a" = "$ino" ]; then
            log "FAIL: ino $ino (object deleted) is still in the policy list"
            overall=FAIL
        fi
    done
done
echo "OVERALL: $overall" | tee "$OUT/report.txt"
printf 'RESULT name=%s status=%s\n' "$TEST_NAME" "$overall"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
