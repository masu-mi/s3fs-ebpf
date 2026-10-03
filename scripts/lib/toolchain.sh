# shellcheck shell=bash disable=SC2034  # S3FS_* constants are read by callers
# scripts/lib/toolchain.sh — single source of truth for "which compiler /
# pahole / bpftool / kernel headers does this host build with".
#
# Used by:
#   scripts/detect-toolchain.sh  -> Makefile (CLANG, KMOD_CC, ...)
#   scripts/doctor.sh            -> pre-flight checks with fix hints
#   scripts/setup-toolchain.sh   -> decides what to install
#
# Every detect_* honours an explicit env override first (CLANG=, KMOD_CC=,
# KMOD_PAHOLE=, BPFTOOL=, KDIR=) so `make CLANG=clang-21` keeps working.
# See docs/dev-notes/build-env-dependencies.md for why each rule exists.

[[ -n "${_S3FS_LIB_TOOLCHAIN:-}" ]] && return 0
_S3FS_LIB_TOOLCHAIN=1

# shellcheck source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# --- requirements -----------------------------------------------------------
# kmod/s3fs_ebpf_dev.c #errors below 6.12 (struct bpf_arena layout).
S3FS_MIN_KERNEL="6.12"
# The struct bpf_arena layout mirror is only verified on this series.
S3FS_VERIFIED_KERNEL_SERIES="6.12"
# -mcpu=v3 + __BPF_FEATURE_ADDR_SPACE_CAST (bpf/Makefile, daemon/generate.go).
S3FS_MIN_CLANG=19
# Kernels built with gcc>=14 put -fmin-function-alignment into the module
# flags; gcc-13 rejects it. Older gcc than the kernel's is OK down to 14.
S3FS_MIN_KMOD_GCC=14

KREL="${KREL:-$(uname -r)}"
KDIR="${KDIR:-/lib/modules/${KREL}/build}"
PAHOLE_PREFIX="${PAHOLE_PREFIX:-$HOME/.local}"

# --- kernel ---------------------------------------------------------------

# kernel_version — "6.12.5" part of uname -r.
kernel_version() { echo "${KREL%%-*}"; }

# kernel_series — "6.12".
kernel_series() { kernel_version | cut -d. -f1,2; }

# is_ubuntu_mainline — kernel.ubuntu.com/mainline builds look like
# 6.12.0-061200-generic and have no linux-headers/linux-tools in apt.
is_ubuntu_mainline() { [[ "$KREL" =~ ^[0-9.]+-[0-9]{6}(rc[0-9]+)?-generic$ ]]; }

# kconfig <SYMBOL> — value of CONFIG_<SYMBOL> from $KDIR/.config (or
# /boot/config-$KREL), quotes stripped. Empty when unknown.
kconfig() {
    local f
    for f in "$KDIR/.config" "/boot/config-$KREL"; do
        if [[ -r "$f" ]]; then
            sed -n "s/^CONFIG_$1=\"\{0,1\}\([^\"]*\)\"\{0,1\}$/\1/p" "$f" | head -n1
            return 0
        fi
    done
}

# kernel_cc_is_clang — true when the running kernel was built with clang
# (then kbuild wants LLVM=1, which this repo does not wire up).
kernel_cc_is_clang() { [[ "$(kconfig CC_IS_CLANG)" == y ]]; }

# kernel_gcc_major — gcc major the kernel was built with (e.g. 15).
kernel_gcc_major() {
    local v
    v=$(kconfig GCC_VERSION)                 # 150100
    if [[ -n "$v" && "$v" != 0 ]]; then
        echo $((v / 10000))
        return 0
    fi
    # Fallback: "... gcc-14 (Debian 14.2.0-19) 14.2.0 ..." / "gcc version 13.3.0"
    sed -nE 's/.*gcc(-[0-9]+)? (version |\([^)]*\) )([0-9]+)\..*/\3/p' /proc/version 2>/dev/null | head -n1
}

# kernel_pahole_version — encoded pahole version the kernel was built with
# (CONFIG_PAHOLE_VERSION, 130 == v1.30). Empty when unknown.
kernel_pahole_version() {
    local v
    v=$(kconfig PAHOLE_VERSION)
    [[ -n "$v" && "$v" != 0 ]] && echo "$v"
}

# --- compilers --------------------------------------------------------------

# clang_major <cmd> — major version of a clang binary, empty if not runnable.
clang_major() {
    "$1" --version 2>/dev/null | sed -nE 's/.*clang version ([0-9]+).*/\1/p' | head -n1
}

# gcc_major <cmd>
gcc_major() { "$1" -dumpversion 2>/dev/null | cut -d. -f1; }

# detect_clang — clang able to build bpf/ (>= S3FS_MIN_CLANG). Prints a
# command *name* (clang-20), not a path: bpf2go derives llvm-strip-20 from
# the "clang" prefix. Returns 1 (printing the best fallback) if none fits.
detect_clang() {
    if [[ -n "${CLANG:-}" ]]; then echo "$CLANG"; return 0; fi
    local v c m
    for v in $(seq 30 -1 "$S3FS_MIN_CLANG"); do
        command -v "clang-$v" >/dev/null 2>&1 && { echo "clang-$v"; return 0; }
    done
    for c in clang /usr/lib/llvm-*/bin/clang; do
        m=$(clang_major "$c")
        if [[ -n "$m" && "$m" -ge "$S3FS_MIN_CLANG" ]]; then echo "$c"; return 0; fi
    done
    echo clang
    return 1
}

