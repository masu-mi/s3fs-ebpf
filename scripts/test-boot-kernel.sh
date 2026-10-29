#!/usr/bin/env bash
# test-boot-kernel.sh — scripts/boot-kernel.sh picks the right grub entry.
#
# Runs on a fixture (scripts/testdata/grub-submenu.cfg: an Ubuntu grub.cfg with a
# lockdep, a generic and a 6.8 kernel) and DRY_RUN=1, so grub is never touched and no
# root is needed.  Guards two bugs the first version had: the submenu id kept a
# leading quote (the saved default became "'gnulinux-advanced-...") and every title
# lost its first character ("buntu, with Linux ...").
#
#   scripts/test-boot-kernel.sh        Exit 0 = PASS.
set -euo pipefail

TOP=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
export GRUB_CFG="$TOP/scripts/testdata/grub-submenu.cfg"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
echo 'GRUB_DEFAULT=saved' >"$tmp"
export GRUB_DEFAULTS=$tmp DRY_RUN=1
U=00000000-0000-0000-0000-000000000000
fail=0
check() {	# check <what> <got> <want-substring>
	if [[ "$2" == *"$3"* ]]; then echo "  ok   $1"; else echo "  FAIL $1: want '$3' in:" >&2; echo "$2" | sed 's/^/       /' >&2; fail=1; fi
}
refute() {	# refute <what> <got> <unwanted-substring>
	if [[ "$2" != *"$3"* ]]; then echo "  ok   $1"; else echo "  FAIL $1: found '$3' in: $2" >&2; fail=1; fi
}

out=$("$TOP/scripts/boot-kernel.sh")
check "lists the lockdep kernel"  "$out" "  Ubuntu, with Linux 6.12.0-s3fs-lockdep"
check "lists the generic kernel"  "$out" "  Ubuntu, with Linux 6.12.0-061200-generic"
check "lists the 6.8 kernel"      "$out" "  Ubuntu, with Linux 6.8.0-147-generic"
refute "no recovery entries"      "$out" "recovery mode"
refute "titles keep their first letter" "$out" "  buntu"

out=$("$TOP/scripts/boot-kernel.sh" generic)
check "generic sets the default"  "$out" "grub-set-default gnulinux-advanced-$U>gnulinux-6.12.0-061200-generic-advanced-$U"
refute "no quote in the id"       "$out" "'gnulinux"
out=$("$TOP/scripts/boot-kernel.sh" lockdep)
check "lockdep sets the default"  "$out" "grub-set-default gnulinux-advanced-$U>gnulinux-6.12.0-s3fs-lockdep-advanced-$U"
out=$("$TOP/scripts/boot-kernel.sh" lockdep --once)
check "--once uses grub-reboot"   "$out" "grub-reboot gnulinux-advanced-$U>gnulinux-6.12.0-s3fs-lockdep-advanced-$U"
check "--once leaves the default" "$out" "the saved default is unchanged"
refute "--once does not set the default" "$out" "grub-set-default"
out=$("$TOP/scripts/boot-kernel.sh" 6.8.0-147-generic)
check "a version substring"       "$out" "gnulinux-6.8.0-147-generic-advanced-$U"

if "$TOP/scripts/boot-kernel.sh" nosuch >/dev/null 2>&1; then echo "  FAIL unknown kernel accepted" >&2; fail=1; else echo "  ok   unknown kernel is refused"; fi

echo 'GRUB_DEFAULT=0' >"$tmp"
if "$TOP/scripts/boot-kernel.sh" generic >/dev/null 2>&1; then echo "  FAIL GRUB_DEFAULT!=saved accepted" >&2; fail=1; else echo "  ok   refuses without GRUB_DEFAULT=saved"; fi

status=PASS; ((fail)) && status=FAIL
echo "RESULT name=test-boot-kernel status=$status"
((fail == 0))
