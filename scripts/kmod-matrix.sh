#!/usr/bin/env bash
# kmod-matrix.sh — build and install-check kmod/ against every installed kernel.
#
#   scripts/kmod-matrix.sh                       # all kernels >= 6.12 under /lib/modules
#   scripts/kmod-matrix.sh 6.12.0-061200-generic # chosen kernels
#   KRELS="a b" scripts/kmod-matrix.sh           # same, via env
#   SETUP=1   scripts/kmod-matrix.sh             # first run `setup-toolchain.sh headers kbuild` per kernel
#   KEEP=1    scripts/kmod-matrix.sh             # keep the scratch dir (printed at the end)
#
# Per kernel (a kernel that fails does not stop the others; exit 1 if any failed):
#   1. doctor --only kmod        headers, $KDIR/vmlinux, gcc / pahole match   (H-*, C-3, C-4)
#   2. build in a scratch copy   so no object from kernel A leaks into B
#   3. .ko vermagic == kernel    modinfo; catches a build against the wrong KDIR
#   4. module BTF is present     check-kmod-btf.sh (kfunc registration at insmod)
#   5. modules_install + depmod  into a scratch INSTALL_MOD_PATH; the .ko must land under
#                                lib/modules/<krel>/ and appear in modules.dep
# Nothing is installed into the real /lib/modules and no module is loaded.
set -uo pipefail

SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/toolchain.sh
source "$SCRIPTS_DIR/lib/toolchain.sh"

SETUP="${SETUP:-0}" KEEP="${KEEP:-0}"
[[ "${1:-}" == -h || "${1:-}" == --help ]] && { sed -n '2,19p' "$0"; exit 0; }
[[ $# -gt 0 ]] && KRELS="$*"

# check_one <krel> — runs in a subshell with KREL/KDIR pinned to that kernel.
check_one() {
    local krel=$1 work=$2
    export KREL="$krel"; unset KDIR KMOD_CC KMOD_PAHOLE
    # shellcheck disable=SC2034  # re-derive the per-kernel defaults
    KDIR="/lib/modules/$krel/build"
    # re-source so KREL/KDIR-dependent defaults are those of this kernel
    unset _S3FS_LIB_TOOLCHAIN; source "$SCRIPTS_DIR/lib/toolchain.sh"

    if [[ "$SETUP" == 1 ]]; then
        ASSUME_YES=1 KREL="$krel" "$SCRIPTS_DIR/setup-toolchain.sh" headers kbuild >&2 \
            || { echo "setup-toolchain headers kbuild failed"; return 1; }
    fi
    KREL="$krel" KDIR="$KDIR" "$SCRIPTS_DIR/doctor.sh" --quiet --only kmod >&2 \
        || { echo "doctor --only kmod: NG (see above)"; return 1; }

    local cc pahole ldlib kcflags src="$work/$krel/src" inst="$work/$krel/root"
    cc=$(detect_kmod_cc) || { echo "no usable gcc for $krel"; return 1; }
    pahole=$(detect_pahole) || { echo "no usable pahole for $krel"; return 1; }
    ldlib=$(pahole_libdir "$pahole"); kcflags=$(kmod_kcflags "$cc")

    mkdir -p "$src/kmod" "$inst"
    cp -r "$S3FS_TOPDIR/kmod/." "$src/kmod/"; cp -r "$S3FS_TOPDIR/include" "$src/include"
    rm -f "$src"/kmod/*.ko "$src"/kmod/*.o "$src"/kmod/.*.cmd 2>/dev/null

    local margs=(KDIR="$KDIR" CC="$cc" HOSTCC="$cc" PAHOLE="$pahole")
    [[ -n "$kcflags" ]] && margs+=(KCFLAGS="$kcflags")
    local env=(env ${ldlib:+LD_LIBRARY_PATH="$ldlib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"})

    "${env[@]}" make -C "$src/kmod" "${margs[@]}" >"$work/$krel/build.log" 2>&1 \
        || { echo "build failed (tail of $work/$krel/build.log):"; tail -n 15 "$work/$krel/build.log"; return 1; }
    local ko="$src/kmod/s3fs_kfunc.ko"
    [[ -f "$ko" ]] || { echo "build succeeded but $ko is missing"; return 1; }
    if grep -q 'Skipping BTF generation' "$work/$krel/build.log"; then
        echo "kbuild skipped BTF generation (no $KDIR/vmlinux?)"; return 1
    fi

    local vm; vm=$(modinfo -F vermagic "$ko" 2>/dev/null | awk '{print $1}')
    [[ "$vm" == "$krel" ]] || { echo "vermagic '$vm' != kernel '$krel'"; return 1; }
    "$SCRIPTS_DIR/check-kmod-btf.sh" "$ko" >/dev/null 2>&1 || {
        "$SCRIPTS_DIR/check-kmod-btf.sh" "$ko" 2>&1 | head -n 3; return 1; }

    "${env[@]}" make -C "$src/kmod" "${margs[@]}" INSTALL_MOD_PATH="$inst" modules_install \
        >"$work/$krel/install.log" 2>&1 \
        || { echo "modules_install failed:"; tail -n 10 "$work/$krel/install.log"; return 1; }
    local installed
    installed=$(find "$inst" -name 's3fs_kfunc.ko*' -path "*/lib/modules/$krel/*" | head -n1)
    [[ -n "$installed" ]] || { echo "s3fs_kfunc.ko not installed under lib/modules/$krel"; return 1; }
    # modules_install runs depmod against the real /lib/modules/<krel>; redo it on the scratch root.
    depmod -b "$inst" "$krel" >>"$work/$krel/install.log" 2>&1
    grep -q 's3fs_kfunc.ko' "$inst"/lib/modules/"$krel"/modules.dep \
        || { echo "s3fs_kfunc not in modules.dep of the scratch root"; return 1; }
    echo "gcc=$cc pahole=$(pahole_version "$pahole" "$ldlib") vermagic=$vm installed=${installed#"$inst"}"
}

WORK=""
main() {
    local krels rc=0 k out work
    mapfile -t krels < <(installed_krels)
    [[ ${#krels[@]} -gt 0 ]] || die "no kernel >= $S3FS_MIN_KERNEL under /lib/modules (make setup-kernel)"
    work=$(mktemp -d); WORK=$work
    [[ "$KEEP" == 1 ]] || trap 'rm -rf "$WORK"' EXIT
    declare -A result
    for k in "${krels[@]}"; do
        log "kmod matrix: $k"
        if out=$( check_one "$k" "$work" ); then result[$k]="PASS  $out"
        else result[$k]="FAIL  $out"; rc=1; fi
        info "${result[$k]}"
    done
    log "kmod matrix summary"
    for k in "${krels[@]}"; do printf '  %s\n' "$k: ${result[$k]}" >&2; done
    [[ "$KEEP" == 1 ]] && info "scratch kept: $work"
    return $rc
}
main
