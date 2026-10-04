#!/usr/bin/env bash
# setup-toolchain.sh — install / repair everything `make all` and the
# benches depend on. Tested on Ubuntu 24.04 (+ mainline 6.12 kernel) and
# Debian. Each step is idempotent and skips itself when `make doctor`
# would already pass for it.
#
# Usage:
#   scripts/setup-toolchain.sh [step...]     # default: all
#   DRY_RUN=1    scripts/setup-toolchain.sh  # print actions only
#   ASSUME_YES=1 scripts/setup-toolchain.sh  # accept prompts (non-tty)
#
# Steps (doctor IDs in brackets, see docs/dev-notes/build-env-dependencies.md):
#   packages   base build packages + docker / compose            [B-2]
#   clang      clang >= 19 (+ matching llvm-strip)               [C-1 C-2]
#   bpftool    a bpftool binary that actually runs               [T-1]
#   headers    /lib/modules/$(uname -r)/build                    [H-1]
#   kbuild     dpkg repair, gcc / pahole match, KDIR/vmlinux     [C-3 C-4 H-2]
#   go         Go toolchain matching daemon/go.mod               [T-2]
#   bench      hyperfine fio s3fs + /etc/passwd-s3fs             [B-1 B-3]
#   docker-group  add $USER to the docker group
#
# After a kernel change re-run: scripts/setup-toolchain.sh headers kbuild
set -euo pipefail

SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/toolchain.sh
source "$SCRIPTS_DIR/lib/toolchain.sh"
# shellcheck source=lib/mainline.sh
source "$SCRIPTS_DIR/lib/mainline.sh"

# Pre-refactor name for "accept the mainline header download without a tty".
# shellcheck disable=SC2034  # read by confirm() in lib/common.sh
[[ -n "${S3FS_INSTALL_MAINLINE_HEADERS:-}" ]] && ASSUME_YES=1

ARCH_DEB=$(dpkg --print-architecture 2>/dev/null || echo amd64)
APT_UPDATED=0
apt_update_once() {
    [[ $APT_UPDATED == 1 ]] && return 0
    run sudo apt-get update
    APT_UPDATED=1
}

# -----------------------------------------------------------------------------
step_packages() {
    log "packages: base build + docker ($(os_id) $(os_codename))"
    apt_update_once
    local pkgs=(build-essential make git curl jq pkg-config llvm libbpf-dev libelf-dev)
    local p
    # BCC tools are optional (ad-hoc tracing only); not every release has them.
    for p in bpfcc-tools python3-bpfcc; do
        apt_has_candidate "$p" && pkgs+=("$p")
    done
    # Do not pull docker.io over an existing docker-ce install (they conflict).
    command -v docker >/dev/null 2>&1 || pkgs+=(docker.io)
    if ! docker compose version >/dev/null 2>&1; then
        # Ubuntu: docker-compose-v2. Debian trixie: docker-compose (v2 plugin).
        # Docker's own repo: docker-compose-plugin. bookworm's docker-compose
        # is v1 (python) and does NOT provide `docker compose`.
        if p=$(apt_first_candidate docker-compose-v2 docker-compose-plugin docker-compose); then
            pkgs+=("$p")
        fi
    fi
    apt_install "${pkgs[@]}"
    if [[ "$DRY_RUN" != 1 ]] && ! docker compose version >/dev/null 2>&1; then
        warn "'docker compose' still unavailable — add Docker's apt repo and install docker-compose-plugin:"
        info "https://docs.docker.com/engine/install/$(os_id)/"
    fi
}

