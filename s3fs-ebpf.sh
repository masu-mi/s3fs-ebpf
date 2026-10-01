#!/usr/bin/env bash
# s3fs-ebpf.sh — end-to-end mount / unmount / policy-reload wrapper.
# Mirrors the flow documented in docs/design/design.md §4.
set -euo pipefail

TOPDIR="$(cd "$(dirname "$0")" && pwd)"
KMODULE="${TOPDIR}/kmod/s3fs_kfunc.ko"
DAEMON="${TOPDIR}/bin/s3fs-ebpf"
MOUNT_POINT="${MOUNT_POINT:-/mnt/s3data}"
POLICY_FILE="${POLICY_FILE:-${TOPDIR}/policies/default_policy.lua}"
BUCKET_NAME="${BUCKET_NAME:-s3fs-test}"
ENDPOINT="${ENDPOINT:-http://localhost:9000}"

mount_s3fs() {
    echo "[1/4] Loading KFunc kernel module..."
    if ! lsmod | grep -q "^s3fs_kfunc"; then
        sudo insmod "${KMODULE}"
    fi

    echo "[2/4] Ensuring mount point exists..."
    sudo mkdir -p "${MOUNT_POINT}"

    echo "[3/4] Launching Go daemon..."
    sudo "${DAEMON}" mount \
        --bucket "${BUCKET_NAME}" \
        --endpoint "${ENDPOINT}" \
        --mount-point "${MOUNT_POINT}" \
        --policy "${POLICY_FILE}" &

    echo "[4/4] Verifying..."
    sleep 2
    df -h "${MOUNT_POINT}" || true
    echo "s3fs-ebpf mounted on ${MOUNT_POINT}"
}

reload_policy() {
    echo "[+] Hot-reloading policy from ${POLICY_FILE}..."
    sudo "${DAEMON}" policy --action reload --plugin "${POLICY_FILE}"
}

unmount_s3fs() {
    echo "[1/2] Unmounting VFS..."
    sudo "${DAEMON}" unmount --mount-point "${MOUNT_POINT}" || true
    sudo umount "${MOUNT_POINT}" 2>/dev/null || true

    echo "[2/2] Unloading kernel module..."
    if lsmod | grep -q "^s3fs_kfunc"; then
        sudo rmmod s3fs_kfunc
    fi
    echo "s3fs-ebpf unmounted."
}

case "${1:-}" in
    mount)   mount_s3fs ;;
    unmount) unmount_s3fs ;;
    reload)  reload_policy ;;
    *)       echo "Usage: $0 {mount|unmount|reload}"; exit 2 ;;
esac
