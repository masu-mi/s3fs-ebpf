#!/usr/bin/env bash
# test-lockdep.sh — run the regression suite on a lockdep kernel and fail on
# any lock-order / sleeping-in-atomic / RCU report (AC-42).
#
# Needs a kernel with CONFIG_PROVE_LOCKING=y: scripts/setup-kernel-lockdep.sh
# builds and installs one; boot it, run `make setup-toolchain`, then:
#
#   scripts/test-lockdep.sh                 # make test-regression with hit_accum 1 and 0
#   MODES="1" scripts/test-lockdep.sh       # only the default
#   TARGETS="test-pcache-evict-race test-pick-victim-stale" scripts/test-lockdep.sh
#   KUNIT=1 scripts/test-lockdep.sh         # also load the KUnit build of the kmod first
#
# Each mode starts from a cleared kernel log.  lockdep turns itself off after
# its first report (/proc/lockdep_stats: debug_locks: 0), so the run stops at
# the first splat and tells you; fix it and run again.  Reports are saved to
# $OUT/dmesg-<mode>.log with the lines around each one.
#
# Exit 0 only when every mode's targets passed, lockdep was still on at the
# end, and the kernel log has none of the patterns below.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$TOPDIR"
# shellcheck source=lib/common.sh
source "$TOPDIR/scripts/lib/common.sh"

MODES=${MODES:-"1 0"}
TARGETS=${TARGETS:-test-regression}
OUT=${OUT:-/tmp/test-lockdep}
KUNIT=${KUNIT:-0}
# Patterns that mean a kernel-side lock or context bug.  "BUG:" and "WARNING:"
# alone are too broad (other drivers print them); these are lockdep's and the
# sleep/RCU checkers' own words, plus an oops in or near our module.
PATTERN='possible circular locking|possible recursive locking|possible irq lock inversion|inconsistent lock state|bad unlock balance|lock held when returning|held lock freed|BUG: sleeping function called from invalid context|BUG: scheduling while atomic|suspicious RCU usage|DEBUG_LOCKS_WARN_ON|INFO: trying to register non-static key|BUG: MAX_LOCKDEP|Oops:|general protection fault|blocked for more than'

mkdir -p "$OUT"
rm -f "$OUT"/dmesg-*.log "$OUT"/run-*.log

# --- is this a lockdep kernel? ------------------------------------------------
[[ -r /proc/lockdep_stats ]] || die "/proc/lockdep_stats is absent: $(uname -r) has no CONFIG_PROVE_LOCKING. Build one with scripts/setup-kernel-lockdep.sh and boot it."
lockdep_on() { awk '/debug_locks:/ { print $2 }' /proc/lockdep_stats; }
[[ "$(lockdep_on)" == 1 ]] || die "lockdep is already off on this boot (debug_locks: 0, an earlier report). Reboot."
info "kernel $(uname -r), lockdep on; classes: $(awk '/lock-classes:/ { print $2 }' /proc/lockdep_stats)"
sudo -n true 2>/dev/null || warn "sudo needs a password; the tests ask for it once"

# --- build for THIS kernel ------------------------------------------------------
log "make kmod daemon bpf (against $(uname -r))"
make kmod daemon bpf >"$OUT/build.log" 2>&1 || { tail -20 "$OUT/build.log" >&2; die "build failed; run make doctor"; }

# --- optional: the KUnit suites under lockdep ---------------------------------------
if [[ "$KUNIT" == 1 ]]; then
	if grep -qs '^CONFIG_KUNIT=y' "/boot/config-$(uname -r)" || { [[ -r /proc/config.gz ]] && zcat /proc/config.gz | grep -q '^CONFIG_KUNIT=y'; }; then
		log "KUnit build of the kmod (results in dmesg)"
		sudo scripts/kmod-unload.sh s3fs_kfunc >/dev/null 2>&1 || true
		sudo dmesg -C
		make kmod-clean >/dev/null 2>&1
		# KUNIT=1 reaches kmod/Kbuild through the environment (kmod/Kbuild: -DS3FS_KUNIT).
		KUNIT=1 make kmod >"$OUT/kunit-build.log" 2>&1 || {
			tail -10 "$OUT/kunit-build.log" >&2
			die "KUNIT=1 build failed (see $OUT/kunit-build.log)"
		}
		sudo insmod kmod/s3fs_kfunc.ko || warn "insmod of the KUnit build failed"
		sleep 5
		sudo dmesg >"$OUT/kunit.log"
		sudo rmmod s3fs_kfunc 2>/dev/null || true
		grep -E 'ok [0-9]+ - |# .*: pass:|not ok' "$OUT/kunit.log" | tail -20 || true
		grep -q 'not ok' "$OUT/kunit.log" && die "KUnit failures under lockdep: $OUT/kunit.log"
		make kmod-clean >/dev/null 2>&1 && make kmod >/dev/null 2>&1   # back to the normal build for the runs below
	else
		warn "this kernel has no CONFIG_KUNIT=y; KUNIT=1 skipped (setup-kernel-lockdep.sh enables it)"
	fi
fi

# --- the runs -------------------------------------------------------------------------
status=PASS
detail=""
for mode in $MODES; do
	log "mode hit_accum=$mode: make $TARGETS"
	sudo scripts/kmod-unload.sh s3fs_kfunc >/dev/null 2>&1 || true
	sudo dmesg -C
	rc=0
	HIT_ACCUM=$mode make -k $TARGETS >"$OUT/run-$mode.log" 2>&1 || rc=$?
	sudo dmesg >"$OUT/dmesg-$mode.log"
	on=$(lockdep_on)
	hits=$(grep -c -E "$PATTERN" "$OUT/dmesg-$mode.log" || true)
	ours=$(grep -c -E 's3fs_|s3fs_kfunc' "$OUT/dmesg-$mode.log" || true)
	grep -E '^RESULT.*status=FAIL' "$OUT/run-$mode.log" | head -5 || true
	info "make rc=$rc, kernel-log hits=$hits (mentioning s3fs: $ours), debug_locks=$on"
	if [[ "$hits" -gt 0 ]]; then
		warn "kernel reports in $OUT/dmesg-$mode.log:"
		grep -n -B2 -A30 -E "$PATTERN" "$OUT/dmesg-$mode.log" | head -120 >&2 || true
	fi
	if [[ "$rc" -ne 0 || "$hits" -gt 0 || "$on" != 1 ]]; then
		status=FAIL
		detail="$detail hit_accum=$mode:FAIL(rc=$rc,reports=$hits,debug_locks=$on)"
	else
		detail="$detail hit_accum=$mode:ok"
	fi
done

sudo scripts/kmod-unload.sh s3fs_kfunc >/dev/null 2>&1 || true
info "lockdep classes now: $(awk '/lock-classes:/ { print $2 }' /proc/lockdep_stats); stats: /proc/lockdep_stats, /proc/lock_stat"
echo "RESULT name=test-lockdep status=$status kernel=$(uname -r)$detail"
[[ "$status" == PASS ]]
