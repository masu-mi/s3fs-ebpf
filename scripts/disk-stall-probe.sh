#!/usr/bin/env bash
# disk-stall-probe.sh — is the disk stalling right now?
#
# Writes N x 100 MiB with O_DIRECT (no page cache, no MinIO, no mount) and times each
# one.  On the KVM dev VM (`QEMU HARDDISK`) the block device stalls for seconds at a time
# (benchmark-results §18.64): a probe iteration over 3x the median means any write-heavy
# measurement taken now is contaminated.  Run it before and after a write-heavy bench
# (B, C, D, G seed) and discard / redo the runs in between when it reports stalls.
#
# Two checks: single iterations over 3x the median (stalls), and the median itself over
# ABS_MS (default 300 ms; a healthy disk writes 100 MiB in ~70 ms, §18.64): the whole
# disk can be slow for minutes, which a relative check cannot see.
#
#   scripts/disk-stall-probe.sh [N=20] [DIR=/tmp]      (ABS_MS=300 to change the limit)
# exit 0: healthy; exit 1: stalls or a slow disk.
set -euo pipefail
N=${1:-20}
DIR=${2:-/tmp}
ABS_MS=${ABS_MS:-300}
f=$(mktemp -p "$DIR" disk-stall-probe.XXXXXX)
trap 'rm -f "$f"' EXIT
ms=()
for _ in $(seq 1 "$N"); do
	t=$EPOCHREALTIME
	dd if=/dev/zero of="$f" bs=1M count=100 oflag=direct conv=notrunc 2>/dev/null
	ms+=("$(awk -v a="$t" -v b="$EPOCHREALTIME" 'BEGIN{printf "%.0f", (b-a)*1000}')")
done
printf '%s\n' "${ms[@]}" | sort -n | awk -v n="$N" -v abs="$ABS_MS" '
	{ a[NR] = $1 }
	END {
		m = a[int((NR + 1) / 2)]; slow = 0
		for (i = 1; i <= NR; i++) if (a[i] > 3 * m) slow++
		printf "disk-stall-probe: n=%d median=%d ms max=%d ms slow(>3x median)=%d%s\n", NR, m, a[NR], slow, (m > abs ? "  SLOW DISK (median > " abs " ms)" : "")
		exit (slow > 0 || m > abs)
	}'
