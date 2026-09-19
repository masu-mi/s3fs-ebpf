#!/usr/bin/env bash
# runner.sh — orchestrate fio + BCC benchmarks against a mounted s3fs_ebpf target.
#
# Env:
#   MOUNT_POINT   default /mnt/s3data
#   RESULT_ROOT   default ./bench/results
#
# Flags:
#   --dry-run    only verify job files exist and print the plan
set -euo pipefail

TOPDIR="$(cd "$(dirname "$0")/.." && pwd)"
BENCH_DIR="${TOPDIR}/bench"
MOUNT_POINT="${MOUNT_POINT:-/mnt/s3data}"
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
    echo "ERROR: ${MOUNT_POINT} is not mounted. Run 's3fs-ebpf mount' first." >&2
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
