# shellcheck shell=bash
# scripts/lib/kmodbuild.sh — build kmod/ against one kernel in a scratch copy.
# Shared by kmod-matrix.sh (check) and build-deb.sh (package).

[[ -n "${_S3FS_LIB_KMODBUILD:-}" ]] && return 0
_S3FS_LIB_KMODBUILD=1

# shellcheck source=toolchain.sh
source "$(dirname "${BASH_SOURCE[0]}")/toolchain.sh"

# kmod_build <krel> <workdir> — build into <workdir>/<krel>/src/kmod and set
# KMOD_KO to the resulting .ko. Prints the reason on stdout and returns 1 on
# failure. Run it in a subshell: it pins KREL/KDIR and re-sources toolchain.sh
# so every per-kernel default (gcc, pahole) is that kernel's.
# Checks: build ok, no "Skipping BTF", vermagic == krel, module BTF present.
kmod_build() {
    local krel=$1 work=$2
    export KREL="$krel"; unset KDIR KMOD_CC KMOD_PAHOLE _S3FS_LIB_TOOLCHAIN
    source "$(dirname "${BASH_SOURCE[0]}")/toolchain.sh"
    local scripts="$S3FS_TOPDIR/scripts"

    "$scripts/doctor.sh" --quiet --only kmod >&2 \
        || { echo "doctor --only kmod: NG (see above)"; return 1; }

    local cc pahole ldlib kcflags src="$work/$krel/src"
    cc=$(detect_kmod_cc) || { echo "no usable gcc for $krel"; return 1; }
    pahole=$(detect_pahole) || { echo "no usable pahole for $krel"; return 1; }
    ldlib=$(pahole_libdir "$pahole"); kcflags=$(kmod_kcflags "$cc")

    mkdir -p "$src/kmod"
    cp -r "$S3FS_TOPDIR/kmod/." "$src/kmod/"; cp -r "$S3FS_TOPDIR/include" "$src/include"
    rm -f "$src"/kmod/*.ko "$src"/kmod/*.o "$src"/kmod/.*.cmd 2>/dev/null

    KMOD_MAKE_ARGS=(KDIR="$KDIR" CC="$cc" HOSTCC="$cc" PAHOLE="$pahole")
    [[ -n "$kcflags" ]] && KMOD_MAKE_ARGS+=(KCFLAGS="$kcflags")
    KMOD_SRC="$src/kmod"
    KMOD_ENV=(env ${ldlib:+LD_LIBRARY_PATH="$ldlib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"})
    KMOD_INFO="gcc=$cc pahole=$(pahole_version "$pahole" "$ldlib")"

    local log="$work/$krel/build.log"
    "${KMOD_ENV[@]}" make -C "$KMOD_SRC" "${KMOD_MAKE_ARGS[@]}" >"$log" 2>&1 \
        || { echo "build failed (tail of $log):"; tail -n 15 "$log"; return 1; }
    KMOD_KO="$KMOD_SRC/s3fs_kfunc.ko"
    [[ -f "$KMOD_KO" ]] || { echo "build succeeded but $KMOD_KO is missing"; return 1; }
    if grep -q 'Skipping BTF generation' "$log"; then
        echo "kbuild skipped BTF generation (no $KDIR/vmlinux?)"; return 1
    fi
    local vm; vm=$(modinfo -F vermagic "$KMOD_KO" 2>/dev/null | awk '{print $1}')
    [[ "$vm" == "$krel" ]] || { echo "vermagic '$vm' != kernel '$krel'"; return 1; }
    "$scripts/check-kmod-btf.sh" "$KMOD_KO" >/dev/null 2>&1 \
        || { "$scripts/check-kmod-btf.sh" "$KMOD_KO" 2>&1 | head -n 3; return 1; }
    KMOD_INFO="$KMOD_INFO vermagic=$vm"
}
