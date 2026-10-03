# shellcheck shell=bash
# scripts/lib/common.sh — helpers shared by setup-*.sh / doctor.sh /
# detect-toolchain.sh. Sourced, never executed.
#
#   DRY_RUN=1  run() only prints the command; confirm() auto-accepts.
#   ASSUME_YES=1  confirm() auto-accepts (non-interactive installs).

[[ -n "${_S3FS_LIB_COMMON:-}" ]] && return 0
_S3FS_LIB_COMMON=1

S3FS_TOPDIR="${S3FS_TOPDIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
DRY_RUN="${DRY_RUN:-0}"
ASSUME_YES="${ASSUME_YES:-0}"

log()  { printf '\n\033[1;34m==>\033[0m %s\n' "$*" >&2; }
info() { printf '  %s\n' "$*" >&2; }
warn() { printf '\033[1;33mWARN\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR\033[0m %s\n' "$*" >&2; exit 1; }

# run <cmd...> — execute (or print under DRY_RUN=1). Pass a single string
# when the command needs a pipe / redirect; it is eval'd.
run() {
    if [[ "$DRY_RUN" == 1 ]]; then
        printf '  [dry-run] %s\n' "$*" >&2
        return 0
    fi
    if [[ $# -eq 1 ]]; then eval "$1"; else "$@"; fi
}

# confirm <prompt> — y/N prompt. Non-tty stdin answers "no" unless
# ASSUME_YES=1 (so CI / piped runs never block and never mutate silently).
confirm() {
    [[ "$DRY_RUN" == 1 || "$ASSUME_YES" == 1 ]] && return 0
    if [[ ! -t 0 ]]; then
        warn "stdin is not a tty; answering no to: $1 (set ASSUME_YES=1 to accept)"
        return 1
    fi
    local ans
    read -rp "$1 [y/N] " ans
    [[ "$ans" =~ ^[Yy]([Ee][Ss])?$ ]]
}

# ver_ge <a> <b> — true when version a >= b (dotted, sort -V semantics).
ver_ge() {
    [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" == "$2" ]]
}

# os_id — distro ID from /etc/os-release (ubuntu / debian / ...).
os_id() {
    ( . /etc/os-release 2>/dev/null && echo "${ID:-unknown}" ) || echo unknown
}

# os_codename — VERSION_CODENAME (noble / bookworm / trixie / ...).
os_codename() {
    ( . /etc/os-release 2>/dev/null && echo "${VERSION_CODENAME:-unknown}" ) || echo unknown
}

# apt_has_candidate <pkg> — true when apt can actually install <pkg>.
# `apt-cache show` returns 0 for names merely referenced by another
# package's Depends (e.g. linux-tools-<mainline>), so look at Candidate.
apt_has_candidate() {
    command -v apt-cache >/dev/null 2>&1 || return 1
    local cand
    cand=$(apt-cache policy "$1" 2>/dev/null | awk '/Candidate:/ { print $2; exit }')
    [[ -n "$cand" && "$cand" != "(none)" ]]
}

# apt_first_candidate <pkg...> — print the first installable package name.
apt_first_candidate() {
    local p
    for p in "$@"; do
        apt_has_candidate "$p" && { echo "$p"; return 0; }
    done
    return 1
}

# apt_install <pkg...> — idempotent apt-get install (skips when empty).
apt_install() {
    [[ $# -eq 0 ]] && return 0
    run sudo apt-get install -y "$@"
}
