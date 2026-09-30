#!/usr/bin/env bash
# setup-toolchain.sh — install build/observability tooling on Ubuntu 24.04 / Debian bookworm.
set -euo pipefail

. /etc/os-release

COMMON_PKGS=(
    build-essential
    clang llvm
    libbpf-dev
    pkg-config
    fio
    bpfcc-tools python3-bpfcc
    curl jq
    docker.io
)

case "${ID:-}" in
    ubuntu)
        EXTRA_PKGS=(
            linux-tools-common linux-tools-generic
            "linux-tools-$(uname -r)"
            docker-compose-v2
        )
        ;;
    debian)
        # bpftool is a standalone package on Debian.
        # perf is tied to the Debian kernel image; skip it when running a
        # non-Debian kernel (e.g. Ubuntu mainline). docker-compose-plugin
        # comes from Docker's official apt repo.
        EXTRA_PKGS=(
            bpftool
            docker-compose-plugin
        )
        ;;
    *)
        echo "unsupported distro: ${ID:-unknown}" >&2
        exit 1
        ;;
esac

echo "==> apt packages (${ID})"
sudo apt-get update
sudo apt-get install -y "${COMMON_PKGS[@]}" "${EXTRA_PKGS[@]}"

echo "==> Go"
if ! command -v go >/dev/null; then
    GO_VER=1.22.5
    ARCH=$(dpkg --print-architecture)
    curl -fsSL "https://go.dev/dl/go${GO_VER}.linux-${ARCH}.tar.gz" \
        | sudo tar -C /usr/local -xz
    echo 'export PATH=$PATH:/usr/local/go/bin' | sudo tee /etc/profile.d/go.sh
fi

echo "==> docker access"
sudo usermod -aG docker "$USER" || true
echo "Log out and back in for docker group to take effect."

echo "Toolchain installed. Verify with: clang --version && bpftool version && go version"
