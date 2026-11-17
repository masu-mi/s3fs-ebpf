#!/usr/bin/env bash
# build-deb.sh — runtime .deb packages: build, check, install.
#
#   scripts/build-deb.sh build [krel...]   # -> dist/deb/*.deb  (default: every kernel >= 6.12)
#   scripts/build-deb.sh check             # inspect dist/deb/*.deb without installing
#   scripts/build-deb.sh install           # apt-get install dist/deb/*.deb, then verify
#
# Packages:
#   s3fs-ebpf               /usr/bin/s3fs-ebpf, /usr/share/s3fs-ebpf/{bpf,policies},
#                           man pages (scripts/gen-man.sh)                            (arch-specific)
#   s3fs-ebpf-doc           /usr/share/doc/s3fs-ebpf-doc: docs/user, design, dev-notes (gzip; no journal)
#   s3fs-ebpf-kmod-<krel>   /lib/modules/<krel>/updates/s3fs_kfunc.ko  (one per kernel;
#                           built against that kernel's headers, Provides s3fs-ebpf-kmod)
# Needs `make daemon bpf` first (the Makefile's `deb` target does it).
# VERSION=x.y.z overrides the version (default: 0.0.0+git<date>.<sha>).
# The daemon finds the bpf objects under /usr/share/s3fs-ebpf (or $S3FS_EBPF_ROOT).
set -uo pipefail

SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/kmodbuild.sh
source "$SCRIPTS_DIR/lib/kmodbuild.sh"

TMP=""
OUT="${OUT:-$S3FS_TOPDIR/dist/deb}"
ARCH=$(dpkg --print-architecture)
VERSION="${VERSION:-0.0.0+git$(git -C "$S3FS_TOPDIR" log -1 --format=%cd --date=format:%Y%m%d 2>/dev/null || date +%Y%m%d).$(git -C "$S3FS_TOPDIR" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
MAINTAINER="${MAINTAINER:-s3fs-ebpf developers <noreply@example.invalid>}"

# write_control <root> <name> <arch> <depends> <provides> <description> [recommends] [suggests]
write_control() {
    local root=$1
    mkdir -p "$root/DEBIAN"
    {
        echo "Package: $2"
        echo "Version: $VERSION"
        echo "Architecture: $3"
        echo "Maintainer: $MAINTAINER"
        echo "Section: utils"
        echo "Priority: optional"
        [[ -n "$4" ]] && echo "Depends: $4"
        [[ -n "$5" ]] && echo "Provides: $5"
        [[ -n "${7:-}" ]] && echo "Recommends: $7"
        [[ -n "${8:-}" ]] && echo "Suggests: $8"
        echo "Description: $6"
    } >"$root/DEBIAN/control"
}

pack() { # <root> <deb>
    fakeroot dpkg-deb --root-owner-group -Zxz -b "$1" "$2" >/dev/null || die "dpkg-deb failed for $2"
    info "built $(basename "$2")"
}