# -----------------------------------------------------------------------------
step_clang() {
    log "clang >= $S3FS_MIN_CLANG (bpf -mcpu=v3 / __BPF_FEATURE_ADDR_SPACE_CAST)"
    local c v
    if c=$(detect_clang); then
        info "using $c (v$(clang_major "$c"))"
        v=$(clang_major "$c")
        if [[ "$c" == clang-* ]] && ! command -v "llvm-strip-$v" >/dev/null 2>&1 \
            && apt_has_candidate "llvm-$v"; then
            apt_update_once; apt_install "llvm-$v"     # llvm-strip-N for bpf2go
        fi
        return 0
    fi
    apt_update_once
    for v in $(seq 30 -1 "$S3FS_MIN_CLANG"); do
        if apt_has_candidate "clang-$v"; then
            # noble-updates: clang-19/20; trixie: clang-19.
            apt_install "clang-$v" "llvm-$v"
            return 0
        fi
    done
    warn "no clang >= $S3FS_MIN_CLANG in apt (e.g. Debian bookworm ships 14-16)."
    info "apt.llvm.org provides it:"
    info "  curl -fsSLO https://apt.llvm.org/llvm.sh && sudo bash llvm.sh 20"
    if confirm "run the apt.llvm.org installer for clang-20 now?"; then
        local tmp; tmp=$(mktemp -d)
        run curl -fsSL -o "$tmp/llvm.sh" https://apt.llvm.org/llvm.sh
        run sudo bash "$tmp/llvm.sh" 20
        rm -rf "$tmp"
    else
        warn "skipped — bpf build fails until a clang >= $S3FS_MIN_CLANG is installed (or make CLANG=...)."
    fi
}

