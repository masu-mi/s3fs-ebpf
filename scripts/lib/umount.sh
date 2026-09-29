# shellcheck shell=bash
# scripts/lib/umount.sh — sourced by bench-*.sh cleanup traps.
#
# umount_hard <mountpoint>
#   Normal umount first; on EBUSY (a child process still holds an fd
#   into the mount — common when SIGINT interrupts a bench mid-run)
#   fall back to `umount -l` so the mount detaches immediately and
#   cleans up when the last reference drops. Both attempts log to
#   stderr so operators can tell which path fired.
umount_hard() {
	local mp=$1
	mountpoint -q "$mp" || return 0
	if sudo umount "$mp" 2>/dev/null; then
		return 0
	fi
	echo "[umount] $mp busy — falling back to lazy umount" >&2
	sudo umount -l "$mp" 2>/dev/null \
		|| echo "[umount] lazy umount of $mp also failed — inspect with 'lsof $mp'" >&2
}
