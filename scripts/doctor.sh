#!/usr/bin/env bash
# doctor.sh — check every host dependency that has broken the build or the
# benches before, and print the fix for each failure.
#
#   scripts/doctor.sh                    # build + runtime + bench checks
#   scripts/doctor.sh --build            # only what `make all` needs
#   scripts/doctor.sh --only kmod|bpf    # one component (Makefile preflight)
#   scripts/doctor.sh --quiet ...        # print only NG / WARN lines
#
# Exit status: 1 if any NG (build will fail), 0 otherwise (WARN allowed).
# The categories and IDs (K-*, H-*, C-*, T-*, B-*) match
# docs/dev-notes/build-env-dependencies.md.
set -uo pipefail

# shellcheck source=lib/toolchain.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/toolchain.sh"

MODE=all QUIET=0 ONLY=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --build) MODE=build ;;
        --all) MODE=all ;;
        --quiet) QUIET=1 ;;
        --only) ONLY=$2; MODE=build; shift ;;
        -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
        *) die "unknown option: $1" ;;
    esac
    shift
done

# --only <component>: the check IDs that gate that component's build.
case "$ONLY" in
    "")   ID_FILTER='.*' ;;
    kmod) ID_FILTER='^(K-1|H-[0-9]|C-3|C-4)$' ;;
    bpf)  ID_FILTER='^(K-3|C-1|T-1)$' ;;
    *)    die "unknown component for --only: $ONLY (kmod|bpf)" ;;
esac

SETUP="scripts/setup-toolchain.sh"
n_ng=0 n_warn=0

if [[ -t 1 ]]; then B=$'\033[1m' G=$'\033[32m' R=$'\033[31m' Y=$'\033[33m' Z=$'\033[0m'
else B='' G='' R='' Y='' Z=''; fi
# Setup steps named in fix hints, collected for the one-line summary.
FIX_STEPS=()
want()    { [[ "$1" =~ $ID_FILTER ]]; }
section() { [[ $QUIET == 1 ]] || printf '\n%s[%s]%s\n' "$B" "$1" "$Z"; }
ok()   { want "$1" || return 0; [[ $QUIET == 1 ]] || printf '  %sOK  %s %-6s %s\n' "$G" "$Z" "$1" "$2"; }
ng()   { want "$1" || return 0; printf '  %sNG  %s %-6s %s\n' "$R" "$Z" "$1" "$2"; shift 2; fix "$@"; n_ng=$((n_ng + 1)); }
wn()   { want "$1" || return 0; printf '  %sWARN%s %-6s %s\n' "$Y" "$Z" "$1" "$2"; shift 2; fix "$@"; n_warn=$((n_warn + 1)); }
# pahole_fmt 130 -> v1.30
pahole_fmt() { [[ -n "$1" ]] && echo "v1.${1#1}" || echo "${2:-none}"; }
fix() {
    local l step
    for l in "$@"; do
        printf '              -> %s\n' "$l"
        if [[ "$l" =~ ^$SETUP\ ([a-z-]+) ]]; then
            step=${BASH_REMATCH[1]}
            [[ " ${FIX_STEPS[*]} " == *" $step "* ]] || FIX_STEPS+=("$step")
        fi
    done
}

# -----------------------------------------------------------------------------
section "kernel ($KREL, $(os_id) $(os_codename))"
kver=$(kernel_version)
if ver_ge "$kver" "$S3FS_MIN_KERNEL"; then
    ok K-1 "kernel $kver >= $S3FS_MIN_KERNEL (bpf_arena, BTF_KFUNCS_START)"
else
    ng K-1 "kernel $kver < $S3FS_MIN_KERNEL — kmod/s3fs_ebpf_dev.c refuses to build" \
        "Ubuntu: make setup-kernel (mainline $S3FS_MIN_KERNEL) && reboot" \
        "Debian: trixie (6.12) or bookworm-backports linux-image-amd64"
fi
if [[ "$(kernel_series)" == "$S3FS_VERIFIED_KERNEL_SERIES" ]]; then
    ok K-2 "kernel series $(kernel_series) matches verified struct bpf_arena layout"
elif ver_ge "$kver" "$S3FS_MIN_KERNEL"; then
    wn K-2 "kernel series $(kernel_series) != $S3FS_VERIFIED_KERNEL_SERIES — struct bpf_arena mirror in kmod/s3fs_ebpf_dev.c is unverified" \
        "builds, but check arena reads (make test-arena) before trusting results"
