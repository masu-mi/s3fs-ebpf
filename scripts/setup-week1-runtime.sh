#!/usr/bin/env bash
# setup-week1-runtime.sh — kept for links from
# docs/journal/week1-build-runtime-notes.md. The 4 runtime workarounds
# (dpkg repair / gcc / pahole / KDIR vmlinux) now live in
# `scripts/setup-toolchain.sh kbuild`; DRY_RUN=1 still works.
exec "$(dirname "${BASH_SOURCE[0]}")/setup-toolchain.sh" kbuild "$@"
