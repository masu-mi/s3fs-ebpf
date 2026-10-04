#!/usr/bin/env bash
# check-kmod-btf.sh [--quiet] <module.ko> — fail unless the module carries
# real BTF. --quiet suppresses the explanation (the caller retries).
#
# kbuild only *skips* module BTF (exit 0) when $KDIR/vmlinux is missing,
# and an old pahole emits a ~400 B stub; either way the build "succeeds"
# and insmod later fails with "failed to register kfunc set". Catch it at
# build time instead (make doctor: H-2 / C-4). On failure the .ko is
# removed so the next `make kmod` relinks it once the cause is fixed.
set -euo pipefail

QUIET=0
[[ "${1:-}" == --quiet ]] && { QUIET=1; shift; }
ko=${1:?usage: check-kmod-btf.sh [--quiet] <module.ko>}
MIN_BTF_BYTES=${MIN_BTF_BYTES:-1024}

size_hex=$(objdump -h "$ko" 2>/dev/null | awk '$2 == ".BTF" { print $3; exit }')
size=$((16#${size_hex:-0}))

if (( size < MIN_BTF_BYTES )); then
    [[ $QUIET == 1 ]] || {
        echo "ERROR: $ko has ${size} bytes of .BTF (< ${MIN_BTF_BYTES}); kfunc registration will fail at insmod."
        echo "       Cause is \$KDIR/vmlinux missing (H-2) or pahole older than the kernel's (C-4)."
        echo "       Fix: scripts/setup-toolchain.sh kbuild   (then: make kmod)"
    } >&2
    rm -f "$ko"
    exit 1
fi
echo "  BTF     $(basename "$ko"): ${size} bytes"
