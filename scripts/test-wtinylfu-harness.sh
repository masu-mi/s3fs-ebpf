#!/usr/bin/env bash
# test-wtinylfu-harness.sh — AC-66: load policy_wtinylfu_pick_ops's operations
# as a SEC("syscall") program (bpf/test/wtinylfu_harness.bpf.c) and drive them
# from Go: the verifier accepts them, the BPF version's hits equal the Go model
# (`wtinylfu-clock` in daemon/internal/trace/sim_wtinylfu.go) on the same
# trace, and picked inos that are only shed come back from limbo.  Needs no
# kmod and no mount, only root (CAP_BPF) and bpf/vmlinux.h.
#
# Usage: sudo ./scripts/test-wtinylfu-harness.sh
# Exit: 0 = PASS, 1 = FAIL, 2 = prereq missing.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CLANG=${CLANG:-clang}
ARCH=$(uname -m | sed 's/x86_64/x86/;s/aarch64/arm64/')
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT

if [[ ! -f "$TOPDIR/bpf/vmlinux.h" ]]; then
    make -C "$TOPDIR/bpf" vmlinux >/dev/null || { echo "SKIP: no bpf/vmlinux.h" >&2; exit 2; }
fi
"$CLANG" -O2 -g -Wall -target bpf -mcpu=v3 -D__TARGET_ARCH_"$ARCH" \
    -I"$TOPDIR/bpf" -I"$TOPDIR/include" \
    -c "$TOPDIR/bpf/test/wtinylfu_harness.bpf.c" -o "$OUT/harness.o"

cd "$TOPDIR/daemon"
if HARNESS_OBJ="$OUT/harness.o" go test -count=1 -tags bpfharness \
    -run 'TestBPF' -v ./internal/trace; then
    echo "PASS: test-wtinylfu-harness"
else
    echo "FAIL: test-wtinylfu-harness"
    exit 1
fi
