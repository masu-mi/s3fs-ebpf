#!/usr/bin/env bash
# setup-toolchain.sh — install build/observability tooling on Ubuntu 24.04 / Debian bookworm.
set -euo pipefail

. /etc/os-release

# apt-cache show returns 0 for names merely referenced by other packages
# (e.g. linux-image-<ver>-generic's Depends line), even when there is no
# installable candidate. Check the actual candidate version instead.
apt_has_candidate() {
    local cand
    cand=$(apt-cache policy "$1" 2>/dev/null | awk '/Candidate:/ { print $2; exit }')
    [ -n "${cand}" ] && [ "${cand}" != "(none)" ]
}

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
        if apt_has_candidate "linux-tools-$(uname -r)"; then
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

echo "==> kernel headers"
# On Ubuntu mainline kernels there is no matching linux-headers-<ver> in the
# archive, so /lib/modules/$(uname -r)/build is missing and out-of-tree builds
# (kmod/) fail. Fetch the matching headers from kernel.ubuntu.com/mainline.
# The commands are printed for review first; user must confirm before running.
if [ "${ID:-}" = "ubuntu" ] && [ ! -d "/lib/modules/$(uname -r)/build" ]; then
    if apt_has_candidate "linux-headers-$(uname -r)"; then
        echo "The archive has linux-headers-$(uname -r); installing via apt."
        sudo apt-get install -y "linux-headers-$(uname -r)"
    else
        KREL=$(uname -r | sed 's/-generic$//')
        KBUILD=$(uname -v | awk '{print $1}' | tr -d '#')
        VVER="v$(echo "${KREL}" | sed 's/\.0$//')"
        BASE="https://kernel.ubuntu.com/mainline/${VVER}/amd64"
        ALL_DEB="linux-headers-${KREL}_${KREL}.${KBUILD}_all.deb"
        GEN_DEB="linux-headers-${KREL}-generic_${KREL}.${KBUILD}_amd64.deb"
        cat <<EOF
Mainline kernel detected; matching headers are not in the Ubuntu archive.
The following commands will download and install them from kernel.ubuntu.com:

  curl -fLO ${BASE}/${ALL_DEB}
  curl -fLO ${BASE}/${GEN_DEB}
  sudo dpkg -i ${ALL_DEB} ${GEN_DEB}

If the URLs 404, open ${BASE}/ in a browser to find the matching filenames
(build timestamp may differ) and install manually.
EOF
        REPLY=""
        if [ -t 0 ]; then
            read -r -p "Proceed with the above commands? [y/N] " REPLY
        else
            echo "stdin is not a tty; skipping (set S3FS_INSTALL_MAINLINE_HEADERS=1 to force)." >&2
            REPLY="${S3FS_INSTALL_MAINLINE_HEADERS:+y}"
        fi
        case "${REPLY}" in
            y|Y|yes|YES)
                TMP=$(mktemp -d)
                trap 'rm -rf "${TMP}"' EXIT
                ( cd "${TMP}" \
                    && curl -fLO "${BASE}/${ALL_DEB}" \
                    && curl -fLO "${BASE}/${GEN_DEB}" \
                    && sudo dpkg -i "${ALL_DEB}" "${GEN_DEB}" )
                ;;
            *)
                echo "skipped mainline header install; kmod build will fail until /lib/modules/$(uname -r)/build exists." >&2
                ;;
        esac
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
