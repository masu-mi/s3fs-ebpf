#!/usr/bin/env bash
# gen-vmlinux.sh — dump the running kernel BTF into a vmlinux.h header.
# Writes to STDOUT; callers redirect (e.g. `gen-vmlinux.sh > bpf/vmlinux.h`).
# bpftool is resolved by detect_bpftool (BPFTOOL= overrides); see
# scripts/lib/toolchain.sh for the Ubuntu-wrapper / Debian-/usr/sbin cases.
set -euo pipefail

# shellcheck source=lib/toolchain.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/toolchain.sh"

BTF_PATH="/sys/kernel/btf/vmlinux"

[[ -r "$BTF_PATH" ]] \
    || die "$BTF_PATH not readable. Kernel needs CONFIG_DEBUG_INFO_BTF=y (make doctor: K-3)."

BPFTOOL_BIN=$(detect_bpftool) \
    || die "no working bpftool. Run 'scripts/setup-toolchain.sh bpftool' or set BPFTOOL=/path/to/bpftool (make doctor: T-1)."

exec "$BPFTOOL_BIN" btf dump file "$BTF_PATH" format c
