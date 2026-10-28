#!/usr/bin/env bash
# setup-kernel-lockdep.sh — build and install a Linux 6.12 kernel with lockdep
# (CONFIG_PROVE_LOCKING=y) for `scripts/test-lockdep.sh` (AC-42).
#
# The distro / mainline-PPA kernels (scripts/setup-kernel-6.12.sh) have
# PROVE_LOCKING off, so lock-order bugs in kmod/vfs_ops.c cannot show up there.
# This builds kernel.org's tarball with the running kernel's config plus the
# lock-debugging options, as Debian packages (bindeb-pkg), and installs the
# image and headers.  It does NOT touch the running kernel's files and the new
# entry is selected by you in grub (see the end of the output).
#
#   scripts/setup-kernel-lockdep.sh                  # download, configure, build, install
#   PREPARE_ONLY=1 scripts/setup-kernel-lockdep.sh   # download + configure, stop (check .config)
#   BUILD_ONLY=1   scripts/setup-kernel-lockdep.sh   # build the debs, do not install
#   DRY_RUN=1      scripts/setup-kernel-lockdep.sh   # print the steps only
#
# Environment:
#   VERSION=6.12          kernel.org version (linux-$VERSION.tar.xz).  Use the
#                         same series as `uname -r` (>= 6.12 is required).
#   SRC_DIR=$HOME/src/linux-lockdep   where the tree is unpacked and built
#   BASE_CONFIG=/boot/config-$(uname -r)   config to start from
#   LOCALMOD=1            keep only the modules loaded now (make localmodconfig).
#                         Start docker / MinIO and mount s3fs_ebpf once before, so the
#                         modules they need (overlay, veth, bridge, netfilter...) are loaded.
#                         LOCALMOD=0 builds every module of the base config.
#   MIN_FREE_GB=15        refuse to start with less free space in SRC_DIR (40 if LOCALMOD=0)
#   JOBS=$(nproc)         make -j
#   LOCALVERSION=-s3fs-lockdep        suffix of the new kernel (uname -r)
#   ASSUME_YES=1          do not ask before installing
#
# Time and space: with LOCALMOD=1 (default) about 20-40 minutes and 10-15 GB; with
# LOCALMOD=0 (every distro module) 60-120 minutes and 40+ GB (debug info is on:
# BTF needs it).  The build stops early if SRC_DIR has less than MIN_FREE_GB.
# Run it on the machine that will boot the kernel (the kmod is built against
# its headers); reboot afterwards, then run `make setup-toolchain` and
# `scripts/test-lockdep.sh`.  docs/dev-notes/lockdep-kernel.md has the details
# and how to go back to the normal kernel.
set -euo pipefail

# shellcheck source=lib/toolchain.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/toolchain.sh"

VERSION="${VERSION:-6.12}"
SRC_DIR="${SRC_DIR:-$HOME/src/linux-lockdep}"
BASE_CONFIG="${BASE_CONFIG:-/boot/config-$(uname -r)}"
JOBS="${JOBS:-$(nproc)}"
LOCALVERSION="${LOCALVERSION:--s3fs-lockdep}"
LOCALMOD="${LOCALMOD:-1}"
if [[ "$LOCALMOD" == 1 ]]; then MIN_FREE_GB="${MIN_FREE_GB:-15}"; else MIN_FREE_GB="${MIN_FREE_GB:-40}"; fi
PREPARE_ONLY="${PREPARE_ONLY:-0}"
BUILD_ONLY="${BUILD_ONLY:-0}"
MAJOR="${VERSION%%.*}"
TARBALL="linux-${VERSION}.tar.xz"
BASE_URL="https://cdn.kernel.org/pub/linux/kernel/v${MAJOR}.x"
TREE="$SRC_DIR/linux-$VERSION"

ver_ge "$VERSION" "$S3FS_MIN_KERNEL" || die "VERSION=$VERSION: the kmod needs >= $S3FS_MIN_KERNEL"
[[ -r "$BASE_CONFIG" ]] || die "no base config at $BASE_CONFIG (set BASE_CONFIG=)"

# --- prerequisites -----------------------------------------------------------
missing=()
for c in curl tar xz make gcc flex bison bc dpkg-deb fakeroot rsync; do
    command -v "$c" >/dev/null 2>&1 || missing+=("$c")
done
for h in /usr/include/openssl/ssl.h /usr/include/libelf.h; do
    [[ -e "$h" ]] || missing+=("$h")