build_main() {
    local root="$1/s3fs-ebpf" f n=0
    [[ -x "$S3FS_TOPDIR/bin/s3fs-ebpf" ]] || die "bin/s3fs-ebpf missing (make daemon)"
    mkdir -p "$root/usr/bin" "$root/usr/share/s3fs-ebpf/bpf" "$root/usr/share/s3fs-ebpf/policies"
    install -m 0755 "$S3FS_TOPDIR/bin/s3fs-ebpf" "$root/usr/bin/s3fs-ebpf"
    for f in "$S3FS_TOPDIR"/bpf/*.bpf.o; do
        [[ -e "$f" ]] || continue
        case "$(basename "$f")" in policy_test_*) continue ;; esac   # test fixtures
        install -m 0644 "$f" "$root/usr/share/s3fs-ebpf/bpf/"; n=$((n + 1))
    done
    [[ $n -gt 0 ]] || die "no bpf/*.bpf.o (make bpf)"
    install -m 0644 "$S3FS_TOPDIR"/policies/* "$root/usr/share/s3fs-ebpf/policies/"
    mkdir -p "$root/usr/share/man/man1"
    "$SCRIPTS_DIR/gen-man.sh" "$root/usr/share/man/man1" || die "man page generation failed"
    chmod 0644 "$root"/usr/share/man/man1/*.gz
    write_control "$root" s3fs-ebpf "$ARCH" "" "" \
        "S3 file system with an eBPF-driven page cache (daemon and policies)
 User-space daemon, CLI and built-in cache policies. Needs the matching
 s3fs-ebpf-kmod-<kernel release> package for the running kernel." \
        "s3fs-ebpf-kmod" "s3fs-ebpf-doc"
    pack "$root" "$OUT/s3fs-ebpf_${VERSION}_${ARCH}.deb"
}

# Documentation: user guide + design + dev notes. docs/journal (measurement
# logs, ~6 MB) stays out. Markdown is gzip'ed (Debian policy for doc files).
DOC_DIRS=(user design dev-notes)
build_doc() {
    local root="$1/s3fs-ebpf-doc" d dst="$1/s3fs-ebpf-doc/usr/share/doc/s3fs-ebpf-doc" f
    mkdir -p "$dst"
    for d in "${DOC_DIRS[@]}"; do cp -r "$S3FS_TOPDIR/docs/$d" "$dst/$d"; done
    cp "$S3FS_TOPDIR/docs/kmod-pcache-invariants.md" "$dst/"
    cp "$S3FS_TOPDIR/README.md" "$dst/README.md"
    # docs/user/cli-reference.md is generated; keep it, drop nothing else.
    find "$dst" -name '*.tmpl' -delete
    find "$dst" -type f -name '*.md' -print0 | xargs -0 gzip -9n
    find "$dst" -type d -exec chmod 0755 {} + ; find "$dst" -type f -exec chmod 0644 {} +
    # Debian requires a copyright file. The tree has no LICENSE yet: say so
    # rather than invent one.
    if [[ -f "$S3FS_TOPDIR/LICENSE" ]]; then cp "$S3FS_TOPDIR/LICENSE" "$dst/copyright"
    else printf 'No LICENSE file in the source tree at build time.\nSet the license before distributing these packages.\n' >"$dst/copyright"; fi
    write_control "$root" s3fs-ebpf-doc all "" "" \
        "Documentation for s3fs-ebpf
 User guide, design documents and development notes, as gzip-compressed
 Markdown under /usr/share/doc/s3fs-ebpf-doc."
    pack "$root" "$OUT/s3fs-ebpf-doc_${VERSION}_all.deb"
}

build_kmod() { # <krel> <work>
    local krel=$1 work=$2 root="$2/pkg-kmod-$1" ko
    local pkg="s3fs-ebpf-kmod-$krel"
    pkg=${pkg,,}; pkg=${pkg//_/-}
    log "kmod package for $krel"
    ko=$( kmod_build "$krel" "$work" && echo "$KMOD_KO" ) || { warn "$krel: $ko"; return 1; }
    ko=$(tail -n1 <<<"$ko")
    mkdir -p "$root/lib/modules/$krel/updates"
    install -m 0644 "$ko" "$root/lib/modules/$krel/updates/s3fs_kfunc.ko"
    write_control "$root" "$pkg" "$ARCH" "s3fs-ebpf (>= $VERSION)" "s3fs-ebpf-kmod" \
        "s3fs-ebpf kernel module for Linux $krel
 Built against the headers of $krel; only loads on that exact kernel."
    for s in postinst postrm; do
        printf '#!/bin/sh\nset -e\ncase "$1" in configure|remove|purge) depmod -a %s || true ;; esac\nexit 0\n' "$krel" \
            >"$root/DEBIAN/$s"; chmod 0755 "$root/DEBIAN/$s"
    done
    pack "$root" "$OUT/${pkg}_${VERSION}_${ARCH}.deb"
}

cmd_build() {
    command -v fakeroot >/dev/null && command -v dpkg-deb >/dev/null || die "need fakeroot and dpkg-deb (apt install fakeroot dpkg-dev)"
    local krels work rc=0 k
    if [[ $# -gt 0 ]]; then KRELS="$*"; fi
    mapfile -t krels < <(installed_krels)
    [[ ${#krels[@]} -gt 0 ]] || die "no kernel >= $S3FS_MIN_KERNEL under /lib/modules"
    work=$(mktemp -d); TMP=$work; trap 'rm -rf "$TMP"' EXIT
    rm -rf "$OUT"; mkdir -p "$OUT"
    log "s3fs-ebpf $VERSION ($ARCH)"
    build_main "$work"
    build_doc "$work"
    for k in "${krels[@]}"; do ( build_kmod "$k" "$work" ) || rc=1; done
    log "built:"; ls -1 "$OUT" >&2
    return $rc
}

# check: every package must be well-formed and, for the kmods, loadable by
# the kernel it names. Nothing is installed.
cmd_check() {
    local debs=("$OUT"/*.deb) d root rc=0 name krel
    [[ -e "${debs[0]}" ]] || die "no packages in $OUT (make deb)"
    root=$(mktemp -d); TMP=$root; trap 'rm -rf "$TMP"' EXIT
    for d in "${debs[@]}"; do
        name=$(dpkg-deb -f "$d" Package)
        log "check $name"
        local x="$root/$name"; mkdir -p "$x"; dpkg-deb -x "$d" "$x"
        case "$name" in
        s3fs-ebpf)
            [[ -x "$x/usr/bin/s3fs-ebpf" ]] || { warn "$name: no /usr/bin/s3fs-ebpf"; rc=1; continue; }
            "$x/usr/bin/s3fs-ebpf" --help >/dev/null 2>&1 || { warn "$name: binary does not run"; rc=1; }
            ls "$x"/usr/share/s3fs-ebpf/bpf/*.bpf.o >/dev/null 2>&1 || { warn "$name: no bpf objects"; rc=1; }
            [[ -f "$x/usr/share/s3fs-ebpf/policies/default.cel" ]] || { warn "$name: no default.cel"; rc=1; }
            local m n_man=0
            for m in "$x"/usr/share/man/man1/*.1.gz; do
                [[ -e "$m" ]] || continue
                n_man=$((n_man + 1))
                gzip -t "$m" 2>/dev/null && zcat "$m" | grep -q '^\.TH ' \
                    && { ! command -v groff >/dev/null || [[ -z "$(zcat "$m" | groff -k -man -ww -z 2>&1)" ]]; } \
                    || { warn "$name: bad man page $(basename "$m")"; rc=1; }
            done
            [[ $n_man -ge 2 && -e "$x/usr/share/man/man1/s3fs-ebpf.1.gz" ]] || { warn "$name: man pages missing ($n_man)"; rc=1; }
            ;;
        s3fs-ebpf-doc)
            local dd="$x/usr/share/doc/s3fs-ebpf-doc"
            [[ -f "$dd/copyright" ]] || { warn "$name: no copyright file"; rc=1; }
            [[ -f "$dd/user/getting-started.md.gz" && -f "$dd/README.md.gz" ]] \
                || { warn "$name: expected docs missing"; rc=1; }
            [[ -z "$(find "$dd" -name '*.md' -print -quit)" ]] || { warn "$name: uncompressed .md left"; rc=1; }
            [[ ! -d "$dd/journal" ]] || { warn "$name: journal must not be packaged"; rc=1; }
            find "$dd" -name '*.md.gz' -print0 | xargs -0 -n50 gzip -t 2>/dev/null || { warn "$name: corrupt gzip"; rc=1; }
            ;;
        s3fs-ebpf-kmod-*)
            krel=$(basename "$(ls -d "$x"/lib/modules/*/ 2>/dev/null | head -n1)")
            local ko="$x/lib/modules/$krel/updates/s3fs_kfunc.ko"
            [[ -f "$ko" ]] || { warn "$name: no .ko under lib/modules/$krel"; rc=1; continue; }
            [[ "$(modinfo -F vermagic "$ko" | awk '{print $1}')" == "$krel" ]] \
                || { warn "$name: vermagic != $krel"; rc=1; }
            "$SCRIPTS_DIR/check-kmod-btf.sh" "$ko" >/dev/null 2>&1 || { warn "$name: no module BTF"; rc=1; }
            depmod -b "$x" "$krel" 2>/dev/null
            grep -q s3fs_kfunc "$x/lib/modules/$krel/modules.dep" || { warn "$name: not in modules.dep"; rc=1; }
            [[ "$name" == "$(echo "s3fs-ebpf-kmod-$krel" | tr 'A-Z_' 'a-z-')" ]] || { warn "$name: name does not match $krel"; rc=1; }
            ;;
        *) warn "unexpected package $name"; rc=1; continue ;;
        esac
        # Dependencies must be resolvable (simulated; no root, nothing installed).
        local sim="$root/.sim"; mkdir -p "$sim"
        apt-get install -s "$d" "${debs[@]}" >"$sim/$name" 2>&1 \
            || { warn "$name: apt-get install -s failed"; tail -n 5 "$sim/$name" >&2; rc=1; }
        [[ $rc == 0 ]] && info "$name: ok"
    done
    return $rc
}

