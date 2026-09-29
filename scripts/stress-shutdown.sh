#!/usr/bin/env bash
# scripts/stress-shutdown.sh — H-06 shutdown-race smoke.
#
# Exercises the pending / miss / flush refcount protocol under load:
# fires many concurrent cat/echo on the mount so the pending hash has
# in-flight waiters, then SIGINTs the daemon while requests are still
# outstanding. If the refcount invariant were broken, kmod would UAF
# (dmesg BUG/KASAN) or rmmod would hang. We assert clean daemon exit
# + no BUG in dmesg + rmmod success.
#
# Prereqs (script does not manage): MinIO up (make compose-up),
# kmod built, daemon built.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/umount.sh
source "$TOPDIR/scripts/lib/umount.sh"
MP=${MP:-/mnt/s3demo}
DAEMON_BIN=${DAEMON_BIN:-$TOPDIR/bin/s3fs-ebpf}
KMOD=${KMOD:-$TOPDIR/kmod/s3fs_kfunc.ko}
ITERS=${ITERS:-200}
CONCURRENCY=${CONCURRENCY:-16}
LOG=/tmp/stress-shutdown.log

log() { echo "[stress-shutdown] $*" >&2; }

cleanup() {
	sudo pkill -9 -f 's3fs-ebpf fastpath' 2>/dev/null || true
	umount_hard "$MP"
	lsmod | grep -q '^s3fs_kfunc' && sudo rmmod s3fs_kfunc 2>/dev/null || true
}
trap cleanup EXIT

log "load kmod + mount + daemon"
lsmod | grep -q '^s3fs_kfunc' || sudo insmod "$KMOD"
mountpoint -q "$MP" || { sudo mkdir -p "$MP"; sudo mount -t s3fs_ebpf none "$MP"; }
sudo pkill -9 -f 's3fs-ebpf fastpath' 2>/dev/null || true
LD_LIBRARY_PATH="$HOME/.local/lib" sudo -E \
	"$DAEMON_BIN" fastpath --s3 --sample-every=60s \
	>"$LOG" 2>&1 &
daemon_pid=$(pgrep -f 's3fs-ebpf fastpath' || true)
for _ in $(seq 1 40); do
	grep -q '\[admit\] ' "$LOG" 2>/dev/null && break
	sleep 0.2
done

log "seed dmesg cursor"
dmesg_cursor=$(sudo dmesg -T | wc -l)

log "firing ${ITERS} iters × ${CONCURRENCY} concurrency of cat"
for _ in $(seq 1 "$ITERS"); do
	for _ in $(seq 1 "$CONCURRENCY"); do
		cat "$MP/hello.txt" >/dev/null 2>&1 &
	done
done

# SIGINT mid-flight — daemon still has queued misses / pending waiters.
sleep 0.05
log "SIGINT daemon (in-flight waiters expected)"
sudo pkill -INT -f 's3fs-ebpf fastpath'

# Wait for cats to drain (they should EIO or return whatever kmod
# gives them on shutdown — we don't care about payload, only that
# nothing hangs).
wait 2>/dev/null || true

# Confirm daemon exits within 10s.
for _ in $(seq 1 50); do
	pgrep -f 's3fs-ebpf fastpath' >/dev/null || break
	sleep 0.2
done
if pgrep -f 's3fs-ebpf fastpath' >/dev/null; then
	echo "FAIL: daemon did not exit within 10s of SIGINT" >&2
	exit 1
fi
log "daemon exited cleanly"

log "check dmesg for BUG/WARN/UAF since cursor"
new_bugs=$(sudo dmesg -T | tail -n +"$dmesg_cursor" | grep -E 'BUG:|WARNING:|KASAN:|use-after-free' || true)
if [ -n "$new_bugs" ]; then
	echo "FAIL: kernel complaint during stress:" >&2
	echo "$new_bugs" >&2
	exit 1
fi
log "no BUG/WARN/KASAN in dmesg"

log "umount + rmmod (must succeed cleanly)"
umount_hard "$MP"
# Cat children may still be draining even after wait — they return
# through kmod paths that touch pending / miss queues on shutdown.
# Retry rmmod up to 5s so we're testing "does the kmod eventually
# become unloadable" (H-06's real concern), not "does it unload
# instantly the microsecond after SIGINT".
rmmod_ok=0
for _ in $(seq 1 25); do
	if sudo rmmod s3fs_kfunc 2>/dev/null; then rmmod_ok=1; break; fi
	sleep 0.2
done
if [ "$rmmod_ok" -ne 1 ]; then
	echo "FAIL: rmmod could not unload after 5s (H-06 refcount leak?)" >&2
	lsmod | grep s3fs_kfunc >&2 || true
	exit 1
fi
log "rmmod OK"

log "PASS — H-06 refcount protocol survived ${ITERS}×${CONCURRENCY} stress + mid-flight SIGINT"
