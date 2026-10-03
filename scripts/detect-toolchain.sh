#!/usr/bin/env bash
# detect-toolchain.sh — print the toolchain the build should use.
#
#   scripts/detect-toolchain.sh          # human readable KEY=value
#   scripts/detect-toolchain.sh --make   # VAR?=value tokens for the Makefile
#
# Never fails: an undetectable tool falls back to its plain name so `make`
# still parses and produces the real compiler error. `make doctor` explains
# what is missing and how to fix it.
set -uo pipefail

# shellcheck source=lib/toolchain.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/toolchain.sh"

clang=$(detect_clang)
bpftool=$(detect_bpftool)
kmod_cc=$(detect_kmod_cc)
pahole=$(detect_pahole)
ldlib=$(pahole_libdir "$pahole")

if [[ "${1:-}" == --make ]]; then
    # One token per variable: values never contain spaces.
    printf 'CLANG?=%s BPFTOOL?=%s KDIR?=%s KMOD_CC?=%s KMOD_PAHOLE?=%s KMOD_LDLIB?=%s\n' \
        "$clang" "$bpftool" "$KDIR" "$kmod_cc" "$pahole" "$ldlib"
else
    printf '%-12s %s\n' \
        CLANG "$clang" BPFTOOL "$bpftool" KDIR "$KDIR" \
        KMOD_CC "$kmod_cc" KMOD_PAHOLE "$pahole" KMOD_LDLIB "${ldlib:-(none)}"
fi
