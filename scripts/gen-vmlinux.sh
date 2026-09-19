#!/usr/bin/env bash
# gen-vmlinux.sh — dump the running kernel BTF into a vmlinux.h header.
# Writes to STDOUT; callers redirect (e.g. `gen-vmlinux.sh > bpf/vmlinux.h`).
set -euo pipefail

BTF_PATH="/sys/kernel/btf/vmlinux"

if [[ ! -r "$BTF_PATH" ]]; then
    echo "ERROR: $BTF_PATH not readable. Kernel needs CONFIG_DEBUG_INFO_BTF=y." >&2
    exit 1
fi

if ! command -v bpftool >/dev/null 2>&1; then
    echo "ERROR: bpftool not found. Install linux-tools-$(uname -r) or build from source." >&2
    exit 1
fi

exec bpftool btf dump file "$BTF_PATH" format c
