#!/usr/bin/env bash
# runner.sh — Week 0/1 skeleton bench runner. LEGACY.
#
# Current bench entry points are scripts/bench-*.sh (invoked via
# `make bench` / `make bench-write` / `make bench-scale` /
# `make bench-3way`). Those handle kmod insmod + mount + daemon
# lifecycle end-to-end and don't need the demo file layout this
# runner assumes (`nrfiles=10000` pre-created files under $MOUNT_POINT).
#
# Kept only for `make bench-dry` (verify fio job files parse). The
# --dry-run path below is the only supported use.
#
# Env:
#   MOUNT_POINT   default /mnt/s3demo (was /mnt/s3data pre-Week-4)
#   RESULT_ROOT   default ./bench/results
#
# Flags:
#   --dry-run    only verify job files exist and print the plan
set -euo pipefail

TOPDIR="$(cd "$(dirname "$0")/.." && pwd)"
BENCH_DIR="${TOPDIR}/bench"
MOUNT_POINT="${MOUNT_POINT:-/mnt/s3demo}"
RESULT_ROOT="${RESULT_ROOT:-${BENCH_DIR}/results}"
STAMP="$(date +%Y%m%d-%H%M%S)"
OUT="${RESULT_ROOT}/${STAMP}"

DRY_RUN=0
for arg in "$@"; do
    case "$arg" in
        --dry-run) DRY_RUN=1 ;;
        -h|--help)
            sed -n '2,10p' "$0"; exit 0 ;;
    esac
done

FIO_JOBS=(
    "${BENCH_DIR}/fio/stat_latency.fio"
    "${BENCH_DIR}/fio/sequential_read.fio"
)
BCC_JOB="${BENCH_DIR}/bcc/vfs_latency.py"

echo "MOUNT_POINT = ${MOUNT_POINT}"
echo "RESULT_DIR  = ${OUT}"
echo "fio jobs    :"
for j in "${FIO_JOBS[@]}"; do
    [[ -f "$j" ]] || { echo "MISSING: $j" >&2; exit 1; }
    echo "  - $j"
done
[[ -f "$BCC_JOB" ]] || { echo "MISSING: $BCC_JOB" >&2; exit 1; }
echo "bcc job     : ${BCC_JOB}"

if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "[dry-run] job files verified; skipping execution."
    exit 0
fi

if ! mountpoint -q "${MOUNT_POINT}"; then
    echo "ERROR: ${MOUNT_POINT} is not mounted." >&2
    echo "       This legacy runner requires a manual mount." >&2
    echo "       For end-to-end bench with auto-mount, use:" >&2
    echo "         make bench            # latency" >&2
    echo "         make bench-write      # write throughput" >&2
    echo "         make bench-scale      # parallel scale" >&2
    exit 1
fi

mkdir -p "${OUT}"

for job in "${FIO_JOBS[@]}"; do
    name="$(basename "$job" .fio)"
    echo "==> fio ${name}"
    MOUNT_POINT="${MOUNT_POINT}" fio --output-format=json+ \
        --output="${OUT}/${name}.json" "${job}"
done

echo "==> bcc vfs_latency (30s sample)"
timeout 30 "${BCC_JOB}" --interval 30 > "${OUT}/vfs_latency.txt" || true

echo "done. results at ${OUT}"
