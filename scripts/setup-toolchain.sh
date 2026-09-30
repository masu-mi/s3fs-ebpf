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
        # bpf_arena needs >=6.9, so mainline kernels
        # (kernel.ubuntu.com/mainline) are expected. Ubuntu ships bpftool as
        # part of linux-tools-<kernel>, and mainline has no matching archive
        # package -- so on mainline we install the stock linux-tools-generic
        # (6.8-era binary, kernel-version-agnostic for our uses) and symlink
        # the real binary over the failing wrapper below. perf is skipped.
        EXTRA_PKGS=(
            linux-tools-common
            linux-tools-generic
            docker-compose-v2
        )
        if apt-cache show "linux-tools-$(uname -r)" >/dev/null 2>&1; then
            EXTRA_PKGS+=("linux-tools-$(uname -r)")
        else
            echo "note: linux-tools-$(uname -r) not in archive (mainline kernel?); using stock linux-tools-generic for bpftool." >&2
        fi
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

# On Ubuntu mainline kernels, /usr/bin/bpftool (from linux-tools-common) is a
# wrapper that dispatches by uname -r and fails when no matching linux-tools
# package is installed. Expose the real binary from the stock generic package.
if [ "${ID:-}" = "ubuntu" ] && ! bpftool version >/dev/null 2>&1; then
    real_bpftool=$(ls -1 /usr/lib/linux-tools/*/bpftool 2>/dev/null | sort -V | tail -n1 || true)
    if [ -n "${real_bpftool}" ]; then
        sudo ln -sf "${real_bpftool}" /usr/local/bin/bpftool
        echo "==> symlinked ${real_bpftool} -> /usr/local/bin/bpftool"
    fi
fi

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