fi
if [[ -r /sys/kernel/btf/vmlinux ]]; then
    ok K-3 "/sys/kernel/btf/vmlinux readable (CONFIG_DEBUG_INFO_BTF=y)"
else
    ng K-3 "/sys/kernel/btf/vmlinux missing — cannot generate bpf/vmlinux.h" \
        "boot a kernel with CONFIG_DEBUG_INFO_BTF=y (all Ubuntu/Debian stock kernels do)"
fi

# -----------------------------------------------------------------------------
section "kernel headers (KDIR=$KDIR)"
if [[ -f "$KDIR/Makefile" ]]; then
    ok H-1 "KDIR present"
else
    hint="$SETUP headers"
    is_ubuntu_mainline && hint="$hint  (mainline kernel: fetched from kernel.ubuntu.com)"
    ng H-1 "KDIR missing — out-of-tree kmod build impossible" "$hint" "or: make KDIR=/path/to/linux-headers"
fi
if [[ -f "$KDIR/vmlinux" ]]; then
    ok H-2 "\$KDIR/vmlinux present (module BTF + resolve_btfids will run)"
elif [[ -f "$KDIR/Makefile" ]]; then
    ng H-2 "\$KDIR/vmlinux missing — kbuild skips module BTF, insmod fails with 'failed to register kfunc set'" \
        "$SETUP kbuild   (extracts /boot/vmlinuz-$KREL; redo after every kernel change)"
fi
if [[ -n "$(kconfig GCC_VERSION)$(kconfig CC_IS_CLANG)" ]]; then
    ok H-3 "kernel .config readable"
else
    wn H-3 "kernel .config not found in KDIR or /boot — gcc/pahole version matching is a guess"
fi

# -----------------------------------------------------------------------------
section "compilers"
clang=$(detect_clang); clang_ok=$?
cm=$(clang_major "$clang")
if [[ $clang_ok -eq 0 && -n "$cm" && "$cm" -ge "$S3FS_MIN_CLANG" ]]; then
    ok C-1 "CLANG=$clang (v$cm >= $S3FS_MIN_CLANG, -mcpu=v3 addr_space_cast)"
else
    ng C-1 "no clang >= $S3FS_MIN_CLANG (found: $clang ${cm:+v$cm}) — bpf/*.bpf.c fails on __arena casts" \
        "$SETUP clang   (Ubuntu noble: clang-20 from noble-updates; Debian bookworm: apt.llvm.org)" \
        "or: make CLANG=clang-NN"
fi
strip="llvm-strip"; [[ "$clang" == clang-* ]] && strip="llvm-strip-${clang#clang-}"
if command -v "$strip" >/dev/null 2>&1; then
    ok C-2 "$strip present (bpf2go, make daemon-gen)"
else
    wn C-2 "$strip missing — only 'make daemon-gen' needs it" "$SETUP clang   (installs llvm-$cm)"
fi

if kernel_cc_is_clang; then
    wn C-3 "kernel was built with clang — kmod build needs LLVM=1, not wired into kmod/Makefile"
else
    want=$(kernel_gcc_major)
    kcc=$(detect_kmod_cc)
    have=$(gcc_major "$kcc")
    # Checked here rather than trusting detect_kmod_cc, so an explicit
    # KMOD_CC= (or the Makefile's fallback name) is held to the same floor.
    floor=$(( ${want:-0} < S3FS_MIN_KMOD_GCC ? ${want:-0} : S3FS_MIN_KMOD_GCC ))
    if [[ -z "$have" || "$have" -lt "$floor" ]]; then
        ng C-3 "no usable gcc for kmod (KMOD_CC=$kcc${have:+ is gcc-$have}; kernel built with gcc-${want:-?}, need >= $floor)" \
            "$SETUP kbuild   (installs gcc-$want or gcc-$S3FS_MIN_KMOD_GCC)"
    elif [[ -n "$want" && "$have" != "$want" ]]; then
        wn C-3 "KMOD_CC=$kcc (gcc-$have) but kernel was built with gcc-$want — builds fine; kbuild prints 'compiler differs'" \
            "gcc-$have lacks newer attributes (e.g. counted_by); Makefile adds KCFLAGS=$(kmod_kcflags "$kcc") to silence that noise" \
            "exact match: install gcc-$want if your distro has it"
    else
        ok C-3 "KMOD_CC=$kcc (gcc-$have, kernel built with gcc-${want:-?})"
    fi
fi