# -----------------------------------------------------------------------------
step_bpftool() {
    log "bpftool"
    local b
    if b=$(detect_bpftool); then info "using $b"; return 0; fi
    apt_update_once
    if apt_has_candidate bpftool; then
        # Debian (and Ubuntu >= 25.04): standalone package in /usr/sbin.
        apt_install bpftool
    elif apt_has_candidate "linux-tools-$KREL"; then
        apt_install linux-tools-common "linux-tools-$KREL"
    else
        # Ubuntu + mainline kernel: no linux-tools-<krel>. /usr/sbin/bpftool
        # is a uname -r dispatching wrapper that fails, so expose the stock
        # generic package's binary (BTF dump is kernel-version agnostic).
        info "linux-tools-$KREL not in apt (mainline kernel?); using linux-tools-generic"
        apt_install linux-tools-common linux-tools-generic
        local real
        # shellcheck disable=SC2012
        real=$(ls -1dv /usr/lib/linux-tools/*/bpftool 2>/dev/null | tail -n1 || true)
        if [[ -n "$real" ]]; then
            run sudo ln -sf "$real" /usr/local/bin/bpftool
            info "symlinked $real -> /usr/local/bin/bpftool"
        fi
    fi
    if [[ "$DRY_RUN" != 1 ]]; then
        b=$(detect_bpftool) || warn "still no working bpftool; set BPFTOOL=/path/to/bpftool"
        info "bpftool: $b"
    fi
}

# -----------------------------------------------------------------------------
step_headers() {
    log "kernel headers for $KREL"
    if [[ -f "$KDIR/Makefile" ]]; then info "$KDIR present"; return 0; fi
    apt_update_once
    if apt_has_candidate "linux-headers-$KREL"; then
        apt_install "linux-headers-$KREL"
        return 0
    fi
    if ! is_ubuntu_mainline; then
        warn "linux-headers-$KREL is not installable and $KREL is not a kernel.ubuntu.com mainline build."
        info "install headers for the running kernel by hand, or build with make KDIR=/path/to/headers"
        return 1
    fi
    local vdir base debs
    vdir=$(mainline_dir_for_krel "$KREL")
    base=${KREL%-generic}                        # 6.12.0-061200
    mapfile -t debs < <(mainline_list_debs "$vdir" \
        "^linux-headers-${base//./\\.}(-generic)?_.*_(all|${ARCH_DEB})\\.deb$")
    if [[ ${#debs[@]} -eq 0 ]]; then
        warn "no headers for $base in ${MAINLINE_URL}/${vdir}/${ARCH_DEB}/ — install manually"
        return 1
    fi
    info "mainline kernel: headers are only on ${MAINLINE_URL}/${vdir}/${ARCH_DEB}/"
    printf '    %s\n' "${debs[@]}" >&2
    if confirm "download and dpkg -i the above?"; then
        mainline_fetch_install "$vdir" "${debs[@]}"
    else
        warn "skipped — kmod build fails until $KDIR exists."
    fi
}

# -----------------------------------------------------------------------------
# kbuild: everything between "headers installed" and "insmod registers the
# kfunc set". These must be redone after every kernel change.

kbuild_dpkg_repair() {
    # A half-installed mainline kernel (iHR / iF / iU) blocks every apt
    # install with "needs to be reinstalled" and has no apt candidate.
    local broken
    broken=$(dpkg -l 2>/dev/null | awk '$1 ~ /^(iH|iF|iU|iHR)/ && $2 ~ /^linux-(image|headers|modules)/ {print $2}')
    [[ -z "$broken" ]] && return 0
    warn "broken kernel packages block apt:"
    # shellcheck disable=SC2001
    sed 's/^/    /' <<<"$broken" >&2
    info "running kernel ($KREL) is not affected; only --force-remove-reinstreq can remove them."
    if confirm "purge the above packages?"; then
        # shellcheck disable=SC2086
        run sudo dpkg --purge --force-remove-reinstreq $broken
    else
        warn "skipped — apt-get install will keep failing."
    fi
}

kbuild_gcc() {
    if kernel_cc_is_clang; then
        warn "kernel built with clang; kmod/Makefile needs LLVM=1 (not wired). Skipping gcc."
        return 0
    fi
    local want c pkg
    want=$(kernel_gcc_major)
    c=$(detect_kmod_cc) && [[ "$(gcc_major "$c")" == "$want" ]] && { info "KMOD_CC=$c (exact match gcc-$want)"; return 0; }
    # Ubuntu mainline 6.12 is built with gcc-15 which noble lacks; gcc-14 is
    # the oldest that understands -fmin-function-alignment.
    if pkg=$(apt_first_candidate "gcc-$want" "gcc-$S3FS_MIN_KMOD_GCC"); then
        apt_update_once; apt_install "$pkg"
        [[ "$pkg" == "gcc-$want" ]] || warn "gcc-$want not in apt; using $pkg (Makefile passes CC=$pkg automatically)"
    elif detect_kmod_cc >/dev/null; then
        info "KMOD_CC=$c (gcc-$want unavailable, $c is compatible)"
    else
        warn "no gcc >= $S3FS_MIN_KMOD_GCC available in apt; kmod build will fail"
    fi
}

kbuild_pahole() {
    local want p tag build
    want=$(kernel_pahole_version)
    if p=$(detect_pahole); then info "KMOD_PAHOLE=$p"; return 0; fi
    # Distro first (Debian trixie: 1.30); noble's 1.25 is too old for 1.30 kernels.
    apt_update_once
    apt_install dwarves
    if [[ "$DRY_RUN" != 1 ]] && p=$(detect_pahole); then info "KMOD_PAHOLE=$p"; return 0; fi
    tag="${PAHOLE_VER:-v1.$(( ${want:-130} % 100 < 30 ? 30 : ${want:-130} % 100 ))}"
    warn "distro pahole is older than the kernel's (CONFIG_PAHOLE_VERSION=${want:-?}); building $tag into $PAHOLE_PREFIX"
    apt_install libelf-dev libdw-dev libbpf-dev libzstd-dev cmake gcc git
    build=$(mktemp -d)
    run git clone --depth 1 --branch "$tag" https://github.com/acmel/dwarves.git "$build/dwarves"
    run cmake -S "$build/dwarves" -B "$build/dwarves/build" -D__LIB=lib -DCMAKE_INSTALL_PREFIX="$PAHOLE_PREFIX"
    run make -C "$build/dwarves/build" -j"$(nproc)"
    run make -C "$build/dwarves/build" install
    rm -rf "$build"
    info "installed; the Makefile picks it up with LD_LIBRARY_PATH=$PAHOLE_PREFIX/lib automatically"
}

kbuild_vmlinux() {
    # Distro header packages omit the vmlinux ELF; kbuild then prints
    # "Skipping BTF generation ... due to unavailability of vmlinux" and the
    # module's kfunc set fails to register (-22 / missing BTF) at insmod.
    if [[ -f "$KDIR/vmlinux" ]]; then info "$KDIR/vmlinux present"; return 0; fi
    [[ -f "$KDIR/Makefile" ]] || { warn "$KDIR missing — run the headers step first"; return 1; }
    local vmlinuz="/boot/vmlinuz-$KREL" extract="" c
    for c in "$KDIR/scripts/extract-vmlinux" \
             /usr/src/linux-headers-"${KREL%-*}"*/scripts/extract-vmlinux \
             /usr/lib/linux-kbuild-"$(kernel_series)"/scripts/extract-vmlinux \
             "$(command -v extract-vmlinux 2>/dev/null || true)"; do
        [[ -n "$c" && -x "$c" ]] && { extract=$c; break; }
    done
    [[ -n "$extract" ]] || { warn "extract-vmlinux not found (linux-headers / linux-kbuild)"; return 1; }
    sudo test -f "$vmlinuz" || { warn "$vmlinuz not found"; return 1; }
    info "extracting $vmlinuz -> $KDIR/vmlinux (with $extract)"
    run "sudo $extract $vmlinuz | sudo tee $KDIR/vmlinux >/dev/null"
    run sudo chmod 644 "$KDIR/vmlinux"
}

step_kbuild() {
    log "kbuild prerequisites for $KREL (gcc / pahole / vmlinux)"
    kbuild_dpkg_repair
    kbuild_gcc
    kbuild_pahole
    kbuild_vmlinux
}

# -----------------------------------------------------------------------------
step_go() {
    local want have g
    want=$(go_required_version); have=$(go_installed_version)
    log "Go >= $want (daemon/go.mod)"
    if [[ -n "$have" ]] && ver_ge "$have" "$want"; then
        info "GO=$(detect_go) ($have)"
    else
        [[ -n "$have" ]] && info "go $have is older than $want; installing go$want into /usr/local/go"
        local url="https://go.dev/dl/go${want}.linux-${ARCH_DEB}.tar.gz"
        run sudo rm -rf /usr/local/go
        run "curl -fsSL $url | sudo tar -C /usr/local -xz"
        run "echo 'export PATH=/usr/local/go/bin:\$PATH' | sudo tee /etc/profile.d/go.sh >/dev/null"
    fi
    # The Makefile resolves GO itself (detect_go), but an older `go` earlier
    # on PATH still wins in interactive shells — say where it comes from.
    g=$(command -v go 2>/dev/null || true)
    if [[ "$DRY_RUN" != 1 && -n "$g" && "$g" != "$(detect_go)" ]]; then
        warn "\`go\` on PATH is $g ($(go_version "$g")); make uses $(detect_go)."
        info "for interactive use put $(dirname "$(detect_go)") first in PATH (check ~/.bashrc, ~/.profile, mise/asdf)"
    fi
}

# -----------------------------------------------------------------------------
step_bench() {
    log "bench tools (hyperfine / fio / s3fs-fuse)"
    apt_update_once
    apt_install hyperfine fio s3fs python3 curl
    # bench-*.sh mount s3fs-fuse for A/B comparison with
    # passwd_file=/etc/passwd-s3fs. Same dev-only creds as deploy/docker-compose.yml.
    if [[ -s /etc/passwd-s3fs ]]; then
        info "/etc/passwd-s3fs already present, leaving as-is"
    else
        run "echo 'minioadmin:minioadmin' | sudo tee /etc/passwd-s3fs >/dev/null"
        run sudo chmod 600 /etc/passwd-s3fs
        info "wrote /etc/passwd-s3fs (minioadmin:minioadmin, 0600)"
    fi
}

step_docker_group() {
    log "docker group"
    local user=${USER:-$(id -un)}
    if id -nG "$user" 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
        info "$user already in docker group"; return 0
    fi
    run sudo usermod -aG docker "$user" || true
    info "log out and back in for the docker group to take effect"
}

# -----------------------------------------------------------------------------
ALL_STEPS=(packages clang bpftool headers kbuild go bench docker-group)

main() {
    [[ "$DRY_RUN" == 1 ]] && warn "DRY_RUN=1 — no changes will be made"
    case "$(os_id)" in
        ubuntu|debian) ;;
        *) die "unsupported distro: $(os_id) (apt-based Ubuntu / Debian only)" ;;
    esac
    local steps=("$@") s rc=0
    [[ ${#steps[@]} -eq 0 || "${steps[0]}" == all ]] && steps=("${ALL_STEPS[@]}")
    for s in "${steps[@]}"; do
        case "$s" in
            packages|clang|bpftool|headers|kbuild|go|bench|docker-group)
                "step_${s//-/_}" || { warn "step '$s' did not complete"; rc=1; } ;;
            -h|--help) sed -n '2,25p' "$0"; return 0 ;;
            *) die "unknown step: $s (one of: ${ALL_STEPS[*]})" ;;
        esac
    done
    log "verify with: make doctor"
    [[ "$DRY_RUN" == 1 ]] || "$SCRIPTS_DIR/doctor.sh" --build || true
    return $rc
}

main "$@"
