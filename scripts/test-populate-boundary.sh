#!/usr/bin/env bash
# test-populate-boundary.sh — P2/Q3 regression: POPULATE_PAGES ioctl must
# reject degenerate arguments at the kmod boundary.
#
# P2: size=0 must return -EINVAL (prevents underflow in invalidate_mapping_pages).
# P2: size > 64 MiB must return -EINVAL (prevents OOM on a single call).
# Q3: (offset + size) overflow past u64 must return -EINVAL.
#
# These guards live in s3fs_ebpf_populate_pages (kmod/vfs_ops.c) and are
# exercised directly via scripts/lib/populate-boundary-ioctl.py which opens
# /dev/s3fs_ebpf and fires the ioctl with each boundary argument set, verifying
# the returned errno.  The four cases are:
#   1. size=0              → EINVAL
#   2. offset=U64_MAX-1, size=16 → overflow → EINVAL
#   3. size=64MiB+1        → EINVAL
#   4. offset=5GiB, size=4MiB  → passes guards (returns ENOENT/ENODEV, not EINVAL)
#
# Usage:
#   sudo ./scripts/test-populate-boundary.sh
#
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
#
# Requirements: kmod + daemon built (make all), MinIO up (make compose-up).

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-populate-boundary}
SETTLE=${SETTLE:-8}
IOCTL_SCRIPT="$TOPDIR/scripts/lib/populate-boundary-ioctl.py"

log()  { printf '[populate-boundary] %s\n' "$*" >&2; }
fail() { log "FAIL: $*"; exit 1; }