# detect_kmod_cc — gcc for the out-of-tree kmod build. Distro/mainline
# kernel Makefiles hard-code the exact compiler they were built with
# (e.g. CC=gcc-15 on Ubuntu mainline 6.12) which may not exist here, so we
# always pass CC=/HOSTCC= explicitly. Prefers the exact major, then the
# newest gcc >= min(kernel major, 14).
detect_kmod_cc() {
    if [[ -n "${KMOD_CC:-}" ]]; then echo "$KMOD_CC"; return 0; fi
    local want floor v m
    want=$(kernel_gcc_major)
    if [[ -n "$want" ]]; then
        command -v "gcc-$want" >/dev/null 2>&1 && { echo "gcc-$want"; return 0; }
        [[ "$(gcc_major gcc)" == "$want" ]] && { echo gcc; return 0; }
        floor=$(( want < S3FS_MIN_KMOD_GCC ? want : S3FS_MIN_KMOD_GCC ))
    else
        floor=0
    fi
    for v in $(seq 30 -1 "$floor"); do
        command -v "gcc-$v" >/dev/null 2>&1 && { echo "gcc-$v"; return 0; }
    done
    m=$(gcc_major gcc)
    if [[ -n "$m" && "$m" -ge "$floor" ]]; then echo gcc; return 0; fi
    echo gcc
    return 1
}

# kmod_kcflags <kmod-cc> — extra KCFLAGS for building with an older gcc
# than the kernel's. The kernel's .config records gcc-15 features
# (CONFIG_CC_HAS_COUNTED_BY), so headers use __counted_by() and gcc-14
# prints "'counted_by' attribute directive ignored" for every include.
# The attribute only feeds bounds checking (no layout change), so the
# warning is pure noise in that combination. Prints nothing otherwise.
kmod_kcflags() {
    local want have
    want=$(kernel_gcc_major); have=$(gcc_major "$1")
    [[ -n "$want" && -n "$have" && "$have" -lt "$want" \
        && "$(kconfig CC_HAS_COUNTED_BY)" == y ]] && echo "-Wno-attributes"
    return 0
}

# --- pahole -----------------------------------------------------------------

# pahole_version <bin> [libdir] — encoded version (v1.30 -> 130), empty if
# the binary is missing or cannot load its libdwarves.
pahole_version() {
    local bin=$1 libdir=${2:-}
    [[ -x "$bin" ]] || bin=$(command -v "$bin" 2>/dev/null) || return 0
    LD_LIBRARY_PATH="${libdir}${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" "$bin" --version 2>/dev/null \
        | sed -nE 's/^v?([0-9]+)\.([0-9]+).*/\1\2/p' | head -n1
}

# pahole_libdir <bin> — LD_LIBRARY_PATH entry a locally built pahole needs.
pahole_libdir() {
    [[ "$1" == "$PAHOLE_PREFIX/bin/pahole" ]] && echo "$PAHOLE_PREFIX/lib"
    return 0
}

# detect_pahole — pahole >= CONFIG_PAHOLE_VERSION. An older pahole (e.g.
# Ubuntu noble's 1.25 vs a kernel built with 1.30) silently emits an empty
# module BTF and kfunc registration fails at insmod. Prefers the distro
# binary, falls back to $PAHOLE_PREFIX (scripts/setup-toolchain.sh kbuild).
detect_pahole() {
    if [[ -n "${KMOD_PAHOLE:-}" ]]; then echo "$KMOD_PAHOLE"; return 0; fi
    local want c v
    want=$(kernel_pahole_version)
    for c in pahole "$PAHOLE_PREFIX/bin/pahole"; do
        v=$(pahole_version "$c" "$(pahole_libdir "$c")")
        [[ -z "$v" ]] && continue
        if [[ -z "$want" || "$v" -ge "$want" ]]; then
            [[ "$c" == pahole ]] && c=$(command -v pahole)
            echo "$c"
            return 0
        fi
    done
    echo pahole
    return 1
}

# --- bpftool ----------------------------------------------------------------

# detect_bpftool — a bpftool that actually runs. Ubuntu's /usr/sbin/bpftool
# is a wrapper that dispatches on uname -r and exits 2 on mainline kernels;
# Debian installs to /usr/sbin which is not on a non-root PATH. BTF dump is
# kernel-version agnostic so any concrete binary is fine.
detect_bpftool() {
    if [[ -n "${BPFTOOL:-}" ]] && "$BPFTOOL" version >/dev/null 2>&1; then
        echo "$BPFTOOL"; return 0
    fi
    local c
    # shellcheck disable=SC2012  # paths are fixed, ls -v gives version sort
    for c in bpftool /usr/sbin/bpftool /usr/local/sbin/bpftool \
             $(ls -dv /usr/lib/linux-tools/*/bpftool /usr/lib/linux-tools-*/bpftool 2>/dev/null | tac); do
        if "$c" version >/dev/null 2>&1; then
            [[ "$c" == bpftool ]] && c=$(command -v bpftool)
            echo "$c"; return 0
        fi
    done
    echo "${BPFTOOL:-bpftool}"
    return 1
}

# --- go ---------------------------------------------------------------------

# go_required_version — "go X.Y.Z" directive from daemon/go.mod.
go_required_version() {
    awk '$1 == "go" { print $2; exit }' "$S3FS_TOPDIR/daemon/go.mod" 2>/dev/null
}

# go_installed_version — "1.25.0" or empty.
go_installed_version() {
    local g
    g=$(command -v go 2>/dev/null || { [[ -x /usr/local/go/bin/go ]] && echo /usr/local/go/bin/go; }) || return 0
    [[ -n "$g" ]] || return 0
    GOTOOLCHAIN=local "$g" env GOVERSION 2>/dev/null | sed 's/^go//'
}
