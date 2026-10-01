#!/usr/bin/env bash
# setup-week1-runtime.sh — prepare Ubuntu 24.04 to build+load kmod against a
# Ubuntu mainline kernel (tested on 6.12.90-061290-generic).
#
# Reproduces the 5 workarounds documented in
# docs/journal/week1-build-runtime-notes.md. Idempotent: each step no-ops when the
# fix is already in place. Review before running — several steps mutate
# system state (see the doc's "副作用の永続性まとめ" section).
#
# Usage:
#   ./scripts/setup-week1-runtime.sh          # runs all steps
#   DRY_RUN=1 ./scripts/setup-week1-runtime.sh  # print actions only
#
# Exit code: 0 on success, non-zero if any step fails or user declines a
# destructive confirmation.

set -euo pipefail

KREL="$(uname -r)"
KDIR="/lib/modules/${KREL}/build"
PAHOLE_PREFIX="${PAHOLE_PREFIX:-$HOME/.local}"
PAHOLE_VER="${PAHOLE_VER:-v1.30}"
DRY_RUN="${DRY_RUN:-0}"

log()  { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN\033[0m %s\n' "$*" >&2; }
run()  { if [[ "$DRY_RUN" == 1 ]]; then printf '  [dry-run] %s\n' "$*"; else eval "$@"; fi; }

confirm() {
    local prompt="$1"
    if [[ "$DRY_RUN" == 1 ]]; then return 0; fi
    read -rp "$prompt [y/N] " ans
    [[ "$ans" =~ ^[Yy]$ ]]
}

# -----------------------------------------------------------------------------
# Step 1: repair dpkg (課題 1)
# -----------------------------------------------------------------------------
step_dpkg_repair() {
    log "Step 1: check for broken dpkg entries blocking apt"
    local broken
    broken="$(dpkg -l 2>/dev/null | awk '$1 ~ /^(iH|iF|iU|iHR)/ && $2 ~ /^linux-(image|headers)/ {print $2}')"
    if [[ -z "$broken" ]]; then
        echo "  no broken linux-* packages"
        return
    fi
    warn "broken kernel packages found:"
    echo "$broken" | sed 's/^/    /'
    warn "these entries have no apt candidate — only --force-remove-reinstreq removes them."
    warn "running kernel (${KREL}) is not affected."
    if ! confirm "purge the above packages?"; then
        echo "  skipped — apt install will fail until these are removed."
        return
    fi
    # shellcheck disable=SC2086
    run "sudo dpkg --purge --force-remove-reinstreq $broken"
}

# -----------------------------------------------------------------------------
# Step 2: install gcc matching kernel build (課題 2)
# -----------------------------------------------------------------------------
step_gcc() {
    log "Step 2: install gcc version matching kernel build"
    local kbuild_cc want_ver have
    kbuild_cc="$(awk -F'"' '/CONFIG_CC_VERSION_TEXT/ {print $2}' "${KDIR}/.config" 2>/dev/null || true)"
    if [[ -z "$kbuild_cc" ]]; then
        warn "cannot read ${KDIR}/.config — skipping gcc check"
        return
    fi
    want_ver="$(sed -nE 's/.*gcc-([0-9]+).*/\1/p' <<<"$kbuild_cc" | head -1)"
    echo "  kernel was built with: $kbuild_cc"
    echo "  wanted gcc major:      $want_ver"
    if command -v "gcc-${want_ver}" >/dev/null; then
        echo "  gcc-${want_ver} already installed"
        return
    fi
    # gcc-15 is not in noble; fall back to gcc-14 (has -fmin-function-alignment)
    if [[ "$want_ver" -ge 15 ]] && ! apt-cache show "gcc-${want_ver}" >/dev/null 2>&1; then
        warn "gcc-${want_ver} not in apt (noble has up to gcc-14)"
        warn "falling back to gcc-14 — you MUST pass CC=gcc-14 HOSTCC=gcc-14 to make."
        run "sudo apt-get install -y gcc-14"
        return
    fi
    run "sudo apt-get install -y gcc-${want_ver}"
}

# -----------------------------------------------------------------------------
# Step 3: build pahole >= kernel's CONFIG_PAHOLE_VERSION (課題 3)
# -----------------------------------------------------------------------------
query_pahole_ver() {
    # Prints the two-digit encoded version (e.g. 125, 130) or nothing on failure.
    local bin="$1" libdir="${2:-}"
    local out
    out="$(LD_LIBRARY_PATH="$libdir" "$bin" --version 2>/dev/null | sed -nE 's/v([0-9]+)\.([0-9]+).*/\1\2/p' || true)"
    printf '%s' "$out"
}

step_pahole() {
    log "Step 3: ensure pahole matches kernel build version"
    local want sys_ver local_ver
    want="$(awk -F= '/CONFIG_PAHOLE_VERSION/ {print $2}' "${KDIR}/.config" 2>/dev/null || echo 0)"
    sys_ver="$(query_pahole_ver /usr/bin/pahole)"
    local_ver="$(query_pahole_ver "${PAHOLE_PREFIX}/bin/pahole" "${PAHOLE_PREFIX}/lib")"
    echo "  kernel expects pahole >= 1.$((want - 100)) (encoded as ${want})"
    echo "  /usr/bin/pahole:            ${sys_ver:-not installed}"
    echo "  ${PAHOLE_PREFIX}/bin/pahole: ${local_ver:-not installed}"
    if [[ -n "$sys_ver" && "$sys_ver" -ge "$want" ]]; then
        echo "  system pahole is new enough — nothing to do"
        return
    fi
    if [[ -n "$local_ver" && "$local_ver" -ge "$want" ]]; then
        echo "  local pahole is new enough — nothing to do (use LD_LIBRARY_PATH=${PAHOLE_PREFIX}/lib)"
        return
    fi
    warn "building pahole ${PAHOLE_VER} from source into ${PAHOLE_PREFIX}"
    run "sudo apt-get install -y libelf-dev libdw-dev libbpf-dev libzstd-dev cmake gcc git"
    local build; build="$(mktemp -d)"
    trap 'rm -rf "$build"' RETURN
    run "git clone --depth 1 --branch ${PAHOLE_VER} https://github.com/acmel/dwarves.git ${build}/dwarves"
    run "cmake -S ${build}/dwarves -B ${build}/dwarves/build -D__LIB=lib -DCMAKE_INSTALL_PREFIX=${PAHOLE_PREFIX}"
    run "make -C ${build}/dwarves/build -j\$(nproc)"
    run "make -C ${build}/dwarves/build install"
    echo "  installed. Use with: LD_LIBRARY_PATH=${PAHOLE_PREFIX}/lib PAHOLE=${PAHOLE_PREFIX}/bin/pahole make ..."
}

# -----------------------------------------------------------------------------
# Step 4: place vmlinux inside KDIR for kbuild BTF pipeline (課題 4)
# -----------------------------------------------------------------------------
step_vmlinux() {
    log "Step 4: place decompressed vmlinux in KDIR"
    if [[ -f "${KDIR}/vmlinux" ]]; then
        echo "  ${KDIR}/vmlinux already present ($(stat -c%s "${KDIR}/vmlinux") bytes)"
        return
    fi
    local vmlinuz="/boot/vmlinuz-${KREL}"
    local extract="${KDIR}/scripts/extract-vmlinux"
    if [[ ! -f "$vmlinuz" ]]; then
        warn "$vmlinuz not found — cannot extract vmlinux"
        return 1
    fi
    if [[ ! -x "$extract" ]]; then
        warn "$extract not found — install linux-headers-${KREL} first"
        return 1
    fi
    run "sudo ${extract} ${vmlinuz} | sudo tee ${KDIR}/vmlinux >/dev/null"
    run "sudo chmod 644 ${KDIR}/vmlinux"
    echo "  placed. Kbuild's cmd_btf_ko will now run pahole + resolve_btfids."
}

# -----------------------------------------------------------------------------
# Step 5 (課題 5): the source-level fix (BTF_SET8 → BTF_KFUNCS) is already
# committed in kmod/tree_traversal.c. No runtime step here.
# -----------------------------------------------------------------------------

# -----------------------------------------------------------------------------
main() {
    if [[ "$DRY_RUN" == 1 ]]; then
        warn "DRY_RUN=1 — no changes will be made"
    fi
    if [[ ! -d "$KDIR" ]]; then
        warn "KDIR $KDIR does not exist. Run scripts/setup-kernel-6.12.sh first."
        exit 1
    fi
    step_dpkg_repair
    step_gcc
    step_pahole
    step_vmlinux

    cat <<EOF

==> All steps complete. To build and run Week 1 verification:

    export LD_LIBRARY_PATH=${PAHOLE_PREFIX}/lib
    make CC=gcc-14 HOSTCC=gcc-14 PAHOLE=${PAHOLE_PREFIX}/bin/pahole test-arena

See docs/journal/week1-build-runtime-notes.md for background and alternatives.
EOF
}

main "$@"