done
if ((${#missing[@]})); then
    warn "missing: ${missing[*]}"
    info "sudo apt install build-essential flex bison bc libssl-dev libelf-dev rsync fakeroot dpkg-dev debhelper"
    [[ "$DRY_RUN" == 1 || "$PREPARE_ONLY" == 1 ]] || die "install the prerequisites and run again"
fi

PAHOLE=$(detect_pahole) || warn "pahole is older than the base kernel's CONFIG_PAHOLE_VERSION; BTF may be empty (scripts/setup-toolchain.sh kbuild builds one into \$HOME/.local)"
PAHOLE_LIB=$(pahole_libdir "$PAHOLE")
info "pahole: $PAHOLE ${PAHOLE_LIB:+(LD_LIBRARY_PATH=$PAHOLE_LIB)}"
# Kconfig itself runs pahole (CONFIG_PAHOLE_VERSION), so every make in the tree
# needs the working one, not the first on PATH.
KMAKE="make -C $TREE PAHOLE=$PAHOLE"
[[ -n "$PAHOLE_LIB" ]] && KMAKE="LD_LIBRARY_PATH=$PAHOLE_LIB\${LD_LIBRARY_PATH:+:\$LD_LIBRARY_PATH} $KMAKE"

# A half-finished earlier build (it ran out of space) leaves ~20 GB of objects and
# debian/ in the tree.  Remove them before the space check, not after it.
if [[ -e "$TREE/vmlinux" || -d "$TREE/debian" || -e "$TREE/.config" ]]; then
    log "cleaning the previous build in $TREE"
    run "$KMAKE mrproper"
    run rm -rf "$TREE/debian"
fi

mkdir -p "$SRC_DIR"
free_gb=$(df -P --block-size=1G "$SRC_DIR" | awk 'NR==2 { print $4 }')
if ((free_gb < MIN_FREE_GB)); then
    warn "only ${free_gb} GB free in $SRC_DIR (need $MIN_FREE_GB; a full build ran out of space at the module BTF step)"
    info "free space, set SRC_DIR= to a bigger disk, or keep LOCALMOD=1 (default) to build far fewer modules"
    [[ "$DRY_RUN" == 1 || "$PREPARE_ONLY" == 1 ]] || die "not enough disk space"
fi

# --- source ------------------------------------------------------------------
fetch_source() {
    mkdir -p "$SRC_DIR"
    if [[ ! -f "$SRC_DIR/$TARBALL" ]]; then
        log "downloading $BASE_URL/$TARBALL"
        run curl -fL --retry 3 -o "$SRC_DIR/$TARBALL.part" "$BASE_URL/$TARBALL"
        run mv "$SRC_DIR/$TARBALL.part" "$SRC_DIR/$TARBALL"
    fi
    # kernel.org publishes sha256 of the .tar (uncompressed) in sha256sums.asc;
    # the compressed file is checked by decompressing through the same hash.
    if [[ "$DRY_RUN" != 1 ]]; then
        log "verifying $TARBALL"
        local want got
        want=$(curl -fsSL "$BASE_URL/sha256sums.asc" | awk -v f="linux-${VERSION}.tar.xz" '$2 == f { print $1 }' | head -n1)
        if [[ -n "$want" ]]; then
            got=$(sha256sum "$SRC_DIR/$TARBALL" | awk '{print $1}')
            [[ "$want" == "$got" ]] || die "sha256 mismatch for $TARBALL (want $want, got $got); remove it and retry"
            info "sha256 ok"
        else
            warn "no checksum line for $TARBALL in sha256sums.asc; xz integrity only"
            xz -t "$SRC_DIR/$TARBALL"
        fi
    fi
    if [[ ! -d "$TREE" ]]; then
        log "unpacking into $SRC_DIR"
        run tar -C "$SRC_DIR" -xf "$SRC_DIR/$TARBALL"
    fi
}

# --- config ------------------------------------------------------------------
# Lock debugging on top of the running kernel's config.  Kept to what finds
# lock-order, sleeping-in-atomic and RCU misuse; the heavy memory debuggers
# (KASAN, KCSAN) are separate runs (kmod/kunit-kasan.config).
configure() {
    log "configuring $TREE"
    run cp "$BASE_CONFIG" "$TREE/.config"
    if [[ "$LOCALMOD" == 1 ]]; then
        # Only the modules this machine has loaded now (the build is otherwise ~4000 modules).
        command -v docker >/dev/null 2>&1 && ! docker ps >/dev/null 2>&1 && warn "docker is not running: its modules (overlay, veth, br_netfilter...) may be dropped; start it and run again"
        run "lsmod > $SRC_DIR/lsmod.txt"
        # `yes` dies of SIGPIPE when make ends: not a failure (the subshell's status is make's).
        run "( set +o pipefail; yes '' | $KMAKE LSMOD=$SRC_DIR/lsmod.txt localmodconfig )"
    fi
    local sc="$TREE/scripts/config --file $TREE/.config"
    # No distro signing keys / certs in a self-built tree (they are not in the tarball).
    run "$sc --set-str SYSTEM_TRUSTED_KEYS '' --set-str SYSTEM_REVOCATION_KEYS '' --disable MODULE_SIG_FORCE"
    run "$sc --disable DEBUG_INFO_NONE --enable DEBUG_INFO --enable DEBUG_INFO_DWARF5 --enable DEBUG_INFO_BTF --enable DEBUG_INFO_BTF_MODULES"
    # Compressed debug sections: about half the disk.
    run "$sc --disable DEBUG_INFO_COMPRESSED_NONE --enable DEBUG_INFO_COMPRESSED_ZSTD"
    run "$sc --set-str LOCALVERSION '$LOCALVERSION' --disable LOCALVERSION_AUTO"
    # The point of this kernel.
    run "$sc --enable PROVE_LOCKING --enable DEBUG_LOCKDEP --enable DEBUG_LOCK_ALLOC --enable LOCK_STAT"
    run "$sc --enable DEBUG_MUTEXES --enable DEBUG_SPINLOCK --enable DEBUG_RWSEMS --enable DEBUG_ATOMIC_SLEEP"
    run "$sc --enable PROVE_RCU --enable DEBUG_LIST --enable DEBUG_PREEMPT"
    # Report and keep going: a splat must not take the box down in the middle of a run.
    run "$sc --disable BOOTPARAM_HUNG_TASK_PANIC --disable PANIC_ON_OOPS"
    # What the kmod and the daemon need (usually already on).
    run "$sc --enable BPF --enable BPF_SYSCALL --enable BPF_JIT --enable BPF_JIT_DEFAULT_ON --enable BPF_STRUCT_OPS --enable DEBUG_INFO_BTF"
    run "$sc --enable FUSE_FS --enable TRACING --enable KPROBES --enable FPROBE --enable BPF_EVENTS"
    # KUnit built in: the KUNIT=1 kmod can then run its suites under lockdep.
    run "$sc --enable KUNIT --enable KUNIT_DEFAULT_ENABLED"
    run "$KMAKE olddefconfig"
    if [[ "$DRY_RUN" != 1 ]]; then
        local c bad=0
        for c in PROVE_LOCKING DEBUG_LOCK_ALLOC DEBUG_INFO_BTF BPF_JIT BPF_SYSCALL KUNIT; do
            if grep -q "^CONFIG_$c=y" "$TREE/.config"; then
                info "CONFIG_$c=y"
            else
                warn "CONFIG_$c is NOT set after olddefconfig (a dependency is off)"
                bad=1
            fi
        done
        ((bad == 0)) || die "fix the config in $TREE/.config (make -C $TREE menuconfig) and run again"
    fi
}

# --- build / install ---------------------------------------------------------
build() {
    log "building (make -j$JOBS bindeb-pkg); this takes a while"
    # -P...nokerneldbg: skip the linux-image-*-dbg package (vmlinux and every module with full debug info, many GB).
    run "$KMAKE -j$JOBS KDEB_PKGVERSION=1 DPKG_FLAGS=-Ppkg.linux-upstream.nokerneldbg bindeb-pkg"
}

install_debs() {
    local rel debs=()
    if [[ "$DRY_RUN" == 1 ]]; then
        info "[dry-run] dpkg -i $SRC_DIR/linux-image-*${LOCALVERSION}*.deb $SRC_DIR/linux-headers-*${LOCALVERSION}*.deb"
        return 0
    fi
    # bindeb-pkg writes the debs next to the tree.  The -dbg package is several GB: not installed.
    mapfile -t debs < <(ls "$SRC_DIR"/linux-image-*"${LOCALVERSION}"*.deb "$SRC_DIR"/linux-headers-*"${LOCALVERSION}"*.deb 2>/dev/null | grep -v -- '-dbg')
    ((${#debs[@]} >= 2)) || die "no linux-image / linux-headers debs in $SRC_DIR"
    printf '  %s\n' "${debs[@]}" >&2
    confirm "install these packages? (the running kernel is not removed)" || die "aborted"
    sudo dpkg -i "${debs[@]}"
    rel=$(dpkg-deb -f "$(printf '%s\n' "${debs[@]}" | grep linux-image | head -n1)" Package | sed 's/^linux-image-//')
    log "installed kernel $rel"
    # 6.12's headers package leaves out tools/bpf/resolve_btfids/resolve_btfids, which
    # Kbuild runs on every out-of-tree module with BTF ("resolve_btfids: not found").
    local rb="$TREE/tools/bpf/resolve_btfids/resolve_btfids"
    if [[ -x "$rb" ]]; then
        sudo install -D -m 0755 "$rb" "/usr/src/linux-headers-$rel/tools/bpf/resolve_btfids/resolve_btfids"
        info "installed resolve_btfids into /usr/src/linux-headers-$rel"
    else
        warn "$rb not found; copy it into /usr/src/linux-headers-$rel/tools/bpf/resolve_btfids/ by hand"
    fi
    info "1. sudo reboot, and pick 'Advanced options for Ubuntu' > 'Ubuntu, with Linux $rel' in grub."
    info "   To boot it once and fall back to the current kernel afterwards (GRUB_DEFAULT=saved needed):"
    info "     sudo grub-reboot 'Advanced options for Ubuntu>Ubuntu, with Linux $rel' && sudo reboot"
    info "2. after the reboot:  uname -r   (should end with $LOCALVERSION)"
    info "     make setup-toolchain   # \$KDIR/vmlinux for module BTF, pahole check"
    info "     make doctor"
    info "     scripts/test-lockdep.sh"
}

fetch_source
configure
if [[ "$PREPARE_ONLY" == 1 ]]; then
    log "PREPARE_ONLY: stopped before the build. Config: $TREE/.config"
    exit 0
fi
build
if [[ "$BUILD_ONLY" == 1 ]]; then
    log "BUILD_ONLY: debs are in $SRC_DIR"
    exit 0
fi
install_debs
