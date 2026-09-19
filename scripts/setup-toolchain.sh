#!/usr/bin/env bash
# setup-toolchain.sh — install build/observability tooling on Ubuntu 24.04.
set -euo pipefail

echo "==> apt packages"
sudo apt-get update
sudo apt-get install -y \
    build-essential \
    clang llvm \
    libbpf-dev \
    linux-tools-common linux-tools-generic \
    "linux-tools-$(uname -r)" \
    pkg-config \
    fio \
    bpfcc-tools python3-bpfcc \
    curl jq \
    docker.io docker-compose-v2

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