TMPDIR_LOCAL=$(mktemp -d /tmp/populate-boundary-XXXXXX)
cleanup() {
    ebpf_stop "$MOUNT" 2>/dev/null || true
    rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

mkdir -p "$OUT"

# ── prereqs ───────────────────────────────────────────────────────────────────
need_cmds docker python3
need_built

# ── seed functional test file BEFORE daemon start ────────────────────────────
# The daemon syncs the bucket on startup; seeding here ensures the inode is
# present on the initial sync so the functional read does not race with the
# 5-second sync interval.
SEED_FILE="populate-boundary-seed.bin"
log "seeding ${SEED_FILE} into MinIO (before daemon start)"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
    minioadmin minioadmin >/dev/null 2>&1
dd if=/dev/urandom bs=$(( 1024 * 1024 )) count=1 iflag=fullblock 2>/dev/null \
    > "$TMPDIR_LOCAL/seed.bin"
SEED_HASH=$(sha256sum "$TMPDIR_LOCAL/seed.bin" | awk '{print $1}')
docker exec -i "$MINIO_CONTAINER" mc pipe \
    "local/$S3_BUCKET/$SEED_FILE" < "$TMPDIR_LOCAL/seed.bin" >/dev/null
log "seed done (hash: ${SEED_HASH})"

# ── mount ─────────────────────────────────────────────────────────────────────
log "mounting s3fs-ebpf"
ebpf_mount "$MOUNT"
DMESG_BEFORE=$(dmesg 2>/dev/null | wc -l || echo 0)
ebpf_start_daemon "$OUT/daemon.log" \
    --arena-metadata-only \
    --policy-bpf="builtin:policy_observe" \
    --softcap=8 --cache-capacity=0 \
    --metrics-addr=:9600 \
    --sample-every=1s --sync-every=5s
log "daemon started, settling ${SETTLE}s"
sleep "$SETTLE"

# Helper: check dmesg for kernel WARNs/BUGs
check_dmesg_clean() {
    local new_dmesg
    new_dmesg=$(dmesg 2>/dev/null | tail -n +"$DMESG_BEFORE")
    if echo "$new_dmesg" | grep -qE 'WARNING:|BUG:|Oops:|kernel BUG|general protection'; then
        log "  dmesg scan: kernel WARN/BUG found — FAIL"
        echo "$new_dmesg" | grep -E 'WARNING:|BUG:|Oops:|kernel BUG|general protection' >&2
        return 1
    fi
    log "  dmesg scan: no kernel WARN/BUG — PASS"
    return 0
}

# Helper: verify daemon is still alive
check_daemon_alive() {
    if ! daemon_running; then
        log "  daemon crashed — FAIL"
        return 1
    fi
    log "  daemon still running — PASS"
    return 0
}

# ── ioctl boundary checks (4 cases via Python helper) ────────────────────────
log "running ioctl boundary checks (populate-boundary-ioctl.py)"
IOCTL_PASS=PASS
if python3 "$IOCTL_SCRIPT" 2>&1; then
    log "  ioctl boundary: PASS"
else
    rc=$?
    if [ "$rc" -eq 2 ]; then
        log "  ioctl boundary: SKIP (device not accessible)"
        IOCTL_PASS=SKIP
    else
        log "  ioctl boundary: FAIL"
        IOCTL_PASS=FAIL
    fi
fi

# ── functional: verify normal 1 MiB read after ioctl guard checks ─────────────
log "functional: reading ${SEED_FILE} (seeded before daemon start)"

FUNC_PASS=PASS
READ_HASH=$(sha256sum "$MOUNT/$SEED_FILE" 2>/dev/null | awk '{print $1}' || echo "ERROR")
if [ "$READ_HASH" = "$SEED_HASH" ]; then
    log "  normal 1 MiB read: PASS (sha256 match)"
else
    log "  normal 1 MiB read: FAIL (expected $SEED_HASH got $READ_HASH)"
    FUNC_PASS=FAIL
fi

# ── dmesg and daemon health ────────────────────────────────────────────────────
DMESG_PASS=PASS
DAEMON_PASS=PASS
log "final checks"
check_dmesg_clean || DMESG_PASS=FAIL
check_daemon_alive || DAEMON_PASS=FAIL

# ── report ────────────────────────────────────────────────────────────────────
overall=PASS
[[ "$IOCTL_PASS" == FAIL ]] && overall=FAIL
[[ "$FUNC_PASS"  == FAIL ]] && overall=FAIL
[[ "$DMESG_PASS" == FAIL ]] && overall=FAIL
[[ "$DAEMON_PASS" == FAIL ]] && overall=FAIL

{
echo ""
echo "=============================================================="
echo "test-populate-boundary  $(date +%Y-%m-%d)"
echo "=============================================================="
echo "ioctl boundary (4 cases)            : $IOCTL_PASS"
echo "functional (1 MiB normal read)      : $FUNC_PASS"
echo "dmesg (no WARN/BUG)                 : $DMESG_PASS"
echo "daemon alive                        : $DAEMON_PASS"
echo ""
echo "OVERALL: $overall"
echo "=============================================================="
} | tee "$OUT/populate-boundary-report.txt"

printf 'RESULT name=test-populate-boundary status=%s ioctl=%s func=%s dmesg=%s daemon=%s\n' \
    "$overall" "$IOCTL_PASS" "$FUNC_PASS" "$DMESG_PASS" "$DAEMON_PASS"
if [[ "$overall" == FAIL ]]; then
    [ "$IOCTL_PASS" = FAIL ] && printf 'DETAIL ioctl: one or more boundary cases returned wrong errno\n'
    [ "$FUNC_PASS"  = FAIL ] && printf 'DETAIL func: 1MiB normal read sha256 mismatch (expected=%s got=%s)\n' \
        "$SEED_HASH" "$READ_HASH"
    [ "$DMESG_PASS" = FAIL ] && printf 'DETAIL dmesg: kernel WARNING/BUG found during run\n'
    [ "$DAEMON_PASS" = FAIL ] && printf 'DETAIL daemon: crashed during test\n'
fi

[[ "$overall" == FAIL ]] && exit 1 || exit 0
