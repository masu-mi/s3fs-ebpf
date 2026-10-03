# shellcheck shell=bash
# scripts/lib/mainline.sh — kernel.ubuntu.com/mainline helpers shared by
# setup-kernel-6.12.sh (image) and setup-toolchain.sh headers (headers).
# Mainline builds have no linux-headers-* / linux-tools-* in the Ubuntu
# archive, so both have to be fetched from the mainline index.

[[ -n "${_S3FS_LIB_MAINLINE:-}" ]] && return 0
_S3FS_LIB_MAINLINE=1

# shellcheck source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

MAINLINE_URL="${MAINLINE_URL:-https://kernel.ubuntu.com/mainline}"

# mainline_dir_for_krel <uname -r> — 6.12.0-061200-generic -> v6.12,
# 6.12.5-061205-generic -> v6.12.5 (trailing .0 dropped, ABI suffix dropped).
mainline_dir_for_krel() {
    local up=${1%%-*}
    echo "v${up%.0}"
}

# mainline_list_debs <vdir> <regex> — print .deb file names in
# $MAINLINE_URL/<vdir>/<arch>/ whose name matches <regex>. Reading the
# index avoids guessing the build timestamp in the file name.
mainline_list_debs() {
    local vdir=$1 re=$2 arch
    arch=$(dpkg --print-architecture)
    curl -fsSL "${MAINLINE_URL}/${vdir}/${arch}/" \
        | grep -oE 'href="[^"]+\.deb"' \
        | sed -E 's/^href="//; s/"$//; s#.*/##' \
        | grep -E "$re" \
        | sort -u
}

# mainline_fetch_install <vdir> <deb...> — download into a temp dir and
# dpkg -i them, resolving missing deps with apt-get -f.
mainline_fetch_install() {
    local vdir=$1; shift
    local arch tmp f
    arch=$(dpkg --print-architecture)
    tmp=$(mktemp -d)
    for f in "$@"; do
        run curl -fL -o "$tmp/$f" "${MAINLINE_URL}/${vdir}/${arch}/${f}" || { rm -rf "$tmp"; return 1; }
    done
    run "sudo dpkg -i $tmp/*.deb || sudo apt-get -f install -y"
    rm -rf "$tmp"
}
