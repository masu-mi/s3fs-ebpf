#!/usr/bin/env bash
# check-kmod-invariants.sh — static checks for rules that live in several
# places of kmod/vfs_ops.c and were broken by editing only one of them
# (issue #22).  No root, no kernel: grep over the sources.  `make check-invariants`.
#
#  1. The policy's on_insert / on_size / on_remove are called only from the
#     helpers s3fs_policy_track / s3fs_policy_untrack, which hold the rules
#     (skip a miss in flight, forget the hit state).
#  2. s3fs_hit_forget (resets the per-ino hit state) is called only from those
#     helpers and from admit_evict_ino, the single exit of a whole-ino evict.
#  3. s3fs_hit_collect / s3fs_hit_replay run only inside s3fs_ebpf_admit_replace
#     (they need s3fs_policy_evict_lock).
#  4. The bench seeds' file sizes are measured, not typed in (a typed
#     FILE_BYTES=44 for 46-byte files once made the cache one slot short).
set -euo pipefail
cd "$(dirname "$0")/.."
SRC=kmod/vfs_ops.c
fail=0
bad() { echo "check-kmod-invariants: $*" >&2; fail=1; }

# fn_of <line>: the function a source line sits in (last "name(" at column 0 above it).
fn_of() {
	awk -v n="$1" 'NR<=n && /^[a-z_]+[ a-z0-9_*]*[ *]s3fs_[a-z0-9_]+\(|^[a-z].* (admit_[a-z_]+|s3fs_[a-z0-9_]+)\(/ { match($0,/(admit_|s3fs_)[a-z0-9_]+\(/); f=substr($0,RSTART,RLENGTH-1) } END { print f }' "$SRC"
}

# only_in <regex> <allowed fn>...: every non-declaration match must sit in an allowed function.
only_in() {
	local re=$1; shift
	local ln fn ok a
	while IFS=: read -r ln _; do
		fn=$(fn_of "$ln")
		ok=0
		for a in "$@"; do [ "$fn" = "$a" ] && ok=1; done
		[ $ok = 1 ] || bad "$SRC:$ln: '$re' called from ${fn:-?}; allowed only in: $*"
	done < <(grep -n -E "$re" "$SRC" | grep -v -E '^[0-9]+:(static |int |void |u64 |bool )' | grep -v -E '^[0-9]+:\s*(/\*|\*)')
}

only_in 's3fs_policy_ops_(insert|size|remove)\(' s3fs_policy_track s3fs_policy_untrack
only_in 's3fs_hit_forget\(' s3fs_policy_untrack admit_evict_ino
only_in 's3fs_hit_(collect|replay)\(' s3fs_ebpf_admit_replace

for f in scripts/bench-hitrate.sh scripts/bench-hitrate-mixed.sh; do
	if grep -n -E '^(FILE_BYTES|SMALL_FILE_BYTES)=[0-9]+' "$f"; then
		bad "$f: file size typed in; measure it from the seeded content"
	fi
done

if [ $fail = 0 ]; then echo "check-kmod-invariants: ok"; fi
exit $fail
