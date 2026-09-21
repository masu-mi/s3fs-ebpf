#!/usr/bin/env bash
# gen-vmlinux.sh — dump the running kernel BTF into a vmlinux.h header.
# Writes to STDOUT; callers redirect (e.g. `gen-vmlinux.sh > bpf/vmlinux.h`).
set -euo pipefail

BTF_PATH="/sys/kernel/btf/vmlinux"

if [[ ! -r "$BTF_PATH" ]]; then
    echo "ERROR: $BTF_PATH not readable. Kernel needs CONFIG_DEBUG_INFO_BTF=y." >&2
    exit 1
fi

# Ubuntu's /usr/sbin/bpftool is a shim that dispatches to
# /usr/lib/linux-tools/<uname -r>/bpftool. On mainline kernels (e.g. 6.12
# from the Ubuntu mainline PPA) there is no matching linux-tools package
# so the shim exits 2 with a "please install" warning. Fall back to the
# newest concrete bpftool in /usr/lib/linux-tools/*/ — bpftool's BTF
# dump is kernel-agnostic and cross-version-compatible.
find_bpftool() {
    if [[ -n "${BPFTOOL:-}" ]] && command -v "$BPFTOOL" >/dev/null 2>&1; then
        echo "$BPFTOOL"
        return 0
    fi
    if bpftool version >/dev/null 2>&1; then
        command -v bpftool
        return 0
    fi
    local candidate
    candidate=$(ls -dv /usr/lib/linux-tools/*/bpftool 2>/dev/null | tail -1)
    if [[ -n "$candidate" && -x "$candidate" ]]; then
        echo "$candidate"
        return 0
    fi
    return 1
}

BPFTOOL_BIN=$(find_bpftool) || {
    echo "ERROR: no working bpftool found. Install linux-tools-generic or set BPFTOOL=/path/to/bpftool." >&2
    exit 1
}

exec "$BPFTOOL_BIN" btf dump file "$BTF_PATH" format c
