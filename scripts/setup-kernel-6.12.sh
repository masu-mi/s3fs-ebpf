#!/usr/bin/env bash
# setup-kernel-6.12.sh — install a mainline Linux 6.12 LTS kernel on Ubuntu 24.04.
# kmod/s3fs_ebpf_dev.c requires >= 6.12 (struct bpf_arena layout); 6.12 is
# the LTS branch the layout mirror is verified against.
#
# Debian trixie already ships 6.12 — this script is Ubuntu-only.
# Uses the kernel.ubuntu.com mainline .deb builds. Review before running.
#
#   VERSION=v6.12.5 scripts/setup-kernel-6.12.sh
#   DRY_RUN=1       scripts/setup-kernel-6.12.sh   # list the debs only
set -euo pipefail

# shellcheck source=lib/mainline.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/mainline.sh"

VERSION="${VERSION:-v6.12}"

[[ "$(os_id)" == ubuntu ]] || warn "not Ubuntu ($(os_id)); mainline debs are built for Ubuntu"

log "kernel ${VERSION} debs from ${MAINLINE_URL}"
mapfile -t DEBS < <(mainline_list_debs "$VERSION" 'linux-(image-unsigned|modules|headers)-.*(generic|_all)')
[[ ${#DEBS[@]} -gt 0 ]] || die "no .deb found. Check ${MAINLINE_URL}/${VERSION}/ manually."
printf '  %s\n' "${DEBS[@]}" >&2

confirm "install the above kernel packages?" || die "aborted"
mainline_fetch_install "$VERSION" "${DEBS[@]}"

log "done. Reboot into the new kernel (sudo reboot), then run:"
info "scripts/setup-toolchain.sh headers kbuild   # headers + gcc/pahole/vmlinux for the new kernel"
info "make doctor"
