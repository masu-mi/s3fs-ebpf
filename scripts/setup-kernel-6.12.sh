#!/usr/bin/env bash
# setup-kernel-6.12.sh — install a mainline Linux 6.12 LTS kernel on Ubuntu 24.04.
# bpf_arena is required by the daemon (landed in Linux 6.9); we pin to 6.12
# because it is the current LTS branch that still ships the arena support.
#
# This script uses the mainline-ppa .deb builds. Review before running.
set -euo pipefail

MAINLINE=https://kernel.ubuntu.com/mainline
VERSION="${VERSION:-v6.12}"
ARCH=$(dpkg --print-architecture)

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

cd "$TMP"

echo "==> Downloading kernel ${VERSION} debs (${ARCH}) from ${MAINLINE}"
INDEX=$(curl -fsSL "${MAINLINE}/${VERSION}/${ARCH}/")
mapfile -t URLS < <(echo "$INDEX" \
    | grep -oE 'href="[^"]+\.deb"' \
    | sed -E 's/href="//; s/"$//' \
    | grep -E "linux-(image-unsigned|modules|headers)-.*generic" \
    | sort -u)

if [[ ${#URLS[@]} -eq 0 ]]; then
    echo "ERROR: no .deb URLs found. Check ${MAINLINE}/${VERSION}/${ARCH}/ manually." >&2
    exit 1
fi

for u in "${URLS[@]}"; do
    curl -fLO "${MAINLINE}/${VERSION}/${ARCH}/${u}"
done

echo "==> Installing"
sudo dpkg -i ./*.deb || sudo apt-get -f install -y

echo "Reboot into the new kernel: sudo reboot"
echo "After reboot verify with: uname -r"