pwant=$(kernel_pahole_version)
pahole=$(detect_pahole)
pv=$(pahole_version "$pahole" "$(pahole_libdir "$pahole")")
if [[ -n "$pv" ]] && { [[ -z "$pwant" ]] || [[ "$pv" -ge "$pwant" ]]; }; then
    ok C-4 "KMOD_PAHOLE=$pahole ($(pahole_fmt "$pv") >= kernel's $(pahole_fmt "$pwant" any))"
else
    ng C-4 "no pahole >= $(pahole_fmt "$pwant" any) (have: $(pahole_fmt "$pv")) — module BTF silently empty" \
        "$SETUP kbuild   (builds dwarves into $PAHOLE_PREFIX)"
fi

# -----------------------------------------------------------------------------
section "tools"
bpftool=$(detect_bpftool)
if "$bpftool" version >/dev/null 2>&1; then
    ok T-1 "BPFTOOL=$bpftool"
else
    ng T-1 "no working bpftool (Ubuntu wrapper fails on mainline kernels; Debian puts it in /usr/sbin)" \
        "$SETUP bpftool   or: make BPFTOOL=/path/to/bpftool"
fi
gwant=$(go_required_version)
gbin=$(detect_go); ghave=$(go_installed_version)
gpath=$(command -v go 2>/dev/null || true)
if [[ -z "$ghave" ]]; then
    ng T-2 "go not found (daemon/go.mod wants $gwant)" "$SETUP go"
elif ver_ge "$ghave" "$gwant"; then
    ok T-2 "GO=$gbin ($ghave >= $gwant, daemon/go.mod)"
    if [[ -n "$gpath" && "$gpath" != "$gbin" ]]; then
        wn T-3 "\`go\` on PATH is $gpath ($(go_version "$gpath")) — make uses GO=$gbin, interactive shells do not" \
            "put $(dirname "$gbin") before $(dirname "$gpath") in PATH (shell rc / mise / asdf), or remove the old go"
    fi
else
    wn T-2 "go $ghave ($gbin) < $gwant — relies on GOTOOLCHAIN auto-download (fails offline)" "$SETUP go"
fi
[[ -n "$gpath" || -z "$ghave" ]] \
    || wn T-3 "$gbin is not on PATH (make still uses it)" "export PATH=$(dirname "$gbin"):\$PATH"

# -----------------------------------------------------------------------------
if [[ "$MODE" == all ]]; then
    section "bench / runtime"
    missing=()
    for t in hyperfine fio s3fs python3 curl; do
        command -v "$t" >/dev/null 2>&1 || missing+=("$t")
    done
    if [[ ${#missing[@]} -eq 0 ]]; then
        ok B-1 "hyperfine fio s3fs python3 curl"
    else
        wn B-1 "missing: ${missing[*]}" "$SETUP bench   (make setup-bench-env)"
    fi
    if ! command -v docker >/dev/null 2>&1; then
        wn B-2 "docker missing (MinIO / Prometheus / Grafana)" "$SETUP packages"
    elif docker compose version >/dev/null 2>&1; then
        ok B-2 "docker compose available"
    else
        wn B-2 "'docker compose' plugin missing" \
            "$SETUP packages   (docker-compose-v2 / docker-compose-plugin / docker-compose)"
    fi
    if [[ -s /etc/passwd-s3fs ]]; then
        ok B-3 "/etc/passwd-s3fs present"
    else
        wn B-3 "/etc/passwd-s3fs missing — s3fs-fuse A/B mounts fail" "$SETUP bench"
    fi
    if docker inspect -f '{{.State.Running}}' s3fs-ebpf-minio 2>/dev/null | grep -q true; then
        ok B-4 "MinIO container running"
    else
        wn B-4 "s3fs-ebpf-minio not running" "make compose-up"
    fi
fi

# -----------------------------------------------------------------------------
if [[ $QUIET != 1 ]]; then
    section "resolved toolchain (scripts/detect-toolchain.sh)"
    "$(dirname "${BASH_SOURCE[0]}")/detect-toolchain.sh" | sed 's/^/  /'
fi

if [[ $QUIET != 1 || $((n_ng + n_warn)) -gt 0 ]]; then
    printf '\n%d NG, %d WARN%s\n' "$n_ng" "$n_warn" "${ONLY:+ (checks gating $ONLY)}"
    [[ ${#FIX_STEPS[@]} -gt 0 ]] && printf 'fix all: %s %s\n' "$SETUP" "${FIX_STEPS[*]}"
fi
[[ $n_ng -eq 0 ]]
