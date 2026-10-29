#!/usr/bin/env bash
# boot-kernel.sh — choose which installed kernel grub boots (Ubuntu, GRUB_DEFAULT=saved).
#
#   scripts/boot-kernel.sh                       # list entries, show the saved default
#   scripts/boot-kernel.sh generic               # make 6.12.0-061200-generic the default
#   scripts/boot-kernel.sh lockdep               # make 6.12.0-s3fs-lockdep the default
#   scripts/boot-kernel.sh lockdep --once        # boot it on the next reboot only (then back to the default)
#   scripts/boot-kernel.sh 6.8.0-147-generic     # any version substring
#   DRY_RUN=1 scripts/boot-kernel.sh lockdep     # print the grub command only
#
# A new kernel package does not change the saved default (with GRUB_DEFAULT=0 the
# newest kernel wins on every install, which put the lockdep kernel first).  One-time
# setup, already done on the dev machine:
#   sudo sed -i 's/^GRUB_DEFAULT=.*/GRUB_DEFAULT=saved/' /etc/default/grub && sudo update-grub
set -euo pipefail

# GRUB_CFG / GRUB_DEFAULTS override the files (scripts/test-boot-kernel.sh runs on a fixture).
cfg=${GRUB_CFG:-/boot/grub/grub.cfg}
defaults=${GRUB_DEFAULTS:-/etc/default/grub}
# /boot/grub/grub.cfg is root-only on Ubuntu.
if [[ -r $cfg ]]; then rd() { "$@"; }; else rd() { sudo "$@"; }; fi
grep -q '^GRUB_DEFAULT=saved' "$defaults" || {
	echo "GRUB_DEFAULT is not 'saved' in /etc/default/grub; run once:" >&2
	echo "  sudo sed -i 's/^GRUB_DEFAULT=.*/GRUB_DEFAULT=saved/' /etc/default/grub && sudo update-grub" >&2
	exit 1
}

# "<menuentry id>  <title>" for the kernels (not recovery) in the Advanced submenu.
entries() {
	rd awk '
	/^submenu / { match($0, /menuentry_id_option .gnulinux-advanced-[^'"'"']+/); sub_id = substr($0, RSTART + 21, RLENGTH - 21); next }
	/^\tmenuentry / && $0 !~ /recovery mode/ {
		match($0, /^\tmenuentry .[^'"'"']+/); title = substr($0, RSTART + 12, RLENGTH - 12)
		match($0, /menuentry_id_option .[^'"'"']+/); id = substr($0, RSTART + 21, RLENGTH - 21)
		print sub_id ">" id "\t" title
	}' "$cfg"
}

want=${1:-}
once=${2:-}
if [[ -z $want ]]; then
	if [[ -n ${GRUB_CFG:-} ]]; then echo "saved default: (fixture)"; else
		echo "saved default: $(sudo grub-editenv list | sed -n 's/^saved_entry=//p')"
	fi
	echo "running:       $(uname -r)"
	echo "entries:"
	entries | sed 's/^[^\t]*\t/  /'
	exit 0
fi

line=$(entries | grep -F -- "${want/generic/6.12.0-061200-generic}" | head -n1 || true)
line=${line:-$(entries | grep -F -- "${want/lockdep/6.12.0-s3fs-lockdep}" | head -n1 || true)}
[[ -n $line ]] || { echo "no kernel entry matches '$want'" >&2; entries | sed 's/^[^\t]*\t/  /' >&2; exit 1; }
id=${line%%$'\t'*}
title=${line#*$'\t'}
run() { if [[ ${DRY_RUN:-0} == 1 ]]; then echo "[dry-run] $*"; else "$@"; fi; }
if [[ $once == --once ]]; then
	run sudo grub-reboot "$id"
	echo "next boot only: $title (the saved default is unchanged)"
else
	run sudo grub-set-default "$id"
	echo "default kernel: $title"
fi
echo "reboot to apply: sudo reboot"