cmd_install() {
    local debs=("$OUT"/*.deb) k rc=0
    [[ -e "${debs[0]}" ]] || die "no packages in $OUT (make deb)"
    apt_get install -y "${debs[@]}" || die "install failed"
    log "verify"
    dpkg -s s3fs-ebpf >/dev/null 2>&1 || { warn "s3fs-ebpf not installed"; rc=1; }
    dpkg -s s3fs-ebpf-doc >/dev/null 2>&1 || { warn "s3fs-ebpf-doc not installed"; rc=1; }
    [[ -e /usr/share/man/man1/s3fs-ebpf.1.gz ]] || { warn "man page not installed"; rc=1; }
    /usr/bin/s3fs-ebpf --help >/dev/null 2>&1 || { warn "/usr/bin/s3fs-ebpf does not run"; rc=1; }
    for d in "${debs[@]}"; do
        k=$(dpkg-deb -f "$d" Package); [[ "$k" == s3fs-ebpf-kmod-* ]] || continue
        k=${k#s3fs-ebpf-kmod-}
        modinfo -k "$k" s3fs_kfunc >/dev/null 2>&1 && info "kernel $k: s3fs_kfunc resolvable" \
            || { warn "kernel $k: modinfo -k $k s3fs_kfunc failed"; rc=1; }
    done
    return $rc
}

case "${1:-}" in
    build) shift; cmd_build "$@" ;;
    check) cmd_check ;;
    install) cmd_install ;;
    *) sed -n '2,17p' "$0"; exit 1 ;;
esac
